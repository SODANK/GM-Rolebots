package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// The ore desk: does the corporation sell its ore, or build from it?
//
// The answer is a share f, how much ore the corporation keeps for production.
// The tags set the bounds: an industry outfit with no mining tag builds
// (f in [0.85, 1]), a mining outfit with no industry tag sells (f = 0), both
// tags decide in between, neither sells. The economics pick the point
// (oreDeskRatio, a sigmoid of ln(build/sell), pulled down by the treasury's
// liquidity pressure) and the CEO's patience and greed set how fast f drifts.
// Ore never strands: a stack held past corpOreHoldDays is sold anyway.

const (
	// corpOreHoldDays is the longest the desk keeps an ore stack unsold.
	corpOreHoldDays = 7
	// oreDeskK is the sigmoid's slope on ln(build/sell): building worth
	// 1.4× selling puts an unconstrained desk at about 73% build.
	oreDeskK = 3.0
	// oreDeskDefaultYield is the refine yield assumed when nobody the
	// corporation could send to refine is known yet.
	oreDeskDefaultYield = 0.6
	// oreDeskFeeFrac is the installation fee allowance, as a share of a
	// run's product value, taken off the build side.
	oreDeskFeeFrac = 0.02
)

// oreBounds is the band the tags allow the build share to move in.
type oreBounds struct{ Min, Max float64 }

// oreBoundsFor reads the band off a corporation's tags.
func oreBoundsFor(tags []string) oreBounds {
	build := industryTagged(tags)
	mine := false
	for _, t := range tags {
		if t == "mining" {
			mine = true
		}
	}
	switch {
	case build && !mine:
		return oreBounds{0.85, 1}
	case build && mine:
		return oreBounds{0, 1}
	}
	return oreBounds{0, 0}
}

// hasIndustryDesk reports whether a corporation's tags give it an industry
// desk at all: a Director, a library, a work board, a factory. The one gate
// every desk path reads (estate, roles, production, donations, the panel).
func hasIndustryDesk(tags []string) bool { return oreBoundsFor(tags).Max > 0 }

// factorySecFloor is the security floor for an industry desk's factory: the
// CEO's own, never above the headquarters' system, so a corporation already
// living at 0.5 does not refuse a factory there.
func factorySecFloor(ceoFloor, hqSec float64) float64 { return math.Min(ceoFloor, hqSec) }

// officerUntenured is the treasury guard: a player too new for an office with
// wallet access. Bots are never held back, since the guard is about human
// wallet risk; a corporation younger than a month must still have officers.
func officerUntenured(human bool, sinceWin32, nowWin32 int64) bool {
	return human && tenureDays(sinceWin32, nowWin32) < officerTenureDays
}

// oreDeskTarget is where the build share wants to be. ratio is build value
// over sell value (0 = nothing on the board to build with it); liquidity is
// the treasury's pressure in [0, 1]; lambda the CEO's cash pull.
func oreDeskTarget(ratio, liquidity, lambda float64, b oreBounds) float64 {
	if b.Max <= b.Min {
		return b.Min
	}
	x := -8.0 // nothing to build: hard toward the market
	if ratio > 0 {
		x = oreDeskK * math.Log(ratio)
	}
	x -= lambda * liquidity
	f := 1 / (1 + math.Exp(-x))
	return math.Max(b.Min, math.Min(b.Max, f))
}

// oreDeskStep moves the share toward its target by an exponential with the
// CEO's half-life, over the time since the last step.
func oreDeskStep(f, target float64, dtSec, halfLifeSec int64) float64 {
	if dtSec <= 0 {
		return f
	}
	if halfLifeSec <= 0 {
		return target
	}
	a := 1 - math.Pow(0.5, float64(dtSec)/float64(halfLifeSec))
	return f + (target-f)*a
}

// oreLiquidity is the treasury's pressure: 0 at or above the working target,
// rising to 1 as free capital falls to nothing. Squared so a small shortfall
// barely registers and a real one dominates.
func oreLiquidity(free, target float64) float64 {
	if target <= 0 || free >= target {
		return 0
	}
	short := 1 - math.Max(0, free)/target
	return short * short
}

// oreDeskLabel names a share for the ledger.
func oreDeskLabel(f float64) string {
	switch {
	case f < 0.35:
		return "selling"
	case f > 0.65:
		return "building"
	}
	return "splitting"
}

// buildOrderValue is one open build order as the desk values it.
type buildOrderValue struct {
	RunsLeft     int
	ProductValue float64 // per run
	MatValue     float64 // per run
	WagePerRun   float64
}

// oreDeskBeta is what the work board turns one ISK of materials into, net of
// wages and fees, weighted by the materials each order still needs. 0 with
// nothing to build.
func oreDeskBeta(orders []buildOrderValue) float64 {
	num, den := 0.0, 0.0
	for _, o := range orders {
		if o.RunsLeft <= 0 || o.MatValue <= 0 {
			continue
		}
		net := o.ProductValue*(1-oreDeskFeeFrac) - o.WagePerRun
		w := o.MatValue * float64(o.RunsLeft)
		num += w * math.Max(0, net) / o.MatValue
		den += w
	}
	if den <= 0 {
		return 0
	}
	return num / den
}

// oreDeskRatio is build value over sell value for a cubic metre of ore.
// Ore sells for oreIntrinsicSpread of its refined value; refined at yield
// and built at beta it earns yield × beta of it.
func oreDeskRatio(beta, yield float64) float64 {
	if beta <= 0 || yield <= 0 {
		return 0
	}
	return beta * yield / oreIntrinsicSpread
}

// corpPolicyRow is rolebot_corp_policy.
type corpPolicyRow struct {
	CorpID        int64
	OreShare      float64
	OreTarget     float64
	OreAt         int64
	OreWhy        string
	DirectorID    int64
	DirectorSince int64
	DeputyID      int64
	SalaryAt      int64
	BoardHash     string
	BoardMailAt   int64
}

// loadCorpPolicy reads a corporation's policy row; a missing row is zeros.
func (m *Mod) loadCorpPolicy(ctx context.Context, corpID int64) (corpPolicyRow, error) {
	p := corpPolicyRow{CorpID: corpID}
	err := m.db.QueryRowContext(ctx, `
		SELECT oreShare, oreTarget, oreAt, oreWhy, directorID, directorSince, deputyID,
		       salaryAt, boardHash, boardMailAt
		  FROM rolebot_corp_policy WHERE corporationID = ?`, corpID).
		Scan(&p.OreShare, &p.OreTarget, &p.OreAt, &p.OreWhy, &p.DirectorID, &p.DirectorSince,
			&p.DeputyID, &p.SalaryAt, &p.BoardHash, &p.BoardMailAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, fmt.Errorf("loadCorpPolicy: %w", err)
	}
	return p, nil
}

// saveCorpPolicyRow writes the whole row.
func (m *Mod) saveCorpPolicyRow(ctx context.Context, p corpPolicyRow) error {
	forgetCorpFactory(p.CorpID)
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_policy
			(corporationID, oreShare, oreTarget, oreAt, oreWhy, directorID, directorSince, deputyID,
			 salaryAt, boardHash, boardMailAt, updatedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			oreShare = VALUES(oreShare), oreTarget = VALUES(oreTarget), oreAt = VALUES(oreAt),
			oreWhy = VALUES(oreWhy), directorID = VALUES(directorID), directorSince = VALUES(directorSince),
			deputyID = VALUES(deputyID), salaryAt = VALUES(salaryAt), boardHash = VALUES(boardHash),
			boardMailAt = VALUES(boardMailAt), updatedAt = VALUES(updatedAt)`,
		p.CorpID, p.OreShare, p.OreTarget, p.OreAt, clampText(p.OreWhy, 255), p.DirectorID,
		p.DirectorSince, p.DeputyID, p.SalaryAt, p.BoardHash, p.BoardMailAt, clock.Unix())
	if err != nil {
		return fmt.Errorf("saveCorpPolicy: %w", err)
	}
	return nil
}

// corpOreShare is the corporation's current build share (0 for any
// corporation the desk has never run for).
func (m *Mod) corpOreShare(ctx context.Context, corpID int64) float64 {
	p, err := m.loadCorpPolicy(ctx, corpID)
	if err != nil {
		return 0
	}
	return p.OreShare
}

// openBuildOrderValues reads every open build order on the corporation's
// board, priced.
func (m *Mod) openBuildOrderValues(ctx context.Context, corpID int64) ([]buildOrderValue, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT w.bpTypeID, w.productTypeID, w.runsWanted - w.runsDone, w.wagePerRun,
		       COALESCE(t.portionSize, 1)
		  FROM rolebot_corp_work w
		  JOIN invTypes t ON t.typeID = w.productTypeID
		 WHERE w.corporationID = ? AND w.status = 'open' AND w.kind = 'build'`, corpID)
	if err != nil {
		return nil, fmt.Errorf("openBuildOrderValues: %w", err)
	}
	type raw struct {
		bp, product int64
		left        int
		wage        float64
		portion     int64
	}
	var list []raw
	for rows.Next() {
		var r raw
		if rows.Scan(&r.bp, &r.product, &r.left, &r.wage, &r.portion) == nil && r.left > 0 {
			list = append(list, r)
		}
	}
	rows.Close()
	if len(list) == 0 {
		return nil, rows.Err()
	}
	var out []buildOrderValue
	for _, r := range list {
		mats, err := evedb.GetRequiredMaterials(ctx, m.db, r.bp, 1)
		if err != nil {
			continue
		}
		ids := []int64{r.product}
		for _, mt := range mats {
			ids = append(ids, mt.TypeID)
		}
		vals, err := evedb.TypeValues(ctx, m.db, ids)
		if err != nil {
			continue
		}
		o := buildOrderValue{RunsLeft: r.left, WagePerRun: r.wage,
			ProductValue: vals[r.product].Value * float64(max(r.portion, 1))}
		for _, mt := range mats {
			o.MatValue += vals[mt.TypeID].Value * float64(mt.Quantity) * mt.DamagePerJob
		}
		out = append(out, o)
	}
	return out, nil
}

// corpRefineYield is the net yield the corporation's refiner gets at its
// factory: the hand of the bot that refines (corpOreRefiner: the deputy, else
// the Industry Director), else the default.
func (m *Mod) corpRefineYield(ctx context.Context, corpID int64, p corpPolicyRow) float64 {
	who := m.corpOreRefiner(ctx, corpID, p)
	if who == 0 {
		return oreDeskDefaultYield
	}
	b, err := m.loadRun(ctx, who)
	if err != nil || b == nil {
		return oreDeskDefaultYield
	}
	station := m.corpFactoryStation(ctx, corpID)
	if y := m.refineHandAt(ctx, b, station, 0).Net(); y > 0 {
		return y
	}
	return oreDeskDefaultYield
}

// ceoOreDesk runs the desk for one think: target, step, persist, and a ledger
// line when the corporation's stance changes.
func (m *Mod) ceoOreDesk(ctx context.Context, c *corpCEO, ch Charter) error {
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	p, err := m.loadCorpPolicy(ctx, c.CorpID)
	if err != nil {
		return err
	}
	bounds := oreBoundsFor(facts.Policy.Tags)

	orders, err := m.openBuildOrderValues(ctx, c.CorpID)
	if err != nil {
		return err
	}
	beta := oreDeskBeta(orders)
	yield := m.corpRefineYield(ctx, c.CorpID, p)
	ratio := oreDeskRatio(beta, yield)

	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return err
	}
	working := m.corpReserve(ctx, c.CorpID, ch) + m.corpCommitted(ctx, c.CorpID) + ch.DirectorSalary
	liq := oreLiquidity(free, working)
	target := oreDeskTarget(ratio, liq, ch.OreCashWeight, bounds)

	now := clock.Unix()
	f := target
	if p.OreAt > 0 {
		f = oreDeskStep(p.OreShare, target, now-p.OreAt, ch.OreHalfLifeSec)
	}
	f = math.Max(bounds.Min, math.Min(bounds.Max, f))

	why := ""
	switch {
	case bounds.Max == 0:
		why = "a corporation with no industry tag sells its ore"
	case ratio == 0:
		why = "nothing on the work board to build it into"
	default:
		why = fmt.Sprintf("building earns %.2f× selling", ratio)
	}
	if liq > 0 {
		why += fmt.Sprintf("; treasury at %.0f%% of working capital", 100*math.Max(0, free)/math.Max(1, working))
	}
	if bounds.Min > 0 && bounds.Max > bounds.Min {
		why += "; an industry outfit keeps at least " + fmt.Sprintf("%.0f%%", bounds.Min*100)
	}

	if p.OreAt > 0 && oreDeskLabel(p.OreShare) != oreDeskLabel(f) {
		m.ceoLog(ctx, c, powerBusiness, "oredesk", 0, f,
			"the ore desk turned to %s (%.0f%% of ore kept for production) — %s", oreDeskLabel(f), f*100, why)
	}
	p.OreShare, p.OreTarget, p.OreAt, p.OreWhy = f, target, now, why
	return m.saveCorpPolicyRow(ctx, p)
}

// oreHoldPlan is how much of one ore stack the business pass lists now, and how
// much it keeps. The share applies to arrivals only: new ore is split f kept,
// 1-f listed, and earlier keep stays kept. Everything is listed once the oldest
// kept ore has waited corpOreHoldDays, and a share of 0 sells at once. Pure.
func oreHoldPlan(qty, keep int64, share float64, heldSince, now int64) (list, newKeep int64, release bool) {
	if qty <= 0 {
		return 0, 0, true
	}
	if share <= 0 || (heldSince > 0 && now-heldSince >= corpOreHoldDays*86400) {
		return qty, 0, true
	}
	if keep >= qty {
		return 0, qty, false
	}
	arrived := qty - keep
	newKeep = keep + int64(math.Floor(float64(arrived)*share))
	return qty - newKeep, newKeep, false
}

// clampText cuts a string to n bytes on a rune boundary.
func clampText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// oreHold is one kept ore stack's bookkeeping.
type oreHold struct {
	keep  int64
	since int64
}

// loadOreHolds reads a corporation's kept stacks by (station, type).
func (m *Mod) loadOreHolds(ctx context.Context, corpID int64) map[[2]int64]oreHold {
	out := map[[2]int64]oreHold{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT stationID, typeID, keepQty, heldSince FROM rolebot_corp_orehold WHERE corporationID = ?`, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st, ty int64
		var h oreHold
		if rows.Scan(&st, &ty, &h.keep, &h.since) == nil {
			out[[2]int64{st, ty}] = h
		}
	}
	return out
}

func (m *Mod) saveOreHold(ctx context.Context, corpID, stationID, typeID, keep, since int64) {
	if keep <= 0 {
		m.dropOreHold(ctx, corpID, stationID, typeID)
		return
	}
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_orehold (corporationID, stationID, typeID, keepQty, heldSince)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE keepQty = VALUES(keepQty), heldSince = VALUES(heldSince)`,
		corpID, stationID, typeID, keep, since)
}

func (m *Mod) dropOreHold(ctx context.Context, corpID, stationID, typeID int64) {
	_, _ = m.db.ExecContext(ctx, `
		DELETE FROM rolebot_corp_orehold WHERE corporationID = ? AND stationID = ? AND typeID = ?`,
		corpID, stationID, typeID)
}
