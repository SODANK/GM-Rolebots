package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	evedb "gurimoon/db"
)

// Tower ice demand. A control tower burns one racial isotope plus Heavy Water and Liquid Ozone
// and keeps strontium for reinforcement, all refined from ice, so every anchored tower is a
// standing order on the ice fields. The ledger is per region and per product: for each tower not
// unanchoring, the stock it should hold (iceStockDays of its full-grid daily burn from
// posfuel.go, plus reinforcement strontium) less its own bay, then less what is listed for sale
// in the region. The rest is the shortfall. The ledger ignores ownership: player and bot towers
// count alike. Readers: iceOpportunity and iceBeatsRock (towerIcePull), pickIceField (the short
// isotope), iceSellStep (a miner in a short region lists the fuel instead of dumping blocks into
// an NPC bid) and iceDemandIndex, which prices the skill road (skills_ice.go).

const (
	// iceStockDays is how many days of a tower's burn the market should hold.
	iceStockDays = 14
	// iceLedgerTTL is how long the ledger stands; a tower event resets it.
	iceLedgerTTL = 900
	// iceTowerPullScale is the shortfall (ISK, distance-damped) that makes a tower pull of 1,
	// about one large tower's fortnight of isotopes.
	iceTowerPullScale = 50e6
	// iceTowerPullCap caps the local pull.
	iceTowerPullCap = 2.0
	// iceWorldPullScale and iceWorldPullCap are the world term on
	// iceDemandIndex: the skill road reads the galaxy, not one pilot's reach.
	iceWorldPullScale = 100e6
	iceWorldPullCap   = 1.5
	// iceTowerJumpDamp is how much each jump to a short region discounts it.
	iceTowerJumpDamp = 0.25
)

// regionIceNeed is one region's shortfall.
type regionIceNeed struct {
	Short  map[int64]int64 // product typeID to units short
	ISK    float64
	Towers int
}

// iceTowerLedger is the whole galaxy's tower fuel shortfall.
type iceTowerLedger struct {
	At       int64
	Regions  map[int64]*regionIceNeed
	WorldISK float64
	Towers   int
}

var (
	iceLedgerP  atomic.Pointer[iceTowerLedger]
	iceLedgerAt atomic.Int64
	iceLedgerMu sync.Mutex
	// iceYieldP is each ice type's refine yield per block (invorereprocessing).
	iceYieldP atomic.Pointer[map[int64]map[int64]int64]
)

// iceLedger is the current ledger (never nil).
func iceLedger() *iceTowerLedger {
	if p := iceLedgerP.Load(); p != nil {
		return p
	}
	return &iceTowerLedger{Regions: map[int64]*regionIceNeed{}}
}

// invalidateIceDemand makes the next reader rebuild both the ledger and the
// demand index: a tower was just anchored, onlined, taken down or lost.
func invalidateIceDemand() {
	iceLedgerAt.Store(0)
	iceDemandAt.Store(0)
}

// refreshIceLedger rebuilds the ledger when it is stale. Single-flight.
func (m *Mod) refreshIceLedger(ctx context.Context) {
	if clock.Unix()-iceLedgerAt.Load() < iceLedgerTTL {
		return
	}
	if !iceLedgerMu.TryLock() {
		return
	}
	defer iceLedgerMu.Unlock()
	iceLedgerAt.Store(clock.Unix())
	l, err := m.buildIceLedger(ctx)
	if err != nil {
		m.logBot(0, logInfo, "mining", "tower ice ledger not rebuilt: %v", err)
		return
	}
	iceLedgerP.Store(l)
}

// buildIceLedger reads every tower and the fuel already listed.
func (m *Mod) buildIceLedger(ctx context.Context) (*iceTowerLedger, error) {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return nil, fmt.Errorf("no map: %v", err)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.entityID, e.typeID, s.solarSystemID, s.ownerCorpID, s.ownerAllianceID
		  FROM starbase s JOIN entity e ON e.itemID = s.entityID
		 WHERE s.state <> ?`, evedb.StarbaseStateUnanchoring)
	if err != nil {
		return nil, err
	}
	type tower struct{ id, typeID, sys, corp, alliance int64 }
	var towers []tower
	for rows.Next() {
		var t tower
		if rows.Scan(&t.id, &t.typeID, &t.sys, &t.corp, &t.alliance) == nil {
			towers = append(towers, t)
		}
	}
	rows.Close()

	isProduct := map[int64]bool{}
	for _, p := range iceProducts {
		isProduct[p] = true
	}
	l := &iceTowerLedger{At: clock.Unix(), Regions: map[int64]*regionIceNeed{}}
	for _, t := range towers {
		batch, err := m.towerFuelBatch(ctx, t.id, t.typeID, t.sys, t.corp, t.alliance)
		if err != nil {
			continue
		}
		want := map[int64]int64{}
		for _, ln := range batch {
			if isProduct[ln.TypeID] {
				want[ln.TypeID] += ln.PerDay * iceStockDays
			}
		}
		for _, ln := range m.strontiumLine(ctx, t.id, t.typeID, t.sys, t.corp, t.alliance, strontiumHoursDefault) {
			if isProduct[ln.TypeID] {
				want[ln.TypeID] += ln.PerDay
			}
		}
		if len(want) == 0 {
			continue
		}
		short := towerShortfall(want, m.bayHeld(ctx, t.id))
		reg := u.meta[t.sys].RegionID
		r := l.Regions[reg]
		if r == nil {
			r = &regionIceNeed{Short: map[int64]int64{}}
			l.Regions[reg] = r
		}
		r.Towers++
		l.Towers++
		for p, q := range short {
			r.Short[p] += q
		}
	}
	if len(l.Regions) == 0 {
		return l, nil
	}

	// What the market already offers in those regions.
	regs := make([]int64, 0, len(l.Regions))
	for id := range l.Regions {
		regs = append(regs, id)
	}
	listed := map[int64]map[int64]int64{}
	lr, err := m.db.QueryContext(ctx, `
		SELECT st.regionID, o.typeID, SUM(o.volRemaining)
		  FROM market_orders o JOIN staStations st ON st.stationID = o.stationID
		 WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		   AND o.typeID IN (`+int64CSV(iceProducts)+`) AND st.regionID IN (`+int64CSV(regs)+`)
		 GROUP BY st.regionID, o.typeID`)
	if err == nil {
		for lr.Next() {
			var reg, t, q int64
			if lr.Scan(&reg, &t, &q) == nil {
				if listed[reg] == nil {
					listed[reg] = map[int64]int64{}
				}
				listed[reg][t] = q
			}
		}
		lr.Close()
	}
	vals, _ := evedb.TypeValues(ctx, m.db, iceProducts)
	for reg, r := range l.Regions {
		r.Short = towerShortfall(r.Short, listed[reg])
		for p, q := range r.Short {
			r.ISK += float64(q) * vals[p].Value
		}
		l.WorldISK += r.ISK
	}
	return l, nil
}

// towerShortfall is want less held, per product, dropping the satisfied. Pure.
func towerShortfall(want, held map[int64]int64) map[int64]int64 {
	out := map[int64]int64{}
	for t, q := range want {
		if s := q - held[t]; s > 0 {
			out[t] = s
		}
	}
	return out
}

// iceWorldPull is the ledger's term on iceDemandIndex. Pure.
func iceWorldPull(worldISK float64) float64 {
	if worldISK <= 0 {
		return 0
	}
	return math.Min(iceWorldPullCap, worldISK/iceWorldPullScale)
}

// iceRegionReach is each region within iceFieldReach jumps of a system, at
// its nearest jump count.
func iceRegionReach(u *universe, sysID int64) map[int64]int {
	out := map[int64]int{}
	for s, j := range bfsJumps(u, sysID, iceFieldReach) {
		reg := u.meta[s].RegionID
		if cur, ok := out[reg]; !ok || j < cur {
			out[reg] = j
		}
	}
	return out
}

// towerPullOf is the distance-damped shortfall over the regions in reach. Pure.
func towerPullOf(l *iceTowerLedger, reach map[int64]int) (pull float64, topRegion int64) {
	best := 0.0
	sum := 0.0
	for reg, j := range reach {
		r := l.Regions[reg]
		if r == nil || r.ISK <= 0 {
			continue
		}
		v := r.ISK / (1 + iceTowerJumpDamp*float64(j))
		sum += v
		if v > best {
			best, topRegion = v, reg
		}
	}
	return math.Min(iceTowerPullCap, sum/iceTowerPullScale), topRegion
}

// towerIcePull is the pull the towers in reach put on this pilot, and a line
// naming the biggest of them.
func (m *Mod) towerIcePull(ctx context.Context, b *botRun) (float64, string) {
	m.refreshIceLedger(ctx)
	l := iceLedger()
	if l.Towers == 0 || l.WorldISK <= 0 {
		return 0, ""
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return 0, ""
	}
	pull, reg := towerPullOf(l, iceRegionReach(u, b.SystemID))
	if pull <= 0 {
		return 0, ""
	}
	r := l.Regions[reg]
	return pull, fmt.Sprintf("%d tower(s) in %s short %s of fuel (%s)", r.Towers, regionNameOf(ctx, m, reg),
		iskString(r.ISK), m.shortProductsNote(ctx, r))
}

// shortProductsNote names a region's short products, biggest first.
func (m *Mod) shortProductsNote(ctx context.Context, r *regionIceNeed) string {
	type kv struct {
		t, q int64
	}
	var xs []kv
	for t, q := range r.Short {
		xs = append(xs, kv{t, q})
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i].q > xs[j].q })
	out := ""
	for i, x := range xs {
		if i == 2 {
			break
		}
		if out != "" {
			out += ", "
		}
		out += fmt.Sprintf("%d %s", x.q, m.typeNameOr(ctx, x.t))
	}
	return out
}

// regionShortFor is a region's shortfall in the products one ice type
// refines into: ISK of what one block would cover, 0 when none of it is short.
func (m *Mod) regionShortFor(ctx context.Context, regionID, iceType int64) float64 {
	r := iceLedger().Regions[regionID]
	if r == nil || len(r.Short) == 0 {
		return 0
	}
	vals, _ := evedb.TypeValues(ctx, m.db, iceProducts)
	v := 0.0
	for p, q := range m.iceYield(ctx, iceType) {
		if s := r.Short[p]; s > 0 {
			v += float64(min(q, s)) * vals[p].Value
		}
	}
	return v
}

// iceYield is one block of an ice type, refined (invorereprocessing).
func (m *Mod) iceYield(ctx context.Context, iceType int64) map[int64]int64 {
	if p := iceYieldP.Load(); p != nil {
		if y, ok := (*p)[iceType]; ok {
			return y
		}
	}
	all := map[int64]map[int64]int64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.oreTypeID, r.mineralTypeID, r.amountPerBatch
		  FROM invorereprocessing r JOIN invTypes t ON t.typeID = r.oreTypeID
		 WHERE t.groupID = ?`, groupIceRock)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var o, mt, q int64
		if rows.Scan(&o, &mt, &q) == nil {
			if all[o] == nil {
				all[o] = map[int64]int64{}
			}
			all[o][mt] = q
		}
	}
	rows.Close()
	iceYieldP.Store(&all)
	return all[iceType]
}
