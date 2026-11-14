package rolebots

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gurimoon/server/lsc"
)

// A player answering a bot in Local. No conversation trees and no language model: the bot
// recognises that it has been addressed, sorts the line into one of a handful of intents
// by keyword, and answers once in its own voice.
// Addressed is strict: the player was barked at by a bot in this system inside barkWindow,
// or the line carries a showinfo link to a bot in this Local, or it names a bot in this
// Local (names under four letters do not count).
// An insult is remembered (insultGrudge, chatter_motive.go): that one bot thinks worse of
// that one pilot for a couple of hours.

type retortIntent string

const (
	intentRaceInsult retortIntent = "raceinsult" // an epithet aimed at the bot's own people
	intentInsult     retortIntent = "insult"
	intentThreat     retortIntent = "threat"
	intentPlea       retortIntent = "plea"
	intentWhy        retortIntent = "why"
	intentRansom     retortIntent = "ransom"
	intentGF         retortIntent = "gf"
	intentLaugh      retortIntent = "laugh"
	intentOther      retortIntent = "other"
)

// intentWords are the keyword lists, checked in this order; the first match wins, so "why
// would you, idiot" is a why and "you will pay" is a threat, not a ransom question.
// Matched on word boundaries over the lower-cased line with markup stripped.
var intentWords = []struct {
	intent retortIntent
	words  []string
}{
	{intentGF, []string{"gf", "gg", "o7", "good fight", "well fought", "fair enough"}},
	{intentWhy, []string{"why", "what did i do", "what for", "what's your problem", "whats your problem", "what is your problem"}},
	{intentThreat, []string{"come at me", "you're dead", "youre dead", "i'll find you", "ill find you", "dead man",
		"bring it", "try me", "you will pay", "i'll kill", "ill kill", "gonna kill", "watch your back", "fight me"}},
	{intentRansom, []string{"how much", "pay", "deal", "price", "ransom", "isk", "money"}},
	{intentPlea, []string{"please", "pls", "plz", "stop", "sorry", "leave me alone", "mercy", "don't", "dont", "no no"}},
	{intentLaugh, []string{"lol", "lmao", "rofl", "haha", "hah", "kek", "lmfao"}},
	{intentInsult, []string{"trash", "scum", "noob", "newb", "idiot", "moron", "loser", "bad", "garbage", "pathetic",
		"coward", "clown", "pleb", "carebear", "bot", "dumb", "stupid", "suck", "sucks", "hate you"}},
}

var (
	reMarkup  = regexp.MustCompile(`<[^>]*>`)
	reWordsep = regexp.MustCompile(`[^a-z0-9' ]+`)
)

// chatterNorm lower-cases a chat line, drops markup, and pads it with
// spaces so a word-boundary test is a plain substring test.
func chatterNorm(text string) string {
	t := strings.ToLower(reMarkup.ReplaceAllString(text, " "))
	t = strings.NewReplacer("&apos;", "'", "&#39;", "'", "’", "'").Replace(t)
	t = reWordsep.ReplaceAllString(t, " ")
	return " " + strings.Join(strings.Fields(t), " ") + " "
}

func hasPhrase(norm, phrase string) bool {
	return strings.Contains(norm, " "+phrase+" ")
}

// classifyRetort sorts a line addressed to a bot of race botRace.
func classifyRetort(text string, botRace int64) retortIntent {
	norm := chatterNorm(text)
	if botRace != 0 {
		for _, e := range epithetsAgainst(botRace) {
			if hasPhrase(norm, normaliseWords(e)) {
				return intentRaceInsult
			}
		}
	}
	for _, set := range intentWords {
		for _, w := range set.words {
			if hasPhrase(norm, normaliseWords(w)) {
				return set.intent
			}
		}
	}
	return intentOther
}

func normaliseWords(s string) string { return strings.TrimSpace(chatterNorm(s)) }

// ── the intake ──────────────────────────────────────────────────────────────

const (
	localQueueDepth = 128
	retortCooldown  = 2 * time.Minute
)

var (
	localInOnce sync.Once
	localIn     chan lsc.ChatMessage

	retortMu sync.Mutex
	retortAt = map[[2]int64]time.Time{}
)

// onLocalChat is the Local half of onChat (conscript_chat.go), on the
// speaker's own connection goroutine: cheap tests and a non-blocking send.
func (m *Mod) onLocalChat(msg lsc.ChatMessage) {
	if !Chatter() || !chatterRetortsOn.Load() || !chatterLocalOn.Load() || msg.SenderCharID == 0 || msg.Text == "" {
		return
	}
	localInOnce.Do(func() {
		localIn = make(chan lsc.ChatMessage, localQueueDepth)
		goSafe("chatter local intake", m.localWorker)
	})
	select {
	case localIn <- msg:
	default:
	}
}

func (m *Mod) localWorker() {
	for msg := range localIn {
		func() {
			defer func() {
				if r := recover(); r != nil {
					rlogf("rolebots: local chat worker recovered: %v", r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if msg.SystemID == 0 {
				m.handleRegionChat(ctx, msg) // routed here by onChat for the region band
				return
			}
			m.handleLocalChat(ctx, msg)
		}()
	}
}

// handleLocalChat finds the bot a human's line is addressed to and answers.
func (m *Mod) handleLocalChat(ctx context.Context, msg lsc.ChatMessage) {
	if m.isSynthetic(msg.SenderCharID) {
		return // the module's own lines come back through the hook too
	}
	sysID := msg.SystemID
	b, barked := openBark(sysID, msg.SenderCharID)
	// A market line (an ad, a remark, or a linked item someone present sells) is answered as
	// such (chatter_market.go), unless a bot is threatening this pilot.
	if !barked && m.marketCallback(ctx, sysID, msg) {
		return
	}
	var bot *chatParty
	if barked {
		bot = b.bot
	} else {
		bot = m.addressedBot(ctx, sysID, msg.Text)
	}
	if bot == nil {
		return
	}
	if !retortAllowed(bot.CharID, msg.SenderCharID) {
		return
	}
	intent := classifyRetort(msg.Text, bot.RaceID)
	switch intent {
	case intentRaceInsult:
		noteInsult(bot.CharID, msg.SenderCharID, 2)
	case intentInsult, intentThreat:
		noteInsult(bot.CharID, msg.SenderCharID, 1)
	}

	player := partyFromID(msg.SenderCharID, msg.SenderName, msg.SenderCorpID, 0, false)
	why := motiveNone
	extra := chatSlots{}
	if barked {
		why = b.why
		for k, v := range b.extra {
			extra[k] = v
		}
		if b.about != nil {
			player.ShipTypeID = b.about.ShipTypeID
		}
	} else if lp := liveProjection(bot.CharID); lp != nil {
		// Unprompted, the bot answers with what it thinks of the pilot, and gives a motive only if
		// it dislikes them.
		read := m.standingMotiveToPilot(ctx, standingSelf{CharID: lp.charID, CorpID: lp.corpID,
			FactionID: lp.factionID, RaceID: raceOf(lp.charID), Xenophobia: lp.tune.Xenophobia},
			msg.SenderCharID)
		if read.Standing <= StandingDisliked {
			why = read.Why
		}
	}
	if lp := liveProjection(bot.CharID); lp != nil {
		lp.mu.Lock()
		if r := lp.ransom; r != nil && !r.settled && r.victimCharID == msg.SenderCharID {
			extra["isk"] = iskWords(r.demanded)
			why = motivePiracy
		}
		lp.mu.Unlock()
	}
	m.runExchange("retort", sysID, false, []beat{
		{after: 2500 * time.Millisecond, speaker: bot, about: player, ev: answerEvent(intent), why: why,
			extra: extra, urgent: true},
	})
}

// retortAllowed spends the per-pair cooldown.
func retortAllowed(botID, pilotID int64) bool {
	now := clock.Now()
	retortMu.Lock()
	defer retortMu.Unlock()
	k := [2]int64{botID, pilotID}
	if at, ok := retortAt[k]; ok && now.Sub(at) < retortCooldown {
		return false
	}
	retortAt[k] = now
	if len(retortAt) > 4096 {
		for k, t := range retortAt {
			if now.Sub(t) >= retortCooldown {
				delete(retortAt, k)
			}
		}
	}
	return true
}

func sweepRetorts(now time.Time) {
	retortMu.Lock()
	for k, t := range retortAt {
		if now.Sub(t) >= retortCooldown {
			delete(retortAt, k)
		}
	}
	retortMu.Unlock()
}

// addressedBot is the bot in this Local a line links or names, nil for none.
func (m *Mod) addressedBot(ctx context.Context, sysID int64, text string) *chatParty {
	if m.lsc == nil {
		return nil
	}
	members := m.lsc.Members(sysID)
	inLocal := make(map[int64]*lsc.Member, len(members))
	for _, mem := range members {
		if mem.Conn == nil {
			inLocal[mem.CharID] = mem
		}
	}
	if len(inLocal) == 0 {
		return nil
	}
	for _, sm := range reChatLink.FindAllStringSubmatch(text, 4) {
		id, err := strconv.ParseInt(sm[2], 10, 64)
		if err != nil {
			continue
		}
		if mem := inLocal[id]; mem != nil {
			return m.retortParty(ctx, mem)
		}
	}
	norm := chatterNorm(text)
	for _, mem := range inLocal {
		name := normaliseWords(mem.CharName)
		if len(name) >= 4 && hasPhrase(norm, name) {
			return m.retortParty(ctx, mem)
		}
	}
	return nil
}

// retortParty is a Local member the module may answer for.
func (m *Mod) retortParty(ctx context.Context, mem *lsc.Member) *chatParty {
	if lp := liveProjection(mem.CharID); lp != nil {
		return partyFromEmbodied(lp)
	}
	if !m.isSynthetic(mem.CharID) {
		return nil
	}
	tn, _ := m.tuneFor(ctx, mem.CharID)
	p := &chatParty{CharID: mem.CharID, Name: mem.CharName, CorpID: mem.CorpID,
		RaceID: raceOf(mem.CharID), BloodlineID: bloodlineOf(mem.CharID), Phobia: tn.Xenophobia, Bot: true}
	p.Voice = voiceFor(mem.CharID, p.RaceID, tn, false)
	return p
}
