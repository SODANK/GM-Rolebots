package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	evedb "gurimoon/db"
)

// The doctrine's paper half: gathering the inputs, writing the rows and handing a fleet
// its doctrine at formation. A row is written only when the hash of its inputs changes
// (doctrineInputs.hash), so a quiet corporation costs one hash compare per desk pass.
// Rows an operator pinned (override = 1) are read and never rewritten.

const (
	settingFleetDoctrine = "rolebots.fleet.doctrine"
	// doctrineDeskEverySec is how often the Security desk re-derives; the inputs move at the
	// census timescale (wingCensusSec) at the fastest.
	doctrineDeskEverySec = 15 * 60
	// doctrineCacheSec is how long a corporation's read doctrines stand in
	// memory between desk passes; derivation refreshes the entry directly.
	doctrineCacheSec = 5 * 60
	// doctrineCostCacheSec is how long the per-class hull price stands.
	doctrineCostCacheSec = 3600
)

// dialFleetDoctrine is the switch: 0 restores gangComposition's single curve for every
// corporation. It is on by default.
var dialFleetDoctrine = &pvpDial{key: settingFleetDoctrine, def: 1, lo: 0, hi: 1, step: 1,
	label: "Fleet doctrines: corporations derive and fly their own fleet shapes (1 = on, 0 = the old single curve)"}

func fleetDoctrineOn() bool { return dialFleetDoctrine.Get() >= 0.5 }

// ensureDoctrineTables is called from ensureTables.
func ensureDoctrineTables(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		// One row per (corporation, kind). inputHash is what the last derivation read;
		// override = 1 pins an operator-edited row that derivation leaves alone.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_doctrine (
			corporationID BIGINT NOT NULL,
			kind          VARCHAR(12) NOT NULL,
			tank          VARCHAR(8) NOT NULL DEFAULT '',
			style         TINYINT NOT NULL DEFAULT 0,
			hullMin       TINYINT NOT NULL DEFAULT 0,
			hullMax       TINYINT NOT NULL DEFAULT 0,
			costCeiling   DOUBLE NOT NULL DEFAULT 0,
			seats         TEXT NULL,
			inputHash     BIGINT UNSIGNED NOT NULL DEFAULT 0,
			derivedAt     BIGINT NOT NULL DEFAULT 0,
			override      TINYINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, kind)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("ensureDoctrineTables: %w", err)
		}
	}
	return nil
}

type fleetDocEntry struct {
	at   int64
	hash uint64
	docs []FleetDoctrine
}

var (
	fleetDocMu    sync.Mutex
	fleetDocCache = map[int64]*fleetDocEntry{}

	hullCostMu sync.Mutex
	hullCostAt int64
	hullCost   map[HullClass]float64
)

// forgetFleetDoctrines drops the in-memory copy (tests, and the ESP override path).
func forgetFleetDoctrines() {
	fleetDocMu.Lock()
	fleetDocCache = map[int64]*fleetDocEntry{}
	fleetDocMu.Unlock()
}

func init() {
	registerCorpForget("fleetDocCache", func(id int64) {
		fleetDocMu.Lock()
		delete(fleetDocCache, id)
		fleetDocMu.Unlock()
	})
	registerSweep("fleetDocCacheSweep", func() {
		now := clock.Unix()
		fleetDocMu.Lock()
		for id, e := range fleetDocCache {
			if now-e.at >= 2*doctrineCacheSec {
				delete(fleetDocCache, id)
			}
		}
		fleetDocMu.Unlock()
	})
}

// ── reading ─────────────────────────────────────────────────────────────────

// fleetDoctrinesFor is a corporation's doctrines, every kind it has. It is empty for a
// corporation with none (operator gangs, unclassified corps, switch off), which tells the
// caller to use gangComposition's single curve.
func (m *Mod) fleetDoctrinesFor(ctx context.Context, corpID int64) []FleetDoctrine {
	if corpID == 0 || !fleetDoctrineOn() {
		return nil
	}
	now := clock.Unix()
	fleetDocMu.Lock()
	if e, ok := fleetDocCache[corpID]; ok && now-e.at < doctrineCacheSec {
		docs := e.docs
		fleetDocMu.Unlock()
		return docs
	}
	fleetDocMu.Unlock()

	docs, hash, err := m.readFleetDoctrines(ctx, corpID)
	if err != nil {
		rlogf("rolebots: doctrines corp=%d: %v", corpID, err)
		return nil
	}
	fleetDocMu.Lock()
	fleetDocCache[corpID] = &fleetDocEntry{at: now, hash: hash, docs: docs}
	fleetDocMu.Unlock()
	return docs
}

// fleetDoctrineFor is one kind of a corporation's doctrines, or nil.
func (m *Mod) fleetDoctrineFor(ctx context.Context, corpID int64, kind doctrineKind) *FleetDoctrine {
	for _, d := range m.fleetDoctrinesFor(ctx, corpID) {
		if d.Kind == kind {
			d := d
			return &d
		}
	}
	return nil
}

func (m *Mod) readFleetDoctrines(ctx context.Context, corpID int64) ([]FleetDoctrine, uint64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT kind, tank, style, hullMin, hullMax, costCeiling, COALESCE(seats, ''), inputHash, derivedAt, override
		FROM rolebot_corp_doctrine WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []FleetDoctrine
	var hash uint64
	for rows.Next() {
		var d FleetDoctrine
		var kind, tank, seats string
		var style, lo, hi, override int
		if err := rows.Scan(&kind, &tank, &style, &lo, &hi, &d.CostCeiling, &seats, &hash, &d.DerivedAt, &override); err != nil {
			return nil, 0, err
		}
		d.CorpID, d.Kind, d.Tank = corpID, doctrineKind(kind), TankPref(tank)
		d.Style, d.HullMin, d.HullMax = combatStance(style), HullClass(lo), HullClass(hi)
		d.Override = override != 0
		if seats != "" {
			_ = json.Unmarshal([]byte(seats), &d.Seats)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return kindIndex(out[i].Kind) < kindIndex(out[j].Kind) })
	return out, hash, rows.Err()
}

// ── deriving ────────────────────────────────────────────────────────────────

// deriveCorpDoctrines is the Security desk's pass: gather the inputs, compare
// the hash with what the rows were derived from, and rewrite only on a change.
func (m *Mod) deriveCorpDoctrines(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts) {
	in, err := m.gatherDoctrineInputs(ctx, c, ch, facts)
	if err != nil {
		m.logBot(c.CharID, logError, "ceo", "doctrine inputs: %v", err)
		return
	}
	hash := in.hash()
	prev, prevHash, err := m.readFleetDoctrines(ctx, c.CorpID)
	if err != nil {
		m.logBot(c.CharID, logError, "ceo", "doctrine rows: %v", err)
		return
	}
	if prevHash == hash && len(prev) > 0 {
		return // nothing moved
	}
	now := clock.Unix()
	docs := deriveFleetDoctrines(in, now)
	pinned := map[doctrineKind]FleetDoctrine{}
	for _, d := range prev {
		if d.Override {
			pinned[d.Kind] = d
		}
	}
	// Write derived rows except where the operator pinned the kind; delete derived rows for
	// kinds no longer offered.
	keep := map[doctrineKind]bool{}
	for i := range docs {
		keep[docs[i].Kind] = true
		if p, ok := pinned[docs[i].Kind]; ok {
			docs[i] = p
			continue
		}
		seats, _ := json.Marshal(docs[i].Seats)
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_doctrine
				(corporationID, kind, tank, style, hullMin, hullMax, costCeiling, seats, inputHash, derivedAt, override)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
			ON DUPLICATE KEY UPDATE tank = VALUES(tank), style = VALUES(style), hullMin = VALUES(hullMin),
				hullMax = VALUES(hullMax), costCeiling = VALUES(costCeiling), seats = VALUES(seats),
				inputHash = VALUES(inputHash), derivedAt = VALUES(derivedAt)`,
			c.CorpID, string(docs[i].Kind), string(docs[i].Tank), int(docs[i].Style),
			int(docs[i].HullMin), int(docs[i].HullMax), docs[i].CostCeiling, string(seats), hash, now); err != nil {
			m.logBot(c.CharID, logError, "ceo", "doctrine %s: %v", docs[i].Kind, err)
			return
		}
	}
	for _, d := range prev {
		if !keep[d.Kind] && !d.Override {
			_, _ = m.db.ExecContext(ctx,
				`DELETE FROM rolebot_corp_doctrine WHERE corporationID = ? AND kind = ? AND override = 0`,
				c.CorpID, string(d.Kind))
		}
	}
	// Pinned kinds the derivation dropped stay as rows and stay in the list.
	for k, p := range pinned {
		if !keep[k] {
			docs = append(docs, p)
		}
	}
	// Pinned rows keep their old hash and the compare above reads whichever row comes last,
	// so every row is stamped with the new hash.
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_doctrine SET inputHash = ? WHERE corporationID = ?`, hash, c.CorpID)
	sort.Slice(docs, func(i, j int) bool { return kindIndex(docs[i].Kind) < kindIndex(docs[j].Kind) })

	fleetDocMu.Lock()
	fleetDocCache[c.CorpID] = &fleetDocEntry{at: now, hash: hash, docs: docs}
	fleetDocMu.Unlock()

	if doctrinesChanged(prev, docs) {
		lines := make([]string, 0, len(docs))
		for _, d := range docs {
			lines = append(lines, doctrineSummary(d))
		}
		if len(lines) == 0 {
			lines = append(lines, "none — no line the wing can fly")
		}
		m.ceoLog(ctx, c, powerRoles, "doctrine", 0, 0,
			"set the fleet doctrines: %s (wing %d, %s)", strings.Join(lines, "; "), in.Wing, doctrineBandWhy(in))
	}
}

// doctrinesChanged reports whether anything a pilot would notice moved: the
// set of kinds, a band, a layer or a style.
func doctrinesChanged(prev, next []FleetDoctrine) bool {
	if len(prev) != len(next) {
		return true
	}
	byKind := map[doctrineKind]FleetDoctrine{}
	for _, d := range prev {
		byKind[d.Kind] = d
	}
	for _, d := range next {
		p, ok := byKind[d.Kind]
		if !ok || p.Tank != d.Tank || p.Style != d.Style || p.HullMin != d.HullMin || p.HullMax != d.HullMax {
			return true
		}
	}
	return false
}

// doctrineBandWhy words the band's reasons for the log.
func doctrineBandWhy(in doctrineInputs) string {
	lo, hi := hullBandFor(in.Able, in.Wing)
	_, capped := capBandByCost(lo, hi, in.ClassCost, in.CostCeiling)
	if capped < hi {
		return fmt.Sprintf("%d can fly a %s but the treasury replaces nothing over %s", in.Able[hi], hi, fmtISK(in.CostCeiling))
	}
	return fmt.Sprintf("%d of them can fly a %s", in.Able[hi], hi)
}

// gatherDoctrineInputs reads the five inputs for one corporation.
func (m *Mod) gatherDoctrineInputs(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts) (doctrineInputs, error) {
	in := doctrineInputs{CorpID: c.CorpID, ROE: m.corpDoctrineFor(ctx, c.CorpID), AtWar: relCorpAtWar(c.CorpID)}
	if facts != nil {
		in.RaceID = facts.RaceID
	}
	if in.RaceID == 0 {
		in.RaceID = raceOf(c.CharID)
	}
	// The wing: the Security arm's members, else every enabled bot in the corporation (a
	// corporation too small for arms fields its whole roster).
	wing, err := int64Column(ctx, m, `
		SELECT characterID FROM rolebot_corp_arm_members WHERE corporationID = ? AND arm = ?`,
		c.CorpID, string(armSecurity))
	if err != nil {
		return in, err
	}
	if len(wing) == 0 {
		wing, err = int64Column(ctx, m, `
			SELECT b.characterID FROM rolebot_characters b
			JOIN character_ ch ON ch.characterID = b.characterID
			WHERE b.enabled = 1 AND ch.corporationID = ?`, c.CorpID)
		if err != nil {
			return in, err
		}
	}
	in.Wing = len(wing)
	sheets, err := m.skillSheetsFor(ctx, wing)
	if err != nil {
		return in, err
	}
	in.Able, err = m.classAbility(ctx, in.RaceID, sheets)
	if err != nil {
		return in, err
	}
	// The market, per layer and for electronics.
	if cat, err := m.buyableModules(ctx); err == nil {
		for _, mm := range cat {
			switch {
			case mm.GroupID == grpShieldTransporter:
				in.LogiShield = true
			case mm.GroupID == grpArmorRepProjector:
				in.LogiArmor = true
			case isEwarGroup(mm.GroupID):
				in.Ewar = true
			}
		}
	}
	in.ClassCost = m.hullClassCosts(ctx)
	// An NPC chair has no treasury to replace hulls from: its ceiling is its
	// spending budget or its median member's wallet (npcchair.go).
	if c.Origin == ceoOriginNPC {
		in.CostCeiling = m.npcDoctrineCeiling(ctx, c.CorpID, wing)
		return in, nil
	}
	// The ceiling: the charter's SRP cap, floored by a share of free capital, so a
	// corporation that cannot afford its own cap flies below it.
	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err == nil {
		in.CostCeiling = free * doctrineCapitalFrac
	}
	if ch.SRPCap > 0 && (in.CostCeiling <= 0 || ch.SRPCap < in.CostCeiling) {
		in.CostCeiling = ch.SRPCap
	}
	in.CostCeiling = math.Round(in.CostCeiling)
	return in, nil
}

// skillSheetsFor is rosterSkillSheets for a named set of pilots.
func (m *Mod) skillSheetsFor(ctx context.Context, ids []int64) (map[int64]map[int64]int, error) {
	out := map[int64]map[int64]int{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.ownerID, e.typeID, COALESCE(a.valueInt, a.valueFloat, 0)
		FROM entity e
		LEFT JOIN entity_attributes a ON a.itemID = e.itemID AND a.attributeID = 280
		WHERE e.flag IN (7, 61) AND e.ownerID IN (`+placeholders(len(ids))+`)`, int64Args(ids)...)
	if err != nil {
		return nil, fmt.Errorf("skillSheetsFor: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var charID, skillID int64
		var level float64
		if err := rows.Scan(&charID, &skillID, &level); err != nil {
			return nil, err
		}
		if out[charID] == nil {
			out[charID] = map[int64]int{}
		}
		if lv := int(level); lv > out[charID][skillID] {
			out[charID][skillID] = lv
		}
	}
	for _, id := range ids {
		if out[id] == nil {
			out[id] = map[int64]int{}
		}
	}
	return out, rows.Err()
}

// classAbility counts, per fighting class, the pilots who can fly at least
// one T1 hull of that class of the given race (any race's when the
// corporation has none). Skill requirements are the hull's own
// (requiredSkills, cached per type).
func (m *Mod) classAbility(ctx context.Context, raceID int64, sheets map[int64]map[int64]int) (map[HullClass]int, error) {
	_, hulls, err := m.loadHullProfiles(ctx)
	if err != nil {
		return nil, err
	}
	byClass := map[HullClass][]*HullProfile{}
	for _, h := range hulls {
		if g, ok := t1LineGroups[h.Class]; !ok || h.GroupID != g {
			continue
		}
		if raceID != 0 && h.RaceID != raceID {
			continue
		}
		byClass[h.Class] = append(byClass[h.Class], h)
	}
	able := map[HullClass]int{}
	for _, c := range hullClassOrder {
		var reqs [][]evedb.SkillReq
		for _, h := range byClass[c] {
			r, err := m.requiredSkills(ctx, h.TypeID)
			if err != nil {
				continue
			}
			reqs = append(reqs, r)
		}
		if len(reqs) == 0 {
			continue
		}
		for _, sheet := range sheets {
			for _, r := range reqs {
				if len(evedb.UnmetSkillReqs(r, sheet, nil)) == 0 {
					able[c]++
					break
				}
			}
		}
	}
	return able, nil
}

// hullClassCosts is the typical T1 hull price per class: the average basePrice of the
// class's group, which is also the insurance and repair base and what the SRP cap is
// measured in.
func (m *Mod) hullClassCosts(ctx context.Context) map[HullClass]float64 {
	now := clock.Unix()
	hullCostMu.Lock()
	if hullCost != nil && now-hullCostAt < doctrineCostCacheSec {
		out := hullCost
		hullCostMu.Unlock()
		return out
	}
	hullCostMu.Unlock()
	out := map[HullClass]float64{}
	for c, g := range t1LineGroups {
		var avg sql.NullFloat64
		if err := m.db.QueryRowContext(ctx,
			`SELECT AVG(basePrice) FROM invTypes WHERE groupID = ? AND published = 1 AND basePrice > 0`, g).Scan(&avg); err == nil && avg.Valid {
			out[c] = avg.Float64
		}
	}
	hullCostMu.Lock()
	hullCost, hullCostAt = out, now
	hullCostMu.Unlock()
	return out
}

// ── at formation ────────────────────────────────────────────────────────────

// fleetDoctrineForFleet is the doctrine a fleet raised from this seat flies, or nil for
// gangComposition's single curve: operator gangs, corporations without doctrines, the
// switch off, and mining fleets (which have a shape of their own).
func (m *Mod) fleetDoctrineForFleet(ctx context.Context, seat fcSeat, activity string, order *fleetOrderSpec) *FleetDoctrine {
	switch activity {
	case ActCombat, ActSecurity, ActPiracy:
	default:
		return nil
	}
	have := m.fleetDoctrinesFor(ctx, seat.corpID)
	if len(have) == 0 {
		// A corporation with no doctrine of its own flies its alliance
		// executor's (gangalliance.go): one standard for the bloc.
		if exec := m.allianceExecutor(ctx, m.corpAllianceID(ctx, seat.corpID)); exec != 0 && exec != seat.corpID {
			have = m.fleetDoctrinesFor(ctx, exec)
		}
	}
	// A siege's one fleet flies the siege muster doctrine (siegefleet.go): any combat hull the
	// pilot can bring, on the corporation's layer. It needs no derived doctrine of its own.
	if order != nil && order.SiegeID != 0 {
		tank, ceiling := TankNone, 0.0
		if len(have) > 0 {
			tank, ceiling = have[0].Tank, have[0].CostCeiling
		}
		return siegeMusterDoctrine(seat.corpID, tank, ceiling)
	}
	if len(have) == 0 {
		return nil
	}
	// A call to arms is a siege-or-defend fleet by definition (gangcalls.go), unless no commander
	// could seat the siege doctrine and it flies the war line instead (LineDoctrine).
	siege := order != nil && order.CallToArms && !order.LineDoctrine
	if !siege && order != nil && order.SystemID != 0 && !order.LineDoctrine {
		if sys, _ := m.siegeOrderFor(ctx, seat.corpID); sys != 0 && sys == order.SystemID {
			siege = true
		}
	}
	roe := m.corpDoctrineFor(ctx, seat.corpID)
	kind := kindForOrder(have, roe, seat.pack, siege, relCorpAtWar(seat.corpID), seat.gank)
	for _, d := range have {
		if d.Kind == kind {
			d := d
			return &d
		}
	}
	return nil
}

// fleetSlots is the seat list a recruiter fills: the doctrine's when the
// fleet has one, gangComposition's curve otherwise.
func (m *Mod) fleetSlots(ctx context.Context, doc *FleetDoctrine, activity string, size int, pack bool) []gangSlot {
	if doc == nil {
		if pack {
			return gangCompositionPvP(size)
		}
		return gangCompositionWith(activity, size, m.logiViable(ctx), m.ewarViable(ctx))
	}
	gunRole, gunJob := RoleHunter, JobCombat
	if activity == ActPiracy {
		gunRole, gunJob = RoleCombat, JobPiracy
	}
	logiOK, ewarOK := m.logiViable(ctx), m.ewarViable(ctx)
	return doc.Slots(size, gunRole, gunJob, func(s seatSpec) bool {
		switch s.Role {
		case RoleLogi:
			return logiOK
		case RoleEwar:
			return ewarOK
		}
		return true
	})
}
