package rolebots

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Exchanges: bots talking to each other and to the pilot they just locked. chatter.go is one
// bot saying one queued line; this is a conversation of a few beats with seconds between,
// started by a moment that will not wait (a first lock, a kill, a member under fire). Every
// engagement a bot starts goes out in Local with the target's name linked and its motive.
// Rules shared with chatter.go:
//   - Nothing is said where no human can hear it (chatterWatched, Registry.HasLiveListener).
//   - Corp chat spends the queued lines' budget (chatterBook). Local has a per-system
//     ceiling and a per-speaker gap.
//   - A line is a template with named slots; a slot that cannot be filled rejects the
//     template. Pools are keyed (event, motive, voice), looked up from specific to general.

// chance rolls a percentage dial.
func chance(pct *atomic.Int64) bool {
	return rand.Intn(100) < int(pct.Load())
}

// ── events, voices, parties ─────────────────────────────────────────────────

// chatEvent is what is happening. Pools are keyed on it first.
type chatEvent string

const (
	// Local.
	evEngage     chatEvent = "engage"     // aggressor, on the first lock
	evVictim     chatEvent = "victim"     // a bot victim answers the engage line
	evComeback   chatEvent = "comeback"   // the aggressor answers the victim
	evUnderFire  chatEvent = "underfire"  // a bot hit with no exchange open
	evBystander  chatEvent = "bystander"  // somebody else in Local remarks on it
	evGloat      chatEvent = "gloat"      // the killer
	evLament     chatEvent = "lament"     // the dead, from the pod
	evSpite      chatEvent = "spite"      // the dead, when the killer gloated about race
	evCamp       chatEvent = "camp"       // a pirate setting up
	evRansom     chatEvent = "ransom"     // the demand
	evRansomPaid chatEvent = "ransompaid" // the money landed
	evRansomLate chatEvent = "ransomlate" // it did not
	evExecute    chatEvent = "execute"    // a pirate that never meant to talk
	evGoodFight  chatEvent = "goodfight"  // the dead, when the ledger read the fight as fair (grace_ledger.go)

	// Corp. A member's lost ship is the queued pers.loss line; evCorpConsole
	// is the corpmate's answer to it.
	evCorpOrder   chatEvent = "corporder"   // a department head calls a member out
	evCorpAck     chatEvent = "corpack"     // the member answers
	evCorpGrumble chatEvent = "corpgrumble" // ... less graciously
	evCorpFollow  chatEvent = "corpfollow"  // the head has the last word
	evCorpHelp    chatEvent = "corphelp"    // a member under fire
	evCorpHelpYes chatEvent = "corphelpyes" // security is coming
	evCorpHelpNo  chatEvent = "corphelpno"  // nobody is coming
	evCorpConsole chatEvent = "corpconsole" // somebody answers a member's loss
	evCorpClaim   chatEvent = "corpclaim"   // a member takes a work order
	evCorpClaimOK chatEvent = "corpclaimok" // the head acknowledges

	// A player answering a bot: keyed "answer:<intent>".
	evAnswer chatEvent = "answer"
)

func answerEvent(i retortIntent) chatEvent { return evAnswer + ":" + chatEvent(i) }

// chatVoice is the register a pilot speaks in, fixed per pilot so a bot always sounds the
// same. A trait colours what a pilot says about its own work, a voice how it talks to
// somebody it is about to shoot.
type chatVoice string

const (
	voiceAny       chatVoice = ""
	voiceCold      chatVoice = "cold"      // clipped, professional menace
	voiceBrash     chatVoice = "brash"     // loud, mocking, enjoys it
	voiceTerse     chatVoice = "terse"     // three words or fewer where possible
	voiceZealot    chatVoice = "zealot"    // Amarr: scripture, the Reclaiming
	voiceCorporate chatVoice = "corporate" // Caldari: contracts, efficiency, the State
	voiceShowman   chatVoice = "showman"   // Gallente: liberty, flair, audience
	voiceTribal    chatVoice = "tribal"    // Minmatar: the tribes, freedom, old debts
	voicePirate    chatVoice = "pirate"    // the trade
)

// Race IDs are posplan.go's and skills.go's constants, names corps.go's
// raceNames; bloodlines are chrBloodlines.
var bloodlineNames = map[int64]string{
	1: "Deteis", 2: "Civire", 3: "Sebiestor", 4: "Brutor", 5: "Amarr", 6: "Ni-Kunni",
	7: "Gallente", 8: "Intaki", 11: "Achura", 12: "Jin-Mei", 13: "Khanid", 14: "Vherokior",
}

// bloodlineCharType is a bloodline's character typeID (1373-1386): the speaker icon and
// the type half of a name link, read without the entity query charTypeID makes.
var bloodlineCharType = map[int64]int64{
	5: 1373, 6: 1374, 2: 1375, 1: 1376, 7: 1377, 8: 1378, 3: 1379, 4: 1380,
	9: 1381, 10: 1382, 11: 1383, 12: 1384, 13: 1385, 14: 1386,
}

// voiceFor derives a pilot's register. Deterministic in the character ID, so
// it needs nothing stored: a third of pilots speak as their people, a strong
// xenophobe almost always does, an aggressive one is brash, and the rest are
// cold or terse.
func voiceFor(charID, race int64, tn Tuning, pirate bool) chatVoice {
	h := splitmix64(uint64(charID) ^ 0x63686174746572) // "chatter"
	roll := int(h % 100)
	if pirate {
		if roll < 60 {
			return voicePirate
		}
		if roll < 85 {
			return voiceBrash
		}
		return voiceCold
	}
	empire := map[int64]chatVoice{
		raceAmarr: voiceZealot, raceCaldari: voiceCorporate,
		raceGallente: voiceShowman, raceMinmatar: voiceTribal,
	}[race]
	share := 35
	if xenoPhobeStage(tn.Xenophobia) >= xenoStageOwnRace {
		share = 80
	}
	if empire != "" && roll < share {
		return empire
	}
	if tn.NeutralAggression >= 0.5 && roll%3 == 0 {
		return voiceBrash
	}
	switch roll % 4 {
	case 0:
		return voiceBrash
	case 1:
		return voiceTerse
	}
	return voiceCold
}

// chatParty is one side of an exchange. Bot false is a human: never spoken
// for, only spoken to.
type chatParty struct {
	CharID      int64
	Name        string
	CorpID      int64
	RaceID      int64
	BloodlineID int64
	ShipTypeID  int64
	Voice       chatVoice
	Phobia      float64
	Philia      float64 // the xenophile pole: curiosity about other peoples
	Bot         bool
	// Alive, when set, is asked before every line: a bot that has since
	// docked, died twice or left the system does not finish its sentence.
	Alive func() bool
}

func (p *chatParty) typeID() int64 {
	if t := bloodlineCharType[p.BloodlineID]; t != 0 {
		return t
	}
	return 1374
}

func (p *chatParty) link() string { return charLink(p.CharID, p.typeID(), p.Name) }

// partyFromEmbodied is a live projection as a party. Map reads only.
func partyFromEmbodied(eb *embodiedBot) *chatParty {
	p := &chatParty{
		CharID: eb.charID, Name: eb.charName, CorpID: eb.corpID,
		RaceID: raceOf(eb.charID), BloodlineID: bloodlineOf(eb.charID),
		Phobia: eb.tune.Xenophobia, Philia: eb.tune.Xenophilia, Bot: true,
	}
	if eb.ent != nil {
		p.ShipTypeID = eb.ent.Slim.TypeID
	}
	p.Voice = voiceFor(eb.charID, p.RaceID, eb.tune, eb.prey)
	sysID := eb.systemID
	p.Alive = func() bool {
		lp := liveProjection(p.CharID)
		return lp != nil && lp.systemID == sysID
	}
	return p
}

// partyFromRun is an abstract-tier pilot as a party.
func partyFromRun(b *botRun, pirate bool) *chatParty {
	p := &chatParty{
		CharID: b.CharID, Name: b.Name, CorpID: b.CorpID,
		RaceID: raceOf(b.CharID), BloodlineID: bloodlineOf(b.CharID),
		Phobia: b.Tune.Xenophobia, Philia: b.Tune.Xenophilia, Bot: true,
	}
	p.Voice = voiceFor(b.CharID, p.RaceID, b.Tune, pirate)
	return p
}

// partyFromID is anybody else: a pilot across a grid, a human, a bystander from a Local
// member list. bot says whether the module may speak for it.
func partyFromID(charID int64, name string, corpID, shipTypeID int64, bot bool) *chatParty {
	p := &chatParty{
		CharID: charID, Name: name, CorpID: corpID, ShipTypeID: shipTypeID,
		RaceID: raceOf(charID), BloodlineID: bloodlineOf(charID), Bot: bot,
	}
	if bot {
		p.Voice = voiceFor(charID, p.RaceID, defaultTuning(), false)
	}
	return p
}

// ── slots and rendering ─────────────────────────────────────────────────────

// chatSlots is the fill for a template. Missing keys reject the template.
type chatSlots map[string]string

var slotRe = regexp.MustCompile(`\{([a-z]+)\}`)

// altRe is one alternation group, "[a|b|c]": a template says one option, picked per
// render. Options may hold slots and may be empty; groups do not nest.
// TestChatterAlternationWellFormed holds the syntax.
var altRe = regexp.MustCompile(`\[([^\[\]]*)\]`)

// expandAlt replaces every alternation group with one of its options, pick(n) choosing
// among n. It runs before the slots are filled, so a slot value is never read as syntax.
// An empty option leaves no doubled or edge spaces behind.
func expandAlt(tpl string, pick func(n int) int) string {
	if !strings.Contains(tpl, "[") {
		return tpl
	}
	out := altRe.ReplaceAllStringFunc(tpl, func(m string) string {
		opts := strings.Split(m[1:len(m)-1], "|")
		return opts[pick(len(opts))]
	})
	return strings.Join(strings.Fields(out), " ")
}

// renderLine fills a template, saying one option of each alternation group. ok is false
// when any slot has no value, and the caller tries the next template.
func renderLine(tpl string, s chatSlots) (string, bool) {
	tpl = expandAlt(tpl, rand.Intn)
	ok := true
	out := slotRe.ReplaceAllStringFunc(tpl, func(m string) string {
		v, have := s[m[1:len(m)-1]]
		if !have || v == "" {
			ok = false
		}
		return v
	})
	return out, ok
}

// slotsFor builds the fill for one speaker addressing one party. The {slur} slot is
// present only when the race switch is on and the speaker's prejudice is strong enough,
// which scales the harshness of race lines with the xenophobe's stage.
func (m *Mod) slotsFor(ctx context.Context, speaker, about *chatParty, extra chatSlots) chatSlots {
	s := chatSlots{}
	if speaker != nil {
		s["self"] = speaker.Name
		s["myrace"] = raceNames[speaker.RaceID]
		s["mybloodline"] = bloodlineNames[speaker.BloodlineID]
	}
	if about != nil {
		s["target"] = about.link()
		s["name"] = about.Name
		s["race"] = raceNames[about.RaceID]
		s["bloodline"] = bloodlineNames[about.BloodlineID]
		if about.ShipTypeID != 0 {
			s["ship"] = m.hullClassName(ctx, about.ShipTypeID)
			s["hull"] = m.typeNameOr(ctx, about.ShipTypeID)
		}
		if speaker != nil && chatterRaceOn.Load() {
			if sl := slurFor(speaker, about); sl != "" {
				s["slur"] = sl
			}
		}
	}
	for k, v := range extra {
		s[k] = v
	}
	return s
}

// hullClassName is a ship's group name in lower case ("frigate"), cached.
func (m *Mod) hullClassName(ctx context.Context, typeID int64) string {
	if v, ok := hullClassCache.Load(typeID); ok {
		return v.(string)
	}
	var name string
	_ = m.db.QueryRowContext(ctx, `
		SELECT g.groupName FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID
		 WHERE t.typeID = ?`, typeID).Scan(&name)
	name = strings.ToLower(name)
	if name == "capsule" {
		name = "pod"
	}
	hullClassCache.Store(typeID, name)
	return name
}

var hullClassCache sync.Map

// ── pools ───────────────────────────────────────────────────────────────────

type poolKey struct {
	ev    chatEvent
	why   motive
	voice chatVoice
}

var chatPools = map[poolKey][]string{}

// addLines registers lines into one pool cell. Called from the lines files'
// init(), never at runtime. A line may hold "[a|b]" alternation groups (expandAlt).
func addLines(ev chatEvent, why motive, voice chatVoice, lines ...string) {
	k := poolKey{ev, why, voice}
	chatPools[k] = append(chatPools[k], lines...)
}

// candidates is every template that could answer (ev, why, voice), most
// specific cell first. The cells are merged, not chosen between: a pilot with
// its own voice still draws from the general lines now and then, which is what
// keeps a zealot from repeating its six scriptures all evening.
func candidates(ev chatEvent, why motive, voice chatVoice) [][]string {
	var out [][]string
	seen := map[poolKey]bool{}
	for _, k := range []poolKey{
		{ev, why, voice}, {ev, why, voiceAny}, {ev, motiveNone, voice}, {ev, motiveNone, voiceAny},
	} {
		if seen[k] {
			continue
		}
		seen[k] = true
		if lines := chatPools[k]; len(lines) > 0 {
			out = append(out, lines)
		}
	}
	return out
}

// voiceTmplID is a template as chatterBook keys it, so an exchange line and a
// queued line can never collide on an ID.
func voiceTmplID(tpl string) string { return "v:" + tpl }

// heardLately reports whether this template was heard recently: Local's own 15-minute
// guard, in a corp channel the queued lines' day, and the addressed human pilot's day
// (listener 0 when nobody human is addressed).
func heardLately(channel, listener int64, tpl string) bool {
	return localBook.recentlySaid(channel, tpl) ||
		localBook.heardByLately(listener, tpl) ||
		chatterBook.heard(clock.Now(), channel, voiceTmplID(tpl), chatterRepeatWindow)
}

// pickChatLine is pickChatLineFor with no human addressee.
func pickChatLine(channel int64, ev chatEvent, why motive, voice chatVoice, s chatSlots) (line, tpl string) {
	return pickChatLineFor(channel, 0, ev, why, voice, s)
}

// pickChatLineFor draws a renderable, not-recently-heard line and its template (the caller
// marks it said once the budget lets it out). listener is the human pilot the line
// addresses, 0 for none. The most specific non-empty cell is tried first: three tries in
// four, else the general pool.
func pickChatLineFor(channel, listener int64, ev chatEvent, why motive, voice chatVoice, s chatSlots) (line, tpl string) {
	tiers := candidates(ev, why, voice)
	if len(tiers) == 0 {
		return "", ""
	}
	order := make([]int, len(tiers))
	for i := range order {
		order[i] = i
	}
	// Occasionally lead with the general pool.
	if len(order) > 1 && rand.Intn(4) == 0 {
		order[0], order[len(order)-1] = order[len(order)-1], order[0]
	}
	for _, ti := range order {
		lines := tiers[ti]
		start := rand.Intn(len(lines))
		for j := range lines {
			tpl := lines[(start+j)%len(lines)]
			if heardLately(channel, listener, tpl) {
				continue
			}
			if out, ok := renderLine(tpl, s); ok {
				return out, tpl
			}
		}
	}
	// Everything renderable was heard recently: say something anyway rather than nothing,
	// the line heard longest ago, the most specific cell winning a tie.
	var bestAt time.Time
	for _, lines := range tiers {
		for _, t := range lines {
			if !lineRenders(t, s) {
				continue
			}
			at := localBook.lastHeard(channel, listener, t)
			if tpl == "" || at.Before(bestAt) {
				tpl, bestAt = t, at
			}
		}
	}
	if tpl == "" {
		return "", ""
	}
	line, _ = renderLine(tpl, s)
	return line, tpl
}

// lineRenders reports whether every slot of a template has a value in s.
func lineRenders(tpl string, s chatSlots) bool {
	for _, m := range slotRe.FindAllStringSubmatch(tpl, -1) {
		if s[m[1]] == "" {
			return false
		}
	}
	return true
}

// ── Local's budget ──────────────────────────────────────────────────────────

// localLedger paces Local, which the hourly corp-channel budget (chatterBook) does not fit:
// a fight is over in a minute. It also keeps the 15-minute template memory pickChatLine
// reads for both kinds of channel, and the day-long memory of what each addressed human
// pilot was told, which follows the pilot from system to system.
type localLedger struct {
	mu        sync.Mutex
	channelAt map[int64][]time.Time
	speakerAt map[int64]time.Time
	saidAt    map[string]time.Time // channel|template
	heardBy   map[string]time.Time // pilot|template
}

func newLocalLedger() *localLedger {
	return &localLedger{
		channelAt: map[int64][]time.Time{},
		speakerAt: map[int64]time.Time{},
		saidAt:    map[string]time.Time{},
		heardBy:   map[string]time.Time{},
	}
}

// localBook is the live ledger. It trims itself by size (and on the chatter
// sweep), so it is exempt from the forget lint like chatterBook.
var localBook = newLocalLedger()

func saidKey(channel int64, tpl string) string { return fmt.Sprintf("%d|%s", channel, tpl) }

func (l *localLedger) recentlySaid(channel int64, tpl string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.saidAt[saidKey(channel, tpl)]
	return ok && clock.Now().Sub(at) < localRepeatWindow
}

func (l *localLedger) noteSaid(channel int64, tpl string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := clock.Now()
	l.saidAt[saidKey(channel, tpl)] = now
	if len(l.saidAt) > 8192 {
		l.trimLocked(now)
	}
}

// heardByLately reports whether a human pilot was told this template within the
// listener window. Pilot 0 (no human addressee) is never remembered.
func (l *localLedger) heardByLately(pilot int64, tpl string) bool {
	if pilot == 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.heardBy[saidKey(pilot, tpl)]
	return ok && clock.Now().Sub(at) < listenerRepeatWindow
}

// noteHeardBy records that a human pilot was told a template.
func (l *localLedger) noteHeardBy(pilot int64, tpl string) {
	if pilot == 0 || tpl == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := clock.Now()
	l.heardBy[saidKey(pilot, tpl)] = now
	if len(l.heardBy) > 8192 {
		l.trimLocked(now)
	}
}

// lastHeard is the latest time the channel or the pilot heard a template; the zero time
// when neither did within its window.
func (l *localLedger) lastHeard(channel, pilot int64, tpl string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	var last time.Time
	if at, ok := l.saidAt[saidKey(channel, tpl)]; ok {
		last = at
	}
	if pilot != 0 {
		if at, ok := l.heardBy[saidKey(pilot, tpl)]; ok && at.After(last) {
			last = at
		}
	}
	return last
}

// allow spends one line of a system's budget. urgent skips the speaker gap,
// never the ceiling.
func (l *localLedger) allow(channel, speaker int64, ceiling int, urgent bool) bool {
	now := clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.channelAt[channel][:0]
	for _, t := range l.channelAt[channel] {
		if now.Sub(t) < localCeilingWindow {
			kept = append(kept, t)
		}
	}
	l.channelAt[channel] = kept
	if len(kept) >= ceiling {
		return false
	}
	if !urgent {
		if gap := time.Duration(chatterGapSec.Load()) * time.Second; gap > 0 {
			if last, ok := l.speakerAt[speaker]; ok && now.Sub(last) < gap {
				return false
			}
		}
	}
	l.speakerAt[speaker] = now
	l.channelAt[channel] = append(l.channelAt[channel], now)
	if len(l.speakerAt) > 8192 {
		l.trimLocked(now)
	}
	return true
}

// trimLocked drops what nothing will read again. Caller holds l.mu.
func (l *localLedger) trimLocked(now time.Time) {
	for k, t := range l.saidAt {
		if now.Sub(t) >= localRepeatWindow {
			delete(l.saidAt, k)
		}
	}
	for k, t := range l.heardBy {
		if now.Sub(t) >= listenerRepeatWindow {
			delete(l.heardBy, k)
		}
	}
	for id, t := range l.speakerAt {
		if now.Sub(t) > localRepeatWindow {
			delete(l.speakerAt, id)
		}
	}
	for id, ts := range l.channelAt {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > localCeilingWindow {
			delete(l.channelAt, id)
		}
	}
}

func (l *localLedger) sweep(now time.Time) {
	l.mu.Lock()
	l.trimLocked(now)
	l.mu.Unlock()
}

// ── saying it ───────────────────────────────────────────────────────────────

// hearable reports whether any human is in the channel to hear a line: the
// audience poll for a corporation's channel, the registry for Local.
func (m *Mod) hearable(channel int64, corp bool) bool {
	if m.lsc == nil || channel == 0 || !Chatter() {
		return false
	}
	if corp {
		return chatterWatched(channel)
	}
	return m.lsc.HasLiveListener(channel)
}

// chatSay is one line, start to finish: the speaker must be a bot and still there, the
// channel must have a listener and budget, and a template must render. follow marks a reply
// to the beat before it, which skips the gaps, never the ceiling. It returns what was said
// (empty for nothing) and blocks only on the hull lookup: call it off the combat goroutine.
func (m *Mod) chatSay(ctx context.Context, channel int64, corp bool, speaker, about *chatParty,
	ev chatEvent, why motive, extra chatSlots, urgent, follow bool) string {
	if speaker == nil || !speaker.Bot || channel == 0 {
		return ""
	}
	// An alliance channel is a corp-shaped channel (seated speakers, the
	// hourly budget) with its own switch.
	ally := corp && isAllianceChannel(channel)
	switch {
	case ally:
		if !chatterAllianceOn.Load() {
			return ""
		}
	case corp:
		if !chatterCorpOn.Load() {
			return ""
		}
	default:
		if !chatterLocalOn.Load() {
			return ""
		}
	}
	if speaker.Alive != nil && !speaker.Alive() {
		return ""
	}
	if !m.hearable(channel, corp) {
		return ""
	}
	kind := "x." + string(ev)
	if xk := exchangeKindFor(ev, ally); xk != "" {
		kind = xk
	}
	if chatterKindOff(kind) {
		chatterDrop("muted")
		return ""
	}
	slots := m.slotsFor(ctx, speaker, about, extra)
	if ally {
		// Allies keep a floor of civility: a xenophobe may be cold to a member corporation of
		// another people, never insulting, so the slur slot is removed.
		delete(slots, "slur")
	}
	var listener int64
	if about != nil && !about.Bot {
		listener = about.CharID
	}
	line, tpl := pickChatLineFor(channel, listener, ev, why, speaker.Voice, slots)
	if line == "" {
		chatterDrop("render")
		return ""
	}
	if corp {
		// A corporation's channel: the speaker must sit in it, and the line
		// spends the same budget the queued lines do.
		sp, ok := chatterSpeakerFor(channel, speaker.CharID)
		if !ok {
			chatterDrop("speaker")
			return ""
		}
		class := chatterClassEvent
		switch {
		case follow:
			class = chatterClassFollow
		case urgent:
			class = chatterClassReply
		}
		// The kind's cooldown is the opener's: an answer six seconds later
		// is the same conversation, not the channel hearing it twice.
		lim := chatterLimitsIn(channel, kind)
		if follow {
			lim.KindCooldown = 0
		}
		if why := chatterBook.allow(clock.Now(), channel, sp.CharID, kind, voiceTmplID(tpl), class, lim); why != "" {
			chatterDrop(why)
			return ""
		}
		localBook.noteSaid(channel, tpl)
		localBook.noteHeardBy(listener, tpl)
		if !m.chatterSay(ctx, sp, channel, kind, voiceTmplID(tpl), line, class, chatterSayPrechecked) {
			return ""
		}
		return line
	}
	if !localBook.allow(channel, speaker.CharID, int(chatterLocalRate.Load()), urgent || follow) {
		chatterDrop("local")
		return ""
	}
	localBook.noteSaid(channel, tpl)
	localBook.noteHeardBy(listener, tpl)
	m.lsc.SpeakBot(channel, speaker.CharID, speaker.Name, speaker.CorpID, speaker.typeID(), line)
	chatterRecord(chatterSaid{At: clock.Unix(), ChannelID: channel, CorpID: speaker.CorpID,
		SpeakerID: speaker.CharID, Speaker: speaker.Name, Kind: kind, Text: line})
	m.logBot(speaker.CharID, logInfo, "chatter", "said in Local %d: %s", channel, line)
	return line
}

// beat is one line of an exchange, said `after` the previous one.
type beat struct {
	after   time.Duration
	speaker *chatParty
	about   *chatParty
	ev      chatEvent
	why     motive
	extra   chatSlots
	urgent  bool
	// odds, when non-zero, is the percentage chance this beat is said at all;
	// a skipped beat ends the exchange, since what follows answers it.
	odds int
	// optional beats may be skipped without ending the exchange.
	optional bool
}

// exchangeSlots bounds the exchanges in flight at once. runExchange says a sequence of
// beats on its own goroutine: a beat whose speaker said nothing (budget, silence, a dead
// pilot) ends the exchange, since the next line would answer something nobody saw.
var exchangeSlots = make(chan struct{}, 8)

func (m *Mod) runExchange(name string, channel int64, corp bool, beats []beat) {
	if len(beats) == 0 || !m.hearable(channel, corp) {
		return
	}
	// Each exchange is a goroutine sleeping between its lines, so eight run at a time and an
	// exchange that finds no seat is not said.
	select {
	case exchangeSlots <- struct{}{}:
	default:
		return
	}
	goSafe("chatter "+name, func() {
		defer func() { <-exchangeSlots }()
		started := false
		for _, b := range beats {
			time.Sleep(jitter(b.after))
			if b.odds > 0 && rand.Intn(100) >= b.odds {
				if b.optional {
					continue
				}
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			said := m.chatSay(ctx, channel, corp, b.speaker, b.about, b.ev, b.why, b.extra, b.urgent, started)
			cancel()
			if said == "" {
				if !b.optional {
					return
				}
				continue
			}
			started = true
		}
	})
}

// jitter spreads a delay ±30% so two exchanges started on one tick do not
// land their lines on the same second.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	f := 0.7 + 0.6*rand.Float64()
	return time.Duration(float64(d) * f)
}

// ── who has spoken to whom ──────────────────────────────────────────────────

// A bark at a HUMAN opens a short window in which that human's reply in Local
// is understood to be addressed to the bot (chatter_retort.go).
const barkWindow = 90 * time.Second

type barkKey struct{ system, pilot int64 }

type bark struct {
	bot   *chatParty
	about *chatParty
	why   motive
	extra chatSlots
	at    time.Time
}

var (
	barkMu sync.Mutex
	barks  = map[barkKey]bark{}

	// exchanges is the (aggressor, victim) pairs an exchange has been opened
	// for, so a target re-locked on every combat tick is barked at once.
	exchangeMu sync.Mutex
	exchanges  = map[[2]int64]time.Time{}
)

const exchangeCooldown = 10 * time.Minute

// claimExchange reports whether an exchange between these two may open now,
// and marks it opened.
func claimExchange(a, b int64) bool {
	now := clock.Now()
	exchangeMu.Lock()
	defer exchangeMu.Unlock()
	k := [2]int64{a, b}
	if at, ok := exchanges[k]; ok && now.Sub(at) < exchangeCooldown {
		return false
	}
	exchanges[k] = now
	if len(exchanges) > 8192 {
		for k, t := range exchanges {
			if now.Sub(t) >= exchangeCooldown {
				delete(exchanges, k)
			}
		}
	}
	return true
}

func noteBark(system int64, bot, about *chatParty, why motive, extra chatSlots) {
	if about == nil || about.Bot {
		return
	}
	barkMu.Lock()
	defer barkMu.Unlock()
	now := clock.Now()
	barks[barkKey{system, about.CharID}] = bark{bot: bot, about: about, why: why, extra: extra, at: now}
	if len(barks) > 4096 {
		for k, b := range barks {
			if now.Sub(b.at) > barkWindow {
				delete(barks, k)
			}
		}
	}
}

// openBark is the live bark at a human in a system, if any.
func openBark(system, pilot int64) (bark, bool) {
	barkMu.Lock()
	defer barkMu.Unlock()
	b, ok := barks[barkKey{system, pilot}]
	if !ok || clock.Now().Sub(b.at) > barkWindow {
		return bark{}, false
	}
	return b, true
}

// sweepExchanges drops expired barks, exchange claims and grudges. The maps also trim by
// size, but a quiet server never reaches the size.
func sweepExchanges(now time.Time) {
	barkMu.Lock()
	for k, b := range barks {
		if now.Sub(b.at) > barkWindow {
			delete(barks, k)
		}
	}
	barkMu.Unlock()
	exchangeMu.Lock()
	for k, t := range exchanges {
		if now.Sub(t) >= exchangeCooldown {
			delete(exchanges, k)
		}
	}
	exchangeMu.Unlock()
	localBook.sweep(now)
	lastMotive.sweep(now)
	corpHelpAt.sweep(now)
	sweepGrudges(now)
	sweepRetorts(now)
	sweepMarketChatter(now)
}
