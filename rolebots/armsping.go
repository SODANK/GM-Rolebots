package rolebots

// Calls to arms override play hours: the ping.
//
// A siege or an alliance defend call that lands in a corporation's night would find nobody: the Security
// director is asleep, so no order goes out, and fleets recruit only logged-in pilots (timezone.go). The
// ping reads the urgent sources directly (armsAlerts) and pages the corporation: the CEO and the Security
// head log in at once, members in reach already on are held there for the fight, and logged-off members in
// reach are rolled, likeliest first, until the corporation has the numbers (armsNeed). Each (alert, pilot)
// is rolled once. A held pilot stays on, past the timezone gate and the online cap, until the alert ends
// plus armsReleaseSec. Switched by rolebots.tz.armsPing (default on).

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

const (
	// How long a responder stays held after the alert stops being urgent.
	armsReleaseSec = 15 * 60
	// The enemy assumed when nothing on the board says how many are coming:
	// an unknown attacker, such as a player gang.
	armsDefaultEnemy = 10
	// Answer odds: near this many minutes from its own hours a pilot nearly
	// always answers; from armsFarMin on it rarely does.
	armsNearMin   = 60
	armsFarMin    = 360
	armsNearOdds  = 0.90
	armsFarOdds   = 0.15
	armsOfficerP  = 0.80
	armsRollSalt  = 0x61726d73 // "arms"
	armsMaxAlerts = 20
)

// armsAlert is one corporation's urgent fight.
type armsAlert struct {
	Key      string
	CorpID   int64
	SystemID int64
	Why      string
	// OppCorpID is the other side, 0 when unknown.
	OppCorpID int64
}

// armsStat is one alert as the panel shows it.
type armsStat struct {
	Key, CorpName, SystemName, Why string
	Need, Held, Answered, Slept    int
}

var (
	armsMu sync.Mutex
	// armsHolds maps charID to the unix time the hold lapses.
	armsHolds = map[int64]int64{}
	// armsRolled maps alert key to charID to answered: every roll ever made for a standing alert.
	armsRolled = map[string]map[int64]bool{}
	// armsByCorp maps corpID to the held responders this pass, read by the other side's need.
	armsByCorp = map[int64]int{}
	armsStats  []armsStat
)

// armsHeld reports whether a pilot is held online by a call to arms.
func armsHeld(charID int64) bool {
	armsMu.Lock()
	defer armsMu.Unlock()
	until, ok := armsHolds[charID]
	return ok && until > clock.Unix()
}

func armsHold(ids []int64, until int64) {
	armsMu.Lock()
	for _, id := range ids {
		if armsHolds[id] < until {
			armsHolds[id] = until
		}
	}
	armsMu.Unlock()
}

// armsSnapshot is the panel's copy of the last pass.
func armsSnapshot() []armsStat {
	armsMu.Lock()
	defer armsMu.Unlock()
	return append([]armsStat(nil), armsStats...)
}

// armsOdds is the chance a logged-off pilot answers: by how far it is from
// its own hours, then its loyalty (the trait's devoted end is negative), and
// never below armsOfficerP for an officer. Pure.
func armsOdds(awayMin int64, loyalty int, officer bool) float64 {
	p := armsFarOdds
	switch {
	case awayMin <= armsNearMin:
		p = armsNearOdds
	case awayMin < armsFarMin:
		f := float64(awayMin-armsNearMin) / float64(armsFarMin-armsNearMin)
		p = armsNearOdds + f*(armsFarOdds-armsNearOdds)
	}
	p *= 1 - 0.25*traitScale(loyalty)
	if officer {
		p = math.Max(p, armsOfficerP)
	}
	return math.Min(p, 1)
}

// armsNeed is how many pilots a corporation pages for against enemy. Pure.
func armsNeed(enemy int, c tzConfig) int {
	n := int(math.Ceil(1.25 * float64(enemy)))
	return int(clampRange(float64(n), float64(c.ArmsMinNeed), float64(max(c.ArmsMinNeed, c.ArmsMaxNeed))))
}

// armsRoll is one pilot's answer to one alert. Deterministic in the pair.
func armsRoll(key string, charID int64, p float64) bool {
	var h uint64 = armsRollSalt
	for _, b := range []byte(key) {
		h = splitmix64(h ^ uint64(b))
	}
	return float64(splitmix64(h^uint64(charID)+noiseGolden)>>11)/(1<<53) < p
}

// siegeArmsWindow reports whether a siege is a call to arms for corpID right now, and why. The
// attacker is called from the muster on. The defender is called while the tower is under fire
// (bash, kill) and around the reinforcement exit, never during a muster it cannot see. Pure in
// (siege, corp, now).
func siegeArmsWindow(s *siegeRow, corpID, now int64, sysName string) (bool, string) {
	attacking := s.AttackerCorpID == corpID
	target := "tower"
	if s.isStation() {
		target = "station"
	}
	switch s.Stage {
	case "muster":
		if attacking {
			return true, "forming up for a siege in " + sysName
		}
	case "bash", "kill", "fight":
		if attacking {
			return true, "besieging the " + target + " in " + sysName
		}
		if s.DefenderCorpID == corpID {
			return true, "defending the " + target + " in " + sysName + " — it is under fire"
		}
	case "timer":
		if now >= s.ExitAt-siegeFormUpSec && now <= s.ExitAt+siegeAfterExitSec {
			verb := "defending"
			if attacking {
				verb = "finishing"
			}
			return true, fmt.Sprintf("%s the tower in %s — reinforcement ends %s",
				verb, sysName, fmtDuration(max(0, s.ExitAt-now)))
		}
	}
	return false, ""
}

// armsAlerts is every bot-CEO corporation's urgent fight: its sieges in their
// window, then its alliance's defend calls.
func (m *Mod) armsAlerts(ctx context.Context) []armsAlert {
	now := clock.Unix()
	var out []armsAlert
	seen := map[int64]bool{}
	for _, s := range m.openSieges(ctx, `1 = 1 ORDER BY exitAt`) {
		type seat struct {
			corp, opp int64
			view      *siegeRow
		}
		seats := []seat{{s.AttackerCorpID, s.DefenderCorpID, s}, {s.DefenderCorpID, s.AttackerCorpID, s}}
		if s.isStation() {
			// A station siege is the whole alliance's: every member corporation of either side is
			// paged, each seen from its own seat.
			for _, mate := range m.allianceMemberCorps(ctx, s.AttackerCorpID) {
				v := *s
				v.AttackerCorpID = mate
				seats = append(seats, seat{mate, s.DefenderCorpID, &v})
			}
			for _, mate := range m.allianceMemberCorps(ctx, s.DefenderCorpID) {
				v := *s
				v.DefenderCorpID = mate
				seats = append(seats, seat{mate, s.AttackerCorpID, &v})
			}
		}
		for _, st := range seats {
			corp, opp := st.corp, st.opp
			if corp == 0 || seen[corp] {
				continue
			}
			if _, managed := managedCEO(corp); !managed {
				continue
			}
			if ok, why := siegeArmsWindow(st.view, corp, now, systemNameOf(ctx, m, s.SystemID)); ok {
				seen[corp] = true
				out = append(out, armsAlert{Key: fmt.Sprintf("s%d:%d", s.SiegeID, corp),
					CorpID: corp, SystemID: s.SystemID, Why: why, OppCorpID: opp})
			}
		}
	}
	if ceos, err := listCEOs(ctx, m.db); err == nil {
		for _, c := range ceos {
			if seen[c.CorpID] || m.corpAllianceID(ctx, c.CorpID) == 0 {
				continue
			}
			if sys, why := m.allianceDefendOrderFor(ctx, c.CorpID); sys != 0 {
				seen[c.CorpID] = true
				out = append(out, armsAlert{Key: fmt.Sprintf("d%d:%d", sys, c.CorpID),
					CorpID: c.CorpID, SystemID: sys, Why: why})
			}
		}
	}
	if len(out) > armsMaxAlerts {
		out = out[:armsMaxAlerts]
	}
	return out
}

// armsEnemy is how many are coming: the other side's armed bots in the
// system, its own held responders, and the players there, whichever is most;
// armsDefaultEnemy when none of that is known.
func (m *Mod) armsEnemy(ctx context.Context, a armsAlert) int {
	n := 0
	if a.OppCorpID != 0 {
		n = len(m.siegeSide(ctx, a.OppCorpID, a.SystemID, "dps"))
		armsMu.Lock()
		n = max(n, armsByCorp[a.OppCorpID])
		armsMu.Unlock()
	}
	if m.sessions != nil {
		players := 0
		for _, s := range m.sessions.FindBySystemID(a.SystemID) {
			if !s.Bot {
				players++
			}
		}
		n = max(n, players)
	}
	if n == 0 {
		n = armsDefaultEnemy
	}
	return n
}

// armsMember is one bot member of a paging corporation.
type armsMember struct {
	CharID  int64
	Loyalty int
	Officer bool
	// Security is a pilot of the corporation's Security arm, its combat wing: it always answers
	// a call to arms, from anywhere, whatever its hours (armsAlwaysAnswers).
	Security bool
}

// armsAlwaysAnswers reports whether a member answers a call to arms without a roll: the combat wing
// (the Security arm) shows up, timezone be damned. Pure.
func armsAlwaysAnswers(mm armsMember) bool { return mm.Security }

// armsMembersInReach is a corporation's enabled bots within reach of the fight, plus every pilot of
// its Security arm wherever it is.
func (m *Mod) armsMembersInReach(ctx context.Context, a armsAlert, reachJumps int) ([]armsMember, error) {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return nil, err
	}
	reach := hopsWithin(u, a.SystemID, reachJumps)
	roles := "0"
	if ok, err := haveColumn(ctx, m.db, "chrCorporationRoles", "corprole"); err == nil && ok {
		roles = `COALESCE((SELECT r.corprole FROM chrCorporationRoles r WHERE r.characterID = ch.characterID), 0)`
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID, ch.solarSystemID, `+roles+` <> 0,
		       COALESCE((SELECT t.value FROM rolebot_traits t
		                  WHERE t.characterID = ch.characterID AND t.trait = '`+TraitLoyalty+`'), 0)
		  FROM character_ ch
		  JOIN rolebot_characters b ON b.characterID = ch.characterID AND b.enabled = 1
		 WHERE ch.corporationID = ?`, a.CorpID)
	if err != nil {
		return nil, fmt.Errorf("armsMembersInReach: %w", err)
	}
	defer rows.Close()
	var out []armsMember
	for rows.Next() {
		var mm armsMember
		var sys int64
		if rows.Scan(&mm.CharID, &sys, &mm.Officer, &mm.Loyalty) != nil {
			continue
		}
		if seat, ok := armOf(mm.CharID, a.CorpID); ok && seat.Arm == armSecurity {
			mm.Security = true
		}
		if _, ok := reach[sys]; ok || mm.Security {
			out = append(out, mm)
		}
	}
	return out, rows.Err()
}

// armsPingPass pages every corporation with an urgent fight. Runs ahead of
// formCallsToArms, so the pilots it brings in are on the server by the time
// the call-to-arms fleet recruits.
func (m *Mod) armsPingPass(ctx context.Context) {
	c := tzSettings()
	if !c.On || !c.ArmsPing {
		armsMu.Lock()
		armsStats, armsByCorp = nil, map[int64]int{}
		armsMu.Unlock()
		return
	}
	now := clock.Unix()
	until := now + armsReleaseSec
	alerts := m.armsAlerts(ctx)

	stats := make([]armsStat, 0, len(alerts))
	held := map[int64]int{}
	live := map[string]bool{}
	for _, a := range alerts {
		live[a.Key] = true
		st := armsStat{Key: a.Key, CorpName: corpNameOf(ctx, m.db, a.CorpID),
			SystemName: systemNameOf(ctx, m, a.SystemID), Why: a.Why}
		st.Need = armsNeed(m.armsEnemy(ctx, a), c)

		// Command answers first: the CEO thinks and the Security desk posts
		// the order only while they are on.
		var command []int64
		if ceo, ok := managedCEO(a.CorpID); ok {
			command = append(command, ceo)
			if s, err := m.loadCorpStaff(ctx, a.CorpID, ceo); err == nil {
				if h := s.head(portSecurity); h != nil && !h.Human {
					command = append(command, h.CharID)
				}
			}
		}
		armsHold(command, until)

		members, err := m.armsMembersInReach(ctx, a, int(c.ArmsReachJumps))
		if err != nil {
			rlogf("rolebots: call-to-arms ping %s: %v", a.Key, err)
			continue
		}
		type cand struct {
			armsMember
			p    float64
			away int64
		}
		var on, wing []int64
		var off []cand
		for _, mm := range members {
			if !popIsOffline(mm.CharID) || armsHeld(mm.CharID) {
				on = append(on, mm.CharID)
				continue
			}
			if armsAlwaysAnswers(mm) {
				wing = append(wing, mm.CharID) // the combat wing logs in, whatever its hours
				continue
			}
			ts := tzNow(mm.CharID)
			away := min(ts.NextMin, ts.SinceEndMin)
			off = append(off, cand{mm, armsOdds(away, mm.Loyalty, mm.Officer), away})
		}
		armsHold(on, until)
		armsHold(wing, until)
		count := len(on) + len(wing)
		for _, id := range wing {
			armsMu.Lock()
			already := armsRolled[a.Key][id]
			armsMu.Unlock()
			if !already {
				m.logBot(id, logInfo, "presence", "answered the call to arms for %s — the combat wing always shows",
					st.SystemName)
			}
		}

		sort.SliceStable(off, func(i, j int) bool {
			if off[i].p != off[j].p {
				return off[i].p > off[j].p
			}
			return off[i].CharID < off[j].CharID
		})
		armsMu.Lock()
		rolled := armsRolled[a.Key]
		if rolled == nil {
			rolled = map[int64]bool{}
			armsRolled[a.Key] = rolled
		}
		armsMu.Unlock()
		var answered []cand
		paged := 0
		for _, cd := range off {
			armsMu.Lock()
			yes, done := rolled[cd.CharID]
			armsMu.Unlock()
			if done {
				if yes {
					answered = append(answered, cd)
					count++
				} else {
					st.Slept++
				}
				continue
			}
			if count >= st.Need {
				break
			}
			yes = armsRoll(a.Key, cd.CharID, cd.p)
			armsMu.Lock()
			rolled[cd.CharID] = yes
			armsMu.Unlock()
			paged++
			if !yes {
				st.Slept++
				continue
			}
			answered = append(answered, cd)
			count++
			m.logBot(cd.CharID, logInfo, "presence", "answered the call to arms for %s — logged in %s from its hours",
				st.SystemName, tzDur(cd.away))
		}
		ids := make([]int64, 0, len(answered))
		for _, cd := range answered {
			ids = append(ids, cd.CharID)
		}
		armsHold(ids, until)
		st.Answered, st.Held = len(answered)+len(wing), count
		held[a.CorpID] += count
		if paged > 0 {
			if ceo, err := getCEO(ctx, m.db, a.CorpID); err == nil && ceo != nil {
				m.ceoLog(ctx, ceo, powerWar, "arms", a.SystemID, 0,
					"paged %d member(s) for %s — %d answered, %d on hand of %d needed",
					paged, strings.TrimSpace(a.Why), st.Answered, count, st.Need)
			}
		}
		stats = append(stats, st)
	}

	armsMu.Lock()
	for key := range armsRolled {
		if !live[key] {
			delete(armsRolled, key)
		}
	}
	for id, t := range armsHolds {
		if t <= now {
			delete(armsHolds, id)
		}
	}
	armsStats, armsByCorp = stats, held
	armsMu.Unlock()
}

func init() {
	registerCharForget("armsHolds", func(id int64) {
		armsMu.Lock()
		delete(armsHolds, id)
		for _, r := range armsRolled {
			delete(r, id)
		}
		armsMu.Unlock()
	})
}
