package rolebots

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Corporation branding: the name, description and logo a bot gives the corporation it founds
// (corpfound.go decides when).
//
// It follows the title vocabulary (corptitlevocab.go): the corporation's kind (titleKindOf over
// the founder's charter tags), the founder's race, and a roll fixed by the founder's ID, plus
// where it is founded, which fills the {region} slot. Every word comes from corpbrandvocab.go.
// It is pure, with no database. A reroll number walks to the next candidate when the caller
// refuses one (a taken name, a logo already flown, a diversity cap); brandProblem refuses what
// nobody may fly.

// corpNameMax is the longest name generated. The column allows 100; the overview bracket, the
// member list and the CONCORD pages do not.
const corpNameMax = 32

// founderSurnameMax: a longer surname crowds the rest of the name out.
const founderSurnameMax = 12

// brandSeed is everything a brand is a function of.
type brandSeed struct {
	FounderID   int64
	FounderName string
	RaceID      int64
	Tags        []string
	RegionName  string
}

// corpBrand is one generated identity. Shapes and Colours are graphicIDs per
// layer (index 0 = slot 1, index 2 = slot 3, the bottom); 0 = empty layer.
// Template, Org and Adj are kept for the diversity caps.
type corpBrand struct {
	Name, Description string
	Kind, Race        string
	Template          string
	Org, Adj          string
	Shapes            [3]int64
	Colours           [3]int64
}

// ── generation ──────────────────────────────────────────────────────────────

// generateCorpBrand builds the brand for one seed and reroll. Deterministic.
func generateCorpBrand(s brandSeed, reroll int) corpBrand {
	kind := titleKindOf(s.Tags)
	race := titleRaceOf(s.RaceID)
	roll := func(slot string) int {
		return tvHash(s.FounderID, fmt.Sprintf("brand|%d|%s", reroll, slot))
	}
	pick := func(pools []tvPool, slot string) string {
		c := brandCandidates(pools, kind, race)
		if len(c) == 0 {
			return ""
		}
		return c[roll(slot)%len(c)]
	}

	lead := regionLead(s.RegionName, kind)
	of := regionOf(s.RegionName)
	surname := founderSurname(s.FounderName)

	var usable []string
	for _, t := range brandCandidates(brandTemplatePools, kind, race) {
		if strings.Contains(t, "{region}") && lead == "" {
			continue
		}
		if strings.Contains(t, "{ofregion}") && of == "" {
			continue
		}
		if strings.Contains(t, "{founder") && surname == "" {
			continue
		}
		usable = append(usable, t)
	}
	b := corpBrand{Kind: kind, Race: race}
	if len(usable) == 0 {
		usable = []string{"{founder} {org}"}
		if surname == "" {
			usable = []string{"{adj} {org}"}
		}
	}
	b.Template = usable[roll("tmpl")%len(usable)]

	fill := map[string]string{
		"{region}":   lead,
		"{ofregion}": of,
		"{founder}":  surname,
		"{founders}": possessive(surname),
	}
	slot := func(token string, pools []tvPool) {
		if !strings.Contains(b.Template, token) {
			return
		}
		w := pick(pools, token)
		fill[token] = w
		switch token {
		case "{adj}":
			b.Adj = w
		case "{org}", "{unit}", "{crew}":
			b.Org = w
		}
	}
	slot("{adj}", brandAdjPools)
	slot("{noun}", brandNounPools)
	slot("{org}", brandOrgPools)
	slot("{unit}", brandUnitPools)
	slot("{crew}", brandCrewPools)

	name := b.Template
	// "The Heimatar Fellowship" reads; "The Genesis Enterprises" does not: a plural takes no article.
	if strings.HasPrefix(name, "The {region}") && strings.HasSuffix(b.Org, "s") {
		name = strings.TrimPrefix(name, "The ")
	}
	for k, v := range fill {
		name = strings.ReplaceAll(name, k, v)
	}
	b.Name = strings.Join(strings.Fields(name), " ")

	desc := pick(brandDescPools, "desc")
	desc = strings.ReplaceAll(desc, "{place}", regionPlace(s.RegionName))
	desc = strings.ReplaceAll(desc, "{founder}", s.FounderName)
	if roll("mottoOn")%2 == 0 {
		if motto := pick(brandMottoPools, "motto"); motto != "" {
			desc += " " + motto
		}
	}
	b.Description = sentenceCase(desc)

	b.Shapes, b.Colours = brandLogo(s.FounderID, reroll, kind, race)
	return b
}

// sentenceCase capitalises the first letter of every sentence, since a region that reads "the
// Forge" mid-sentence can open one.
func sentenceCase(s string) string {
	r := []rune(s)
	start := true
	for i, c := range r {
		switch {
		case start && unicode.IsLetter(c):
			r[i] = unicode.ToUpper(c)
			start = false
		case c == '.' || c == '!' || c == '?':
			start = true
		case !unicode.IsSpace(c):
			start = false
		}
	}
	return string(r)
}

// brandCandidates pools every matching row, a more specific row counted more times, so race
// and kind words outweigh the shared ones without crowding them out.
func brandCandidates(pools []tvPool, kind, race string) []string {
	var out []string
	for _, p := range pools {
		if !tvMatches(p.kinds, kind) || !tvMatches(p.races, race) {
			continue
		}
		w := tvSpecificity(p.kinds, p.races) + 1
		for i := 0; i < w; i++ {
			out = append(out, p.names...)
		}
	}
	return out
}

// ── regions ─────────────────────────────────────────────────────────────────

func regionClass(name string) brandRegion {
	if r, ok := brandRegions[name]; ok {
		return r
	}
	return brandRegion{Class: regionNever}
}

// regionLead is the region as it may lead a name, "" when it may not.
func regionLead(name, kind string) string {
	r := regionClass(name)
	switch r.Class {
	case regionPlain:
		return name
	case regionArticle:
		return strings.TrimPrefix(name, "The ")
	case regionWord:
		if kind == tvPirate || kind == tvCartel {
			return name
		}
	}
	return ""
}

// regionOf is the "of <region>" form, "" for a region never named.
func regionOf(name string) string {
	r := regionClass(name)
	if r.Class == regionNever {
		return ""
	}
	if r.Of != "" {
		return r.Of
	}
	if r.Class == regionArticle {
		return "of the " + strings.TrimPrefix(name, "The ")
	}
	return "of " + name
}

// regionPlace is the region as it reads mid-sentence.
func regionPlace(name string) string {
	r := regionClass(name)
	switch {
	case r.Class == regionNever:
		return "deep space"
	case r.Of != "":
		return strings.TrimPrefix(r.Of, "of ")
	case r.Class == regionArticle:
		return "the " + strings.TrimPrefix(name, "The ")
	}
	return name
}

// regionNamable reports whether a corporation may be founded under this region's name.
func regionNamable(name string) bool { return regionClass(name).Class != regionNever }

// ── founder ─────────────────────────────────────────────────────────────────

// founderSurname is the last word of a name of two or more words, "" for a mononym, an
// initial, or anything too long or not a plain word.
func founderSurname(name string) string {
	f := strings.Fields(name)
	if len(f) < 2 {
		return ""
	}
	last := f[len(f)-1]
	if n := len([]rune(last)); n < 3 || n > founderSurnameMax {
		return ""
	}
	for _, r := range last {
		if !unicode.IsLetter(r) && r != '-' && r != '\'' {
			return ""
		}
	}
	return last
}

func possessive(s string) string {
	if s == "" {
		return ""
	}
	if strings.HasSuffix(s, "s") {
		return s + "'"
	}
	return s + "'s"
}

// ── logo ────────────────────────────────────────────────────────────────────

var logoTopShapes, logoBottomShapes = func() ([]int, []int) {
	var top, bottom []int
	for n, mask := range logoShapeSlots {
		if mask&logoSlotTop != 0 {
			top = append(top, n)
		}
		if mask&logoSlotBottom != 0 {
			bottom = append(bottom, n)
		}
	}
	sort.Ints(top)
	sort.Ints(bottom)
	return top, bottom
}()

const (
	logoSlot2Pct = 85
	logoSlot1Pct = 55
)

// brandLogo rolls the three layers. Slot 3 (the bottom) is always a plate; slots 2 and 1 are
// optional top layers. No two painted layers share a colour as it renders (two IDs that draw
// the same hex count as one).
func brandLogo(founderID int64, reroll int, kind, race string) (shapes, colours [3]int64) {
	roll := func(slot string) int {
		return tvHash(founderID, fmt.Sprintf("logo|%d|%s", reroll, slot))
	}
	pal := logoPalettes[race]
	if kind == tvPirate || kind == tvCartel {
		pal = logoPalettes["outlaw"]
	}
	if len(pal) == 0 {
		pal = logoPalettes[tvNeutral]
	}
	used := map[string]bool{}
	colour := func(slot string) int64 {
		start := roll(slot) % len(pal)
		for i := 0; i < len(pal); i++ {
			c := pal[(start+i)%len(pal)]
			if hex := logoColourHex[c]; hex != "" && !used[hex] {
				used[hex] = true
				return c
			}
		}
		return pal[start]
	}

	shapes[2] = logoShapeGraphicBase + int64(logoBottomShapes[roll("s3")%len(logoBottomShapes)])
	colours[2] = colour("c3")
	if roll("p2")%100 < logoSlot2Pct {
		shapes[1] = logoShapeGraphicBase + int64(logoTopShapes[roll("s2")%len(logoTopShapes)])
		colours[1] = colour("c2")
	}
	if roll("p1")%100 < logoSlot1Pct {
		n := logoTopShapes[roll("s1")%len(logoTopShapes)]
		if g := logoShapeGraphicBase + int64(n); g == shapes[1] {
			n = logoTopShapes[(roll("s1")+1)%len(logoTopShapes)]
		}
		shapes[0] = logoShapeGraphicBase + int64(n)
		colours[0] = colour("c1")
	}
	return shapes, colours
}

// ── what nobody may fly ─────────────────────────────────────────────────────

// brandLore is the set a name must not borrow from: the static lists plus the database's NPC
// corporation and faction names. Ship names are not in it, since hull names are good nouns.
type brandLore struct {
	phrases []string        // canonical, matched as whole words
	tokens  map[string]bool // single canonical words
}

// newBrandLore builds the set. npcNames are NPC corporation and faction names, matched as
// phrases.
func newBrandLore(npcNames []string) *brandLore {
	l := &brandLore{tokens: map[string]bool{}}
	for _, t := range brandLoreTokens {
		l.tokens[canonName(t)] = true
	}
	for _, t := range brandProfanity {
		l.tokens[t] = true
	}
	for _, p := range brandBlockPhrases {
		l.phrases = append(l.phrases, canonName(p))
	}
	for _, n := range npcNames {
		c := canonName(strings.TrimSuffix(strings.TrimSpace(n), " Inc."))
		if c == "" || brandNPCPhraseIgnore[c] {
			continue
		}
		l.phrases = append(l.phrases, c)
	}
	return l
}

// canonName lowercases and turns everything but letters, digits, hyphens and apostrophes into
// single spaces.
func canonName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '\'':
			return unicode.ToLower(r)
		}
		return ' '
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// nameWords are the canonical words of a name, each also split at hyphens, with a trailing
// possessive dropped.
func nameWords(canon string) []string {
	var out []string
	for _, w := range strings.Fields(canon) {
		w = strings.TrimSuffix(strings.TrimSuffix(w, "'s"), "'")
		out = append(out, w)
		if strings.Contains(w, "-") {
			out = append(out, strings.Split(w, "-")...)
		}
	}
	return out
}

// brandProblem says why a name may not be flown, "" when it may.
func brandProblem(name string, lore *brandLore) string {
	if name == "" {
		return "empty"
	}
	if n := len([]rune(name)); n > corpNameMax {
		return fmt.Sprintf("too long (%d > %d)", n, corpNameMax)
	}
	canon := canonName(name)
	seen := map[string]bool{}
	for _, w := range strings.Fields(canon) {
		if w == "of" || w == "the" {
			continue
		}
		if seen[w] {
			return "repeats " + w
		}
		seen[w] = true
	}
	if lore == nil {
		lore = newBrandLore(nil)
	}
	padded := " " + canon + " "
	for _, p := range lore.phrases {
		if strings.Contains(padded, " "+p+" ") {
			return "borrows " + p
		}
	}
	for _, w := range nameWords(canon) {
		// A plural borrows as surely as the word: "Kadors" is still Kador.
		if lore.tokens[w] || (strings.HasSuffix(w, "s") && lore.tokens[strings.TrimSuffix(w, "s")]) {
			return "borrows " + w
		}
	}
	return ""
}

// pickCorpBrand walks the rerolls until a brand passes brandProblem and the caller's own test
// (nil = any). ok is false when none of n does.
func pickCorpBrand(s brandSeed, lore *brandLore, n int, accept func(corpBrand) bool) (corpBrand, bool) {
	for r := 0; r < n; r++ {
		b := generateCorpBrand(s, r)
		if brandProblem(b.Name, lore) != "" {
			continue
		}
		if accept != nil && !accept(b) {
			continue
		}
		return b, true
	}
	return corpBrand{}, false
}

// ── diversity ───────────────────────────────────────────────────────────────

const (
	// brandOrgCap: how many live bot-founded corporations may share one organisation word
	// ("Holdings", "Raiders").
	brandOrgCap = 8
	// brandTemplateShare: the most of them one template may account for.
	brandTemplateShare = 0.20
)

// brandDiversity keeps a board of a hundred names from reading as twenty variations of one.
type brandDiversity struct {
	total int
	orgs  map[string]int
	tmpls map[string]int
	adjs  map[string]bool // region|adj
}

func newBrandDiversity() *brandDiversity {
	return &brandDiversity{orgs: map[string]int{}, tmpls: map[string]int{}, adjs: map[string]bool{}}
}

func (d *brandDiversity) allows(b corpBrand, region string) bool {
	if b.Org != "" && d.orgs[strings.ToLower(b.Org)] >= brandOrgCap {
		return false
	}
	limit := int(brandTemplateShare * float64(d.total+1))
	if limit < 2 {
		limit = 2
	}
	if d.tmpls[b.Template]+1 > limit {
		return false
	}
	if b.Adj != "" && d.adjs[region+"|"+strings.ToLower(b.Adj)] {
		return false
	}
	return true
}

func (d *brandDiversity) add(b corpBrand, region string) {
	d.total++
	if b.Org != "" {
		d.orgs[strings.ToLower(b.Org)]++
	}
	d.tmpls[b.Template]++
	if b.Adj != "" {
		d.adjs[region+"|"+strings.ToLower(b.Adj)] = true
	}
}
