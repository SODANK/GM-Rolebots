package rolebots

// The NPC ask as the anchor of every listing. The cost-plus, cheapest-ask and
// catalogue prices are blind to the NPC seeder's ask, the one price that is
// both real and stable: it restocks to depth every dynamics tick and anybody
// in the region can buy from it. Placement: a listing is never placed under
// anchor x npcAnchorFrac. Upkeep: undercutting another bot stops at anchor x
// npcAnchorFloorFrac, and an order under that band is repriced up to the
// placement price (nudgeOrder). The anchor only lifts. A type the NPC does not
// sell in the region is anchored to its metal when the region clears above it
// (askAnchor), else not at all.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"
	"time"

	evedb "gurimoon/db"
)

const (
	// The placement band: a generous pilot (MineralMarkup 1.0) lists at 90%
	// of the NPC ask, a greedy one (1.9) at 98%.
	npcAnchorFracLo = 0.90
	npcAnchorFracHi = 0.98

	// How far below the NPC ask bots may undercut each other. The room between
	// here and the placement band is where they compete; under it goods are given
	// away below what the NPC's stock says a buyer would pay.
	npcAnchorFloorFrac = 0.80
)

// npcAnchorFrac is the fraction of the NPC ask this pilot lists at.
func npcAnchorFrac(tn Tuning) float64 {
	f := npcAnchorFracLo + (tn.MineralMarkup-1.0)/0.9*(npcAnchorFracHi-npcAnchorFracLo)
	return math.Max(npcAnchorFracLo, math.Min(npcAnchorFracHi, f))
}

// liftToAnchor is the placement rule, pure: price, lifted to anchor × frac
// when it sits under it. anchor <= 0 means no NPC sells the type here and the
// price stands. Reports whether it lifted.
func liftToAnchor(price, anchor, frac float64) (float64, bool) {
	if anchor <= 0 || frac <= 0 {
		return price, false
	}
	target := roundISK(anchor * frac)
	if price < target {
		return target, true
	}
	return price, false
}

// npcAskAnchor is the cheapest NPC seeder ask for a type in the region that
// holds stationID, 0 when no NPC sells it there. Only the two catalogue
// seeders count (ore has its own ask rules, oreask.go). It reads the region,
// not the station, because a buyer's search reads the whole region.
func (m *Mod) npcAskAnchor(ctx context.Context, stationID, typeID int64) float64 {
	if stationID == 0 || typeID == 0 {
		return 0
	}
	t := m.npcAnchorTable(ctx)
	if t == nil {
		return 0 // an unreadable anchor is no anchor
	}
	region := t.stationRegion[stationID]
	if region == 0 {
		return 0
	}
	return t.asks[typeID][region]
}

// npcAnchorTTL is how long one read of the NPC book is trusted. The seeder's
// asks move only on the dynamics tick and the read is a heavy GROUP BY, while
// the order sweep needs an anchor for every open ask each minute.
const npcAnchorTTL = 15 * time.Minute

type npcAnchorBook struct {
	asks          map[int64]map[int64]float64 // typeID to regionID to cheapest NPC ask
	stationRegion map[int64]int64
}

var (
	npcAnchorMu     sync.Mutex // guards the two fields below
	npcAnchorCur    *npcAnchorBook
	npcAnchorAt     time.Time
	npcAnchorLoadMu sync.Mutex // one reload at a time
)

// npcAnchorTable returns the cached NPC book, reloading it when stale. A
// stale table keeps serving while one caller reloads; only a cold start
// waits.
func (m *Mod) npcAnchorTable(ctx context.Context) *npcAnchorBook {
	npcAnchorMu.Lock()
	cur, at := npcAnchorCur, npcAnchorAt
	npcAnchorMu.Unlock()
	if cur != nil && clock.Now().Sub(at) < npcAnchorTTL {
		return cur
	}
	if cur != nil {
		if !npcAnchorLoadMu.TryLock() {
			return cur // somebody is already reloading
		}
	} else {
		npcAnchorLoadMu.Lock()
	}
	defer npcAnchorLoadMu.Unlock()
	npcAnchorMu.Lock()
	if npcAnchorCur != nil && clock.Now().Sub(npcAnchorAt) < npcAnchorTTL {
		cur = npcAnchorCur
		npcAnchorMu.Unlock()
		return cur
	}
	npcAnchorMu.Unlock()

	sctx, cancel := snapContext(ctx)
	defer cancel()
	book, err := m.loadNPCAnchorBook(sctx)
	if err != nil {
		rlogf("rolebots: npc anchor book: %v", err)
		return cur
	}
	npcAnchorMu.Lock()
	npcAnchorCur, npcAnchorAt = book, clock.Now()
	npcAnchorMu.Unlock()
	return book
}

func (m *Mod) loadNPCAnchorBook(ctx context.Context) (*npcAnchorBook, error) {
	book := &npcAnchorBook{
		asks:          map[int64]map[int64]float64{},
		stationRegion: map[int64]int64{},
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, regionID, MIN(price) FROM market_orders
		 WHERE bid = 0 AND orderState = 1 AND volRemaining > 0 AND price > 0
		   AND charID IN (?, ?)
		 GROUP BY typeID, regionID`, evedb.SeederCharID, evedb.AdminSeederCharID)
	if err != nil {
		return nil, fmt.Errorf("npc asks: %w", err)
	}
	for rows.Next() {
		var typeID, regionID int64
		var p float64
		if err := rows.Scan(&typeID, &regionID, &p); err != nil {
			rows.Close()
			return nil, err
		}
		byRegion := book.asks[typeID]
		if byRegion == nil {
			byRegion = map[int64]float64{}
			book.asks[typeID] = byRegion
		}
		byRegion[regionID] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	srows, err := m.db.QueryContext(ctx, `SELECT stationID, regionID FROM staStations`)
	if err != nil {
		return nil, fmt.Errorf("station regions: %w", err)
	}
	defer srows.Close()
	for srows.Next() {
		var st, reg sql.NullInt64
		if err := srows.Scan(&st, &reg); err != nil {
			return nil, err
		}
		if st.Valid && reg.Valid {
			book.stationRegion[st.Int64] = reg.Int64
		}
	}
	return book, srows.Err()
}

const (
	// scrapAnchorMul puts a scrap anchor's repair line (npcAnchorFloorFrac)
	// exactly on the metal: 1.25 × 0.80 = 1.
	scrapAnchorMul = 1.25
	// scrapNormMargin is how far the usual price must clear the metal before
	// the floor stands. Under it a floor would sit at the market price, and a
	// falling market could not clear.
	scrapNormMargin = 1.125
)

// scrapAnchorFor is the synthetic anchor for a type with no NPC ask: 0 unless
// the region's own history shows it clears above its metal. A listing floored
// over what the market pays never fills. Pure.
func scrapAnchorFor(scrap, usual float64) float64 {
	if scrap <= 0 || usual < scrap*scrapNormMargin {
		return 0
	}
	return scrap * scrapAnchorMul
}

// askAnchor is what a listing is anchored to: the NPC ask, else the type's
// metal. Every placement and upkeep rule reads this one number; if they read
// different ones, liftUnderAnchor could not lift what the floor flags and the
// order would wedge in nudgeOrder.
func (m *Mod) askAnchor(ctx context.Context, stationID, typeID int64) float64 {
	if a := m.npcAskAnchor(ctx, stationID, typeID); a > 0 {
		return a
	}
	if stationID == 0 || typeID == 0 || !m.marketPlay(ctx).ScrapFloor {
		return 0
	}
	t := m.npcAnchorTable(ctx)
	if t == nil {
		return 0
	}
	usual := m.marketHistory(ctx).norm(typeID, t.stationRegion[stationID])
	if usual <= 0 {
		return 0
	}
	return scrapAnchorFor(m.scrapValue(ctx, typeID), usual)
}

// anchoredAsk applies the placement rule for one listing at one station.
func (m *Mod) anchoredAsk(ctx context.Context, tn Tuning, stationID, typeID int64, price float64) (float64, bool) {
	return liftToAnchor(price, m.askAnchor(ctx, stationID, typeID), npcAnchorFrac(tn))
}

// anchoredFloor is the upkeep rule: the lowest price bots may undercut each
// other to at this station, 0 when the type has no anchor here.
func (m *Mod) anchoredFloor(ctx context.Context, stationID, typeID int64) float64 {
	if a := m.askAnchor(ctx, stationID, typeID); a > 0 {
		return roundISK(a * npcAnchorFloorFrac)
	}
	return 0
}

// The competitive half of the NPC band, per pilot. anchoredFloor's 0.80 is the
// repair line; this is where a pilot stops undercutting a rival (0.80 generous,
// 0.90 greedy), so races do not end in identical prices. Never under 0.80.
const (
	npcCompeteFloorLo = npcAnchorFloorFrac
	npcCompeteFloorHi = 0.90
)

// anchoredFloorFor is anchoredFloor at this pilot's own competitive fraction.
func (m *Mod) anchoredFloorFor(ctx context.Context, tn Tuning, stationID, typeID int64) float64 {
	if a := m.askAnchor(ctx, stationID, typeID); a > 0 {
		return roundISK(a * (npcCompeteFloorLo + personalT(tn)*(npcCompeteFloorHi-npcCompeteFloorLo)))
	}
	return 0
}

// liftUnderAnchor reprices one ask under the anchor band back up to this
// pilot's placement price. Raising an order charges a broker fee on the
// difference (db.ModifyMarketOrder); a pilot that cannot cover it is left as
// it is and the next sweep asks again.
func (m *Mod) liftUnderAnchor(ctx context.Context, o orderRow, tune Tuning) {
	want, lifted := m.anchoredAsk(ctx, tune, o.StationID, o.TypeID, o.LivePrice)
	if !lifted {
		return
	}
	out, err := evedb.ModifyMarketOrder(ctx, m.db, o.CharID, o.OrderID, want)
	if err != nil || out == nil || out.ErrorMsg != "" {
		return
	}
	m.bookTrade(ctx, tradeEntry{
		CharID: o.CharID, Kind: tradeKindPrice, TypeID: o.TypeID, TypeName: o.TypeName,
		Qty: o.LiveVol, Price: want, Value: 0, Station: o.StationID, System: o.SystemID,
		OrderID: o.OrderID,
		Note:    fmt.Sprintf("repriced up to its anchor band (was %.2f)", o.LivePrice),
	})
	m.logBot(o.CharID, logInfo, "market",
		"its %s at station %d was far under its anchor — %.2f → %.2f",
		o.TypeName, o.StationID, o.LivePrice, want)
	_, _ = m.db.ExecContext(ctx,
		`UPDATE rolebot_orders SET price = ?, placedAt = ? WHERE orderID = ?`,
		want, clock.Unix(), o.OrderID)
}
