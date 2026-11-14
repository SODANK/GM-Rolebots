package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Help calls: how a lot of small fleets add up to a big one when it matters. A fleet that
// lost ships or found a fight it cannot take posts a call (rolebot_fleet_help), and a
// Director of Security posts a call to arms while a siege or alliance defend stands.
// Nearby fleets of the same corporation, alliance and mutual allies answer: the
// commander's site pick (gangSite) takes the call's system ahead of its own sweep and
// bumps SiteSeq so members re-plan. The fleets stay separate gangs; a fight on the shared
// grid pools them (siblingCombatants, read by engageOnce). A call stops drawing answers
// once enough fleets are on it (helpCallSatisfied) and closes on its TTL or when its
// poster is gone. A call to arms has no ceiling and gets a dedicated uncapped fleet
// (formCallsToArms).

const (
	helpCallToArms   = "calltoarms"
	helpSiege        = "siege"
	helpRetreat      = "retreat"
	helpOutnumbered  = "outnumbered"
	helpCallsMax     = 50
	callToArmsMuster = 20 * 60

	settingHelpRadius = "rolebots.gang.helpRadiusJumps"
	settingHelpTTL    = "rolebots.gang.helpTTLMin"

	defaultHelpRadius = 8
	defaultHelpTTLMin = 20
)

var (
	helpRadiusVal atomic.Int64
	helpTTLMinVal atomic.Int64
)

func init() {
	helpRadiusVal.Store(defaultHelpRadius)
	helpTTLMinVal.Store(defaultHelpTTLMin)
}

// HelpRadiusJumps is how far a fleet will fly to answer a call.
func HelpRadiusJumps() int { return int(helpRadiusVal.Load()) }

// HelpTTLSec is how long a call stands unless refreshed.
func HelpTTLSec() int64 { return helpTTLMinVal.Load() * 60 }

func (m *Mod) loadHelpSettings(ctx context.Context) {
	for _, s := range []struct {
		key      string
		val      *atomic.Int64
		min, max int64
	}{
		{settingHelpRadius, &helpRadiusVal, 0, 30},
		{settingHelpTTL, &helpTTLMinVal, 1, 240},
	} {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, s.key)
		if err != nil || !ok {
			continue
		}
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= s.min && n <= s.max {
			s.val.Store(n)
		}
	}
}

// helpPriority ranks the kinds: a call to arms over a siege timer over a fleet
// that just lost ships over one that found a fight it cannot take. Pure.
func helpPriority(kind string) int {
	switch kind {
	case helpCallToArms:
		return 4
	case helpSiege:
		return 3
	case helpRetreat:
		return 2
	case helpOutnumbered:
		return 1
	}
	return 0
}

// helpCall is one open row.
type helpCall struct {
	CallID, GangID, CorpID, SystemID int64
	Kind                             string
	Need                             int
	Note                             string
	ExpiresAt                        int64
	// jumps is filled in by the answering side.
	jumps int
}

// helpCallSatisfied reports whether enough pilots are already answering. A
// call to arms (need 0) is never satisfied. Pure.
func helpCallSatisfied(need, responders int) bool {
	if need <= 0 {
		return false
	}
	return responders >= int(math.Ceil(float64(need)*1.5))
}

// pickHelpCall chooses which call a fleet answers: highest priority, then the
// nearest, then the oldest (lowest ID). Pure.
func pickHelpCall(calls []helpCall) *helpCall {
	if len(calls) == 0 {
		return nil
	}
	sort.SliceStable(calls, func(i, j int) bool {
		pi, pj := helpPriority(calls[i].Kind), helpPriority(calls[j].Kind)
		if pi != pj {
			return pi > pj
		}
		if calls[i].jumps != calls[j].jumps {
			return calls[i].jumps < calls[j].jumps
		}
		return calls[i].CallID < calls[j].CallID
	})
	return &calls[0]
}

// postHelpCall files or refreshes a call. One open row per (fleet, kind); a
// call to arms is the corporation's (gangID 0), one per (corp, system).
func (m *Mod) postHelpCall(ctx context.Context, gangID, corpID, sysID int64, kind string, need int, note string) {
	if corpID == 0 || sysID == 0 || m.db == nil {
		return
	}
	now := clock.Unix()
	exp := now + HelpTTLSec()
	var id int64
	if gangID != 0 {
		_ = m.db.QueryRowContext(ctx, `
			SELECT callID FROM rolebot_fleet_help
			 WHERE gangID = ? AND kind = ? AND closedAt = 0 AND expiresAt > ?
			 ORDER BY callID DESC LIMIT 1`, gangID, kind, now).Scan(&id)
	} else {
		_ = m.db.QueryRowContext(ctx, `
			SELECT callID FROM rolebot_fleet_help
			 WHERE gangID = 0 AND corpID = ? AND systemID = ? AND kind = ? AND closedAt = 0 AND expiresAt > ?
			 ORDER BY callID DESC LIMIT 1`, corpID, sysID, kind, now).Scan(&id)
	}
	if id != 0 {
		if _, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_fleet_help SET systemID = ?, need = GREATEST(need, ?), expiresAt = ?, note = ?
			 WHERE callID = ?`, sysID, need, exp, truncate(note, 255), id); err != nil {
			rlogf("rolebots: refresh help call %d: %v", id, err)
		}
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_fleet_help
		  (gangID, corpID, allianceID, systemID, kind, priority, need, note, postedAt, expiresAt, closedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		gangID, corpID, m.corpAllianceID(ctx, corpID), sysID, kind, helpPriority(kind), need,
		truncate(note, 255), now, exp); err != nil {
		rlogf("rolebots: post help call: %v", err)
		return
	}
	m.logBot(0, logInfo, "gang", "help call (%s) from %s in %s — %s",
		kind, corpNameOf(ctx, m.db, corpID), m.nameOfSystem(ctx, sysID), note)
	// Said in the alliance channel, if a person is reading it (chatter_alliance.go).
	m.chatterAllyCall(ctx, gangID, corpID, sysID, kind)
}

// closeHelpCall closes one call.
func (m *Mod) closeHelpCall(ctx context.Context, callID int64) {
	if callID == 0 {
		return
	}
	_, _ = m.db.ExecContext(ctx,
		`UPDATE rolebot_fleet_help SET closedAt = ? WHERE callID = ? AND closedAt = 0`, clock.Unix(), callID)
}

// helpCallOpen loads one call if it is still open and its poster still
// stands.
func (m *Mod) helpCallOpen(ctx context.Context, callID int64) *helpCall {
	if callID == 0 {
		return nil
	}
	var c helpCall
	err := m.db.QueryRowContext(ctx, `
		SELECT h.callID, h.gangID, h.corpID, h.systemID, h.kind, h.need, h.note, h.expiresAt
		  FROM rolebot_fleet_help h
		 WHERE h.callID = ? AND h.closedAt = 0 AND h.expiresAt > ?
		   AND (h.gangID = 0 OR EXISTS (SELECT 1 FROM rolebot_gangs g WHERE g.gangID = h.gangID AND g.enabled = 1))`,
		callID, clock.Unix()).Scan(&c.CallID, &c.GangID, &c.CorpID, &c.SystemID, &c.Kind, &c.Need, &c.Note, &c.ExpiresAt)
	if err != nil {
		return nil
	}
	return &c
}

// kinCorps is a corporation, its alliance's other members and its mutual
// allies: who it answers, and who answers it.
func (m *Mod) kinCorps(ctx context.Context, corpID int64) []int64 {
	if corpID == 0 {
		return nil
	}
	seen := map[int64]bool{corpID: true}
	out := []int64{corpID}
	for _, id := range append(m.allianceMemberCorps(ctx, corpID), relAlliesOf(corpID)...) {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// openHelpCallsFor lists the open calls of these corporations, a fleet's own
// excluded, and closes the ones whose poster has gone.
func (m *Mod) openHelpCallsFor(ctx context.Context, corps []int64, exceptGang int64) []helpCall {
	if len(corps) == 0 {
		return nil
	}
	args := []any{clock.Unix(), exceptGang}
	args = append(args, int64Args(corps)...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT h.callID, h.gangID, h.corpID, h.systemID, h.kind, h.need, h.note, h.expiresAt,
		       (h.gangID = 0 OR EXISTS (SELECT 1 FROM rolebot_gangs g WHERE g.gangID = h.gangID AND g.enabled = 1))
		  FROM rolebot_fleet_help h
		 WHERE h.closedAt = 0 AND h.expiresAt > ? AND h.gangID <> ?
		   AND h.corpID IN (`+placeholders(len(corps))+`)
		 ORDER BY h.priority DESC, h.callID
		 LIMIT `+fmt.Sprint(helpCallsMax), args...)
	if err != nil {
		return nil
	}
	var out []helpCall
	var dead []int64
	for rows.Next() {
		var c helpCall
		var alive bool
		if rows.Scan(&c.CallID, &c.GangID, &c.CorpID, &c.SystemID, &c.Kind, &c.Need, &c.Note, &c.ExpiresAt, &alive) != nil {
			continue
		}
		if !alive {
			dead = append(dead, c.CallID)
			continue
		}
		out = append(out, c)
	}
	rows.Close()
	for _, id := range dead {
		m.closeHelpCall(ctx, id)
	}
	return out
}

// helpResponders is how many pilots are in the fleets already answering a
// call.
func (m *Mod) helpResponders(ctx context.Context, callID int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_gang_members gm
		  JOIN rolebot_gangs g ON g.gangID = gm.gangID AND g.enabled = 1
		 WHERE JSON_VALUE(g.state, '$.helpCallID') = ?`, callID).Scan(&n)
	return n
}

// gangAnswerHelp is the commander's question before it picks a site of its own: is a kin
// fleet or the corporation calling for help within reach? It returns the system to sweep
// and the call, or (0, nil). A fleet already answering keeps answering while the call
// stands; a fleet on a human's order answers only a call to arms.
func (m *Mod) gangAnswerHelp(ctx context.Context, b *botRun, g *Gang) (int64, *helpCall) {
	if g == nil || b == nil || HelpRadiusJumps() <= 0 {
		return 0, nil
	}
	switch g.Activity {
	case ActCombat, ActSecurity:
	default:
		return 0, nil
	}
	if g.State.HelpCallID != 0 {
		if c := m.helpCallOpen(ctx, g.State.HelpCallID); c != nil {
			return c.SystemID, c
		}
		g.State.HelpCallID = 0 // the call is over; the caller saves the state
	}
	corp := g.CorpID
	if corp == 0 {
		corp = b.CorpID
	}
	calls := m.openHelpCallsFor(ctx, m.kinCorps(ctx, corp), g.GangID)
	if len(calls) == 0 {
		return 0, nil
	}
	u, err := m.universe(ctx)
	if err != nil {
		return 0, nil
	}
	near := hopsWithin(u, b.SystemID, HelpRadiusJumps())
	now := clock.Unix()
	var reach []helpCall
	for _, c := range calls {
		j, ok := near[c.SystemID]
		if !ok {
			continue
		}
		// A call the fleet's commitment outranks is not one it can answer (gangintent.go);
		// picking it would crowd out one it could.
		if !objectiveMayTake(&g.State, objCall, callObjectivePrio(c.Kind), now) {
			continue
		}
		// Nor one that closes before the fleet could get there.
		if now+int64(j)*fleetSecPerJump(0) >= c.ExpiresAt {
			continue
		}
		if g.State.OrderID != 0 && c.Kind != helpCallToArms {
			continue
		}
		// A call to arms is its own corporation's or alliance's business; an ally's war does not
		// summon a corporation that only trades with it.
		if c.Kind == helpCallToArms && c.CorpID != corp &&
			(m.corpAllianceID(ctx, corp) == 0 || m.corpAllianceID(ctx, corp) != m.corpAllianceID(ctx, c.CorpID)) {
			continue
		}
		if helpCallSatisfied(c.Need, m.helpResponders(ctx, c.CallID)) {
			continue
		}
		// A site call is graded before the fleet flies (sites.go): a fleet that would refuse on
		// arrival should not make the trip.
		if siteID, _ := parseSiteCall(c.Kind, c.Note); siteID != 0 && !m.gangMayAnswerSiteCall(ctx, b, g, siteID) {
			continue
		}
		c.jumps = j
		reach = append(reach, c)
	}
	pick := pickHelpCall(reach)
	if pick == nil {
		return 0, nil
	}
	return pick.SystemID, pick
}

// helpPollSec is how long one fleet's "is anybody calling?" answer stands. Every
// planning member and the commander's step ask, and one query round a minute per fleet
// is plenty for a call that stands twenty.
const helpPollSec = 60

var (
	helpPollMu sync.Mutex
	helpPollAt = map[int64]int64{} // gangID to last time the board was read
)

// adoptHelpCall points the fleet at a call it should answer, or takes it off
// one that is over. It reports whether the fleet is now (still) answering a
// call, in which case the caller's own site pick is moot. Saves the gang
// state when it changes anything.
func (m *Mod) adoptHelpCall(ctx context.Context, b *botRun, g *Gang) (bool, error) {
	if g == nil {
		return false, nil
	}
	gs := &g.State
	now := clock.Unix()
	// The memo only short-cuts a fleet answering nothing: a fleet on a call
	// re-reads it (one indexed row) so it notices the call close promptly.
	if gs.HelpCallID == 0 {
		helpPollMu.Lock()
		last := helpPollAt[g.GangID]
		fresh := now-last < helpPollSec
		if !fresh {
			helpPollAt[g.GangID] = now
			if len(helpPollAt) > 4096 {
				for id, at := range helpPollAt {
					if now-at > 10*helpPollSec {
						delete(helpPollAt, id)
					}
				}
			}
		}
		helpPollMu.Unlock()
		if fresh {
			return false, nil
		}
	}
	was := gs.HelpCallID
	sys, call := m.gangAnswerHelp(ctx, b, g)
	if call == nil {
		if was != 0 {
			// The call is over: back to its own sweep. clearObjective re-reads, since saving the
			// stale copy would undo what the bounty pass wrote.
			return false, m.clearObjective(ctx, g.GangID, gs, func(st *GangState) bool {
				if st.HelpCallID != was {
					return false
				}
				st.HelpCallID, st.HuntPickedAt = 0, 0
				return true
			})
		}
		return false, nil
	}
	if sys == gs.HuntSystemID && was == call.CallID {
		return true, nil // on it already
	}
	siteID, siteName := parseSiteCall(call.Kind, call.Note)
	sysName := m.nameOfSystem(ctx, sys)
	v, err := m.commitObjective(ctx, g.GangID, gs, GangObjective{
		Kind: objCall, Ref: call.CallID, SystemID: sys,
		Prio: callObjectivePrio(call.Kind), Until: call.ExpiresAt, Announce: true,
	}, func(st *GangState) {
		fileHuntRecent(st, sys)
		dropSiteOffHunt(st, g.GangID, sys)
		st.HuntSystemID, st.HuntSystemName, st.HuntPickedAt = sys, sysName, now
		st.HelpCallID = call.CallID
		// A site call (sites.go) names the site; the pack runs it instead of
		// sweeping the belts it lands among.
		st.SiteID, st.SiteName = siteID, siteName
	})
	if err != nil {
		return false, err
	}
	switch v {
	case objRefuse:
		// The fleet is committed to something that outranks this call.
		return gs.HelpCallID != 0, nil
	case objSame:
		return true, nil
	}
	if gs.SiteID != 0 {
		// A refused place is found out on arrival (siteRunStep's "crowded").
		if s, ok := modapi.Sites().Get(gs.SiteID); ok {
			if card, err := m.siteCardFor(ctx, s.DungeonVID); err == nil && card != nil {
				m.takeSiteClaim(ctx, b, g.GangID, s, card)
			}
		}
	}
	m.logBot(b.CharID, logInfo, "gang", "gang %q answers a %s call — converging on %s (%s)",
		g.Name, call.Kind, gs.HuntSystemName, call.Note)
	m.narrateFleet(ctx, g.GangID, true, "Answering a call for help — all ships to %s.", gs.HuntSystemName)
	// Another member corporation's call: the commander says so in the alliance channel.
	m.chatterAllyOmw(ctx, g, call)
	return true, nil
}

// siblingCombatants is the kin fleets' pilots standing in sysID, out in space and driven,
// in a combat, security or raid fleet other than exceptGang, as combatants. A fight pools
// them with the fleet that started it; command stays with each fleet.
func (m *Mod) siblingCombatants(ctx context.Context, corpID, exceptGang, sysID int64, have map[int64]bool) []*pvpCombatant {
	corps := m.kinCorps(ctx, corpID)
	if len(corps) == 0 || sysID == 0 {
		return nil
	}
	args := []any{exceptGang, sysID}
	args = append(args, int64Args(corps)...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT gm.characterID, gm.role
		  FROM rolebot_gang_members gm
		  JOIN rolebot_gangs g ON g.gangID = gm.gangID AND g.enabled = 1
		  JOIN rolebot_characters rb ON rb.characterID = gm.characterID AND rb.enabled = 1
		  JOIN character_ c ON c.characterID = gm.characterID
		 WHERE g.gangID <> ? AND g.activity IN ('`+ActCombat+`', '`+ActSecurity+`', '`+ActPiracy+`')
		   AND c.solarSystemID = ? AND COALESCE(c.stationID, 0) = 0
		   AND g.corpID IN (`+placeholders(len(corps))+`)
		 LIMIT 60`, args...)
	if err != nil {
		return nil
	}
	type mate struct {
		id   int64
		role string
	}
	var mates []mate
	for rows.Next() {
		var x mate
		// A logged-off pilot is not in space (popcap.go).
		if rows.Scan(&x.id, &x.role) == nil && !have[x.id] && !popIsOffline(x.id) {
			mates = append(mates, x)
		}
	}
	rows.Close()
	var out []*pvpCombatant
	for _, x := range mates {
		if pc := m.pvpCombatantFor(ctx, x.id, x.role); pc != nil && pc.SystemID == sysID {
			have[x.id] = true
			out = append(out, pc)
		}
	}
	return out
}

// combatantIDs is the set of characters already on a side.
func combatantIDs(side []*pvpCombatant) map[int64]bool {
	out := make(map[int64]bool, len(side))
	for _, c := range side {
		if c != nil {
			out[c.CharID] = true
		}
	}
	return out
}

// ── calls to arms: the uncapped fleet ───────────────────────────────────────

// formCallsToArms raises a dedicated fleet for every open call to arms that has none yet:
// every free pilot of the corporation, the siege doctrine, a short quorum muster. It runs
// ahead of the formation budget (pass zero of gangFormPass); sibling fleets converge on
// the call through gangAnswerHelp.
func (m *Mod) formCallsToArms(ctx context.Context, tally *formTally) {
	// Page the corporations first (armsping.go), so the pilots who answer are
	// on the server by the time the fleets below recruit.
	m.armsPingPass(ctx)
	// The board's housekeeping rides the same pass: a day after a call
	// stopped standing, nothing reads it.
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_fleet_help WHERE expiresAt < ?`, clock.Unix()-86400)
	rows, err := m.db.QueryContext(ctx, `
		SELECT h.callID, h.corpID, h.systemID, h.expiresAt, h.note
		  FROM rolebot_fleet_help h
		 WHERE h.kind = ? AND h.gangID = 0 AND h.closedAt = 0 AND h.expiresAt > ?
		   AND NOT EXISTS (SELECT 1 FROM rolebot_gangs g
		                    WHERE g.enabled = 1 AND g.corpID = h.corpID
		                      AND JSON_VALUE(g.state, '$.helpCallID') = h.callID)
		 ORDER BY h.callID
		 LIMIT 10`, helpCallToArms, clock.Unix())
	if err != nil {
		return
	}
	type call struct {
		id, corp, sys, exp int64
		note               string
	}
	var calls []call
	for rows.Next() {
		var c call
		if rows.Scan(&c.id, &c.corp, &c.sys, &c.exp, &c.note) == nil {
			calls = append(calls, c)
		}
	}
	rows.Close()
	for _, c := range calls {
		m.formCallToArms(ctx, tally, c.id, c.corp, c.sys, c.exp, c.note)
	}
}

// callToArmsTries is how many commanders one call to arms tries per pass, across both
// departments and both doctrines.
const callToArmsTries = 6

// formCallToArms raises the fleet for one call to arms. It tries the Security, then the Combat
// department's commanders under the siege doctrine, then again under the war line
// (LineDoctrine), and stops at the first fleet that forms or after callToArmsTries attempts.
func (m *Mod) formCallToArms(ctx context.Context, tally *formTally, callID, corpID, sysID, exp int64, note string) {
	var seats []fcSeat
	why := ""
	for _, act := range []string{ActSecurity, ActCombat} {
		// formFleetAround skips the formation back-off for an order, so it is read here: a
		// call-to-arms fleet that stood down at muster (siegeAttackFloor) is not raised again
		// on the next pass.
		if formBackedOff(corpID, act) > 0 {
			why = "backing off after a fleet that fell apart"
			continue
		}
		got, w := m.pickOrderCommanders(ctx, corpID, act, callToArmsTries/2)
		if len(got) == 0 {
			why = w
		}
		seats = append(seats, got...)
	}
	if len(seats) == 0 {
		tally.refuse(fcSeat{activity: helpCallToArms}, "call to arms: "+why)
		return
	}
	sysName := m.nameOfSystem(ctx, sysID)
	tries := 0
	var last fcSeat
	refusal := ""
	for _, line := range []bool{false, true} {
		for _, seat := range seats {
			if tries >= callToArmsTries {
				break
			}
			tries++
			spec := &fleetOrderSpec{
				SystemID: sysID, SystemName: sysName,
				LifeSec: max(exp-clock.Unix(), 0) + 2*3600, CallToArms: true, HelpCallID: callID,
				LineDoctrine: line,
			}
			formed, r, err := m.formFleetAround(ctx, seat, spec)
			if err != nil {
				rlogf("rolebots: call to arms corp %d: %v", corpID, err)
				return
			}
			if formed {
				tally.form(seat)
				doctrine := "the siege doctrine"
				if line {
					doctrine = "the war line"
				}
				m.logBot(seat.charID, logInfo, "gang", "raised a call-to-arms fleet for %s on %s — %s",
					sysName, doctrine, strings.TrimSpace(note))
				return
			}
			last, refusal = seat, r
		}
	}
	tally.refuse(last, "call to arms: "+refusal)
}

// allianceDefendOrderFor is the alliance's defend call (gangalliance.go) as a standing
// order for a member corporation's combat wing: the tower's system, from siegeFormUpSec
// before the timer ends until the call lapses. The Security desk reads it after its own
// siege arm; a desk-source order would be overwritten by the next Security pass.
func (m *Mod) allianceDefendOrderFor(ctx context.Context, corpID int64) (int64, string) {
	aid := m.corpAllianceID(ctx, corpID)
	if aid == 0 {
		return 0, ""
	}
	reqs, err := m.allianceRequestsPending(ctx, aid, allianceReqDefend)
	if err != nil {
		return 0, ""
	}
	now := clock.Unix()
	for _, r := range reqs {
		if r.SystemID == 0 || r.ExpiresAt <= now {
			continue
		}
		// A call with a timer expires an hour after it, and the wing forms up siegeFormUpSec
		// before the timer. A call without one carries the flat TTL and is answered at once.
		if r.ExpiresAt-r.PostedAt != allianceDefendTTLSec {
			if exitAt := r.ExpiresAt - 3600; exitAt > now && now < exitAt-siegeFormUpSec {
				continue
			}
		}
		return r.SystemID, "alliance defend call: " + r.Note
	}
	return 0, ""
}
