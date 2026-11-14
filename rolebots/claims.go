package rolebots

// The roster's claims board: what every pilot is building and hauling. Two planners read it
// so the roster does not pile onto one idea: the industry picker counts rival projects per
// type (industryActiveProjects) and the trade planner counts commodities and refuses exact
// routes in flight (tradeActiveRuns).
// The board has two halves. A snapshot of the table is read once per claimsTTL through the
// single-flight snapshotFor the market reads use, holding only the claims plus who is
// enabled. A write-through overlay fed by saveState makes a claim just written visible at
// once; it wins over the snapshot for the same pilot, a dropped claim included.
// Between a pilot choosing a project and its step's saveState nobody can see the choice.

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"
)

// claimsTTL is how long one table read stands. Enabling or halting a pilot
// from the panel, and anything written by another process, shows up within
// this; everything saveState writes shows up at once through the overlay.
const claimsTTL = 30 * time.Second

// claimOverlayLife must outlast the slowest snapshot that could have started
// before a save and so missed it: claimsTTL of cache life after a read that
// can itself take up to snapStatementCap.
const claimOverlayLife = 3 * claimsTTL

// botClaim is the part of a pilot's state the two planners read.
type botClaim struct {
	Activity string
	Halted   bool

	IndTypeID    int64
	IndJobID     int64
	IndStartedAt int64

	TradeTypeID        int64
	TradeQty           int64
	TradeBuyStationID  int64
	TradeSellStationID int64
	// TradeKind and TradeBought size the claim against the book
	// (tradeLaneLoad): a spec run lists rather than selling into a bid, and a
	// run that has bought has already taken its units off the ask.
	TradeKind   string
	TradeBought bool

	// CourierMissionID is the contract a pilot is flying to pick up or already carrying
	// (courier_accept.go). It is written at the pick, since the accept happens at the pickup
	// station, so planners can steer other pilots off the same contract.
	CourierMissionID int64
}

func claimFromState(activity string, halted bool, d *StateData) botClaim {
	return botClaim{
		Activity: activity, Halted: halted,
		IndTypeID: d.IndTypeID, IndJobID: d.IndJobID, IndStartedAt: d.IndStartedAt,
		TradeTypeID: d.TradeTypeID, TradeQty: d.TradeQty,
		TradeBuyStationID: d.TradeBuyStationID, TradeSellStationID: d.TradeSellStationID,
		TradeKind: d.TradeKind, TradeBought: d.TradeUnitCost > 0,
		CourierMissionID: d.CourierMissionID,
	}
}

// claimsAnything is whether any planner could count this pilot. The
// snapshot keeps only these; the overlay keeps everything, because an empty
// claim there is what retracts a stale one in the snapshot.
func (c botClaim) claimsAnything() bool {
	return c.IndTypeID != 0 || c.TradeTypeID != 0 || c.CourierMissionID != 0
}

// claimedCouriers is every courier contract some OTHER pilot is already
// flying for, missionID → charID. Read by courierAcceptPlan so two pilots
// do not chase one contract.
func (m *Mod) claimedCouriers(ctx context.Context, self int64) map[int64]int64 {
	out := map[int64]int64{}
	forEachClaim(m.claimsBoardNow(ctx), func(charID int64, c botClaim) {
		if charID != self && c.CourierMissionID != 0 {
			out[c.CourierMissionID] = charID
		}
	})
	return out
}

// buildingNow is projectCountsAsBuilding over a claim, so the rule stays in
// one place.
func (c botClaim) buildingNow(now int64) bool {
	return projectCountsAsBuilding(&StateData{
		IndState: IndState{IndTypeID: c.IndTypeID, IndJobID: c.IndJobID, IndStartedAt: c.IndStartedAt},
	}, now)
}

// tradeRunning is the filter tradeActiveRuns applies to a pilot's state.
func (c botClaim) tradeRunning() bool {
	return c.Activity == ActTrade && c.TradeTypeID != 0 && c.TradeQty > 0
}

// installedJob is one (installer, product) count of manufacturing jobs still
// on a line or waiting for delivery.
type installedJob struct {
	InstallerID int64
	TypeID      int64
	N           int
}

// claimsBoard is one snapshot. Shared by every reader: never mutated after
// the loader returns it.
type claimsBoard struct {
	enabled map[int64]bool     // every enabled pilot that has a state row
	claims  map[int64]botClaim // the enabled pilots that claim something
	jobs    []installedJob
}

type claimNote struct {
	c  botClaim
	at time.Time
}

var (
	claimsMu      sync.Mutex
	claimsOverlay = map[int64]claimNote{}
)

// noteClaim records what saveState just wrote for a pilot.
func noteClaim(charID int64, c botClaim) {
	claimsMu.Lock()
	claimsOverlay[charID] = claimNote{c: c, at: clock.Now()}
	claimsMu.Unlock()
}

// forgetClaim drops a pilot's overlay entry; called beside forgetSavedState
// wherever a state row is deleted.
func forgetClaim(charID int64) {
	claimsMu.Lock()
	delete(claimsOverlay, charID)
	claimsMu.Unlock()
}

// sweepClaims drops overlay entries the snapshot has had time to catch up on.
func sweepClaims() {
	cut := clock.Now().Add(-claimOverlayLife)
	claimsMu.Lock()
	for id, n := range claimsOverlay {
		if n.at.Before(cut) {
			delete(claimsOverlay, id)
		}
	}
	claimsMu.Unlock()
}

// forEachClaim calls fn for every enabled pilot's current claim: the
// overlay's where it has one, the snapshot's otherwise. fn runs under
// claimsMu and must not call back into this file.
func forEachClaim(board *claimsBoard, fn func(charID int64, c botClaim)) {
	cut := clock.Now().Add(-claimOverlayLife)
	claimsMu.Lock()
	defer claimsMu.Unlock()
	for id, n := range claimsOverlay {
		if n.at.Before(cut) || !board.enabled[id] {
			continue
		}
		fn(id, n.c)
	}
	for id, c := range board.claims {
		if n, ok := claimsOverlay[id]; ok && !n.at.Before(cut) {
			continue // the overlay already answered for this pilot
		}
		fn(id, c)
	}
}

// emptyClaimsBoard is what a failed read answers with: nobody is doing anything.
var emptyClaimsBoard = &claimsBoard{enabled: map[int64]bool{}, claims: map[int64]botClaim{}}

// claimsBoardNow returns the shared snapshot, reading it if it has expired.
func (m *Mod) claimsBoardNow(ctx context.Context) *claimsBoard {
	b, err := snapshotFor("claims-board", claimsTTL, func() (*claimsBoard, error) {
		rctx, cancel := snapContext(ctx)
		defer cancel()
		return loadClaimsBoard(rctx, m.db)
	})
	if err != nil || b == nil {
		return emptyClaimsBoard
	}
	return b
}

func loadClaimsBoard(ctx context.Context, db *sql.DB) (*claimsBoard, error) {
	board := &claimsBoard{enabled: map[int64]bool{}, claims: map[int64]botClaim{}}
	rows, err := db.QueryContext(ctx, capStatement(`
		SELECT s.characterID, COALESCE(s.activity, ''), s.halted, s.data FROM rolebot_state s
		JOIN rolebot_characters b ON b.characterID = s.characterID AND b.enabled = 1`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			charID   int64
			activity string
			halted   bool
			blob     sql.NullString
		)
		if rows.Scan(&charID, &activity, &halted, &blob) != nil {
			continue
		}
		board.enabled[charID] = true
		if !blob.Valid || blob.String == "" {
			continue
		}
		var d StateData
		if json.Unmarshal([]byte(blob.String), &d) != nil {
			continue
		}
		if c := claimFromState(activity, halted, &d); c.claimsAnything() {
			board.claims[charID] = c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	jobs, err := db.QueryContext(ctx, capStatement(`
		SELECT installerID, outputTypeID, COUNT(*) FROM ramJobs
		WHERE activityID = 1 AND completedStatusID IN (0, 1)
		GROUP BY installerID, outputTypeID`))
	if err != nil {
		return nil, err
	}
	defer jobs.Close()
	for jobs.Next() {
		var j installedJob
		if jobs.Scan(&j.InstallerID, &j.TypeID, &j.N) != nil {
			continue
		}
		board.jobs = append(board.jobs, j)
	}
	return board, jobs.Err()
}

func init() { registerSweep("claims", sweepClaims) }