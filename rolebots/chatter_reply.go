package rolebots

import (
	"context"
	"math/rand"
	"strings"
	"sync"
	"time"

	"gurimoon/server/lsc"
)

// Answering a person.
//
// Human lines in a corporation's channel reach the module through the same
// hook fleet orders use (lsc.ModuleChat → onChat, conscript_chat.go); the
// corp branch lands here on the chat worker. Three things get an answer, in
// this precedence: an "x" (or "join") while a fleet offer is open (an invitation goes
// out), a bot addressed by name (that bot says what it is doing), and a
// greeting on its own (somebody says hello back). Anything else is
// conversation among people, and the bots stay out of it.

// chatterReply is what a line asks for.
type chatterReply struct {
	Kind  string // "join" | "mention" | "greet" | "gf" | "thanks" | ""
	BotID int64  // mention: who was addressed
	Words string // the normalised line
}

// chatterGreetWords are whole lines (after stripping) that read as hellos.
var chatterGreetWords = map[string]string{
	"o7": "greet", "o/": "greet", "7o": "greet", "\\o": "greet",
	"hi": "greet", "hello": "greet", "hey": "greet", "yo": "greet", "hiya": "greet", "heya": "greet",
	"morning": "greet", "evening": "greet", "afternoon": "greet",
	"hi all": "greet", "hello all": "greet", "hey all": "greet", "hi guys": "greet", "hi everyone": "greet",
	"hello everyone": "greet", "o7 all": "greet", "o7 o7": "greet", "good morning": "greet", "good evening": "greet",
	"gf": "gf", "gf all": "gf", "gfs": "gf",
	"thanks": "thanks", "thank you": "thanks", "ty": "thanks", "thx": "thanks", "cheers": "thanks", "thanks all": "thanks",
}

// chatterJoinWords open an invitation when an offer is standing. The offer asks for an "x"
// (the fleet-up convention); the other words are kept so "join" is not ignored.
var chatterJoinWords = []string{"x", "x up", "xup", "x'd", "xx", "x pls", "x please", "x me",
	"join", "inv", "invite", "seat", "invite me", "join please", "can i join", "inv me", "inv pls", "join pls"}

// parseChatterReply is the pure half. bots are the speakers present in the
// channel; offerOpen says a fleet announce is still answerable.
func parseChatterReply(raw string, bots []chatterSpeaker, offerOpen bool) chatterReply {
	text, links := extractChatLinks(raw)
	words := strings.ToLower(normaliseChat(text))
	words = strings.Trim(words, " .!?,")
	out := chatterReply{Words: words}
	if words == "" {
		return out
	}
	nwords := len(strings.Fields(words))

	// join: a short line that is the word.
	if offerOpen && nwords <= 4 {
		for _, w := range chatterJoinWords {
			if words == w || strings.HasPrefix(words, w+" ") {
				out.Kind = "join"
				return out
			}
		}
	}

	// mention: a character link to a present bot, a full name, or an
	// unambiguous first name of four letters or more.
	byID := map[int64]chatterSpeaker{}
	for _, b := range bots {
		byID[b.CharID] = b
	}
	for _, l := range links {
		if l.TypeID >= firstCharTypeID && l.TypeID <= lastCharTypeID {
			if _, ok := byID[l.ItemID]; ok {
				out.Kind, out.BotID = "mention", l.ItemID
				return out
			}
		}
	}
	for _, b := range bots {
		if full := strings.ToLower(b.Name); full != "" && containsWords(words, full) {
			out.Kind, out.BotID = "mention", b.CharID
			return out
		}
	}
	firsts := map[string][]int64{}
	for _, b := range bots {
		f := strings.ToLower(firstName(b.Name))
		if len(f) >= 4 {
			firsts[f] = append(firsts[f], b.CharID)
		}
	}
	for f, ids := range firsts {
		if len(ids) == 1 && containsWords(words, f) {
			out.Kind, out.BotID = "mention", ids[0]
			return out
		}
	}

	// greet: the whole line, three words at most.
	if nwords <= 3 {
		if k, ok := chatterGreetWords[words]; ok {
			out.Kind = k
		}
	}
	return out
}

// ── fleet offers ────────────────────────────────────────────────────────────

// fleetOffer is a fleet announce a human may still answer with an "x".
type fleetOffer struct {
	ModGangID int64
	LeaderID  int64
	Until     int64
}

var (
	offerMu           sync.Mutex
	pendingFleetOffer = map[int64]fleetOffer{} // by corpID
)

func openFleetOffer(corpID, gangID, leaderID, now int64) {
	offerMu.Lock()
	pendingFleetOffer[corpID] = fleetOffer{ModGangID: gangID, LeaderID: leaderID, Until: now + int64(chatterOfferTTL/time.Second)}
	offerMu.Unlock()
}

func closeFleetOffer(corpID int64) {
	offerMu.Lock()
	delete(pendingFleetOffer, corpID)
	offerMu.Unlock()
}

// closeFleetOfferFor ends the offer for one fleet, wherever it stands.
func closeFleetOfferFor(gangID int64) {
	offerMu.Lock()
	for corpID, o := range pendingFleetOffer {
		if o.ModGangID == gangID {
			delete(pendingFleetOffer, corpID)
		}
	}
	offerMu.Unlock()
}

func chatterOfferOpen(corpID, now int64) (fleetOffer, bool) {
	offerMu.Lock()
	defer offerMu.Unlock()
	o, ok := pendingFleetOffer[corpID]
	if !ok || o.Until <= now {
		return fleetOffer{}, false
	}
	return o, true
}

func expireFleetOffers(now int64) {
	offerMu.Lock()
	for corpID, o := range pendingFleetOffer {
		if o.Until <= now {
			delete(pendingFleetOffer, corpID)
		}
	}
	offerMu.Unlock()
}

// chatterOfferRows is the panel's view: corpID → seconds left.
func chatterOfferRows(now int64) map[int64]int64 {
	offerMu.Lock()
	defer offerMu.Unlock()
	out := map[int64]int64{}
	for corpID, o := range pendingFleetOffer {
		if o.Until > now {
			out[corpID] = o.Until - now
		}
	}
	return out
}

// ── the handler ─────────────────────────────────────────────────────────────

var (
	chatterReplyMu sync.Mutex
	chatterReplyAt = map[int64]time.Time{} // per human
)

func chatterReplyOK(humanID int64, now time.Time) bool {
	chatterReplyMu.Lock()
	defer chatterReplyMu.Unlock()
	if last, ok := chatterReplyAt[humanID]; ok && now.Sub(last) < chatterReplyGap {
		return false
	}
	chatterReplyAt[humanID] = now
	return true
}

func sweepChatterReplies(now time.Time) {
	chatterReplyMu.Lock()
	for id, t := range chatterReplyAt {
		if now.Sub(t) > time.Hour {
			delete(chatterReplyAt, id)
		}
	}
	chatterReplyMu.Unlock()
}

// chatterWantsCorpLine is onChat's test, on the sender's goroutine: a map
// read and nothing else.
func chatterWantsCorpLine(msg lsc.ChatMessage) bool {
	return Chatter() && chatterRepliesOn.Load() && msg.GangID == 0 && msg.SystemID == 0 &&
		msg.SenderCharID != 0 && msg.Text != "" && chatterWatched(msg.ChannelID)
}

// handleCorpChat runs on the chat worker.
func (m *Mod) handleCorpChat(ctx context.Context, msg lsc.ChatMessage) {
	if !chatterWantsCorpLine(msg) || m.isSynthetic(msg.SenderCharID) {
		return
	}
	corpID := msg.ChannelID
	humans := chatterHumans(corpID)
	if humans[msg.SenderCharID] == "" {
		return // not a listener we know; the next poll will say
	}
	now := clock.Now()
	bots := chatterPresentBots(corpID)
	if len(bots) == 0 {
		return
	}
	// The channel may be an alliance's (chatter_alliance.go): every member corporation's bots
	// sit in it, a fleet offer is never open there, and a person asking for help gets a truthful answer.
	ally := isAllianceChannel(corpID)
	if ally {
		if !chatterAllianceOn.Load() {
			return
		}
		if ask, where := parseHelpAsk(msg.Text); ask {
			if !chatterReplyOK(msg.SenderCharID, now) {
				chatterDrop("replygap")
				return
			}
			m.chatterAllyHumanHelp(ctx, corpID, msg.SenderCharID, firstName(humans[msg.SenderCharID]), where)
			return
		}
	}
	offerOpen := false
	if !ally {
		_, offerOpen = chatterOfferOpen(corpID, now.Unix())
	}
	r := parseChatterReply(msg.Text, bots, offerOpen)
	if r.Kind == "" {
		return
	}
	if !chatterReplyOK(msg.SenderCharID, now) {
		chatterDrop("replygap")
		return
	}
	first := firstName(humans[msg.SenderCharID])
	switch r.Kind {
	case "join":
		m.chatterJoin(ctx, corpID, msg.SenderCharID, first)
	case "mention":
		sp, ok := chatterSpeakerFor(corpID, r.BotID)
		if !ok {
			return
		}
		status := m.whatImDoing(ctx, sp.CharID)
		if status == "" {
			return
		}
		status = strings.ToLower(status[:1]) + status[1:]
		slots := map[string]string{"status": status, "first": first}
		trait := dominantTrait(m.chatterTraitsOf(ctx, sp.CharID))
		if tmplID, text, ok := renderChatter("social.status", trait, slots, rand.Int()); ok {
			m.chatterSay(ctx, sp, corpID, "social.status", tmplID, text, chatterClassReply, chatterSayBudget)
		}
	case "greet", "gf", "thanks":
		sp, ok := chatterAnyBot(corpID, chatterLastSpeakerIn(corpID))
		if !ok {
			return
		}
		kind := "social.ack"
		switch r.Kind {
		case "gf":
			kind = "social.gf"
		case "thanks":
			kind = "social.thanks"
		}
		slots := map[string]string{"first": first}
		if tmplID, text, ok := renderChatter(kind, "", slots, rand.Int()); ok {
			m.chatterSay(ctx, sp, corpID, kind, tmplID, text, chatterClassReply, chatterSayBudget)
		}
	}
}

// chatterJoin answers "join": the fleet's commander invites the person as the Corporate
// Portal's "Request a seat" does (fleetmirror.go requestSeat), and says so, or says why not.
func (m *Mod) chatterJoin(ctx context.Context, corpID, humanID int64, first string) {
	o, ok := chatterOfferOpen(corpID, clock.Unix())
	if !ok {
		return
	}
	g, err := getGang(ctx, m.db, o.ModGangID)
	sp, present := chatterSpeakerFor(corpID, o.LeaderID)
	if !present {
		if g != nil {
			sp, present = chatterSpeakerFor(corpID, g.LeaderCharID)
		}
		if !present {
			closeFleetOffer(corpID)
			return
		}
	}
	var text, tmplID string
	switch {
	case err != nil || g == nil || !g.Enabled:
		text, tmplID = "That fleet has already stood down, "+first+".", "join.gone"
		closeFleetOffer(corpID)
	default:
		if refusal := m.requestSeat(ctx, g, humanID, ""); refusal != "" {
			text, tmplID = refusal, "join.refused"
		} else {
			var ok bool
			if tmplID, text, ok = renderChatter("social.join", "", map[string]string{"first": first}, rand.Int()); !ok {
				return
			}
		}
	}
	m.chatterSay(ctx, sp, corpID, "social.join", tmplID, text, chatterClassReply, chatterSayBudget)
}

func init() {
	registerCharForget("chatterReplyAt", func(id int64) {
		chatterReplyMu.Lock()
		delete(chatterReplyAt, id)
		chatterReplyMu.Unlock()
	})
	registerCorpForget("pendingFleetOffer", closeFleetOffer)
}
