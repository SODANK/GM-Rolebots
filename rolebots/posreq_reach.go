package rolebots

import "context"

// Where a corporation's buyer looks for supplies. A buy trip shops the way any resident shops
// (shopLadder): its own region, then its neighbours, capped two regions out. A requisition is
// bought for an office, though, and when the office sits far from where the buyer lives the
// goods are near the office and nowhere near the buyer: shopNearDest is the second search.
// The buyer's own personality still applies. A xenophobe's station filter rides the second
// search too, so a pilot that will not dock at a Gallente station will not buy a charter there.
// That buyer declines the line and is snubbed for it (declineNoSeller), and the line goes to
// somebody who will; without the snub it would come straight back on every pass.

// shopNearDest is the buyer's second look for a requisition line's seller: stations in the
// region of the office the goods are bound for and the regions adjacent to it, ranked by the
// jumps the buyer would fly from where it is now. nil when there is no office, when residency
// is off (the ladder already searched the whole map), or when nothing there will sell to it.
func (m *Mod) shopNearDest(ctx context.Context, b *botRun, typeID, destStationID int64) (*marketStop, error) {
	if destStationID == 0 || !Residency() || !HomeLoop() {
		return nil, nil
	}
	region := m.stationRegionOf(ctx, destStationID)
	if region == 0 {
		return nil, nil
	}
	scope := m.scopeRegions(ctx, region, bandAdjacent)
	if len(scope) == 0 {
		return nil, nil
	}
	move := rulesForBot(b)
	reach, err := m.reachFrom(ctx, b.SystemID, move)
	if err != nil {
		return nil, err
	}
	look := move
	look.Home, look.Regions, look.Band = region, scope, bandAdjacent
	for _, f := range stationFilterPasses(b.Profile, b.Tune) {
		scan, err := m.scanMarket(ctx, []int64{typeID}, true, f.scoped(look), reach, nil, botScope(b))
		if err != nil {
			return nil, err
		}
		if s := bestBuy(scan.Stops, b.Tune); s != nil {
			return s, nil
		}
	}
	return nil, nil
}

// declineNoSeller hands a line back that this buyer found no seller for and snubs the buyer
// for it, so the next hand-out goes to somebody else (pickReqBuyer). The usual cause is the
// buyer itself: where it lives, or which stations it will dock at. Any advance it holds comes
// back first.
func (m *Mod) declineNoSeller(ctx context.Context, buyer int64, r *requisition) {
	m.reqDecline(ctx, buyer, r, "no seller in reach")
	reqSnubs.Store(r.ReqID, buyer)
	m.reclaimAdvance(ctx, r)
	_, _ = m.db.ExecContext(ctx,
		`UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0 WHERE reqID = ? AND buyerID = ?`, r.ReqID, buyer)
	r.Status, r.BuyerID = "open", 0
}
