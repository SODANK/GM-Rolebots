package rolebots

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The organisation orders: the rest of what a bot CEO decides, given to the
// chair as orders on the Management page (corporders_org.go, corpmanage_people.go) and carried out by
// the order desk (corpcommanddesk_org.go). A placement (seat, arm, posting)
// writes the member's row with its set-by-the-chair mark and is done: the passes
// that deal those rows keep a chair's placement. A setting (tags, industry desk,
// budget, ore drop-off, ore desk) is applied at once and then held: the order
// stays live until its hold runs out, and the CEO power for the same setting
// asks humanHoldOn first and stands aside. One rule covers every setting and
// shows each hold on the Operations page.

const (
	cmdKindTags     = "tags"
	cmdKindSeat     = "seat"
	cmdKindIndustry = "industry"
	cmdKindArm      = "arm"
	cmdKindPosting  = "posting"
	cmdKindBudget   = "budget"
	cmdKindOreDrop  = "oredrop"
	cmdKindOreDesk  = "oredesk"
	cmdKindHangars  = "hangars"
)

// cmdHoldKinds are the settings that stay live for their hold.
var cmdHoldKinds = map[string]bool{
	cmdKindTags: true, cmdKindIndustry: true, cmdKindBudget: true, cmdKindOreDrop: true, cmdKindOreDesk: true,
}

type cmdTagsPayload struct {
	Tags     []string `json:"tags"`
	HoldDays int      `json:"holdDays"`
	// Prev is what the setting was, put back when the order is withdrawn (undoOrgCommand).
	Prev  []string `json:"prev,omitempty"`
	Saved bool     `json:"saved,omitempty"`
}

type cmdSeatPayload struct {
	CharID   int64  `json:"charID"`
	Activity string `json:"activity"`
	Release  bool   `json:"release,omitempty"`
}

type cmdIndustryPayload struct {
	DirectorID int64 `json:"directorID"`
	DeputyID   int64 `json:"deputyID,omitempty"`
	HoldDays   int   `json:"holdDays"`
}

type cmdArmPayload struct {
	CharID int64  `json:"charID"`
	Arm    string `json:"arm"` // "" releases the member to the pool
}

type cmdPostingPayload struct {
	CharID    int64 `json:"charID"`
	StationID int64 `json:"stationID"`
}

type cmdBudgetPayload struct {
	ReserveMonths float64 `json:"reserveMonths"`
	StipendShare  float64 `json:"stipendShare"`
	StoreMarkup   float64 `json:"storeMarkup"`
	BuybackFrac   float64 `json:"buybackFrac"`
	LevyOn        bool    `json:"levyOn"`
	HoldDays      int     `json:"holdDays"`
}

type cmdOreDropPayload struct {
	StationID int64 `json:"stationID"` // 0 = sell as usual
	HoldDays  int   `json:"holdDays"`
	Prev      int64 `json:"prev,omitempty"`
	Saved     bool  `json:"saved,omitempty"`
}

type cmdOreDeskPayload struct {
	Share    float64 `json:"share"` // of ore kept for production, 0..1
	HoldDays int     `json:"holdDays"`
	Prev     float64 `json:"prev,omitempty"`
	Saved    bool    `json:"saved,omitempty"`
}

// cmdHangarsPayload is the chair's choice of hangar for each war-chest purpose (corphangars.go): 0
// leaves it to the allocator, pinNone gives it none, 2..5 puts it there.
type cmdHangarsPayload struct {
	Armory int `json:"armory"`
	Yard   int `json:"yard"`
	Spoils int `json:"spoils"`
}

// pin is the payload's choice for one purpose.
func (p cmdHangarsPayload) pin(pur divPurpose) int {
	switch pur {
	case purArmory:
		return p.Armory
	case purYard:
		return p.Yard
	}
	return p.Spoils
}

func (c corpCommand) hangars() (cmdHangarsPayload, bool) {
	var p cmdHangarsPayload
	return p, c.Kind == cmdKindHangars && json.Unmarshal([]byte(c.Payload), &p) == nil
}

func (c corpCommand) tags() (cmdTagsPayload, bool) {
	var p cmdTagsPayload
	return p, c.Kind == cmdKindTags && json.Unmarshal([]byte(c.Payload), &p) == nil
}
func (c corpCommand) seat() (cmdSeatPayload, bool) {
	var p cmdSeatPayload
	return p, c.Kind == cmdKindSeat && json.Unmarshal([]byte(c.Payload), &p) == nil
}
func (c corpCommand) industry() (cmdIndustryPayload, bool) {
	var p cmdIndustryPayload
	return p, c.Kind == cmdKindIndustry && json.Unmarshal([]byte(c.Payload), &p) == nil
}
func (c corpCommand) arm() (cmdArmPayload, bool) {
	var p cmdArmPayload
	return p, c.Kind == cmdKindArm && json.Unmarshal([]byte(c.Payload), &p) == nil
}
func (c corpCommand) posting() (cmdPostingPayload, bool) {
	var p cmdPostingPayload
	return p, c.Kind == cmdKindPosting && json.Unmarshal([]byte(c.Payload), &p) == nil
}
func (c corpCommand) budget() (cmdBudgetPayload, bool) {
	var p cmdBudgetPayload
	return p, c.Kind == cmdKindBudget && json.Unmarshal([]byte(c.Payload), &p) == nil
}
func (c corpCommand) oreDrop() (cmdOreDropPayload, bool) {
	var p cmdOreDropPayload
	return p, c.Kind == cmdKindOreDrop && json.Unmarshal([]byte(c.Payload), &p) == nil
}
func (c corpCommand) oreDesk() (cmdOreDeskPayload, bool) {
	var p cmdOreDeskPayload
	return p, c.Kind == cmdKindOreDesk && json.Unmarshal([]byte(c.Payload), &p) == nil
}

// orgTitle / orgSummary word the organisation orders (Title / Summary fall
// through to these).
func (c corpCommand) orgTitle(m *Mod, ctx context.Context) string {
	switch c.Kind {
	case cmdKindTags:
		return "Identity"
	case cmdKindSeat:
		if p, ok := c.seat(); ok && p.Release {
			return "Fleet seat released"
		}
		return "Fleet seat"
	case cmdKindIndustry:
		return "Industry desk"
	case cmdKindArm:
		return "Arm"
	case cmdKindPosting:
		return "Posting"
	case cmdKindBudget:
		return "Budget"
	case cmdKindOreDrop:
		return "Ore drop-off"
	case cmdKindOreDesk:
		return "Ore desk"
	case cmdKindHangars:
		return "Hangars"
	}
	if t, ok := c.boardTitle(m, ctx); ok {
		return t
	}
	return strings.ToUpper(c.Kind[:1]) + c.Kind[1:]
}

func (c corpCommand) orgSummary(m *Mod, ctx context.Context) string {
	name := func(id int64) string {
		if m == nil || id == 0 {
			return "nobody"
		}
		return m.charNameOr(ctx, id)
	}
	station := func(id int64) string {
		if m == nil {
			return fmt.Sprintf("station %d", id)
		}
		return m.stationLabel(ctx, id)
	}
	switch c.Kind {
	case cmdKindTags:
		if p, ok := c.tags(); ok {
			return strings.Join(p.Tags, " / ") + fmt.Sprintf(", held %d day(s)", p.HoldDays)
		}
	case cmdKindSeat:
		if p, ok := c.seat(); ok {
			if p.Release {
				return name(p.CharID) + " stands down"
			}
			return name(p.CharID) + " commands " + strings.ToLower(activityLabel(p.Activity)) + " fleets"
		}
	case cmdKindIndustry:
		if p, ok := c.industry(); ok {
			s := "Director of Industry " + name(p.DirectorID)
			if p.DeputyID != 0 {
				s += ", deputy " + name(p.DeputyID)
			}
			return s + fmt.Sprintf(", held %d day(s)", p.HoldDays)
		}
	case cmdKindArm:
		if p, ok := c.arm(); ok {
			if p.Arm == "" {
				return name(p.CharID) + " released to the pool"
			}
			return name(p.CharID) + " to " + armLabel(corpArm(p.Arm))
		}
	case cmdKindPosting:
		if p, ok := c.posting(); ok {
			return name(p.CharID) + " to " + station(p.StationID)
		}
	case cmdKindBudget:
		if p, ok := c.budget(); ok {
			levy := "no levy"
			if p.LevyOn {
				levy = "levy on"
			}
			return fmt.Sprintf("reserve %.1f months, stipends %.0f%%, store +%.0f%%, buyback %.0f%% of value, %s, held %d day(s)",
				p.ReserveMonths, p.StipendShare*100, p.StoreMarkup*100, p.BuybackFrac*100, levy, p.HoldDays)
		}
	case cmdKindOreDrop:
		if p, ok := c.oreDrop(); ok {
			if p.StationID == 0 {
				return fmt.Sprintf("fleets sell their ore, held %d day(s)", p.HoldDays)
			}
			return fmt.Sprintf("fleet ore to %s, held %d day(s)", station(p.StationID), p.HoldDays)
		}
	case cmdKindOreDesk:
		if p, ok := c.oreDesk(); ok {
			return fmt.Sprintf("%.0f%% of ore kept for production, held %d day(s)", p.Share*100, p.HoldDays)
		}
	case cmdKindHangars:
		if p, ok := c.hangars(); ok {
			return hangarsWords(p)
		}
	}
	if s, ok := c.boardSummary(m, ctx); ok {
		return s
	}
	return c.Note
}

// humanHoldOn reports whether the chair has a setting of this kind in force:
// a live order whose hold has not run out. The CEO power that moves the same
// setting stands aside while it does.
func (m *Mod) humanHoldOn(ctx context.Context, corpID int64, kind string) bool {
	return m.humanHold(ctx, corpID, kind) != nil
}

// humanHold is the live order of a kind, nil for none.
func (m *Mod) humanHold(ctx context.Context, corpID int64, kind string) *corpCommand {
	if corpID == 0 {
		return nil
	}
	c, err := scanCommand(m.db.QueryRowContext(ctx, `
		SELECT `+cmdCols+` FROM rolebot_corp_commands
		 WHERE corporationID = ? AND kind = ? AND status = 'live' AND expiresAt > ?
		 ORDER BY postedAt DESC LIMIT 1`, corpID, kind, clock.Unix()))
	if err != nil {
		return nil
	}
	return &c
}
