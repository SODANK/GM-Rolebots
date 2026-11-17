package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	evedb "gurimoon/db"
)

// The Armory and the Fleet Yard: ships a corporation lends for a fleet op (docs/FLEET_DOCTRINE.md
// section 6).
//
// The quartermaster keeps one kit per seat of the standing doctrine (hull and fit, assembled, in
// the Armory division at the fleet base), and the Yard keeps an industrial for the mining op's
// hauler seat. A recruit with no hull in its seat's band is lent one (corparmory_issue.go).
// A lent kit stays the corporation's property, so every bot path that sells, escrows, insures,
// strips or registers a ship (it asks ownerID = charID) never sees it; loadShipsBulk reads
// loanedTo for the pilot's own boarded ship. A human's kit is handed over (corparmory_human.go).

const (
	kitArmory = "armory"
	kitYard   = "yard"

	kitPlanned = "planned" // parts on order
	kitReady   = "ready"   // assembled, on the shelf
	kitIssued  = "issued"  // in a member's hands

	settingArmorySpares = "rolebots.fleet.armorySpares"

	kitPlanEverySec   = 1800       // the chore's cadence per corporation
	kitStaleSec       = 14 * 86400 // a plan with no hull after this is dropped
	kitHullMissingSec = 3600       // a hull line filed as delivered with no hull on the shelf after this is lost
	kitFitTries       = 4          // hulls tried per seat
	kitFloorShare     = 0.5        // a kit's skills are what half the wing has
	kitRaceMatchBonus = 1.15
)

var (
	armorySparesV atomic.Int64
	kitPlanAt     sync.Map // corpID → unix of the last chore
)

func init() {
	armorySparesV.Store(1)
	armoryReserved = kitReservedParts
	registerCorpForget("kitPlanAt", func(id int64) { kitPlanAt.Delete(id) })
	// A forgotten (purged) pilot flies nothing: its open loan is settled by reconcileBotIssues
	// from the table, not this map.
	registerCharForget("loanedKits", func(id int64) {
		loanedKits.Range(func(k, v any) bool {
			if v.(int64) == id {
				unloanKit(k)
			}
			return true
		})
	})
}

func (m *Mod) loadArmorySettings(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingArmorySpares); err == nil && ok {
		var n int64
		if _, err := fmt.Sscan(v, &n); err == nil && n >= 0 && n <= 6 {
			armorySparesV.Store(n)
		}
	}
}

// corpKit is one rolebot_corp_kit row.
type corpKit struct {
	KitID, CorpID   int64
	Kind            string
	SeatRole, Job   string
	HullTypeID      int64
	HullClass       HullClass
	Tank            TankPref
	Fit             *Fit
	ShipItemID      int64
	StationID       int64
	Status          string
	Value           float64
	PlannedAt       int64
	// ForCharID is the pilot a siege kit was bought for (siegekits.go), 0 for a shelf kit.
	ForCharID int64
}

const kitCols = `kitID, corporationID, kind, seatRole, job, hullTypeID, hullClass, tank, fitJSON,
	shipItemID, stationID, status, value, plannedAt, forCharID`

func scanKit(sc interface{ Scan(...any) error }) (corpKit, error) {
	var k corpKit
	var fitJSON sql.NullString
	var class int
	var tank string
	err := sc.Scan(&k.KitID, &k.CorpID, &k.Kind, &k.SeatRole, &k.Job, &k.HullTypeID, &class, &tank,
		&fitJSON, &k.ShipItemID, &k.StationID, &k.Status, &k.Value, &k.PlannedAt, &k.ForCharID)
	if err != nil {
		return k, err
	}
	k.HullClass, k.Tank = HullClass(class), TankPref(tank)
	if fitJSON.Valid && fitJSON.String != "" {
		var f Fit
		if json.Unmarshal([]byte(fitJSON.String), &f) == nil {
			k.Fit = &f
		}
	}
	return k, nil
}

func (m *Mod) corpKits(ctx context.Context, corpID int64, where string, args ...any) ([]corpKit, error) {
	q := `SELECT ` + kitCols + ` FROM rolebot_corp_kit WHERE corporationID = ?`
	if where != "" {
		q += ` AND ` + where
	}
	rows, err := m.db.QueryContext(ctx, q+` ORDER BY kitID`, append([]any{corpID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("corpKits: %w", err)
	}
	defer rows.Close()
	var out []corpKit
	for rows.Next() {
		k, err := scanKit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ---- loan registry: which corporation hulls a pilot is flying ----

var (
	loanedKits   sync.Map // shipItemID → characterID
	loanedByChar sync.Map // characterID → shipItemID, the same loans the other way round
)

// loanKit records a kit lent to a pilot (both indexes).
func loanKit(shipItemID, charID int64) {
	if shipItemID == 0 {
		return
	}
	loanedKits.Store(shipItemID, charID)
	loanedByChar.Store(charID, shipItemID)
}

// unloanKit forgets a kit's loan. ship is the itemID (an any, so a Range key can be passed
// as it is).
func unloanKit(ship any) {
	if v, ok := loanedKits.Load(ship); ok {
		if cur, ok2 := loanedByChar.Load(v.(int64)); ok2 && cur == ship {
			loanedByChar.Delete(v.(int64))
		}
	}
	loanedKits.Delete(ship)
}

// loanedTo is the pilot a corporation kit is lent to, 0 when it is not lent.
func loanedTo(shipItemID int64) int64 {
	if v, ok := loanedKits.Load(shipItemID); ok {
		return v.(int64)
	}
	return 0
}

// holdsLoanedKit reports whether a pilot is flying a lent kit, from memory. The registry is
// filled at boot from every open issue and kept by issueKit and the returns, so planSolo and
// every gang seat can skip the issue read.
func holdsLoanedKit(charID int64) bool {
	_, ok := loanedByChar.Load(charID)
	return ok
}

// loadLoanedKits fills the registry from the open issues (boot).
func (m *Mod) loadLoanedKits(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT shipItemID, characterID FROM rolebot_corp_issue WHERE status = 'issued'`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ship, char int64
		if rows.Scan(&ship, &char) == nil && ship != 0 {
			loanKit(ship, char)
		}
	}
}

// ---- where the shelves are ----

// kitStation is where a corporation keeps its kits of one kind: the stored station while the
// office there stands, else the Armory goes to the Security arm's staging office (then HQ) and
// the Yard to where corporation ore is dropped (corpDepositTarget). 0 when no real office.
func (m *Mod) kitStation(ctx context.Context, corpID int64, kind string) (stationID, officeID int64) {
	var stored int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT stationID FROM rolebot_corp_armory WHERE corporationID = ? AND kind = ?`,
		corpID, kind).Scan(&stored)
	office := func(st int64) int64 {
		if st == 0 {
			return 0
		}
		o, _, err := evedb.GetCorpOfficeAt(ctx, m.db, corpID, st)
		if err != nil || o == 0 || o == st {
			return 0
		}
		return o
	}
	if o := office(stored); o != 0 {
		return stored, o
	}
	var pick int64
	switch kind {
	case kitArmory:
		if rows, err := m.db.QueryContext(ctx, `
			SELECT stationID, purposes FROM rolebot_corp_offices
			 WHERE corporationID = ? AND state <> 'dropping' ORDER BY stationID`, corpID); err == nil {
			for rows.Next() {
				var st int64
				var bits int
				if rows.Scan(&st, &bits) == nil && officeBits(corpID, st, bits)&officeStaging != 0 && pick == 0 {
					pick = st
				}
			}
			rows.Close()
		}
		if office(pick) == 0 {
			pick = m.corpHQStation(ctx, corpID)
		}
	case kitYard:
		_, pick, _ = m.corpDepositTarget(ctx, corpID)
	}
	o := office(pick)
	if o == 0 {
		return 0, 0
	}
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_armory (corporationID, kind, stationID) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE stationID = VALUES(stationID)`, corpID, kind, pick)
	return pick, o
}

// kitPurpose is the war-chest division a kit kind lives in.
func kitPurpose(kind string) divPurpose {
	if kind == kitYard {
		return purYard
	}
	return purArmory
}

// ---- what the shelf should hold ----

// kitTarget is one seat the shelf keeps a kit for.
type kitTarget struct {
	Kind string
	Spec seatSpec
	Job  string
	Want int
	// A siege kit (siegekits.go): the pilot it is bought for, and the deadline its requisitions
	// carry, short enough that they go out as funded buy trips, not patient buy orders.
	ForCharID int64
	Deadline  int64
}

// kitTargetsFor is the shelf's targets: one kit (armorySpares) per seat of the standing
// doctrine, one industrial for the mining op's hauler. Pure.
func kitTargetsFor(doc *FleetDoctrine, yard bool, spares int) []kitTarget {
	var out []kitTarget
	if doc != nil && spares > 0 {
		for _, s := range doc.Seats {
			job := s.Job
			if job == "" {
				job = JobCombat
			}
			out = append(out, kitTarget{Kind: kitArmory, Spec: s, Job: job, Want: spares})
		}
	}
	if yard {
		out = append(out, kitTarget{Kind: kitYard, Spec: haulerSeatSpec, Job: JobTrade, Want: 1})
	}
	return out
}

// kitMatches reports whether a kit fills a target's seat. Pure.
func kitMatches(k corpKit, kind string, s seatSpec) bool {
	if k.Kind != kind || k.SeatRole != s.Role {
		return false
	}
	if !hullInBand(k.HullClass, s) {
		return false
	}
	return s.Tank == TankNone || k.Tank == TankNone || k.Tank == s.Tank
}

// floorSkills is, per skill, the level at least share of the wing has, so a kit built on it
// can be flown by that share. Pure.
func floorSkills(sheets map[int64]map[int64]int, share float64) map[int64]int {
	if len(sheets) == 0 {
		return nil
	}
	levels := map[int64][]int{}
	for _, sh := range sheets {
		for sk, lv := range sh {
			levels[sk] = append(levels[sk], lv)
		}
	}
	need := int(float64(len(sheets))*share + 0.5)
	if need < 1 {
		need = 1
	}
	out := map[int64]int{}
	for sk, lv := range levels {
		if len(lv) < need {
			continue // fewer than share of the wing have it at all
		}
		sort.Sort(sort.Reverse(sort.IntSlice(lv)))
		out[sk] = lv[need-1]
	}
	return out
}

// kitValue is what a fit's hull and every line in it is worth.
func (m *Mod) kitValue(ctx context.Context, f *Fit) float64 {
	if f == nil {
		return 0
	}
	v := evedb.TypeValueOf(ctx, m.db, f.HullTypeID).Value
	for _, mod := range f.Modules {
		v += evedb.TypeValueOf(ctx, m.db, mod.TypeID).Value
		if mod.ChargeTypeID != 0 {
			v += evedb.TypeValueOf(ctx, m.db, mod.ChargeTypeID).Value * float64(mod.ChargeQty)
		}
	}
	for _, c := range f.Cargo {
		v += evedb.TypeValueOf(ctx, m.db, c.TypeID).Value * float64(c.Qty)
	}
	for _, d := range f.Drones {
		v += evedb.TypeValueOf(ctx, m.db, d.TypeID).Value * float64(d.Qty)
	}
	return v
}

// kitFit picks a hull and builds the kit's fit for one seat: the seat's band and layer, a
// hull on the market flyable on the wing's floor skills, the corporation's own race first.
// The first that fits under the ceiling wins.
func (m *Mod) kitFit(ctx context.Context, raceID int64, t kitTarget, floor map[int64]int, ceiling float64) (*Fit, *HullProfile, float64, error) {
	_, hulls, err := m.loadHullProfiles(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	sold, err := m.hullsOnTheMarket(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	type cand struct {
		h    *HullProfile
		rank float64
	}
	var cands []cand
	for _, h := range hulls {
		w := h.RoleWeight(t.Job)
		if w <= 0 || !hullInBand(h.Class, t.Spec) || !sold[h.TypeID] {
			continue
		}
		if ok, _ := m.pilotCanUse(ctx, h.TypeID, floor); !ok {
			continue
		}
		if raceID != 0 && h.RaceID == raceID {
			w *= kitRaceMatchBonus
		}
		cands = append(cands, cand{h, w})
	}
	if len(cands) == 0 {
		return nil, nil, 0, nil
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rank > cands[j].rank })
	cat, err := m.buyableModules(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	dcat, _ := m.buyableDrones(ctx)
	// A disposable seat (a gank striker) is where price is a merit (fitselect.go): every try is
	// generated and rankDisposable picks the cheapest. An ordinary seat takes the first that fits.
	var disp []disposableCand
	for i, c := range cands {
		if i >= kitFitTries {
			break
		}
		tpl, _, err := m.generateFitFor(ctx, 0, t.Job, hullForSeat(c.h, t.Spec), floor, cat, dcat, nil, nil, fitTaste{})
		if err != nil || tpl == nil || tpl.Fit == nil {
			continue
		}
		v := m.kitValue(ctx, tpl.Fit)
		if ceiling > 0 && v > ceiling {
			continue
		}
		if !t.Spec.Disposable {
			return tpl.Fit, c.h, v, nil
		}
		power := 0.0
		if s, err := m.ComputeFitStats(ctx, tpl.Fit); err == nil {
			power = jobPower(t.Job, s)
		}
		disp = append(disp, disposableCand{fit: tpl.Fit, hull: c.h, cost: v, power: power})
	}
	if i := rankDisposable(disp); i >= 0 {
		return disp[i].fit, disp[i].hull, disp[i].cost, nil
	}
	return nil, nil, 0, nil
}

// disposableCand is one fit in the running for a disposable seat.
type disposableCand struct {
	fit   *Fit
	hull  *HullProfile
	cost  float64
	power float64
}

// rankDisposable picks the cheapest candidate that clears the job floor (power > 0, a known
// cost): the index into cands, or -1. Pure. Ties keep the earlier candidate, which encodes the
// hull ranking. A hull that cannot be given its guns on this market has a price and no job.
func rankDisposable(cands []disposableCand) int {
	best := -1
	for i, c := range cands {
		if c.power <= 0 || c.cost <= 0 {
			continue
		}
		if best < 0 || c.cost < cands[best].cost {
			best = i
		}
	}
	return best
}

// ---- the quartermaster's chore ----

// quartermasterChore keeps a corporation's shelves: plans a kit for every seat short of one,
// requisitions its parts, assembles what has arrived.
func (m *Mod) quartermasterChore(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts) error {
	if v, ok := kitPlanAt.Load(c.CorpID); ok && clock.Unix()-v.(int64) < kitPlanEverySec {
		return nil
	}
	kitPlanAt.Store(c.CorpID, clock.Unix())
	m.pruneIssues(ctx)
	m.reconcileBotIssues(ctx, c)
	m.armoryWatchHumans(ctx, c)
	p := m.corpDivs(ctx, c.CorpID)
	var raceID int64
	if facts != nil {
		raceID = facts.RaceID
	}

	var doc *FleetDoctrine
	if _, ok := p.Flag(purArmory); ok && fleetArmoryOn.Load() {
		if have := m.fleetDoctrinesFor(ctx, c.CorpID); len(have) > 0 {
			kind := kindForOrder(have, m.corpDoctrineFor(ctx, c.CorpID), false, false, relCorpAtWar(c.CorpID), false)
			for _, d := range have {
				if d.Kind == kind {
					d := d
					doc = &d
				}
			}
		}
	}
	_, yardOK := p.Flag(purYard)
	targets := kitTargetsFor(doc, yardOK && fleetYardOn.Load(), int(armorySparesV.Load()))

	kits, err := m.corpKits(ctx, c.CorpID, "")
	if err != nil {
		return err
	}
	// Assemble first: a kit whose parts arrived goes on the shelf this pass.
	for i := range kits {
		if kits[i].Status != kitPlanned {
			continue
		}
		if err := m.assembleKit(ctx, c, &kits[i]); err != nil {
			m.logBot(c.CharID, logError, "ceo", "kit %d: %v", kits[i].KitID, err)
		}
	}

	// Then plan what is short.
	var floor map[int64]int
	floorLoaded := false
	for _, t := range targets {
		have := 0
		for _, k := range kits {
			// A kit bought for one pilot's siege seat is not the shelf's (siegekits.go).
			if k.ForCharID == 0 && kitMatches(k, t.Kind, t.Spec) {
				have++
			}
		}
		if have >= t.Want {
			continue
		}
		st, _ := m.kitStation(ctx, c.CorpID, t.Kind)
		if st == 0 {
			continue
		}
		if !floorLoaded {
			floor = m.wingFloorSkills(ctx, c.CorpID, t.Kind)
			floorLoaded = true
		}
		ceiling := 0.0
		if doc != nil && t.Kind == kitArmory {
			ceiling = doc.CostCeiling
		}
		fit, hull, value, err := m.kitFit(ctx, raceID, t, floor, ceiling)
		if err != nil {
			return err
		}
		if fit == nil {
			continue
		}
		free, _, ferr := m.corpFreeCapital(ctx, c.CorpID, ch)
		if ferr != nil || free < value {
			if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "kit.short") >= 86400 {
				m.ceoLog(ctx, c, powerProduction, "kit.short", hull.TypeID, value,
					"cannot afford a %s kit (%s) for its %s yet", hull.Name, iskString(value), kitLabel(t.Kind))
			}
			break // the cheapest are not cheaper further down; try next pass
		}
		if err := m.planKit(ctx, c, t, fit, hull, value, st); err != nil {
			return err
		}
		break // one new kit a pass: spreads the spend
	}
	return nil
}

func kitLabel(kind string) string {
	if kind == kitYard {
		return "Fleet Yard"
	}
	return "Armory"
}

// wingFloorSkills is the floor skills of who would borrow this kind: the Security arm for the
// Armory, the Mining arm for the Yard, the whole roster when the arm is empty.
func (m *Mod) wingFloorSkills(ctx context.Context, corpID int64, kind string) map[int64]int {
	arm := armSecurity
	if kind == kitYard {
		arm = armMining
	}
	ids, err := int64Column(ctx, m, `
		SELECT characterID FROM rolebot_corp_arm_members WHERE corporationID = ? AND arm = ?`,
		corpID, string(arm))
	if err != nil || len(ids) == 0 {
		ids, err = int64Column(ctx, m, `
			SELECT b.characterID FROM rolebot_characters b
			  JOIN character_ ch ON ch.characterID = b.characterID
			 WHERE b.enabled = 1 AND ch.corporationID = ?`, corpID)
		if err != nil {
			return nil
		}
	}
	sheets, err := m.skillSheetsFor(ctx, ids)
	if err != nil {
		return nil
	}
	return floorSkills(sheets, kitFloorShare)
}

// planKit writes a planned kit and requisitions everything it needs that the Armory and
// Spoils do not already hold.
func (m *Mod) planKit(ctx context.Context, c *corpCEO, t kitTarget, fit *Fit, hull *HullProfile, value float64, stationID int64) error {
	flag, ok := m.corpDivFlag(ctx, c.CorpID, kitPurpose(t.Kind))
	if !ok {
		return nil
	}
	raw, err := json.Marshal(fit)
	if err != nil {
		return err
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_kit
			(corporationID, kind, seatRole, job, hullTypeID, hullClass, tank, fitJSON, stationID, status, value, plannedAt, forCharID)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CorpID, t.Kind, t.Spec.Role, t.Job, hull.TypeID, int(hull.Class), string(t.Spec.Tank), string(raw),
		stationID, kitPlanned, value, clock.Unix(), t.ForCharID)
	if err != nil {
		return fmt.Errorf("plan kit: %w", err)
	}
	kitID, _ := res.LastInsertId()
	officeID, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, c.CorpID, stationID)
	need := kitParts(fit)
	onHand := m.kitStock(ctx, c.CorpID, officeID, flag)
	reserved := kitReservedParts(ctx, m, c.CorpID) // other planned kits' claims
	posted := 0
	for typeID, qty := range need {
		have := onHand[typeID] - reserved[typeID] + qty // this kit's own claim is in reserved
		short := qty - max(0, have)
		short -= m.reqPending(ctx, c.CorpID, typeID, stationID, flag)
		if short <= 0 {
			continue
		}
		if _, err := m.postRequisition(ctx, c, requisition{TypeID: typeID, Qty: short,
			DestStationID: stationID, DestFlag: flag, Requester: "security", RefKind: "kit", RefID: kitID,
			Deadline: t.Deadline, Note: "kit: " + hull.Name}); err != nil {
			return err
		}
		posted++
	}
	m.ceoLog(ctx, c, powerProduction, "kit.plan", hull.TypeID, value,
		"planned a %s kit for its %s (%s seat, %s) — %d line(s) requisitioned",
		hull.Name, kitLabel(t.Kind), seatRoleLabel(t.Spec.Role), iskString(value), posted)
	return nil
}

func seatRoleLabel(role string) string {
	if role == "" {
		return "line"
	}
	return role
}

// kitParts is every item a fit needs, hull included, by typeID. Pure.
func kitParts(f *Fit) map[int64]int64 {
	out := map[int64]int64{}
	if f == nil {
		return out
	}
	out[f.HullTypeID]++
	for _, mod := range f.Modules {
		out[mod.TypeID]++
		if mod.ChargeTypeID != 0 && mod.ChargeQty > 0 {
			out[mod.ChargeTypeID] += mod.ChargeQty
		}
	}
	for _, c := range f.Cargo {
		out[c.TypeID] += c.Qty
	}
	for _, d := range f.Drones {
		out[d.TypeID] += d.Qty
	}
	return out
}

// kitStock is what the Armory (or Yard) and Spoils hold at one office, loose stacks only, by
// typeID.
func (m *Mod) kitStock(ctx context.Context, corpID, officeID, flag int64) map[int64]int64 {
	out := map[int64]int64{}
	flags := []any{flag}
	if sf, ok := m.corpDivFlag(ctx, corpID, purSpoils); ok && sf != flag {
		flags = append(flags, sf)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, SUM(quantity) FROM entity
		 WHERE locationID = ? AND ownerID = ? AND singleton = 0 AND flag IN (`+placeholders(len(flags))+`)
		 GROUP BY typeID`, append([]any{officeID, corpID}, flags...)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t, q int64
		if rows.Scan(&t, &q) == nil {
			out[t] = q
		}
	}
	return out
}

// kitReservedParts is what the corporation's planned kits still need, by typeID, held back
// from the Spoils listing (corpspoils.go).
func kitReservedParts(ctx context.Context, m *Mod, corpID int64) map[int64]int64 {
	kits, err := m.corpKits(ctx, corpID, "status = ?", kitPlanned)
	if err != nil || len(kits) == 0 {
		return nil
	}
	out := map[int64]int64{}
	for _, k := range kits {
		if k.Fit == nil {
			continue
		}
		var missing []ShopItem
		if k.ShipItemID != 0 {
			missing, _ = m.shipMissing(ctx, k.ShipItemID, k.Fit)
		} else {
			for t, q := range kitParts(k.Fit) {
				missing = append(missing, ShopItem{TypeID: t, Qty: q})
			}
		}
		for _, it := range missing {
			out[it.TypeID] += it.Qty
		}
	}
	return out
}

// assembleKit puts the hull together and fits what has arrived. A kit with every module,
// drone and round aboard goes on the shelf.
func (m *Mod) assembleKit(ctx context.Context, c *corpCEO, k *corpKit) error {
	if k.Fit == nil {
		_, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_kit WHERE kitID = ?`, k.KitID)
		return err
	}
	flag, ok := m.corpDivFlag(ctx, c.CorpID, kitPurpose(k.Kind))
	if !ok {
		return nil
	}
	officeID, _, err := evedb.GetCorpOfficeAt(ctx, m.db, c.CorpID, k.StationID)
	if err != nil || officeID == 0 || officeID == k.StationID {
		return nil // the office went; the plan waits for a new base
	}
	if k.ShipItemID == 0 {
		hull := m.corpStackAt(ctx, c.CorpID, officeID, k.Fit.HullTypeID, flag)
		if hull == 0 {
			if m.kitHullLost(ctx, k) {
				m.dropKitPlan(ctx, k.KitID)
				m.ceoLog(ctx, c, powerProduction, "kit.nohull", k.Fit.HullTypeID, k.Value,
					"the hull for its %s kit never reached the shelf — planning it again",
					m.typeNameOr(ctx, k.Fit.HullTypeID))
				return nil
			}
			if clock.Unix()-k.PlannedAt > kitStaleSec {
				m.dropKitPlan(ctx, k.KitID)
			}
			return nil
		}
		id, got, err := m.takeUnits(ctx, hull, 1)
		if err != nil || got != 1 {
			return err
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE entity SET singleton = 1, flag = ? WHERE itemID = ?`, flag, id); err != nil {
			return err
		}
		k.ShipItemID = id
		if _, err := m.db.ExecContext(ctx,
			`UPDATE rolebot_corp_kit SET shipItemID = ? WHERE kitID = ?`, id, k.KitID); err != nil {
			return err
		}
	}
	missing, err := m.shipMissing(ctx, k.ShipItemID, k.Fit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			m.dropKitPlan(ctx, k.KitID)
		}
		return err
	}
	left := 0
	for _, it := range missing {
		stack := m.corpStackAt(ctx, c.CorpID, officeID, it.TypeID, flag)
		if stack == 0 {
			if sf, ok := m.corpDivFlag(ctx, c.CorpID, purSpoils); ok && sf != flag {
				stack = m.corpStackAt(ctx, c.CorpID, officeID, it.TypeID, sf)
			}
		}
		if stack == 0 {
			left++
			continue
		}
		if err := m.applyKitItem(ctx, k.ShipItemID, it, stack); err != nil {
			rlogf("rolebots: kit %d fit %d: %v", k.KitID, it.TypeID, err)
			left++
		}
	}
	if left > 0 {
		return nil
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_corp_kit SET status = ? WHERE kitID = ?`, kitReady, k.KitID); err != nil {
		return err
	}
	k.Status = kitReady
	m.ceoLog(ctx, c, powerProduction, "kit.ready", k.HullTypeID, k.Value,
		"put a fitted %s on the %s shelf at %s", m.typeNameOr(ctx, k.HullTypeID), kitLabel(k.Kind),
		m.stationLabel(ctx, k.StationID))
	return nil
}

// dropKitPlan forgets a plan that cannot be met, and its open requisitions.
func (m *Mod) dropKitPlan(ctx context.Context, kitID int64) {
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_kit WHERE kitID = ? AND status = ?`, kitID, kitPlanned)
	// A line still on the market or with a buyer is taken back first: the bid's escrow and the
	// buyer's advance return to the corporation. A line Industry is building finishes and files
	// into the division, where the next kit's plan counts it as stock.
	if reqs, err := m.queryReqs(ctx, `WHERE refKind = 'kit' AND refID = ? AND status IN ('bidding','buying')`, kitID); err == nil {
		for i := range reqs {
			r := &reqs[i]
			if r.Status == "bidding" {
				m.cancelReqBid(ctx, r)
			} else {
				m.reclaimAdvance(ctx, r)
			}
		}
	}
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_requisition SET status = 'void'
		 WHERE refKind = 'kit' AND refID = ? AND status IN ('open','bidding','buying')`, kitID)
}

// kitHullLost reports whether a kit's hull line was filed as delivered more than
// kitHullMissingSec ago while no hull sits on the shelf for it: the hull was never bought
// or went elsewhere, and the plan waits for nothing.
func (m *Mod) kitHullLost(ctx context.Context, k *corpKit) bool {
	var closedAt int64
	err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(closedAt), 0) FROM rolebot_corp_requisition
		 WHERE refKind = 'kit' AND refID = ? AND typeID = ? AND status = 'done'`,
		k.KitID, k.Fit.HullTypeID).Scan(&closedAt)
	return err == nil && closedAt > 0 && clock.Unix()-closedAt > kitHullMissingSec
}

// corpStackAt is the biggest loose corporation stack of a type in one office division, 0 for
// none.
func (m *Mod) corpStackAt(ctx context.Context, corpID, officeID, typeID, flag int64) int64 {
	var id int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT itemID FROM entity
		 WHERE locationID = ? AND ownerID = ? AND typeID = ? AND flag = ? AND singleton = 0 AND quantity > 0
		 ORDER BY quantity DESC LIMIT 1`, officeID, corpID, typeID, flag).Scan(&id)
	return id
}

// applyKitItem fits one line onto a corporation hull out of a corporation stack. Ownership
// never changes and nothing is boarded (every primitive is shipID-based). An occupied slot is
// left alone, so the kit stays short that line until the slot is cleared.
func (m *Mod) applyKitItem(ctx context.Context, shipID int64, it ShopItem, stack int64) error {
	id, got, err := m.takeUnits(ctx, stack, it.Qty)
	if err != nil || got <= 0 {
		return err
	}
	switch it.Dest {
	case "module":
		var occupied int
		_ = m.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entity WHERE locationID = ? AND flag = ?`, shipID, it.Flag).Scan(&occupied)
		if occupied > 0 {
			return fmt.Errorf("slot %d occupied", it.Flag)
		}
		if err := evedb.FitModuleToShip(ctx, m.db, id, shipID, it.Flag); err != nil {
			return err
		}
		return evedb.SetModuleOnlineState(ctx, m.db, id, true)
	case "charge":
		// Only into a module that fires it, and no more than it holds; the rest rides in the hold
		// (fitslotcharge.go).
		room, err := m.slotChargeRoom(ctx, shipID, it.Flag, it.TypeID, 0)
		if err != nil {
			return err
		}
		if room <= 0 {
			_, err := m.db.ExecContext(ctx, `UPDATE entity SET locationID = ?, flag = 5 WHERE itemID = ?`, shipID, id)
			return err
		}
		if got > room {
			spill, err := evedb.SplitStackN(ctx, m.db, id, got-room)
			if err != nil {
				return err
			}
			if _, err := m.db.ExecContext(ctx, `UPDATE entity SET locationID = ?, flag = 5 WHERE itemID = ?`, shipID, spill); err != nil {
				return err
			}
			got = room
		}
		return evedb.LoadChargeIntoModule(ctx, m.db, id, shipID, it.Flag, got)
	case "cargo":
		_, err := m.db.ExecContext(ctx, `UPDATE entity SET locationID = ?, flag = 5 WHERE itemID = ?`, shipID, id)
		return err
	case "drone":
		return evedb.MoveStackToDroneBay(ctx, m.db, id, shipID)
	case "implant":
		// A kit is lent and returned, and an implant is destroyed on unplug, so a kit never carries one.
		return fmt.Errorf("a kit cannot lend an implant (%s)", it.Name)
	}
	return fmt.Errorf("unknown kit line %q", it.Dest)
}
