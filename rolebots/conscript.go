package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/gang"
	"gurimoon/modapi"
	"gurimoon/server/gangsta"
	"gurimoon/server/lsc"
)

// Conscription: a bot flying in a player's fleet, under that player's orders.
//
// The operator's fleets (rolebot_gangs, gangform.go) are separate: there the module decides who
// flies with whom, here a human invites a pilot and orders it in fleet chat. The two share no
// table and no lifecycle; a conscript never carries a rolebot_gangs row (see rolebot_conscripts
// in tables.go). The core seam is mod-rebuild.ps1 section 1h: the gangsta.Module* hooks for
// invites, gangsta.BotLeave/BotMembers/GangOf/GangLeader for membership, modapi.Gangs(), and
// lsc.GangChannelID with SpeakBot/JoinBot for fleet chat.

const (
	// settingConscript is the master switch, persisted in server_settings and off until an
	// operator turns it on.
	settingConscript = "rolebots.conscript.enabled"
	// settingConscriptMaxFleet caps one player's crew and settingConscriptMaxTotal caps the whole
	// server's.
	settingConscriptMaxFleet = "rolebots.conscript.maxPerFleet"
	settingConscriptMaxTotal = "rolebots.conscript.maxTotal"
	// settingConscriptIdleTTL is how long a pilot serves without hearing an order before it
	// excuses itself, in seconds.
	settingConscriptIdleTTL = "rolebots.conscript.idleTTL"
	// settingConscriptDefence is the autonomous half: whether a servant steps in when somebody
	// opens up on its fleet unasked. On by default; off gives a passive escort.
	settingConscriptDefence = "rolebots.conscript.defendFleet"

	defaultConscriptMaxFleet = 5
	defaultConscriptMaxTotal = 50
	defaultConscriptIdleTTL  = 3600

	// conscriptEveryTicks is the lane cadence (ticks are one second). The lane does bookkeeping
	// only; fast reactions live on the one-second grid lane.
	conscriptEveryTicks = 5
)

// ActConscript is the activity slug a pilot carries while serving. It is never offered by the
// appetite draw (as the gang seats, fitjob.go); it lets the panel and detailConscript say what
// the pilot is doing. A mining order swaps the activity to ActMining before buying anything.
const ActConscript = "conscript"

// The conscript roles a player may assign are the operator fleet's vocabulary (gangs.go), so a
// seat means the same whoever asked for it, and seatActivityFor turns one into work.
var conscriptRoles = gangRoles

var (
	conscriptOn        atomic.Bool
	conscriptDefendOn  atomic.Bool
	conscriptMaxFleet  atomic.Int64
	conscriptMaxTotal  atomic.Int64
	conscriptIdleTTL   atomic.Int64
	conscriptLaneBusy  atomic.Bool
	conscriptStartOnce sync.Once
)

// Conscription reports whether players may command bots at all.
func Conscription() bool { return conscriptOn.Load() }

func setConscription(on bool) { conscriptOn.Store(on) }

// ConscriptDefence reports whether servants defend their fleet unasked.
func ConscriptDefence() bool { return conscriptDefendOn.Load() }

func setConscriptDefence(on bool) { conscriptDefendOn.Store(on) }

func setConscriptMaxFleet(n int) {
	if n < 1 {
		n = defaultConscriptMaxFleet
	}
	conscriptMaxFleet.Store(int64(n))
}

func setConscriptMaxTotal(n int) {
	if n < 1 {
		n = defaultConscriptMaxTotal
	}
	conscriptMaxTotal.Store(int64(n))
}

func setConscriptIdleTTL(n int) {
	if n < 60 {
		n = defaultConscriptIdleTTL
	}
	conscriptIdleTTL.Store(int64(n))
}

// loadConscriptSettings reads the live dials. Called from Init before the lane starts, and
// again from the panel on save.
func (m *Mod) loadConscriptSettings(ctx context.Context) {
	setConscriptMaxFleet(defaultConscriptMaxFleet)
	setConscriptMaxTotal(defaultConscriptMaxTotal)
	setConscriptIdleTTL(defaultConscriptIdleTTL)
	setConscriptDefence(true)
	// On by default: a fleeted rolebot is expected to answer. The panel's switch turns it off.
	setConscription(true)
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingConscript); err != nil {
		rlogf("rolebots: read conscription setting: %v (leaving it on)", err)
	} else if ok {
		setConscription(v == "1")
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingConscriptDefence); err == nil && ok {
		setConscriptDefence(v == "1")
	}
	readInt := func(key string, set func(int)) {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, key)
		if err != nil || !ok {
			return
		}
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			set(n)
		}
	}
	readInt(settingConscriptMaxFleet, setConscriptMaxFleet)
	readInt(settingConscriptMaxTotal, setConscriptMaxTotal)
	readInt(settingConscriptIdleTTL, setConscriptIdleTTL)

	// The voice budget (conscript_voice.go). On by default, since a silent crew looks broken;
	// every limit can be set down to silence.
	setConscriptVoice(true)
	setConscriptVoiceGap(defaultVoiceGapSec)
	setConscriptVoiceRate(defaultVoicePerMin)
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingConscriptVoice); err == nil && ok {
		setConscriptVoice(v == "1")
	}
	readInt(settingConscriptVoiceGap, setConscriptVoiceGap)
	readInt(settingConscriptVoicePerMin, setConscriptVoiceRate)
}

// ── the roster row ──────────────────────────────────────────────────────────

// Conscript is one pilot's service under one player.
type Conscript struct {
	CharID        int64
	GangID        int64
	BossCharID    int64
	BossCorpID    int64
	Role          string
	DutyStationID int64
	JoinedAt      int64
	LastOrderAt   int64
}

func loadConscripts(ctx context.Context, db *sql.DB) ([]*Conscript, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT characterID, gangID, bossCharID, bossCorpID, role,
		       dutyStationID, joinedAt, lastOrderAt
		  FROM rolebot_conscripts ORDER BY gangID, characterID`)
	if err != nil {
		return nil, fmt.Errorf("loadConscripts: %w", err)
	}
	defer rows.Close()
	var out []*Conscript
	for rows.Next() {
		k := &Conscript{}
		if err := rows.Scan(&k.CharID, &k.GangID, &k.BossCharID, &k.BossCorpID,
			&k.Role, &k.DutyStationID, &k.JoinedAt, &k.LastOrderAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ── the live fleet cache ────────────────────────────────────────────────────
//
// Every conscript question at combat-tick rate goes through this cache, never the DB: is this
// pilot serving, who else is in its fleet, who is the boss. It is refreshed once per lane pass.

type conscriptCrew struct {
	GangID     int64
	BossCharID int64
	BossCorpID int64
	Members    []*Conscript
}

var (
	conscriptMu    sync.RWMutex
	conscriptByBot = map[int64]*Conscript{}
	conscriptCrews = map[int64]*conscriptCrew{} // by gangID
)

func setConscriptCache(list []*Conscript) {
	byBot := make(map[int64]*Conscript, len(list))
	crews := map[int64]*conscriptCrew{}
	for _, k := range list {
		byBot[k.CharID] = k
		c := crews[k.GangID]
		if c == nil {
			c = &conscriptCrew{GangID: k.GangID, BossCharID: k.BossCharID, BossCorpID: k.BossCorpID}
			crews[k.GangID] = c
		}
		c.Members = append(c.Members, k)
	}
	conscriptMu.Lock()
	conscriptByBot, conscriptCrews = byBot, crews
	conscriptMu.Unlock()
}

// conscriptOf reports this pilot's service, or nil. Cache read, no I/O.
func conscriptOf(charID int64) *Conscript {
	conscriptMu.RLock()
	defer conscriptMu.RUnlock()
	return conscriptByBot[charID]
}

// crewOf reports a fleet's whole bot crew, or nil.
func crewOf(gangID int64) *conscriptCrew {
	conscriptMu.RLock()
	defer conscriptMu.RUnlock()
	return conscriptCrews[gangID]
}

// conscriptCharIDs is every pilot currently serving somebody. The grid lane reads it to widen
// its roster, so a conscript far away still gets a turn every second.
func conscriptCharIDs() []int64 {
	conscriptMu.RLock()
	defer conscriptMu.RUnlock()
	out := make([]int64, 0, len(conscriptByBot))
	for id := range conscriptByBot {
		out = append(out, id)
	}
	return out
}

func conscriptCount() int {
	conscriptMu.RLock()
	defer conscriptMu.RUnlock()
	return len(conscriptByBot)
}

// ── registration ────────────────────────────────────────────────────────────

// registerConscription publishes the module's answers to the core's fleet questions. Called
// from Init.
func (m *Mod) registerConscription() {
	gangsta.ModuleOwns = m.ownsCharacter
	gangsta.ModuleGangCandidate = m.conscriptCandidate
	gangsta.ModuleGangJoined = m.conscriptJoined
	// A commander's "Warp Gang" (conscript_embodied.go). The core warps its own connected pilots
	// and hands this the rest of the gang.
	gang.ModuleWarp = m.onGangWarp
	// Leaves, kicks, disbands and leadership changes (conscript_command.go). The lane's sweep runs
	// the same reconcile as a backstop.
	m.startGangEvents()
	gangsta.ModuleGangEvent = m.onGangEvent
	// The commander's overview tags (fctags.go).
	m.installFCTags()

	// The ears (conscript_chat.go) register here so the hook and the policy that answers it go in
	// together.
	m.startChatIntake()
}

// ownsCharacter reports whether this character is one of ours. It uses the cache the mail gate
// uses (mailsuppress.go): same question, same table, kept fresh by every roster mutation. It
// runs for every offline invite target, so it must not cost a query; on a DB error the last
// known roster stands.
func (m *Mod) ownsCharacter(charID int64) bool {
	return m.isBotMailbox(charID)
}

// conscriptCandidate is the policy gate: will this pilot take the seat? It returns the pilot's
// corporation and an empty refusal on yes, or a sentence the inviter reads on no. See
// docs/comment-notes-b03.md#conscript-candidate-gate.
func (m *Mod) conscriptCandidate(gangID, inviterCharID, targetCharID int64) (int64, string) {
	if targetCharID == 0 || inviterCharID == 0 {
		return 0, "That pilot cannot be reached."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var name string
	var corpID int64
	err := m.db.QueryRowContext(ctx, `
		SELECT c.characterName, c.corporationID
		  FROM rolebot_characters b
		  JOIN character_ c ON c.characterID = b.characterID
		 WHERE b.enabled = 1 AND b.characterID = ?`, targetCharID).Scan(&name, &corpID)
	if err != nil {
		// Not an enabled rolebot: assigned to the module but switched off, or gone.
		return 0, "That pilot is not taking fleet invitations."
	}
	if !Master() || !Conscription() {
		return 0, name + " is not taking fleet invitations right now."
	}
	if popIsOffline(targetCharID) {
		return 0, name + " is not logged in."
	}

	// Already serving another fleet. Saying yes would move the pilot between fleets mid-order.
	if k := conscriptOf(targetCharID); k != nil && k.GangID != gangID {
		return 0, name + " is already flying with another fleet."
	}
	// In a fleet the module opened for a capsuleer (fleetmirror.go): a real gang with a human in
	// it, and a seat there is kept.
	if real := gangsta.GangOf(targetCharID); real != 0 && conscriptOf(targetCharID) == nil {
		return 0, name + " is already flying in a fleet."
	}
	if conscriptCount() >= int(conscriptMaxTotal.Load()) && conscriptOf(targetCharID) == nil {
		return 0, name + " is not taking fleet invitations right now."
	}
	if gangID != 0 && m.crewSize(gangID) >= int(conscriptMaxFleet.Load()) {
		return 0, "Your fleet already has all the pilots it can command."
	}

	// Who is asking comes before what the pilot is doing: a stranger is refused whatever they
	// interrupt. The crew obeys the gang's leader (conscript_command.go), so a human leader other
	// than the inviter must pass too.
	if why := m.willFlyFor(ctx, targetCharID, corpID, inviterCharID); why != "" {
		return 0, name + why
	}
	if leader := gangsta.GangLeader(gangID); leader != 0 && leader != inviterCharID && !m.isSynthetic(leader) {
		if why := m.willFlyFor(ctx, targetCharID, corpID, leader); why != "" {
			return 0, name + why
		}
	}

	st, err := loadState(ctx, m.db, targetCharID)
	if err != nil || !interruptibleState(st) {
		phase := ""
		if st != nil {
			phase = st.Phase
		}
		m.logBot(targetCharID, logInfo, "conscript",
			"declined %d's fleet: mid-%s, which would strand something", inviterCharID, phase)
		return 0, name + " is in the middle of a job and cannot break off. Ask again once they have docked."
	}
	if st != nil && st.Data.GangID != 0 && mirrorHasRiders(st.Data.GangID) {
		return 0, name + " is flying with their corporation's fleet and a capsuleer is aboard it."
	}
	return corpID, ""
}

// ── who a pilot will fly with ───────────────────────────────────────────────

// willFlyFor is the acquaintance and hostility half of the invite gate: empty when this pilot
// flies for that human, else the rest of a sentence that follows the pilot's name.
func (m *Mod) willFlyFor(ctx context.Context, botCharID, botCorpID, humanID int64) string {
	acq := m.inviteAcquaintance(ctx, botCorpID, m.charCorpID(ctx, humanID))
	if acq.rel == inviteStranger && graceOpinionOf(botCharID, humanID) >= StandingLiked {
		// A stranger this pilot has come to respect (grace_ledger.go) is a friend here.
		acq.rel = inviteFriend
	}
	if acq.rel == inviteStranger {
		m.logBot(botCharID, logInfo, "conscript",
			"declined %d's fleet: not one of ours, not a friend", humanID)
		return " does not fly with strangers. They will join pilots of their own " +
			"corporation or alliance, pilots of their own empire, and friends of their corporation or alliance."
	}
	if m.inviteHostile(ctx, acq, botCharID, humanID) {
		m.logBot(botCharID, logInfo, "conscript", "declined %d's fleet: hostile", humanID)
		return " will not fly with you."
	}
	return ""
}

// inviteRel is how a pilot knows the player who invited it.
type inviteRel int

const (
	inviteStranger inviteRel = iota
	// inviteKin is the structural bloc (kinship.go): one corporation, one
	// alliance, or one employer faction.
	inviteKin
	// inviteFriend is a corporation its own directors hold in regard.
	inviteFriend
)

// inviteWelcome is the whole acquaintance rule, pure so the harness can pin every branch.
// regard is what the pilot's corporation, or its alliance, declares toward the inviter's
// (declaredRegard); StandingLiked or better is a friend. With no alliance, faction or declared
// friends a pilot is a stranger to everyone.
func inviteWelcome(botCorp, botAlliance, botFaction, invCorp, invAlliance, invFaction int64,
	regard float64) inviteRel {
	if sameBloc(botCorp, botAlliance, botFaction, invCorp, invAlliance, invFaction) {
		return inviteKin
	}
	if regard >= StandingLiked {
		return inviteFriend
	}
	return inviteStranger
}

// inviteAcq is what the invite gate learned about the two sides, so the friend test and the
// hostility veto read one set of facts.
type inviteAcq struct {
	rel              inviteRel
	botCorp, botAlly int64
	invCorp, invAlly int64
	regard           float64
	declared         bool
}

// inviteAcquaintance resolves both sides' alliances and the bot corporation's declared regard
// for the inviter, and classifies the pair. It reads crpStandings and allianceRelationships
// directly, not the relations snapshot (corprel.go), which is behind
// rolebots.relations.enabled and ships off; the invite path asks four single-row questions.
func (m *Mod) inviteAcquaintance(ctx context.Context, botCorp, invCorp int64) inviteAcq {
	q := inviteAcq{botCorp: botCorp, invCorp: invCorp}
	if botCorp == 0 || invCorp == 0 {
		return q
	}
	q.botAlly = m.corpAllianceID(ctx, botCorp)
	q.invAlly = m.corpAllianceID(ctx, invCorp)
	q.regard, q.declared = declaredRegard(botCorp, q.botAlly, invCorp, q.invAlly,
		func(fromIsAlliance bool, from, to int64) (float64, bool) {
			return m.declaredStanding(ctx, fromIsAlliance, from, to)
		})
	q.rel = inviteWelcome(botCorp, q.botAlly, m.corpFactionID(ctx, botCorp),
		invCorp, q.invAlly, m.corpFactionID(ctx, invCorp), q.regard)
	return q
}

// declaredRegard is what the bot's corporation, or failing that its alliance, has declared
// about the inviter's corporation or alliance. Precedence is relResolveDeclarations' (corprel.go):
// the first answer wins, so a corporation's own word outranks its bloc's. Only the bot's side
// counts. Pure over lookup.
func declaredRegard(botCorp, botAlly, invCorp, invAlly int64,
	lookup func(fromIsAlliance bool, from, to int64) (float64, bool)) (float64, bool) {
	steps := []struct {
		fromAlly bool
		from, to int64
	}{
		{false, botCorp, invCorp},
		{false, botCorp, invAlly},
		{true, botAlly, invCorp},
		{true, botAlly, invAlly},
	}
	for _, st := range steps {
		if st.from == 0 || st.to == 0 || st.from == st.to {
			continue
		}
		if v, ok := lookup(st.fromAlly, st.from, st.to); ok {
			return v, true
		}
	}
	return 0, false
}

// declaredStanding is one row of the declarations: a corporation's standing toward an owner,
// or an alliance's relationship with one. A missing table or row is neutral (corprel.go).
func (m *Mod) declaredStanding(ctx context.Context, fromIsAlliance bool, from, to int64) (float64, bool) {
	if fromIsAlliance {
		var rel int64
		if err := m.db.QueryRowContext(ctx,
			`SELECT relationship FROM allianceRelationships WHERE allianceID = ? AND toID = ?`,
			from, to).Scan(&rel); err != nil {
			return 0, false
		}
		v := allianceRelStanding(rel)
		return v, v != 0
	}
	var v float64
	if err := m.db.QueryRowContext(ctx,
		`SELECT standing FROM crpStandings WHERE corporationID = ? AND toID = ?`,
		from, to).Scan(&v); err != nil || v == 0 {
		return 0, false
	}
	return clampStanding(v), true
}

// inviteHostile is the veto that applies even to kin: a declared war between the two sides
// (HostileWarBetween, corporation and alliance IDs), a declaration of Disliked or worse, or a
// grudge standing. Neutral and warm pass. standingToPilot gets no xenophobia, since a stranger
// was already refused.
func (m *Mod) inviteHostile(ctx context.Context, q inviteAcq, botCharID, inviterCharID int64) bool {
	if q.declared && q.regard <= StandingDisliked {
		return true
	}
	if war, err := evedb.HostileWarBetween(ctx, m.db,
		[]int64{q.botCorp, q.botAlly}, []int64{q.invCorp, q.invAlly}); err == nil && war {
		return true
	}
	self := standingSelf{CharID: botCharID, CorpID: q.botCorp, FactionID: m.corpFactionID(ctx, q.botCorp)}
	return m.standingToPilot(ctx, self, inviterCharID) < 0
}

// crewSize counts the bots already serving in a fleet, cache first: the invite path should not
// hit the DB for it.
func (m *Mod) crewSize(gangID int64) int {
	if c := crewOf(gangID); c != nil {
		return len(c.Members)
	}
	return 0
}

// conscriptJoined is the follow-through once the pilot is a member and the humans have been
// told. The slow work lives here so the inviter's fleet window is not delayed.
func (m *Mod) conscriptJoined(gangID, botCharID, bossCharID, bossCorpID int64) {
	goSafe("conscript joined", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.enlist(ctx, gangID, botCharID, bossCharID, bossCorpID); err != nil {
			rlogf("rolebots: enlist %d into gang %d: %v", botCharID, gangID, err)
		}
	})
}

// ── enlisting and releasing ─────────────────────────────────────────────────

// enlist records the service and puts the pilot in fleet chat.
// enlist records the service and puts the pilot in fleet chat. The rolebot_gang_members row is
// written first: it decides which lane owns the pilot, and an operator-fleet gangID would put it
// on the slower roster cadence. A pilot cannot serve two masters.
func (m *Mod) enlist(ctx context.Context, gangID, botCharID, bossCharID, bossCorpID int64) error {
	if g, _, err := gangForChar(ctx, m.db, botCharID); err == nil && g != nil {
		if err := removeGangMember(ctx, m.db, g.GangID, botCharID); err != nil {
			rlogf("rolebots: conscript %d leaving operator gang %d: %v", botCharID, g.GangID, err)
		}
	}

	now := clock.Unix()
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_conscripts
			(characterID, gangID, bossCharID, bossCorpID, role, dutyStationID, joinedAt, lastOrderAt)
		VALUES (?, ?, ?, ?, '', 0, ?, ?)
		ON DUPLICATE KEY UPDATE gangID = VALUES(gangID), bossCharID = VALUES(bossCharID),
			bossCorpID = VALUES(bossCorpID), lastOrderAt = VALUES(lastOrderAt)`,
		botCharID, gangID, bossCharID, bossCorpID, now, now)
	if err != nil {
		return fmt.Errorf("enlist: %w", err)
	}

	m.refreshConscripts(ctx)
	m.joinFleetChat(ctx, gangID, botCharID)
	// The inviter may not lead; the crew follows the gang's human leader.
	m.reconcileCrew(ctx, gangID, gangsta.GangEvent{})
	k := conscriptOf(botCharID)
	if k == nil {
		return nil
	}
	bossCharID = k.BossCharID

	// The first bot to fly for a player tells them how to use one. It is keyed by the player, so
	// later pilots and fleets add nothing (conscript_voice.go).
	m.sendBriefing(ctx, botCharID, bossCharID, false)
	m.sayFleet(ctx, gangID, botCharID, true, "Reporting in. Say %q if you want the list of what I can do.", "help")

	m.logBot(botCharID, logInfo, "conscript", "joined %d's fleet (gang %d)", bossCharID, gangID)
	return nil
}

// joinFleetChat puts a synthetic member in the fleet's chat channel so the player sees the
// name and SpeakBot has somewhere to talk. It stays out of syncPresence, which reconciles
// against the DB location and would rebuild the channel each time the bot changed system.
func (m *Mod) joinFleetChat(ctx context.Context, gangID, botCharID int64) {
	if m.lsc == nil || gangID == 0 {
		return
	}
	chID := lsc.GangChannelID(gangID)
	if chID == 0 {
		return
	}
	name, corpID := m.charNameCorp(ctx, botCharID)
	m.lsc.JoinBot(chID, botCharID, 0, corpID, m.charTypeID(ctx, botCharID), 0, name)
}

func (m *Mod) leaveFleetChat(gangID, botCharID int64) {
	if m.lsc == nil || gangID == 0 {
		return
	}
	if chID := lsc.GangChannelID(gangID); chID != 0 {
		m.lsc.LeaveBot(chID, botCharID)
	}
}

// charNameCorp is the pair the chat registry wants for a synthetic member.
func (m *Mod) charNameCorp(ctx context.Context, charID int64) (string, int64) {
	var name string
	var corpID int64
	err := m.db.QueryRowContext(ctx,
		`SELECT characterName, corporationID FROM character_ WHERE characterID = ?`, charID).
		Scan(&name, &corpID)
	if err != nil {
		return "", 0
	}
	return name, corpID
}

// release ends one pilot's service and hands it back to itself. The gang seat is given up
// last: leaving can dissolve the fleet, and that notification should describe a pilot that
// has already stopped taking orders.
func (m *Mod) release(ctx context.Context, botCharID int64, why string) {
	k := conscriptOf(botCharID)
	gangID := int64(0)
	if k != nil {
		gangID = k.GangID
	}
	if gangID == 0 {
		gangID = gangsta.GangOf(botCharID)
	}

	// The pending instruction goes first, so the pilot's next turn cannot act on a stranger's
	// word after it stopped serving.
	dropOrder(botCharID)
	dropFireCall(botCharID)
	dropMoveCall(botCharID)

	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_conscripts WHERE characterID = ?`, botCharID); err != nil {
		rlogf("rolebots: release %d: %v", botCharID, err)
	}
	rosterCacheInvalidate() // rostercache.go
	// resetBot clears the phase and halt flag but idleData carries the standing order, so the
	// order must be wiped explicitly afterwards or a freed pilot acts on it for a stranger.
	if err := resetBot(ctx, m.db, botCharID); err != nil {
		rlogf("rolebots: release %d reset: %v", botCharID, err)
	}
	if st, err := loadState(ctx, m.db, botCharID); err == nil {
		clearStandingOrder(st)
		if err := saveState(ctx, m.db, st); err != nil {
			rlogf("rolebots: release %d clear order: %v", botCharID, err)
		}
	}
	m.leaveFleetChat(gangID, botCharID)
	gangsta.BotLeave(botCharID)
	m.refreshConscripts(ctx)
	m.logBot(botCharID, logInfo, "conscript", "released from gang %d: %s", gangID, why)
}

// refreshConscripts reloads the cache from the table.
func (m *Mod) refreshConscripts(ctx context.Context) {
	rosterCacheInvalidate() // the roster's commander column moved (rostercache.go)
	list, err := loadConscripts(ctx, m.db)
	if err != nil {
		rlogf("rolebots: refresh conscripts: %v", err)
		return
	}
	setConscriptCache(list)
}

// ── boot cleanup ────────────────────────────────────────────────────────────

// releaseAllConscripts empties the table at startup. Every row is stale: gang.Registry is in
// memory and restarts its IDs at 1, so a surviving row would alias another fleet. The pilots
// are re-idled, since a bot in a conscript.* phase has no task to advance it.
func (m *Mod) releaseAllConscripts(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `SELECT characterID FROM rolebot_conscripts`)
	if err != nil {
		rlogf("rolebots: boot conscript sweep: %v", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	if _, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_conscripts`); err != nil {
		rlogf("rolebots: boot conscript sweep delete: %v", err)
	}
	rosterCacheInvalidate() // rostercache.go
	// Anything left mid-order, with or without a row: a phase nothing advances is a wedged pilot.
	stranded, err := m.db.QueryContext(ctx,
		`SELECT characterID FROM rolebot_state WHERE phase LIKE 'conscript.%'`)
	if err == nil {
		for stranded.Next() {
			var id int64
			if err := stranded.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		stranded.Close()
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if err := resetBot(ctx, m.db, id); err != nil {
			rlogf("rolebots: boot conscript reset %d: %v", id, err)
			continue
		}
		// As in release(): idleData carries the order, so re-idling alone would leave last session's
		// instruction on the pilot.
		if st, err := loadState(ctx, m.db, id); err == nil {
			clearStandingOrder(st)
			if err := saveState(ctx, m.db, st); err != nil {
				rlogf("rolebots: boot conscript clear order %d: %v", id, err)
			}
		}
	}
	setConscriptCache(nil)
	if len(seen) > 0 {
		rlogf("rolebots: released %d conscript(s) left over from the last run", len(seen))
	}
}

// ── the lane ────────────────────────────────────────────────────────────────

// startConscription registers the bookkeeping lane: cache refresh, fleet-chat reconciliation
// and the release sweep. Orders are consumed on the sim and grid lanes, not here.
func (m *Mod) startConscription() {
	if m.tick == nil {
		return
	}
	conscriptStartOnce.Do(func() {
		m.registerPeriodic("rolebots-conscript", conscriptEveryTicks, func(uint64) bool {
			if !Master() {
				return true
			}
			if !conscriptLaneBusy.CompareAndSwap(false, true) {
				return true
			}
			go func() {
				defer conscriptLaneBusy.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				m.conscriptPass(ctx)
			}()
			return true
		})
	})
}

func (m *Mod) conscriptPass(ctx context.Context) {
	m.refreshConscripts(ctx)
	// Both ledgers are keyed by pilot and line and nothing else frees an entry.
	sweepVoiceLedger()
	sweepOrderRate()
	// The gang-event hook's backstop: a commander who left or a leader nobody announced.
	for _, gid := range conscriptGangIDs() {
		m.reconcileCrew(ctx, gid, gangsta.GangEvent{})
	}
	pruneFCTags()
	for _, k := range conscriptSnapshot() {
		if why, done := m.conscriptExpired(k); done {
			m.release(ctx, k.CharID, why)
			continue
		}
		// Idempotent, and heals a channel a reconnecting player rebuilt.
		m.joinFleetChat(ctx, k.GangID, k.CharID)
	}
}

func conscriptSnapshot() []*Conscript {
	conscriptMu.RLock()
	defer conscriptMu.RUnlock()
	out := make([]*Conscript, 0, len(conscriptByBot))
	for _, k := range conscriptByBot {
		out = append(out, k)
	}
	return out
}

// conscriptExpired is the whole release rule, in the order the reasons stop being true. A
// fleet whose only human logged out survives on bot members alone, since gang.Registry only
// dissolves at one member; releasing the crew lets it collapse.
func (m *Mod) conscriptExpired(k *Conscript) (string, bool) {
	if !Conscription() {
		return "conscription switched off", true
	}
	live := gangsta.GangOf(k.CharID)
	if live == 0 || live != k.GangID {
		return "no longer in that fleet", true
	}
	if len(gangsta.HumanMembersOnline(k.GangID)) == 0 {
		return "the fleet has nobody flying it", true
	}
	if ttl := conscriptIdleTTL.Load(); ttl > 0 && clock.Unix()-k.LastOrderAt > ttl {
		return "no orders for a long time", true
	}
	return "", false
}

// ── the boss ────────────────────────────────────────────────────────────────

// bossLockedTargets is what the commanding player has locked, as entity IDs. Empty when the
// boss is docked, offline or locking nothing, which "attack" must fall back from.
func bossLockedTargets(bossCharID int64) []uint32 {
	h := modapi.Gangs()
	if h.LockedTargets == nil || bossCharID == 0 {
		return nil
	}
	return h.LockedTargets(bossCharID)
}
