package rolebots

import (
	"context"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/server/lsc"
)

// Market talk: what a trade hub sounds like, in Local and the region channel, only where a
// person is reading. Three kinds of line:
//   - An ad: a bot that lists something says so (Show Info link, count, price, place).
//   - A remark: a bot that paid or refused a gouged price, or a docked bot whose career item
//     is far from the fortnight's trade average, says so.
//   - A callback: a reply to a person answering an ad or asking after a linked item, quoting
//     the real order. Nothing is negotiated.
// Everything goes through chatSay (chatter_voice.go), and a channel hears at most one market
// line every marketLocalGap (Local) or marketRegionGap (region).

const (
	settingChatterMarket          = "rolebots.chatter.market"
	settingChatterMarketRegion    = "rolebots.chatter.marketRegion"
	settingChatterMarketAdPct     = "rolebots.chatter.marketAdPct"
	settingChatterMarketRemarkPct = "rolebots.chatter.marketRemarkPct"
	settingChatterMarketLookMin   = "rolebots.chatter.marketLookMin"

	defaultMarketAdPct     = 15
	defaultMarketRemarkPct = 30
	defaultMarketLookMin   = 20

	marketLocalGap   = 3 * time.Minute
	marketRegionGap  = 10 * time.Minute
	marketAdCooldown = 2 * time.Hour
	marketNoteTTL    = 10 * time.Minute
	marketNoteDepth  = 6
	// marketLoneNoteWindow is how recent the only ad in a channel must be for
	// an unaddressed "how much?" to be read as a question about it.
	marketLoneNoteWindow = 2 * time.Minute

	// marketAdMinISK: a listing worth less than this whole is not news.
	marketAdMinISK = 250_000.0

	// The thresholds, all against the region's two-week trade average.
	marketGougeRatio   = 1.8
	marketBargainRatio = 0.6
	marketRefuseRatio  = 1.3
	marketClimbRatio   = 1.4
	marketSlumpRatio   = 0.65
	// marketMinTrades is how much tape a region needs before its average is
	// a price to measure against; marketMinShortTrades the same for a day.
	marketMinTrades      = 5
	marketMinShortTrades = 3
	marketShortWindowSec = 86400

	marketLookEveryTicks = 60
	marketLookChannels   = 2
	marketLookTries      = 4
)

var (
	chatterMarketOn        atomic.Bool
	chatterMarketRegionOn  atomic.Bool
	chatterMarketAdPct     atomic.Int64
	chatterMarketRemarkPct atomic.Int64
	chatterMarketLookMin   atomic.Int64
	marketLookBusy         atomic.Bool
)

func setChatterMarket(on bool)       { chatterMarketOn.Store(on) }
func setChatterMarketRegion(on bool) { chatterMarketRegionOn.Store(on) }
func setChatterMarketAdPct(n int)    { chatterMarketAdPct.Store(int64(clampPct(n))) }
func setChatterMarketRemarkPct(n int) {
	chatterMarketRemarkPct.Store(int64(clampPct(n)))
}

func setChatterMarketLookMin(n int) {
	if n < 1 {
		n = defaultMarketLookMin
	}
	chatterMarketLookMin.Store(int64(n))
}

func setChatterMarketDefaults() {
	setChatterMarket(true)
	setChatterMarketRegion(true)
	setChatterMarketAdPct(defaultMarketAdPct)
	setChatterMarketRemarkPct(defaultMarketRemarkPct)
	setChatterMarketLookMin(defaultMarketLookMin)
}

// marketTalk reports whether market lines may be said at all. Speaking in
// Local is the parent switch: a market line is a Local line.
func marketTalk() bool {
	return Chatter() && chatterMarketOn.Load() && chatterLocalOn.Load()
}

// ── events and cells ────────────────────────────────────────────────────────

const (
	evMarketAd      chatEvent = "mktad"      // a listing
	evMarketPaid    chatEvent = "mktpaid"    // bought at a gouged price
	evMarketRefused chatEvent = "mktrefused" // walked away from one
	evMarketBargain chatEvent = "mktbargain" // bought well under the average
	evMarketGouge   chatEvent = "mktgouge"   // saw a gouged ask, looking
	evMarketClimb   chatEvent = "mktclimb"   // the day's trades well over the fortnight's
	evMarketSlump   chatEvent = "mktslump"   // ... well under
	evMarketAnswer  chatEvent = "mktanswer"  // keyed "mktanswer:<intent>"
)

// The motive cell of a market line is the speaker's career, so a gunner and
// a miner grumble differently; an ad's cell says whether it is cheap; an
// answer's says what it answers.
const (
	mktCombat   motive = "mkt.combat"
	mktMining   motive = "mkt.mining"
	mktIndustry motive = "mkt.industry"
	mktTrade    motive = "mkt.trade"
	mktCheap    motive = "mkt.cheap"
	mktNoteAd   motive = "mkt.note.ad"
	mktNoteTalk motive = "mkt.note.remark"
	mktNoteShop motive = "mkt.note.stock" // a seller found for a linked item
)

type marketIntent string

const (
	mktIntentHaggle marketIntent = "haggle"
	mktIntentPrice  marketIntent = "price"
	mktIntentBuy    marketIntent = "buy"
	mktIntentAgree  marketIntent = "agree"
	mktIntentMock   marketIntent = "mock"
)

func marketAnswerEvent(i marketIntent) chatEvent { return evMarketAnswer + ":" + chatEvent(i) }

// marketIntentWords are checked in this order, first match wins: "how much
// cheaper" is a haggle, "how much for ten" a price.
var marketIntentWords = []struct {
	intent marketIntent
	words  []string
}{
	{mktIntentHaggle, []string{"cheaper", "discount", "lower", "too much", "too expensive", "overpriced",
		"knock", "best price", "any less", "go lower", "come down"}},
	{mktIntentPrice, []string{"how much", "price", "each", "per unit", "what for", "cost", "costs", "asking"}},
	{mktIntentBuy, []string{"buy", "buying", "i'll take", "ill take", "interested", "want", "need", "selling",
		"anyone have", "anybody have", "for sale", "wtb", "where can i", "who has", "got any"}},
	{mktIntentAgree, []string{"tell me about it", "robbery", "same", "agreed", "true", "insane", "ridiculous",
		"crazy", "absurd", "daylight", "theft", "highway"}},
	{mktIntentMock, []string{"lol", "lmao", "haha", "scam", "joke", "dream on", "nobody", "who buys",
		"good luck", "kek", "rofl"}},
}

// classifyMarket sorts a line by market intent, "" when it has none.
func classifyMarket(text string) marketIntent {
	norm := chatterNorm(text)
	for _, set := range marketIntentWords {
		for _, w := range set.words {
			if hasPhrase(norm, normaliseWords(w)) {
				return set.intent
			}
		}
	}
	return ""
}

// careerMotive is a pilot's career as a market cell: the activity it has
// worked longest lately, else what it is doing now.
func careerMotive(st *SimState) motive {
	if st == nil {
		return motiveNone
	}
	act, best := "", 0.0
	for a, s := range st.Data.Worked {
		if s > best || (s == best && a < act) {
			act, best = a, s
		}
	}
	if act == "" {
		act = st.Activity
	}
	switch act {
	case ActCombat, ActSecurity, ActPiracy:
		return mktCombat
	case ActMining, ActIndMining, ActIce:
		return mktMining
	case ActIndustry, ActResearch:
		return mktIndustry
	case ActTrade:
		return mktTrade
	}
	return motiveNone
}

// careerGroups are the item groups a career shops for and so has opinions on.
var careerGroups = map[motive][]int64{
	// Turrets, launchers and the ammunition and missiles they eat.
	mktCombat: {53, 55, 74, 507, 509, 510, 511, 83, 85, 86, 89, 384, 385, 386, 387},
	// Mining lasers, strip miners, deep-core lasers and mercoxit crystals.
	mktMining: {54, 464, 483, 663},
	// Minerals.
	mktIndustry: {18},
	mktTrade:    {18},
}

// ── channels, gaps and the region map ──────────────────────────────────────

var (
	sysRegionMu  sync.Mutex
	sysRegionMap map[int64]int64 // solar system → region, the whole map in one read

	marketGapMu sync.Mutex
	// marketLast is when each channel (a system's Local or a region) last
	// heard a market line; marketLookedAt when each was last looked at.
	marketLast     = map[int64]time.Time{}
	marketLookedAt = map[int64]time.Time{}
)

// systemRegion is the region a solar system is in, 0 when unknown. The map
// is loaded once, whole: the look lane asks it for every system a bot stands
// in, and one query per system would be thousands on the first pass.
func (m *Mod) systemRegion(ctx context.Context, sysID int64) int64 {
	sysRegionMu.Lock()
	defer sysRegionMu.Unlock()
	if sysRegionMap == nil {
		// A failed read is not cached: the next caller tries again.
		rows, err := m.db.QueryContext(ctx, `SELECT solarSystemID, regionID FROM mapSolarSystems`)
		if err != nil {
			return 0
		}
		out := map[int64]int64{}
		for rows.Next() {
			var s, r int64
			if rows.Scan(&s, &r) == nil {
				out[s] = r
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || len(out) == 0 {
			return 0
		}
		sysRegionMap = out
	}
	return sysRegionMap[sysID]
}

func isRegionChannel(ch int64) bool { return ch >= 10_000_000 && ch < 20_000_000 }

func marketGapFor(ch int64) time.Duration {
	if isRegionChannel(ch) {
		return marketRegionGap
	}
	return marketLocalGap
}

// claimMarketGap reports whether a channel may hear a market line now, and
// marks it. Claimed before the line is rendered: a line that then fails to go
// costs the channel one gap, which errs quiet.
func claimMarketGap(ch int64) bool {
	now := clock.Now()
	marketGapMu.Lock()
	defer marketGapMu.Unlock()
	if at, ok := marketLast[ch]; ok && now.Sub(at) < marketGapFor(ch) {
		return false
	}
	marketLast[ch] = now
	return true
}

// marketGapOpen is claimMarketGap without the claim.
func marketGapOpen(ch int64) bool {
	marketGapMu.Lock()
	defer marketGapMu.Unlock()
	at, ok := marketLast[ch]
	return !ok || clock.Now().Sub(at) >= marketGapFor(ch)
}

// claimMarketLook spends a channel's look interval.
func claimMarketLook(ch int64) bool {
	now := clock.Now()
	every := time.Duration(chatterMarketLookMin.Load()) * time.Minute
	marketGapMu.Lock()
	defer marketGapMu.Unlock()
	if at, ok := marketLookedAt[ch]; ok && now.Sub(at) < every {
		return false
	}
	marketLookedAt[ch] = now
	return true
}

// marketChannel is where a bot standing in sysID would say a market line: its Local when a
// person is in it, else sometimes its region's channel when a person is in that. 0 for
// nowhere, the usual answer.
func (m *Mod) marketChannel(ctx context.Context, sysID int64) int64 {
	if m.lsc == nil || sysID == 0 {
		return 0
	}
	if m.lsc.HasLiveListener(sysID) && marketGapOpen(sysID) {
		return sysID
	}
	if !chatterMarketRegionOn.Load() || rand.Intn(2) == 0 {
		return 0
	}
	if reg := m.systemRegion(ctx, sysID); reg != 0 && m.lsc.HasLiveListener(reg) && marketGapOpen(reg) {
		return reg
	}
	return 0
}

// ── speakers ────────────────────────────────────────────────────────────────

// marketParty is a present bot as a speaker, and the system it stands in.
// A bot on grid speaks as its projection; anyone else from its presence row,
// alive for as long as it is still seated in that system.
func (m *Mod) marketParty(ctx context.Context, charID int64) (*chatParty, int64) {
	presenceMu.Lock()
	r, ok := presenceApplied[charID]
	presenceMu.Unlock()
	if !ok || r.SolarSystemID == 0 {
		return nil, 0
	}
	if lp := liveProjection(charID); lp != nil {
		return partyFromEmbodied(lp), r.SolarSystemID
	}
	tn, _ := m.tuneFor(ctx, charID)
	p := &chatParty{CharID: charID, Name: r.CharName, CorpID: r.CorpID, RaceID: raceOf(charID),
		BloodlineID: bloodlineOf(charID), Phobia: tn.Xenophobia, Bot: true}
	p.Voice = voiceFor(charID, p.RaceID, tn, false)
	sys := r.SolarSystemID
	p.Alive = func() bool {
		presenceMu.Lock()
		now, ok := presenceApplied[charID]
		presenceMu.Unlock()
		return ok && now.SolarSystemID == sys
	}
	return p, sys
}

// presentSystem is the system a bot is seated in, 0 for none.
func presentSystem(charID int64) int64 {
	presenceMu.Lock()
	defer presenceMu.Unlock()
	return presenceApplied[charID].SolarSystemID
}

// marketPlaceSlots are the where-slots of a line: station, system, region.
func (m *Mod) marketPlaceSlots(ctx context.Context, s chatSlots, stationID, sysID int64) {
	if stationID != 0 {
		s["station"] = m.stationNameOr(ctx, stationID)
		if sysID == 0 {
			sysID = m.stationSystemOf(ctx, stationID)
		}
	}
	if sysID != 0 {
		if n := m.systemNameCached(ctx, sysID); n != "" {
			s["system"] = n
		}
		if n := m.regionNameCached(ctx, m.systemRegion(ctx, sysID)); n != "" {
			s["region"] = n
		}
	}
}

// marketRef is a type's normal price in a region: the region's fortnight of
// trades when it is a market, else the cluster's, else the book's base price.
// Answers 0 when nothing at all is known.
func (m *Mod) marketRef(ctx context.Context, typeID, regionID int64) (ref float64, regional tapeRef) {
	refs := m.tradeRefsOf(ctx, typeID)
	regional = refs.Region[regionID]
	base := 0.0
	if r, ok := m.typeStaticRec(ctx, typeID); ok {
		base = r.BasePrice
	}
	ref, _ = pickTradeValue(regional, refs.Cluster, base)
	return ref, regional
}

// pctWords is a ratio as a move: 1.62 → "62%".
func pctWords(ratio float64) string {
	d := ratio - 1
	if d < 0 {
		d = -d
	}
	return strconv.Itoa(int(d*100+0.5)) + "%"
}

// unitsWords is a count as a person says it.
func unitsWords(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + " million"
	case n >= 10_000:
		return strconv.FormatInt(n/1000, 10) + " thousand"
	}
	return strconv.FormatInt(n, 10)
}

// ── notes: what a channel was just told ────────────────────────────────────

// marketNote is one ad or remark a channel heard, kept for the callbacks.
type marketNote struct {
	Bot    *chatParty
	Cell   motive // mktNoteAd / mktNoteTalk / mktNoteShop
	TypeID int64
	Slots  chatSlots
	At     time.Time
}

var (
	marketNoteMu sync.Mutex
	// marketNotes is the last few market lines per channel; marketAdSeen
	// when each (pilot, type) was last announced, keyed charID|typeID.
	marketNotes  = map[int64][]marketNote{}
	marketAdSeen = map[[2]int64]time.Time{}
)

func noteMarket(ch int64, n marketNote) {
	marketNoteMu.Lock()
	defer marketNoteMu.Unlock()
	list := append(pruneNotes(marketNotes[ch], n.At), n)
	if len(list) > marketNoteDepth {
		list = list[len(list)-marketNoteDepth:]
	}
	marketNotes[ch] = list
}

func pruneNotes(list []marketNote, now time.Time) []marketNote {
	kept := list[:0]
	for _, n := range list {
		if now.Sub(n.At) < marketNoteTTL {
			kept = append(kept, n)
		}
	}
	return kept
}

// claimAd spends a (pilot, type) ad cooldown.
func claimAd(charID, typeID int64) bool {
	now := clock.Now()
	k := [2]int64{charID, typeID}
	marketNoteMu.Lock()
	defer marketNoteMu.Unlock()
	if at, ok := marketAdSeen[k]; ok && now.Sub(at) < marketAdCooldown {
		return false
	}
	marketAdSeen[k] = now
	if len(marketAdSeen) > 8192 {
		for k, t := range marketAdSeen {
			if now.Sub(t) >= marketAdCooldown {
				delete(marketAdSeen, k)
			}
		}
	}
	return true
}

var reTypeLink = regexp.MustCompile(`(?i)<(?:url=|a\s+href=")showinfo:(\d+)"?>([^<]*)</(?:url|a)>`)

// typeLinksIn is the item types a chat line links (character links, which
// carry "//id", do not match).
func typeLinksIn(text string) []int64 {
	var out []int64
	for _, sm := range reTypeLink.FindAllStringSubmatch(text, 4) {
		if id, err := strconv.ParseInt(sm[1], 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

// findMarketNote is the note a person's line is about: one whose bot the line links or
// names, one whose item it links, or, for a bare question right after the only ad in the
// channel, that ad.
func findMarketNote(ch int64, text string, intent marketIntent) (marketNote, bool) {
	now := clock.Now()
	marketNoteMu.Lock()
	list := pruneNotes(marketNotes[ch], now)
	marketNotes[ch] = list
	notes := append([]marketNote(nil), list...)
	marketNoteMu.Unlock()
	if len(notes) == 0 {
		return marketNote{}, false
	}
	charLinks := map[int64]bool{}
	for _, sm := range reChatLink.FindAllStringSubmatch(text, 4) {
		if id, err := strconv.ParseInt(sm[2], 10, 64); err == nil {
			charLinks[id] = true
		}
	}
	typeLinks := map[int64]bool{}
	for _, id := range typeLinksIn(text) {
		typeLinks[id] = true
	}
	norm := chatterNorm(text)
	for i := len(notes) - 1; i >= 0; i-- {
		n := notes[i]
		if charLinks[n.Bot.CharID] || typeLinks[n.TypeID] {
			return n, true
		}
		if name := normaliseWords(n.Bot.Name); len(name) >= 4 && hasPhrase(norm, name) {
			return n, true
		}
	}
	if len(charLinks) == 0 && len(typeLinks) == 0 && (intent == mktIntentPrice || intent == mktIntentBuy || intent == mktIntentHaggle) {
		var lone []marketNote
		for _, n := range notes {
			if n.Cell == mktNoteAd && now.Sub(n.At) < marketLoneNoteWindow {
				lone = append(lone, n)
			}
		}
		if len(lone) == 1 {
			return lone[0], true
		}
	}
	return marketNote{}, false
}

// ── B1: ads ─────────────────────────────────────────────────────────────────

// chatterMarketListed is registerOrder's hook (tradebook.go): a bot just put a resting sell
// order up. Cheap gates run first on the caller's goroutine, since listings happen thousands
// of times an hour and nearly all are heard by nobody.
func (m *Mod) chatterMarketListed(charID, typeID, stationID, sysID int64, typeName string, price float64, vol int64) {
	// The Summit's shop threads have their own gates (summit_trade.go).
	m.summitMarketListed(charID, typeID, stationID, sysID, typeName, price, vol)
	if !marketTalk() || typeID == 0 || vol <= 0 || price <= 0 || price*float64(vol) < marketAdMinISK {
		return
	}
	if rand.Intn(100) >= int(chatterMarketAdPct.Load()) {
		return
	}
	goSafe("chatter market ad", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		m.sayMarketAd(ctx, charID, typeID, stationID, sysID, typeName, price, vol)
	})
}

// marketQuietCategories are never announced: ore, materials (minerals, moon
// goo, ice products) and commodities move by the hold, and a hub announcing
// every hold would be a ticker.
var marketQuietCategories = map[int64]bool{4: true, 25: true, 17: true}

func (m *Mod) sayMarketAd(ctx context.Context, charID, typeID, stationID, sysID int64, typeName string, price float64, vol int64) {
	if cats, err := m.typeCategories(ctx); err != nil || marketQuietCategories[cats[typeID]] {
		return
	}
	if sysID == 0 {
		sysID = m.stationSystemOf(ctx, stationID)
	}
	m.allyAdFor(ctx, charID, typeID, stationID, sysID, typeName, price, vol)
	ch := m.marketChannel(ctx, sysID)
	if ch == 0 {
		return
	}
	sp, at := m.marketParty(ctx, charID)
	if sp == nil || at != sysID {
		return
	}
	if !claimAd(charID, typeID) || !claimMarketGap(ch) {
		return
	}
	if typeName == "" {
		typeName = m.typeNameOr(ctx, typeID)
	}
	s := chatSlots{
		"item":  typeLink(typeID, typeName),
		"units": unitsWords(vol),
		"isk":   iskWords(price),
		"total": iskWords(price * float64(vol)),
	}
	m.marketPlaceSlots(ctx, s, stationID, sysID)
	why := motiveNone
	if ref, _ := m.marketRef(ctx, typeID, m.systemRegion(ctx, sysID)); ref > 0 {
		s["ref"] = iskWords(ref)
		if price <= ref*0.9 {
			why = mktCheap
		}
	}
	noteMarket(ch, marketNote{Bot: sp, Cell: mktNoteAd, TypeID: typeID, Slots: s, At: clock.Now()})
	m.runExchange("market ad", ch, false, []beat{
		{after: 2 * time.Second, speaker: sp, ev: evMarketAd, why: why, extra: s},
	})
}

// ── B2: remarks ─────────────────────────────────────────────────────────────

// chatterMarketBought is bookPurchase's hook (materialize.go): a bot just bought. Said only
// when the price was far from the region's normal. The item is relevant to the speaker,
// since it bought it.
func (m *Mod) chatterMarketBought(charID, stationID, typeID, qty int64, unit float64, resale bool) {
	if !marketTalk() || resale || qty <= 0 || unit <= 0 {
		return
	}
	if rand.Intn(100) >= int(chatterMarketRemarkPct.Load()) {
		return
	}
	sysID := presentSystem(charID)
	if sysID == 0 || m.lsc == nil || (!m.lsc.HasLiveListener(sysID) && !chatterMarketRegionOn.Load()) {
		return
	}
	goSafe("chatter market bought", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		m.sayMarketPrice(ctx, charID, stationID, typeID, unit, false)
	})
}

// chatterMarketRefused is buyAtStationCapped's hook (sweep.go): the cheapest
// ask was over what the bot would pay, and it walked away.
func (m *Mod) chatterMarketRefused(charID, stationID, typeID int64, ask float64) {
	if !marketTalk() || ask <= 0 || rand.Intn(100) >= int(chatterMarketRemarkPct.Load()) {
		return
	}
	if presentSystem(charID) == 0 {
		return
	}
	goSafe("chatter market refused", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		m.sayMarketPrice(ctx, charID, stationID, typeID, ask, true)
		// "Anyone in the alliance making these?", asked only if one does.
		if sp := m.seatedParty(ctx, charID); sp != nil {
			m.chatterAllyShopAsk(ctx, sp, typeID, typeLink(typeID, m.typeNameOr(ctx, typeID)))
		}
	})
}

// sayMarketPrice voices a purchase (or a refusal) at a price far from normal.
func (m *Mod) sayMarketPrice(ctx context.Context, charID, stationID, typeID int64, unit float64, refused bool) {
	sysID := m.stationSystemOf(ctx, stationID)
	ref, regional := m.marketRef(ctx, typeID, m.systemRegion(ctx, sysID))
	if ref <= 0 || regional.Trades < marketMinTrades {
		return
	}
	ratio := unit / ref
	var ev chatEvent
	switch {
	case refused && ratio >= marketRefuseRatio:
		ev = evMarketRefused
	case !refused && ratio >= marketGougeRatio:
		ev = evMarketPaid
	case !refused && ratio <= marketBargainRatio:
		ev = evMarketBargain
	default:
		return
	}
	ch := m.marketChannel(ctx, sysID)
	if ch == 0 {
		return
	}
	sp, at := m.marketParty(ctx, charID)
	if sp == nil || at != sysID || !claimMarketGap(ch) {
		return
	}
	st, _ := loadState(ctx, m.db, charID)
	s := chatSlots{
		"item": typeLink(typeID, m.typeNameOr(ctx, typeID)),
		"isk":  iskWords(unit),
		"ref":  iskWords(ref),
		"pct":  pctWords(ratio),
	}
	m.marketPlaceSlots(ctx, s, stationID, sysID)
	noteMarket(ch, marketNote{Bot: sp, Cell: mktNoteTalk, TypeID: typeID, Slots: s, At: clock.Now()})
	m.runExchange("market remark", ch, false, []beat{
		{after: 3 * time.Second, speaker: sp, ev: ev, why: careerMotive(st), extra: s},
	})
}

// ── B2: the look lane ───────────────────────────────────────────────────────

func (m *Mod) startMarketChatter() {
	m.registerPeriodic("rolebots-chatter-market", marketLookEveryTicks, func(uint64) bool {
		if !Master() || !marketTalk() {
			return true
		}
		if !marketLookBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("chatter market look", func() {
			defer marketLookBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), chatterLaneTimeout)
			defer cancel()
			m.marketLookPass(ctx)
		})
		return true
	})
}

// marketLookPass is one minute of the lane: find the channels a person is
// reading that a docked bot sits behind, and let one bot in each of a couple
// of them glance at its region's market for something its career uses. Most
// glances find nothing worth saying, which is the point.
func (m *Mod) marketLookPass(ctx context.Context) {
	if m.lsc == nil {
		return
	}
	docked := map[int64][]int64{} // system → docked bots
	presenceMu.Lock()
	for id, r := range presenceApplied {
		if r.StationID != 0 && r.SolarSystemID != 0 {
			docked[r.SolarSystemID] = append(docked[r.SolarSystemID], id)
		}
	}
	presenceMu.Unlock()
	if len(docked) == 0 {
		return
	}
	type cand struct {
		ch   int64
		bots []int64
	}
	var cands []cand
	byRegion := map[int64][]int64{}
	for sys, bots := range docked {
		if m.lsc.HasLiveListener(sys) {
			cands = append(cands, cand{sys, bots})
		}
		if chatterMarketRegionOn.Load() {
			if reg := m.systemRegion(ctx, sys); reg != 0 {
				byRegion[reg] = append(byRegion[reg], bots...)
			}
		}
	}
	for reg, bots := range byRegion {
		if m.lsc.HasLiveListener(reg) {
			cands = append(cands, cand{reg, bots})
		}
	}
	rand.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	looked := 0
	for _, c := range cands {
		if looked >= marketLookChannels || ctx.Err() != nil {
			return
		}
		if !marketGapOpen(c.ch) || !claimMarketLook(c.ch) {
			continue
		}
		looked++
		m.glanceMarket(ctx, c.ch, c.bots[rand.Intn(len(c.bots))])
	}
}

// glanceMarket is one bot glancing at its region's market.
func (m *Mod) glanceMarket(ctx context.Context, ch, charID int64) {
	if !m.isSynthetic(charID) {
		return
	}
	st, err := loadState(ctx, m.db, charID)
	if err != nil || st == nil {
		return
	}
	career := careerMotive(st)
	types := m.careerTypes(ctx, career)
	if len(types) == 0 {
		return
	}
	sp, sysID := m.marketParty(ctx, charID)
	if sp == nil {
		return
	}
	region := m.systemRegion(ctx, sysID)
	asks, err := m.allAskByRegion(ctx)
	if err != nil {
		return
	}
	for try := 0; try < marketLookTries && ctx.Err() == nil; try++ {
		typeID := types[rand.Intn(len(types))]
		ref, regional := m.marketRef(ctx, typeID, region)
		if ref <= 0 || regional.Trades < marketMinTrades {
			continue
		}
		var ev chatEvent
		ratio := 0.0
		if ask := scopedAsk(asks, typeID, []int64{region}); ask > 0 && ask >= ref*marketGougeRatio {
			ev, ratio = evMarketGouge, ask/ref
		} else if day := m.tradeRefShortOf(ctx, typeID)[region]; day.Trades >= marketMinShortTrades && day.Avg > 0 {
			switch r := day.Avg / ref; {
			case r >= marketClimbRatio:
				ev, ratio = evMarketClimb, r
			case r <= marketSlumpRatio:
				ev, ratio = evMarketSlump, r
			}
		}
		if ev == "" {
			continue
		}
		if !claimMarketGap(ch) {
			return
		}
		s := chatSlots{
			"item": typeLink(typeID, m.typeNameOr(ctx, typeID)),
			"ref":  iskWords(ref),
			"isk":  iskWords(ref * ratio),
			"pct":  pctWords(ratio),
		}
		m.marketPlaceSlots(ctx, s, 0, sysID)
		noteMarket(ch, marketNote{Bot: sp, Cell: mktNoteTalk, TypeID: typeID, Slots: s, At: clock.Now()})
		m.runExchange("market look", ch, false, []beat{
			{after: time.Second, speaker: sp, ev: ev, why: career, extra: s},
		})
		return
	}
}

var (
	careerTypesMu sync.Mutex
	careerTypesBy = map[motive][]int64{}
)

// careerTypes is the published, plain (tech I and named) types of a career's groups that
// have a market: what a pilot of that career would go shopping for. Read once per career.
func (m *Mod) careerTypes(ctx context.Context, career motive) []int64 {
	groups := careerGroups[career]
	if len(groups) == 0 {
		return nil
	}
	careerTypesMu.Lock()
	if out, ok := careerTypesBy[career]; ok {
		careerTypesMu.Unlock()
		return out
	}
	careerTypesMu.Unlock()
	ph := strings.TrimSuffix(strings.Repeat("?,", len(groups)), ",")
	args := make([]any, len(groups))
	for i, g := range groups {
		args[i] = g
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID FROM invTypes t
		 WHERE t.published = 1 AND t.marketGroupID IS NOT NULL AND t.basePrice > 0
		   AND t.groupID IN (`+ph+`)
		   AND NOT EXISTS (SELECT 1 FROM invMetaTypes mt WHERE mt.typeID = t.typeID AND mt.metaGroupID <> 1)`,
		args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	careerTypesMu.Lock()
	careerTypesBy[career] = out
	careerTypesMu.Unlock()
	return out
}

// tradeRefShortOf is tradeRefsOf over the last day only, on the same buyer-row tape and
// covering index (idx_mt_type_item_date), per region. Its average against the fortnight's is
// the prices-are-climbing signal.
func (m *Mod) tradeRefShortOf(ctx context.Context, typeID int64) map[int64]tapeRef {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	out, err := snapshotFor(snapKey("market-tape-day", typeID), corpTradeRefTTL, func() (map[int64]tapeRef, error) {
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT regionID, COUNT(*), SUM(quantity), SUM(price * quantity)
			  FROM market_transactions
			 WHERE transactionType = 1 AND typeID = ? AND transactionDateTime >= ?
			   AND quantity > 0 AND price > 0
			 GROUP BY regionID`),
			typeID, evedb.UnixToWin32(clock.Unix()-marketShortWindowSec))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		res := map[int64]tapeRef{}
		for rows.Next() {
			var region, n, qty int64
			var v float64
			if err := rows.Scan(&region, &n, &qty, &v); err != nil {
				return nil, err
			}
			if qty > 0 && v > 0 {
				res[region] = tapeRef{Avg: v / float64(qty), Trades: n, Units: qty}
			}
		}
		return res, rows.Err()
	})
	if err != nil {
		return nil
	}
	return out
}

// ── B3: callbacks ───────────────────────────────────────────────────────────

// marketCallback answers a person's line about the market in a channel (a
// system's Local, or a region): about an ad or remark just said there, or a
// linked item somebody present is selling. Reports whether it answered, so
// the Local intake does not also answer with a combat retort.
func (m *Mod) marketCallback(ctx context.Context, ch int64, msg lsc.ChatMessage) bool {
	if !marketTalk() {
		return false
	}
	intent := classifyMarket(msg.Text)
	if intent == "" {
		return false
	}
	note, ok := findMarketNote(ch, msg.Text, intent)
	if !ok {
		if intent != mktIntentBuy && intent != mktIntentPrice {
			return false
		}
		links := typeLinksIn(msg.Text)
		if len(links) == 0 {
			return false
		}
		if note, ok = m.marketSellerFor(ctx, ch, links[0]); !ok {
			return false
		}
		noteMarket(ch, note)
	}
	if note.Bot == nil || (note.Bot.Alive != nil && !note.Bot.Alive()) {
		return false
	}
	if !retortAllowed(note.Bot.CharID, msg.SenderCharID) {
		return true // already answered this person lately; nobody else should
	}
	player := partyFromID(msg.SenderCharID, msg.SenderName, msg.SenderCorpID, 0, false)
	extra := chatSlots{}
	for k, v := range note.Slots {
		extra[k] = v
	}
	m.runExchange("market answer", ch, false, []beat{
		{after: 2500 * time.Millisecond, speaker: note.Bot, about: player, ev: marketAnswerEvent(intent),
			why: note.Cell, extra: extra, urgent: true},
	})
	return true
}

// marketSellerFor is a present bot with a live sell order for a type where a
// person asked: in that system for Local, anywhere in the region for a
// region channel. The cheapest such order wins.
func (m *Mod) marketSellerFor(ctx context.Context, ch, typeID int64) (marketNote, bool) {
	region, sysID := ch, int64(0)
	if !isRegionChannel(ch) {
		sysID, region = ch, m.systemRegion(ctx, ch)
	}
	if region == 0 {
		return marketNote{}, false
	}
	q := `SELECT charID, stationID, solarSystemID, price, volRemaining FROM market_orders
	       WHERE typeID = ? AND regionID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0`
	args := []any{typeID, region}
	if sysID != 0 {
		q += ` AND solarSystemID = ?`
		args = append(args, sysID)
	}
	rows, err := m.db.QueryContext(ctx, q+` ORDER BY price LIMIT 25`, args...)
	if err != nil {
		return marketNote{}, false
	}
	type row struct {
		char, station, sys, vol int64
		price                   float64
	}
	var found []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.char, &r.station, &r.sys, &r.price, &r.vol) == nil {
			found = append(found, r)
		}
	}
	rows.Close()
	for _, r := range found {
		if !m.isSynthetic(r.char) {
			continue
		}
		at := presentSystem(r.char)
		if at == 0 || (sysID != 0 && at != sysID) || (sysID == 0 && m.systemRegion(ctx, at) != region) {
			continue
		}
		sp, _ := m.marketParty(ctx, r.char)
		if sp == nil {
			continue
		}
		s := chatSlots{
			"item":  typeLink(typeID, m.typeNameOr(ctx, typeID)),
			"units": unitsWords(r.vol),
			"isk":   iskWords(r.price),
			"total": iskWords(r.price * float64(r.vol)),
		}
		m.marketPlaceSlots(ctx, s, r.station, r.sys)
		return marketNote{Bot: sp, Cell: mktNoteShop, TypeID: typeID, Slots: s, At: clock.Now()}, true
	}
	return marketNote{}, false
}

// handleRegionChat is the region channel's intake: market talk only. A
// region channel is not where a bot is shot at, so nothing else answers.
func (m *Mod) handleRegionChat(ctx context.Context, msg lsc.ChatMessage) {
	if m.isSynthetic(msg.SenderCharID) || !chatterMarketRegionOn.Load() {
		return
	}
	m.marketCallback(ctx, msg.ChannelID, msg)
}

// ── housekeeping ────────────────────────────────────────────────────────────

func sweepMarketChatter(now time.Time) {
	marketGapMu.Lock()
	for ch, t := range marketLast {
		if now.Sub(t) >= marketRegionGap {
			delete(marketLast, ch)
		}
	}
	every := time.Duration(chatterMarketLookMin.Load()) * time.Minute
	for ch, t := range marketLookedAt {
		if now.Sub(t) >= every {
			delete(marketLookedAt, ch)
		}
	}
	marketGapMu.Unlock()
	marketNoteMu.Lock()
	for ch, list := range marketNotes {
		if list = pruneNotes(list, now); len(list) == 0 {
			delete(marketNotes, ch)
		} else {
			marketNotes[ch] = list
		}
	}
	for k, t := range marketAdSeen {
		if now.Sub(t) >= marketAdCooldown {
			delete(marketAdSeen, k)
		}
	}
	marketNoteMu.Unlock()
}

func init() {
	registerCharForget("marketNotes/marketAdSeen", func(id int64) {
		marketNoteMu.Lock()
		defer marketNoteMu.Unlock()
		for ch, list := range marketNotes {
			kept := list[:0]
			for _, n := range list {
				if n.Bot == nil || n.Bot.CharID != id {
					kept = append(kept, n)
				}
			}
			if len(kept) == 0 {
				delete(marketNotes, ch)
			} else {
				marketNotes[ch] = kept
			}
		}
		for k := range marketAdSeen {
			if k[0] == id {
				delete(marketAdSeen, k)
			}
		}
	})
}
