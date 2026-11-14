package rolebots

import (
	"context"
	"fmt"
	"sync"

	evedb "gurimoon/db"
)

// Fitted ships on escrow, as a source of hulls. A listed ship is a seller of the fit it could
// become, weighed as the blueprint hunt weighs an escrowed print (bpotrade.go):
//
//	selection   a candidate fit whose hull is listed within reach is priced at the listing plus
//	            what the crate lacks, when that beats the market. Price is a gate, not a merit.
//	trip start  startProcurement makes the same pick, pays by it and holds the listing.
//	findhull    flies to the listing and claims it (procure.escrowship); buyhull boards it.
//
// claimEscrowShip unfits the rack into the pilot's hangar, where the outfit stop reads its own
// stock first (ownstock.go), so only what the crate lacked is bought.

// escrowShip is one public escrow listing holding exactly one ship.
type escrowShip struct {
	EscrowID     int64
	IssuerID     int64
	StationID    int64
	SystemID     int64
	RegionID     int64
	OwnerFaction int64 // the station owner's NPC faction, 0 when none
	Price        float64
	HullTypeID   int64
	// Contents is everything else in the crate, typeID → units.
	Contents map[int64]int64
	// Value is the crate at market value (hull + contents), the bound the
	// listing's price is held to (escrow.shipBotPremium).
	Value float64
}

// escrowShipBoard is the shared read, indexed by hull. The two settings ride
// in it because server settings are an uncached query and the selector asks
// this once per candidate hull per pilot per pass.
type escrowShipBoard struct {
	byHull  map[int64][]escrowShip
	premium float64 // escrow.shipBotPremium
}

// escrowShipListings is every public, single-ship escrow listing, shared by
// the roster on the market snapshot's clock.
func (m *Mod) escrowShipListings(ctx context.Context) (escrowShipBoard, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshot(snapKey("escrow-ships"), func() (escrowShipBoard, error) {
		if !escrowBuyingEnabled(sctx, m.db) {
			return escrowShipBoard{}, nil // switched off: an empty board
		}
		// Two halves: what lies in the crate itself (the hull), and what is fitted to or carried by a
		// ship in the crate. The second reports category 0 so a nested item is never counted as the
		// listing's hull.
		const listed = `
			FROM escrows e
			LEFT JOIN staStations s ON s.stationID = e.pickupStationID
			LEFT JOIN crpNPCCorporations nc ON nc.corporationID = s.corporationID`
		const live = `
			WHERE e.state = 0 AND e.dateExpires > UNIX_TIMESTAMP()
			  AND e.assigneeID IS NULL AND e.crateContainerID > 0`
		rows, err := m.db.QueryContext(sctx, `
			SELECT e.escrowID, e.issuerID, e.pickupStationID, e.pickupSolarSystemID,
			       e.pickupRegionID, e.collateral, COALESCE(nc.factionID, 0),
			       i.typeID, i.quantity, g.categoryID`+listed+`
			JOIN entity i ON i.locationID = e.crateContainerID
			JOIN invTypes t ON t.typeID = i.typeID
			JOIN invGroups g ON g.groupID = t.groupID`+live+`
			UNION ALL
			SELECT e.escrowID, e.issuerID, e.pickupStationID, e.pickupSolarSystemID,
			       e.pickupRegionID, e.collateral, COALESCE(nc.factionID, 0),
			       i.typeID, i.quantity, 0`+listed+`
			JOIN entity h ON h.locationID = e.crateContainerID
			JOIN invTypes ht ON ht.typeID = h.typeID
			JOIN invGroups hg ON hg.groupID = ht.groupID AND hg.categoryID = ?
			JOIN entity i ON i.locationID = h.itemID`+live, categoryShip)
		if err != nil {
			return escrowShipBoard{}, fmt.Errorf("escrow ships: %w", err)
		}
		defer rows.Close()
		type acc struct {
			l     escrowShip
			ships int
		}
		byID := map[int64]*acc{}
		var order []int64
		for rows.Next() {
			var l escrowShip
			var typeID, qty, cat int64
			if err := rows.Scan(&l.EscrowID, &l.IssuerID, &l.StationID, &l.SystemID,
				&l.RegionID, &l.Price, &l.OwnerFaction, &typeID, &qty, &cat); err != nil {
				return escrowShipBoard{}, err
			}
			a, ok := byID[l.EscrowID]
			if !ok {
				l.Contents = map[int64]int64{}
				a = &acc{l: l}
				byID[l.EscrowID] = a
				order = append(order, l.EscrowID)
			}
			if qty < 1 {
				qty = 1
			}
			if cat == categoryShip {
				a.ships++
				a.l.HullTypeID = typeID
				continue
			}
			a.l.Contents[typeID] += qty
		}
		if err := rows.Err(); err != nil {
			return escrowShipBoard{}, err
		}
		// Value every type on the board in one read.
		typeSet := map[int64]bool{}
		var kept []*acc
		for _, id := range order {
			a := byID[id]
			if a.ships != 1 {
				continue // a crate of hulls is cargo, not a ship for sale
			}
			kept = append(kept, a)
			typeSet[a.l.HullTypeID] = true
			for t := range a.l.Contents {
				typeSet[t] = true
			}
		}
		types := make([]int64, 0, len(typeSet))
		for t := range typeSet {
			types = append(types, t)
		}
		prices, _ := evedb.TypeValues(sctx, m.db, types)
		board := escrowShipBoard{byHull: map[int64][]escrowShip{},
			premium: escrowSettingFloat(sctx, m.db, "escrow.shipBotPremium", 1.25)}
		for _, a := range kept {
			l := a.l
			if tv, ok := prices[l.HullTypeID]; ok {
				l.Value += tv.Value
			}
			for t, q := range l.Contents {
				if tv, ok := prices[t]; ok {
					l.Value += tv.Value * float64(q)
				}
			}
			board.byHull[l.HullTypeID] = append(board.byHull[l.HullTypeID], l)
		}
		return board, nil
	})
}

// ── what a crate covers ─────────────────────────────────────────────────────

// fitNeeds is everything a fit asks for except its hull, typeID → units.
func fitNeeds(f *Fit) map[int64]int64 {
	need := map[int64]int64{}
	if f == nil {
		return need
	}
	for _, mod := range f.Modules {
		need[mod.TypeID]++
		if mod.ChargeTypeID != 0 && mod.ChargeQty > 0 {
			need[mod.ChargeTypeID] += mod.ChargeQty
		}
	}
	for _, c := range f.Cargo {
		need[c.TypeID] += c.Qty
	}
	for _, d := range f.Drones {
		need[d.TypeID] += d.Qty
	}
	return need
}

// shopNeeds is fitNeeds over an outstanding shopping list: what a trip still
// has to find, hull excluded.
func shopNeeds(list []ShopItem) map[int64]int64 {
	need := map[int64]int64{}
	for _, it := range list {
		if it.Dest != "hull" {
			need[it.TypeID] += it.Qty
		}
	}
	return need
}

// escrowUncovered is what of `need` a crate holding `have` leaves to buy.
func escrowUncovered(need, have map[int64]int64) map[int64]int64 {
	out := map[int64]int64{}
	for t, n := range need {
		if left := n - have[t]; left > 0 {
			out[t] = left
		}
	}
	return out
}

// shipEscrowAcceptable is the buyer's bound on a listed ship: never more than its crate is worth
// at market times the premium (one trip instead of a shopping list). It stops a listing priced at
// ten times its contents from being bought just because the pilot is rich.
func shipEscrowAcceptable(price, value, premium float64) bool {
	return price > 0 && value > 0 && price <= value*premium
}

// ── the pick ────────────────────────────────────────────────────────────────

// escrowShipStop is a listing a pilot has weighed and would fly to.
type escrowShipStop struct {
	escrowShip
	Jumps int
	// Outlay is the whole bill for the fit bought this way: the listing plus
	// the uncovered lines at market (with the same slippage fitCost carries).
	Outlay float64
	// Covered counts the units of the fit the crate supplies, for the log.
	Covered int64
}

// escrowShipReach is the horizon a hull purchase searches: the same buy
// fence the market ladder ends at (shopClimb), from where the pilot stands.
func (m *Mod) escrowShipReach(ctx context.Context, b *botRun) map[int64]int {
	_, capBand := shopClimb(true, defaultBand(b))
	capBand = frontBuyCap(b.CharID, capBand)
	reach, err := m.reachFrom(ctx, b.SystemID, m.economicRules(ctx, b, capBand))
	if err != nil {
		return map[int64]int{}
	}
	return reach
}

// bestEscrowShip finds the listed ship of hullTypeID that fills `need` for
// the least, once the flying is priced in, among listings in `reach`. It
// answers only when that beats marketCost (0 = no market answer to beat).
// reach is a func so a pilot whose hulls nobody lists never pays for a BFS.
func (m *Mod) bestEscrowShip(ctx context.Context, b *botRun, hullTypeID int64,
	need map[int64]int64, marketCost float64, reach func() map[int64]int) *escrowShipStop {
	board, err := m.escrowShipListings(ctx)
	if err != nil {
		return nil
	}
	cands := board.byHull[hullTypeID]
	if len(cands) == 0 {
		return nil
	}
	refused := map[int64]bool{}
	if b.Profile != nil {
		_, xHard := xenoStationFactions(raceOf(b.CharID), b.Tune, b.Profile.LikedFactions())
		for _, f := range mergeIDs(b.Profile.DespisedFactions(), xHard) {
			refused[f] = true
		}
	}
	regions := m.valuationRegions(ctx, b)
	var rch map[int64]int
	var best *escrowShipStop
	bestEff := 0.0
	for _, l := range cands {
		if l.IssuerID == b.CharID || escrowShipHeldByOther(l.EscrowID, b.CharID) {
			continue
		}
		if refused[l.OwnerFaction] {
			continue // never docks there, so never buys there
		}
		if !shipEscrowAcceptable(l.Price, l.Value, board.premium) {
			continue
		}
		if rch == nil {
			rch = reach()
		}
		j, ok := rch[l.SystemID]
		if !ok {
			continue
		}
		rest := 0.0
		for t, q := range escrowUncovered(need, l.Contents) {
			p, _, perr := m.estimatePriceRefIn(ctx, regions, t)
			if perr != nil {
				rest = -1
				break
			}
			rest += p * float64(q)
		}
		if rest < 0 {
			continue
		}
		outlay := l.Price + rest*fitCostSlippage
		// The same per-jump slope a market stop pays (marketStop.buyCost), so a distant crate has to be
		// cheaper than the next till by the flying.
		eff := outlay * (1 + b.Tune.IskJumpSlope*float64(j))
		if marketCost > 0 && eff >= marketCost {
			continue
		}
		if best == nil || eff < bestEff {
			var covered int64
			for t, n := range need {
				covered += min(n, l.Contents[t])
			}
			best = &escrowShipStop{escrowShip: l, Jumps: j, Outlay: outlay, Covered: covered}
			bestEff = eff
		}
	}
	return best
}

// escrowShipNote is the clause a fit log line gets when its price came off
// an escrow listing.
func (m *Mod) escrowShipNote(ctx context.Context, es *escrowShipStop) string {
	if es == nil {
		return ""
	}
	return fmt.Sprintf(" — a listed ship at %s (%d jumps) for %.0f ISK carries %d unit(s) of it",
		m.stationLabel(ctx, es.StationID), es.Jumps, es.Price, es.Covered)
}

// ── holds ───────────────────────────────────────────────────────────────────
//
// A listing is one crate and the snapshot is shared, so without a hold every pilot wanting that hull flies for
// it and all but one arrive to nothing. The hold is process memory, not a table: a restart drops every trip it guards.

// escrowShipHoldSec is how long a trip may take to reach its crate.
const escrowShipHoldSec = 30 * 60

var (
	escrowShipHoldMu sync.Mutex
	escrowShipHolds  = map[int64]escrowHold{}
)

type escrowHold struct {
	charID int64
	until  int64
}

// holdEscrowShip reserves a listing for a pilot's trip.
func holdEscrowShip(escrowID, charID int64) {
	now := clock.Unix()
	escrowShipHoldMu.Lock()
	defer escrowShipHoldMu.Unlock()
	for id, h := range escrowShipHolds {
		if h.until <= now {
			delete(escrowShipHolds, id)
		}
	}
	escrowShipHolds[escrowID] = escrowHold{charID: charID, until: now + escrowShipHoldSec}
}

// escrowShipHeldByOther reports a live hold by somebody else.
func escrowShipHeldByOther(escrowID, charID int64) bool {
	escrowShipHoldMu.Lock()
	defer escrowShipHoldMu.Unlock()
	h, ok := escrowShipHolds[escrowID]
	return ok && h.charID != charID && h.until > clock.Unix()
}

// releaseEscrowShip drops a hold, claimed or given up.
func releaseEscrowShip(escrowID int64) {
	escrowShipHoldMu.Lock()
	delete(escrowShipHolds, escrowID)
	escrowShipHoldMu.Unlock()
}

// ── the claim ───────────────────────────────────────────────────────────────

// claimEscrowShip buys a listing the pilot is docked beside and returns the
// hull it delivered and what it cost. The wallet is asked by the same floor
// every till asks (spendable), not by the raw balance the core checks.
func (m *Mod) claimEscrowShip(ctx context.Context, b *botRun, escrowID, hullTypeID int64) (int64, float64, error) {
	defer releaseEscrowShip(escrowID)
	var state int
	var price float64
	if err := m.db.QueryRowContext(ctx,
		`SELECT state, collateral FROM escrows WHERE escrowID = ?`, escrowID).Scan(&state, &price); err != nil {
		return 0, 0, fmt.Errorf("listing %d: %w", escrowID, err)
	}
	if state != 0 {
		return 0, 0, fmt.Errorf("listing %d was taken before it arrived", escrowID)
	}
	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return 0, 0, fmt.Errorf("wallet: %w", err)
	}
	if spendable(bal) < price {
		return 0, 0, fmt.Errorf("listing %d asks %.0f ISK and the wallet may spend %.0f", escrowID, price, spendable(bal))
	}
	corpID, _ := evedb.GetCharCorpID(ctx, m.db, b.CharID)
	moved, err := evedb.ClaimEscrowMission(ctx, m.db, escrowID, b.CharID, corpID, b.StationID, true)
	if err != nil {
		return 0, 0, err
	}
	if len(moved) == 0 {
		return 0, price, fmt.Errorf("listing %d delivered nothing", escrowID)
	}
	var shipID int64
	args := append(int64Args(moved), hullTypeID, b.CharID)
	if err := m.db.QueryRowContext(ctx, `
		SELECT itemID FROM entity
		WHERE itemID IN (`+placeholders(len(moved))+`) AND typeID = ? AND ownerID = ?
		LIMIT 1`, args...).Scan(&shipID); err != nil {
		return 0, price, fmt.Errorf("listing %d held no %d hull: %w", escrowID, hullTypeID, err)
	}
	// The outfit stop builds this pilot's template from hangar stock (ownstock.go) onto an empty hull,
	// so the rack comes off into the hangar. A leftover module is handled by stripOffFitModules.
	rack, err := m.db.QueryContext(ctx,
		`SELECT itemID FROM entity WHERE locationID = ? AND ownerID = ?`, shipID, b.CharID)
	if err == nil {
		var ids []int64
		for rack.Next() {
			var id int64
			if rack.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rack.Close()
		for _, id := range ids {
			if err := evedb.UnfitModuleToHangar(ctx, m.db, id, b.StationID); err != nil {
				rlogf("rolebots: unfit claimed escrow item %d: %v", id, err)
			}
		}
	}
	return shipID, price, nil
}

// hullHereFor reports whether shipID is this pilot's hull of hullTypeID in the hangar it is
// docked at: the claimed hull is still where the claim put it.
func (m *Mod) hullHereFor(ctx context.Context, b *botRun, shipID, hullTypeID int64) bool {
	if b.StationID == 0 {
		return false
	}
	var one int
	return m.db.QueryRowContext(ctx, `
		SELECT 1 FROM entity
		WHERE itemID = ? AND ownerID = ? AND typeID = ? AND locationID = ?`,
		shipID, b.CharID, hullTypeID, b.StationID).Scan(&one) == nil
}

// shoppingMarketCost is what an outstanding list costs at the till, hull included, on the
// estimates and slippage fitCost uses: the number a listed ship has to beat once a trip is under
// way. 0 when any line cannot be priced (no comparison, not a bargain).
func (m *Mod) shoppingMarketCost(ctx context.Context, b *botRun, list []ShopItem) float64 {
	total := 0.0
	for _, it := range list {
		p, err := m.estimatePriceFor(ctx, b, it.TypeID)
		if err != nil {
			return 0
		}
		total += p * float64(it.Qty)
	}
	return total * fitCostSlippage
}
