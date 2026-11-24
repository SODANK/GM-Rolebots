package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"

	"gurimoon/esp"

	evedb "gurimoon/db"
)

// The order desk's board half: a board order (corpcommands_board.go) becomes the row a bot CEO would
// have posted, the order follows the row to its end, and withdrawing the order closes the row. Every
// poster is the one the bots use, so whoever carries it out cannot tell who gave it but by its note.

// chairAllowed is why a command may no longer be carried out, "" when it may: its poster has left, lost
// the rank or lost the division since it was filed, or the CEO has since ordered otherwise. An error
// is the database's, and says nothing about the order.
func (m *Mod) chairAllowed(ctx context.Context, c corpCommand) (string, error) {
	s, err := m.commandScopeErr(ctx, c.CorpID, c.PosterID)
	if err != nil {
		return "", err
	}
	if c.Arm == "" && !s.CEO {
		return "only the CEO gives that order", nil
	}
	if !s.Commands(corpArm(c.Arm)) {
		return m.charNameOr(ctx, c.PosterID) + " no longer commands " + armLabel(corpArm(c.Arm)), nil
	}
	if why := m.overruled(ctx, s, []corpCommand{c}); why != "" {
		return strings.TrimSuffix(strings.ToLower(why[:1])+why[1:], "."), nil
	}
	return "", nil
}

// refuseCommand closes an order the desk will not carry out and tells its poster.
func (m *Mod) refuseCommand(ctx context.Context, c corpCommand, why string) {
	_ = m.setCommandStatus(ctx, c.CommandID, cmdRefused, why)
	m.mailCommandOutcome(ctx, c, "Order refused: "+c.Title(m, ctx), "It could not be carried out: "+esp.IGBText(why)+".")
}

// takeBoardCommand carries out one board order. Reports whether the kind was one of these.
func (m *Mod) takeBoardCommand(ctx context.Context, c corpCommand) bool {
	who := m.charNameOr(ctx, c.PosterID)
	done := func(refKind string, refID int64, result string) {
		_ = m.claimCommand(ctx, c.CommandID, c.PosterID, refKind, refID, result)
		_ = m.setCommandStatus(ctx, c.CommandID, cmdDone, result)
	}
	if h, ok := c.head(); ok {
		m.takeHeadCommand(ctx, c, h, done)
		return true
	}
	if s, ok := c.standing(); ok {
		expires := clock.Unix() + int64(s.Hours)*3600
		if err := m.postFleetOrder(ctx, corpFleetOrder{CorpID: c.CorpID, Dept: s.Dept, SystemID: s.SystemID, OreNames: splitOres(s.Ores),
			Reason: "ordered by " + who, PostedBy: c.PosterID, Source: fleetOrderHuman, ExpiresAt: expires}); err != nil {
			m.commandRetry(ctx, c, "the order could not be posted")
			return true
		}
		_ = m.claimCommand(ctx, c.CommandID, c.PosterID, "order", c.CorpID, "stands until "+igbWhen(expires))
		m.humanLog(ctx, c.CorpID, c.PosterID, "standing", s.SystemID, 0, "%s gave a standing order: %s", who, c.Summary(m, ctx))
		return true
	}
	p, ok := c.board()
	if !ok {
		return false
	}
	ceo := m.deskCEOFor(ctx, c.CorpID, corpArm(c.Arm))
	if ceo == nil && c.Kind != cmdKindInvite {
		m.commandRetry(ctx, c, "no active bot of the corporation could act on it")
		return true
	}
	ctx = m.withChairStaff(ctx, ceo)
	if ceo != nil {
		// The money leaves in the name of whoever ordered it.
		ctx = withStaffActor(ctx, staffActor{CharID: ceo.CharID, Name: m.charNameOr(ctx, ceo.CharID), Office: "for " + who})
	}
	afford := func(amount float64) bool {
		if why := m.afford(ctx, c.CorpID, amount); why != "" {
			m.commandRetry(ctx, c, why)
			return false
		}
		return true
	}
	note := m.postedLine(ctx, c)
	now := clock.Unix()
	live := func(refKind string, refID int64, result string) {
		_ = m.claimCommand(ctx, c.CommandID, c.PosterID, refKind, refID, result)
		m.humanLog(ctx, c.CorpID, c.PosterID, c.Kind, p.TypeID, p.Reward+p.Wage, "%s ordered: %s — %s", who, c.Title(m, ctx), c.Summary(m, ctx))
	}

	switch c.Kind {
	case cmdKindReq:
		if !afford(m.reqBudget(ctx, p.TypeID, p.Qty)) {
			return true
		}
		by := reqChair
		if p.BuyOnly {
			by = reqChairBuy
		}
		id, err := m.postRequisition(ctx, ceo, requisition{TypeID: p.TypeID, Qty: p.Qty, DestStationID: p.StationID, DestFlag: p.Flag,
			Requester: by, RefKind: refKindCommand, RefID: c.CommandID, Deadline: now + int64(max(p.Days, 1))*86400,
			RequesterArm: c.Arm, Note: note, PastEnvelope: c.PosterCEO || ceo.Origin == ceoOriginHuman})
		if id == 0 {
			why := "the requisition could not be opened"
			if err == nil {
				why = "the division's weekly envelope does not cover it"
			}
			m.commandRetry(ctx, c, why)
			return true
		}
		live("req", id, "on the board")

	case cmdKindHaul:
		if m.officeUnits(ctx, c.CorpID, p.FromStationID, p.FromFlag, p.TypeID) < p.Qty {
			m.refuseCommand(ctx, c, "the goods are no longer there")
			return true
		}
		if !afford(p.Wage) {
			return true
		}
		id, err := m.postHaul(ctx, ceo, haulLeg{Kind: haulKindMove, FromKind: haulFromOffice, FromID: m.officeAt(ctx, c.CorpID, p.FromStationID),
			FromStationID: p.FromStationID, FromFlag: p.FromFlag, ToKind: haulToOffice, ToStationID: p.StationID, ToFlag: p.Flag,
			Items: []haulItem{{TypeID: p.TypeID, Qty: p.Qty, Flag: p.FromFlag}}, Wage: p.Wage, PostedBy: haulByChair,
			Deadline: now + int64(max(p.Days, 1))*86400, Note: note})
		if id == 0 {
			rlogf("rolebots: chair haul: %v", err)
			m.commandRetry(ctx, c, "the haul could not be posted")
			return true
		}
		live("haul", id, "on the board")

	case cmdKindSurvey:
		res, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_survey (corporationID, systemID, wagePerMoon, postedAt) VALUES (?, ?, ?, ?)`,
			c.CorpID, p.SystemID, surveyWageBase, now)
		if err != nil {
			m.commandRetry(ctx, c, "the survey could not be posted")
			return true
		}
		id, _ := res.LastInsertId()
		markPosWork(c.CorpID)
		live("survey", id, "waiting for a surveyor")

	case cmdKindAssign:
		if !assignmentsOn() {
			m.refuseCommand(ctx, c, "the Assignments board is switched off")
			return true
		}
		if !afford(p.Reward) {
			return true
		}
		a := assignment{CorpID: c.CorpID, Kind: p.Assign, TypeID: p.TypeID, Qty: p.Qty, StationID: p.StationID, SystemID: p.SystemID,
			WorkID: p.WorkID, Reward: p.Reward, MeritPts: assignMerit(p.Reward), Status: asPosted, OfferedAt: now, ExpiresAt: now + assignBoardSec}
		if p.Assign == assignDeliver {
			a.Division = 1
		}
		a.OfferedBy, _ = m.letterFrom(ctx, ceo, assignTopic(p.Assign))
		id, err := m.insertAssignment(ctx, a)
		if id == 0 {
			rlogf("rolebots: chair assignment: %v", err)
			m.commandRetry(ctx, c, "the assignment could not be posted")
			return true
		}
		a.ID = id
		live("assign", id, "on the Assignments board")
		m.assignNotice(ctx, ceo, []assignment{a})

	case cmdKindBid:
		m.takeBidCommand(ctx, c, p, ceo, afford, live, done)

	case cmdKindTrade:
		if !afford(p.Price*float64(p.Qty) + p.Wage) {
			return true
		}
		jumps := 1
		if dist, err := m.corpJumpMap(ctx, m.stationSystemID(ctx, p.FromStationID)); err == nil {
			if d, ok := dist[m.stationSystemID(ctx, p.StationID)]; ok {
				jumps = max(d, 1)
			}
		}
		// postTradeReq lets the buyer pay a slack over the ask; the price
		// given is the most, so the slack comes off it.
		d := &tradeDeal{TypeID: p.TypeID, TypeName: m.typeNameOr(ctx, p.TypeID), Ask: p.Price / (1 + tradeDeskBudgetSlack), Units: p.Qty,
			BuyStationID: p.FromStationID, SellStationID: p.StationID}
		id, err := m.postTradeReq(ctx, ceo, d, p.Wage, jumps)
		if id == 0 {
			rlogf("rolebots: chair trade run: %v", err)
			m.commandRetry(ctx, c, "the run could not be posted")
			return true
		}
		live("req", id, "waiting for a hauler")
		if reqs, err := m.queryReqs(ctx, `WHERE reqID = ?`, id); err == nil && len(reqs) == 1 {
			m.tradeDeskAssign(ctx, ceo, &reqs[0])
		}

	case cmdKindPrintBuy:
		if m.charCorpID(ctx, p.CharID) != c.CorpID {
			m.refuseCommand(ctx, c, "the buyer has left the corporation")
			return true
		}
		if m.openPOs(ctx, c.CorpID) >= poMaxOpen {
			m.commandRetry(ctx, c, "another blueprint is being bought")
			return true
		}
		id, err := m.postPOWith(ctx, ceo, p.CharID, p.TypeID, p.StationID, p.Price/poAdvanceMarkup, p.CopyOK, false)
		if err != nil || id == 0 {
			why := "the purchase could not be opened"
			if err != nil {
				why = err.Error()
			}
			m.commandRetry(ctx, c, why)
			return true
		}
		live("po", id, m.charNameOr(ctx, p.CharID)+" is to buy it")

	case cmdKindMove:
		if why := m.printMoveRefusal(ctx, c.CorpID, p.ItemID); why != "" {
			m.refuseCommand(ctx, c, why)
			return true
		}
		kind := moveOriginal
		if p.IsCopy {
			kind = moveCopy
		}
		if !m.postPrintMove(ctx, ceo, ceoCharterOf(ctx, m, ceo), p.ItemID, p.TypeID, kind, p.FromStationID, p.StationID, note) {
			m.commandRetry(ctx, c, "the move could not be posted")
			return true
		}
		var id int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT moveID FROM rolebot_corp_print_moves WHERE itemID = ? AND status IN `+liveMoveStatuses+` ORDER BY moveID DESC LIMIT 1`, p.ItemID).Scan(&id)
		live("printmove", id, "waiting for a carrier")

	case cmdKindSale:
		if !m.markPrintSale(ctx, ceo, p.ItemID, p.TypeID, p.StationID) {
			m.refuseCommand(ctx, c, "that copy cannot be put up for sale")
			return true
		}
		live("printsale", p.ItemID, "waiting for a member at the office to list it")

	case cmdKindInvite:
		expires := now + headhuntInviteTTL
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_invites (corporationID, characterID, reason, invitedBy, invitedAt, expiresAt)
			VALUES (?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE reason = VALUES(reason), invitedBy = VALUES(invitedBy), invitedAt = VALUES(invitedAt), expiresAt = VALUES(expiresAt)`,
			c.CorpID, p.CharID, clampText(who+" asked for you", 255), c.PosterID, now, expires); err != nil {
			m.commandRetry(ctx, c, "the invitation could not be sent")
			return true
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_reviews SET nextReviewAt = LEAST(nextReviewAt, ?) WHERE characterID = ?`, now, p.CharID)
		live("invite", p.CharID, "the invitation stands until "+igbWhen(expires))

	case cmdKindFactory:
		at := facilitySite{SystemID: m.stationSystemID(ctx, p.StationID)}
		at.StationID, at.Name = p.StationID, m.stationLabel(ctx, p.StationID)
		if m.officeAt(ctx, c.CorpID, p.StationID) == 0 || !m.stationHasLine(ctx, p.StationID) {
			m.refuseCommand(ctx, c, "the corporation has no office with assembly lines there")
			return true
		}
		if err := m.pinFactory(ctx, ceo, m.corpFactoryStation(ctx, c.CorpID), at, "ordered by "+who); err != nil {
			m.commandRetry(ctx, c, "the factory could not be moved")
			return true
		}
		forgetCorpFactory(c.CorpID)
		_, _ = m.syncOfficePurposes(ctx, c.CorpID)
		done("factory", p.StationID, "the factory is at "+at.Name)
		m.humanLog(ctx, c.CorpID, c.PosterID, "factory", p.StationID, 0, "%s made %s the factory", who, at.Name)
	}
	return true
}

func (m *Mod) takeHeadCommand(ctx context.Context, c corpCommand, h cmdHeadPayload, done func(string, int64, string)) {
	who := m.charNameOr(ctx, c.PosterID)
	arm := corpArm(h.Arm)
	if h.CharID == 0 {
		cur, ok := m.corpHeads(ctx, c.CorpID)[arm]
		if !ok {
			done("head", 0, "nobody headed "+armLabel(arm))
			return
		}
		if err := m.dismissHead(ctx, cur); err != nil {
			m.commandRetry(ctx, c, "the head could not be stood down")
			return
		}
		done("head", cur.CharID, m.charNameOr(ctx, cur.CharID)+" stood down from "+armLabel(arm))
		m.humanLog(ctx, c.CorpID, c.PosterID, "head", cur.CharID, 0, "%s stood %s down from %s", who, m.charNameOr(ctx, cur.CharID), armLabel(arm))
		m.orderLetter(ctx, c, cur.CharID, "You no longer head "+armLabel(arm), "You have been stood down as head of "+esp.IGBText(armLabel(arm))+".<br>")
		return
	}
	if err := m.appointHead(ctx, corpHead{CorpID: c.CorpID, Arm: arm, CharID: h.CharID, DeputyID: h.DeputyID, SetBy: c.PosterID, Flags: h.Flags}); err != nil {
		m.refuseCommand(ctx, c, err.Error())
		return
	}
	name := m.charNameOr(ctx, h.CharID)
	done("head", h.CharID, name+" heads "+armLabel(arm))
	m.humanLog(ctx, c.CorpID, c.PosterID, "head", h.CharID, 0, "%s put %s in charge of %s", who, name, armLabel(arm))
	m.logBot(h.CharID, logInfo, "corp", "put in charge of %s by the CEO (%s)", armLabel(arm), who)
	m.orderLetter(ctx, c, h.CharID, "You head "+armLabel(arm), "You have been put in charge of <b>"+esp.IGBText(armLabel(arm))+
		"</b>. Its orders are yours to give on the "+igbLink(managePath, "Management page")+" of the Corporate Portal.<br>")
}

// takeBidCommand places the chair's buy order.
func (m *Mod) takeBidCommand(ctx context.Context, c corpCommand, p cmdBoardPayload, ceo *corpCEO, afford func(float64) bool,
	live func(string, int64, string), done func(string, int64, string)) {

	if m.officeAt(ctx, c.CorpID, p.StationID) == 0 {
		m.refuseCommand(ctx, c, "the corporation has no office there any more")
		return
	}
	if !afford(p.Price * float64(p.Qty)) {
		return
	}
	placer := m.corpOrderPlacer(ctx, c.CorpID, ceo.CharID)
	if placer == 0 {
		m.commandRetry(ctx, c, "no bot with Trader or Director has a market order slot free")
		return
	}
	out, err := evedb.PlaceBuyOrder(ctx, m.db, placer, p.StationID, p.TypeID, p.Price, p.Qty, -1, 1, corpBidDays, true)
	if err != nil {
		m.commandRetry(ctx, c, "the market did not take the order")
		return
	}
	if out.ErrorMsg != "" {
		m.commandRetry(ctx, c, "the market refused it: "+out.ErrorMsg)
		return
	}
	if filled := out.FilledQty; filled > 0 {
		m.corpBook(ctx, c.CorpID, lineBuyback, -p.Price*float64(filled), p.TypeID, fmt.Sprintf("%d× %s", filled, m.typeNameOr(ctx, p.TypeID)))
		m.noteCorpBuyCost(ctx, c.CorpID, p.TypeID, filled, p.Price*float64(filled))
	}
	if out.RestingOrderID == 0 {
		done("bid", 0, fmt.Sprintf("bought %d at once", out.FilledQty))
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_orders (orderID, corporationID, stationID, typeID, bid, price, volSeen, placedAt, placedBy, kind)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?)`,
		out.RestingOrderID, c.CorpID, p.StationID, p.TypeID, p.Price, p.Qty-out.FilledQty, clock.Unix(), placer, corpOrderHuman); err != nil {
		rlogf("rolebots: chair bid %d: %v", out.RestingOrderID, err)
	}
	live("bid", out.RestingOrderID, "on the market, placed by "+m.charNameOr(ctx, placer))
}

// ── following ───────────────────────────────────────────────────────────────

// followBoardCommand moves a board order along with its row. Reports whether the kind was one of these.
func (m *Mod) followBoardCommand(ctx context.Context, c corpCommand) bool {
	finish := func(status, result string) {
		_ = m.setCommandStatus(ctx, c.CommandID, status, result)
		if status == cmdDone {
			m.mailCommandOutcome(ctx, c, "Done: "+c.Title(m, ctx), esp.IGBText(result)+".")
		} else {
			m.mailCommandOutcome(ctx, c, "Not done: "+c.Title(m, ctx), esp.IGBText(result)+".")
		}
	}
	note := func(result string) {
		if result != "" && (c.Status != cmdLive || c.Result != result) {
			_ = m.setCommandStatus(ctx, c.CommandID, cmdLive, result)
		}
	}
	if s, ok := c.standing(); ok {
		if clock.Unix() >= c.ClaimedAt+int64(s.Hours)*3600 {
			_ = m.setCommandStatus(ctx, c.CommandID, cmdDone, "the order ran its time")
			return true
		}
		if o := m.corpFleetOrderFor(ctx, c.CorpID, s.Dept); o == nil || o.PostedBy != c.PosterID || o.Source != fleetOrderHuman ||
			o.SystemID != s.SystemID || strings.Join(o.OreNames, ",") != strings.Join(splitOres(s.Ores), ",") {
			_ = m.setCommandStatus(ctx, c.CommandID, cmdDone, "another order replaced it")
			return true
		}
		note("the fleets have it")
		return true
	}
	p, ok := c.board()
	if !ok {
		return false
	}
	switch c.Kind {
	case cmdKindReq, cmdKindTrade:
		reqs, err := m.queryReqs(ctx, `WHERE reqID = ?`, c.RefID)
		if err != nil {
			return true
		}
		if len(reqs) == 0 {
			finish(cmdRefused, "the line left the board")
			return true
		}
		r := reqs[0]
		switch r.Status {
		case "done":
			finish(cmdDone, fmt.Sprintf("%d delivered", r.QtyDone))
		case "void":
			finish(cmdRefused, "the board withdrew it: "+r.Note)
		default:
			s := reqStatusWords(r.Status)
			if r.BuyerID != 0 {
				s = m.charNameOr(ctx, r.BuyerID) + " is on it"
			}
			note(s)
		}
	case cmdKindHaul:
		l, err := m.loadHaul(ctx, c.RefID)
		if err != nil {
			return true
		}
		switch {
		case l == nil:
			finish(cmdRefused, "the leg left the board")
		case l.Status == "done":
			finish(cmdDone, "delivered")
		case l.Status == "open":
		case l.Status == "claimed":
			note(m.charNameOr(ctx, l.CarrierID) + " is carrying it")
		case l.Status == haulStatusContract:
			note("out to a courier contract")
		default:
			finish(cmdRefused, "the leg closed: "+l.Note)
		}
	case cmdKindSurvey:
		var status string
		var by int64
		err := m.db.QueryRowContext(ctx, `SELECT status, surveyorID FROM rolebot_corp_survey WHERE surveyID = ?`, c.RefID).Scan(&status, &by)
		switch {
		case err == sql.ErrNoRows:
			finish(cmdRefused, "the survey left the board")
		case err != nil:
		case status == "done":
			finish(cmdDone, "the survey is in")
		case status == "void":
			finish(cmdRefused, "nobody could survey it in time")
		case status == "claimed":
			note(m.charNameOr(ctx, by) + " is surveying")
		}
	case cmdKindAssign:
		a, err := m.getAssignment(ctx, c.RefID)
		switch {
		case err != nil:
		case a == nil:
			finish(cmdRefused, "the assignment left the board")
		case a.Status == asDone:
			finish(cmdDone, m.charNameOr(ctx, a.CharID)+" did it")
		case a.Status == asAccepted:
			note(m.charNameOr(ctx, a.CharID) + " has taken it")
		case a.Status == asPosted:
		default:
			finish(cmdRefused, "nobody did it in time")
		}
	case cmdKindBid:
		var rem, state int64
		err := m.db.QueryRowContext(ctx, `SELECT volRemaining, orderState FROM market_orders WHERE orderID = ?`, c.RefID).Scan(&rem, &state)
		switch {
		case err != nil && err != sql.ErrNoRows:
		case err == sql.ErrNoRows:
			finish(cmdDone, "the buy order has come off the market")
		case state != 1 && rem > 0:
			finish(cmdRefused, fmt.Sprintf("it came off the market with %d of %d bought", max(p.Qty-rem, 0), p.Qty))
		case rem <= 0:
			finish(cmdDone, fmt.Sprintf("bought all %d", p.Qty))
		default:
			note(fmt.Sprintf("%d of %d bought", max(p.Qty-rem, 0), p.Qty))
		}
	case cmdKindPrintBuy:
		var status string
		err := m.db.QueryRowContext(ctx, `SELECT status FROM rolebot_corp_po WHERE poID = ?`, c.RefID).Scan(&status)
		switch {
		case err == sql.ErrNoRows:
			finish(cmdRefused, "the purchase left the books")
		case err != nil || status == "open":
		case status == "done":
			finish(cmdDone, "it is in the library")
		default:
			finish(cmdRefused, "the purchase was called off")
		}
	case cmdKindMove:
		mv, err := m.loadPrintMove(ctx, c.RefID)
		switch {
		case err != nil:
		case mv == nil:
			finish(cmdRefused, "the move left the board")
		case mv.Status == moveDone:
			finish(cmdDone, "carried by "+m.charNameOr(ctx, mv.CarrierID))
		case mv.Status == moveOpen:
		case mv.Status == moveVoid:
			finish(cmdRefused, mv.Note)
		default:
			note(m.charNameOr(ctx, mv.CarrierID) + " is carrying it")
		}
	case cmdKindSale:
		var status string
		var price float64
		err := m.db.QueryRowContext(ctx, `SELECT status, price FROM rolebot_corp_print_sales WHERE itemID = ?`, c.RefID).Scan(&status, &price)
		switch {
		case err == sql.ErrNoRows:
			finish(cmdRefused, "the sale left the books")
		case err != nil || status == printSaleWanted:
		case status == printSaleListed:
			note("listed for " + iskString(price))
		case status == printSaleSold:
			finish(cmdDone, "sold for "+iskString(price))
		default:
			finish(cmdRefused, "it did not sell ("+status+")")
		}
	case cmdKindInvite:
		switch {
		case m.charCorpID(ctx, p.CharID) == c.CorpID:
			finish(cmdDone, m.charNameOr(ctx, p.CharID)+" has joined")
		case clock.Unix() >= c.ClaimedAt+headhuntInviteTTL:
			finish(cmdRefused, m.charNameOr(ctx, p.CharID)+" did not come")
		}
	}
	return true
}

// ── undoing ─────────────────────────────────────────────────────────────────

// undoBoardCommand closes the row an order became. Answers why it cannot, "" when it is closed.
// handled is false for a kind that is not a board order.
func (m *Mod) undoBoardCommand(ctx context.Context, c corpCommand) (why string, handled bool) {
	p, ok := c.board()
	if !ok {
		return "", false
	}
	switch c.Kind {
	case cmdKindReq:
		m.voidReqsFor(ctx, c.CorpID, refKindCommand, c.CommandID, "withdrawn")
	case cmdKindTrade:
		reqs, err := m.queryReqs(ctx, `WHERE reqID = ? AND status IN ('open','buying')`, c.RefID)
		if err != nil {
			return "The board could not be read. Try again shortly.", true
		}
		for i := range reqs {
			if reqs[i].Advance > 0 && reqs[i].BuyerID != 0 {
				m.reclaimAdvance(ctx, &reqs[i])
			}
			m.closeReq(ctx, &reqs[i], "void", "withdrawn")
		}
	case cmdKindHaul:
		l, err := m.loadHaul(ctx, c.RefID)
		if err != nil {
			return "The board could not be read. Try again shortly.", true
		}
		if l != nil && l.carryingAny() {
			return m.charNameOr(ctx, l.CarrierID) + " has the goods aboard. Let them land.", true
		}
		if l != nil && l.Status == haulStatusContract {
			return "It is out to a courier contract.", true
		}
		if l != nil && (l.Status == "open" || l.Status == "claimed") {
			m.closeHaul(ctx, l, "void", "withdrawn")
		}
	case cmdKindSurvey:
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_survey SET status = 'void', closedAt = ? WHERE surveyID = ? AND status IN ('open','claimed')`,
			clock.Unix(), c.RefID)
	case cmdKindAssign:
		a, err := m.getAssignment(ctx, c.RefID)
		if err != nil {
			return "The board could not be read. Try again shortly.", true
		}
		if a != nil && a.Status == asAccepted {
			return m.charNameOr(ctx, a.CharID) + " has taken it. Let them finish.", true
		}
		if a != nil && a.Status == asPosted {
			res, err := m.db.ExecContext(ctx, `
				UPDATE rolebot_corp_assignments SET status = ?, closedAt = ? WHERE assignmentID = ? AND status = ?`,
				asExpired, clock.Unix(), a.ID, asPosted)
			if err != nil {
				return "The board could not be written. Try again shortly.", true
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return "Somebody has just taken it.", true
			}
			m.closeCommit(ctx, "assign", a.ID)
		}
	case cmdKindBid:
		var owner, state, rem int64
		if err := m.db.QueryRowContext(ctx, `SELECT charID, orderState, volRemaining FROM market_orders WHERE orderID = ?`, c.RefID).
			Scan(&owner, &state, &rem); err == nil && state == 1 && rem > 0 {
			if _, err := evedb.CancelMarketOrder(ctx, m.db, owner, c.RefID); err != nil {
				return "The market did not cancel the order. Try again shortly.", true
			}
		}
		m.forgetCorpOrder(ctx, c.RefID)
	case cmdKindPrintBuy:
		var po corpPO
		err := m.db.QueryRowContext(ctx, `
			SELECT poID, corporationID, characterID, bpTypeID, stationID, advance, spent, status, openedAt
			  FROM rolebot_corp_po WHERE poID = ? AND status = 'open'`, c.RefID).
			Scan(&po.POID, &po.CorpID, &po.CharID, &po.BPTypeID, &po.StationID, &po.Advance, &po.Spent, &po.Status, &po.OpenedAt)
		if err == nil && po.Advance > 0 {
			return m.charNameOr(ctx, po.CharID) + " has the money and is on the way for it.", true
		}
		if err == nil {
			m.voidPO(ctx, &po, "withdrawn")
		}
	case cmdKindMove:
		mv, err := m.loadPrintMove(ctx, c.RefID)
		if err != nil {
			return "The board could not be read. Try again shortly.", true
		}
		if mv != nil && mv.Status != moveOpen && mv.Status != moveDone && mv.Status != moveVoid {
			return m.charNameOr(ctx, mv.CarrierID) + " is already carrying it.", true
		}
		if mv != nil && mv.Status == moveOpen {
			m.closePrintMove(ctx, mv, moveVoid, "withdrawn")
		}
	case cmdKindSale:
		res, err := m.db.ExecContext(ctx, `UPDATE rolebot_corp_print_sales SET status = ? WHERE itemID = ? AND status = ?`,
			printSaleWithdrawn, c.RefID, printSaleWanted)
		if err != nil {
			return "The books could not be written. Try again shortly.", true
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return "It is already listed. It comes back if it does not sell.", true
		}
	case cmdKindInvite:
		_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_invites WHERE corporationID = ? AND characterID = ?`, c.CorpID, p.CharID)
	}
	return "", true
}

// ── what the board orders need to know ──────────────────────────────────────

// chairSurveys is the surveys a person ordered, as an IN list: the tower desk does not call them off.
func (m *Mod) chairSurveys(ctx context.Context, corpID int64) string {
	ids, _ := int64Column(ctx, m, `
		SELECT refID FROM rolebot_corp_commands WHERE corporationID = ? AND kind = ? AND status IN ('claimed', 'live')`,
		corpID, cmdKindSurvey)
	return int64CSV(append(ids, 0))
}

// reqBudget is what a requisition may spend (postRequisition's own sum).
func (m *Mod) reqBudget(ctx context.Context, typeID, qty int64) float64 {
	unit := evedb.TypeValueOf(ctx, m.db, typeID).Value
	if unit <= 0 {
		unit = 1
	}
	return math.Ceil(float64(qty) * unit * reqBudgetMarkup)
}

// officeUnits is how many of a type lie in one division of an office.
func (m *Mod) officeUnits(ctx context.Context, corpID, stationID, flag, typeID int64) int64 {
	office := m.officeAt(ctx, corpID, stationID)
	if office == 0 {
		return 0
	}
	n, _ := evedb.GetMaterialQuantityAt(ctx, m.db, corpID, office, flag, typeID)
	return n
}

// haulWageBetween is the board's wage for a load between two offices (postHaul's own sum).
func (m *Mod) haulWageBetween(ctx context.Context, typeID, qty, from, to int64) float64 {
	jumps := 1
	if u, err := m.universe(ctx); err == nil && u != nil {
		if j, ok := bfsJumps(u, m.stationSystemID(ctx, from), 40)[m.stationSystemID(ctx, to)]; ok {
			jumps = max(j, 1)
		}
	}
	return haulWageFor(m.typeVolume(ctx, typeID)*float64(qty), jumps)
}

// printMoveRefusal is why a print may not be moved, "" when it may. An order on it would be left
// where it cannot be done, and a locked original is the members' vote to unlock.
func (m *Mod) printMoveRefusal(ctx context.Context, corpID, itemID int64) string {
	if why := m.printBusy(ctx, corpID, itemID); why != "" {
		return why
	}
	if m.lockedByVote(ctx, corpID, itemID) {
		return "That print is locked down. Unlock it in the corporation window first."
	}
	return ""
}
