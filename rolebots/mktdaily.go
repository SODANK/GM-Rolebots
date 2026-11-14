package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	evedb "gurimoon/db"
)

// The market's own history, as a player reads it off the price chart and a
// station's trade tape before listing anything.
//
// rolebot_mkt_daily is market_transactions folded to (day, station, type) on
// a transactionID cursor (one more row in rolebot_mkt_flow_cursor), so a
// decision reads rollup rows instead of scanning the tape. Every fill counts:
// players, roster and NPC alike. Seeder snapshot rows carry stationID 0 and
// never enter. "Buy" is the buyer's row (transactionType 1), one per trade.
// "Sell" is the seller's row (type 0), which also carries sales into NPC bids
// that write no buyer row.

const (
	// mktCursorDaily is this ledger's row in rolebot_mkt_flow_cursor.
	mktCursorDaily = 3
	// mktDailyBootstrapDays is how far back a fresh ledger reads: a day past
	// the longest window a reader asks for, so it is ready in one catch-up.
	mktDailyBootstrapDays = mktRegionWindowDays + 1
	// mktDailyRetentionDays is how long a day row is kept.
	mktDailyRetentionDays = 30

	// mktStationWindowDays is what "sells at this station" means: the last week.
	// mktRegionWindowDays is the region's demand and price reference: a fortnight,
	// so a thin type still has a number and one week of dumping cannot set it.
	mktStationWindowDays = 7
	mktRegionWindowDays  = 14
	// mktRecentDays is the leading edge the reference compares against its
	// fortnight: a market that has fallen since is priced where it is now.
	mktRecentDays = 3
	// mktNormMinDays is how many trading days a type needs in a region before
	// its median counts as a usual price.
	mktNormMinDays = 3
	// mktAbsorbDays is how many days of the region's buying one listing may
	// count on. A week matches the order's patience scale (orderPatience runs
	// hours, the pull 3x that).
	mktAbsorbDays = 7.0

	// mktHistoryTTL is how long the readers' copy stands. Day rows move by
	// the minute, a fortnight's demand does not.
	mktHistoryTTL = 10 * time.Minute

	// win32TicksPerDay and unixEpochWin32Days turn a FILETIME into a unix day
	// in SQL: 11644473600 s between 1601 and 1970 is exactly 134774 days.
	win32TicksPerDay   = 864000000000
	unixEpochWin32Days = 134774
)

// mktDailyPass folds new transactions into rolebot_mkt_daily. One batch is a
// transactionID range; the cursor moves after each, so a pass cut short
// resumes where it stopped.
func (m *Mod) mktDailyPass(ctx context.Context, now int64) error {
	cursor, err := m.mktTapeCursor(ctx, now, mktCursorDaily, mktDailyBootstrapDays)
	if err != nil {
		return err
	}
	var head int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(transactionID), 0) FROM market_transactions`).Scan(&head); err != nil {
		return fmt.Errorf("mkt daily head: %w", err)
	}
	day := fmt.Sprintf("CAST(t.transactionDateTime DIV %d AS SIGNED) - %d", win32TicksPerDay, unixEpochWin32Days)
	for i := 0; i < iskFlowBatches && cursor < head; i++ {
		hi := cursor + iskFlowWindowIDs
		if hi > head {
			hi = head
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_mkt_daily
				(day, stationID, typeID, regionID, buyUnits, buyISK, buyN, sellUnits, sellISK, minPrice, maxPrice,
				 extSellUnits)
			SELECT `+day+` AS d, t.stationID, t.typeID, MAX(t.regionID),
			       SUM(IF(t.transactionType = 1, t.quantity, 0)),
			       SUM(IF(t.transactionType = 1, t.price * t.quantity, 0)),
			       SUM(t.transactionType = 1),
			       SUM(IF(t.transactionType = 0, t.quantity, 0)),
			       SUM(IF(t.transactionType = 0, t.price * t.quantity, 0)),
			       MIN(t.price), MAX(t.price),
			       SUM(IF(t.transactionType = 0 AND rc.characterID IS NULL, t.quantity, 0))
			  FROM market_transactions t
			  LEFT JOIN rolebot_characters rc ON rc.characterID = t.clientID
			 WHERE t.transactionID > ? AND t.transactionID <= ?
			   AND t.stationID > 0 AND t.quantity > 0 AND t.price > 0
			 GROUP BY d, t.stationID, t.typeID
			ON DUPLICATE KEY UPDATE
			    buyUnits = buyUnits + VALUES(buyUnits), buyISK = buyISK + VALUES(buyISK),
			    buyN = buyN + VALUES(buyN),
			    sellUnits = sellUnits + VALUES(sellUnits), sellISK = sellISK + VALUES(sellISK),
			    minPrice = LEAST(minPrice, VALUES(minPrice)),
			    maxPrice = GREATEST(maxPrice, VALUES(maxPrice)),
			    extSellUnits = extSellUnits + VALUES(extSellUnits)`,
			cursor, hi); err != nil {
			return fmt.Errorf("mkt daily batch %d..%d: %w", cursor, hi, err)
		}
		cursor = hi
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_mkt_flow_cursor (id, lastTxID) VALUES (?, ?)
			ON DUPLICATE KEY UPDATE lastTxID = VALUES(lastTxID)`, mktCursorDaily, cursor); err != nil {
			return fmt.Errorf("mkt daily cursor: %w", err)
		}
	}
	return nil
}

// ── the readers' copy ──────────────────────────────────────────────────────

// mktFlow is what traded of one type at one place over a window.
type mktFlow struct {
	BuyUnits  int64
	BuyISK    float64
	BuyN      int64
	SellUnits int64
	// ExtSellUnits is SellUnits less what went into rostered pilots' bids:
	// what sellers sold to somebody other than the roster.
	ExtSellUnits int64
	MaxPrice     float64 // the dearest any unit changed hands at
}

// mktBuyer is one station that bought a type over the station window.
type mktBuyer struct {
	StationID int64
	Units     int64
}

// mktNorm is a type's usual price in a region: the median of its daily
// averages over the region window, and how many days traded. Unlike Ref it
// does not follow a crash down, so it is what a crash is measured against.
type mktNorm struct {
	Price float64
	Days  int
}

// mktRecent is what end users bought over the last mktRecentDays: the roster's
// resale legs are off it, so a trader's own buying neither reads as demand nor
// moves the price. Days counts the days the type traded at all.
type mktRecent struct {
	Units int64
	Avg   float64
	Days  int
}

// mktHistory is the market's history as the trade logic reads it. Ready is
// false until the ledger covers the whole region window; every reader passes
// through while it is false, since an unready ledger reads "nobody bought it".
type mktHistory struct {
	Ready   bool
	Station map[[2]int64]mktFlow // (stationID, typeID), last mktStationWindowDays
	Region  map[[2]int64]mktFlow // (regionID, typeID), last mktRegionWindowDays
	Global  map[int64]mktFlow    // typeID, every region, last mktRegionWindowDays
	Ref     map[[2]int64]float64 // (typeID, regionID): what it has been clearing at
	Buyers  map[int64][]mktBuyer // typeID to the stations that bought it, most first
	Norm    map[[2]int64]mktNorm // (typeID, regionID): the usual price
	// Recent is empty when the resale read failed: no evidence, so no crash
	// or surge is acted on.
	Recent map[[2]int64]mktRecent // (typeID, regionID): last mktRecentDays, net
	// StationResale maps (stationID, typeID) to the units the roster bought there
	// for resale over the last mktRecentDays.
	StationResale map[[2]int64]int64
}

func emptyMktHistory() *mktHistory {
	return &mktHistory{
		Station: map[[2]int64]mktFlow{},
		Region:  map[[2]int64]mktFlow{},
		Global:  map[int64]mktFlow{},
		Ref:     map[[2]int64]float64{},
		Buyers:  map[int64][]mktBuyer{},
		Norm:    map[[2]int64]mktNorm{},
		Recent:  map[[2]int64]mktRecent{},

		StationResale: map[[2]int64]int64{},
	}
}

// norm is the type's usual price in the region, 0 until the history is Ready
// and the type has traded on mktNormMinDays days.
func (h *mktHistory) norm(typeID, regionID int64) float64 {
	if !h.Ready {
		return 0
	}
	if n := h.Norm[[2]int64{typeID, regionID}]; n.Days >= mktNormMinDays {
		return n.Price
	}
	return 0
}

// recentPerDay is the units end users bought in the region per day lately.
func (h *mktHistory) recentPerDay(typeID, regionID int64) float64 {
	return float64(h.Recent[[2]int64{typeID, regionID}].Units) / mktRecentDays
}

// stationPerDay is the units of this type bought at the station per day.
func (h *mktHistory) stationPerDay(stationID, typeID int64) float64 {
	return float64(h.Station[[2]int64{stationID, typeID}].BuyUnits) / mktStationWindowDays
}

// regionPerDay is the units of this type bought anywhere in the region per day.
func (h *mktHistory) regionPerDay(regionID, typeID int64) float64 {
	return float64(h.Region[[2]int64{regionID, typeID}].BuyUnits) / mktRegionWindowDays
}

// globalPerDay is the units of this type bought anywhere in the cluster per day.
func (h *mktHistory) globalPerDay(typeID int64) float64 {
	return float64(h.Global[typeID].BuyUnits) / mktRegionWindowDays
}

// marketHistory is the readers' entry point. Never nil: a failed read answers
// an empty, not-Ready history, which every reader treats as "no evidence"
// and passes through.
func (m *Mod) marketHistory(ctx context.Context) *mktHistory {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	h, err := snapshotFor(snapKey("mkt-history"), mktHistoryTTL, func() (*mktHistory, error) {
		return m.loadMarketHistory(sctx)
	})
	if err != nil || h == nil {
		return emptyMktHistory()
	}
	return h
}

func (m *Mod) loadMarketHistory(ctx context.Context) (*mktHistory, error) {
	out := emptyMktHistory()
	today := clock.Unix() / 86400
	var lo, hi sql.NullInt64
	if err := m.db.QueryRowContext(ctx,
		`SELECT MIN(day), MAX(day) FROM rolebot_mkt_daily`).Scan(&lo, &hi); err != nil {
		return nil, fmt.Errorf("mkt history span: %w", err)
	}
	if !lo.Valid {
		return out, nil
	}
	out.Ready = mktHistoryCovers(lo.Int64, hi.Int64, today)

	regionFrom := today - mktRegionWindowDays + 1
	stationFrom := today - mktStationWindowDays + 1
	rows, err := m.db.QueryContext(ctx, capStatement(`
		SELECT day, stationID, typeID, regionID, buyUnits, buyISK, buyN, sellUnits, maxPrice, extSellUnits
		  FROM rolebot_mkt_daily WHERE day >= ?`), regionFrom)
	if err != nil {
		return nil, fmt.Errorf("mkt history: %w", err)
	}
	defer rows.Close()
	days := map[[2]int64][]mktDay{} // (typeID, regionID) to day VWAPs
	dayAt := map[[3]int64]int{}     // (typeID, regionID, day) to index in days
	for rows.Next() {
		var day, station, typeID, region, buyUnits, buyN, sellUnits, extSell int64
		var buyISK, maxPrice float64
		if err := rows.Scan(&day, &station, &typeID, &region, &buyUnits, &buyISK, &buyN,
			&sellUnits, &maxPrice, &extSell); err != nil {
			return nil, err
		}
		rk := [2]int64{region, typeID}
		r := out.Region[rk]
		r.BuyUnits += buyUnits
		r.BuyISK += buyISK
		r.BuyN += buyN
		r.SellUnits += sellUnits
		r.MaxPrice = max(r.MaxPrice, maxPrice)
		out.Region[rk] = r
		g := out.Global[typeID]
		g.BuyUnits += buyUnits
		g.SellUnits += sellUnits
		out.Global[typeID] = g
		if day >= stationFrom {
			sk := [2]int64{station, typeID}
			s := out.Station[sk]
			s.BuyUnits += buyUnits
			s.BuyISK += buyISK
			s.BuyN += buyN
			s.SellUnits += sellUnits
			s.ExtSellUnits += extSell
			s.MaxPrice = max(s.MaxPrice, maxPrice)
			out.Station[sk] = s
		}
		if buyUnits > 0 {
			tk := [2]int64{typeID, region}
			dk := [3]int64{typeID, region, day}
			if i, ok := dayAt[dk]; ok {
				days[tk][i].Units += buyUnits
				days[tk][i].ISK += buyISK
			} else {
				dayAt[dk] = len(days[tk])
				days[tk] = append(days[tk], mktDay{Day: day, Units: buyUnits, ISK: buyISK})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	resale, stationResale, rerr := m.recentResale(ctx, (today-mktRecentDays+1)*86400)
	if rerr != nil {
		rlogf("rolebots: %v — no recent market read this pass", rerr)
	} else {
		out.StationResale = stationResale
	}
	for k, ds := range days {
		if p := clearingRef(ds, today); p > 0 {
			out.Ref[k] = p
		}
		if n := clearingNorm(ds); n.Days > 0 {
			out.Norm[k] = n
		}
		if rerr == nil {
			if r := recentFlow(ds, today, resale[k]); r.Units > 0 {
				out.Recent[k] = r
			}
		}
	}
	for k, s := range out.Station {
		if s.BuyUnits > 0 {
			out.Buyers[k[1]] = append(out.Buyers[k[1]], mktBuyer{StationID: k[0], Units: s.BuyUnits})
		}
	}
	for t := range out.Buyers {
		b := out.Buyers[t]
		sort.Slice(b, func(i, j int) bool {
			if b[i].Units != b[j].Units {
				return b[i].Units > b[j].Units
			}
			return b[i].StationID < b[j].StationID
		})
	}
	return out, nil
}

// recentResale is the roster's resale buying since `since`: by (typeID,
// regionID) with its ISK, and by (stationID, typeID) in units.
func (m *Mod) recentResale(ctx context.Context, since int64) (map[[2]int64]resaleLeg, map[[2]int64]int64, error) {
	rows, err := m.db.QueryContext(ctx, capStatement(`
		SELECT t.typeID, s.regionID, t.stationID, SUM(t.quantity), SUM(t.value)
		  FROM rolebot_trades t
		  JOIN mapSolarSystems s ON s.solarSystemID = t.systemID
		 WHERE t.resale = 1 AND t.ts >= ? AND t.quantity > 0
		 GROUP BY t.typeID, s.regionID, t.stationID`), since)
	if err != nil {
		return nil, nil, fmt.Errorf("recent resale: %w", err)
	}
	defer rows.Close()
	byRegion := map[[2]int64]resaleLeg{}
	byStation := map[[2]int64]int64{}
	for rows.Next() {
		var typeID, region, station int64
		var qty sql.NullInt64
		var isk sql.NullFloat64
		if err := rows.Scan(&typeID, &region, &station, &qty, &isk); err != nil {
			return nil, nil, fmt.Errorf("recent resale: %w", err)
		}
		k := [2]int64{typeID, region}
		l := byRegion[k]
		l.Units += qty.Int64
		l.ISK += isk.Float64
		byRegion[k] = l
		byStation[[2]int64{station, typeID}] += qty.Int64
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("recent resale: %w", err)
	}
	return byRegion, byStation, nil
}

// mktHistoryCovers says the ledger holds the whole region window and has
// caught up to yesterday. Pure.
func mktHistoryCovers(minDay, maxDay, today int64) bool {
	return minDay <= today-mktRegionWindowDays+1 && maxDay >= today-1
}

// mktDay is one day's buying of one type in one region.
type mktDay struct {
	Day   int64
	Units int64
	ISK   float64
}

// clearingRef is what a type has been clearing at in a region: the median of
// its daily average prices over the window, or the recent average when the
// last mktRecentDays traded lower. Pure.
func clearingRef(days []mktDay, today int64) float64 {
	n := clearingNorm(days)
	if n.Days == 0 {
		return 0
	}
	med := n.Price
	var recentUnits int64
	var recentISK float64
	for _, d := range days {
		if d.Units > 0 && d.ISK > 0 && d.Day > today-mktRecentDays {
			recentUnits += d.Units
			recentISK += d.ISK
		}
	}
	if recentUnits > 0 {
		if recent := recentISK / float64(recentUnits); recent < med {
			return recent
		}
	}
	return med
}

// clearingNorm is the median of the window's daily averages and how many days
// traded. Pure.
func clearingNorm(days []mktDay) mktNorm {
	var avgs []float64
	for _, d := range days {
		if d.Units > 0 && d.ISK > 0 {
			avgs = append(avgs, d.ISK/float64(d.Units))
		}
	}
	if len(avgs) == 0 {
		return mktNorm{}
	}
	sort.Float64s(avgs)
	med := avgs[len(avgs)/2]
	if len(avgs)%2 == 0 {
		med = (avgs[len(avgs)/2-1] + avgs[len(avgs)/2]) / 2
	}
	return mktNorm{Price: med, Days: len(avgs)}
}

// recentFlow is the last mktRecentDays of a type's days with the roster's
// resale buying taken off (netResale). Zero when resale was all of it. Pure.
func recentFlow(days []mktDay, today int64, r resaleLeg) mktRecent {
	var units int64
	var isk float64
	n := 0
	for _, d := range days {
		if d.Units > 0 && d.ISK > 0 && d.Day > today-mktRecentDays {
			units += d.Units
			isk += d.ISK
			n++
		}
	}
	if units <= 0 {
		return mktRecent{}
	}
	t := netResale(tapeRef{Avg: isk / float64(units), Trades: 1, Units: units}, isk, r)
	if t.Units <= 0 {
		return mktRecent{}
	}
	return mktRecent{Units: t.Units, Avg: t.Avg, Days: n}
}

// ── what the readers decide ────────────────────────────────────────────────

// marketAbsorb is how many of want units a listing can count on selling: a
// week of what the whole region has been buying. Zero means the region bought
// none of it in the window. Pure.
func marketAbsorb(want int64, regionPerDay float64) int64 {
	if want <= 0 || regionPerDay <= 0 {
		return 0
	}
	cap := int64(regionPerDay * mktAbsorbDays)
	if cap < want {
		return cap
	}
	return want
}

// regionBoughtNone is the trade gate's refusal for paths that list without a
// run behind them (a hangar stray, salvage, a miner's ore): the station's
// region bought none of the type in the region window. False whenever the
// ledger is not Ready.
func (m *Mod) regionBoughtNone(ctx context.Context, stationID, typeID int64) bool {
	hist := m.marketHistory(ctx)
	if !hist.Ready {
		return false
	}
	return hist.regionPerDay(m.stationRegionOf(ctx, stationID), typeID) <= 0
}

// The build side of the record: a factory asks the history whether the region
// already holds more than it buys (historyFlood, globalFlood) and how much it
// will take (historyCapRuns). See docs/comment-notes-b14.md#market-history-build-side.
const (
	// mktBuildCoverDays is how many days of the region's buying the supply may
	// already hold before a factory stops adding to it: two sell-order lifetimes
	// (industrySellDays).
	mktBuildCoverDays = 14.0
	// mktBuildAbsorbDays is the horizon a batch is sized to: the same fortnight,
	// net of what is already there.
	mktBuildAbsorbDays = 14.0
	// mktGlobalCoverDays is mktBuildCoverDays for the cluster's record: twice
	// the fortnight, because stock elsewhere has to be hauled to a buyer.
	mktGlobalCoverDays = 28.0
)

// historyFlood is the flood cut off the region's record. decided is false
// when the ledger is not Ready or the type was neither bought nor listed in
// the region; the caller then asks globalFlood. Flooded once supply covers
// mktBuildCoverDays of buying, or outright when listed and never bought. Pure.
func historyFlood(ready bool, perDay float64, regionListed, supply int64) (flooded, decided bool) {
	return historyFloodCover(ready, perDay, regionListed, supply, mktBuildCoverDays)
}

// historyFloodCover is historyFlood at a given cover in days. Pure.
func historyFloodCover(ready bool, perDay float64, listed, supply int64, coverDays float64) (flooded, decided bool) {
	switch {
	case !ready:
		return false, false
	case perDay > 0:
		return float64(supply) >= perDay*coverDays, true
	case listed > 0:
		return true, true
	}
	return false, false
}

// globalFlood is historyFlood off the cluster's record, for a type the
// region has no record of. perDay and listed are the cluster's, supply its
// listings plus the roster's held stock; the cover is mktGlobalCoverDays.
// decided is false when the cluster has no record either. Pure.
func globalFlood(ready bool, perDay float64, listed, supply int64) (flooded, decided bool) {
	return historyFloodCover(ready, perDay, listed, supply, mktGlobalCoverDays)
}

// historyCapRuns caps a batch at what the region buys over mktBuildAbsorbDays,
// net of the supply already standing. Runs pass unchanged without a record.
// Never under one run: a candidate that passed the flood cut is worth the
// smallest batch. Pure.
func historyCapRuns(runs int, ready bool, perDay float64, supply, portion int64) int {
	if !ready || perDay <= 0 || runs < 1 {
		return runs
	}
	if portion < 1 {
		portion = 1
	}
	room := int64(perDay*mktBuildAbsorbDays) - max(supply, 0)
	capRuns := int((room + portion - 1) / portion)
	if capRuns < 1 {
		capRuns = 1
	}
	return min(runs, capRuns)
}

// specVelocityConf scales a speculation's confidence by how much of the lot
// this station has been buying in a week. A dock with its own record earns the
// full confidence; one where only the region buys keeps most of it. Pure.
func specVelocityConf(conf, stationPerDay float64, units int64) float64 {
	if units <= 0 {
		return conf
	}
	share := stationPerDay * mktStationWindowDays / float64(units)
	if share > 1 {
		share = 1
	}
	return conf * (0.7 + 0.3*share)
}

// sellingStation is the best station in reach to list a holding at, by the
// market's record of buying it: units bought there in a week, discounted by
// the jumps from where the goods sit. 0 when no station in reach has bought
// any. fromHops is the reach map's hop count to the goods' own system.
func (m *Mod) sellingStation(ctx context.Context, h *mktHistory, typeID, exceptStation int64,
	jumps map[int64]int, fromHops int) (int64, int64) {

	const maxLooked = 40
	var bestStation, bestSystem int64
	var bestRank float64
	for i, c := range h.Buyers[typeID] {
		if i >= maxLooked {
			break
		}
		if c.StationID == exceptStation {
			continue
		}
		sys := m.stationSystemOf(ctx, c.StationID)
		hops, ok := jumps[sys]
		if sys == 0 || !ok {
			continue
		}
		d := hops - fromHops
		if d < 0 {
			d = -d
		}
		if rank := float64(c.Units) / float64(1+d); rank > bestRank {
			bestRank, bestStation, bestSystem = rank, c.StationID, sys
		}
	}
	return bestStation, bestSystem
}

// regionSellingStation is sellingStation inside one region, with the roster's
// own resale buying taken off each dock: a dock a trader has just bought out
// reads as a great buyer otherwise. 0 when no dock there bought any.
func (m *Mod) regionSellingStation(ctx context.Context, h *mktHistory, typeID, regionID, exceptStation int64,
	jumps map[int64]int, fromHops int) (int64, int64) {

	var bestStation, bestSystem int64
	var bestRank float64
	for _, c := range h.Buyers[typeID] {
		if c.StationID == exceptStation || m.stationRegionOf(ctx, c.StationID) != regionID {
			continue
		}
		net := c.Units - h.StationResale[[2]int64{c.StationID, typeID}]
		if net <= 0 {
			continue
		}
		sys := m.stationSystemOf(ctx, c.StationID)
		hops, ok := jumps[sys]
		if sys == 0 || !ok {
			continue
		}
		d := hops - fromHops
		if d < 0 {
			d = -d
		}
		if rank := float64(net) / float64(1+d); rank > bestRank {
			bestRank, bestStation, bestSystem = rank, c.StationID, sys
		}
	}
	return bestStation, bestSystem
}

// mktTapeCursor is a transactions cursor in rolebot_mkt_flow_cursor, started
// bootstrapDays back on a fresh ledger (through idx_mt_type_date_agg, as
// liquidityCursor does).
func (m *Mod) mktTapeCursor(ctx context.Context, now int64, id, bootstrapDays int) (int64, error) {
	var cur int64
	err := m.db.QueryRowContext(ctx,
		`SELECT lastTxID FROM rolebot_mkt_flow_cursor WHERE id = ?`, id).Scan(&cur)
	if err == nil {
		return cur, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("mkt tape cursor %d: %w", id, err)
	}
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MIN(transactionID), 0) FROM market_transactions
		 WHERE transactionType = 1 AND transactionDateTime >= ?`,
		evedb.UnixToWin32(now-int64(bootstrapDays)*86400)).Scan(&cur); err != nil {
		return 0, fmt.Errorf("mkt tape bootstrap %d: %w", id, err)
	}
	if cur > 0 {
		cur-- // exclusive
	}
	return cur, nil
}

// stationSoldTypes is what sellers have sold at a station over the station
// window, busiest first, at most limit types. It counts only sales to
// somebody other than the roster (ExtSellUnits), so roster bids filled by
// roster sellers do not make a dock look busy. Pure over the history.
func stationSoldTypes(h *mktHistory, stationID int64, limit int) []int64 {
	type sold struct {
		typeID int64
		units  int64
	}
	var all []sold
	for k, f := range h.Station {
		if k[0] == stationID && f.ExtSellUnits > 0 {
			all = append(all, sold{k[1], f.ExtSellUnits})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].units != all[j].units {
			return all[i].units > all[j].units
		}
		return all[i].typeID < all[j].typeID
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	out := make([]int64, 0, len(all))
	for _, s := range all {
		out = append(out, s.typeID)
	}
	return out
}
