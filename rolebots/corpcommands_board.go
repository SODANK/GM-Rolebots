package rolebots

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gurimoon/esp"
)

// The board orders: what a bot CEO and its department heads ask of the members, given by a human
// CEO or a division head on the Management page (corpmanage_board.go) and carried out by the order
// desk (corpcommanddesk_board.go) through the posters the bots use. An order lives as long as the
// board row it became: the row's status is its own.

const (
	cmdKindHead     = "head"      // who heads a division (corpheads.go)
	cmdKindStanding = "standing"  // a standing fleet order with no fleet raised for it
	cmdKindReq      = "req"       // a requisition: something to have (posreq.go)
	cmdKindHaul     = "haul"      // goods from one office to another (poshaul.go)
	cmdKindSurvey   = "survey"    // a moon survey (posintel.go)
	cmdKindAssign   = "assign"    // a job on the Assignments board (corpassign.go)
	cmdKindBid      = "bid"       // a corporation buy order (corpbuyback.go)
	cmdKindTrade    = "trade"     // a trade run (corptrade.go)
	cmdKindPrintBuy = "printbuy"  // a purchase order for a blueprint (corplibrary.go)
	cmdKindMove     = "printmove" // a print carried to another base (corpprintmove.go)
	cmdKindSale     = "printsale" // a copy put up for sale (corpprintsale.go)
	cmdKindInvite   = "invite"    // an invitation to a pilot (corpstaff_dept.go headhunt)
	cmdKindFactory  = "factory"   // where the factory is (corpfacility.go)

	// refKindCommand marks a requisition the chair opened, so it never merges with a desk's line
	// and no envelope refuses it. Its refID is the command.
	refKindCommand = "command"
	// reqChair and reqChairBuy are who asked: the chair, and the chair when it wants it bought
	// rather than built.
	reqChair    = "chair"
	reqChairBuy = "chairbuy"
	// corpOrderHuman marks a buy order the chair placed: the desks' passes leave it alone.
	corpOrderHuman = "human"
	// haulByChair marks a leg the chair posted: it is never put out to contract.
	haulByChair = "chair"
)

type cmdHeadPayload struct {
	Arm      string `json:"arm"`
	CharID   int64  `json:"charID"` // 0 stands the head down
	DeputyID int64  `json:"deputyID,omitempty"`
	Flags    int    `json:"flags,omitempty"`
}

type cmdStandingPayload struct {
	Dept       string `json:"dept"`
	SystemID   int64  `json:"systemID,omitempty"`
	SystemName string `json:"systemName,omitempty"`
	Ores       string `json:"ores,omitempty"`
	Hours      int    `json:"hours"`
}

// cmdBoardPayload is every board order's payload; each kind uses the fields it needs.
type cmdBoardPayload struct {
	TypeID        int64   `json:"typeID,omitempty"`
	Qty           int64   `json:"qty,omitempty"`
	Price         float64 `json:"price,omitempty"`
	Wage          float64 `json:"wage,omitempty"`
	Reward        float64 `json:"reward,omitempty"`
	StationID     int64   `json:"stationID,omitempty"`
	FromStationID int64   `json:"fromStationID,omitempty"`
	Flag          int64   `json:"flag,omitempty"`
	FromFlag      int64   `json:"fromFlag,omitempty"`
	ItemID        int64   `json:"itemID,omitempty"`
	SystemID      int64   `json:"systemID,omitempty"`
	CharID        int64   `json:"charID,omitempty"`
	WorkID        int64   `json:"workID,omitempty"`
	Days          int     `json:"days,omitempty"`
	BuyOnly       bool    `json:"buyOnly,omitempty"`
	CopyOK        bool    `json:"copyOK,omitempty"`
	IsCopy        bool    `json:"isCopy,omitempty"`
	Assign        string  `json:"assign,omitempty"`
}

func (c corpCommand) head() (cmdHeadPayload, bool) {
	var p cmdHeadPayload
	return p, c.Kind == cmdKindHead && json.Unmarshal([]byte(c.Payload), &p) == nil
}

func (c corpCommand) standing() (cmdStandingPayload, bool) {
	var p cmdStandingPayload
	return p, c.Kind == cmdKindStanding && json.Unmarshal([]byte(c.Payload), &p) == nil
}

// cmdBoardKinds are the kinds whose payload is a cmdBoardPayload.
var cmdBoardKinds = map[string]bool{
	cmdKindReq: true, cmdKindHaul: true, cmdKindSurvey: true, cmdKindAssign: true, cmdKindBid: true, cmdKindTrade: true,
	cmdKindPrintBuy: true, cmdKindMove: true, cmdKindSale: true, cmdKindInvite: true, cmdKindFactory: true,
}

func (c corpCommand) board() (cmdBoardPayload, bool) {
	var p cmdBoardPayload
	return p, cmdBoardKinds[c.Kind] && json.Unmarshal([]byte(c.Payload), &p) == nil
}

// workKindWords is a work order's kind as a verb. Pure.
func workKindWords(kind string) string {
	switch kind {
	case workKindME:
		return "Material research on"
	case workKindPE:
		return "Time research on"
	case workKindCopy:
		return "Copies of"
	}
	return "Build"
}

// workUnit is what a work order counts. Pure.
func workUnit(kind string) string {
	switch kind {
	case workKindME, workKindPE:
		return "level(s)"
	case workKindCopy:
		return "copies"
	}
	return "run(s)"
}

// boardTitle and boardSummary word a board order (orgTitle and orgSummary fall through to these).
func (c corpCommand) boardTitle(m *Mod, ctx context.Context) (string, bool) {
	if c.Kind == cmdKindHead {
		return "Division head", true
	}
	if c.Kind == cmdKindStanding {
		return "Standing fleet order", true
	}
	p, ok := c.board()
	if !ok {
		return "", false
	}
	typeName := func() string {
		if m == nil {
			return fmt.Sprintf("type %d", p.TypeID)
		}
		return m.typeNameOr(ctx, p.TypeID)
	}
	switch c.Kind {
	case cmdKindReq:
		return "Requisition: " + typeName(), true
	case cmdKindHaul:
		return "Haul " + typeName(), true
	case cmdKindSurvey:
		return "Moon survey", true
	case cmdKindAssign:
		return "Assignment", true
	case cmdKindBid:
		return "Buy " + typeName(), true
	case cmdKindTrade:
		return "Trade run: " + typeName(), true
	case cmdKindPrintBuy:
		return "Purchase " + typeName(), true
	case cmdKindMove:
		return "Move " + typeName(), true
	case cmdKindSale:
		return "Sell a copy of " + typeName(), true
	case cmdKindInvite:
		return "Invitation", true
	case cmdKindFactory:
		return "Factory base", true
	}
	return "", false
}

func (c corpCommand) boardSummary(m *Mod, ctx context.Context) (string, bool) {
	name := func(id int64) string {
		if m == nil || id == 0 {
			return "nobody"
		}
		return m.charNameOr(ctx, id)
	}
	if h, ok := c.head(); ok {
		if h.CharID == 0 {
			return armLabel(corpArm(h.Arm)) + " has no head", true
		}
		return name(h.CharID) + " heads " + armLabel(corpArm(h.Arm)), true
	}
	if s, ok := c.standing(); ok {
		what := "the combat wing to " + s.SystemName
		if s.Ores != "" {
			what = "mining fleets onto " + strings.ReplaceAll(s.Ores, ",", ", ")
		}
		return what + " for " + durationWords(s.Hours*60), true
	}
	p, ok := c.board()
	if !ok {
		return "", false
	}
	station := func(id int64) string {
		if m == nil {
			return fmt.Sprintf("station %d", id)
		}
		return m.stationLabel(ctx, id)
	}
	system := func(id int64) string {
		if m == nil {
			return fmt.Sprintf("system %d", id)
		}
		return systemNameOf(ctx, m, id)
	}
	typeName := func(id int64) string {
		if m == nil {
			return fmt.Sprintf("type %d", id)
		}
		return m.typeNameOr(ctx, id)
	}
	switch c.Kind {
	case cmdKindReq:
		return fmt.Sprintf("%s to %s within %d day(s)", fmtUnits(p.Qty), station(p.StationID), p.Days), true
	case cmdKindHaul:
		return fmt.Sprintf("%s from %s to %s", fmtUnits(p.Qty), station(p.FromStationID), station(p.StationID)), true
	case cmdKindSurvey:
		return "the moons of " + system(p.SystemID), true
	case cmdKindAssign:
		switch p.Assign {
		case assignDeliver:
			return fmt.Sprintf("deliver %s %s to %s for %s", fmtUnits(p.Qty), typeName(p.TypeID), station(p.StationID), iskString(p.Reward)), true
		case assignPatrol:
			return fmt.Sprintf("kill %d pirates in %s for %s", p.Qty, system(p.SystemID), iskString(p.Reward)), true
		}
		return fmt.Sprintf("build %d run(s) of %s for a bonus of %s", p.Qty, typeName(p.TypeID), iskString(p.Reward)), true
	case cmdKindBid:
		return fmt.Sprintf("%s at %s each, at %s", fmtUnits(p.Qty), iskString(p.Price), station(p.StationID)), true
	case cmdKindTrade:
		return fmt.Sprintf("%s from %s to %s, at most %s each, wage %s", fmtUnits(p.Qty), station(p.FromStationID), station(p.StationID),
			iskString(p.Price), iskString(p.Wage)), true
	case cmdKindPrintBuy:
		return fmt.Sprintf("up to %s, bought by %s", iskString(p.Price), name(p.CharID)), true
	case cmdKindMove:
		return "from " + station(p.FromStationID) + " to " + station(p.StationID), true
	case cmdKindSale:
		return "at " + station(p.StationID), true
	case cmdKindInvite:
		return name(p.CharID), true
	case cmdKindFactory:
		return station(p.StationID), true
	}
	return "", false
}

// sameCommandTarget reports whether two orders are about the same thing, so a division head's does
// not go over the CEO's. Pure.
func sameCommandTarget(a, b corpCommand) bool {
	if da, ok := fleetDeptOf(a); ok {
		db, ok := fleetDeptOf(b)
		return ok && da == db
	}
	if ia, ok := printOf(a); ok {
		ib, ok := printOf(b)
		return ok && ia == ib
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case cmdKindArm:
		x, _ := a.arm()
		y, _ := b.arm()
		return x.CharID == y.CharID
	case cmdKindPosting:
		x, _ := a.posting()
		y, _ := b.posting()
		return x.CharID == y.CharID
	case cmdKindSeat:
		x, _ := a.seat()
		y, _ := b.seat()
		return x.CharID == y.CharID
	case cmdKindBid:
		x, _ := a.board()
		y, _ := b.board()
		return x.TypeID == y.TypeID && x.StationID == y.StationID
	}
	return false
}

// fleetDeptOf is the fleets an order steers, ok false for an order that steers none. Pure.
func fleetDeptOf(c corpCommand) (string, bool) {
	if p, ok := c.fleet(); ok {
		return deptForActivity(p.Activity), true
	}
	if p, ok := c.standing(); ok {
		return p.Dept, true
	}
	return "", false
}

// printOf is the one print an order is about, ok false for an order about none. Pure.
func printOf(c corpCommand) (int64, bool) {
	if p, ok := c.work(); ok && p.BPItemID != 0 {
		return p.BPItemID, true
	}
	if p, ok := c.board(); ok && (c.Kind == cmdKindMove || c.Kind == cmdKindSale) {
		return p.ItemID, true
	}
	return 0, false
}

// postedLine is the note a board row carries from the chair.
func (m *Mod) postedLine(ctx context.Context, c corpCommand) string {
	s := "ordered by " + m.charNameOr(ctx, c.PosterID)
	if c.Note != "" {
		s += ": " + c.Note
	}
	return clampText(s, 255)
}

// orderLetter tells a person they were assigned, from whoever assigned them.
func (m *Mod) orderLetter(ctx context.Context, c corpCommand, charID int64, subject, body string) {
	if charID == 0 || charID == c.PosterID || m.isSynthetic(charID) {
		return
	}
	m.mailPilot(ctx, charID, c.PosterID, subject, body+
		m.signature(ctx, c.PosterID, esp.IGBText(c.PosterHat), m.corpNameOf(ctx, c.CorpID)))
}
