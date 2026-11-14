package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

// The Trade desk: how a trade corporation makes its money. Trading is a solo career,
// so a corporation adds capital rather than pilots. The Director of Commerce (or the
// CEO) scans the market with the solo trader's search against the corporation's free
// capital, within the draft radius of the headquarters or an office (tradeDeskBuyReach),
// and posts a run that clears as a requisition line (posreq.go). A member with a hauler
// flies the funded buy trip and is paid the haul board's rate on delivery. At the far
// end it sells into bids on its own account and hands the proceeds to the corporation
// (sellTradeRunHere), or files the goods into the Trade division at an office, where the
// desk lists them. Scope: corporations tagged trade, retail or hauling (tradeDeskTagged).

const (
	corpOrderTrade = "trade" // rolebot_corp_orders.kind for the desk's listings
	reqTrade       = "trade" // rolebot_corp_requisition.requester for a trade run

	// tradeDeskEverySec is how often one corporation's desk scans.
	tradeDeskEverySec = 1800
	// tradeDeskMaxOpen caps the runs a corporation has in flight at once.
	tradeDeskMaxOpen = 2
	// tradeDeskMinFreeISK is the free capital under which the desk does not scan at all;
	// a smaller run is not worth a member's time.
	tradeDeskMinFreeISK = 2_000_000
	// tradeDeskCapitalFrac is the share of the production budget
	// (Charter.CapitalRisk × free capital) one run may spend.
	tradeDeskCapitalFrac = 0.5
	// tradeDeskMaxRunISK caps one run's outlay whatever the wallet holds.
	tradeDeskMaxRunISK = 50_000_000
	// tradeDeskMinProfitISK is what a run must clear AFTER the wage and the
	// fees to be posted.
	tradeDeskMinProfitISK = 250_000
	// tradeDeskBudgetSlack is the margin the budget and the pinned ask carry
	// over the scan's price, for the book moving between scan and buy.
	tradeDeskBudgetSlack = 0.05
	// tradeDeskDeadlineSec is a line's deadline. It is under reqPatientSec, and
	// reqPatient also refuses the line by requester.
	tradeDeskDeadlineSec = 36 * 3600
	// tradeDeskStaleSec voids a line nobody has delivered.
	tradeDeskStaleSec = 4 * 86400
	// tradeDeskScanMembers bounds the roster read for the largest hold.
	tradeDeskScanMembers = 60
	// tradeDeskHaulMaxJumps is how far from the seller a member may be drafted from, and
	// so how far from the headquarters or an office the desk buys (tradeDeskBuyReach).
	// One radius for both ends keeps the scan from finding runs the draft cannot staff.
	tradeDeskHaulMaxJumps = 15
	// tradeDeskUnclaimedSec is how long a posted run waits for a member to
	// take it up. Past this with nobody assigned, the desk drops the idea
	// rather than holding a run slot and its capital for days.
	tradeDeskUnclaimedSec = 3600
	// tradeDeskDropCooldownSec keeps a dropped lane from being posted again
	// straight away: the same best deal would otherwise come back on the
	// very next scan and sit unclaimed all over again.
	tradeDeskDropCooldownSec = 6 * 3600
	// tradeDeskListDays is how long a listing rests.
	tradeDeskListDays = 30
)

// tradeDeskTagged is whether a corporation's tags give it a Trade desk:
// the commerce outfits, and only them. Pure.
func tradeDeskTagged(tags []string) bool {
	for _, t := range tags {
		switch t {
		case "trade", "retail", "hauling":
			return true
		}
	}
	return false
}

// tradeDeskClears is whether a run is worth posting: its gross margin, less
// the wage the corporation promises the hauler and the fees the listing
// costs, has to leave the minimum. Pure.
func tradeDeskClears(profit, wage, fees, minProfit float64) bool {
	return profit-wage-fees >= minProfit
}

// tradeListPrice is what the desk asks for a lot that cost landed per unit. A standing
// bid at or over the landed cost plus the margin is taken on the spot; otherwise the
// lot rests at that floor, or just under the best ask when that is higher. Pure.
func tradeListPrice(bestBid, bestAsk, landed, minMargin float64) float64 {
	// To the cent before the comparison: 100 × 1.1 is 110.00000000000001 in
	// binary, and a bid of exactly 110 must take it.
	floor := math.Round(landed*(1+minMargin)*100) / 100
	if bestBid > 0 && bestBid >= floor {
		return bestBid
	}
	price := floor
	if bestAsk > 0 {
		if under := math.Floor(bestAsk*0.99*100) / 100; under > price {
			price = under
		}
	}
	return math.Round(price*100) / 100
}

// tradeDesk runs the desk for one corporation: sweep its lines, list what
// arrived, and post a new run when there is room for one.
func (m *Mod) tradeDesk(ctx context.Context, c *corpCEO, ch Charter) error {
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil || facts == nil || !tradeDeskTagged(facts.Policy.Tags) {
		return nil
	}
	if !deskDue(c.CorpID, "tradedesk", tradeDeskEverySec) {
		return nil
	}
	open, err := m.tradeDeskSweep(ctx, c)
	if err != nil {
		return err
	}
	if err := m.ceoListTradeStock(ctx, c); err != nil {
		return err
	}
	if open >= tradeDeskMaxOpen {
		return nil
	}
	return m.tradeDeskScan(ctx, c, ch)
}

// tradeDeskSweep keeps the desk's own lines honest and reports how many are
// still in flight. The Procurer's desk (posreq.go) skips trade lines, so
// nothing else closes a delivered one, voids a stale one or takes a line
// back from a buyer that left.
func (m *Mod) tradeDeskSweep(ctx context.Context, c *corpCEO) (int, error) {
	return m.tradeLinesSweep(ctx, c, false)
}

// tradeLinesSweep is tradeDeskSweep over one kind of trade line: the desk's
// own runs, or (goo) the starbase's moon-goods sale runs (posgoosale.go),
// which ride the same rails but are staffed by the Sales arm alone.
func (m *Mod) tradeLinesSweep(ctx context.Context, c *corpCEO, goo bool) (int, error) {
	kind := `refKind <> ?`
	if goo {
		kind = `refKind = ?`
	}
	reqs, err := m.queryReqs(ctx,
		`WHERE corporationID = ? AND requester = ? AND `+kind+` AND status IN ('open','buying') ORDER BY reqID`,
		c.CorpID, reqTrade, refGooSale)
	if err != nil {
		return 0, err
	}
	now := clock.Unix()
	open := 0
	for i := range reqs {
		r := &reqs[i]
		if r.left() <= 0 {
			m.closeReq(ctx, r, "done", "delivered")
			continue
		}
		if now-r.OpenedAt > tradeDeskStaleSec {
			m.reclaimAdvance(ctx, r)
			m.closeReq(ctx, r, "void", "nobody delivered it")
			m.ceoLog(ctx, c, powerBusiness, "tradevoid", r.TypeID, 0,
				"wrote off the trade run for %d× %s — nobody delivered it in %s",
				r.left(), m.typeNameOr(ctx, r.TypeID), fmtDuration(tradeDeskStaleSec))
			continue
		}
		if r.Status == "buying" {
			var corpNow int64
			_ = m.db.QueryRowContext(ctx, `SELECT corporationID FROM character_ WHERE characterID = ?`, r.BuyerID).Scan(&corpNow)
			if corpNow != c.CorpID || !m.isRostered(ctx, r.BuyerID) {
				m.reclaimAdvance(ctx, r)
				_, _ = m.db.ExecContext(ctx,
					`UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0 WHERE reqID = ?`, r.ReqID)
				r.Status, r.BuyerID = "open", 0
			}
		}
		assign := m.tradeDeskAssign
		if goo {
			assign = m.gooSaleAssign
		}
		if r.Status == "open" && !assign(ctx, c, r) && tradeDeskUnclaimed(r.OpenedAt, now) {
			m.closeReq(ctx, r, "void", "nobody took it up")
			markTradeLaneDropped(c.CorpID, r.TypeID, r.SrcStationID, r.DestStationID, now)
			m.ceoLog(ctx, c, powerBusiness, "tradevoid", r.TypeID, 0,
				"dropped the trade run for %d× %s — no member took it up in %s",
				r.left(), m.typeNameOr(ctx, r.TypeID), fmtDuration(tradeDeskUnclaimedSec))
			continue
		}
		open++
	}
	if open > 0 {
		// The errand gate (postask.go) reads this mark; a restart clears it.
		markPosWork(c.CorpID)
	}
	return open, nil
}

// tradeDeskUnclaimed is whether a run still waiting for a member has waited
// long enough to be dropped. Pure.
func tradeDeskUnclaimed(openedAt, now int64) bool {
	return now-openedAt > tradeDeskUnclaimedSec
}

// tradeDeskBuyReach is every system within maxJumps of any of from (the headquarters
// and the offices): where the desk may buy. Pure over the universe.
func tradeDeskBuyReach(u *universe, from []int64, maxJumps int) map[int64]bool {
	out := map[int64]bool{}
	for _, s := range from {
		if s == 0 || out[s] {
			continue
		}
		for sys := range bfsJumps(u, s, maxJumps) {
			out[sys] = true
		}
	}
	return out
}

// tradeLaneDropped remembers lanes the desk posted and dropped unclaimed, keyed
// corp|type|buy station|sell station to when. It is in memory only: a restart costs
// one early repost, never a missed run.
var tradeLaneDropped sync.Map

func tradeLaneKey(corpID, typeID, buyStation, sellStation int64) string {
	return fmt.Sprintf("%d|%d|%d|%d", corpID, typeID, buyStation, sellStation)
}

func markTradeLaneDropped(corpID, typeID, buyStation, sellStation, now int64) {
	tradeLaneDropped.Store(tradeLaneKey(corpID, typeID, buyStation, sellStation), now)
}

// tradeLaneCooling reports whether a lane was dropped within the cooldown,
// and forgets every entry past it so the map stays small.
func tradeLaneCooling(corpID, typeID, buyStation, sellStation, now int64) bool {
	// One lookup. This walked the whole map per call, from inside the supply
	// run's work × source loop; the lapsed entries go on the cache sweep.
	key := tradeLaneKey(corpID, typeID, buyStation, sellStation)
	v, ok := tradeLaneDropped.Load(key)
	if !ok {
		return false
	}
	if now-v.(int64) >= tradeDeskDropCooldownSec {
		tradeLaneDropped.Delete(key)
		return false
	}
	return true
}

func init() {
	registerSweep("tradeLaneDroppedSweep", func() {
		now := clock.Unix()
		tradeLaneDropped.Range(func(k, v any) bool {
			if now-v.(int64) >= tradeDeskDropCooldownSec {
				tradeLaneDropped.Delete(k)
			}
			return true
		})
	})
}

// tradeDeskOffices is every station the corporation can list at (headquarters and
// offices), keyed by station, with the headquarters' station and system to scan from.
func (m *Mod) tradeDeskOffices(ctx context.Context, corpID int64) (offices map[int64]bool, hqStation, hqSystem int64) {
	addrs, err := m.corpAnchorStations(ctx, corpID)
	if err != nil {
		return nil, 0, 0
	}
	offices = map[int64]bool{}
	for stationID, a := range addrs {
		if m.officeAt(ctx, corpID, stationID) == 0 {
			continue
		}
		offices[stationID] = true
		if a.HQ || hqStation == 0 {
			hqStation, hqSystem = stationID, a.SystemID
		}
	}
	return offices, hqStation, hqSystem
}

// tradeDeskHold is the largest free hold on a trade hull any enabled member owns
// (what one run is sized to), and how many members hold one.
func (m *Mod) tradeDeskHold(ctx context.Context, corpID int64) (hold float64, carriers int) {
	ids, err := int64Column(ctx, m, `
		SELECT b.characterID FROM rolebot_characters b
		  JOIN character_ c ON c.characterID = b.characterID
		 WHERE b.enabled = 1 AND c.corporationID = ?
		 ORDER BY b.characterID LIMIT ?`, corpID, tradeDeskScanMembers)
	if err != nil {
		return 0, 0
	}
	for _, id := range ids {
		ship, _, err := m.shipForActivity(ctx, id, ActTrade)
		if err != nil || ship == nil || !ship.ShipExists {
			continue
		}
		free, err := m.holdFreeM3(ctx, id, ship.ShipItemID)
		if err != nil || free <= 0 {
			continue
		}
		carriers++
		if free > hold {
			hold = free
		}
	}
	return hold, carriers
}

// tradeDeskScan looks for one run and posts it.
func (m *Mod) tradeDeskScan(ctx context.Context, c *corpCEO, ch Charter) error {
	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return err
	}
	budget := math.Min(free*ch.CapitalRisk*tradeDeskCapitalFrac, tradeDeskMaxRunISK)
	if free < tradeDeskMinFreeISK || budget <= 0 {
		return nil
	}
	offices, hqStation, hqSystem := m.tradeDeskOffices(ctx, c.CorpID)
	if len(offices) == 0 || hqSystem == 0 {
		return nil
	}
	hold, carriers := m.tradeDeskHold(ctx, c.CorpID)
	if carriers == 0 {
		return nil
	}
	// The desk prices as its head would: the Director of Commerce when that is a pilot
	// the module flies, else the CEO. The scan starts at the headquarters.
	scanner := m.commerceHead(ctx, c.CorpID)
	if scanner == 0 || !m.isRostered(ctx, scanner) {
		scanner = c.CharID
	}
	b, err := m.loadRun(ctx, scanner)
	if err != nil {
		return nil // no bot to price with
	}
	scan := *b
	scan.SystemID, scan.StationID = hqSystem, hqStation
	// Buy only where a member would fly from: within the draft radius of the headquarters
	// or an office. The ladder below widens only over sellers inside this ring.
	u, err := m.universe(ctx)
	if err != nil {
		return err
	}
	homes := []int64{hqSystem}
	for stationID := range offices {
		homes = append(homes, m.stationSystemID(ctx, stationID))
	}
	// The search pays the hauler's wage before ranking (tradeConfig.HaulCostPerM3Jump)
	// and floors at the desk's minimum (MinProfitISK), so its best deal is one tradeDeskClears accepts.
	cfg := tradeConfig{TradeWithHated: true, NoSpec: true,
		BuySystems:        tradeDeskBuyReach(u, homes, tradeDeskHaulMaxJumps),
		HaulCostPerM3Jump: haulISKPerM3Jump, MinProfitISK: tradeDeskMinProfitISK}
	askMargin, floorMargin := b.Tune.TradeMinMarginFrac, b.Tune.TradeFloorMarginFrac
	if floorMargin > askMargin {
		floorMargin = askMargin
	}
	r := m.economicRules(ctx, &scan, startBand(scanner, bandAdjacent))
	var deal *tradeDeal
	var searchErr error
	for {
		deal, searchErr = m.tradeSearch(ctx, &scan, cfg, r, budget, hold, askMargin, floorMargin)
		if deal != nil {
			break
		}
		wider, ok := m.widen(ctx, &scan, r)
		if !ok {
			break
		}
		r = wider
	}
	// A supply run to one of the corporation's factory offices (corpsupplyrun.go) is
	// weighed against the best sure run on expected value and taken only when it beats it.
	if sup := m.tradeDeskSupplyRun(ctx, c, &scan, cfg, r, offices, u, budget, hold); supplyRunBeats(sup, deal) {
		return m.postSupplyRun(ctx, c, sup)
	}
	if deal == nil {
		if searchErr != nil {
			m.logBot(c.CharID, logInfo, "ceo", "the trade desk found nothing to run — %v", searchErr)
		}
		return nil
	}
	if m.reqPending(ctx, c.CorpID, deal.TypeID, deal.SellStationID, corpDivTrade) > 0 {
		return nil // the same lane is already in flight
	}
	if tradeLaneCooling(c.CorpID, deal.TypeID, deal.BuyStationID, deal.SellStationID, clock.Unix()) {
		return nil // posted lately and nobody took it up
	}
	jumps, ok := bfsJumps(u, deal.BuySystemID, 40)[deal.SellSystemID]
	if !ok {
		return nil
	}
	wage := haulWageFor(float64(deal.Units)*deal.UnitVol, jumps)
	gross := deal.Bid * float64(deal.Units)
	rates := m.marketRates(ctx, c.CharID, deal.SellStationID)
	// Sales tax only: the hauler sells into a standing bid, which is an
	// immediate order and pays no broker fee.
	fees := gross * rates.Tax
	if !tradeDeskClears(deal.Profit, wage, fees, tradeDeskMinProfitISK) {
		m.logBot(c.CharID, logInfo, "ceo",
			"the trade desk passed on %d× %s (%s → %s): %s margin does not clear a %s wage and %s in fees",
			deal.Units, deal.TypeName, m.stationLabel(ctx, deal.BuyStationID), m.stationLabel(ctx, deal.SellStationID),
			iskString(deal.Profit), iskString(wage), iskString(fees))
		return nil
	}
	id, err := m.postTradeReq(ctx, c, deal, wage, jumps)
	if err != nil || id == 0 {
		return err
	}
	if reqs, err := m.queryReqs(ctx, `WHERE reqID = ?`, id); err == nil && len(reqs) == 1 {
		m.tradeDeskAssign(ctx, c, &reqs[0])
	}
	return nil
}

// postTradeReq writes a run as a requisition line and commits its budget.
// A direct insert rather than postRequisition: that prices the budget off the
// type's rough value and logs under the starbase power, and this line has a
// real ask, a real seller and a wage.
func (m *Mod) postTradeReq(ctx context.Context, c *corpCEO, d *tradeDeal, wage float64, jumps int) (int64, error) {
	outlay := d.Ask * float64(d.Units)
	budget := math.Ceil(outlay*(1+tradeDeskBudgetSlack)) + wage
	maxPrice := math.Round(d.Ask*(1+tradeDeskBudgetSlack)*100) / 100
	note := truncate(fmt.Sprintf("buy at %s, sell at %s (%d jumps)",
		m.stationLabel(ctx, d.BuyStationID), m.stationLabel(ctx, d.SellStationID), jumps), 255)
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_requisition
			(corporationID, typeID, qty, destStationID, destFlag, requester, refKind, refID, deadline, budget,
			 openedAt, note, srcStationID, maxPrice, wage)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CorpID, d.TypeID, d.Units, d.SellStationID, corpDivTrade, reqTrade, reqTrade, d.BuyStationID,
		clock.Unix()+tradeDeskDeadlineSec, budget, clock.Unix(), note, d.BuyStationID, maxPrice, wage)
	if err != nil {
		return 0, fmt.Errorf("postTradeReq: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := m.openCommit(ctx, c.CorpID, "req", id, budget); err != nil {
		return id, err
	}
	m.ceoLog(ctx, c, powerBusiness, "traderun", d.TypeID, budget,
		"posted a trade run: %d× %s, buy %s at %s, sell %s at %s — %s expected after a %s wage",
		d.Units, d.TypeName, iskString(d.Ask), m.stationLabel(ctx, d.BuyStationID),
		iskString(d.Bid), m.stationLabel(ctx, d.SellStationID), iskString(d.Profit-wage), iskString(wage))
	markPosWork(c.CorpID)
	return id, nil
}

// tradeDeskAssign hands an open line to the nearest free member with a
// hauler: the Sales arm's members when the corporation has dealt any
// (corparm.go), else anybody on the roster. Reports whether one was found.
func (m *Mod) tradeDeskAssign(ctx context.Context, c *corpCEO, r *requisition) bool {
	if r.Status != "open" || r.SrcStationID == 0 {
		return false
	}
	// The run was sized to the biggest hold in the corporation (tradeDeskHold), so it
	// goes first to a hauler whose hold takes all of it.
	runM3 := float64(r.left()) * m.typeVolume(ctx, r.TypeID)
	pick, jumps, arm := m.pickFreeHaulerFor(ctx, c.CorpID, r.SrcStationID, tradeDeskHaulMaxJumps, 0, runM3, false)
	if pick == 0 && arm {
		pick, jumps, _ = m.pickFreeHaulerFor(ctx, c.CorpID, r.SrcStationID, tradeDeskHaulMaxJumps, 0, runM3, true)
	}
	if pick == 0 {
		// Nobody free holds the whole run: a part load still beats none.
		pick, jumps = m.pickFreeHauler(ctx, c.CorpID, r.SrcStationID, tradeDeskHaulMaxJumps, 0)
	}
	if pick == 0 {
		return false
	}
	m.assignReqBuyer(ctx, r, pick)
	if r.Status == "buying" {
		m.ceoLog(ctx, c, powerBusiness, "tradeassign", pick, r.Wage,
			"sent %s for the %d× %s run (%d jumps from the seller, %s wage)",
			m.charNameOr(ctx, pick), r.left(), m.typeNameOr(ctx, r.TypeID), jumps, iskString(r.Wage))
		return true
	}
	return false
}

// pickFreeHauler is the corporation's nearest free member with a hauler to a station,
// within maxJumps: the Sales arm's members when it has dealt any (corparm.go), else
// anybody on the roster; docked, interruptible, online and holding no 'buying' line.
// exclude is passed over. Returns 0 when nobody qualifies.
func (m *Mod) pickFreeHauler(ctx context.Context, corpID, nearStation int64, maxJumps int, exclude int64) (int64, int) {
	id, jumps, _ := m.pickFreeHaulerFor(ctx, corpID, nearStation, maxJumps, exclude, 0, false)
	return id, jumps
}

// pickFreeHaulerFor is pickFreeHauler for cargo of a size: only a member whose trade
// hull holds needM3 (one unit) qualifies, and allArms widens the pool from the Sales
// arm to every member. arm reports whether the corporation has a Sales arm at all.
func (m *Mod) pickFreeHaulerFor(ctx context.Context, corpID, nearStation int64, maxJumps int, exclude int64,
	needM3 float64, allArms bool) (int64, int, bool) {
	pick := haulerPick{Arm: armSales}
	if allArms {
		pick.Arm = ""
	}
	r := m.pickHauler(ctx, corpID, nearStation, maxJumps, exclude, needM3, pick)
	return r.CharID, r.Jumps, r.ArmStaffed
}

// haulerPick is which members a hauler search reads and how it ranks them.
type haulerPick struct {
	// Arm limits the pool to one arm's members; "" is the whole roster.
	Arm corpArm
	// ByHold ranks the biggest fitted hold first (then jumps) instead of the nearest:
	// the order for a load only big hulls can lift.
	ByHold bool
	// CargoHull turns away a member whose trade hull is not a cargo hull (reqHaulGroups): the
	// pool for bulk tower fuel, which a frigate's hold moves a few percent of a trip.
	CargoHull bool
}

// haulerPicked is a hauler search's answer.
type haulerPicked struct {
	CharID int64
	Jumps  int
	// ArmStaffed: the arm asked for has members at all (always false for
	// the whole roster).
	ArmStaffed bool
	// HoldShort: free members were in reach and every one was turned away by its hold;
	// the load is too big for this pool, not unattended.
	HoldShort bool
}

// pickHauler is the search behind pickFreeHaulerFor.
func (m *Mod) pickHauler(ctx context.Context, corpID, nearStation int64, maxJumps int, exclude int64,
	needM3 float64, pick haulerPick) haulerPicked {
	var out haulerPicked
	var pool []int64
	if pick.Arm != "" {
		ids, err := int64Column(ctx, m, `
			SELECT characterID FROM rolebot_corp_arm_members WHERE corporationID = ? AND arm = ?`,
			corpID, string(pick.Arm))
		if err != nil || len(ids) == 0 {
			if pick.Arm != armSales {
				return out // an unstaffed arm is nobody, not everybody
			}
		} else {
			pool = ids
			out.ArmStaffed = true
		}
	}
	where := `WHERE b.enabled = 1 AND c.corporationID = ? AND c.stationID <> 0`
	args := []any{corpID}
	if len(pool) > 0 {
		where += ` AND b.characterID IN (` + int64CSV(pool) + `)`
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT b.characterID, c.solarSystemID, COALESCE(s.phase, 'idle'), COALESCE(s.data, ''), COALESCE(s.halted, 0)
		  FROM rolebot_characters b
		  JOIN character_ c ON c.characterID = b.characterID
		  LEFT JOIN rolebot_state s ON s.characterID = b.characterID
		 `+where+`
		   AND NOT EXISTS (SELECT 1 FROM rolebot_corp_requisition q
		                    WHERE q.buyerID = b.characterID AND q.status = 'buying')
		 ORDER BY b.characterID LIMIT 200`, args...)
	if err != nil {
		return out
	}
	type cand struct {
		charID, systemID int64
	}
	var cands []cand
	for rows.Next() {
		var ca cand
		var phase, blob string
		var halted int
		if rows.Scan(&ca.charID, &ca.systemID, &phase, &blob, &halted) != nil || halted != 0 || ca.charID == exclude {
			continue
		}
		st := &SimState{Phase: phase}
		if blob != "" {
			_ = json.Unmarshal([]byte(blob), &st.Data)
		}
		// Logged off, benched or winding down to log off: it would only sit on the work until
		// the desk takes it back (reqBuyerAway).
		if !interruptibleState(st) || reqBuyerAway(ca.charID) != "" {
			continue
		}
		cands = append(cands, ca)
	}
	rows.Close()
	if len(cands) == 0 {
		return out
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return out
	}
	reach := bfsJumps(u, m.stationSystemID(ctx, nearStation), maxJumps)
	type ranked struct {
		charID int64
		jumps  int
		hold   float64
	}
	var picks []ranked
	short := false
	for _, ca := range cands {
		j, ok := reach[ca.systemID]
		if !ok {
			continue
		}
		ship, _, err := m.shipForActivity(ctx, ca.charID, ActTrade)
		if err != nil || ship == nil || !ship.ShipExists {
			continue
		}
		if pick.CargoHull && !m.shipItemIsCargoHull(ctx, ship.ShipItemID) {
			short = true
			continue
		}
		hold := 0.0
		if needM3 > 0 || pick.ByHold {
			hold = m.shipItemHold(ctx, ship.ShipItemID)
		}
		if needM3 > 0 && !reqHoldFits(needM3, hold) {
			short = true
			continue
		}
		picks = append(picks, ranked{ca.charID, j, hold})
	}
	if len(picks) == 0 {
		out.HoldShort = short
		return out
	}
	sort.Slice(picks, func(i, j int) bool {
		if pick.ByHold && picks[i].hold != picks[j].hold {
			return picks[i].hold > picks[j].hold
		}
		if picks[i].jumps != picks[j].jumps {
			return picks[i].jumps < picks[j].jumps
		}
		return picks[i].charID < picks[j].charID
	})
	out.CharID, out.Jumps = picks[0].charID, picks[0].jumps
	return out
}

// ── selling at the far end ──────────────────────────────────────────────────

// tradeRunUnitCost is what one unit of a run cost the corporation landed:
// the buy and the whole wage over every unit bought. 0 for no units. Pure.
func tradeRunUnitCost(spent, wage float64, units int64) float64 {
	if units <= 0 {
		return 0
	}
	return (spent + wage) / float64(units)
}

// sellTradeRunHere is the far end of a Trade desk run at a station with no corporation
// office. The hauler sells into standing bids on its own account at no less than landed
// cost, hands the corporation the proceeds net of sales tax (booked as sales, not in the
// trade book the levy reads), and carries the rest to the nearest office. Returns that office.
func (m *Mod) sellTradeRunHere(ctx context.Context, b *botRun, r *requisition) (int64, error) {
	carried := m.cargoUnitsOwnedBy(ctx, b.ShipID, b.CorpID, r.TypeID)
	qty := min(carried, r.left())
	if qty <= 0 {
		return 0, nil
	}
	floor := math.Ceil(tradeRunUnitCost(r.Spent, r.Wage, r.QtyDone+carried)*100) / 100
	goo := r.RefKind == refGooSale
	label := "trade desk run"
	if goo {
		// A moon-goods run cost nothing to buy: its floor is the price at
		// which the bid still beats the listing at home (gooSaleFloor).
		floor, label = r.MaxPrice, "moon goods run"
	}
	moved, err := m.moveUnits(ctx, b.CorpID, b.ShipID, 5, r.TypeID, qty, b.StationID, 4, b.CharID)
	if err != nil || moved <= 0 {
		return 0, err
	}
	var itemID int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT itemID FROM entity
		 WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0
		 ORDER BY quantity DESC, itemID LIMIT 1`, b.CharID, b.StationID, r.TypeID).Scan(&itemID); err != nil {
		_, _ = m.moveUnits(ctx, b.CharID, b.StationID, 4, r.TypeID, moved, b.ShipID, 5, b.CorpID)
		return 0, fmt.Errorf("trade run stack: %w", err)
	}
	var filled int64
	gross := 0.0
	if floor > 0 {
		out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID, r.TypeID, itemID, floor, moved,
			-1 /*station*/, 1, 0 /*immediate*/, false)
		if err != nil {
			_, _ = m.moveUnits(ctx, b.CharID, b.StationID, 4, r.TypeID, moved, b.ShipID, 5, b.CorpID)
			return 0, fmt.Errorf("trade run sale: %w", err)
		}
		if out.ErrorMsg == "" {
			filled = out.FilledQty
			for _, f := range out.Fills {
				gross += f.Price * float64(f.Quantity)
			}
		}
	}
	if filled > 0 {
		net := math.Floor(m.netSaleAt(ctx, b.CharID, b.StationID, gross))
		if bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID); err == nil && net > math.Floor(bal) {
			net = math.Floor(bal)
		}
		if net >= 1 {
			if _, _, err := evedb.TransferISK(ctx, m.db, b.CharID, r.CorpID, net, refPlayerDonation,
				"Trade desk sale proceeds"); err != nil {
				m.logBot(b.CharID, logError, "corp", "could not hand the corporation the %s its trade run sold for: %v",
					iskString(net), err)
				net = 0
			}
		}
		if net > 0 {
			m.corpBook(ctx, r.CorpID, lineSales, net, r.TypeID,
				fmt.Sprintf("%d× %s sold at %s (%s)", filled, m.typeNameOr(ctx, r.TypeID), m.stationLabel(ctx, b.StationID), label))
			m.creditSalesSeat(ctx, r.CorpID, b.CharID, net)
		}
		r.QtyDone += filled
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET qtyDone = qtyDone + ? WHERE reqID = ?`, filled, r.ReqID)
		m.payTradeRunWage(ctx, r, b.CharID, filled)
		if c, _ := getCEO(ctx, m.db, r.CorpID); c != nil && goo {
			m.ceoLog(ctx, c, powerBusiness, "goosale", r.TypeID, net,
				"%s sold %d× %s from its towers into the bids at %s — %s to the corporation (floor %s each)",
				m.charNameOr(ctx, b.CharID), filled, m.typeNameOr(ctx, r.TypeID), m.stationLabel(ctx, b.StationID),
				iskString(net), iskString(floor))
		} else if c != nil {
			m.ceoLog(ctx, c, powerBusiness, "tradesale", r.TypeID, net,
				"%s sold %d× %s into the bids at %s — %s to the corporation (landed %s each)",
				m.charNameOr(ctx, b.CharID), filled, m.typeNameOr(ctx, r.TypeID), m.stationLabel(ctx, b.StationID),
				iskString(net), iskString(floor))
		}
	}
	rest := moved - filled
	if rest <= 0 {
		return 0, nil
	}
	to := m.nearestTradeOffice(ctx, b.CorpID, b.SystemID)
	if goo && m.officeAt(ctx, b.CorpID, r.SrcStationID) != 0 {
		to = r.SrcStationID // back to the staging office, where the towers' goods are listed
	}
	if to == 0 {
		// No office anywhere to file it: the corporation lost every office mid-run. The
		// hauler keeps the remainder and the line counts it delivered.
		r.QtyDone += rest
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET qtyDone = qtyDone + ? WHERE reqID = ?`, rest, r.ReqID)
		m.logBot(b.CharID, logInfo, "corp", "the bids at %s took %d of %d %s and the corporation has no office to file the rest — keeping it",
			m.stationLabel(ctx, b.StationID), filled, moved, m.typeNameOr(ctx, r.TypeID))
		// On the hauler's stock ledger, so its sweeps and a trader can still find it (deadstock_pass.go).
		m.addStock(ctx, b.CharID, b.StationID, b.SystemID, r.TypeID, m.typeNameOr(ctx, r.TypeID), rest, 0)
		return 0, nil
	}
	if _, err := m.moveUnits(ctx, b.CharID, b.StationID, 4, r.TypeID, rest, b.ShipID, 5, b.CorpID); err != nil {
		return 0, err
	}
	r.DestStationID = to
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET destStationID = ?, note = ? WHERE reqID = ?`,
		to, truncate("remainder to the office at "+m.stationLabel(ctx, to), 255), r.ReqID)
	m.logBot(b.CharID, logInfo, "corp", "the bids at %s took %d of %d %s — carrying the other %d to the corporation's office at %s",
		m.stationLabel(ctx, b.StationID), filled, moved, m.typeNameOr(ctx, r.TypeID), rest, m.stationLabel(ctx, to))
	return to, nil
}

// nearestTradeOffice is the corporation's office station fewest jumps from
// a system, 0 when it has none.
func (m *Mod) nearestTradeOffice(ctx context.Context, corpID, fromSystem int64) int64 {
	stations, err := int64Column(ctx, m, `SELECT stationID FROM crpoffices WHERE corporationID = ? ORDER BY stationID`, corpID)
	if err != nil || len(stations) == 0 {
		return 0
	}
	u, err := m.universe(ctx)
	if err != nil {
		return stations[0]
	}
	reach := bfsJumps(u, fromSystem, 80)
	best, bestJumps := int64(0), math.MaxInt
	for _, s := range stations {
		if j, ok := reach[m.stationSystemID(ctx, s)]; ok && j < bestJumps {
			best, bestJumps = s, j
		}
	}
	if best == 0 {
		best = stations[0]
	}
	return best
}

// ── listing ─────────────────────────────────────────────────────────────────

// tradeLandedCost is what the corporation paid per unit, wage included, for
// the latest run that delivered this type to this station; 0 when none did.
func (m *Mod) tradeLandedCost(ctx context.Context, corpID, stationID, typeID int64) float64 {
	var spent, wage sql.NullFloat64
	var done sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `
		SELECT spent, wagePaid, qtyDone FROM rolebot_corp_requisition
		 WHERE corporationID = ? AND destStationID = ? AND typeID = ? AND requester = ? AND refKind <> ? AND qtyDone > 0
		 ORDER BY closedAt DESC, reqID DESC LIMIT 1`, corpID, stationID, typeID, reqTrade, refGooSale).
		Scan(&spent, &wage, &done)
	if !done.Valid || done.Int64 <= 0 {
		return 0
	}
	return (spent.Float64 + wage.Float64) / float64(done.Int64)
}

// ceoListTradeStock lists what the desk's runs delivered: the biggest stack
// of each type in the Trade division of each office, one order per
// (station, type), on the corporation's account.
func (m *Mod) ceoListTradeStock(ctx context.Context, c *corpCEO) error {
	offices, err := m.db.QueryContext(ctx,
		`SELECT stationID, itemID FROM crpoffices WHERE corporationID = ?`, c.CorpID)
	if err != nil {
		return fmt.Errorf("trade offices: %w", err)
	}
	type office struct{ stationID, itemID int64 }
	var offs []office
	for offices.Next() {
		var o office
		if offices.Scan(&o.stationID, &o.itemID) == nil {
			offs = append(offs, o)
		}
	}
	offices.Close()
	minMargin := defaultTuning().TradeFloorMarginFrac
	askMargin := defaultTuning().TradeMinMarginFrac
	if b, err := m.loadRun(ctx, c.CharID); err == nil {
		minMargin, askMargin = b.Tune.TradeFloorMarginFrac, b.Tune.TradeMinMarginFrac
	}
	askMargin = math.Max(askMargin, minMargin)
	listed := 0
	for _, o := range offs {
		if _, err := evedb.StackAllItems(ctx, m.db, c.CorpID, o.itemID, corpDivTrade); err != nil {
			rlogf("rolebots: stacking corp %d trade shelf %d: %v", c.CorpID, o.itemID, err)
		}
		rows, err := m.db.QueryContext(ctx, `
			SELECT e.itemID, e.typeID, e.quantity FROM entity e
			 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = ? AND e.quantity > 0 AND e.singleton = 0
			 ORDER BY e.typeID, e.quantity DESC, e.itemID`, c.CorpID, o.itemID, corpDivTrade)
		if err != nil {
			return err
		}
		type lot struct{ itemID, typeID, qty int64 }
		var lots []lot
		seen := map[int64]bool{}
		for rows.Next() {
			var l lot
			if rows.Scan(&l.itemID, &l.typeID, &l.qty) == nil && !seen[l.typeID] {
				seen[l.typeID] = true
				lots = append(lots, l)
			}
		}
		rows.Close()
		for _, l := range lots {
			if listed >= corpListPerThink {
				return nil
			}
			var open int
			_ = m.db.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM rolebot_corp_orders
				 WHERE corporationID = ? AND stationID = ? AND typeID = ? AND bid = 0`,
				c.CorpID, o.stationID, l.typeID).Scan(&open)
			if open > 0 {
				continue
			}
			placer := m.corpOrderPlacer(ctx, c.CorpID, c.CharID)
			if placer == 0 {
				addCorpOrderPressure(c.CorpID, len(lots)-listed)
				return nil
			}
			var bid, ask sql.NullFloat64
			_ = m.db.QueryRowContext(ctx, `
				SELECT MAX(price) FROM market_orders
				 WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0`,
				o.stationID, l.typeID).Scan(&bid)
			_ = m.db.QueryRowContext(ctx, `
				SELECT MIN(price) FROM market_orders
				 WHERE stationID = ? AND typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0 AND isCorp = 0`,
				o.stationID, l.typeID).Scan(&ask)
			landed := m.tradeLandedCost(ctx, c.CorpID, o.stationID, l.typeID)
			if landed <= 0 {
				landed = evedb.TypeValueOf(ctx, m.db, l.typeID).Value
			}
			price := tradeListPrice(bid.Float64, ask.Float64, landed, minMargin)
			// At a factory that wants it (a supply run's shelf), priced off
			// what its industrials would pay otherwise (factorydemand.go).
			if alt := m.factoryAltAt(ctx, o.stationID, l.typeID); alt > 0 {
				price = factoryListPrice(bid.Float64, ask.Float64, landed, alt, askMargin, minMargin)
			}
			if price <= 0 {
				continue
			}
			out, err := evedb.PlaceSellOrder(ctx, m.db, placer, o.stationID, l.typeID, l.itemID, price, l.qty,
				-1 /*station*/, 1, tradeDeskListDays, true)
			if err != nil {
				return fmt.Errorf("list trade %s: %w", m.typeNameOr(ctx, l.typeID), err)
			}
			if out.ErrorMsg != "" {
				m.logBot(c.CharID, logInfo, "ceo", "the market refused the trade desk's %s listing: %s",
					m.typeNameOr(ctx, l.typeID), out.ErrorMsg)
				continue
			}
			listed++
			if out.FilledQty > 0 {
				v := 0.0
				for _, f := range out.Fills {
					v += f.Price * float64(f.Quantity)
				}
				m.corpBook(ctx, c.CorpID, lineSales, v, l.typeID,
					fmt.Sprintf("%d× %s (trade desk)", out.FilledQty, m.typeNameOr(ctx, l.typeID)))
				m.creditSalesSeat(ctx, c.CorpID, placer, v)
			}
			if out.RestingOrderID != 0 {
				if _, err := m.db.ExecContext(ctx, `
					INSERT INTO rolebot_corp_orders
						(orderID, corporationID, stationID, typeID, bid, price, volSeen, placedAt, kind, placedBy)
					VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, ?)`,
					out.RestingOrderID, c.CorpID, o.stationID, l.typeID, price, l.qty-out.FilledQty,
					clock.Unix(), corpOrderTrade, placer); err != nil {
					return err
				}
			}
			m.ceoLog(ctx, c, powerBusiness, "tradelist", l.typeID, price*float64(l.qty),
				"listed the trade desk's %d× %s at %s for %s each (landed %s)%s",
				l.qty, m.typeNameOr(ctx, l.typeID), m.stationLabel(ctx, o.stationID), iskString(price), iskString(landed),
				map[bool]string{true: fmt.Sprintf(" (%d sold on the spot)", out.FilledQty)}[out.FilledQty > 0])
		}
	}
	return nil
}
