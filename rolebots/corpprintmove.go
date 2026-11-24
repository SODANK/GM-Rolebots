package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// Print moves: carrying a blueprint from one of the corporation's bases to another. A corporation with
// more than one R&D base (corpexpand.go) splits its library, because research runs where the original
// is (corplibplan.go). The Director of R&D's desk decides what goes where (planLibrarySplit).
// An original is locked in its office (corpvault.go): only a Director or the CEO carries one, and the
// lock comes off for the trip and back on at the other end. A copy made at an R&D base goes to the
// Industry base whose build order runs on it (planCopyMoves); any member posted at its base carries it.
// Bots carry by default (TaskIndPrintMove). A capsuleer may claim a move first from the R&D page's
// "Carry this" link. The deposit into the destination office (esp.OfficeDepositHook) is the proof: the
// print is re-locked and the carrier paid. A claim holds the bots off for printMoveHoldSec.
// Pay is per jump, capped at a share of free capital (printMoveWage).

const (
	moveOriginal = "original"
	moveCopy     = "copy"

	moveOpen     = "open"
	moveClaimed  = "claimed"
	moveCarrying = "carrying"
	moveDone     = "done"
	moveVoid     = "void"

	// printMoveFreeShare caps one move's pay at this share of free capital.
	printMoveFreeShare = 0.01
	// printMoveOpenMax / printCopyMoveOpenMax bound the moves a corporation
	// has open at once, originals and copies.
	printMoveOpenMax     = 6
	printCopyMoveOpenMax = 8
	// splitTolerance: two bases within this share of each other's load are
	// balanced.
	splitTolerance = 0.2
	// printMoveBotClaimSec is how long a bot's claim on a move holds before
	// another carrier may take it, while nothing has been loaded.
	printMoveBotClaimSec = 6 * 3600
)

// printMove is a rolebot_corp_print_moves row.
type printMove struct {
	MoveID, CorpID, ItemID, BPTypeID int64
	Kind                             string
	From, To                         int64
	Reason                           string
	Status                           string
	CarrierID                        int64
	Human                            bool
	Jumps                            int
	Wage                             float64
	LibSource                        string
	LibCost                          float64
	LibDonor                         int64
	PostedAt, ClaimedAt, DoneAt      int64
	Note                             string
}

const printMoveCols = `moveID, corporationID, itemID, bpTypeID, kind, fromStationID, toStationID, reason,
	status, carrierID, human, jumps, wage, libSource, libCost, libDonor, postedAt, claimedAt, doneAt, note`

func scanPrintMove(row interface{ Scan(...any) error }) (*printMove, error) {
	var mv printMove
	var human int
	if err := row.Scan(&mv.MoveID, &mv.CorpID, &mv.ItemID, &mv.BPTypeID, &mv.Kind, &mv.From, &mv.To, &mv.Reason,
		&mv.Status, &mv.CarrierID, &human, &mv.Jumps, &mv.Wage, &mv.LibSource, &mv.LibCost, &mv.LibDonor,
		&mv.PostedAt, &mv.ClaimedAt, &mv.DoneAt, &mv.Note); err != nil {
		return nil, err
	}
	mv.Human = human != 0
	return &mv, nil
}

// loadPrintMove reads one move, nil when there is none.
func (m *Mod) loadPrintMove(ctx context.Context, moveID int64) (*printMove, error) {
	mv, err := scanPrintMove(m.db.QueryRowContext(ctx,
		`SELECT `+printMoveCols+` FROM rolebot_corp_print_moves WHERE moveID = ?`, moveID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return mv, err
}

// printMovesWhere lists moves.
func (m *Mod) printMovesWhere(ctx context.Context, where string, args ...any) ([]printMove, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+printMoveCols+` FROM rolebot_corp_print_moves `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("printMoves: %w", err)
	}
	defer rows.Close()
	var out []printMove
	for rows.Next() {
		mv, err := scanPrintMove(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *mv)
	}
	return out, rows.Err()
}

// liveMoveStatuses are the statuses of a move still under way.
const liveMoveStatuses = `('open', 'claimed', 'carrying')`

// movingItems is every item a live move holds, for the corporation.
func (m *Mod) movingItems(ctx context.Context, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	ids, err := int64Column(ctx, m, `
		SELECT itemID FROM rolebot_corp_print_moves
		 WHERE corporationID = ? AND status IN `+liveMoveStatuses, corpID)
	if err == nil {
		for _, id := range ids {
			out[id] = true
		}
	}
	return out
}

// printMoveWage is one move's pay: perJump a jump, at least one jump, capped
// at printMoveFreeShare of the corporation's free capital but never below one
// jump's pay. Pure.
func printMoveWage(perJump float64, jumps int, free float64) float64 {
	w := perJump * float64(max(1, jumps))
	limit := math.Max(perJump, printMoveFreeShare*free)
	return math.Floor(math.Min(w, limit))
}

// ── the split (pure) ─────────────────────────────────────────────────────────

// splitPrint is one original as the split sees it.
type splitPrint struct {
	ItemID, BPTypeID, Station int64
	Backlog                   int  // open research and copy runs pinned to it
	Movable                   bool // not in a job, no claimed work, not already moving
}

// splitMove is one original the split sends.
type splitMove struct {
	ItemID, BPTypeID, From, To int64
	Backlog                    int
}

// splitWeight is what a print adds to its base's load: itself, and the work
// queued on it.
func splitWeight(p splitPrint) int { return 1 + p.Backlog }

// planLibrarySplit balances the library over the R&D bases. Originals at an office that is no R&D
// base go to the lightest base. Then, while the heaviest outweighs the lightest by more than
// splitTolerance, its movable print with the most queued work that narrows the gap goes to the
// lightest. At most limit moves. Pure.
func planLibrarySplit(prints []splitPrint, bases []int64, limit int) []splitMove {
	if len(bases) == 0 || limit <= 0 {
		return nil
	}
	isBase := map[int64]bool{}
	load := map[int64]int{}
	for _, st := range bases {
		isBase[st] = true
		load[st] = 0
	}
	for _, p := range prints {
		if isBase[p.Station] {
			load[p.Station] += splitWeight(p)
		}
	}
	lightest := func() int64 {
		var best int64
		for _, st := range bases {
			if best == 0 || load[st] < load[best] || (load[st] == load[best] && st < best) {
				best = st
			}
		}
		return best
	}
	heaviest := func() int64 {
		var best int64
		for _, st := range bases {
			if best == 0 || load[st] > load[best] || (load[st] == load[best] && st < best) {
				best = st
			}
		}
		return best
	}
	byWeight := append([]splitPrint(nil), prints...)
	sort.SliceStable(byWeight, func(i, j int) bool {
		wi, wj := splitWeight(byWeight[i]), splitWeight(byWeight[j])
		if wi != wj {
			return wi > wj
		}
		return byWeight[i].ItemID < byWeight[j].ItemID
	})
	var out []splitMove
	moved := map[int64]bool{}
	// Strays first: an original nowhere it can be researched.
	for _, p := range byWeight {
		if len(out) >= limit {
			return out
		}
		if isBase[p.Station] || !p.Movable {
			continue
		}
		to := lightest()
		out = append(out, splitMove{ItemID: p.ItemID, BPTypeID: p.BPTypeID, From: p.Station, To: to, Backlog: p.Backlog})
		load[to] += splitWeight(p)
		moved[p.ItemID] = true
	}
	if len(bases) < 2 {
		return out
	}
	for len(out) < limit {
		hi, lo := heaviest(), lightest()
		if hi == lo {
			break
		}
		gap := load[hi] - load[lo]
		if float64(gap) <= splitTolerance*float64(load[hi]) {
			break
		}
		var pick *splitPrint
		for i := range byWeight {
			p := &byWeight[i]
			if p.Station != hi || !p.Movable || moved[p.ItemID] {
				continue
			}
			if splitWeight(*p) < gap { // narrows the gap without flipping it
				pick = p
				break
			}
		}
		if pick == nil {
			break
		}
		w := splitWeight(*pick)
		out = append(out, splitMove{ItemID: pick.ItemID, BPTypeID: pick.BPTypeID, From: hi, To: lo, Backlog: pick.Backlog})
		load[hi] -= w
		load[lo] += w
		moved[pick.ItemID] = true
	}
	return out
}

// ── the desk ────────────────────────────────────────────────────────────────

// printMoveDesk is the Director of R&D's desk, run from the production pass:
// settle what is overdue, split the library over the R&D bases, and send each
// Industry base the copies its builders are waiting on.
func (m *Mod) printMoveDesk(ctx context.Context, c *corpCEO, ch Charter, factory int64, labBases []int64) {
	m.sweepPrintMoves(ctx, c)
	if !printMovesOn() {
		return
	}
	moving := m.movingItems(ctx, c.CorpID)
	if c.Origin == ceoOriginHuman {
		// A human chair's originals stay where it keeps them unless it orders one moved, and a print
		// under the chair's own order is not taken from under it.
		if ws, err := m.queryWork(ctx, `WHERE corporationID = ? AND status = 'open' AND bpItemID <> 0 AND source = 'human'`, c.CorpID); err == nil {
			for _, w := range ws {
				moving[w.BPItemID] = true
			}
		}
	} else {
		m.planOriginalMoves(ctx, c, ch, labBases, moving)
	}
	if factory != 0 {
		m.planCopyMoves(ctx, c, ch, factory, moving)
	}
}

// planOriginalMoves posts the split's moves.
func (m *Mod) planOriginalMoves(ctx context.Context, c *corpCEO, ch Charter, labBases []int64, moving map[int64]bool) {
	if len(labBases) == 0 {
		return
	}
	open := 0
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_print_moves
		 WHERE corporationID = ? AND kind = ? AND status IN `+liveMoveStatuses, c.CorpID, moveOriginal).Scan(&open)
	if open >= printMoveOpenMax {
		return
	}
	prints, err := m.splitPrints(ctx, c.CorpID, moving)
	if err != nil {
		m.logBot(c.CharID, logError, "ceo", "library split: %v", err)
		return
	}
	// With one base, only strays move; with none of them, nothing.
	for _, mv := range planLibrarySplit(prints, labBases, printMoveOpenMax-open) {
		why := "the library is split over the corporation's research bases"
		if !containsInt64(labBases, mv.From) {
			why = "research runs where the original is, and this office has no labs"
		}
		m.postPrintMove(ctx, c, ch, mv.ItemID, mv.BPTypeID, moveOriginal, mv.From, mv.To, why)
	}
}

// containsInt64 reports whether xs holds x. Pure.
func containsInt64(xs []int64, x int64) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// splitPrints reads every original in the corporation's offices with the work
// queued on it and whether it may move.
func (m *Mod) splitPrints(ctx context.Context, corpID int64, moving map[int64]bool) ([]splitPrint, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, o.stationID, e.flag
		  FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		  LEFT JOIN rolebot_corp_offices r ON r.corporationID = o.corporationID AND r.stationID = o.stationID
		 WHERE o.corporationID = ? AND e.flag IN (`+corpLibraryDivisions+`) AND COALESCE(b.copy, 0) = 0
		   AND COALESCE(r.state, 'open') <> 'dropping'`, corpID)
	if err != nil {
		return nil, fmt.Errorf("splitPrints: %w", err)
	}
	var out []splitPrint
	for rows.Next() {
		var p splitPrint
		var flag int64
		if rows.Scan(&p.ItemID, &p.BPTypeID, &p.Station, &flag) == nil {
			p.Movable = flag != 111 && !moving[p.ItemID]
			out = append(out, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	backlog := map[int64]int{}
	claimed := map[int64]bool{}
	wr, err := m.db.QueryContext(ctx, `
		SELECT w.bpItemID, GREATEST(w.runsWanted - w.runsDone, 0),
		       EXISTS (SELECT 1 FROM rolebot_corp_work_claims k WHERE k.workID = w.workID)
		  FROM rolebot_corp_work w
		 WHERE w.corporationID = ? AND w.status = 'open' AND w.bpItemID <> 0`, corpID)
	if err == nil {
		for wr.Next() {
			var item int64
			var left, cl int
			if wr.Scan(&item, &left, &cl) == nil {
				backlog[item] += left
				if cl != 0 {
					claimed[item] = true
				}
			}
		}
		wr.Close()
	}
	for i := range out {
		out[i].Backlog = backlog[out[i].ItemID]
		if claimed[out[i].ItemID] {
			out[i].Movable = false
		}
	}
	return out, nil
}

// copyNeed is one (Industry base, print type) with a build order open there.
type copyNeed struct{ Station, BPTypeID int64 }

// planCopyMoveList picks which copies to carry where: for every base whose build order has no copy of
// its type there (free, on a line, or inbound), the spare copy with the most runs, a spare being one at
// an office with no build order of its type. Neediest first, at most limit moves. copies must come
// most-runs-first per type. Pure.
func planCopyMoveList(want map[copyNeed]int, copies []libraryCopy, inbound map[copyNeed]int,
	moving map[int64]bool, limit int) []splitMove {
	have := map[copyNeed]int{}
	spare := map[int64][]libraryCopy{}
	for _, cp := range copies {
		k := copyNeed{cp.StationID, cp.BPTypeID}
		if moving[cp.ItemID] {
			continue // counted where it is going (inbound)
		}
		if _, needed := want[k]; needed || cp.Flag == 111 {
			have[k]++ // working where it is, or wanted there
			continue
		}
		spare[cp.BPTypeID] = append(spare[cp.BPTypeID], cp)
	}
	keys := make([]copyNeed, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if want[keys[i]] != want[keys[j]] {
			return want[keys[i]] > want[keys[j]]
		}
		if keys[i].Station != keys[j].Station {
			return keys[i].Station < keys[j].Station
		}
		return keys[i].BPTypeID < keys[j].BPTypeID
	})
	var out []splitMove
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		if have[k]+inbound[k] > 0 || len(spare[k.BPTypeID]) == 0 {
			continue
		}
		cp := spare[k.BPTypeID][0]
		spare[k.BPTypeID] = spare[k.BPTypeID][1:]
		out = append(out, splitMove{ItemID: cp.ItemID, BPTypeID: k.BPTypeID, From: cp.StationID, To: k.Station})
	}
	return out
}

// planCopyMoves sends copies to the Industry base whose build order needs one
// (corpbuildbases.go) while a spare copy sits at another office.
func (m *Mod) planCopyMoves(ctx context.Context, c *corpCEO, ch Charter, factory int64, moving map[int64]bool) {
	open := 0
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_print_moves
		 WHERE corporationID = ? AND kind = ? AND reason NOT LIKE ? AND status IN `+liveMoveStatuses,
		c.CorpID, moveCopy, printSaleMoveReason+"%").Scan(&open)
	if open >= printCopyMoveOpenMax {
		return
	}
	work, err := m.openWorkFor(ctx, c.CorpID)
	if err != nil {
		return
	}
	want := map[copyNeed]int{}
	for _, w := range work {
		if w.Kind == workKindBuild && w.RunsLeft() > 0 {
			want[copyNeed{orderStation(w, factory), w.BPTypeID}] += w.RunsLeft()
		}
	}
	if len(want) == 0 {
		return
	}
	copies, err := m.libraryCopiesAll(ctx, c.CorpID)
	if err != nil {
		return
	}
	inbound := map[copyNeed]int{}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT toStationID, bpTypeID, COUNT(*) FROM rolebot_corp_print_moves
		 WHERE corporationID = ? AND kind = ? AND status IN `+liveMoveStatuses+`
		 GROUP BY toStationID, bpTypeID`, c.CorpID, moveCopy); err == nil {
		for rows.Next() {
			var k copyNeed
			var n int
			if rows.Scan(&k.Station, &k.BPTypeID, &n) == nil {
				inbound[k] += n
			}
		}
		rows.Close()
	}
	for _, mv := range planCopyMoveList(want, copies, inbound, moving, printCopyMoveOpenMax-open) {
		why := "the factory's builders are waiting on a copy"
		if mv.To != factory {
			why = "the builders at " + stationNameOf(ctx, m, mv.To) + " are waiting on a copy"
		}
		m.postPrintMove(ctx, c, ch, mv.ItemID, mv.BPTypeID, moveCopy, mv.From, mv.To, why)
	}
}

// postPrintMove opens one move, voiding the work pinned to the print that
// nobody holds (it is re-posted where the print lands).
func (m *Mod) postPrintMove(ctx context.Context, c *corpCEO, ch Charter, itemID, bpTypeID int64, kind string,
	from, to int64, why string) bool {
	if from == to || from == 0 || to == 0 {
		return false
	}
	var live int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_print_moves WHERE itemID = ? AND status IN `+liveMoveStatuses, itemID).Scan(&live)
	if live > 0 {
		return false
	}
	jumps := 1
	if dist, err := m.corpJumpMap(ctx, m.stationSystemID(ctx, from)); err == nil {
		if d, ok := dist[m.stationSystemID(ctx, to)]; ok {
			jumps = d
		}
	}
	free, _, _ := m.corpFreeCapital(ctx, c.CorpID, ch)
	wage := printMoveWage(printMoveWagePerJump(), jumps, free)
	var src string
	var cost float64
	var donor int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT source, cost, donorID FROM rolebot_corp_library WHERE itemID = ?`, itemID).Scan(&src, &cost, &donor)
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_print_moves (corporationID, itemID, bpTypeID, kind, fromStationID, toStationID,
			reason, jumps, wage, libSource, libCost, libDonor, postedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CorpID, itemID, bpTypeID, kind, from, to, clampText(why, 255), jumps, wage, src, cost, donor, clock.Unix())
	if err != nil {
		m.logBot(c.CharID, logError, "ceo", "post print move: %v", err)
		return false
	}
	id, _ := res.LastInsertId()
	if err := m.openCommit(ctx, c.CorpID, "printmove", id, wage); err != nil {
		rlogf("rolebots: %v", err)
	}
	// Work pinned to the print that nobody has started cannot be done where
	// it was posted any more.
	if ws, err := m.queryWork(ctx, `WHERE corporationID = ? AND status = 'open' AND bpItemID = ?`, c.CorpID, itemID); err == nil {
		for _, w := range ws {
			// The chair's own order stands until the chair withdraws it.
			if w.Source != fleetOrderHuman && !m.workHasClaims(ctx, w.WorkID) {
				m.closeWork(ctx, w.WorkID, workVoid)
			}
		}
	}
	what := "an original"
	if kind == moveCopy {
		what = "a copy"
	}
	m.ceoLog(ctx, c, powerProduction, "printmove", bpTypeID, wage,
		"wants %s of %s carried from %s to %s (%d jump(s), %s) — %s", what, m.typeNameOr(ctx, bpTypeID),
		stationNameOf(ctx, m, from), stationNameOf(ctx, m, to), jumps, iskString(wage), why)
	return true
}

// sweepPrintMoves settles what nobody will finish: a print gone from where it
// waited, a capsuleer's claim past its hold, a bot's empty-handed claim gone
// stale.
func (m *Mod) sweepPrintMoves(ctx context.Context, c *corpCEO) {
	moves, err := m.printMovesWhere(ctx, `WHERE corporationID = ? AND status IN `+liveMoveStatuses, c.CorpID)
	if err != nil {
		return
	}
	now := clock.Unix()
	hold := printMoveHoldSec()
	for i := range moves {
		mv := &moves[i]
		var owner, loc, office int64
		err := m.db.QueryRowContext(ctx, `
			SELECT e.ownerID, e.locationID, COALESCE(o.stationID, 0)
			  FROM entity e LEFT JOIN crpoffices o ON o.itemID = e.locationID AND o.corporationID = e.ownerID
			 WHERE e.itemID = ?`, mv.ItemID).Scan(&owner, &loc, &office)
		gone := errors.Is(err, sql.ErrNoRows)
		atFrom := err == nil && owner == c.CorpID && office == mv.From
		switch {
		case gone:
			m.closePrintMove(ctx, mv, moveVoid, "the print no longer exists")
			m.ceoLog(ctx, c, powerProduction, "printmove.lost", mv.BPTypeID, 0,
				"lost a %s of %s on its way to %s", mv.Kind, m.typeNameOr(ctx, mv.BPTypeID), stationNameOf(ctx, m, mv.To))
		case err == nil && owner == c.CorpID && office == mv.To:
			// Somebody put it there by hand.
			m.closePrintMove(ctx, mv, moveDone, "found at its destination")
		case mv.Status == moveOpen && !atFrom:
			m.closePrintMove(ctx, mv, moveVoid, "the print left the office it was waiting in")
		case mv.Status == moveClaimed && mv.Human && now-mv.ClaimedAt > hold && atFrom:
			// A capsuleer said yes and never came: the lock goes back on and
			// the bots may have it.
			if mv.Kind == moveOriginal {
				_, _ = m.lockPrint(ctx, c.CorpID, mv.ItemID)
			}
			m.reopenPrintMove(ctx, mv)
		case mv.Status == moveClaimed && mv.Human && now-mv.ClaimedAt > 2*hold && !atFrom &&
			owner == mv.CarrierID && mv.Note == "":
			// Taken out of the office and not delivered.
			m.notePrintMove(ctx, mv, "overdue")
			m.ceoLog(ctx, c, powerProduction, "printmove.late", mv.CarrierID, 0,
				"%s took a %s of %s out of %s %s ago and has not delivered it to %s", m.charNameOr(ctx, mv.CarrierID),
				mv.Kind, m.typeNameOr(ctx, mv.BPTypeID), stationNameOf(ctx, m, mv.From), fmtDuration(now-mv.ClaimedAt),
				stationNameOf(ctx, m, mv.To))
			m.mailPilot(ctx, mv.CarrierID, c.CharID, "The blueprint you are carrying", fmt.Sprintf(
				"You took %s out of the corporation's office at %s to carry it to %s. "+
					"Put it into the corporation hangar there (any division) and you are paid %s.<br>-- %s",
				esp.IGBText(m.typeNameOr(ctx, mv.BPTypeID)), esp.IGBText(stationNameOf(ctx, m, mv.From)),
				esp.IGBText(stationNameOf(ctx, m, mv.To)), iskString(mv.Wage), esp.IGBText(m.charNameOr(ctx, c.CharID))))
		case mv.Status == moveClaimed && !mv.Human && now-mv.ClaimedAt > printMoveBotClaimSec && atFrom:
			m.reopenPrintMove(ctx, mv)
		}
	}
}

// closePrintMove ends a move and releases its commitment.
func (m *Mod) closePrintMove(ctx context.Context, mv *printMove, status, note string) {
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_print_moves SET status = ?, doneAt = ?, note = ? WHERE moveID = ?`,
		status, clock.Unix(), clampText(note, 255), mv.MoveID); err != nil {
		rlogf("rolebots: close print move %d: %v", mv.MoveID, err)
	}
	mv.Status = status
	m.closeCommit(ctx, "printmove", mv.MoveID)
}

// reopenPrintMove hands a move back to whoever will carry it.
func (m *Mod) reopenPrintMove(ctx context.Context, mv *printMove) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_print_moves SET status = 'open', carrierID = 0, human = 0, claimedAt = 0, note = ''
		 WHERE moveID = ? AND status = 'claimed'`, mv.MoveID)
	mv.Status, mv.CarrierID, mv.Human = moveOpen, 0, false
}

// notePrintMove stamps the note column (a once-only flag for the sweep).
func (m *Mod) notePrintMove(ctx context.Context, mv *printMove, note string) {
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_print_moves SET note = ? WHERE moveID = ?`, note, mv.MoveID)
	mv.Note = note
}

// mayCarry reports whether a member may carry a move: a Director or the CEO
// carries anything; a copy also goes with any member posted at its base.
func (m *Mod) mayCarry(ctx context.Context, mv *printMove, charID int64) bool {
	if m.isCorpOfficer(ctx, mv.CorpID, charID) {
		return true
	}
	if mv.Kind != moveCopy {
		return false
	}
	var corpID, base int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT c.corporationID, COALESCE(r.baseID, 0) FROM character_ c
		  LEFT JOIN chrCorporationRoles r ON r.characterID = c.characterID
		 WHERE c.characterID = ?`, charID).Scan(&corpID, &base)
	return corpID == mv.CorpID && base == mv.From
}

// settlePrintMove finishes a delivered move: provenance back on the library
// row, the lock back on an original, the carrier paid.
func (m *Mod) settlePrintMove(ctx context.Context, mv *printMove, carrier int64) {
	if mv.Kind == moveOriginal {
		if _, err := m.lockPrint(ctx, mv.CorpID, mv.ItemID); err != nil {
			rlogf("rolebots: relock moved print %d: %v", mv.ItemID, err)
		}
		if mv.LibSource != "" {
			now := clock.Unix()
			_, _ = m.db.ExecContext(ctx, `
				INSERT INTO rolebot_corp_library (itemID, corporationID, bpTypeID, stationID, source, cost, donorID,
					acquiredAt, lastSeenAt)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE stationID = VALUES(stationID), source = VALUES(source), cost = VALUES(cost),
					donorID = VALUES(donorID), lastSeenAt = VALUES(lastSeenAt)`,
				mv.ItemID, mv.CorpID, mv.BPTypeID, mv.To, mv.LibSource, mv.LibCost, mv.LibDonor, now, now)
		}
	}
	paid := false
	if carrier != 0 && mv.Wage >= 1 {
		paid = m.payCorpWage(ctx, mv.CorpID, carrier, mv.Wage,
			fmt.Sprintf("carrying %s to %s", m.typeNameOr(ctx, mv.BPTypeID), stationNameOf(ctx, m, mv.To)))
		if paid {
			m.spendCommit(ctx, "printmove", mv.MoveID, mv.Wage)
		}
	}
	m.closePrintMove(ctx, mv, moveDone, map[bool]string{true: "delivered and paid", false: "delivered"}[paid])
	if c, _ := getCEO(ctx, m.db, mv.CorpID); c != nil {
		m.ceoLog(ctx, c, powerProduction, "printmove.done", mv.BPTypeID, mv.Wage,
			"%s carried a %s of %s from %s to %s", m.charNameOr(ctx, carrier), mv.Kind,
			m.typeNameOr(ctx, mv.BPTypeID), stationNameOf(ctx, m, mv.From), stationNameOf(ctx, m, mv.To))
	}
}

// ── a capsuleer carries ─────────────────────────────────────────────────────

var errMoveGone = errors.New("that move is no longer open")

// humanClaimPrintMove is the click on the signed link: the move is the
// capsuleer's, and an original is unlocked so the client lets it out.
func (m *Mod) humanClaimPrintMove(ctx context.Context, moveID, charID int64) (*printMove, error) {
	mv, err := m.loadPrintMove(ctx, moveID)
	if err != nil {
		return nil, err
	}
	if mv == nil || mv.Status != moveOpen {
		return nil, errMoveGone
	}
	if !m.mayCarry(ctx, mv, charID) {
		if mv.Kind == moveOriginal {
			return nil, errors.New("only a Director or the Chief Executive carries an original")
		}
		return nil, errors.New("a copy goes with a member posted at the office it leaves")
	}
	if m.lockedByVote(ctx, mv.CorpID, mv.ItemID) {
		return nil, errors.New("the print is locked down")
	}
	now := clock.Unix()
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_print_moves SET status = 'claimed', carrierID = ?, human = 1, claimedAt = ?, note = ''
		 WHERE moveID = ? AND status = 'open'`, charID, now, moveID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, errMoveGone
	}
	mv.Status, mv.CarrierID, mv.Human, mv.ClaimedAt = moveClaimed, charID, true, now
	if mv.Kind == moveOriginal {
		if _, err := m.unlockPrint(ctx, mv.CorpID, mv.ItemID); err != nil {
			rlogf("rolebots: unlock print %d for a carrier: %v", mv.ItemID, err)
		}
	}
	return mv, nil
}

// noteMoveDeposit is the office-deposit hook's first question: is this a print on a move? True means
// the deposit was a move's and is settled here, not a donation.
func (m *Mod) noteMoveDeposit(ctx context.Context, corpID, charID, itemID int64) bool {
	moves, err := m.printMovesWhere(ctx, `WHERE corporationID = ? AND itemID = ? AND status IN `+liveMoveStatuses+`
		ORDER BY moveID DESC LIMIT 1`, corpID, itemID)
	if err != nil || len(moves) == 0 {
		return false
	}
	mv := &moves[0]
	var station int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT o.stationID FROM entity e JOIN crpoffices o ON o.itemID = e.locationID
		 WHERE e.itemID = ?`, itemID).Scan(&station)
	switch station {
	case mv.To:
		carrier := charID
		if mv.CarrierID != 0 && mv.CarrierID != charID {
			carrier = mv.CarrierID // a bot's load handed in by somebody else: pay who claimed it
		}
		m.settlePrintMove(ctx, mv, carrier)
	case mv.From:
		// Put back where it was: the move stands for somebody else.
		if mv.Kind == moveOriginal {
			_, _ = m.lockPrint(ctx, corpID, itemID)
		}
		m.reopenPrintMove(ctx, mv)
	default:
		m.closePrintMove(ctx, mv, moveVoid, "delivered to another office")
	}
	return true
}

// ── a bot carries (TaskIndPrintMove) ─────────────────────────────────────────

// printMoveDue is the planner's pre-emption: a member who may carry an open
// move near it, with no project in hand, takes it. A pilot already holding a
// move finishes it first.
func (m *Mod) printMoveDue(ctx context.Context, b *botRun) (bool, error) {
	if b == nil || b.CorpID == 0 || b.State == nil {
		return false, nil
	}
	if !corpDeskRun(b.CorpID) || !printMovesOn() {
		return false, nil
	}
	if b.State.Data.IndPrintMove != 0 {
		b.State.Phase = string(TaskIndPrintMove)
		return true, m.printMoveStep(ctx, b)
	}
	if b.State.Data.IndTypeID != 0 {
		return false, nil
	}
	now := clock.Unix()
	// Which corporations have a move posted at all, shared for a minute:
	// every managed member read its corporation's board per plan.
	posted, _ := snapshotFor(snapKey("print-moves-open"), time.Minute, func() (map[int64]bool, error) {
		return m.corpIDsWhere(ctx, `SELECT DISTINCT corporationID FROM rolebot_corp_print_moves WHERE status IN ('open', 'claimed')`)
	})
	if !posted[b.CorpID] {
		return false, nil
	}
	moves, err := m.printMovesWhere(ctx, `
		WHERE corporationID = ? AND (status = 'open' OR (status = 'claimed' AND human = 0 AND claimedAt < ?))
		ORDER BY kind = 'original' DESC, postedAt LIMIT 20`, b.CorpID, now-printMoveBotClaimSec)
	if err != nil || len(moves) == 0 {
		return false, err
	}
	for i := range moves {
		mv := &moves[i]
		if !m.mayCarry(ctx, mv, b.CharID) {
			continue
		}
		fromSys := m.stationSystemID(ctx, mv.From)
		if b.StationID != mv.From {
			if j := m.jumpsTo(ctx, b, fromSys); j < 0 || j > corpWorkMaxJumps {
				continue
			}
		}
		res, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_print_moves SET status = 'claimed', carrierID = ?, human = 0, claimedAt = ?
			 WHERE moveID = ? AND (status = 'open' OR (status = 'claimed' AND human = 0 AND claimedAt < ?))`,
			b.CharID, now, mv.MoveID, now-printMoveBotClaimSec)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		st := b.State
		st.Activity = ActIndustry
		st.Data = idleData(st.Data)
		d := &st.Data
		d.IndPrintMove = mv.MoveID
		d.IndTypeID = mv.BPTypeID
		d.IndTypeName = m.typeNameOr(ctx, mv.BPTypeID)
		d.IndFactoryID, d.IndFactorySysID = mv.To, m.stationSystemID(ctx, mv.To)
		d.IndSellerStationID, d.IndSellerSysID = mv.From, fromSys
		d.IndStartedAt = now
		m.logBot(b.CharID, logInfo, "industry", "carrying a %s of %s from %s to %s for its corporation",
			mv.Kind, d.IndTypeName, m.stationLabel(ctx, mv.From), m.stationLabel(ctx, mv.To))
		return true, m.beginTravel(ctx, b, fromSys, mv.From, string(TaskIndPrintMove))
	}
	return false, nil
}

// printMoveStep is the TaskIndPrintMove phase: at the base it leaves, load;
// at the base it goes to, deliver.
func (m *Mod) printMoveStep(ctx context.Context, b *botRun) error {
	d := &b.State.Data
	finish := func(note string) error {
		if note != "" {
			m.logBot(b.CharID, logInfo, "industry", "%s", note)
		}
		m.industryClearProject(ctx, b, false)
		b.State.Activity, b.State.Phase = "", string(TaskIdle)
		b.State.Data = idleData(b.State.Data)
		return saveState(ctx, m.db, b.State)
	}
	mv, err := m.loadPrintMove(ctx, d.IndPrintMove)
	if err != nil {
		return err
	}
	if mv == nil || mv.CarrierID != b.CharID || (mv.Status != moveClaimed && mv.Status != moveCarrying) {
		return finish("the print it was to carry was moved without it")
	}
	if mv.Status == moveClaimed {
		// ── loading ──
		if b.StationID != mv.From {
			return m.beginTravel(ctx, b, m.stationSystemID(ctx, mv.From), mv.From, string(TaskIndPrintMove))
		}
		fromOffice, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, mv.CorpID, mv.From)
		if fromOffice == 0 || fromOffice == mv.From {
			m.closePrintMove(ctx, mv, moveVoid, "the corporation has no office there any more")
			return finish("found no corporation office to take the print from")
		}
		if _, ok := m.ensureRunabout(ctx, b, "carrying a print"); !ok {
			m.reopenPrintMove(ctx, mv)
			return finish("had no ship to carry its corporation's print in")
		}
		if m.lockedByVote(ctx, mv.CorpID, mv.ItemID) {
			m.closePrintMove(ctx, mv, moveVoid, "the print was locked down")
			return finish("found the print it came for locked down")
		}
		res, err := m.db.ExecContext(ctx, `
			UPDATE entity SET locationID = ?, flag = 5
			 WHERE itemID = ? AND ownerID = ? AND locationID = ? AND flag <> 111`,
			b.ShipID, mv.ItemID, mv.CorpID, fromOffice)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			m.closePrintMove(ctx, mv, moveVoid, "the print was not on the shelf when the carrier came")
			return finish("the print it came for was not on its corporation's shelf")
		}
		if mv.Kind == moveOriginal {
			// Moved first, then unlocked: in a hold it is no longer lockable,
			// so the keeper cannot re-lock it between.
			if _, err := m.unlockPrint(ctx, mv.CorpID, mv.ItemID); err != nil {
				m.logBot(b.CharID, logError, "industry", "print move: unlock %d: %v", mv.ItemID, err)
			}
		}
		if _, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_print_moves SET status = 'carrying' WHERE moveID = ?`, mv.MoveID); err != nil {
			return err
		}
		return m.beginTravel(ctx, b, m.stationSystemID(ctx, mv.To), mv.To, string(TaskIndPrintMove))
	}
	// ── delivering ──
	target := mv.To
	toOffice, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, mv.CorpID, target)
	if toOffice == 0 || toOffice == target {
		// The destination closed on the way: back where it came from.
		target = mv.From
		toOffice, _, _ = evedb.GetCorpOfficeAt(ctx, m.db, mv.CorpID, target)
	}
	if toOffice == 0 || toOffice == target {
		m.closePrintMove(ctx, mv, moveVoid, "neither office is the corporation's any more")
		return finish("had nowhere to put its corporation's print")
	}
	if b.StationID != target {
		return m.beginTravel(ctx, b, m.stationSystemID(ctx, target), target, string(TaskIndPrintMove))
	}
	var loc, shipOwner, shipAt int64
	err = m.db.QueryRowContext(ctx, `
		SELECT e.locationID, COALESCE(s.ownerID, 0), COALESCE(s.locationID, 0)
		  FROM entity e LEFT JOIN entity s ON s.itemID = e.locationID
		 WHERE e.itemID = ? AND e.ownerID = ? AND e.flag = 5`, mv.ItemID, mv.CorpID).Scan(&loc, &shipOwner, &shipAt)
	if err != nil || shipOwner != b.CharID || (loc != b.ShipID && shipAt != b.StationID) {
		m.closePrintMove(ctx, mv, moveVoid, "the print was lost on the way")
		return finish("lost its corporation's print on the way")
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = ? WHERE itemID = ? AND ownerID = ? AND locationID = ?`,
		toOffice, corpDivLibrary, mv.ItemID, mv.CorpID, loc); err != nil {
		return err
	}
	if target != mv.To {
		if mv.Kind == moveOriginal {
			_, _ = m.lockPrint(ctx, mv.CorpID, mv.ItemID)
		}
		m.closePrintMove(ctx, mv, moveVoid, "its destination closed on the way; it went back")
		return finish("took its corporation's print back — the office it was going to is gone")
	}
	m.settlePrintMove(ctx, mv, b.CharID)
	if strings.HasPrefix(mv.Reason, printSaleMoveReason) {
		// Carried to be sold here: the carrier is standing at the counter,
		// so it lists it (corpprintsale.go).
		m.corpPrintSaleChore(ctx, b, b.StationID, toOffice)
	}
	return finish(fmt.Sprintf("delivered a %s of %s to %s", mv.Kind, m.typeNameOr(ctx, mv.BPTypeID),
		m.stationLabel(ctx, mv.To)))
}
