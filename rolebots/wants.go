package rolebots

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The capability ledger: what the roster could use, whether or not it has tried to buy one.
//
// For every item a pilot could fit, fly or load, the census counts how many pilots on the roster
// have the skills to use it. The ledger gives every supplier the same overview and no instruction;
// a supplier picks something and competes with others who picked the same (buildbook.go).
//
// Hulls weigh more than modules (capabilityHullWeight). Meta level breaks ties, lowest first, so
// the base item with a market blueprint beats its faction version. The census ignores what the
// market stocks. The `buildable` column marks rows an ordinary blueprint makes, and production
// readers (ledger, build book, wants page) filter buildable = 1.

const (
	// capabilitySweepEveryTicks is how often the census is retaken (ticks of 1 s). Skill sheets move
	// slowly, and a sweep walks the whole roster and item catalogue, so it does not belong on a plan pass.
	capabilitySweepEveryTicks = 900 // 15 minutes
	// capabilityHullWeight is what a ship hull is worth against a module with the same headcount.
	capabilityHullWeight = 2.5
	// capabilityChargeWeight: ammunition is consumed and re-bought, so a charge is worth more per head
	// than a module, which is bought once.
	capabilityChargeWeight = 1.5
	// capabilityRoundsPerPilot is how much ammunition one pilot who can shoot it is counted as wanting: a
	// magazine, since a pilot burns rounds every undock. It is its own number, not fitGenChargeQty
	// (chargeQtyFor, fitammo.go); industryBatchRuns caps a singleton charge.
	capabilityRoundsPerPilot = 200
	// capabilityDronesPerPilot is the same thought, smaller: drones are lost
	// rather than consumed, so a pilot replaces a few and not a magazine.
	capabilityDronesPerPilot = 5
)

// Item categories the census covers. Anything a pilot fits, flies, loads or
// launches; nothing a pilot merely owns.
const (
	catShip   = 6
	catModule = 7
	catCharge = 8
	catDrone  = 18
	// catImplant is not a census category either: an implant is wanted per
	// pilot (implantwants.go), never per roster.
	catImplant = 20

	// Not a census category: the refined-mineral market (minerals.go, and the trader's supply
	// catalogue). catMaterial also holds ice products and moon goo, so the mineral ledger tests the group.
	catMaterial  = 4
	groupMineral = 18
)

// capabilityBusy stops two sweeps overlapping. Periodics fire on the tick
// goroutine and the first pass after a restart has real work to do.
var capabilityBusy atomic.Bool

// startCapabilitySweep registers the census.
func (m *Mod) startCapabilitySweep() {
	m.registerPeriodic("rolebot-capability-sweep", capabilitySweepEveryTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		if !capabilityBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("capability census", func() {
			defer capabilityBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := m.retakeCapabilityCensus(ctx); err != nil {
				rlogf("rolebots: capability census: %v", err)
			}
		})
		return true
	})
}

// skillReq is one requirement pair off a type.
type skillReq struct {
	SkillID int64
	Level   int
}

// retakeCapabilityCensus recounts, for every census item, how many enabled rolebots could use it.
// It counts in Go rather than one SQL join, so the "can use" rule is the one the fit generator
// applies (UnmetSkillReqs).
func (m *Mod) retakeCapabilityCensus(ctx context.Context) error {
	started := clock.Now()

	out, pilots, err := m.computeCapability(ctx)
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return nil
	}

	now := clock.Unix()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("capability census: %w", err)
	}
	defer tx.Rollback()
	for _, r := range out {
		buildable := 0
		if r.item.Buildable {
			buildable = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rolebot_capability
				(typeID, typeName, groupID, categoryID, capable, users, held, metaLevel, buildable, updatedAt)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				typeName = VALUES(typeName), groupID = VALUES(groupID),
				categoryID = VALUES(categoryID), capable = VALUES(capable),
				users = VALUES(users), held = VALUES(held),
				metaLevel = VALUES(metaLevel), buildable = VALUES(buildable),
				updatedAt = VALUES(updatedAt)`,
			r.item.TypeID, r.item.TypeName, r.item.GroupID, r.item.CategoryID,
			r.capable, r.users, r.held, r.item.MetaLevel, buildable, now); err != nil {
			return fmt.Errorf("capability census write: %w", err)
		}
	}
	// A type nobody can use any more stops being an opportunity. Rows are stamped every sweep, so an
	// older stamp means the type was not counted this time.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM rolebot_capability WHERE updatedAt < ?`, now); err != nil {
		return fmt.Errorf("capability census prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("capability census commit: %w", err)
	}
	used := 0
	for _, r := range out {
		if r.item.Buildable && r.users > 0 {
			used++
		}
	}
	rlogf("rolebots: capability census — %d type(s) at least one pilot can use, %d buildable "+
		"one(s) the roster fits, loads or flies, across %d pilot(s), in %s",
		len(out), used, pilots, clock.Now().Sub(started).Round(time.Millisecond))
	return nil
}

// capabilityTally is one counted item, before it is written down.
type capabilityTally struct {
	item    censusItem
	capable int64
	// users is the production headcount: pilots who fit, load or fly it (wantsusers.go). capable stays
	// the skill headcount the loot fence reads.
	users int64
	// held is the roster's unsold stock of it, in units (wantsusers.go rateHeld).
	held int64
}

// computeCapability does the counting and nothing else: no writes, so it can be run against a real
// roster to see what it says.
func (m *Mod) computeCapability(ctx context.Context) ([]capabilityTally, int, error) {
	sheets, err := m.rosterSkillSheets(ctx)
	if err != nil {
		return nil, 0, err
	}
	if len(sheets) == 0 {
		return nil, 0, nil
	}
	items, reqs, err := m.censusItems(ctx)
	if err != nil {
		return nil, 0, err
	}
	hullImplied := m.specialtyHullPilots(ctx, items, reqs, sheets)
	out := make([]capabilityTally, 0, len(items))
	byType := make(map[int64]int64, len(items))
	for _, it := range items {
		need := reqs[it.TypeID]
		capable := int64(0)
		for _, sheet := range sheets {
			if meetsReqs(need, sheet) {
				capable++
			}
		}
		if spec, ok := specialtyModuleGroups[it.GroupID]; ok && hullImplied[spec] > capable {
			capable = hullImplied[spec]
		}
		byType[it.TypeID] = capable
		if capable == 0 {
			continue // nobody on this roster could ever use it
		}
		out = append(out, capabilityTally{item: it, capable: capable})
	}
	accepts, err := m.moduleChargeAccepts(ctx)
	if err != nil {
		return nil, 0, err
	}
	sizes, err := m.chargeSizes(ctx)
	if err != nil {
		return nil, 0, err
	}
	rateCharges(out, byType, accepts, sizes)
	if err := m.rateUsers(ctx, out, accepts, sizes); err != nil {
		return nil, 0, err
	}
	// Best-effort: a failed read leaves held at 0.
	if err := m.rateHeld(ctx, out); err != nil {
		rlogf("rolebots: capability census: %v — roster stock not counted this time", err)
	}
	// A charge nobody can shoot drops out entirely.
	kept := out[:0]
	for _, v := range out {
		if v.capable > 0 || v.users > 0 {
			kept = append(kept, v)
		}
	}
	return kept, len(sheets), nil
}

// specialtyModuleGroups is the module group each hull specialty is about: the EWAR families plus
// the two remote repairers a bot can run.
var specialtyModuleGroups = func() map[int64]string {
	out := map[int64]string{
		grpShieldTransporter: SpecRemoteShield,
		grpArmorRepProjector: SpecRemoteArmor,
	}
	for g, s := range ewarSpecialtyByGroup {
		out[g] = s
	}
	return out
}()

// specialtyHullPilots counts, per hull specialty, the pilots who can fly at least one hull built
// around it (the Blackbird's jammer, the Celestis's damp, the Exequror's armour projector). The
// specialty module is counted at least that many heads: its own skill headcount alone can read near
// zero, so it would never be built (fitgen.go ewarSpecialtyWishes).
func (m *Mod) specialtyHullPilots(ctx context.Context, items []censusItem,
	reqs map[int64][]skillReq, sheets []map[int64]int) map[string]int64 {
	byID, _, err := m.loadHullProfiles(ctx)
	if err != nil {
		return nil
	}
	hulls := map[string][]int64{}
	for _, it := range items {
		if it.CategoryID != catShip {
			continue
		}
		h := byID[it.TypeID]
		if h == nil {
			continue
		}
		seen := map[string]bool{}
		for _, spec := range specialtyModuleGroups {
			if !seen[spec] && h.HasSpecialty(spec) {
				seen[spec] = true
				hulls[spec] = append(hulls[spec], it.TypeID)
			}
		}
	}
	out := make(map[string]int64, len(hulls))
	for spec, ids := range hulls {
		n := int64(0)
		for _, sheet := range sheets {
			for _, id := range ids {
				if meetsReqs(reqs[id], sheet) {
					n++
					break
				}
			}
		}
		out[spec] = n
	}
	return out
}

// chargeGroupAttrs are the five columns a module uses to say which
// ammunition groups it accepts.
var chargeGroupAttrs = []int64{604, 605, 606, 609, 610}

// rateCharges replaces every charge's headcount with the number of pilots who can shoot it.
// Ammunition needs no skills, so counting it like other items would credit every pilot for every
// charge. A charge inherits the best-served module that accepts its group and size: a lower bound,
// not the exact union of those modules' pilots.
func rateCharges(tallies []capabilityTally, byType map[int64]int64,
	accepts []chargeAccept, sizes map[int64]int64) {
	type accept struct{ group, size int64 }
	best := map[accept]int64{} // (charge group, size) to best module headcount
	for _, a := range accepts {
		if n := byType[a.module]; n > best[accept{a.group, a.size}] {
			best[accept{a.group, a.size}] = n
		}
	}
	for i := range tallies {
		if tallies[i].item.CategoryID != catCharge {
			continue
		}
		grp := tallies[i].item.GroupID
		size := sizes[tallies[i].item.TypeID]
		n := best[accept{grp, size}]
		if size == 0 {
			// Sizeless ammunition (scripts, some drones' charges): any
			// module taking the group will do.
			for k, v := range best {
				if k.group == grp && v > n {
					n = v
				}
			}
		}
		tallies[i].capable = n
	}
}

// chargeSizes reads every charge's own size, the same authority the fitting
// code uses.
func (m *Mod) chargeSizes(ctx context.Context) (map[int64]int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT ta.typeID, CAST(COALESCE(NULLIF(ta.valueFloat,0), ta.valueInt) AS SIGNED)
		FROM dgmTypeAttributes ta
		JOIN invTypes t ON t.typeID = ta.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = ?
		WHERE ta.attributeID = ?`, catCharge, attrChargeSize)
	if err != nil {
		return nil, fmt.Errorf("chargeSizes: %w", err)
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var typeID, sz int64
		if err := rows.Scan(&typeID, &sz); err != nil {
			return nil, err
		}
		out[typeID] = sz
	}
	return out, rows.Err()
}

// meetsReqs reports whether one skill sheet satisfies every requirement.
func meetsReqs(need []skillReq, sheet map[int64]int) bool {
	for _, r := range need {
		if sheet[r.SkillID] < r.Level {
			return false
		}
	}
	return true
}

// rosterSkillSheets loads every enabled rolebot's trained skill levels in one query.
func (m *Mod) rosterSkillSheets(ctx context.Context) ([]map[int64]int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.ownerID, e.typeID, COALESCE(a.valueInt, a.valueFloat, 0)
		FROM rolebot_characters b
		JOIN entity e ON e.ownerID = b.characterID AND e.flag IN (7, 61)
		LEFT JOIN entity_attributes a ON a.itemID = e.itemID AND a.attributeID = 280
		WHERE b.enabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("rosterSkillSheets: %w", err)
	}
	defer rows.Close()
	byChar := map[int64]map[int64]int{}
	for rows.Next() {
		var charID, skillID int64
		var level float64
		if err := rows.Scan(&charID, &skillID, &level); err != nil {
			return nil, fmt.Errorf("rosterSkillSheets scan: %w", err)
		}
		if byChar[charID] == nil {
			byChar[charID] = map[int64]int{}
		}
		if lv := int(level); lv > byChar[charID][skillID] {
			byChar[charID][skillID] = lv
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Pilots with no skills at all still exist and still count for anything
	// that needs none, so they are included with an empty sheet.
	var ids []int64
	if err := func() error {
		r, err := m.db.QueryContext(ctx,
			`SELECT characterID FROM rolebot_characters WHERE enabled = 1`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var id int64
			if err := r.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return r.Err()
	}(); err != nil {
		return nil, fmt.Errorf("rosterSkillSheets roster: %w", err)
	}
	out := make([]map[int64]int, 0, len(ids))
	for _, id := range ids {
		s := byChar[id]
		if s == nil {
			s = map[int64]int{}
		}
		out = append(out, s)
	}
	return out, nil
}

// censusItem is one thing the census counts.
type censusItem struct {
	TypeID     int64
	TypeName   string
	GroupID    int64
	CategoryID int64
	MetaLevel  int64
	// Buildable is whether an ordinary blueprint produces this type and the market can carry that
	// blueprint (bpBuyableJoin). Production reads it; the loot fence only asks whether anybody could use it.
	Buildable bool
}

// censusItems is every item worth counting, with its skill requirements. The blueprint join feeds
// the buildable flag and is not a filter, so meta and faction gear is counted too; marketGroupID > 0
// does the junk filtering. Faction ammo has a blueprint row nobody can get, which bpBuyableJoin
// (buildbook.go) excludes.
func (m *Mod) censusItems(ctx context.Context) ([]censusItem, map[int64][]skillReq, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID, g.categoryID, COALESCE(ml.lvl, 0),
		       CASE WHEN bpt.typeID IS NULL OR mt.typeID IS NOT NULL THEN 0 ELSE 1 END
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN invBlueprintTypes bp ON bp.productTypeID = t.typeID
		LEFT JOIN invTypes bpt ON bpt.typeID = bp.blueprintTypeID
		                      AND bpt.marketGroupID > 0 AND bpt.basePrice > 0
		-- A meta product (any invMetaTypes row: named T1, T2, faction, ...)
		-- is never buildable on the census, whatever its blueprint's price
		-- says — see bpBuyableJoin. Its capable count is still taken, which
		-- is what sizes a batch off a print a pilot happens to own.
		LEFT JOIN invMetaTypes mt ON mt.typeID = t.typeID
		LEFT JOIN (SELECT typeID, COALESCE(NULLIF(valueFloat,0), valueInt) lvl
		           FROM dgmTypeAttributes WHERE attributeID = ?) ml ON ml.typeID = t.typeID
		WHERE t.published = 1 AND t.marketGroupID > 0
		  AND g.categoryID IN (?, ?, ?, ?)
		  -- Drones only in the groups a fit carries (fitdrones.go). A
		  -- trained pilot is not a customer for a drone no fit will ever
		  -- put in a bay, and building one is stock nobody buys.
		  AND (g.categoryID <> ? OR t.groupID IN (?, ?))`,
		attrMetaLevel, catShip, catModule, catCharge, catDrone,
		catDrone, grpCombatDrone, grpMiningDrone)
	if err != nil {
		return nil, nil, fmt.Errorf("censusItems: %w", err)
	}
	defer rows.Close()
	var items []censusItem
	for rows.Next() {
		var it censusItem
		var buildable int64
		if err := rows.Scan(&it.TypeID, &it.TypeName, &it.GroupID, &it.CategoryID,
			&it.MetaLevel, &buildable); err != nil {
			return nil, nil, fmt.Errorf("censusItems scan: %w", err)
		}
		it.Buildable = buildable != 0
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	reqs := make(map[int64][]skillReq, len(items))
	for _, it := range items {
		r, err := m.requiredSkills(ctx, it.TypeID)
		if err != nil {
			continue // unreadable requirements: treat as needing none
		}
		if len(r) == 0 {
			continue
		}
		out := make([]skillReq, 0, len(r))
		for _, one := range r {
			out = append(out, skillReq{SkillID: one.SkillID, Level: one.Level})
		}
		reqs[it.TypeID] = out
	}
	return items, reqs, nil
}

// ── what an industrialist reads ─────────────────────────────────────────────

// capabilityWeight is what one entry is worth per head: hulls above everything, then ammunition,
// then modules.
func capabilityWeight(categoryID int64) float64 {
	switch categoryID {
	case catShip:
		return capabilityHullWeight
	case catCharge:
		return capabilityChargeWeight
	}
	return 1
}

// capabilityUnitsPerPilot is how many of a thing one capable pilot counts as wanting: a ship or a
// magazine.
func capabilityUnitsPerPilot(categoryID int64) int64 {
	switch categoryID {
	case catCharge:
		return capabilityRoundsPerPilot
	case catDrone:
		return capabilityDronesPerPilot
	}
	return 1
}

// CapabilityRow is one line of the ledger, for the build book and the panel.
type CapabilityRow struct {
	TypeID     int64
	TypeName   string
	CategoryID int64
	// Capable is the skill headcount; Users the production headcount the
	// ledger is ordered and sized on (wantsusers.go).
	Capable  int64
	Users    int64
	BPTypeID int64
	// Qty is how many units the Users headcount adds up to: one hull or module each, a magazine each
	// for ammunition. A production run is sized from it, and the ordering does not use it.
	Qty int64
	// Pull is Users x the category weight, the number the ordering is done on. The panel shows it so a
	// hull outranking a module with a bigger headcount is explained.
	Pull float64
}

// capabilityLedger is the top of the roll: what the most pilots could use, hulls counting for more.
// It has no claim rule, no per-pilot filter and no limit cut, so it returns the whole buildable census
// in a stable order; headcount is a term in the book's score (headcountPull) and the draw randomises.
// It is cached for capabilityLedgerTTL, since every supplier reads it on each plan pass.
func (m *Mod) capabilityLedger(ctx context.Context, limit int) ([]CapabilityRow, error) {
	all, err := m.capabilityLedgerAll(ctx)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(all) > limit {
		return all[:limit], nil
	}
	return all, nil
}

// capabilityLedgerTTL is how long one read of the buildable census is shared.
// A third of the sweep interval: a retaken census reaches the book within
// minutes, and the read is not repeated per pilot.
const capabilityLedgerTTL = 5 * time.Minute

var (
	capLedgerMu   sync.Mutex
	capLedgerRows []CapabilityRow
	capLedgerAt   time.Time
	capHeadByID   map[int64]censusHead
)

// censusHead is one census row as the build book scores it: every row, not only the buildable ones,
// since an owned print of a meta item still wants its headcount. Rows with zero users are kept: the
// book's unused veto reads them (censusUnused).
type censusHead struct {
	Capable    int64
	Users      int64
	CategoryID int64
	// Held is the roster's unsold stock, scaled by the operator's rolebots.floodHeldShare. The flood
	// test adds it to the listings.
	Held int64
}

func (m *Mod) capabilityLedgerAll(ctx context.Context) ([]CapabilityRow, error) {
	capLedgerMu.Lock()
	defer capLedgerMu.Unlock()
	if capLedgerRows != nil && clock.Now().Sub(capLedgerAt) < capabilityLedgerTTL {
		return capLedgerRows, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.typeID, c.typeName, c.categoryID, c.capable, c.users, MIN(bt.blueprintTypeID)
		FROM rolebot_capability c
		JOIN invBlueprintTypes bt ON bt.productTypeID = c.typeID
		`+bpBuyableJoin+`
		WHERE c.users > 0 AND c.buildable = 1
		GROUP BY c.typeID, c.typeName, c.categoryID, c.capable, c.users, c.metaLevel
		ORDER BY c.users * CASE c.categoryID WHEN ? THEN ? WHEN ? THEN ? ELSE 1 END DESC,
		         c.metaLevel ASC, c.typeID ASC`,
		catShip, capabilityHullWeight, catCharge, capabilityChargeWeight)
	if err != nil {
		return nil, fmt.Errorf("capabilityLedger: %w", err)
	}
	defer rows.Close()
	out := []CapabilityRow{}
	for rows.Next() {
		var r CapabilityRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.CategoryID, &r.Capable, &r.Users, &r.BPTypeID); err != nil {
			return nil, fmt.Errorf("capabilityLedger scan: %w", err)
		}
		r.Pull = float64(r.Users) * capabilityWeight(r.CategoryID)
		r.Qty = r.Users * capabilityUnitsPerPilot(r.CategoryID)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	heads := map[int64]censusHead{}
	share := floodHeldShare(ctx, m.db)
	hrows, err := m.db.QueryContext(ctx,
		`SELECT typeID, categoryID, capable, users, held FROM rolebot_capability`)
	if err != nil {
		return nil, fmt.Errorf("capability heads: %w", err)
	}
	defer hrows.Close()
	for hrows.Next() {
		var id, held int64
		var h censusHead
		if hrows.Scan(&id, &h.CategoryID, &h.Capable, &h.Users, &held) == nil {
			h.Held = scaleHeld(held, share)
			heads[id] = h
		}
	}
	if err := hrows.Err(); err != nil {
		return nil, err
	}
	capLedgerRows, capHeadByID, capLedgerAt = out, heads, clock.Now()
	return out, nil
}

// capabilityHeads is every census row's headcounts and category, from the same
// cached read as the ledger. Absent means the census does not count the type.
func (m *Mod) capabilityHeads(ctx context.Context) map[int64]censusHead {
	if _, err := m.capabilityLedgerAll(ctx); err != nil {
		return nil
	}
	capLedgerMu.Lock()
	defer capLedgerMu.Unlock()
	return capHeadByID
}

// capabilityCounts is capabilityCount for a whole hold at once: one query for a haul of a dozen
// stacks. A type absent from the census is absent from the map, which reads as nobody.
func (m *Mod) capabilityCounts(ctx context.Context, typeIDs []int64) map[int64]int64 {
	out := make(map[int64]int64, len(typeIDs))
	if len(typeIDs) == 0 {
		return out
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT typeID, capable FROM rolebot_capability WHERE typeID IN (`+
			placeholders(len(typeIDs))+`)`, int64Args(typeIDs)...)
	if err != nil {
		rlogf("rolebots: capability counts: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var typeID, capable int64
		if err := rows.Scan(&typeID, &capable); err != nil {
			return out
		}
		out[typeID] = capable
	}
	return out
}

// ── the other half: saying what the market did not have ─────────────────────
//
// The census says what the roster could use. A fit that goes out with an empty slot nobody sells is
// reported as a log line, not a ledger row, so the table keeps one meaning (logFitGaps).

// wantGap is one slot the generator could not fill because nothing serving
// it is for sale anywhere (fitgen.go distinguishes that from a slot lost to
// grid or to skills, which are nobody else's problem).
type wantGap struct {
	Band  int64
	Roles []modRole
	Hull  *HullProfile
	// Empty is whether the band still had an empty slot once the backfill pass was done. A slot the
	// backfill filled is not reported as going out empty.
	Empty bool
}

// fitGapSilence is how long the same pilot's same unfillable slots stay quiet after being reported
// once. The generator is reached once per job, not once per plan, so the same line would otherwise
// repeat within a second. The window runs on the sim clock (clock.go), like other abstract-tier cooldowns.
const fitGapSilenceSec = 30 * 60

var fitGapSaid sync.Map // charID -> *fitGapNote

type fitGapNote struct {
	mu  sync.Mutex
	sig string
	at  int64 // clock.Unix() of the last report
}

// logFitGaps says, once per pilot per gap, what the market could not supply.
func (m *Mod) logFitGaps(ctx context.Context, charID int64, c *generatedCandidate) {
	if c == nil || len(c.Gaps) == 0 || c.Hull == nil {
		return
	}
	var parts []string
	for _, gap := range c.Gaps {
		if !gap.Empty {
			continue // backfill found something for it (wantGap.Empty)
		}
		roles := make([]string, 0, len(gap.Roles))
		for _, r := range gap.Roles {
			roles = append(roles, string(r))
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", slotBandName(gap.Band), strings.Join(roles, "/")))
	}
	if len(parts) == 0 {
		return
	}
	// The hull is part of the signature: a pilot on a different shortlist has a different report to make.
	sig := c.Hull.Name + "|" + strings.Join(parts, ", ")
	v, _ := fitGapSaid.LoadOrStore(charID, &fitGapNote{})
	note := v.(*fitGapNote)
	now := clock.Unix()
	note.mu.Lock()
	if note.sig == sig && now-note.at < fitGapSilenceSec {
		note.mu.Unlock()
		return
	}
	note.sig, note.at = sig, now
	note.mu.Unlock()

	m.logBot(charID, logInfo, "fit",
		"its %s goes out with %s empty — nothing serving those is sold in its reach",
		c.Hull.Name, strings.Join(parts, ", "))
}

// sweepFitGapSaid drops the silence for pilots nobody is planning for any more, so a purged bot's
// entry is not held for the life of the process (cachesweep.go).
func sweepFitGapSaid() {
	now := clock.Unix()
	fitGapSaid.Range(func(k, v any) bool {
		note := v.(*fitGapNote)
		note.mu.Lock()
		stale := now-note.at >= fitGapSilenceSec
		note.mu.Unlock()
		if stale {
			fitGapSaid.Delete(k)
		}
		return true
	})
}

// slotBandName names a slot band the way a pilot would.
func slotBandName(band int64) string {
	switch band {
	case flagHiFirst:
		return "high"
	case flagMedFirst:
		return "mid"
	case flagLoFirst:
		return "low"
	}
	return "slot"
}

func init() {
	registerCharForget("fitGapSaid", func(id int64) { fitGapSaid.Delete(id) })
	registerSweep("fitGapSaid", sweepFitGapSaid)
}
