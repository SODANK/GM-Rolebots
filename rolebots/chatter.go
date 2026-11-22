package rolebots

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// Chatter: bots talking in a channel a person is reading. This layer sits between "something
// happened in this corporation" and "one bot says one short line about it", and most of it is
// about not saying it: a line goes only where a person is connected, at most a handful an
// hour, never two in a burst, never the same wording twice in a day.
// Producers call chatterNote with a typed event (chatter_lines.go is the catalogue); it drops
// at once unless a human is live in the channel (chatter_audience.go). A lane every fifteen
// seconds pops at most one queued event per channel, renders it in the speaker's voice, asks
// the budget (chatter_budget.go) and speaks. Replies to humans ride the fleet-chat intake
// (chatter_reply.go). The queue is corp chat only (every producer sets ChannelID = CorpID);
// conversations are chatter_voice.go's exchanges and spend the same budget there.

const (
	settingChatterOn       = "rolebots.chatter.on"
	settingChatterPerHour  = "rolebots.chatter.perHour"
	settingChatterPilotGap = "rolebots.chatter.pilotGapSec"
	settingChatterMinGap   = "rolebots.chatter.minGapSec"
	settingChatterGreet    = "rolebots.chatter.greet"
	settingChatterReplies  = "rolebots.chatter.replies"
	settingChatterFarewell = "rolebots.chatter.farewell"
	settingChatterKindsOff = "rolebots.chatter.kindsOff"

	defaultChatterPerHour  = 20
	defaultChatterPilotGap = 300
	defaultChatterMinGap   = 45
	chatterGreetReserve    = 2
	chatterRepeatWindow    = 24 * time.Hour

	chatterEveryTicks    = 15
	chatterLaneTimeout   = 30 * time.Second
	chatterQueueDepth    = 16
	chatterEventTTL      = 10 * time.Minute
	chatterGreetDelayMin = 5
	chatterGreetDelayMax = 30
	chatterGreetOncePer  = 6 * time.Hour
	chatterReplyGap      = 20 * time.Second
	chatterOfferTTL      = 10 * time.Minute
	chatterRingSize      = 100
	chatterTraitTTL      = 10 * time.Minute
)

// The exchange dials (chatter_voice.go). All under the master switch above;
// each part on by default once the master is, so switching chatter on gives
// the whole of it and every part can then be taken back out.
const (
	settingChatterLocal     = "rolebots.chatter.local"
	settingChatterRace      = "rolebots.chatter.race"
	settingChatterCorp      = "rolebots.chatter.corp"
	settingChatterRetorts   = "rolebots.chatter.retorts"
	settingChatterLocalRate = "rolebots.chatter.localPerMin"
	settingChatterGap       = "rolebots.chatter.speakerGapSec"
	settingChatterBystander = "rolebots.chatter.bystanderPct"
	settingChatterReply     = "rolebots.chatter.replyPct"
	// settingChatterOpenerGap is one pilot's floor between two engagement openers in Local.
	// Openers are urgent (they skip the speaker gap so the line lands with the first shot),
	// so without this a gate camper taking a new war target every minute held Local at its
	// ceiling on its own. 0 turns the floor off.
	settingChatterOpenerGap = "rolebots.chatter.openerGapSec"

	defaultChatterLocalRate = 6
	defaultChatterGapSec    = 30
	defaultChatterOpenerGap = 240
	defaultChatterBystander = 25
	defaultChatterReply     = 70

	localRepeatWindow  = 15 * time.Minute
	localCeilingWindow = time.Minute
	// listenerRepeatWindow is how long a human pilot addressed by a line is spared that
	// template again, in any channel.
	listenerRepeatWindow = 24 * time.Hour
)

// The alliance channel and the banter lane (chatter_alliance.go,
// chatter_banter.go). Same master switch; each on by default once it is.
const (
	settingChatterAlliance      = "rolebots.chatter.alliance"
	settingChatterAlliancePerHr = "rolebots.chatter.alliancePerHour"
	settingChatterAllianceGreet = "rolebots.chatter.allianceGreet"
	settingChatterBanterLocal   = "rolebots.chatter.banterLocal"
	settingChatterBanterAlly    = "rolebots.chatter.banterAlliance"
	settingChatterBanterMin     = "rolebots.chatter.banterMin"
	settingChatterRibPct        = "rolebots.chatter.ribPct"
	settingChatterShopPct       = "rolebots.chatter.shopPct"

	defaultChatterAlliancePerHr = 12
	defaultChatterBanterMin     = 25
	defaultChatterRibPct        = 25
	defaultChatterShopPct       = 15
)

var (
	chatterAllianceOn      atomic.Bool
	chatterAlliancePerHr   atomic.Int64
	chatterAllianceGreetOn atomic.Bool
	chatterBanterLocalOn   atomic.Bool
	chatterBanterAllyOn    atomic.Bool
	chatterBanterMin       atomic.Int64
	chatterRibPct          atomic.Int64
	chatterShopPct         atomic.Int64
)

func setChatterAlliance(on bool)      { chatterAllianceOn.Store(on) }
func setChatterAllianceGreet(on bool) { chatterAllianceGreetOn.Store(on) }
func setChatterBanterLocal(on bool)   { chatterBanterLocalOn.Store(on) }
func setChatterBanterAlly(on bool)    { chatterBanterAllyOn.Store(on) }
func setChatterRibPct(n int)          { chatterRibPct.Store(int64(clampPct(n))) }
func setChatterShopPct(n int)         { chatterShopPct.Store(int64(clampPct(n))) }

func setChatterAlliancePerHr(n int) {
	if n < 0 {
		n = defaultChatterAlliancePerHr
	}
	chatterAlliancePerHr.Store(int64(n))
}

func setChatterBanterMin(n int) {
	if n < 1 {
		n = defaultChatterBanterMin
	}
	chatterBanterMin.Store(int64(n))
}

func setChatterAllianceDefaults() {
	setChatterAlliance(true)
	setChatterAlliancePerHr(defaultChatterAlliancePerHr)
	setChatterAllianceGreet(true)
	setChatterBanterLocal(true)
	setChatterBanterAlly(true)
	setChatterBanterMin(defaultChatterBanterMin)
	setChatterRibPct(defaultChatterRibPct)
	setChatterShopPct(defaultChatterShopPct)
}

var (
	chatterLocalOn   atomic.Bool
	chatterRaceOn    atomic.Bool
	chatterCorpOn    atomic.Bool
	chatterRetortsOn atomic.Bool
	chatterLocalRate atomic.Int64
	chatterGapSec    atomic.Int64
	chatterBystander atomic.Int64
	chatterReplyPct  atomic.Int64
)

func setChatterLocal(on bool)   { chatterLocalOn.Store(on) }
func setChatterRace(on bool)    { chatterRaceOn.Store(on) }
func setChatterCorp(on bool)    { chatterCorpOn.Store(on) }
func setChatterRetorts(on bool) { chatterRetortsOn.Store(on) }

func setChatterLocalRate(n int) {
	if n < 1 {
		n = defaultChatterLocalRate
	}
	chatterLocalRate.Store(int64(n))
}

func setChatterGap(n int) {
	if n < 0 {
		n = defaultChatterGapSec
	}
	chatterGapSec.Store(int64(n))
}

// chatterOpenerGapSec is settingChatterOpenerGap's live value, in seconds.
var chatterOpenerGapSec atomic.Int64

// setChatterOpenerGap takes seconds; negative restores the default, 0 turns the floor off.
func setChatterOpenerGap(n int) {
	if n < 0 {
		n = defaultChatterOpenerGap
	}
	chatterOpenerGapSec.Store(int64(n))
}

func clampPct(n int) int {
	return max(0, min(100, n))
}

func setChatterBystander(n int) { chatterBystander.Store(int64(clampPct(n))) }
func setChatterReply(n int)     { chatterReplyPct.Store(int64(clampPct(n))) }

// setChatterExchangeDefaults is the exchange half of the defaults, apart so
// a test (or a boot that never reaches loadChatterSettings) has them too.
func setChatterExchangeDefaults() {
	setChatterLocal(true)
	setChatterRace(true)
	setChatterCorp(true)
	setChatterRetorts(true)
	setChatterLocalRate(defaultChatterLocalRate)
	setChatterGap(defaultChatterGapSec)
	setChatterOpenerGap(defaultChatterOpenerGap)
	setChatterBystander(defaultChatterBystander)
	setChatterReply(defaultChatterReply)
	setChatterMarketDefaults()
	setChatterAllianceDefaults()
}

func init() { setChatterExchangeDefaults() }

var (
	chatterOnV       atomic.Bool
	chatterGreetOn   atomic.Bool
	chatterRepliesOn atomic.Bool
	chatterFarewell  atomic.Bool
	chatterPerHour   atomic.Int64
	chatterPilotGap  atomic.Int64
	chatterMinGap    atomic.Int64
	chatterKindsOff  atomic.Pointer[map[string]bool]
	chatterBusy      atomic.Bool
	chatterOnce      sync.Once
)

// Chatter reports the master switch.
func Chatter() bool { return chatterOnV.Load() }

func setChatterOn(on bool)       { chatterOnV.Store(on) }
func setChatterGreet(on bool)    { chatterGreetOn.Store(on) }
func setChatterReplies(on bool)  { chatterRepliesOn.Store(on) }
func setChatterFarewell(on bool) { chatterFarewell.Store(on) }

func setChatterPerHour(n int) {
	if n < 0 {
		n = defaultChatterPerHour
	}
	chatterPerHour.Store(int64(n))
}

func setChatterPilotGap(n int) {
	if n < 0 {
		n = defaultChatterPilotGap
	}
	chatterPilotGap.Store(int64(n))
}

func setChatterMinGap(n int) {
	if n < 0 {
		n = defaultChatterMinGap
	}
	chatterMinGap.Store(int64(n))
}

// setChatterKindsOff takes the muted kinds as a comma list; unknown names
// are dropped rather than kept, so the stored value is always a set the
// catalogue knows.
func setChatterKindsOff(csv string) {
	off := map[string]bool{}
	for _, k := range strings.Split(csv, ",") {
		k = strings.TrimSpace(k)
		if _, known := chatterKinds[k]; known {
			off[k] = true
		} else if _, known := chatterExchangeKinds[k]; known {
			off[k] = true
		}
	}
	chatterKindsOff.Store(&off)
}

func chatterKindOff(kind string) bool {
	p := chatterKindsOff.Load()
	return p != nil && (*p)[kind]
}

// chatterKindsOffCSV is the stored form, sorted, for the panel and the save.
func chatterKindsOffCSV() string {
	p := chatterKindsOff.Load()
	if p == nil {
		return ""
	}
	var out []string
	for k := range *p {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// chatterLimitsFor is the budget as the dials stand, with the kind's own
// cooldown folded in.
func chatterLimitsFor(kind string) chatterLimits {
	lim := chatterLimits{
		PerHour:      int(chatterPerHour.Load()),
		GreetReserve: chatterGreetReserve,
		MinGap:       time.Duration(chatterMinGap.Load()) * time.Second,
		PilotGap:     time.Duration(chatterPilotGap.Load()) * time.Second,
		RepeatWindow: chatterRepeatWindow,
	}
	if k, ok := chatterKinds[kind]; ok {
		lim.KindCooldown = k.Cooldown
	} else if k, ok := chatterExchangeKinds[kind]; ok {
		lim.KindCooldown = k.Cooldown
	}
	return lim
}

// chatterLimitsIn is chatterLimitsFor for a channel: an alliance channel has
// its own hourly ceiling, because it is shared by every member corporation's
// bots and would otherwise talk as much as all their corp channels together.
func chatterLimitsIn(channel int64, kind string) chatterLimits {
	lim := chatterLimitsFor(kind)
	if isAllianceChannel(channel) {
		lim.PerHour = int(chatterAlliancePerHr.Load())
	}
	return lim
}

// loadChatterSettings reads the dials. Off by default: a new voice in a
// player's corp chat is something an operator switches on having read the
// panel, not something a deploy does to them.
func (m *Mod) loadChatterSettings(ctx context.Context) {
	setChatterOn(false)
	setChatterGreet(true)
	setChatterReplies(true)
	setChatterFarewell(false)
	setChatterPerHour(defaultChatterPerHour)
	setChatterPilotGap(defaultChatterPilotGap)
	setChatterMinGap(defaultChatterMinGap)
	setChatterKindsOff("")
	setChatterExchangeDefaults()
	readBool := func(key string, set func(bool)) {
		if v, ok, err := evedb.GetServerSetting(ctx, m.db, key); err == nil && ok {
			set(truthySetting(v))
		}
	}
	readInt := func(key string, set func(int)) {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, key)
		if err != nil || !ok {
			return
		}
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			set(n)
		}
	}
	readBool(settingChatterOn, setChatterOn)
	readBool(settingChatterGreet, setChatterGreet)
	readBool(settingChatterReplies, setChatterReplies)
	readBool(settingChatterFarewell, setChatterFarewell)
	readInt(settingChatterPerHour, setChatterPerHour)
	readInt(settingChatterPilotGap, setChatterPilotGap)
	readInt(settingChatterMinGap, setChatterMinGap)
	readBool(settingChatterLocal, setChatterLocal)
	readBool(settingChatterRace, setChatterRace)
	readBool(settingChatterCorp, setChatterCorp)
	readBool(settingChatterRetorts, setChatterRetorts)
	readInt(settingChatterLocalRate, setChatterLocalRate)
	readInt(settingChatterGap, setChatterGap)
	readInt(settingChatterOpenerGap, setChatterOpenerGap)
	readInt(settingChatterBystander, setChatterBystander)
	readInt(settingChatterReply, setChatterReply)
	readBool(settingChatterMarket, setChatterMarket)
	readBool(settingChatterMarketRegion, setChatterMarketRegion)
	readInt(settingChatterMarketAdPct, setChatterMarketAdPct)
	readInt(settingChatterMarketRemarkPct, setChatterMarketRemarkPct)
	readInt(settingChatterMarketLookMin, setChatterMarketLookMin)
	readBool(settingChatterAlliance, setChatterAlliance)
	readInt(settingChatterAlliancePerHr, setChatterAlliancePerHr)
	readBool(settingChatterAllianceGreet, setChatterAllianceGreet)
	readBool(settingChatterBanterLocal, setChatterBanterLocal)
	readBool(settingChatterBanterAlly, setChatterBanterAlly)
	readInt(settingChatterBanterMin, setChatterBanterMin)
	readInt(settingChatterRibPct, setChatterRibPct)
	readInt(settingChatterShopPct, setChatterShopPct)
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingChatterKindsOff); err == nil && ok {
		setChatterKindsOff(v)
	}
}

// ── events ──────────────────────────────────────────────────────────────────

// chatterEvent is one thing worth a line.
type chatterEvent struct {
	Kind      string
	ChannelID int64 // where it would be said; corp chat = CorpID
	CorpID    int64
	ActorID   int64 // who did it: the speaker
	At        int64 // clock.Unix() when it happened
	Priority  int   // from the kind when 0
	Slots     map[string]string
	Ref       int64 // kind-specific: module gangID for fleet.*, the human for ceo.hr.hire
}

// chatterQueue is the per-channel backlog. Bounded, newest wins per
// (Kind, Actor): a builder that installs twice in a minute has one thing to
// say, not two.
type chatterQueue struct {
	mu   sync.Mutex
	byCh map[int64][]chatterEvent
}

var chatterQ = &chatterQueue{byCh: map[int64][]chatterEvent{}}

func (q *chatterQueue) push(ev chatterEvent, depth int) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	list := q.byCh[ev.ChannelID]
	for i, old := range list {
		if old.Kind == ev.Kind && old.ActorID == ev.ActorID {
			list[i] = ev
			q.byCh[ev.ChannelID] = list
			return ""
		}
	}
	if len(list) >= depth {
		// Drop the least important, oldest first among equals, unless the newcomer is the least
		// important of all.
		worst, worstAt := -1, 0
		for i, old := range list {
			if worst < 0 || old.Priority < list[worst].Priority ||
				(old.Priority == list[worst].Priority && old.At < list[worst].At) {
				worst, worstAt = i, old.Priority
			}
		}
		if worst < 0 || ev.Priority < worstAt {
			return "queue.full"
		}
		list = append(list[:worst], list[worst+1:]...)
		chatterDrop("queue.full")
	}
	q.byCh[ev.ChannelID] = append(list, ev)
	return ""
}

// pop takes the most important event, oldest among equals, dropping anything
// stale on the way.
func (q *chatterQueue) pop(ch, now int64, ttl time.Duration) (chatterEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	list := q.byCh[ch]
	kept := list[:0]
	for _, ev := range list {
		if now-ev.At > int64(ttl/time.Second) {
			chatterDrop("stale")
			continue
		}
		kept = append(kept, ev)
	}
	if len(kept) == 0 {
		delete(q.byCh, ch)
		return chatterEvent{}, false
	}
	best := 0
	for i, ev := range kept {
		if ev.Priority > kept[best].Priority ||
			(ev.Priority == kept[best].Priority && ev.At < kept[best].At) {
			best = i
		}
	}
	ev := kept[best]
	kept = append(kept[:best], kept[best+1:]...)
	if len(kept) == 0 {
		delete(q.byCh, ch)
	} else {
		q.byCh[ch] = kept
	}
	return ev, true
}

// putBack returns an event the budget refused on timing alone.
func (q *chatterQueue) putBack(ev chatterEvent) {
	q.mu.Lock()
	q.byCh[ev.ChannelID] = append(q.byCh[ev.ChannelID], ev)
	q.mu.Unlock()
}

func (q *chatterQueue) channels() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]int64, 0, len(q.byCh))
	for ch := range q.byCh {
		out = append(out, ch)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (q *chatterQueue) depth(ch int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.byCh[ch])
}

func (q *chatterQueue) clear(ch int64) {
	q.mu.Lock()
	delete(q.byCh, ch)
	q.mu.Unlock()
}

// has reports whether an event of this kind with this Ref is waiting.
func (q *chatterQueue) has(ch int64, kind string, ref int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, ev := range q.byCh[ch] {
		if ev.Kind == kind && ev.Ref == ref {
			return true
		}
	}
	return false
}

// chatterNote is the producer entry point. It is cheap (called from the sim worker, ceoLog
// and a death path) and silent on every refusal; the drop counters show refusals.
func (m *Mod) chatterNote(ctx context.Context, ev chatterEvent) {
	if !Chatter() {
		chatterDrop("off")
		return
	}
	k, known := chatterKinds[ev.Kind]
	if !known {
		chatterDrop("unknown")
		return
	}
	if chatterKindOff(ev.Kind) {
		chatterDrop("muted")
		return
	}
	if ev.ChannelID == 0 || !chatterWatched(ev.ChannelID) {
		chatterDrop("noaudience")
		return
	}
	if isAllianceChannel(ev.ChannelID) && !chatterAllianceOn.Load() {
		chatterDrop("off")
		return
	}
	if ev.Priority == 0 {
		ev.Priority = k.Priority
	}
	if ev.At == 0 {
		ev.At = clock.Unix()
	}
	if why := chatterQ.push(ev, chatterQueueDepth); why != "" {
		chatterDrop(why)
	}
}

// chatterCEO is the ceoLog adaptor (corpceo.go). The speaker is the officer
// who did the thing when an officer did it, else the CEO. Name lookups run
// only once the event is known to have an audience: ceoLog is synchronous
// in a CEO's think.
func (m *Mod) chatterCEO(ctx context.Context, c *corpCEO, actorID int64, power, verb string, subjectID int64, amount float64, detail string) {
	if c == nil || !Chatter() || !chatterWatched(c.CorpID) {
		return
	}
	kind := chatterCEOKind(power, verb)
	if kind == "" {
		return
	}
	speaker := actorID
	if speaker == 0 {
		speaker = c.CharID
	}
	ev := chatterEvent{Kind: kind, ChannelID: c.CorpID, CorpID: c.CorpID, ActorID: speaker, Slots: map[string]string{}}
	if detail != "" {
		ev.Slots["detail"] = strings.TrimSpace(detail)
	}
	if amount > 0 {
		ev.Slots["isk"] = iskWords(amount)
	}
	switch power {
	case powerWar, powerDiplomacy:
		switch verb {
		case "envoy", "envoylost":
			ev.Slots["who"] = m.chatterCharLink(ctx, subjectID)
		case "siege", "lapse":
			// subject is a tower or a bill; the detail carries the story.
		default:
			if subjectID != 0 {
				ev.Slots["corp"] = m.corpNameOf(ctx, subjectID)
			}
		}
		if kind == "ceo.dip.stance" {
			ev.Slots["stance"] = chatterStanceWord(verb)
		}
	case powerHR, powerRoles:
		if verb == "order" {
			if subjectID != 0 && subjectID >= 30_000_000 && subjectID < 40_000_000 {
				ev.Slots["system"] = m.nameOfSystem(ctx, subjectID)
			}
		} else if subjectID != 0 {
			ev.Slots["who"] = m.chatterCharLink(ctx, subjectID)
			ev.Slots["first"] = firstName(m.charNameOr(ctx, subjectID))
			ev.Ref = subjectID
		}
	case powerEstate:
		if subjectID != 0 {
			ev.Slots["station"] = m.stationNameCached(ctx, subjectID)
		}
	case powerProduction:
		switch verb {
		case "post", "printsale":
			if subjectID != 0 {
				ev.Slots["what"] = m.typeNameOr(ctx, subjectID)
			}
		}
	}
	m.chatterNote(ctx, ev)
}

// chatterStanceWord is the stance verb in a sentence.
func chatterStanceWord(verb string) string {
	switch verb {
	case "ally":
		return "allies"
	case "friend":
		return "friendly"
	case "rival":
		return "rivals"
	case "enemy":
		return "hostile"
	case "withdraw":
		return "neutral again"
	case "pactbreak":
		return "oath-breakers"
	}
	return verb
}

// chatterCharLink is a pilot as a clickable name, or "" when unknown.
func (m *Mod) chatterCharLink(ctx context.Context, charID int64) string {
	if charID == 0 {
		return ""
	}
	name, _ := m.charNameCorp(ctx, charID)
	if name == "" {
		return ""
	}
	return charLink(charID, m.charTypeID(ctx, charID), name)
}

// ── speakers ────────────────────────────────────────────────────────────────

// chatterSpeaker is a bot that can speak in a channel right now.
type chatterSpeaker struct {
	CharID int64
	CorpID int64
	TypeID int64
	Name   string
}

// chatterSpeakerFor is the actor as a speaker, if it is present in that channel: its
// corporation's or that corporation's alliance's (presence.go seats it in both). An actor
// that moved corps, logged off or was purged is not, and the event is dropped. A corpID
// never equals an allianceID.
func chatterSpeakerFor(channel, actorID int64) (chatterSpeaker, bool) {
	presenceMu.Lock()
	r, ok := presenceApplied[actorID]
	presenceMu.Unlock()
	if !ok || !seatedIn(r, channel) {
		return chatterSpeaker{}, false
	}
	return chatterSpeaker{CharID: r.CharID, CorpID: r.CorpID, TypeID: r.CharTypeID, Name: r.CharName}, true
}

// seatedIn reports whether a presence row sits in a corp or alliance channel.
func seatedIn(r presenceRow, channel int64) bool {
	return channel != 0 && (r.CorpID == channel || r.AllianceID == channel)
}

// chatterPresentBots lists the bots present in a corporation's channel, or in an alliance's,
// where it is every member corporation's bots.
func chatterPresentBots(channel int64) []chatterSpeaker {
	presenceMu.Lock()
	defer presenceMu.Unlock()
	var out []chatterSpeaker
	for _, r := range presenceApplied {
		if seatedIn(r, channel) {
			out = append(out, chatterSpeaker{CharID: r.CharID, CorpID: r.CorpID, TypeID: r.CharTypeID, Name: r.CharName})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CharID < out[j].CharID })
	return out
}

// chatterTraits is a short cache of a speaker's traits: the greeter pick and
// the voice both read them, and a trait read per line is a DB hit per line.
var (
	chatterTraitMu    sync.Mutex
	chatterTraitCache = map[int64]struct {
		traits map[string]int
		at     time.Time
	}{}
)

func (m *Mod) chatterTraitsOf(ctx context.Context, charID int64) map[string]int {
	now := clock.Now()
	chatterTraitMu.Lock()
	e, ok := chatterTraitCache[charID]
	chatterTraitMu.Unlock()
	if ok && now.Sub(e.at) < chatterTraitTTL {
		return e.traits
	}
	t, err := getTraits(ctx, m.db, charID)
	if err != nil {
		t = map[string]int{}
	}
	chatterTraitMu.Lock()
	chatterTraitCache[charID] = struct {
		traits map[string]int
		at     time.Time
	}{t, now}
	chatterTraitMu.Unlock()
	return t
}

// chatterGreeter is who says hello: the CEO when present, else the most
// leaderly and loyal bot in the channel, else anyone.
// In an alliance channel the executor corporation speaks for the alliance:
// its CEO when seated, else its most leaderly bot, and only then anyone's.
func (m *Mod) chatterGreeter(ctx context.Context, corpID int64, exclude map[int64]bool) (chatterSpeaker, bool) {
	host := corpID
	if isAllianceChannel(corpID) {
		host = m.allianceExecutor(ctx, corpID)
	}
	if ceo, ok := managedCEO(host); ok && !exclude[ceo] {
		if sp, ok := chatterSpeakerFor(corpID, ceo); ok {
			return sp, true
		}
	}
	bots := chatterPresentBots(corpID)
	if host != corpID && host != 0 {
		var own []chatterSpeaker
		for _, b := range bots {
			if b.CorpID == host {
				own = append(own, b)
			}
		}
		if len(own) > 0 {
			bots = own
		}
	}
	var best chatterSpeaker
	bestScore, found := -1<<30, false
	for _, b := range bots {
		if exclude[b.CharID] {
			continue
		}
		t := m.chatterTraitsOf(ctx, b.CharID)
		score := t[TraitLeadership] + t[TraitLoyalty]
		if score > bestScore {
			best, bestScore, found = b, score, true
		}
	}
	return best, found
}

// chatterAnyBot is a random present bot, avoiding one.
func chatterAnyBot(corpID, avoid int64) (chatterSpeaker, bool) {
	bots := chatterPresentBots(corpID)
	if len(bots) == 0 {
		return chatterSpeaker{}, false
	}
	if len(bots) > 1 && avoid != 0 {
		kept := bots[:0]
		for _, b := range bots {
			if b.CharID != avoid {
				kept = append(kept, b)
			}
		}
		bots = kept
	}
	return bots[rand.Intn(len(bots))], true
}

// ── saying it ───────────────────────────────────────────────────────────────

// chatterSaid is one line, for the panel's ring.
type chatterSaid struct {
	At        int64
	ChannelID int64
	CorpID    int64
	SpeakerID int64
	Speaker   string
	Kind      string
	Text      string
}

var (
	chatterRingMu sync.Mutex
	chatterRing   []chatterSaid
	chatterDropsN = map[string]int64{}
	// chatterLastSpeaker is who spoke last in each channel, so an
	// acknowledgement comes from somebody else.
	chatterLastSpeaker = map[int64]int64{}
)

func chatterRecord(s chatterSaid) {
	chatterRingMu.Lock()
	chatterRing = append(chatterRing, s)
	if len(chatterRing) > chatterRingSize {
		chatterRing = chatterRing[len(chatterRing)-chatterRingSize:]
	}
	chatterLastSpeaker[s.ChannelID] = s.SpeakerID
	chatterRingMu.Unlock()
}

// chatterRecent is the ring, newest first.
func chatterRecent() []chatterSaid {
	chatterRingMu.Lock()
	defer chatterRingMu.Unlock()
	out := make([]chatterSaid, len(chatterRing))
	for i, s := range chatterRing {
		out[len(chatterRing)-1-i] = s
	}
	return out
}

func chatterDrop(reason string) {
	chatterRingMu.Lock()
	chatterDropsN[reason]++
	chatterRingMu.Unlock()
}

func chatterDrops() map[string]int64 {
	chatterRingMu.Lock()
	defer chatterRingMu.Unlock()
	out := make(map[string]int64, len(chatterDropsN))
	for k, v := range chatterDropsN {
		out[k] = v
	}
	return out
}

func chatterLastSpeakerIn(ch int64) int64 {
	chatterRingMu.Lock()
	defer chatterRingMu.Unlock()
	return chatterLastSpeaker[ch]
}

// chatterSayMode is how a line meets the budget.
type chatterSayMode uint8

const (
	chatterSayBudget     chatterSayMode = iota // ask the ledger here
	chatterSayPrechecked                       // the caller already asked and was stamped
	chatterSayForced                           // the panel's test line: past the budget, still stamped
)

// chatterSay is the one place chatter reaches SpeakBot. Reports whether
// the line went.
func (m *Mod) chatterSay(ctx context.Context, sp chatterSpeaker, channelID int64, kind, tmplID, text string, class chatterClass, mode chatterSayMode) bool {
	if m.lsc == nil || channelID == 0 || sp.CharID == 0 || text == "" {
		return false
	}
	now := clock.Now()
	switch mode {
	case chatterSayForced:
		chatterBook.stampForced(now, channelID)
	case chatterSayBudget:
		if why := chatterBook.allow(now, channelID, sp.CharID, kind, tmplID, class, chatterLimitsIn(channelID, kind)); why != "" {
			chatterDrop(why)
			return false
		}
	}
	typeID := sp.TypeID
	if typeID < firstCharTypeID || typeID > lastCharTypeID {
		typeID = m.charTypeID(ctx, sp.CharID)
	}
	m.lsc.SpeakBot(channelID, sp.CharID, sp.Name, sp.CorpID, typeID, text)
	chatterRecord(chatterSaid{At: now.Unix(), ChannelID: channelID, CorpID: sp.CorpID, SpeakerID: sp.CharID, Speaker: sp.Name, Kind: kind, Text: text})
	m.logBot(sp.CharID, logInfo, "chatter", "said in channel %d: %s", channelID, text)
	return true
}

// chatterVoice renders an event in a speaker's voice, retrying once past a
// wording the channel has heard today.
func (m *Mod) chatterVoice(ctx context.Context, sp chatterSpeaker, ev chatterEvent) (tmplID, text string, ok bool) {
	trait := dominantTrait(m.chatterTraitsOf(ctx, sp.CharID))
	return renderChatter(ev.Kind, trait, ev.Slots, rand.Int())
}

// ── the lane ────────────────────────────────────────────────────────────────

func (m *Mod) startChatter() {
	if m.tick == nil {
		return
	}
	chatterOnce.Do(func() {
		m.registerPeriodic("rolebots-chatter", chatterEveryTicks, func(uint64) bool {
			if !Master() {
				return true
			}
			if !chatterBusy.CompareAndSwap(false, true) {
				return true
			}
			goSafe("chatter pass", func() {
				defer chatterBusy.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), chatterLaneTimeout)
				defer cancel()
				m.chatterPass(ctx)
			})
			return true
		})
		m.startMarketChatter()
		m.startBanterChatter()
	})
}

// chatterPass is one tick: refresh who is listening, greet anyone new, and
// say at most one queued line per channel.
func (m *Mod) chatterPass(ctx context.Context) {
	changes := m.pollAudience(ctx)
	if !Chatter() {
		// The poll still runs so the panel shows who is in which channel;
		// nothing is said and nothing is queued.
		return
	}
	now := clock.Unix()
	m.scheduleGreetings(changes)
	m.fireDueGreetings(ctx, now)
	expireFleetOffers(now)

	for _, ch := range chatterQ.channels() {
		if ctx.Err() != nil {
			return
		}
		if !chatterWatched(ch) {
			chatterQ.clear(ch)
			continue
		}
		ev, ok := chatterQ.pop(ch, now, chatterEventTTL)
		if !ok {
			continue
		}
		sp, ok := chatterSpeakerFor(ev.CorpID, ev.ActorID)
		if !ok {
			chatterDrop("speaker")
			continue
		}
		tmplID, text, ok := m.chatterVoice(ctx, sp, ev)
		if !ok {
			chatterDrop("render")
			continue
		}
		offer := ev.Kind == "fleet.formed" && FleetMirror() && ev.Ref != 0
		said := text
		if offer {
			said = text + " " + pickLine(chatterOfferSuffix)
		}
		lim := chatterLimitsFor(ev.Kind)
		why := chatterBook.allow(clock.Now(), ch, sp.CharID, ev.Kind, tmplID, chatterClassEvent, lim)
		if why == "repeat" {
			// One more roll for a wording the channel has not heard today.
			if t2, x2, ok2 := renderChatter(ev.Kind, dominantTrait(m.chatterTraitsOf(ctx, sp.CharID)), ev.Slots, rand.Int()); ok2 && t2 != tmplID {
				tmplID, text = t2, x2
				said = text
				if offer {
					said = text + " " + pickLine(chatterOfferSuffix)
				}
				why = chatterBook.allow(clock.Now(), ch, sp.CharID, ev.Kind, tmplID, chatterClassEvent, lim)
			}
		}
		switch why {
		case "":
		case "hour", "mingap":
			chatterDrop(why)
			chatterQ.putBack(ev)
			continue
		default:
			chatterDrop(why)
			continue
		}
		if !m.chatterSay(ctx, sp, ch, ev.Kind, tmplID, said, chatterClassEvent, chatterSayPrechecked) {
			continue
		}
		if offer {
			openFleetOffer(ev.CorpID, ev.Ref, sp.CharID, now)
		}
		if ev.Kind == "pers.loss" {
			// A corpmate answers (chatter_corp.go).
			m.chatterCorpConsole(ctx, ev.CorpID, sp, ev.Slots["system"])
		}
	}
}

// chatterTest is the panel's "say a test line": the greeter speaks one line
// past the budget so an operator can see the pipe work.
func (m *Mod) chatterTest(ctx context.Context, corpID int64, kind string) error {
	if !Chatter() {
		return fmt.Errorf("chatter is switched off")
	}
	if m.lsc == nil {
		return fmt.Errorf("no chat registry")
	}
	sp, ok := m.chatterGreeter(ctx, corpID, nil)
	if !ok {
		return fmt.Errorf("no bot of corporation or alliance %d is present in its channel", corpID)
	}
	text := "Testing the corporation channel — ignore me."
	if isAllianceChannel(corpID) {
		text = "Testing the alliance channel — ignore me."
	}
	tmplID := "test"
	if xk, ok := chatterExchangeKinds[kind]; ok {
		// An exchange kind has no queued wording; its sample event's lines
		// render with every slot shown as [slot].
		slots := chatSlots{}
		for _, s := range chatterExchangeSlotNames {
			slots[s] = "[" + s + "]"
		}
		line, tpl := pickChatLine(corpID, xk.Event, xk.SampleWhy, voiceAny, slots)
		if line == "" {
			return fmt.Errorf("kind %q has no wording that fills", kind)
		}
		text, tmplID = line, voiceTmplID(tpl)
		kind = ""
	}
	if kind != "" {
		k, known := chatterKinds[kind]
		if !known {
			return fmt.Errorf("unknown kind %q", kind)
		}
		slots := map[string]string{}
		for _, s := range k.Slots {
			slots[s] = "[" + s + "]"
		}
		var ok bool
		if tmplID, text, ok = renderChatter(kind, "", slots, rand.Int()); !ok {
			return fmt.Errorf("kind %q has no wording that fills", kind)
		}
	}
	if !m.chatterSay(ctx, sp, corpID, "test", tmplID, text, chatterClassReply, chatterSayForced) {
		return fmt.Errorf("the line was not sent")
	}
	return nil
}

func init() {
	registerSweep("chatterLedger", func() {
		chatterBook.sweep(clock.Now(), chatterLimitsFor(""))
		now := clock.Now()
		chatterTraitMu.Lock()
		for id, e := range chatterTraitCache {
			if now.Sub(e.at) > chatterTraitTTL {
				delete(chatterTraitCache, id)
			}
		}
		chatterTraitMu.Unlock()
		sweepChatterReplies(now)
		sweepExchanges(now)
	})
	registerCharForget("chatterTraitCache", func(id int64) {
		chatterTraitMu.Lock()
		delete(chatterTraitCache, id)
		chatterTraitMu.Unlock()
	})
	registerCorpForget("chatterLastSpeaker", func(id int64) {
		chatterRingMu.Lock()
		delete(chatterLastSpeaker, id)
		chatterRingMu.Unlock()
		chatterQ.clear(id)
	})
}

// chatterNoteBot is chatterNote for a per-pilot producer that knows only the
// pilot: the channel is the corporation the pilot is present in right now,
// read off presence rather than carried by the caller, so a pilot that moved
// corporations between the deed and the line speaks where it now sits.
func (m *Mod) chatterNoteBot(ctx context.Context, charID int64, kind string, slots map[string]string, ref int64) {
	if !Chatter() {
		chatterDrop("off")
		return
	}
	presenceMu.Lock()
	r, ok := presenceApplied[charID]
	presenceMu.Unlock()
	if !ok || r.CorpID == 0 {
		chatterDrop("speaker")
		return
	}
	m.chatterNote(ctx, chatterEvent{Kind: kind, ChannelID: r.CorpID, CorpID: r.CorpID, ActorID: charID, Slots: slots, Ref: ref})
}

// chatterActivityWord is a fleet's purpose as a person says it in chat.
func chatterActivityWord(activity string) string {
	switch activity {
	case ActMining:
		return "mining"
	case ActIndMining:
		return "mining for the factory"
	case ActCombat:
		return "ratting"
	case ActSecurity:
		return "a patrol"
	case ActPiracy:
		return "raiding"
	}
	return strings.ToLower(activityLabel(activity))
}

// plural is "1 run" / "3 runs".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return itoa(n) + " " + many
}

// workWords is how much industry work there is, in the units that kind of work counts: runs
// for a build, copies for a copy, levels for research. A research order's runs are levels
// and must not be read as manufacturing runs.
func workWords(kind string, n int) string {
	switch kind {
	case workKindME:
		return plural(n, "level", "levels") + " of material research"
	case workKindPE:
		return plural(n, "level", "levels") + " of time research"
	case workKindCopy:
		return plural(n, "copy", "copies")
	}
	return plural(n, "run", "runs")
}

// etaWords is a duration as "2h05m" / "40m" / "under a minute".
func etaWords(secs int64) string {
	switch {
	case secs < 60:
		return "under a minute"
	case secs < 3600:
		return fmt.Sprintf("%dm", secs/60)
	}
	return fmt.Sprintf("%dh%02dm", secs/3600, (secs%3600)/60)
}
