package rolebots

// Envoys: who carries a letter (corpletters.go), and how. An envoy is an errand,
// not a job. The diplomacy pass hands a letter to one idle member (no officer, no
// seat, a ship under it) by giving it the diplomacy family's one seat
// (seatEnvoy). Its next plan pass sees the seat (envoyErrandDue), flies to the
// other corporation's headquarters, lands the letter (deliverLetter), flies home
// and hands the seat back. An envoy killed on the way is a letter lost and an
// incident against its killer (envoyLost, from fileKillmail). A letter nobody can
// carry (no idle member, ship or route) goes straight to the other desk
// (dispatchLetter). The ladder row sits after the starbase errands and ahead of
// gang duty: a letter in hand has a three-day expiry.

import (
	"context"
	"fmt"
)

// dispatchEnvoy hands a drafted letter to an idle member. False when nobody
// can carry it, in which case the caller delivers it directly.
func (m *Mod) dispatchEnvoy(ctx context.Context, c *corpCEO, l *corpLetter) bool {
	if !relEnvoysOn() {
		return false
	}
	target := m.corpHQStation(ctx, l.ToCorpID)
	if target == 0 {
		return false
	}
	charID := m.pickEnvoy(ctx, c)
	if charID == 0 {
		return false
	}
	now := clock.Unix()
	if err := m.setEnvoySeat(ctx, c.CorpID, charID, true, now); err != nil {
		rlogf("rolebots: envoy seat corp %d pilot %d: %v", c.CorpID, charID, err)
		return false
	}
	if err := m.setLetterStatus(ctx, l.ID, letterSent, `envoyCharID = ?, sentAt = ?`, charID, now); err != nil {
		rlogf("rolebots: envoy letter %d: %v", l.ID, err)
		_ = m.setEnvoySeat(ctx, c.CorpID, charID, false, now)
		return false
	}
	l.Status, l.EnvoyCharID, l.SentAt = letterSent, charID, now
	m.ceoLog(ctx, c, powerDiplomacy, "envoy", charID, 0,
		"sent %s to %s with the letter proposing %s", m.charNameOr(ctx, charID),
		corpNameOf(ctx, m.db, l.ToCorpID), letterKindWords(l.Kind))
	m.logBot(charID, logInfo, "diplomacy", "carrying a letter to %s", corpNameOf(ctx, m.db, l.ToCorpID))
	return true
}

// pickEnvoy is one idle, enabled, seatless member of the corporation that is not
// its CEO, the travelled and the leaders first, so the envoy is one that gets
// about.
func (m *Mod) pickEnvoy(ctx context.Context, c *corpCEO) int64 {
	rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID FROM character_ ch
		  JOIN rolebot_characters b ON b.characterID = ch.characterID AND b.enabled = 1
		  JOIN rolebot_state s ON s.characterID = ch.characterID AND s.phase = ?
		  LEFT JOIN rolebot_corp_posts p ON p.characterID = ch.characterID AND p.corporationID = ch.corporationID
		 WHERE ch.corporationID = ? AND ch.characterID <> ? AND p.characterID IS NULL
		 LIMIT 40`, string(TaskIdle), c.CorpID, c.CharID)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var best int64
	bestScore := -1e9
	for rows.Next() {
		var id int64
		if rows.Scan(&id) != nil {
			continue
		}
		traits, err := getTraits(ctx, m.db, id)
		if err != nil {
			continue
		}
		tn := resolveTuning(traits)
		p := pilotIdentity{CharID: id, Traits: traits, Tune: tn}
		score := p.trait(TraitWanderlust) + 0.5*p.trait(TraitLeadership)
		if score > bestScore {
			best, bestScore = id, score
		}
	}
	return best
}

// setEnvoySeat gives or takes the diplomacy family's one seat, keeping the
// family's other rows (another envoy on another road) intact.
func (m *Mod) setEnvoySeat(ctx context.Context, corpID, charID int64, on bool, now int64) error {
	want := m.familyPostsOf(ctx, corpID, famDiplomacy)
	if on {
		want[charID] = corpPost{CorpID: corpID, CharID: charID, Post: seatEnvoy, Since: now, Perf: 1}
	} else {
		delete(want, charID)
	}
	return m.savePosts(ctx, corpID, famDiplomacy, want)
}

// envoyLetterFor is the letter this pilot is carrying, if any.
func (m *Mod) envoyLetterFor(ctx context.Context, charID int64) *corpLetter {
	ls, err := m.lettersWhere(ctx, `envoyCharID = ? AND status = ?`, charID, letterSent)
	if err != nil || len(ls) == 0 {
		return nil
	}
	return ls[0]
}

// envoyErrandDue is plan()'s hook: a pilot holding the envoy seat is routed to
// the letter's destination. Free on the common pass (a map read).
func (m *Mod) envoyErrandDue(ctx context.Context, b *botRun) (bool, error) {
	if b == nil || b.State == nil || b.CorpID == 0 {
		return false, nil
	}
	if postIn(b.CharID, b.CorpID, famDiplomacy) != seatEnvoy {
		return false, nil
	}
	st := b.State
	d := &st.Data
	if d.EnvoyLetterID != 0 && (st.Phase == string(TaskEnvoyDeliver) || st.Phase == string(TaskEnvoyHome) ||
		d.NextPhase == string(TaskEnvoyDeliver) || d.NextPhase == string(TaskEnvoyHome)) {
		return true, nil // already on the road
	}
	l := m.envoyLetterFor(ctx, b.CharID)
	if l == nil {
		// The letter closed under it (answered, expired, withdrawn): the seat
		// is stale. Hand it back and carry on.
		_ = m.setEnvoySeat(ctx, b.CorpID, b.CharID, false, clock.Unix())
		return false, nil
	}
	target := m.corpHQStation(ctx, l.ToCorpID)
	targetSys := m.stationSystemID(ctx, target)
	if target == 0 || targetSys == 0 {
		return false, m.envoyGiveUp(ctx, b, l, "the other corporation has no address to carry it to")
	}
	// A ship to carry it in: a capsule is no envoy. The runabout rule the
	// patent errand uses (shuttle.go).
	if b.ShipID == 0 || m.isCapsule(ctx, b.ShipID) {
		if _, ok := m.ensureRunabout(ctx, b, "carrying a letter to "+corpNameOf(ctx, m.db, l.ToCorpID)); !ok {
			return false, m.envoyGiveUp(ctx, b, l, "it has no ship to carry it in")
		}
	}
	d.EnvoyLetterID = l.ID
	d.EnvoyStationID, d.EnvoySysID = target, targetSys
	d.EnvoyHomeStationID = b.StationID
	if d.EnvoyHomeStationID == 0 {
		d.EnvoyHomeStationID = m.corpHQStation(ctx, b.CorpID)
	}
	d.EnvoyHomeSysID = m.stationSystemID(ctx, d.EnvoyHomeStationID)
	st.Activity = ActCourier
	if err := m.beginTravel(ctx, b, targetSys, target, string(TaskEnvoyDeliver)); err != nil {
		if ctx.Err() != nil {
			return true, err
		}
		return false, m.envoyGiveUp(ctx, b, l, fmt.Sprintf("no route to %s (%v)", m.nameOfSystem(ctx, targetSys), err))
	}
	return true, nil
}

// envoyGiveUp drops a trip that cannot be made: the letter goes straight to
// the other desk so the proposal still arrives, and the seat comes back.
func (m *Mod) envoyGiveUp(ctx context.Context, b *botRun, l *corpLetter, why string) error {
	m.logBot(b.CharID, logInfo, "diplomacy", "cannot carry the letter — %s; it goes by mail instead", why)
	m.deliverLetter(ctx, l, b.CharID)
	return m.finishEnvoyTrip(ctx, b)
}

// envoyStep advances the trip: deliver at the destination, then go home.
func (m *Mod) envoyStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	switch TaskKind(st.Phase) {
	case TaskEnvoyDeliver:
		if d.EnvoyLetterID == 0 {
			return false, m.finishEnvoyTrip(ctx, b)
		}
		if b.StationID != d.EnvoyStationID {
			return false, m.beginTravel(ctx, b, d.EnvoySysID, d.EnvoyStationID, string(TaskEnvoyDeliver))
		}
		l, _ := m.letterByID(ctx, d.EnvoyLetterID)
		if l == nil || l.Status != letterSent {
			// Closed under it on the road: nothing to deliver.
			return false, m.envoyHome(ctx, b)
		}
		m.deliverLetter(ctx, l, b.CharID)
		m.logBot(b.CharID, logInfo, "diplomacy", "delivered the letter to %s at %s",
			corpNameOf(ctx, m.db, l.ToCorpID), m.nameOfStation(ctx, b.StationID))
		if c, _ := getCEO(ctx, m.db, l.FromCorpID); c != nil {
			m.ceoLog(ctx, c, powerDiplomacy, "deliver", l.ToCorpID, 0,
				"%s delivered the letter proposing %s to %s", m.charNameOr(ctx, b.CharID),
				letterKindWords(l.Kind), corpNameOf(ctx, m.db, l.ToCorpID))
		}
		return false, m.envoyHome(ctx, b)
	case TaskEnvoyHome:
		if d.EnvoyHomeStationID == 0 || b.StationID == d.EnvoyHomeStationID {
			return false, m.finishEnvoyTrip(ctx, b)
		}
		return false, m.beginTravel(ctx, b, d.EnvoyHomeSysID, d.EnvoyHomeStationID, string(TaskEnvoyHome))
	}
	return false, m.finishEnvoyTrip(ctx, b)
}

// envoyHome turns the envoy around.
func (m *Mod) envoyHome(ctx context.Context, b *botRun) error {
	d := &b.State.Data
	if d.EnvoyHomeStationID == 0 || d.EnvoyHomeSysID == 0 {
		return m.finishEnvoyTrip(ctx, b)
	}
	return m.beginTravel(ctx, b, d.EnvoyHomeSysID, d.EnvoyHomeStationID, string(TaskEnvoyHome))
}

// finishEnvoyTrip hands the seat back and the pilot to the planner.
func (m *Mod) finishEnvoyTrip(ctx context.Context, b *botRun) error {
	_ = m.setEnvoySeat(ctx, b.CorpID, b.CharID, false, clock.Unix())
	st := b.State
	st.Data.EnvoyState = EnvoyState{}
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	return saveState(ctx, m.db, st)
}

// envoyLost is the road's verdict: a pilot carrying a letter died. The
// letter is lost, the seat comes back, and whoever did it has a mark on the
// ledger beside the kill itself. Called from fileKillmail's goroutine.
func (m *Mod) envoyLost(ctx context.Context, charID, corpID, killID, sysID int64, attackerCorps []int64, value float64) {
	if corpID == 0 || !hasPost(charID, corpID, seatEnvoy) {
		return
	}
	l := m.envoyLetterFor(ctx, charID)
	_ = m.setEnvoySeat(ctx, corpID, charID, false, clock.Unix())
	if l == nil {
		return
	}
	_ = m.setLetterStatus(ctx, l.ID, letterLost, `answeredAt = ?, reason = ?`, clock.Unix(), "the envoy was killed on the road")
	for _, ac := range attackerCorps {
		m.noteCorpIncidentBoth(ctx, corpID, ac, incEnvoyKill, value, sysID, killID,
			fmt.Sprintf("envoykill:%d", killID), "our envoy "+m.charNameOr(ctx, charID))
	}
	if c, _ := getCEO(ctx, m.db, corpID); c != nil {
		m.ceoLog(ctx, c, powerDiplomacy, "envoylost", charID, 0,
			"lost %s on the road with the letter to %s", m.charNameOr(ctx, charID), corpNameOf(ctx, m.db, l.ToCorpID))
	}
}

// clearEnvoySeats hands back the seats of letters that closed without their
// envoy (expired, withdrawn).
func (m *Mod) clearEnvoySeats(ctx context.Context, corpID int64) {
	want := m.familyPostsOf(ctx, corpID, famDiplomacy)
	if len(want) == 0 {
		return
	}
	changed := false
	for charID := range want {
		if m.envoyLetterFor(ctx, charID) == nil {
			delete(want, charID)
			changed = true
		}
	}
	if changed {
		_ = m.savePosts(ctx, corpID, famDiplomacy, want)
	}
}

// detailEnvoy is the panel's one-line description of an envoy's trip.
func detailEnvoy(m *Mod, ctx context.Context, st *SimState) string {
	d := st.Data
	if d.EnvoyLetterID == 0 {
		return ""
	}
	switch st.Phase {
	case string(TaskEnvoyDeliver):
		return fmt.Sprintf("carrying a letter to %s", m.nameOfStation(ctx, d.EnvoyStationID))
	case string(TaskEnvoyHome):
		return "coming home from an embassy"
	}
	return ""
}
