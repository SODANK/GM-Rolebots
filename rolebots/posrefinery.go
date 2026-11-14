package rolebots

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

// The refinery tower in use: members bring ore, take minerals. A refinery tower (posplan.go,
// purposeRefinery) carries a Medium Intensive or Intensive Refining Array: 75% with no tax,
// against a station's 50% base. The core refines whatever sits in an array's Processing Area in
// one cycle (db.StartRefining) and keeps the output per owner (starbaseRefineOutput), so a
// member's ore comes back as that member's minerals. This file adds the traffic:
//   - the CEO's pass starts a cycle whenever an array is idle with ore in it (ceoRefineryPass);
//   - a co-op mining fleet whose op is within refineryFleetReach of a corporation refinery drops
//     its load there, each miner's share under that miner's name (gangRefineryDrop);
//   - a member with minerals waiting in an array takes them home to its hangar (posTowerRefineDue).

const (
	// refineryFleetReach is how far from the op a fleet will carry its load
	// to a corporation refinery.
	refineryFleetReach = 8
	// refineryMinYield keeps the plain 35% Refining Array out of the traffic:
	// worse than a station, nobody should bring ore to it.
	refineryMinYield = 0.7
	// refineryFullHoldSec is how long a full array is passed over.
	refineryFullHoldSec = 15 * 60
	// TaskTowerRefine is a member collecting its minerals from a refinery.
	TaskTowerRefine TaskKind = "starbase.refine"

	attrRefineYield = int64(717)
	attrRefineMs    = int64(719)
)

func init() {
	registerTask(TaskDef{
		Kind: TaskTowerRefine, Label: "Collecting its minerals from its corporation's refinery",
		Posture: PostureHold, Step: (*Mod).towerRefineStep,
	})
}

// refineryArray is one corporation refinery a member may use.
type refineryArray struct {
	ArrayID, PlanID, MoonID, SystemID int64
	Yield, DurationMs                 float64
}

// refineryFull remembers an array that refused ore: arrayID to unix time.
var refineryFull sync.Map

// corpRefineryArrays is every online refining array of yield at least refineryMinYield in the
// corporation's running towers, whatever the tower is labelled.
func (m *Mod) corpRefineryArrays(ctx context.Context, corpID int64) []refineryArray {
	plans, err := m.corpTowerPlans(ctx, corpID, true)
	if err != nil {
		return nil
	}
	var out []refineryArray
	for _, p := range plans {
		if p.Status != towerRunning && p.Status != towerRigging {
			continue
		}
		for _, pc := range p.Layout.Pieces {
			if pc.ItemID == 0 || pc.Strip || (pc.TypeID != typeMedRefinery && pc.TypeID != typeBigRefinery) {
				continue
			}
			rec, ok, err := evedb.GetStarbaseStructure(ctx, m.db, pc.ItemID)
			if err != nil || !ok || rec.State != evedb.StarbaseStateOnline {
				continue
			}
			attrs, err := evedb.TypeAttrs(ctx, m.db, pc.TypeID)
			if err != nil || attrs[attrRefineYield] < refineryMinYield {
				continue
			}
			out = append(out, refineryArray{ArrayID: pc.ItemID, PlanID: p.PlanID, MoonID: p.MoonID, SystemID: p.SystemID,
				Yield: attrs[attrRefineYield], DurationMs: attrs[attrRefineMs]})
		}
	}
	return out
}

// pickRefinery is the nearest array within reach that is not marked full.
// jumps maps a system to its distance (absent = out of reach). Pure.
func pickRefinery(arrs []refineryArray, jumps map[int64]int, full map[int64]bool) (refineryArray, bool) {
	best, bestJ := refineryArray{}, -1
	s := append([]refineryArray(nil), arrs...)
	sort.SliceStable(s, func(i, j int) bool { return s[i].ArrayID < s[j].ArrayID })
	for _, a := range s {
		j, ok := jumps[a.SystemID]
		if !ok || full[a.ArrayID] {
			continue
		}
		if bestJ < 0 || j < bestJ {
			best, bestJ = a, j
		}
	}
	return best, bestJ >= 0
}

// nearestCorpRefinery is the corporation's nearest usable refinery within
// maxJumps of a system.
func (m *Mod) nearestCorpRefinery(ctx context.Context, corpID, fromSystem int64, maxJumps int) (refineryArray, bool) {
	arrs := m.corpRefineryArrays(ctx, corpID)
	if len(arrs) == 0 {
		return refineryArray{}, false
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return refineryArray{}, false
	}
	now := clock.Unix()
	full := map[int64]bool{}
	for _, a := range arrs {
		if at, ok := refineryFull.Load(a.ArrayID); ok && now-at.(int64) < refineryFullHoldSec {
			full[a.ArrayID] = true
		}
	}
	return pickRefinery(arrs, bfsJumps(u, fromSystem, maxJumps), full)
}

// refineryArrayByID finds one of the corporation's arrays.
func (m *Mod) refineryArrayByID(ctx context.Context, corpID, arrayID int64) (refineryArray, bool) {
	for _, a := range m.corpRefineryArrays(ctx, corpID) {
		if a.ArrayID == arrayID {
			return a, true
		}
	}
	return refineryArray{}, false
}

// startRefineryIfIdle starts a cycle on an idle array that holds ore.
func (m *Mod) startRefineryIfIdle(ctx context.Context, a refineryArray, startedBy int64) bool {
	if evedb.RefineryBusyUntil(ctx, m.db, a.ArrayID) != 0 {
		return false
	}
	var ore int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity e JOIN invTypes t ON t.typeID = e.typeID JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.locationID = ? AND e.flag = ? AND g.categoryID = 25 AND e.singleton = 0`,
		a.ArrayID, evedb.FlagRefineryInput).Scan(&ore)
	if ore == 0 {
		return false
	}
	if _, _, err := evedb.StartRefining(ctx, m.db, a.ArrayID, startedBy, a.Yield, a.DurationMs); err != nil {
		rlogf("rolebots: start refining %d: %v", a.ArrayID, err)
		return false
	}
	return true
}

// ceoRefineryPass presses the button on every idle array with ore in it.
func (m *Mod) ceoRefineryPass(ctx context.Context, c *corpCEO) {
	for _, a := range m.corpRefineryArrays(ctx, c.CorpID) {
		if m.startRefineryIfIdle(ctx, a, c.CharID) {
			m.ceoLog(ctx, c, powerStarbase, "refine", a.ArrayID, 0, "started a refining cycle at its tower on %s",
				moonNameOf(ctx, m, a.MoonID))
		}
	}
}

// ── the fleet's drop ────────────────────────────────────────────────────────

// gangRefineryTarget is the refinery a co-op fleet's hauler runs its load to,
// or false to use the ordinary dropoff.
func (m *Mod) gangRefineryTarget(ctx context.Context, b *botRun, g *Gang) (refineryArray, bool) {
	if !fleetSharesOre(g.Activity) || b.CorpID == 0 {
		return refineryArray{}, false
	}
	return m.nearestCorpRefinery(ctx, b.CorpID, b.SystemID, refineryFleetReach)
}

// gangRefineryDrop is the hauler at the refinery: every miner's share of the load goes into
// the Processing Area under that miner's name (the handBackFleetOre split), the claims are
// settled, and a cycle starts if the array is idle. done means the drop is finished or abandoned
// and the hauler is back to the fleet.
func (m *Mod) gangRefineryDrop(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	finish := func() error {
		st.Activity, st.Phase = "", "idle"
		st.Data = idleData(st.Data)
		st.Data.GangID = g.GangID
		return saveState(ctx, m.db, st)
	}
	a, ok := m.refineryArrayByID(ctx, b.CorpID, st.Data.PosRefineArray)
	if !ok {
		st.Data.PosRefineArray = 0
		return m.gangHaulDispatch(ctx, b, g)
	}
	if b.SystemID != a.SystemID || b.StationID != 0 {
		return m.beginTravel(ctx, b, a.SystemID, 0, string(TaskGangHaulDrop))
	}
	if m.embodyTowerPrep(b.CharID, a.MoonID) {
		st.Data.LegEndsAt = clock.Unix() + navLegHoldSec
		return saveState(ctx, m.db, st)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, COALESCE(t.typeName, ''), SUM(e.quantity) FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID JOIN invGroups gr ON gr.groupID = t.groupID
		 WHERE e.locationID = ? AND e.ownerID = ? AND e.flag = ? AND gr.categoryID = 25
		 GROUP BY e.typeID, t.typeName`, b.ShipID, b.CharID, flagCargo)
	if err != nil {
		return err
	}
	type lot struct {
		typeID int64
		name   string
		units  int64
	}
	var lots []lot
	for rows.Next() {
		var l lot
		if rows.Scan(&l.typeID, &l.name, &l.units) == nil && l.units > 0 {
			lots = append(lots, l)
		}
	}
	rows.Close()
	fullUp := false
	dropped := int64(0)
	miners := map[int64]bool{}
	for _, l := range lots {
		claims, err := m.coopClaimsFor(ctx, g.GangID, b.CharID, l.typeID)
		if err != nil {
			return err
		}
		shares := splitCoopLoad(l.units, claims, b.CharID, coopHaulCutFrac)
		ids := make([]int64, 0, len(shares))
		for id := range shares {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			n, err := m.depositOreAs(ctx, b, a.ArrayID, l.typeID, shares[id], id)
			dropped += n
			if n > 0 {
				miners[id] = true
				// Delivered, like the staging hand-back (fleetore.go): without
				// this a tower drop read as canned-and-never-returned.
				m.noteOreFlow(ctx, l.typeID, oreStageHandBack, n, 0)
				if id != b.CharID {
					m.logBot(id, logInfo, "gang", "the %q hauler dropped %d units of its %s at the corporation refinery on %s — its minerals will wait there",
						g.Name, n, l.name, moonNameOf(ctx, m, a.MoonID))
				}
			}
			if errors.Is(err, evedb.ErrBayFull) {
				fullUp = true
				break
			}
			if err != nil {
				return err
			}
		}
		if fullUp {
			break
		}
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM rolebot_gang_ore WHERE gangID = ? AND haulerCharID = ? AND typeID = ?`,
			g.GangID, b.CharID, l.typeID); err != nil {
			return fmt.Errorf("clear co-op claims: %w", err)
		}
	}
	m.startRefineryIfIdle(ctx, a, b.CharID)
	m.logBot(b.CharID, logInfo, "gang", "dropped %d units of the fleet's ore at the corporation refinery on %s for %d miner(s)",
		dropped, moonNameOf(ctx, m, a.MoonID), len(miners))
	if fullUp {
		// The array is full: the rest goes to the ordinary dropoff.
		refineryFull.Store(a.ArrayID, clock.Unix())
		st.Data.PosRefineArray = 0
		return m.gangHaulDispatch(ctx, b, g)
	}
	return finish()
}

// coopClaimsFor is this hauler's claims on a type, as handBackFleetOre reads
// them.
func (m *Mod) coopClaimsFor(ctx context.Context, gangID, haulerID, typeID int64) ([]coopClaim, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, units FROM rolebot_gang_ore
		 WHERE gangID = ? AND haulerCharID = ? AND typeID = ? AND units > 0
		 ORDER BY characterID`, gangID, haulerID, typeID)
	if err != nil {
		return nil, fmt.Errorf("co-op claims: %w", err)
	}
	defer rows.Close()
	var out []coopClaim
	for rows.Next() {
		var c coopClaim
		if rows.Scan(&c.CharID, &c.Units) == nil {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// depositOreAs puts up to want units of a type from the pilot's hold into an
// array's Processing Area under ownerID's name.
func (m *Mod) depositOreAs(ctx context.Context, b *botRun, arrayID, typeID, want, ownerID int64) (int64, error) {
	moved := int64(0)
	for moved < want {
		var itemID, qty int64
		if err := m.db.QueryRowContext(ctx, `
			SELECT itemID, quantity FROM entity WHERE locationID = ? AND ownerID = ? AND flag = ? AND typeID = ?
			 ORDER BY quantity DESC LIMIT 1`, b.ShipID, b.CharID, flagCargo, typeID).Scan(&itemID, &qty); err != nil {
			return moved, nil // nothing left aboard
		}
		take := min(qty, want-moved)
		if _, err := evedb.AddToStarbaseBay(ctx, m.db, arrayID, itemID, take, evedb.FlagRefineryInput, ownerID, false); err != nil {
			return moved, err
		}
		moved += take
	}
	return moved, nil
}

// ── collecting ──────────────────────────────────────────────────────────────

// towerMineralsOwed is a corporation refinery holding minerals of this
// pilot's, with no cycle running (what is there is finished), within reach.
func (m *Mod) towerMineralsOwed(ctx context.Context, b *botRun) (refineryArray, bool) {
	for _, a := range m.corpRefineryArrays(ctx, b.CorpID) {
		if evedb.RefineryBusyUntil(ctx, m.db, a.ArrayID) != 0 {
			continue
		}
		var n int
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM entity e JOIN invTypes t ON t.typeID = e.typeID
			 WHERE e.locationID = ? AND e.flag = ? AND e.ownerID = ? AND t.groupID = ?`,
			a.ArrayID, evedb.FlagRefineryInput, b.CharID, groupMineral).Scan(&n)
		if n > 0 {
			if j := m.jumpsTo(ctx, b, a.SystemID); j >= 0 && j <= corpWorkMaxJumps {
				return a, true
			}
		}
	}
	return refineryArray{}, false
}

// posTowerRefineDue sends a docked member out for minerals waiting at a
// corporation refinery.
func (m *Mod) posTowerRefineDue(ctx context.Context, b *botRun) (bool, error) {
	if b.StationID == 0 || b.CorpID == 0 {
		return false, nil
	}
	a, ok := m.towerMineralsOwed(ctx, b)
	if !ok {
		return false, nil
	}
	st := b.State
	st.Activity = ActIndustry
	st.Data = idleData(st.Data)
	st.Data.PosRefineArray = a.ArrayID
	m.logBot(b.CharID, logInfo, "corp", "flying out to collect its minerals from the corporation refinery on %s",
		moonNameOf(ctx, m, a.MoonID))
	return true, m.beginTravel(ctx, b, a.SystemID, 0, string(TaskTowerRefine))
}

// towerRefineStep is TaskTowerRefine: at the array, take every mineral of
// this pilot's into the hold; then home, where they go into its hangar.
func (m *Mod) towerRefineStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	done := func(note string) (bool, error) {
		if note != "" {
			m.logBot(b.CharID, logInfo, "corp", "%s", note)
		}
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		return false, saveState(ctx, m.db, st)
	}
	if d.PosRefineArray < 0 {
		// Carrying home: unload into the hangar once docked there.
		home := m.homeStationFor(ctx, b.CharID)
		if home == 0 || b.StationID == home {
			if b.StationID != 0 {
				m.unloadMinerals(ctx, b)
			}
			return done("brought its refined minerals home from the corporation refinery")
		}
		return false, m.beginTravel(ctx, b, m.stationSystemID(ctx, home), home, string(TaskTowerRefine))
	}
	a, ok := m.refineryArrayByID(ctx, b.CorpID, d.PosRefineArray)
	if !ok {
		return done("the corporation refinery it was flying to is gone")
	}
	if b.SystemID != a.SystemID || b.StationID != 0 {
		return false, m.beginTravel(ctx, b, a.SystemID, 0, string(TaskTowerRefine))
	}
	if m.embodyTowerPrep(b.CharID, a.MoonID) {
		d.LegEndsAt = clock.Unix() + navLegHoldSec
		return false, saveState(ctx, m.db, st)
	}
	// An unwatched ship is put beside the array it empties (parkAtTower reads any structure's
	// coordinate), so a projection built afterwards appears there.
	m.parkAtTower(ctx, b, &haulLeg{ToID: a.ArrayID})
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity) FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.locationID = ? AND e.flag = ? AND e.ownerID = ? AND t.groupID = ?
		 GROUP BY e.typeID`, a.ArrayID, evedb.FlagRefineryInput, b.CharID, groupMineral)
	if err != nil {
		return false, err
	}
	owed := map[int64]int64{}
	for rows.Next() {
		var t, q int64
		if rows.Scan(&t, &q) == nil {
			owed[t] = q
		}
	}
	rows.Close()
	room := m.shipHoldRoom(ctx, b)
	took := int64(0)
	for t, q := range owed {
		vol := m.typeVolume(ctx, t)
		n := q
		if vol > 0 {
			n = min(q, int64(room/vol))
		}
		if n <= 0 {
			continue
		}
		got, err := evedb.TakeFromStarbaseBay(ctx, m.db, a.ArrayID, t, n, evedb.FlagRefineryInput, b.ShipID, flagCargo, b.CharID)
		if err != nil {
			continue
		}
		took += got
		room -= float64(got) * vol
	}
	if took == 0 {
		return done("found nothing of its own waiting at the corporation refinery")
	}
	m.logBot(b.CharID, logInfo, "corp", "took %d units of its refined minerals from the corporation refinery on %s",
		took, moonNameOf(ctx, m, a.MoonID))
	d.PosRefineArray = -a.ArrayID
	home := m.homeStationFor(ctx, b.CharID)
	if home == 0 {
		return done("")
	}
	return false, m.beginTravel(ctx, b, m.stationSystemID(ctx, home), home, string(TaskTowerRefine))
}

// unloadMinerals moves every mineral in the hold into the pilot's hangar here and
// returns the units moved. Undocked it moves nothing.
func (m *Mod) unloadMinerals(ctx context.Context, b *botRun) int64 {
	if b.StationID == 0 || b.ShipID == 0 {
		return 0
	}
	held := m.cargoMinerals(ctx, b)
	if len(held) == 0 {
		return 0
	}
	var moved int64
	for t, q := range held {
		n, _ := m.moveCargoUnitsTo(ctx, b, t, q, b.CharID)
		moved += n
	}
	m.stackHangar(ctx, b.CharID, b.StationID)
	return moved
}

// cargoMinerals is the minerals in the pilot's hold by typeID.
func (m *Mod) cargoMinerals(ctx context.Context, b *botRun) map[int64]int64 {
	held := map[int64]int64{}
	if b.ShipID == 0 {
		return held
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity) FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.locationID = ? AND e.ownerID = ? AND e.flag = ? AND t.groupID = ?
		 GROUP BY e.typeID`, b.ShipID, b.CharID, flagCargo, groupMineral)
	if err != nil {
		return held
	}
	defer rows.Close()
	for rows.Next() {
		var t, q int64
		if rows.Scan(&t, &q) == nil && q > 0 {
			held[t] = q
		}
	}
	return held
}
