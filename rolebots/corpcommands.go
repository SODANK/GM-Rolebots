package rolebots

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Orders from the chair: what a human CEO or Director posts on the Corporate
// Portal for the corporation's bots to carry out (rolebot_corp_commands). The
// table word is command because rolebot_corp_orders is the market-order ledger;
// the page says order. One row per posting holds its whole life: who posted it
// and wearing which hat, the kind, the payload, the status the order desk
// (corpcommanddesk.go) moves it through and the sentence it left behind.
// Statuses: pending, claimed (refKind/refID say which fleet, work row or
// directive it became), live, done, refused (Result says why), expired (nobody
// could take it before it lapsed), cancelled (withdrawn by a director).

const (
	cmdPending   = "pending"
	cmdClaimed   = "claimed"
	cmdLive      = "live"
	cmdDone      = "done"
	cmdRefused   = "refused"
	cmdExpired   = "expired"
	cmdCancelled = "cancelled"
)

// The kinds. Each has a payload shape (the cmd*Payload structs below, and those of
// corpcommands_org.go and corpcommands_board.go) and a form on the Management page.
const (
	cmdKindFleet     = "fleet"
	cmdKindDirective = "directive"
	cmdKindWork      = "work"
	cmdKindChat      = "chat" // a verb typed in fleet chat, recorded for the page
)

type corpCommand struct {
	CommandID int64
	CorpID    int64
	PosterID  int64
	PosterHat string
	Kind      string
	Payload   string
	Note      string
	PostedAt  int64
	ExpiresAt int64
	Status    string
	ClaimedBy int64
	ClaimedAt int64
	ClosedAt  int64
	RefKind   string
	RefID     int64
	Result    string
	Attempts  int
	// Arm is the division the order was given to ("" for the corporation's own settings), and
	// PosterCEO that the CEO gave it: a division head neither sees past its own arm nor
	// overrules the CEO (corpscope.go).
	Arm       string
	PosterCEO bool
}

// Open reports whether the order is still being acted on.
func (c corpCommand) Open() bool {
	switch c.Status {
	case cmdPending, cmdClaimed, cmdLive:
		return true
	}
	return false
}

// cmdFleetPayload is a fleet order: send a fleet of this kind to this
// system, this big, for this long.
type cmdFleetPayload struct {
	Activity    string  `json:"activity"`
	SystemID    int64   `json:"systemID"`
	SystemName  string  `json:"systemName,omitempty"`
	Size        int     `json:"size"`
	DurationMin int     `json:"durationMin"`
	Ores        []int64 `json:"ores,omitempty"`
	OreNames    string  `json:"oreNames,omitempty"`
}

// cmdDirectivePayload is a directive: where the corporation's effort goes
// and how hard the chair leans on it.
type cmdDirectivePayload struct {
	Weights  map[string]float64 `json:"weights"`
	Strength float64            `json:"strength"`
	HoldDays int                `json:"holdDays"`
}

// cmdWorkPayload is a work order for the Production Board.
type cmdWorkPayload struct {
	BPTypeID      int64   `json:"bpTypeID"`
	ProductTypeID int64   `json:"productTypeID"`
	StationID     int64   `json:"stationID"`
	Runs          int     `json:"runs"`
	WagePerRun    float64 `json:"wagePerRun"`
	Kind          string  `json:"kind,omitempty"`
	// BPItemID pins the one original a research or copy order is for; Priority is the board's (0 =
	// first); RequesterArm and DestFlag say whose it is and where it goes (corprequest.go).
	BPItemID     int64  `json:"bpItemID,omitempty"`
	Priority     int    `json:"priority,omitempty"`
	RequesterArm string `json:"requesterArm,omitempty"`
	DestFlag     int64  `json:"destFlag,omitempty"`
}

func (c corpCommand) fleet() (cmdFleetPayload, bool) {
	var p cmdFleetPayload
	if c.Kind != cmdKindFleet || json.Unmarshal([]byte(c.Payload), &p) != nil {
		return p, false
	}
	return p, true
}

func (c corpCommand) directive() (cmdDirectivePayload, bool) {
	var p cmdDirectivePayload
	if c.Kind != cmdKindDirective || json.Unmarshal([]byte(c.Payload), &p) != nil {
		return p, false
	}
	return p, true
}

func (c corpCommand) work() (cmdWorkPayload, bool) {
	var p cmdWorkPayload
	if c.Kind != cmdKindWork || json.Unmarshal([]byte(c.Payload), &p) != nil {
		return p, false
	}
	return p, true
}

// Title and Summary word an order for a page or a letter.
func (c corpCommand) Title(m *Mod, ctx context.Context) string {
	switch c.Kind {
	case cmdKindFleet:
		if p, ok := c.fleet(); ok {
			return activityLabel(p.Activity) + " fleet"
		}
		return "Fleet"
	case cmdKindDirective:
		return "Directive"
	case cmdKindWork:
		if p, ok := c.work(); ok {
			return workKindWords(p.Kind) + " " + m.typeNameOr(ctx, p.ProductTypeID)
		}
		return "Work order"
	case cmdKindChat:
		return "Fleet chat order"
	}
	return c.orgTitle(m, ctx)
}

func (c corpCommand) Summary(m *Mod, ctx context.Context) string {
	switch c.Kind {
	case cmdKindFleet:
		if p, ok := c.fleet(); ok {
			s := fmt.Sprintf("%d pilots to %s for %s", p.Size, p.SystemName, durationWords(p.DurationMin))
			if p.OreNames != "" {
				s += " — " + strings.ReplaceAll(p.OreNames, ",", ", ")
			}
			return s
		}
	case cmdKindDirective:
		if p, ok := c.directive(); ok {
			return directiveText(p.Weights) + fmt.Sprintf(", held %d day(s)", p.HoldDays)
		}
	case cmdKindWork:
		if p, ok := c.work(); ok {
			return fmt.Sprintf("%d %s at %s each", p.Runs, workUnit(p.Kind), iskString(p.WagePerRun))
		}
	case cmdKindChat:
		return c.Note
	}
	return c.orgSummary(m, ctx)
}

func durationWords(mins int) string {
	switch {
	case mins <= 0:
		return "a while"
	case mins < 60:
		return fmt.Sprintf("%d min", mins)
	case mins%60 == 0:
		return fmt.Sprintf("%d h", mins/60)
	}
	return fmt.Sprintf("%d h %d min", mins/60, mins%60)
}

// ── table access ────────────────────────────────────────────────────────────

const cmdCols = `commandID, corporationID, posterID, posterHat, kind, payload, note, postedAt, expiresAt,
	status, claimedBy, claimedAt, closedAt, refKind, refID, result, attempts, arm, posterCEO`

func scanCommand(sc interface{ Scan(...any) error }) (corpCommand, error) {
	var c corpCommand
	err := sc.Scan(&c.CommandID, &c.CorpID, &c.PosterID, &c.PosterHat, &c.Kind, &c.Payload, &c.Note,
		&c.PostedAt, &c.ExpiresAt, &c.Status, &c.ClaimedBy, &c.ClaimedAt, &c.ClosedAt,
		&c.RefKind, &c.RefID, &c.Result, &c.Attempts, &c.Arm, &c.PosterCEO)
	return c, err
}

// listCorpCommands is a corporation's orders, open ones first, newest first.
func (m *Mod) listCorpCommands(ctx context.Context, corpID int64, limit int) []corpCommand {
	rows, err := m.db.QueryContext(ctx, `
		SELECT `+cmdCols+` FROM rolebot_corp_commands
		 WHERE corporationID = ?
		 ORDER BY (status IN ('pending','claimed','live')) DESC, postedAt DESC
		 LIMIT ?`, corpID, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []corpCommand
	for rows.Next() {
		if c, err := scanCommand(rows); err == nil {
			out = append(out, c)
		}
	}
	return out
}

func (m *Mod) getCorpCommand(ctx context.Context, id int64) (*corpCommand, error) {
	c, err := scanCommand(m.db.QueryRowContext(ctx,
		`SELECT `+cmdCols+` FROM rolebot_corp_commands WHERE commandID = ?`, id))
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// postCorpCommand files an order. The payload is marshalled by the caller's
// kind; status starts pending.
func (m *Mod) postCorpCommand(ctx context.Context, c corpCommand) (int64, error) {
	now := clock.Unix()
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_commands
			(corporationID, posterID, posterHat, kind, payload, note, postedAt, expiresAt, arm, posterCEO, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
		c.CorpID, c.PosterID, clampText(c.PosterHat, 16), c.Kind, c.Payload, clampText(c.Note, 255), now, c.ExpiresAt,
		c.Arm, c.PosterCEO)
	if err != nil {
		return 0, fmt.Errorf("postCorpCommand: %w", err)
	}
	return res.LastInsertId()
}

// setCommandStatus moves an order along and records what became of it.
func (m *Mod) setCommandStatus(ctx context.Context, id int64, status string, result string) error {
	now := clock.Unix()
	closed := int64(0)
	switch status {
	case cmdDone, cmdRefused, cmdExpired, cmdCancelled:
		closed = now
	}
	_, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_commands SET status = ?, result = ?, closedAt = IF(? > 0, ?, closedAt)
		 WHERE commandID = ?`, status, clampText(result, 255), closed, closed, id)
	if err != nil {
		return fmt.Errorf("setCommandStatus: %w", err)
	}
	return nil
}

// claimCommand records who took an order and what it became.
func (m *Mod) claimCommand(ctx context.Context, id, claimedBy int64, refKind string, refID int64, result string) error {
	_, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_commands
		   SET status = 'claimed', claimedBy = ?, claimedAt = ?, refKind = ?, refID = ?, result = ?
		 WHERE commandID = ?`, claimedBy, clock.Unix(), refKind, refID, clampText(result, 255), id)
	if err != nil {
		return fmt.Errorf("claimCommand: %w", err)
	}
	return nil
}

// noteCommandAttempt counts a failed try and keeps why.
func (m *Mod) noteCommandAttempt(ctx context.Context, id int64, why string) error {
	_, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_commands SET attempts = attempts + 1, result = ? WHERE commandID = ?`, clampText(why, 255), id)
	return err
}
