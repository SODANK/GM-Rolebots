package rolebots

import (
	"context"
	"fmt"
	"math"
	"math/bits"
	"sort"

	evedb "gurimoon/db"
)

// The event half of the morale ledger (corpmorale.go): what each kind of
// employer did to its members since the last sample, read off tables other
// desks already write. Nothing here writes outside the ledger.
//
// Every corporation: deaths, fleet wins, fleet seats, books bought.
// A bot CEO's: payroll, SRP, merit rank, dividends.
// A human-run one: ISK paid to the member, roles granted or taken.
// An NPC chair's: the Director of Security appointment.

const (
	mwDeath      = 0.04
	mwDeathPod   = 0.03
	mwFleetWin   = -0.02
	mwSeated     = -0.10
	mwUnseated   = 0.06
	mwBook       = -0.08
	mwHead       = -0.10
	mwSkipped    = 0.12
	mwNoPayroll  = 0.04
	mwPaid       = -0.06
	mwSRPDenied  = 0.15
	mwSRPLapsed  = 0.20
	mwSRPGranted = -0.08
	mwPromoted   = -0.15
	mwDemoted    = 0.05
	mwDividend   = -0.05

	// moraleChequeWindowSec: the payroll books its cheques in one run that
	// starts at lastPayrollAt.
	moraleChequeWindowSec = 600
	moraleSRPLookbackSec  = 60 * 86400
)

// moraleEvents gathers every event and observation for one sample.
func (m *Mod) moraleEvents(ctx context.Context, cv moraleCorpView, members []moraleMember,
	cells map[int64]map[string]moraleCell, cur *moraleCursor, since, now int64) ([]moraleEvent, error) {
	if len(members) == 0 {
		return nil, nil
	}
	byID := map[int64]*moraleMember{}
	ids := make([]int64, len(members))
	for i := range members {
		ids[i] = members[i].CharID
		byID[ids[i]] = &members[i]
	}
	var out []moraleEvent
	add := func(evs []moraleEvent, err error) error {
		out = append(out, evs...)
		return err
	}
	if err := add(m.moraleDeaths(ctx, cv, ids, byID, since)); err != nil {
		return nil, err
	}
	if err := add(m.moraleFleetWins(ctx, cv.CorpID, ids, since)); err != nil {
		return nil, err
	}
	if err := add(m.moraleBooks(ctx, cv.CorpID, byID, since)); err != nil {
		return nil, err
	}
	if err := add(m.moraleSeats(ctx, cv.CorpID, ids, cells, now)); err != nil {
		return nil, err
	}
	switch {
	case cv.BotCEO:
		if err := add(m.moralePayroll(ctx, cv.CorpID, ids, cur)); err != nil {
			return nil, err
		}
		if err := add(m.moraleSRP(ctx, cv.CorpID, byID, now)); err != nil {
			return nil, err
		}
		if err := add(m.moraleMerit(ctx, cv.CorpID, byID, cells)); err != nil {
			return nil, err
		}
		if err := add(m.moraleDividends(ctx, cv.CorpID, byID, cells)); err != nil {
			return nil, err
		}
	case cv.Human:
		if err := add(m.moraleHumanPay(ctx, cv.CorpID, ids, since)); err != nil {
			return nil, err
		}
		out = append(out, moraleRoleEvents(members, cells)...)
	case cv.Chaired:
		if err := add(m.moraleHead(ctx, cv.CorpID, byID, cells)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// obsOf is a pilot's last observation of one kind, and whether there is one.
func obsOf(cells map[int64]map[string]moraleCell, charID int64, kind string) (float64, bool) {
	c, ok := cells[charID][kind]
	return c.Value, ok
}

func (m *Mod) moraleDeaths(ctx context.Context, cv moraleCorpView, ids []int64, byID map[int64]*moraleMember, since int64) ([]moraleEvent, error) {
	ph, args := inArgs(ids)
	rows, err := m.db.QueryContext(ctx, `
		SELECT killID, victimCharID, isPod FROM killmails
		 WHERE victimCharID IN (`+ph+`) AND victimCorpID = ? AND killTime >= ?`,
		append(args, cv.CorpID, since)...)
	if err != nil {
		return nil, fmt.Errorf("morale deaths: %w", err)
	}
	defer rows.Close()
	var out []moraleEvent
	for rows.Next() {
		var killID, charID int64
		var pod int
		if err := rows.Scan(&killID, &charID, &pod); err != nil {
			return nil, err
		}
		w := mwDeath
		if pod != 0 {
			w += mwDeathPod
		}
		if mb := byID[charID]; mb != nil {
			w *= bend(traitScale(mb.Traits[TraitCaution]), 1.4, 1, 0.7)
		}
		if cv.AtWar {
			w *= 1.5
		}
		out = append(out, moraleEvent{CharID: charID, Kind: mkDeath, SrcKey: fmt.Sprintf("kill:%d", killID),
			Weight: w, Detail: fmt.Sprintf("killmail %d", killID)})
	}
	return out, rows.Err()
}

func (m *Mod) moraleFleetWins(ctx context.Context, corpID int64, ids []int64, since int64) ([]moraleEvent, error) {
	ph, args := inArgs(ids)
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT a.charID, a.killID FROM killmail_attackers a
		  JOIN killmails k ON k.killID = a.killID
		 WHERE a.charID IN (`+ph+`) AND a.corpID = ? AND k.killTime >= ?`,
		append(args, corpID, since)...)
	if err != nil {
		return nil, fmt.Errorf("morale fleet wins: %w", err)
	}
	defer rows.Close()
	var out []moraleEvent
	for rows.Next() {
		var charID, killID int64
		if err := rows.Scan(&charID, &killID); err != nil {
			return nil, err
		}
		out = append(out, moraleEvent{CharID: charID, Kind: mkFleetWin, SrcKey: fmt.Sprintf("kill:%d", killID),
			Weight: mwFleetWin, Detail: fmt.Sprintf("killmail %d", killID)})
	}
	return out, rows.Err()
}

// moraleBooks: a skill book the corporation bought a member (corpBuysBook,
// npcChairBuysBook), read off the CEO ledger.
func (m *Mod) moraleBooks(ctx context.Context, corpID int64, byID map[int64]*moraleMember, since int64) ([]moraleEvent, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT actionID, subjectID, detail FROM rolebot_ceo_actions
		 WHERE corporationID = ? AND verb = 'skillbook' AND at >= ?`, corpID, since)
	if err != nil {
		return nil, fmt.Errorf("morale books: %w", err)
	}
	defer rows.Close()
	var out []moraleEvent
	for rows.Next() {
		var id, subj int64
		var detail string
		if err := rows.Scan(&id, &subj, &detail); err != nil {
			return nil, err
		}
		if byID[subj] == nil {
			continue
		}
		out = append(out, moraleEvent{CharID: subj, Kind: mkBook, SrcKey: fmt.Sprintf("act:%d", id),
			Weight: mwBook, Detail: detail})
	}
	return out, rows.Err()
}

// moraleSeats: a fleet-commander seat dealt or taken away (corprank.go), whoever dealt it: a bot CEO, an NPC
// chair or the weekly promotion pass.
func (m *Mod) moraleSeats(ctx context.Context, corpID int64, ids []int64, cells map[int64]map[string]moraleCell, now int64) ([]moraleEvent, error) {
	ranks, err := m.listCorpRanks(ctx, corpID)
	if err != nil {
		return nil, err
	}
	seated := map[int64]corpRankRow{}
	for _, r := range ranks {
		seated[r.CharID] = r
	}
	var out []moraleEvent
	for _, id := range ids {
		prev, seen := obsOf(cells, id, mkObsSeat)
		r, on := seated[id]
		v := 0.0
		if on {
			v = 1
		}
		switch {
		case on && (!seen || prev < 1):
			out = append(out, moraleEvent{CharID: id, Kind: mkSeated,
				SrcKey: fmt.Sprintf("seat:%s:%d", r.Activity, r.AppointedAt), Weight: mwSeated,
				Detail: "fleet seat: " + r.Activity})
		case !on && seen && prev >= 1:
			out = append(out, moraleEvent{CharID: id, Kind: mkUnseated,
				SrcKey: fmt.Sprintf("unseat:%d", now/86400), Weight: mwUnseated, Detail: "lost its fleet seat"})
		}
		out = append(out, moraleEvent{CharID: id, Kind: mkObsSeat, Weight: v})
	}
	return out, nil
}

// moralePayroll reads the week's payroll once it has run: who earned a share
// of it (labour credit or merit in the window), who was paid, and who was
// skipped while others were. The payroll row is written after every cheque
// (corppayroll.go), so a newer lastPayrollAt means the cheques are on file.
func (m *Mod) moralePayroll(ctx context.Context, corpID int64, ids []int64, cur *moraleCursor) ([]moraleEvent, error) {
	var lastAt int64
	var pot float64
	var paidN int
	err := m.db.QueryRowContext(ctx, `
		SELECT lastPayrollAt, lastPot, lastPaid FROM rolebot_corp_payroll WHERE corporationID = ?`,
		corpID).Scan(&lastAt, &pot, &paidN)
	if err != nil || lastAt <= cur.LastPayrollSeen {
		return nil, nil
	}
	from := cur.LastPayrollSeen
	cur.LastPayrollSeen = lastAt
	if from == 0 {
		return nil, nil // the window before the ledger is unknown
	}
	ph, args := inArgs(ids)
	earned := map[int64]bool{}
	for _, q := range []string{
		`SELECT characterID FROM rolebot_corp_labour WHERE characterID IN (` + ph + `) AND corporationID = ? AND at >= ? AND at < ? AND credit > 0`,
		`SELECT characterID FROM rolebot_corp_merit_log WHERE characterID IN (` + ph + `) AND corporationID = ? AND at >= ? AND at < ? AND points > 0`,
	} {
		rows, err := m.db.QueryContext(ctx, q, append(append([]any{}, args...), corpID, from, lastAt)...)
		if err != nil {
			return nil, fmt.Errorf("morale payroll window: %w", err)
		}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				earned[id] = true
			}
		}
		rows.Close()
	}
	cheques := map[int64]float64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT counterID, SUM(-amount) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND line = ? AND at >= ? AND at < ? AND counterID IN (`+ph+`)
		 GROUP BY counterID`,
		append([]any{corpID, linePayroll, lastAt, lastAt + moraleChequeWindowSec}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("morale cheques: %w", err)
	}
	for rows.Next() {
		var id int64
		var amt float64
		if rows.Scan(&id, &amt) == nil && amt > 0 {
			cheques[id] = amt
		}
	}
	rows.Close()

	key := fmt.Sprintf("pay:%d", lastAt)
	var out []moraleEvent
	if len(cheques) == 0 {
		for id := range earned {
			out = append(out, moraleEvent{CharID: id, Kind: mkNoPayroll, SrcKey: key, Weight: mwNoPayroll,
				Detail: "worked the week, the payroll paid nobody"})
		}
		return out, nil
	}
	amts := make([]float64, 0, len(cheques))
	for _, a := range cheques {
		amts = append(amts, a)
	}
	median := medianOf(amts)
	for id, amt := range cheques {
		rel := 1.0
		if median > 0 {
			rel = math.Max(0.5, math.Min(1.5, amt/median))
		}
		out = append(out, moraleEvent{CharID: id, Kind: mkPaid, SrcKey: key, Weight: mwPaid * rel,
			Detail: fmt.Sprintf("payroll cheque %.0f ISK", amt)})
	}
	for id := range earned {
		if _, ok := cheques[id]; ok {
			continue
		}
		out = append(out, moraleEvent{CharID: id, Kind: mkSkipped, SrcKey: key, Weight: mwSkipped,
			Detail: "worked the week, others were paid and it was not"})
	}
	return out, nil
}

func (m *Mod) moraleSRP(ctx context.Context, corpID int64, byID map[int64]*moraleMember, now int64) ([]moraleEvent, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT killID, characterID, status, value FROM rolebot_corp_srp
		 WHERE corporationID = ? AND lostAt >= ? AND status IN ('void', 'lapsed', 'granted')`,
		corpID, now-moraleSRPLookbackSec)
	if err != nil {
		return nil, fmt.Errorf("morale srp: %w", err)
	}
	defer rows.Close()
	var out []moraleEvent
	for rows.Next() {
		var killID, charID int64
		var status string
		var value float64
		if err := rows.Scan(&killID, &charID, &status, &value); err != nil {
			return nil, err
		}
		if byID[charID] == nil {
			continue
		}
		kind, w := mkSRPGranted, mwSRPGranted
		switch status {
		case "void":
			kind, w = mkSRPDenied, mwSRPDenied
		case "lapsed":
			kind, w = mkSRPLapsed, mwSRPLapsed
		}
		out = append(out, moraleEvent{CharID: charID, Kind: kind, SrcKey: fmt.Sprintf("srp:%d:%s", killID, status),
			Weight: w, Detail: fmt.Sprintf("SRP %s on a %.0f ISK loss", status, value)})
	}
	return out, rows.Err()
}

// moraleMerit: a step up or down the merit ladder (corpmerit.go).
func (m *Mod) moraleMerit(ctx context.Context, corpID int64, byID map[int64]*moraleMember, cells map[int64]map[string]moraleCell) ([]moraleEvent, error) {
	merit, err := m.loadMerit(ctx, corpID)
	if err != nil {
		return nil, err
	}
	var out []moraleEvent
	for id := range byID {
		r := merit[id]
		rank := 0
		var since int64
		if r != nil {
			rank, since = r.Rank, r.RankSince
		}
		prev, seen := obsOf(cells, id, mkObsRank)
		if seen {
			switch {
			case float64(rank) > prev:
				out = append(out, moraleEvent{CharID: id, Kind: mkPromoted, SrcKey: fmt.Sprintf("rank:%d:%d", rank, since),
					Weight: mwPromoted, Detail: fmt.Sprintf("promoted to rank %d", rank)})
			case float64(rank) < prev:
				out = append(out, moraleEvent{CharID: id, Kind: mkDemoted, SrcKey: fmt.Sprintf("rank:%d:%d", rank, since),
					Weight: mwDemoted, Detail: fmt.Sprintf("dropped to rank %d", rank)})
			}
		}
		out = append(out, moraleEvent{CharID: id, Kind: mkObsRank, Weight: float64(rank)})
	}
	return out, nil
}

// moraleDividends: money back on the member's shares (corpdividend.go), read
// as growth of its equity paid-out line.
func (m *Mod) moraleDividends(ctx context.Context, corpID int64, byID map[int64]*moraleMember, cells map[int64]map[string]moraleCell) ([]moraleEvent, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, paidOut, lastAt FROM rolebot_corp_equity WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("morale equity: %w", err)
	}
	defer rows.Close()
	var out []moraleEvent
	for rows.Next() {
		var id, at int64
		var paid float64
		if err := rows.Scan(&id, &paid, &at); err != nil {
			return nil, err
		}
		if byID[id] == nil {
			continue
		}
		if prev, seen := obsOf(cells, id, mkObsPaidOut); seen && paid > prev+1 {
			out = append(out, moraleEvent{CharID: id, Kind: mkDividend, SrcKey: fmt.Sprintf("div:%d", at),
				Weight: mwDividend, Detail: fmt.Sprintf("%.0f ISK back on its shares", paid-prev)})
		}
		out = append(out, moraleEvent{CharID: id, Kind: mkObsPaidOut, Weight: paid})
	}
	return out, rows.Err()
}

// moraleHumanPay: ISK a human-run corporation's wallet sent the member, the
// one way a person running a corporation can pay its bots.
func (m *Mod) moraleHumanPay(ctx context.Context, corpID int64, ids []int64, since int64) ([]moraleEvent, error) {
	ph, args := inArgs(ids)
	rows, err := m.db.QueryContext(ctx, `
		SELECT refID, characterID, amount FROM market_journal
		 WHERE characterID IN (`+ph+`) AND ownerID1 = ? AND amount > 0 AND transDate >= ?`,
		append(args, corpID, evedb.UnixToWin32(since))...)
	if err != nil {
		return nil, fmt.Errorf("morale corp pay: %w", err)
	}
	defer rows.Close()
	var out []moraleEvent
	for rows.Next() {
		var ref, id int64
		var amt float64
		if err := rows.Scan(&ref, &id, &amt); err != nil {
			return nil, err
		}
		out = append(out, moraleEvent{CharID: id, Kind: mkPaid, SrcKey: fmt.Sprintf("j:%d", ref),
			Weight: mwPaid, Detail: fmt.Sprintf("%.0f ISK from the corporation", amt)})
	}
	return out, rows.Err()
}

// moraleRoleEvents: roles a human director granted or took away, by count.
// Pure over the members' current role masks and the last count seen.
func moraleRoleEvents(members []moraleMember, cells map[int64]map[string]moraleCell) []moraleEvent {
	var out []moraleEvent
	for _, mb := range members {
		n := float64(bits.OnesCount64(mb.Roles))
		if prev, seen := obsOf(cells, mb.CharID, mkObsRoles); seen {
			switch {
			case n > prev:
				out = append(out, moraleEvent{CharID: mb.CharID, Kind: mkPromoted, SrcKey: fmt.Sprintf("roles:%d", mb.Roles),
					Weight: mwPromoted, Detail: "granted corporation roles"})
			case n < prev:
				out = append(out, moraleEvent{CharID: mb.CharID, Kind: mkDemoted, SrcKey: fmt.Sprintf("roles-:%d", mb.Roles),
					Weight: mwDemoted, Detail: "lost corporation roles"})
			}
		}
		out = append(out, moraleEvent{CharID: mb.CharID, Kind: mkObsRoles, Weight: n})
	}
	return out
}

// moraleHead: an NPC chair putting the member in charge of Security.
func (m *Mod) moraleHead(ctx context.Context, corpID int64, byID map[int64]*moraleMember, cells map[int64]map[string]moraleCell) ([]moraleEvent, error) {
	r, ok, err := m.getNPCChair(ctx, corpID)
	if err != nil || !ok {
		return nil, err
	}
	ids := make([]int64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []moraleEvent
	for _, id := range ids {
		v := 0.0
		if id == r.SecurityHeadID {
			v = 1
		}
		if prev, seen := obsOf(cells, id, mkObsHead); v == 1 && (!seen || prev < 1) {
			out = append(out, moraleEvent{CharID: id, Kind: mkHead, SrcKey: fmt.Sprintf("head:%d", r.HeadReviewAt),
				Weight: mwHead, Detail: "made Director of Security"})
		}
		out = append(out, moraleEvent{CharID: id, Kind: mkObsHead, Weight: v})
	}
	return out, nil
}
