package rolebots

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Summit composition: a post from line banks and real facts (summit.go). A post is a title (new
// threads only), an opener, one to three fact sentences, a closer and the author's signature, each
// from a pool keyed by beat kind, part and the author's voice (chatter_voice.go).
// Lines are taken least recently used, not at random, and the memory survives a restart
// (rolebot_summit_tpl). Titles and openers are blocked for repeatDays; a pool that runs dry drops
// the opener rather than repeat it. Slots are {name}: a line with a slot the facts cannot fill is
// skipped. Lines are plain text; the Summit escapes on render.

const (
	summitPartTitle = "title"
	summitPartOpen  = "open"
	summitPartFact  = "fact"
	summitPartClose = "close"
)

type summitLine struct {
	ID   string
	Text string
}

// summitBank maps kind|part|voice to lines. voiceAny ("") is the general pool.
var summitBank = map[string][]summitLine{}

func summitBankKey(kind, part string, v chatVoice) string {
	return kind + "|" + part + "|" + string(v)
}

// addSummit registers lines. The ID hashes the text, so adding or reordering
// lines never renames the ones already remembered as used.
func addSummit(kind, part string, v chatVoice, lines ...string) {
	k := summitBankKey(kind, part, v)
	for _, t := range lines {
		h := fnv.New64a()
		h.Write([]byte(k + "|" + t))
		summitBank[k] = append(summitBank[k], summitLine{ID: fmt.Sprintf("%s.%s.%x", kind, part, h.Sum64()&0xffffffffff), Text: t})
	}
}

// summitPool is a part's candidates for a voice: the voice's own lines and
// the general ones. A title falls back to its family's generic titles
// ("war" for "war.gloat"), for a reply that has to open its story's thread.
func summitPool(kind, part string, v chatVoice) []summitLine {
	out := append([]summitLine(nil), summitBank[summitBankKey(kind, part, v)]...)
	if v != voiceAny {
		out = append(out, summitBank[summitBankKey(kind, part, voiceAny)]...)
	}
	if fam := summitFamily(kind); len(out) == 0 && part == summitPartTitle && fam != kind {
		out = append(out, summitBank[summitBankKey(fam, part, v)]...)
		if v != voiceAny {
			out = append(out, summitBank[summitBankKey(fam, part, voiceAny)]...)
		}
	}
	return out
}

// summitFamily is a kind's prefix: "war" for "war.gloat".
func summitFamily(kind string) string {
	if i := strings.IndexByte(kind, '.'); i > 0 {
		return kind[:i]
	}
	return kind
}

var summitSlotRe = regexp.MustCompile(`\{([a-zA-Z]+)\}`)

// fillSummit fills every slot, or fails if any slot has no fact.
func fillSummit(text string, f summitFacts) (string, bool) {
	ok := true
	out := summitSlotRe.ReplaceAllStringFunc(text, func(s string) string {
		v := strings.TrimSpace(f[s[1:len(s)-1]])
		if v == "" {
			ok = false
		}
		return v
	})
	return out, ok
}

// pickSummitLine takes the least recently used fillable line, ties at
// random. blockSec > 0 refuses a line used within it; stale lets a fully
// blocked pool fall back to its least recent line anyway.
func pickSummitLine(pool []summitLine, f summitFacts, mem map[string]int64, now, blockSec int64,
	stale bool, skip map[string]bool, rng *rand.Rand) (summitLine, string, bool) {

	type cand struct {
		l    summitLine
		text string
		at   int64
	}
	var fresh, all []cand
	for _, l := range pool {
		if skip[l.ID] {
			continue
		}
		text, ok := fillSummit(l.Text, f)
		if !ok {
			continue
		}
		c := cand{l, text, mem[l.ID]}
		all = append(all, c)
		if blockSec <= 0 || c.at == 0 || now-c.at >= blockSec {
			fresh = append(fresh, c)
		}
	}
	use := fresh
	if len(use) == 0 && stale {
		use = all
	}
	if len(use) == 0 {
		return summitLine{}, "", false
	}
	rng.Shuffle(len(use), func(i, j int) { use[i], use[j] = use[j], use[i] })
	best := use[0]
	for _, c := range use[1:] {
		if c.at < best.at {
			best = c
		}
	}
	return best.l, best.text, true
}

// summitGenericSlots are the names any sentence may repeat.
var summitGenericSlots = map[string]bool{
	"us": true, "them": true, "usTicker": true, "themTicker": true, "sideA": true, "sideB": true,
	"alliance": true, "allianceTicker": true, "author": true, "authorCorp": true, "authorTicker": true,
}

// summitSpecificSlots is a line's slots other than the generic names.
func summitSpecificSlots(text string) []string {
	var out []string
	for _, mm := range summitSlotRe.FindAllStringSubmatch(text, -1) {
		if !summitGenericSlots[mm[1]] {
			out = append(out, mm[1])
		}
	}
	return out
}

func summitSharesSlot(text string, taken map[string]bool) bool {
	for _, s := range summitSpecificSlots(text) {
		if taken[s] {
			return true
		}
	}
	return false
}

// summitPost is a composed post.
type summitPost struct {
	Title     string
	Body      string
	Templates []string // line IDs used, for the memory
}

// composeSummitPost writes one post. Pure: the memory and the dice are
// passed in. The signature is added by the caller.
func composeSummitPost(kind string, v chatVoice, f summitFacts, opening bool,
	mem map[string]int64, now int64, repeatDays int, rng *rand.Rand) (summitPost, bool) {

	var p summitPost
	block := int64(repeatDays) * 86400
	used := map[string]bool{}
	take := func(l summitLine) {
		used[l.ID] = true
		p.Templates = append(p.Templates, l.ID)
	}
	if opening {
		l, t, ok := pickSummitLine(summitPool(kind, summitPartTitle, v), f, mem, now, block, true, used, rng)
		if !ok {
			return p, false
		}
		take(l)
		p.Title = t
	}
	var paras []string
	if l, t, ok := pickSummitLine(summitPool(kind, summitPartOpen, v), f, mem, now, block, false, used, rng); ok {
		take(l)
		paras = append(paras, t)
	}
	want := 1 + rng.Intn(3)
	if v == voiceTerse {
		want = 1
	}
	// Fact sentences never share a specific slot (two lines quoting the same
	// ISK read as a template), and the one with the most specifics leads.
	type factLine struct {
		text string
		n    int
	}
	var facts []factLine
	slotsTaken := map[string]bool{}
	pool := summitPool(kind, summitPartFact, v)
	for len(facts) < want {
		// The first sentence is the headline: one with at least two
		// specifics (who, what, where) when the facts fill such a line.
		floor := 0
		if len(facts) == 0 {
			for _, l := range pool {
				if _, ok := fillSummit(l.Text, f); ok {
					if n := len(summitSpecificSlots(l.Text)); n > floor {
						floor = n
					}
				}
			}
			if floor > 2 {
				floor = 2
			}
		}
		var fresh []summitLine
		for _, l := range pool {
			if !summitSharesSlot(l.Text, slotsTaken) && len(summitSpecificSlots(l.Text)) >= floor {
				fresh = append(fresh, l)
			}
		}
		l, t, ok := pickSummitLine(fresh, f, mem, now, 2*86400, len(facts) == 0, used, rng)
		if !ok {
			break
		}
		take(l)
		n := 0
		for _, s := range summitSpecificSlots(l.Text) {
			slotsTaken[s] = true
			n++
		}
		facts = append(facts, factLine{t, n})
	}
	sort.SliceStable(facts, func(i, j int) bool { return facts[i].n > facts[j].n })
	if len(facts) > 0 {
		ts := make([]string, len(facts))
		for i, fl := range facts {
			ts[i] = fl.text
		}
		paras = append(paras, strings.Join(ts, " "))
	}
	if len(paras) == 0 {
		return p, false
	}
	closeOdds := 60
	if v == voiceTerse {
		closeOdds = 30
	}
	if rng.Intn(100) < closeOdds {
		if l, t, ok := pickSummitLine(summitPool(kind, summitPartClose, v), f, mem, now, 3*86400, false, used, rng); ok {
			take(l)
			paras = append(paras, t)
		}
	}
	p.Body = strings.Join(paras, "\n\n")
	return p, true
}

// ── template memory ─────────────────────────────────────────────────────────

var (
	summitTplMu  sync.Mutex
	summitTplMem = map[string]int64{}
)

func summitTplSnapshot() map[string]int64 {
	summitTplMu.Lock()
	defer summitTplMu.Unlock()
	out := make(map[string]int64, len(summitTplMem))
	for k, v := range summitTplMem {
		out[k] = v
	}
	return out
}

func (m *Mod) summitLoadTplMem(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `SELECT tplID, lastUsed FROM rolebot_summit_tpl`)
	if err != nil {
		rlogf("rolebots: summit template memory: %v", err)
		return
	}
	defer rows.Close()
	mem := map[string]int64{}
	for rows.Next() {
		var id string
		var at int64
		if rows.Scan(&id, &at) == nil {
			mem[id] = at
		}
	}
	summitTplMu.Lock()
	summitTplMem = mem
	summitTplMu.Unlock()
}

func (m *Mod) summitTplUsed(ctx context.Context, ids []string, now int64) {
	summitTplMu.Lock()
	for _, id := range ids {
		summitTplMem[id] = now
	}
	summitTplMu.Unlock()
	for _, id := range ids {
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_summit_tpl (tplID, lastUsed, uses) VALUES (?, ?, 1)
			ON DUPLICATE KEY UPDATE lastUsed = VALUES(lastUsed), uses = uses + 1`, id, now)
	}
}

// ── the author's side ───────────────────────────────────────────────────────

// summitCompose fills the author's facts, writes the post and signs it.
func (m *Mod) summitCompose(ctx context.Context, b summitBeatRow, author int64, opening bool) (summitPost, bool) {
	f := summitFacts{}
	for k, v := range b.Facts {
		f[k] = v
	}
	var corpID int64
	var corpName, ticker string
	_ = m.db.QueryRowContext(ctx, `
		SELECT ch.corporationID, COALESCE(c.corporationName, ''), COALESCE(c.tickerName, '')
		  FROM character_ ch LEFT JOIN corporation c ON c.corporationID = ch.corporationID
		 WHERE ch.characterID = ?`, author).Scan(&corpID, &corpName, &ticker)
	f["author"] = m.charNameOr(ctx, author)
	if corpName != "" {
		f["authorCorp"] = corpName
	}
	if ticker != "" {
		f["authorTicker"] = ticker
	}
	v := voiceAny
	if p := m.corpParty(ctx, author, corpID); p != nil {
		v = p.Voice
	}
	d := summitDialsNow()
	rng := rand.New(rand.NewSource(rand.Int63()))
	post, ok := composeSummitPost(b.Kind, v, f, opening, summitTplSnapshot(), clock.Unix(), d.RepeatDays, rng)
	if !ok {
		return post, false
	}
	if sig := m.summitSignature(ctx, author, v, f, rng); sig != "" {
		post.Body += "\n\n" + sig
	}
	return post, true
}

// summitSignature is an author's fixed sign-off, made once and kept. About
// a third of authors have none ("-" in the table).
func (m *Mod) summitSignature(ctx context.Context, author int64, v chatVoice, f summitFacts, rng *rand.Rand) string {
	var sig string
	_ = m.db.QueryRowContext(ctx, `SELECT signature FROM rolebot_summit_authors WHERE characterID = ?`, author).Scan(&sig)
	if sig == "" {
		sig = makeSummitSignature(v, f, rng)
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_summit_authors (characterID, signature) VALUES (?, ?)
			ON DUPLICATE KEY UPDATE signature = VALUES(signature)`, author, sig)
	}
	if sig == "-" {
		return ""
	}
	return sig
}

// makeSummitSignature draws a signature: a motto in the author's voice, a
// plain name-and-ticker line, or both. "-" is no signature.
func makeSummitSignature(v chatVoice, f summitFacts, rng *rand.Rand) string {
	if rng.Intn(100) < 33 {
		return "-"
	}
	mottos := append([]string(nil), summitMottos[v]...)
	mottos = append(mottos, summitMottos[voiceAny]...)
	motto := mottos[rng.Intn(len(mottos))]
	name, tick := f["author"], f["authorTicker"]
	switch rng.Intn(4) {
	case 0:
		return motto
	case 1:
		if tick != "" {
			return "-- " + name + " [" + tick + "]"
		}
		return "-- " + name
	case 2:
		if tick != "" {
			return motto + "\n" + name + " [" + tick + "]"
		}
		return motto
	default:
		return "~ " + motto + " ~"
	}
}
