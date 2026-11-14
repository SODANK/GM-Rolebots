package rolebots

import (
	"context"
	"math"
	"sort"
	"time"

	evedb "gurimoon/db"
)

// Deep minerals (Nocxium, Zydrine, Megacyte) in the ordinary corporate buyback.
// Hulls above a frigate eat a lot of them and they come out of null-sec ore
// nobody digs unless it pays, so a corporation whose work board needs them
// pulls three levers as part of the plain buyback (corpbuyback.go):
//   - Bid it up: opens at DeepPremium x the tape and climbs deepBidStep a day
//     while unfilled (tendDeepBids), to deepCeiling. The bid is real escrow.
//   - Send operations: the Director of Operations' fleet order names deep ores
//     first (operationsDesk) and notes the want (noteDeepMineralWants).
//   - Ask the members: members at the factory sell hangar stock into the bids
//     (sellInToCorpBids). See docs/comment-notes-b04.md#deep-minerals.

const (
	// deepCeilMul is the most a deep-mineral bid climbs to over its tape; the
	// default of rolebots.deepBid.ceilMul.
	deepCeilMul = 2.0
	// deepCeilMulKey lets an operator brake (or loosen) that multiple,
	// clamped to deepCeilMulMin..deepCeilMulMax.
	deepCeilMulKey = "rolebots.deepBid.ceilMul"
	deepCeilMulMin = 1.0
	deepCeilMulMax = 4.0
	// deepBaseAnchorMul is the static anchor: no premium past this multiple of the
	// metal's basePrice. The tape includes the corporations' own fills, so a ceiling
	// that floated only with it could ratchet up. It never takes a bid under the tape.
	deepBaseAnchorMul = 3.0
	// deepBidStep is how far a deep bid climbs, as a share of the tape, per
	// deepBidStepSec unfilled.
	deepBidStep    = 0.10
	deepBidStepSec = 86400
	// deepCeilTTL is how long one (corporation, station, metal) set of order
	// needs stands: tend, retire and placement each ask per bid.
	deepCeilTTL = 10 * time.Minute
)

// deepPricedTypes is the metals bid on the tape with the deep premium. Wider
// than isDeepMineral: Nocxium has a real ask book, so it keeps the stale-bid buy
// trip and stays out of the deep-ore fleet order.
var deepPricedTypes = []int64{typeNocxium, typeZydrine, typeMegacyte}

// deepPriced reports whether a type is bid on the tape with the deep premium.
func deepPriced(typeID int64) bool {
	for _, t := range deepPricedTypes {
		if t == typeID {
			return true
		}
	}
	return false
}

// isDeepMineral reports whether a type is one of the two metals that gate big
// hulls and come only out of deep ore: the deep-ore fleet order, the mineral
// ledger and the stale-bid sweep's skip read this, not deepPriced.
func isDeepMineral(typeID int64) bool { return typeID == typeZydrine || typeID == typeMegacyte }

// deepBidMul is the multiple of its tape a fresh bid for a type opens at: the
// CEO's DeepPremium for a deep-priced metal, 1 for anything else. Pure.
func deepBidMul(typeID int64, ch Charter) float64 {
	if !deepPriced(typeID) || ch.DeepPremium <= 1 {
		return 1
	}
	return ch.DeepPremium
}

// deepBidCeiling is the most one unit of a mineral may cost before a run's
// bill eats the whole of what the run sells for: what the run lists at, less
// everything else on it, over the units of this mineral it needs. Pure.
func deepBidCeiling(listPerRun, otherPerRun float64, qtyPerRun int64) float64 {
	if qtyPerRun <= 0 {
		return 0
	}
	return math.Max(0, (listPerRun-otherPerRun)/float64(qtyPerRun))
}

// deepRowNeed is one open build order's pull on a deep metal: the most a unit
// may cost before the order stops paying (deepBidCeiling), and how many units
// the order still eats (its weight).
type deepRowNeed struct {
	Ceiling float64
	Weight  float64
}

// deepRowsCeiling is what the orders a bid serves can pay for one unit: the
// weighted median ceiling of the orders that can pay the tape at all, 0 when
// none can. The median, not the minimum or best, so one thin or cost-plus row
// cannot set the price for the whole board. Pure.
func deepRowsCeiling(rows []deepRowNeed, ref float64) float64 {
	var paying []deepRowNeed
	total := 0.0
	for _, r := range rows {
		if r.Weight > 0 && r.Ceiling >= ref {
			paying = append(paying, r)
			total += r.Weight
		}
	}
	if len(paying) == 0 {
		return 0
	}
	sort.Slice(paying, func(i, j int) bool { return paying[i].Ceiling < paying[j].Ceiling })
	run := 0.0
	for _, r := range paying {
		run += r.Weight
		if run >= total/2 {
			return r.Ceiling
		}
	}
	return paying[len(paying)-1].Ceiling
}

// deepBidCap is the highest a deep bid may stand: the orders' ceiling (the tape
// when none can pay more), held under ceilMul x the tape and the base anchor,
// but the anchor never under the tape itself. Pure.
func deepBidCap(rowsCeil, ref, base, ceilMul float64) float64 {
	limit := rowsCeil
	if limit <= 0 {
		limit = ref
	}
	if ceilMul > 0 {
		limit = math.Min(limit, ceilMul*ref)
	}
	if base > 0 {
		limit = math.Min(limit, math.Max(ref, deepBaseAnchorMul*base))
	}
	return math.Max(limit, 0)
}

// nextDeepBid is where a resting deep bid moves on a day unfilled: a step up
// from where it stands, never under today's opening price, never over its
// limit. 0 when that is no higher than it stands. Pure.
func nextDeepBid(live, open, limit, step float64) float64 {
	next := math.Min(limit, math.Max(open, roundBidPrice(live+step)))
	if next <= live {
		return 0
	}
	return next
}

// deepCeilMulSetting is rolebots.deepBid.ceilMul, clamped.
func (m *Mod) deepCeilMulSetting(ctx context.Context) float64 {
	v := escrowSettingFloat(ctx, m.db, deepCeilMulKey, deepCeilMul)
	return math.Min(math.Max(v, deepCeilMulMin), deepCeilMulMax)
}

// deepRowNeeds is every open build order at a station that eats the metal,
// priced the way the board posted it: valued in the station's region, with a
// big hull the corporation builds for itself at cost plus (applyCostPlus). Other
// materials stay at book (TypeValues), as planRowAt valued them.
func (m *Mod) deepRowNeeds(ctx context.Context, c *corpCEO, ch Charter, stationID, typeID int64) []deepRowNeed {
	open, err := m.openWorkFor(ctx, c.CorpID)
	if err != nil {
		return nil
	}
	factory := m.corpFactoryStation(ctx, c.CorpID)
	region := m.stationRegionID(ctx, stationID)
	scope := costPlusScope(ctx, m.db)
	book := evedb.TypeValueOf(ctx, m.db, typeID).Value
	var out []deepRowNeed
	for _, w := range open {
		if w.Kind != workKindBuild || w.RunsLeft() <= 0 || orderStation(w, factory) != stationID {
			continue
		}
		mats, err := evedb.GetRequiredMaterials(ctx, m.db, w.BPTypeID, 1)
		if err != nil {
			continue
		}
		qty := 0.0
		for _, mt := range mats {
			if mt.TypeID == typeID {
				qty += float64(mt.Quantity) * mt.DamagePerJob
			}
		}
		if qty <= 0 {
			continue
		}
		row, err := m.planRowAt(ctx, ch, w.BPTypeID, region)
		if err != nil || row == nil {
			continue
		}
		row.Purpose = w.Purpose
		m.applyCostPlus(ctx, ch, row, m.typeKinds(ctx, []int64{row.ProductTypeID})[row.ProductTypeID].Group, scope)
		other := row.MatsPerRun - book*qty
		list := row.ValuePerRun*(1-oreDeskFeeFrac) - row.WagePerRun
		out = append(out, deepRowNeed{
			Ceiling: deepBidCeiling(list, other, int64(math.Ceil(qty))),
			Weight:  qty * float64(w.RunsLeft()),
		})
	}
	return out
}

// deepCeiling is the most a corporation's bid for a deep-priced metal at one
// station may stand at, given its tape ref (deepBidCap over deepRowNeeds).
// The one limit tendDeepBids climbs to and retireBuybackBids judges against.
func (m *Mod) deepCeiling(ctx context.Context, c *corpCEO, ch Charter, stationID, typeID int64, ref float64) float64 {
	if ref <= 0 {
		return 0
	}
	sctx, cancel := snapContext(ctx)
	defer cancel()
	rows, _ := snapshotFor(snapKey("deep-ceil", c.CorpID, stationID, typeID), deepCeilTTL, func() ([]deepRowNeed, error) {
		return m.deepRowNeeds(sctx, c, ch, stationID, typeID), nil
	})
	return deepBidCap(deepRowsCeiling(rows, ref), ref, m.basePriceCached(ctx, typeID), m.deepCeilMulSetting(ctx))
}

// deepBidLimit is deepCeiling as a bid price, never under the ordinary
// buyback price.
func (m *Mod) deepBidLimit(ctx context.Context, c *corpCEO, ch Charter, stationID, typeID int64, ref float64) float64 {
	normal := roundBidPrice(ref * buybackBid(ch.BuybackFrac))
	return math.Max(normal, roundBidPrice(m.deepCeiling(ctx, c, ch, stationID, typeID, ref)))
}

// deepBidPrice is what the corporation opens a bid for a unit of a deep
// metal at: the premium over its tape, held to the ceiling, and never under
// the ordinary buyback price.
func (m *Mod) deepBidPrice(ctx context.Context, c *corpCEO, ch Charter, stationID, typeID int64, ref float64) float64 {
	normal := roundBidPrice(ref * buybackBid(ch.BuybackFrac))
	open := roundBidPrice(math.Min(ref*deepBidMul(typeID, ch), m.deepCeiling(ctx, c, ch, stationID, typeID, ref)))
	return math.Max(normal, open)
}

// tendDeepBids climbs the corporation's resting bids for deep-priced metals:
// one that has sat a day without a fill (sweepCorpBids stamps repricedAt on a
// fill) moves to today's opening price or deepBidStep of the tape higher, up
// to deepBidLimit. The ordinary buyback leaves a resting bid where it is.
func (m *Mod) tendDeepBids(ctx context.Context, c *corpCEO, ch Charter) {
	args := []any{c.CorpID}
	args = append(args, int64Args(deepPricedTypes)...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.orderID, r.typeID, r.stationID, r.price, r.placedAt, r.repricedAt, o.charID, o.price, o.volRemaining
		  FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND r.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
		   AND r.typeID IN (`+placeholders(len(deepPricedTypes))+`)`, args...)
	if err != nil {
		return
	}
	type bid struct {
		orderID, typeID, stationID, placedAt, repAt, placer, rem int64
		price, live                                             float64
	}
	var list []bid
	for rows.Next() {
		var b bid
		if rows.Scan(&b.orderID, &b.typeID, &b.stationID, &b.price, &b.placedAt, &b.repAt, &b.placer, &b.live, &b.rem) == nil {
			list = append(list, b)
		}
	}
	rows.Close()
	now := clock.Unix()
	for _, b := range list {
		if b.placer == 0 || now-max(b.placedAt, b.repAt) < deepBidStepSec {
			continue
		}
		ref := m.buybackUnitValue(ctx, b.stationID, b.typeID, evedb.TypeValueOf(ctx, m.db, b.typeID).Value)
		if ref <= 0 {
			continue
		}
		limit := m.deepBidLimit(ctx, c, ch, b.stationID, b.typeID, ref)
		next := nextDeepBid(b.live, m.deepBidPrice(ctx, c, ch, b.stationID, b.typeID, ref), limit, ref*deepBidStep)
		if next <= 0 {
			continue // at its ceiling: it waits there
		}
		out, err := evedb.ModifyMarketOrder(ctx, m.db, b.placer, b.orderID, next)
		if err != nil || out == nil || out.ErrorMsg != "" {
			continue
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_orders SET price = ?, repricedAt = ? WHERE orderID = ?`,
			next, now, b.orderID)
		m.ceoLog(ctx, c, powerProduction, "deepbid", b.typeID, next*float64(b.rem),
			"raised its bid for %s to %s (×%.2f of what it trades for) — the work board still needs it",
			m.typeNameOr(ctx, b.typeID), iskString(next), next/ref)
	}
}

// corpDeepOutstanding is how many units of Zydrine and Megacyte the
// corporation's resting bids are still buying, by type.
func (m *Mod) corpDeepOutstanding(ctx context.Context, corpID int64) map[int64]int64 {
	out := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.typeID, COALESCE(SUM(o.volRemaining), 0) FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND r.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
		   AND r.typeID IN (?, ?)
		 GROUP BY r.typeID`, corpID, typeZydrine, typeMegacyte)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t, n int64
		if rows.Scan(&t, &n) == nil && n > 0 {
			out[t] = n
		}
	}
	return out
}

// deepOreNames are the ore families that carry Zydrine and Megacyte, best
// Megacyte first (Arkonor, Bistot, Spodumain; Crokite for Zydrine).
var deepOreNames = []string{"Arkonor", "Bistot", "Spodumain", "Crokite"}

// noteDeepMineralWants puts the corporation's want of deep minerals on the
// mineral ledger the prospectors read (minerals.go), as the CEO's.
func (m *Mod) noteDeepMineralWants(ctx context.Context, c *corpCEO, stationID int64, want map[int64]int64) {
	loc, err := evedb.GetStationLocation(ctx, m.db, stationID)
	if err != nil {
		return
	}
	now := clock.Unix()
	for typeID, n := range want {
		if !isDeepMineral(typeID) || n <= 0 {
			continue
		}
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_mineral_demand
				(typeID, stationID, systemID, regionID, typeName, wanted, lastCharID, lastAt)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE wanted = GREATEST(wanted, VALUES(wanted)), lastCharID = VALUES(lastCharID),
				lastAt = VALUES(lastAt)`,
			typeID, stationID, loc.SolarSystemID, loc.RegionID, m.typeNameOr(ctx, typeID), n, c.CharID, now)
	}
}

// sellInToCorpBids is a member's sell-in on its visit home: at its corporation's
// factory station, with no project of its own in hand, it sells what its hangar
// holds of the corporation's resting bids into them, when the bid valued up by
// its obedience meets what the thing is worth.
func (m *Mod) sellInToCorpBids(ctx context.Context, b *botRun) {
	if b == nil || b.CorpID == 0 || b.State == nil || b.State.Data.IndTypeID != 0 {
		return
	}
	if _, ok := managedCEO(b.CorpID); !ok {
		return
	}
	// Any office of the corporation's, not only the factory: members live at the
	// office their job is posted to (corpposting.go).
	if b.StationID == 0 || m.officeAt(ctx, b.CorpID, b.StationID) == 0 {
		return
	}
	// Its own corporation's bids for anything; any managed corporation's bid for a
	// deep metal (corpdeepcarry.go), since the roster's Zydrine belongs in some shipyard.
	args := append([]any{b.StationID, b.CorpID}, int64Args(deepPricedTypes)...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.corporationID, r.typeID, o.price, o.volRemaining FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.bid = 1 AND r.stationID = ? AND o.orderState = 1 AND o.volRemaining > 0
		   AND (r.corporationID = ? OR r.typeID IN (`+placeholders(len(deepPricedTypes))+`))`,
		args...)
	if err != nil {
		return
	}
	type want struct {
		corpID, typeID, rem int64
		price               float64
	}
	var list []want
	for rows.Next() {
		var w want
		if rows.Scan(&w.corpID, &w.typeID, &w.price, &w.rem) == nil {
			if _, managed := managedCEO(w.corpID); w.corpID == b.CorpID || managed {
				list = append(list, w)
			}
		}
	}
	rows.Close()
	ownNudge := 1 + corpLoyalNudge*math.Max(0, b.Tune.Obedience)
	for _, w := range list {
		nudge := 1.0 // loyalty is to its own corporation only
		if w.corpID == b.CorpID {
			nudge = ownNudge
		}
		if w.price*nudge < evedb.TypeValueOf(ctx, m.db, w.typeID).Value {
			continue
		}
		var itemID, qty int64
		if m.db.QueryRowContext(ctx, `
			SELECT itemID, quantity FROM entity
			 WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0
			 ORDER BY quantity DESC LIMIT 1`, b.CharID, b.StationID, w.typeID).Scan(&itemID, &qty) != nil || qty <= 0 {
			continue
		}
		took, value, err := m.dumpToBestBid(ctx, b, w.typeID, itemID, m.typeNameOr(ctx, w.typeID), min(qty, w.rem),
			bidDump{Note: "sold into its corporation's bid", Tag: "minerals", PlayerOnly: true})
		if err != nil || took <= 0 {
			continue
		}
		m.logBot(b.CharID, logInfo, "corp", "sold %d× %s into a corporation's bid for %s",
			took, m.typeNameOr(ctx, w.typeID), iskString(value))
	}
}
