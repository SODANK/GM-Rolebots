package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// SimState is the persistent abstract-tier state of one rolebot. The sim
// resumes exactly where it stopped across server restarts.
type SimState struct {
	CharID   int64  `json:"-"`
	Activity string `json:"-"` // current activity slug ("" = none)
	Phase    string `json:"-"` // FSM phase, e.g. "travel", "mining.mine"
	Data     StateData

	// Failsafe bookkeeping: a bot that fails maxConsecutiveErrors steps in a row is halted, and the
	// sim skips it until an operator hits Reboot in the panel. A dead end costs one log line, not a
	// retry storm.
	ErrCount    int
	LastError   string
	LastErrorAt int64
	Halted      bool

	// catchUp is the leg catch-up armed for the turn in progress (legcatchup.go). It lives
	// for one turn and is never stored.
	catchUp legCatchUp
}

// StateData is the JSON blob column: everything a phase needs to resume.
type StateData struct {
	// Embedded families keep JSON keys flat (st.Data.IndJobID); each lives in state_<family>.go and says
	// whether idleData keeps it. SQL reads undockRunUntil (parklane.go), piracyHeatUntil (legalcensus.go)
	// and gangID (combat_abstract.go), so those stay top level; TestStateDataWireKeysUnchanged pins keys.
	TravelState
	UndockState
	AppetiteState
	StickyState
	GangDuty
	OrderState
	TradeState
	TradeMemory
	ProcureState
	HuntState
	CombatMemory
	PiracyState
	PiracyMemory
	MineState
	MineMemory
	IceState
	IndState
	PosState
	RndState
	LabState
	CourierState
	MissionState
	EscrowState
	EnvoyState
	CarryState
}

// LabCookingJob is one research job the pilot walked away from. It is thinner than IndCookingJob:
// research produces nothing to sell, so the entry only says which print, where it is and where
// it belongs afterwards.
type LabCookingJob struct {
	JobID      int64  `json:"jobID"`
	BPItemID   int64  `json:"bpItemID"`
	BPTypeID   int64  `json:"bpTypeID,omitempty"`
	BPName     string `json:"bpName,omitempty"`
	ActivityID int64  `json:"activityID,omitempty"`
	// StationID is the lab: where the print is while the job runs and where it is handed back.
	// FactoryID is where it belongs. The two often differ, so a delivered print can be a stray the
	// repatriation sweep brings home.
	StationID    int64 `json:"stationID,omitempty"`
	StationSysID int64 `json:"stationSysID,omitempty"`
	FactoryID    int64 `json:"factoryID,omitempty"`
	FactorySysID int64 `json:"factorySysID,omitempty"`
	EndsAt       int64 `json:"endsAt,omitempty"`
	StartedAt    int64 `json:"startedAt,omitempty"`
	// Copies and CopyRuns describe a copy job (activity 5): how many copies come off the line and
	// how many runs each carries. Copy stock counts them before they exist, so a researcher does not
	// order the same stock twice (copyStockFor).
	Copies   int   `json:"copies,omitempty"`
	CopyRuns int64 `json:"copyRuns,omitempty"`
}

// IndCookingJob is one installed job the pilot walked away from, carrying what the collect and
// sell phases need: the costs the sale price is built from, and where the line is.
type IndCookingJob struct {
	JobID        int64   `json:"jobID"`
	TypeID       int64   `json:"typeID"`
	TypeName     string  `json:"typeName"`
	BPTypeID     int64   `json:"bpTypeID,omitempty"`
	BPItemID     int64   `json:"bpItemID,omitempty"`
	Evidence     string  `json:"evidence,omitempty"`
	FactoryID    int64   `json:"factoryID"`
	FactorySysID int64   `json:"factorySysID"`
	Runs         int     `json:"runs"`
	BPOCost      float64 `json:"bpoCost,omitempty"`
	MatValue     float64 `json:"matValue,omitempty"`
	Fees         float64 `json:"fees,omitempty"`
	// CashSpent is the project's IndSpentISK at install time: the wallet's side, carried so the
	// sell log can report what the batch cost against what it grossed.
	CashSpent float64 `json:"cashSpent,omitempty"`
	// BPOPaid is the project's IndBPOPaid at install time.
	BPOPaid   float64 `json:"bpoPaid,omitempty"`
	StartedAt int64   `json:"startedAt,omitempty"`
	// Delivered means the line was handed back and the output sits in the hangar at the factory
	// (industryOvenSweep, when the oven rings). Only the listing is left, which needs a pilot there.
	// The entry stays, keeping its demand claim warm, until dropped.
	Delivered bool `json:"delivered,omitempty"`
}

// ShopItem is one outstanding purchase of a procurement trip.
type ShopItem struct {
	TypeID int64  `json:"t"`
	Name   string `json:"n"`
	Qty    int64  `json:"q"`
	Dest   string `json:"d"`           // hull | module | charge | cargo | drone | implant
	Flag   int64  `json:"f,omitempty"` // slot flag for module/charge
}

// stateCols is the column list every read of rolebot_state selects, in the order scanStateInto
// expects. The single read and the bulk read (bulkload.go) share it, so both decode a row the
// same way.
const stateCols = `activity, phase, data, errCount, lastError, lastErrorAt, halted`

// newSimState is what a pilot with no state row is: idle, nothing pending.
func newSimState(charID int64) *SimState {
	return &SimState{CharID: charID, Phase: "idle"}
}

// scanStateInto decodes one stateCols row into st. It takes the row's Scan
// rather than the row, so a caller selecting extra LEADING columns (the bulk
// loader selects characterID first) can wrap it.
func scanStateInto(scan func(dest ...any) error, st *SimState) error {
	var data, lastErr sql.NullString
	var halted int64
	if err := scan(&st.Activity, &st.Phase, &data, &st.ErrCount, &lastErr,
		&st.LastErrorAt, &halted); err != nil {
		return err
	}
	st.LastError = lastErr.String
	st.Halted = halted != 0
	if data.Valid && data.String != "" {
		if err := json.Unmarshal([]byte(data.String), &st.Data); err != nil {
			// Corrupt blob: reset rather than wedge the bot forever.
			st.Data = StateData{}
			st.Phase = "idle"
		}
	}
	if st.Phase == "" {
		st.Phase = "idle"
	}
	return nil
}

func loadState(ctx context.Context, db *sql.DB, charID int64) (*SimState, error) {
	st := newSimState(charID)
	row := db.QueryRowContext(ctx,
		`SELECT `+stateCols+` FROM rolebot_state WHERE characterID = ?`, charID)
	err := scanStateInto(row.Scan, st)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loadState: %w", err)
	}
	return st, nil
}

func saveState(ctx context.Context, db *sql.DB, st *SimState) error {
	// A leg this turn started is timed from when the last one ended, not from when the
	// late turn came round (legcatchup.go).
	st.applyLegCatchUp()
	blob, err := json.Marshal(&st.Data)
	if err != nil {
		return fmt.Errorf("saveState marshal: %w", err)
	}
	halted := 0
	if st.Halted {
		halted = 1
	}
	sum := stateFingerprint(st, blob, halted)
	if stateUnchanged(st.CharID, sum) {
		return nil // statesave.go: nothing to write but updatedAt, which nobody reads
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO rolebot_state
			(characterID, activity, phase, data, updatedAt, errCount, lastError, lastErrorAt, halted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE activity = VALUES(activity), phase = VALUES(phase),
			data = VALUES(data), updatedAt = VALUES(updatedAt),
			errCount = VALUES(errCount), lastError = VALUES(lastError),
			lastErrorAt = VALUES(lastErrorAt), halted = VALUES(halted)`,
		st.CharID, st.Activity, st.Phase, string(blob), clock.Unix(),
		st.ErrCount, st.LastError, st.LastErrorAt, halted)
	if err != nil {
		return fmt.Errorf("saveState: %w", err)
	}
	noteStateSaved(st.CharID, sum)
	noteClaim(st.CharID, claimFromState(st.Activity, st.Halted, &st.Data)) // claims.go
	noteGridDue(st.CharID, st.Data.LegEndsAt, st.Halted)                   // griddue.go
	// A write from outside the pilot's roster turn (another lane, the panel) changes what
	// it waits on; the turn's own writes are followed by a fresh record (rosterwake.go).
	rosterWakeClear(st.CharID)
	return nil
}

// ── failsafe ────────────────────────────────────────────────────────────────

// maxConsecutiveErrors is how many failed steps in a row a bot gets before the sim stops
// touching it. Steps run every 5s, so a wedged bot halts within about 15s.
const maxConsecutiveErrors = 3

// haltRetrySec is how long a halted bot sits before the sim drops its trip and lets it plan a
// fresh one. The halt must not be permanent: dropping the trip fixes most causes, and a cause
// that remains halts the bot again after three failures. The panel still shows the halt.
const haltRetrySec = 300

// noteFailure records one failed step and halts the bot when it has failed
// too many times in a row. Returns whether the bot is now halted.
func (m *Mod) noteFailure(ctx context.Context, st *SimState, category, msg string) bool {
	st.ErrCount++
	st.LastError = msg
	if len(st.LastError) > 500 {
		st.LastError = st.LastError[:500]
	}
	st.LastErrorAt = clock.Unix()
	if st.ErrCount >= maxConsecutiveErrors && !st.Halted {
		st.Halted = true
		m.logBot(st.CharID, logError, category,
			"HALTED after %d consecutive failures — last: %s (reboot from the Rolebot panel to resume)",
			st.ErrCount, msg)
	}
	if err := saveState(ctx, m.db, st); err != nil {
		rlogf("rolebots: noteFailure save: %v", err)
	}
	return st.Halted
}

// clearFailure wipes the failure counters after a step that got somewhere.
func (m *Mod) clearFailure(ctx context.Context, st *SimState) {
	if st.ErrCount == 0 && st.LastError == "" {
		return
	}
	st.ErrCount, st.LastError, st.LastErrorAt = 0, "", 0
	if err := saveState(ctx, m.db, st); err != nil {
		rlogf("rolebots: clearFailure save: %v", err)
	}
}

// resetBot clears the trip a bot was in the middle of (phase, halt flag, error) and leaves it to
// re-plan. It keeps the bot's memory of itself: recent belt systems, appetite tally, hauled
// commodity and restock cooldown. That matters on death (pod.go): a podded bot must remember the
// system that killed it and steer away.
func resetBot(ctx context.Context, db *sql.DB, charID int64) error {
	st, err := loadState(ctx, db, charID)
	if err != nil {
		return fmt.Errorf("resetBot: %w", err)
	}
	st.Activity, st.Phase = "", string(TaskIdle)
	// idleData is the "this trip is over" filter: it drops trip facts and carries the facts about
	// the bot.
	st.Data = idleData(st.Data)
	st.ErrCount, st.LastError, st.LastErrorAt, st.Halted = 0, "", 0, false
	err = saveState(ctx, db, st)
	// Even on a failed save: the trip is over (the hull is gone, the pilot
	// released), and the copy in a roster pass's hands is stale either way.
	noteStateReset(charID) // stateepoch.go
	return err
}

// wipeBot forgets a bot completely, the trip and everything it had learned. It is the operator's
// clean-slate action, for a bot wedged by bad remembered state or being repurposed.
func wipeBot(ctx context.Context, db *sql.DB, charID int64) error {
	forgetSavedState(charID)
	noteStateReset(charID)
	if _, err := db.ExecContext(ctx,
		`DELETE FROM rolebot_state WHERE characterID = ?`, charID); err != nil {
		return fmt.Errorf("wipeBot: %w", err)
	}
	return nil
}

// haltedBots lists the rolebots currently halted.
func haltedBots(ctx context.Context, db *sql.DB) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.characterID FROM rolebot_state s
		JOIN rolebot_characters b ON b.characterID = s.characterID
		WHERE s.halted = 1`)
	if err != nil {
		return nil, fmt.Errorf("haltedBots: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
