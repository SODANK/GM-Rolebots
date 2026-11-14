package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// Ice is the fuel half of the starbase economy, mined by the roster. A tower burns isotopes,
// Heavy Water, Liquid Ozone and strontium, all refined out of ice. Ice is not an activity of its
// own: it is mining done in a barge. The industry umbrella offers one mining trip
// (miningTripPlan, industrytrip.go) that goes to the rock or the ice, whichever the world wants
// (iceBeatsRock). The pull on ice is the towers (icedemand.go) plus iceMarketIndex. The rocks are
// the real field's (beltfield.go), debited through asteroidDepletion.
//
// Needs Mining Barge I and Ice Harvesting I (skills_ice.go), an owned barge and an Ice Harvester.
// A pilot without a barge mines rock; the fit ladder (fitselect.go) buys one when the wallet allows.

const (
	ActIce = "ice"

	skillIceHarvesting       = 16281
	skillMiningBarge         = 17940
	typeIceHarvesterI        = 16278
	typeIceHarvesterII       = 22229
	typeIceHarvesterUpgradeI = 22576
	groupIceRock             = 465
	groupMiningBarge         = 463
	groupExhumer             = 543
	iceFieldTypeID           = 17774
	// iceMarkerTypeID (Clear Icicle) stands in for "ice" on a projection's ore slot: it gives
	// the ice rock group to the miner gate and the 1000 m³ block volume to the cycle maths.
	iceMarkerTypeID = 16262

	iceFieldReach     = 12
	iceDemandTTL      = 3600
	iceDefaultCycleMs = 600000
	iceMinUnits       = 1
	// iceStockDetour is how many jumps nearer a refinery counts when it
	// stands in a region whose towers are short of fuel.
	iceStockDetour = 4
)

// iceProducts is what a tower burns that ice refines into.
var iceProducts = []int64{16272, 16273, 16274, 16275, 17887, 17888, 17889}

var (
	// iceDemandBits is the full index (market plus the world's towers), read by the skill road;
	// iceMarketBits is the market alone, to which a pilot's opportunity adds the local tower pull.
	iceDemandBits atomic.Uint64
	iceMarketBits atomic.Uint64
	iceDemandAt   atomic.Int64
	iceDemandMu   sync.Mutex
	iceTypeSet    atomic.Pointer[map[int64]bool]
)

func init() {
	activityCatalog = append(activityCatalog, ActivityDef{
		// A trip of the industry umbrella (industrytrip.go), not a chooser option. The entry supplies
		// the disposition, the opportunity probe, the half-life and the slug every ice phase keys on.
		Slug: ActIce, Label: "ice harvesting", Job: JobMining, Tactic: true,
		Plan:        (*Mod).icePlan,
		Disposition: func(tn Tuning) float64 { return tn.MiningAppetite },
		Opportunity: iceOpportunity,
		HalfLife:    func(tn Tuning) float64 { return careerHalfLife(tn, tn.CareerMining, careerStretch) },
	})
	registerTask(TaskDef{Kind: TaskIceBoard, Label: "Boarding its barge for the ice",
		Posture: PostureDocked, Step: (*Mod).iceBoardStep})
	registerTask(TaskDef{Kind: TaskIceWork, Label: "Harvesting ice", Interruptible: true,
		Posture: PostureWorkBelt, Exposed: true, Step: (*Mod).iceWorkStep, Detail: detailIce})
	registerTask(TaskDef{Kind: TaskIceSell, Label: "Refining and selling ice",
		Posture: PostureDocked, Step: (*Mod).iceSellStep, Detail: detailIce})
}

const (
	TaskIceBoard TaskKind = "ice.board"
	TaskIceWork  TaskKind = "ice.harvest"
	TaskIceSell  TaskKind = "ice.sell"
)

func detailIce(m *Mod, ctx context.Context, st *SimState) string {
	if st.Data.IceSysID == 0 {
		return ""
	}
	n := int64(0)
	for _, l := range st.Data.IceLots {
		n += l.Units
	}
	return fmt.Sprintf("%d block(s) of ice from %s", n, systemNameOf(ctx, m, st.Data.IceSysID))
}

// iceDemandIndex is the world's pull toward ice: ~1 when the products fetch
// their shelf value, higher when bids run above it or corporations are
// requisitioning them, or towers anywhere are short of fuel. Cached for an
// hour; a tower event resets it.
func iceDemandIndex() float64 {
	v := math.Float64frombits(iceDemandBits.Load())
	if v <= 0 {
		return 1
	}
	return v
}

// iceMarketIndex is iceDemandIndex without the towers' term.
func iceMarketIndex() float64 {
	v := math.Float64frombits(iceMarketBits.Load())
	if v <= 0 {
		return 1
	}
	return v
}

func (m *Mod) refreshIceDemand(ctx context.Context) {
	if clock.Unix()-iceDemandAt.Load() < iceDemandTTL {
		return
	}
	if !iceDemandMu.TryLock() {
		return
	}
	defer iceDemandMu.Unlock()
	iceDemandAt.Store(clock.Unix())
	vals, err := evedb.TypeValues(ctx, m.db, iceProducts)
	if err != nil {
		return
	}
	sum, n := 0.0, 0
	for _, t := range iceProducts {
		ref := vals[t].Value
		if ref <= 0 {
			continue
		}
		var bid sql.NullFloat64
		_ = m.db.QueryRowContext(ctx, `
			SELECT MAX(price) FROM market_orders WHERE typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0`,
			t).Scan(&bid)
		ratio := 0.8
		if bid.Valid && bid.Float64 > 0 {
			ratio = bid.Float64 / ref
		}
		sum += math.Min(2, ratio)
		n++
	}
	idx := 1.0
	if n > 0 {
		idx = sum / float64(n)
	}
	var reqUnits sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `
		SELECT SUM(GREATEST(qty - qtyDone, 0)) FROM rolebot_corp_requisition
		 WHERE status IN ('open','bidding','buying') AND typeID IN (`+int64CSV(iceProducts)+`)`).Scan(&reqUnits)
	idx += math.Min(1.5, float64(reqUnits.Int64)/200000)
	market := math.Max(0.2, math.Min(3, idx))
	iceMarketBits.Store(math.Float64bits(market))
	m.refreshIceLedger(ctx)
	iceDemandBits.Store(math.Float64bits(math.Min(3, market+iceWorldPull(iceLedger().WorldISK))))
}

// isIceType reports whether a rock type is ice (group 465).
func (m *Mod) isIceType(ctx context.Context, typeID int64) bool {
	if p := iceTypeSet.Load(); p != nil {
		return (*p)[typeID]
	}
	set := map[int64]bool{}
	ids, _ := int64Column(ctx, m, `SELECT typeID FROM invTypes WHERE groupID = ?`, groupIceRock)
	for _, id := range ids {
		set[id] = true
	}
	iceTypeSet.Store(&set)
	return set[typeID]
}

// iceHull is an ice-capable hull the pilot owns (a barge or an exhumer) and
// where it sits: the boarded ship first.
func (m *Mod) iceHull(ctx context.Context, b *botRun) (shipID, locationID int64) {
	_ = m.db.QueryRowContext(ctx, `
		SELECT e.itemID, e.locationID FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.ownerID = ? AND t.groupID IN (?, ?)
		 ORDER BY (e.itemID = ?) DESC, (e.locationID = ?) DESC, e.itemID LIMIT 1`,
		b.CharID, groupMiningBarge, groupExhumer, b.ShipID, b.StationID).Scan(&shipID, &locationID)
	return shipID, locationID
}

// iceHarvesters counts harvesters with the ship (fitted or in the hold).
func (m *Mod) iceHarvesters(ctx context.Context, charID, shipID int64) int64 {
	var n sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `
		SELECT SUM(GREATEST(quantity, 1)) FROM entity WHERE locationID = ? AND ownerID = ? AND typeID IN (?, ?)`,
		shipID, charID, typeIceHarvesterI, typeIceHarvesterII).Scan(&n)
	return n.Int64
}

// iceOpportunity is the world's multiplier on ice for one pilot: the market's index plus
// the pull of the towers in its reach. No barge gives 0 and a note, so the mining trip mines
// rock. A barge without a harvester is no refusal: icePlan buys one, here or at the nearest shelf.
func iceOpportunity(m *Mod, ctx context.Context, b *botRun) (float64, string) {
	m.refreshIceDemand(ctx)
	pull, pullWhy := m.towerIcePull(ctx, b)
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return 0, "skills unreadable"
	}
	if skills[skillIceHarvesting] < 1 || skills[skillMiningBarge] < 1 {
		return 0, withPull("cannot run an ice harvester yet", pullWhy)
	}
	if ship, _ := m.iceHull(ctx, b); ship == 0 {
		return 0, withPull("owns no barge yet — mining rock and saving for one", pullWhy)
	}
	d := iceMarketIndex() + pull
	why := fmt.Sprintf("ice products fetch %.2f× their value", iceMarketIndex())
	if pullWhy != "" {
		why += "; " + pullWhy
	}
	// Cached per corporation for five minutes: every barge pilot asks this on each scoring pass.
	corpReq, _ := snapshotFor(snapKey("ice-req", b.CorpID), 5*time.Minute, func() (int, error) {
		var n int
		err := m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM rolebot_corp_requisition
			 WHERE corporationID = ? AND status IN ('open','bidding','buying') AND typeID IN (`+int64CSV(iceProducts)+`)`,
			b.CorpID).Scan(&n)
		return n, err
	})
	if corpReq > 0 {
		d *= 1.5
		why += ", and its own corporation is buying fuel"
	}
	return math.Max(0.1, d), why
}

// withPull appends the towers' note to a refusal, so the log says what the
// pilot is missing out on.
func withPull(why, pull string) string {
	if pull == "" {
		return why
	}
	return why + " (" + pull + ")"
}

// nearestIceSeller is the best station within iceFieldReach jumps with an ask for typeID,
// and its system. The ask includes the flying (jumpPriceCost a jump), so distance alone
// does not win.
func (m *Mod) nearestIceSeller(ctx context.Context, b *botRun, typeID int64) (stationID, sysID int64) {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return 0, 0
	}
	near := bfsJumps(u, b.SystemID, iceFieldReach)
	ids := make([]int64, 0, len(near))
	for s := range near {
		ids = append(ids, s)
	}
	if len(ids) == 0 {
		return 0, 0
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, s.solarSystemID, MIN(o.price) FROM market_orders o
		  JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.typeID = ? AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		   AND s.solarSystemID IN (`+int64CSV(ids)+`)
		 GROUP BY o.stationID, s.solarSystemID`, typeID)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()
	best := math.Inf(1)
	for rows.Next() {
		var st, sy int64
		var p float64
		if rows.Scan(&st, &sy, &p) != nil || p <= 0 {
			continue
		}
		if cost := p * (1 + jumpPriceCost*float64(near[sy])); cost < best {
			best, stationID, sysID = cost, st, sy
		}
	}
	return stationID, sysID
}

// icePlan starts an ice trip: into the barge, a harvester aboard, off to the
// best Ice Field in reach.
func (m *Mod) icePlan(ctx context.Context, b *botRun) error {
	st := b.State
	ship, loc := m.iceHull(ctx, b)
	if ship == 0 {
		return fmt.Errorf("%w: no barge", errNoFitAvailable)
	}
	st.Activity = ActIce
	if ship != b.ShipID {
		if loc != b.StationID || b.StationID == 0 {
			st.Data = idleData(st.Data)
			st.Data.IceShipID = ship
			return m.beginTravel(ctx, b, m.stationSystemID(ctx, loc), loc, string(TaskIceBoard))
		}
		if err := m.boardShip(ctx, b, ship); err != nil {
			return err
		}
	}
	if m.iceHarvesters(ctx, b.CharID, b.ShipID) == 0 {
		if b.StationID == 0 {
			return fmt.Errorf("%w: no ice harvester aboard", errNoFitAvailable)
		}
		itemID, _, _, err := m.buyAtStation(ctx, b.CharID, b.StationID, typeIceHarvesterI, 1, "fitting")
		if err != nil {
			// Not on this shelf: fly the barge to the nearest one that has it.
			if station, sys := m.nearestIceSeller(ctx, b, typeIceHarvesterI); station != 0 && station != b.StationID {
				st.Data = idleData(st.Data)
				st.Data.IceShipID = b.ShipID
				m.logBot(b.CharID, logInfo, "mining", "flying its barge to %s for an ice harvester",
					systemNameOf(ctx, m, sys))
				return m.beginTravel(ctx, b, sys, station, string(TaskIceBoard))
			}
			return fmt.Errorf("%w: no ice harvester for sale in reach", errNoFitAvailable)
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE entity SET locationID = ?, flag = 5 WHERE itemID = ?`, b.ShipID, itemID)
	}
	sys, belt, why, err := m.pickIceField(ctx, b)
	if err != nil {
		return err
	}
	if sys == 0 {
		return fmt.Errorf("%w: no ice field in reach (%s)", errNoFitAvailable, why)
	}
	st.Data = idleData(st.Data)
	st.Data.IceSysID, st.Data.IceBeltID = sys, belt
	m.logBot(b.CharID, logInfo, "mining", "going ice harvesting at %s — %s", systemNameOf(ctx, m, sys), why)
	return m.beginTravel(ctx, b, sys, 0, string(TaskIceWork))
}

// pickIceField is the best Ice Field in reach: its rocks' refine value, less
// the flying and the measured danger.
func (m *Mod) pickIceField(ctx context.Context, b *botRun) (sysID, beltID int64, why string, err error) {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return 0, 0, "", err
	}
	reach := bfsJumps(u, b.SystemID, iceFieldReach)
	ids := make([]int64, 0, len(reach))
	for s := range reach {
		ids = append(ids, s)
	}
	if len(ids) == 0 {
		return 0, 0, "no map", nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, solarSystemID FROM mapDenormalize
		 WHERE typeID = ? AND solarSystemID IN (`+int64CSV(ids)+`)`, iceFieldTypeID)
	if err != nil {
		return 0, 0, "", err
	}
	type cand struct {
		belt, sys int64
		score     float64
		iceType   int64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.belt, &c.sys) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()
	best := cand{}
	for _, c := range cands {
		f, err := m.beltFieldFor(ctx, c.sys)
		if err != nil || f == nil {
			continue
		}
		units := map[int64]int64{}
		for _, r := range f.Rocks {
			if r.BeltID == c.belt && r.Qty > 0 && m.isIceType(ctx, r.TypeID) {
				units[r.TypeID] += r.Qty
			}
		}
		if len(units) == 0 {
			continue
		}
		// A block of the ice a short region's towers burn is worth its refine value plus the
		// shortfall it covers.
		meta := u.meta[c.sys]
		top, topV := int64(0), 0.0
		for t := range units {
			if v := m.iceRefineValue(ctx, t) + m.regionShortFor(ctx, meta.RegionID, t); v > topV {
				top, topV = t, v
			}
		}
		danger := m.towerDanger(ctx, b.CorpID, c.sys, meta.Security, 1)
		// A mine contract's ground is nearer than it looks by the job's pull
		// (gmmjobs.go; zero without the GMMordu module).
		jumps := max(0, float64(reach[c.sys])-gmmMinePull(b.CharID, c.sys))
		c.score = topV / (1 + 0.25*jumps) / (1 + danger)
		c.iceType = top
		if c.score > best.score {
			best = c
		}
	}
	if best.belt == 0 {
		return 0, 0, "no ice field in reach holds ice", nil
	}
	why = fmt.Sprintf("%s worth %s a block refined", m.typeNameOr(ctx, best.iceType),
		iskString(m.iceRefineValue(ctx, best.iceType)))
	if m.regionShortFor(ctx, u.meta[best.sys].RegionID, best.iceType) > 0 {
		why += ", and the towers here are short of what it refines into"
	}
	return best.sys, best.belt, why, nil
}

// iceRefineValue is one ice block refined, at the products' value.
func (m *Mod) iceRefineValue(ctx context.Context, iceType int64) float64 {
	mats, err := evedb.GetTypeMaterials(ctx, m.db, iceType)
	if err != nil || len(mats) == 0 {
		return evedb.TypeValueOf(ctx, m.db, iceType).Value
	}
	ids := make([]int64, 0, len(mats))
	for _, mt := range mats {
		ids = append(ids, mt.MaterialTypeID)
	}
	vals, err := evedb.TypeValues(ctx, m.db, ids)
	if err != nil {
		return 0
	}
	v := 0.0
	for _, mt := range mats {
		v += vals[mt.MaterialTypeID].Value * float64(mt.Quantity)
	}
	return v
}

// iceTakeReal debits ice rocks of one Ice Field, biggest first, through the live system
// when it is loaded. It follows beltTakeReal's rule for ore.
func (m *Mod) iceTakeReal(ctx context.Context, sysID, beltID, want int64) ([]oreLot, int64, error) {
	if want <= 0 {
		return nil, 0, nil
	}
	lk := beltSysLock(sysID)
	lk.Lock()
	defer lk.Unlock()
	f, err := m.beltFieldLocked(ctx, sysID)
	if err != nil || f == nil {
		return nil, 0, err
	}
	var idx []int
	for i := range f.Rocks {
		r := &f.Rocks[i]
		if r.BeltID == beltID && r.Qty > 0 && m.isIceType(ctx, r.TypeID) {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool {
		ra, rb := f.Rocks[idx[a]], f.Rocks[idx[b]]
		if ra.Qty != rb.Qty {
			return ra.Qty > rb.Qty
		}
		return ra.EntityID < rb.EntityID
	})
	ss := m.liveSystem(sysID)
	lots := map[int64]int64{}
	var writes []evedb.AsteroidDepletionRow
	var got int64
	for _, i := range idx {
		if got >= want {
			break
		}
		r := &f.Rocks[i]
		take := min(r.Qty, want-got)
		if ss != nil {
			take = ss.DecrementAsteroidQty(r.EntityID, take)
			if take <= 0 {
				r.Qty = 0
				continue
			}
			if ss.GetAsteroidQty(r.EntityID) <= 0 {
				ss.MarkAsteroidDepleted(r.EntityID)
			}
		} else {
			writes = append(writes, evedb.AsteroidDepletionRow{EntityID: r.EntityID, BeltID: r.BeltID, Qty: r.Qty - take})
		}
		r.Qty -= take
		got += take
		lots[r.TypeID] += take
	}
	if len(writes) > 0 {
		if err := evedb.FlushAsteroidDepletion(ctx, m.db, sysID, writes); err != nil {
			invalidateBeltField(sysID)
			return nil, 0, err
		}
	}
	out := make([]oreLot, 0, len(lots))
	for t, u := range lots {
		out = append(out, oreLot{TypeID: t, Units: u})
	}
	return out, got, nil
}

// ── the steps ───────────────────────────────────────────────────────────────

func (m *Mod) iceIdle(ctx context.Context, b *botRun, note string) (bool, error) {
	if note != "" {
		m.logBot(b.CharID, logInfo, "mining", "%s", note)
	}
	b.State.Activity, b.State.Phase = "", string(TaskIdle)
	b.State.Data = idleData(b.State.Data)
	return false, saveState(ctx, m.db, b.State)
}

// iceBoardStep boards the barge it travelled to and plans the trip.
func (m *Mod) iceBoardStep(ctx context.Context, b *botRun) (bool, error) {
	ship := b.State.Data.IceShipID
	if ship != 0 && ship != b.ShipID {
		if err := m.boardShip(ctx, b, ship); err != nil {
			return m.iceIdle(ctx, b, "could not board its barge: "+err.Error())
		}
	}
	if err := m.icePlan(ctx, b); err != nil {
		return m.iceIdle(ctx, b, "ice trip did not start: "+err.Error())
	}
	return false, nil
}

// iceWorkStep runs the harvest: one leg sized to the hold, then off to a
// refinery.
func (m *Mod) iceWorkStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	if b.SystemID != d.IceSysID || b.StationID != 0 {
		return false, m.beginTravel(ctx, b, d.IceSysID, 0, string(TaskIceWork))
	}
	// A projection with harvesters running fills the real hold (mine_embodied.go).
	if m.embodiedMining(b.CharID) {
		return m.iceWatchHold(ctx, b)
	}
	if len(d.IceLots) == 0 {
		harvesters := m.iceHarvesters(ctx, b.CharID, b.ShipID)
		if harvesters <= 0 {
			return m.iceIdle(ctx, b, "has no ice harvester aboard")
		}
		blocks := int64(m.shipHoldRoom(ctx, b) / 1000)
		if blocks < iceMinUnits {
			return m.iceToRefinery(ctx, b)
		}
		lots, got, err := m.iceTakeReal(ctx, d.IceSysID, d.IceBeltID, blocks)
		if err != nil {
			return false, err
		}
		if got <= 0 {
			return m.iceIdle(ctx, b, "the ice field at "+systemNameOf(ctx, m, d.IceSysID)+" is worked out")
		}
		cycle := m.iceCycleMs(ctx, b)
		m.parkAtCelestialEntity(ctx, b.ShipID, d.IceBeltID)
		d.IceLots = lots
		d.LegEndsAt = clock.Unix() + int64(math.Ceil(float64(got)*cycle/1000/float64(harvesters)))
		return false, saveState(ctx, m.db, st)
	}
	// The leg has run: the blocks go into the hold, as many as it has room for.
	delivered, err := m.iceDeliverLots(ctx, b, d.IceLots)
	if err != nil {
		return false, err
	}
	m.logBot(b.CharID, logInfo, "mining", "harvested %s", m.oreLotNote(ctx, delivered))
	d.IceLots = nil
	return m.iceToRefinery(ctx, b)
}

// iceDeliverLots puts a leg's debited ice blocks into the hold, each lot capped by the
// free space, which is read again because the hold can have filled since the leg
// started. It returns the lots as delivered. Blocks that do not fit are lost: the field
// was already debited.
func (m *Mod) iceDeliverLots(ctx context.Context, b *botRun, lots []oreLot) ([]oreLot, error) {
	free, err := m.holdFreeM3(ctx, b.CharID, b.ShipID)
	if err != nil {
		return nil, fmt.Errorf("ice delivery: %w", err)
	}
	delivered := lots[:0:0]
	for _, l := range lots {
		vol, verr := evedb.GetTypeVolume(ctx, m.db, l.TypeID)
		if verr != nil {
			vol = 0
		}
		units := holdCapUnits(l.Units, free, vol)
		if units <= 0 {
			continue
		}
		if err := m.addCargoStack(ctx, b, l.TypeID, units); err != nil {
			return delivered, err
		}
		free -= float64(units) * vol
		l.Units = units
		delivered = append(delivered, l)
		if verr == nil {
			m.emitOreMined(ctx, b.CharID, l.TypeID, units, float64(units)*vol, b.State.Data.IceSysID, true)
		}
	}
	return delivered, nil
}

// iceWatchHold is the ice.harvest step while the pilot's projection runs real
// harvesters. A leg the abstract tier had already debited is delivered first, so the
// hand-off neither loses nor repeats it. The trip ends when the hold has no room for a
// block or the projection reports the field worked out.
func (m *Mod) iceWatchHold(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	if len(d.IceLots) > 0 {
		delivered, err := m.iceDeliverLots(ctx, b, d.IceLots)
		if err != nil {
			return false, err
		}
		m.logBot(b.CharID, logInfo, "mining", "in space now — %s from this leg aboard, harvesters take over",
			m.oreLotNote(ctx, delivered))
		d.IceLots = nil
		if err := saveState(ctx, m.db, st); err != nil {
			return false, err
		}
	}
	if m.embodiedOreDry(b.CharID) {
		if m.iceAboard(ctx, b) > 0 {
			return m.iceToRefinery(ctx, b)
		}
		return m.iceIdle(ctx, b, "the ice field at "+systemNameOf(ctx, m, d.IceSysID)+" is worked out")
	}
	free, err := m.holdFreeM3(ctx, b.CharID, b.ShipID)
	if err != nil {
		return false, err
	}
	if free < 1000 {
		return m.iceToRefinery(ctx, b)
	}
	d.LegEndsAt = clock.Unix() + realMiningPollSec
	return false, saveState(ctx, m.db, st)
}

// iceAboard counts the ice blocks in the pilot's hold, every grade.
func (m *Mod) iceAboard(ctx context.Context, b *botRun) int64 {
	lots, err := m.cargoOreLots(ctx, b, 0)
	if err != nil {
		return 0
	}
	var n int64
	for _, l := range lots {
		if m.isIceType(ctx, l.TypeID) {
			n += l.Units
		}
	}
	return n
}

// iceCycleMs is one harvester cycle for this pilot in this ship: the harvester's own
// duration, less Ice Harvesting (-5% a level), Mining Barge (-3% a level, on an ore hull)
// and each Ice Harvester Upgrade in the lows (-5% each, multiplicative). The skill road
// prices its levers on this.
func (m *Mod) iceCycleMs(ctx context.Context, b *botRun) float64 {
	cycle, _, _ := evedb.GetTypeAttributeFloat(ctx, m.db, typeIceHarvesterI, "duration")
	if cycle <= 0 {
		cycle = iceDefaultCycleMs
	}
	// The Ice Harvesting hardwirings, the term the core's activation path folds.
	cycle *= evedb.GetIceCycleMult(ctx, m.db, b.CharID)
	if skills, err := m.charSkillsFor(ctx, b.CharID); err == nil {
		cycle *= 1 - iceHarvestCycleBonus*float64(min(5, skills[skillIceHarvesting]))
		var grp int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT t.groupID FROM entity e JOIN invTypes t ON t.typeID = e.typeID WHERE e.itemID = ?`,
			b.ShipID).Scan(&grp)
		if grp == groupMiningBarge || grp == groupExhumer {
			cycle *= 1 - iceBargeCycleBonus*float64(min(5, skills[skillMiningBarge]))
		}
	}
	var ups int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity WHERE locationID = ? AND typeID = ? AND flag BETWEEN 11 AND 18`,
		b.ShipID, typeIceHarvesterUpgradeI).Scan(&ups)
	for i := 0; i < ups; i++ {
		cycle *= 1 - iceHarvestCycleBonus
	}
	return math.Max(cycle, iceDefaultCycleMs/4)
}

// iceToRefinery flies the hold to the nearest station that reprocesses.
func (m *Mod) iceToRefinery(ctx context.Context, b *botRun) (bool, error) {
	var station, sys int64
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return false, err
	}
	near := bfsJumps(u, b.SystemID, 8)
	ids := make([]int64, 0, len(near))
	for s := range near {
		ids = append(ids, s)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, s.solarSystemID FROM staStations s
		 WHERE s.solarSystemID IN (`+int64CSV(ids)+`) AND COALESCE(s.reprocessingEfficiency, 0) > 0`)
	if err != nil {
		return false, err
	}
	// Nearest wins, but a refinery in a region whose towers are short of fuel counts
	// iceStockDetour jumps nearer, so fuel is listed where it is wanted.
	l := iceLedger()
	best := math.MaxInt32
	for rows.Next() {
		var st, sy int64
		if rows.Scan(&st, &sy) == nil {
			j := near[sy]
			if r := l.Regions[u.meta[sy].RegionID]; r != nil && len(r.Short) > 0 {
				j -= iceStockDetour
			}
			if j < best {
				best, station, sys = j, st, sy
			}
		}
	}
	rows.Close()
	if station == 0 {
		return m.iceIdle(ctx, b, "no refinery within reach of the ice — the blocks ride along")
	}
	return false, m.beginTravel(ctx, b, sys, station, string(TaskIceSell))
}

// iceSellStep stocks or sells the harvest. Blocks whose fuel this region's
// towers are short of are refined and the products sold into a bid here (a
// corporation's requisition) or listed here; blocks nobody is short of go
// into the best ice bid here, refined only when there is none.
func (m *Mod) iceSellStep(ctx context.Context, b *botRun) (bool, error) {
	if b.StationID == 0 {
		return m.iceIdle(ctx, b, "")
	}
	ids, _ := int64Column(ctx, m, `
		SELECT DISTINCT e.typeID FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.locationID = ? AND e.ownerID = ? AND e.flag = 5 AND t.groupID = ?`, b.ShipID, b.CharID, groupIceRock)
	for _, t := range ids {
		_ = m.moveCargoToHangar(ctx, b, t)
	}
	// Where this region's towers are short of what a block refines into, the block is refined
	// and the fuel listed. Elsewhere the blocks go into the best bid here.
	region := m.stationRegionID(ctx, b.StationID)
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.quantity FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.locationID = ? AND e.ownerID = ? AND e.flag = 4 AND t.groupID = ?`, b.StationID, b.CharID, groupIceRock)
	if err != nil {
		return false, err
	}
	type iceStack struct{ item, typeID, qty int64 }
	var stacks []iceStack
	for rows.Next() {
		var s iceStack
		if rows.Scan(&s.item, &s.typeID, &s.qty) == nil {
			stacks = append(stacks, s)
		}
	}
	rows.Close()
	rawSold, stocking := 0.0, false
	for _, s := range stacks {
		if m.regionShortFor(ctx, region, s.typeID) <= 0 {
			if got, ok := m.sellIceToBid(ctx, b, s.item, s.typeID, s.qty); ok {
				rawSold += got
				continue
			}
		} else {
			stocking = true
		}
		if _, err := m.refineStack(ctx, b, s.item); err != nil {
			m.logBot(b.CharID, logInfo, "mining", "could not refine ice here: %v", err)
		}
	}
	sold := 0.0
	for _, t := range iceProducts {
		var item, qty int64
		if m.db.QueryRowContext(ctx, `
			SELECT itemID, quantity FROM entity WHERE ownerID = ? AND locationID = ? AND flag = 4 AND typeID = ? AND singleton = 0
			 ORDER BY quantity DESC LIMIT 1`, b.CharID, b.StationID, t).Scan(&item, &qty) != nil || qty <= 0 {
			continue
		}
		price := m.iceSellPrice(ctx, b, t)
		if price <= 0 {
			continue
		}
		out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID, t, item, price, qty, -1, 1, 14, false)
		if err != nil || out == nil || out.ErrorMsg != "" {
			continue
		}
		for _, f := range out.Fills {
			sold += f.Price * float64(f.Quantity)
		}
	}
	note := "refined its ice"
	if stocking {
		note = "refined its ice and stocked the fuel for the towers in " + regionNameOf(ctx, m, region)
		// Its own listing counts against the shortfall from the next rebuild.
		iceLedgerAt.Store(0)
	}
	if sold > 0 {
		note += fmt.Sprintf(", %s of it sold on the spot", iskString(sold))
	}
	if rawSold > 0 {
		note = fmt.Sprintf("sold its ice blocks for %s — no tower here is short of fuel", iskString(rawSold))
		if stocking {
			note += "; the rest refined and stocked"
		}
	}
	return m.iceIdle(ctx, b, note)
}

// sellIceToBid sells one stack of ice blocks into the best bid at this
// station. ok is false when there is no bid to sell into.
func (m *Mod) sellIceToBid(ctx context.Context, b *botRun, itemID, typeID, qty int64) (float64, bool) {
	var bid sql.NullFloat64
	_ = m.db.QueryRowContext(ctx, `
		SELECT MAX(price) FROM market_orders WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0`,
		b.StationID, typeID).Scan(&bid)
	if !bid.Valid || bid.Float64 <= 0 {
		return 0, false
	}
	out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID, typeID, itemID, bid.Float64, qty, -1, 1, 14, false)
	if err != nil || out == nil || out.ErrorMsg != "" {
		return 0, false
	}
	got := 0.0
	for _, f := range out.Fills {
		got += f.Price * float64(f.Quantity)
	}
	return got, got > 0
}

// iceSellPrice is what an ice product is offered at here: the best bid here
// when it is fair, else a hair under the region's best ask, never under 85%
// of value.
func (m *Mod) iceSellPrice(ctx context.Context, b *botRun, typeID int64) float64 {
	val := evedb.TypeValueOf(ctx, m.db, typeID).Value
	var bid sql.NullFloat64
	_ = m.db.QueryRowContext(ctx, `
		SELECT MAX(price) FROM market_orders WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0`,
		b.StationID, typeID).Scan(&bid)
	if bid.Valid && bid.Float64 >= val*0.85 {
		return bid.Float64
	}
	ask := m.reqAskNear(ctx, typeID, b.StationID) * 0.98
	return math.Floor(math.Max(ask, val*0.85)*100) / 100
}
