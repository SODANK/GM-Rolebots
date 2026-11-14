package rolebots

import (
	"regexp"
	"strconv"
	"strings"
)

// Turning a line of fleet chat into an order.
//
// Everything here is pure: strings in, an order struct out, no DB and no clock. The silence
// gate keeps the fleet from interrupting people, so a test can pin it.
//
// Two client facts shape the design. A line starting with "/" is swallowed client-side
// (lscchannel.py) and routed to slash, so orders are ordinary sentences. Dragging a pilot's name
// into chat defaults to "Text only", giving a bare name with no markup; the linked form
// (<url=showinfo:1377//140000123>Name</url>) needs "Show Info", so bare-name matching is the
// main path.

// chatLink is one showinfo link lifted out of a chat line.
type chatLink struct {
	TypeID int64
	ItemID int64
	Label  string
}

// reChatLink matches both link forms. The client and charLink emit <url=...>. The server's
// markup elsewhere (agent dialogue, killmails, mail) emits <a href="...">, which chat drops and
// prints raw, but a pasted line may carry either.
var reChatLink = regexp.MustCompile(`(?i)<(?:url=|a\s+href=")showinfo:(\d+)//(\d+)"?>([^<]*)</(?:url|a)>`)

// reChatTag strips the rest of the markup the edit control can emit: <br>, <fontsize=...>,
// <color=...>, <b>/<i>/<u> and their closers.
var reChatTag = regexp.MustCompile(`(?i)</?(?:br|b|i|u|fontsize|color|url|a)\b[^>]*>`)

var reSpace = regexp.MustCompile(`\s+`)

// chatEntities is the escaping SaveTextMarkup applies to plain text. &amp; must be last, or
// "&amp;lt;" unescapes twice into "<".
var chatEntities = []struct{ from, to string }{
	{"&lt;", "<"}, {"&gt;", ">"}, {"&quot;", `"`}, {"&#39;", "'"}, {"&amp;", "&"},
}

// firstCharTypeID and lastCharTypeID bound the bloodline types. A showinfo link is a character
// link only inside this band; outside it the syntax is a station, ship, bookmark or item.
const (
	firstCharTypeID = 1373
	lastCharTypeID  = 1386
)

// extractChatLinks pulls the links out and returns the line with each link replaced by its
// label, so the remaining text still reads as a sentence.
func extractChatLinks(s string) (string, []chatLink) {
	var links []chatLink
	out := reChatLink.ReplaceAllStringFunc(s, func(match string) string {
		g := reChatLink.FindStringSubmatch(match)
		if g == nil {
			return match
		}
		links = append(links, chatLink{
			TypeID: atoi64(g[1]),
			ItemID: atoi64(g[2]),
			Label:  g[3],
		})
		return " " + g[3] + " "
	})
	return out, links
}

// normaliseChat reduces a raw chat line to the words in it.
func normaliseChat(s string) string {
	s = reChatTag.ReplaceAllString(s, " ")
	for _, e := range chatEntities {
		s = strings.ReplaceAll(s, e.from, e.to)
	}
	return strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
}

func atoi64(s string) int64 {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// ── the verb table ──────────────────────────────────────────────────────────

// Order verbs. They are stored on the pilot's state row as OrderVerb, so the strings are
// persisted and must not be renamed.
const (
	OrderAttack    = "attack"
	OrderHoldFire  = "holdfire"
	OrderDefend    = "defend"
	OrderTackle    = "tackle"
	OrderLogi      = "logi"
	OrderFlee      = "flee"
	OrderDock      = "dock"
	OrderUndock    = "undock"
	OrderFollow    = "follow"
	OrderAlign     = "align"
	OrderMine      = "mine"
	OrderPickOre   = "pickore"
	OrderJetcan    = "jetcan"
	OrderHaul      = "haul"
	OrderStockpile = "stockpile"
	OrderSell      = "sell"
	OrderList      = "list"
	OrderStatus    = "status"
	OrderHelp      = "help"
	OrderSetRole   = "setrole"
	OrderStay      = "stay"
	OrderResume    = "resume"
	OrderDismiss   = "dismiss"
	OrderGoHome    = "gohome"
	// OrderAnchor is a fleet-level call only (fleetmirror_orders.go): the hull the line orbits.
	OrderAnchor = "anchor"

	OrderWarpTo      = "warpto"
	OrderOrbit       = "orbit"
	OrderApproach    = "approach"
	OrderKeepRange   = "keeprange"
	OrderWeaponsFree = "weaponsfree"
	OrderOnCall      = "oncall"
	OrderSecondary   = "secondary"
	OrderSplit       = "split"

	// Mirror-fleet calls (fleetmirror_orders.go): an officer takes the fleet from its bot
	// commander, hands it back, or moves the hunt to their own system.
	OrderTakeCommand = "takecommand"
	OrderHandBack    = "handback"
	OrderHuntHere    = "hunthere"
)

type verbSpec struct {
	Verb    string
	Phrases []string
}

// bareOnlyPhrases are one-word verbs that also open ordinary sentences ("home time", "run that
// again", "wait what"). With nobody addressed, such a phrase is an order only when it is the
// whole line.
var bareOnlyPhrases = map[string]bool{
	"home": true, "run": true, "wait": true, "point": true, "report": true, "cover": true,
	"list": true, "split": true, "hunt": true, "rocks": true, "warp": true,
}

// subjectVerbs take a friendly subject by bare name: a crewmate or a human in the gang named
// after the verb is who the order is about ("orbit Onra at 5", "rep Kaelen"). Hostile verbs
// never do, so "attack, Onra" still addresses Onra; their subject comes from a link.
var subjectVerbs = map[string]bool{
	OrderDefend: true, OrderLogi: true, OrderOrbit: true, OrderApproach: true,
	OrderKeepRange: true, OrderWarpTo: true, OrderAnchor: true,
}

// rangeVerbs read a lone number in the line as kilometres.
var rangeVerbs = map[string]bool{
	OrderOrbit: true, OrderApproach: true, OrderKeepRange: true, OrderWarpTo: true,
}

// helpTopics maps the word after "help" to a manual section.
var helpTopics = map[string]string{
	"command": "command", "leading": "command", "lead": "command",
	"fighting": "fighting", "fight": "fighting", "targets": "fighting", "targeting": "fighting",
	"moving": "moving", "move": "moving", "movement": "moving", "warping": "moving",
	"mining": "mining", "ore": "mining", "industry": "mining",
	"housekeeping": "housekeeping", "admin": "housekeeping",
}

// orderVerbs is matched longest phrase first (matchVerb), so "warp to me" means follow while
// "warp out" means flee. A flat scan in declaration order would resolve both to the first.
var orderVerbs = []verbSpec{
	{OrderAttack, []string{"attack", "engage", "kill it", "kill him", "kill her", "kill them", "shoot", "primary", "get him", "get her", "get it", "open fire", "fire"}},
	{OrderHoldFire, []string{"hold fire", "cease fire", "stop shooting", "weapons cold", "weapons off", "hold your fire"}},
	{OrderDefend, []string{"defend", "guard", "protect", "cover me", "cover", "watch my back"}},
	{OrderTackle, []string{"tackle", "point", "scram", "hold him", "hold her", "hold them", "don't let it go", "dont let it go"}},
	{OrderLogi, []string{"rep me", "repair me", "rep", "repair", "logi", "heal"}},
	{OrderFlee, []string{"flee", "run", "run for it", "get out", "warp out", "bail", "scatter", "disengage"}},
	{OrderWarpTo, []string{"warp to", "warp"}},
	{OrderOrbit, []string{"orbit"}},
	{OrderApproach, []string{"approach", "close on"}},
	{OrderKeepRange, []string{"keep at range", "keep at", "keep range"}},
	{OrderWeaponsFree, []string{"weapons free", "fire at will", "free fire", "weapons hot"}},
	{OrderOnCall, []string{"on call", "fire on my call", "wait for the call", "weapons tight"}},
	{OrderSecondary, []string{"secondary"}},
	{OrderSplit, []string{"split up", "split the fleet", "split"}},
	{OrderTakeCommand, []string{"take command", "i have command", "i have the fleet"}},
	{OrderHandBack, []string{"hand back", "hand it back", "you have command", "you have the fleet"}},
	{OrderHuntHere, []string{"hunt here", "hunt"}},
	{OrderDock, []string{"dock up", "dock", "get docked", "get inside"}},
	{OrderUndock, []string{"undock", "launch", "get out here", "get in space"}},
	{OrderFollow, []string{"follow me", "warp to me", "come to me", "come here", "on me", "with me", "stay with me", "follow"}},
	{OrderAlign, []string{"align to", "align out", "align"}},
	{OrderMine, []string{"start mining", "get mining", "mine", "rocks", "get on the rocks"}},
	{OrderPickOre, []string{"pick ore", "mine only", "switch to", "get me some", "get me"}},
	{OrderJetcan, []string{"jet can", "jetcan", "jettison", "can it", "dump the hold"}},
	{OrderHaul, []string{"collect the cans", "pick up the cans", "haul for us", "you haul", "haul"}},
	{OrderStockpile, []string{"stockpile here", "stockpile at", "stockpile", "drop it here", "dump it here", "drop it at", "deliver here"}},
	{OrderSell, []string{"sell the ore", "sell it", "sell", "trade it", "cash it in"}},
	{OrderList, []string{"roll call", "sound off", "who is here", "who's here", "list"}},
	{OrderStatus, []string{"sitrep", "status", "report", "what are you doing"}},
	{OrderHelp, []string{"what can you do", "send the list", "commands", "orders", "help"}},
	{OrderSetRole, []string{"set role", "you are the", "you're the", "youre the", "be the"}},
	{OrderStay, []string{"hold position", "sit tight", "stand by", "stay", "wait"}},
	{OrderResume, []string{"as you were", "carry on", "back to work", "stand down", "resume"}},
	{OrderDismiss, []string{"leave the fleet", "you can go", "dismissed", "dismiss"}},
	{OrderGoHome, []string{"head home", "go home", "home"}},
	// A fleet-level call (fleetmirror_orders.go): the hull the line orbits.
	{OrderAnchor, []string{"anchor on", "anchor"}},
}

// verbPhrases is orderVerbs flattened and sorted longest first, built once.
var verbPhrases = func() []struct {
	Verb   string
	Phrase string
} {
	type pv = struct {
		Verb   string
		Phrase string
	}
	var out []pv
	for _, v := range orderVerbs {
		for _, p := range v.Phrases {
			out = append(out, pv{v.Verb, p})
		}
	}
	// Insertion sort by descending length: the table is small and fixed, and the ordering rule
	// stays visible.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j].Phrase) > len(out[j-1].Phrase); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}()

// groupAliases address the whole crew or a role within it. Roles use the operator fleet's
// vocabulary (gangs.go).
var groupAliases = map[string]string{
	"all": "", "everyone": "", "everybody": "", "fleet": "", "bots": "", "boys": "", "team": "",
	"miners": RoleMiner, "miner": RoleMiner,
	"haulers": RoleHauler, "hauler": RoleHauler,
	"guns": RoleCombat, "combat": RoleCombat, "fighters": RoleCombat, "dps": RoleCombat,
	"traders": RoleTrader, "trader": RoleTrader,
	"hunters": RoleHunter, "hunter": RoleHunter,
	"tackle": RoleTackler, "tacklers": RoleTackler,
	"logi":    RoleLogi,
	"ewar":    RoleEwar, "jammers": RoleEwar,
}

// ── parsing ─────────────────────────────────────────────────────────────────

// crewMember is the parser's view of one bot under command.
type crewMember struct {
	CharID int64
	Name   string
	Role   string
}

// ChatOrder is one parsed instruction.
type ChatOrder struct {
	Verb string
	// Targets is who must obey. Empty means the whole crew.
	Targets []int64
	// Rest is what was left after the addressee and verb were taken out: an ore name, a station,
	// a role, a target's name.
	Rest string
	// Subject is a character named in the line who is not crew: an enemy to shoot, a fleetmate
	// to guard.
	Subject int64
	// Ambiguous is set when a bare name matched more than one pilot. No order is issued; the
	// fleet asks which one.
	Ambiguous []string
	// Places are the non-character links in the line (a planet, a station, a gate), in order.
	Places []chatLink
	// RangeM is a distance the line gave ("at 30", "10km", "500m"), valid when HasRange.
	RangeM   float64
	HasRange bool
	// Topic is the manual section a "help" line asked for, "" for the whole manual.
	Topic string
}

// maxOrderRangeM caps a spoken range, so a typo cannot send a ship to the far side of a system.
const maxOrderRangeM = 300000

var (
	reRangeAt   = regexp.MustCompile(`\bat\s+(\d+(?:\.\d+)?)\s*(km|k|m)?\b`)
	reRangeUnit = regexp.MustCompile(`\b(\d+(?:\.\d+)?)\s*(km|k|m)\b`)
	reRangeLone = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
)

// parseRangeM reads a distance from a lower-cased line: "at 30" and a bare unit-less number
// after "at" are kilometres, "km" and "k" are kilometres, "m" is metres. It returns the
// distance in metres and the matched text, so the caller can cut it out of the line.
func parseRangeM(low string) (float64, string, bool) {
	g := reRangeAt.FindStringSubmatch(low)
	if g == nil {
		g = reRangeUnit.FindStringSubmatch(low)
	}
	if g == nil {
		return 0, "", false
	}
	v, err := strconv.ParseFloat(g[1], 64)
	if err != nil || v < 0 {
		return 0, "", false
	}
	if g[2] != "m" {
		v *= 1000
	}
	if v > maxOrderRangeM {
		v = maxOrderRangeM
	}
	return v, g[0], true
}

// maxOrderTokens bounds what counts as an order. A verb buried in a long sentence is a person
// talking, not a command.
const maxOrderTokens = 8

// ParseOrder turns one chat line into an order, or reports that it was not one. The silence
// gate: a line is an order only when it names a bot or starts with a verb phrase, and is
// short. Everything else is dropped without a reply.
func ParseOrder(raw string, crew []crewMember) (ChatOrder, bool) {
	return parseOrderIn(raw, crew, nil)
}

// parseOrderIn is ParseOrder with a second name pool: pals are the humans in the gang, who can
// be the subject of a friendly verb but never an addressee.
func parseOrderIn(raw string, crew, pals []crewMember) (ChatOrder, bool) {
	stripped, links := extractChatLinks(raw)
	text := normaliseChat(stripped)
	if text == "" {
		return ChatOrder{}, false
	}
	low := strings.ToLower(text)

	byID := map[int64]crewMember{}
	for _, c := range crew {
		byID[c.CharID] = c
	}

	var o ChatOrder
	named := false
	// linkWords are the labels of links that are not addressees; the bare-only rule reads past
	// them, so "point <link>" is still an order.
	var linkWords []string

	// 1. Links. A character link inside the crew is an addressee, one outside it is the subject.
	//    Non-character links go to Places and stay in Rest as possible arguments.
	for _, l := range links {
		if l.TypeID < firstCharTypeID || l.TypeID > lastCharTypeID {
			o.Places = append(o.Places, l)
			linkWords = append(linkWords, strings.ToLower(l.Label))
			continue
		}
		if _, ok := byID[l.ItemID]; ok {
			o.Targets = append(o.Targets, l.ItemID)
			named = true
			low = removeWord(low, strings.ToLower(l.Label))
			text = removeWord(text, l.Label)
			continue
		}
		if o.Subject == 0 {
			o.Subject = l.ItemID
		}
		linkWords = append(linkWords, strings.ToLower(l.Label))
	}

	// 1b. A friendly verb's subject by bare name, taken from after the verb so it is not read
	//     as the addressee.
	if o.Subject == 0 {
		if vm, ok := matchVerb(low); ok && subjectVerbs[vm.Verb] {
			cut := vm.At + len(vm.Phrase)
			pool := make([]crewMember, 0, len(crew)+len(pals))
			pool = append(append(pool, crew...), pals...)
			if id, name, ok := matchSubjectName(low[cut:], pool); ok {
				o.Subject = id
				low = low[:cut] + " " + removeName(low[cut:], strings.ToLower(name))
				text = removeNameCI(text, name)
			}
		}
	}

	// 2. Group aliases, leading or trailing.
	aliasWord, preAlias := "", low
	if !named {
		if role, alias, ok := matchGroupAlias(low); ok {
			named = true
			aliasWord = alias
			if role != "" {
				for _, c := range crew {
					if c.Role == role {
						o.Targets = append(o.Targets, c.CharID)
					}
				}
				// A role nobody fills addresses nobody; it must not widen to the whole crew.
				if len(o.Targets) == 0 {
					o.Targets = []int64{-1}
				}
			}
			low = removeWord(low, alias)
			text = removeWord(text, alias)
		}
	}

	// 3. Bare name: what the drag dialog's default produces.
	if !named {
		ids, amb, ok := matchBareName(low, crew)
		switch {
		case len(amb) > 0:
			o.Ambiguous = amb
			return o, true
		case ok:
			o.Targets = append(o.Targets, ids...)
			named = true
			for _, id := range ids {
				n := strings.ToLower(byID[id].Name)
				low = removeName(low, n)
				text = removeNameCI(text, byID[id].Name)
			}
		}
	}

	low = strings.TrimSpace(reSpace.ReplaceAllString(low, " "))

	vm, ok := matchVerb(low)
	if !ok && aliasWord != "" && isVerbPhrase(aliasWord) {
		// "tackle" or "logi" alone: the seat's name is also an order to everyone.
		o.Targets, named = nil, false
		low = strings.TrimSpace(reSpace.ReplaceAllString(preAlias, " "))
		vm, ok = matchVerb(low)
	}
	if !ok {
		return ChatOrder{}, false
	}
	// The gate: somebody was named, or the line opens with the verb.
	if !named && vm.At != 0 {
		return ChatOrder{}, false
	}
	rest := vm.Rest
	if !named && bareOnlyPhrases[vm.Phrase] && !onlyLinkWords(rest, linkWords) {
		return ChatOrder{}, false
	}
	if len(strings.Fields(rest)) > maxOrderTokens {
		return ChatOrder{}, false
	}
	r, span, ok := parseRangeM(rest)
	if !ok && rangeVerbs[vm.Verb] {
		// "keep at range 20", "orbit me 5": after a range verb a lone number is kilometres.
		if g := reRangeLone.FindString(rest); g != "" {
			r, span, ok = parseRangeM("at " + g)
			span = g
		}
	}
	if ok {
		o.RangeM, o.HasRange = r, true
		rest = strings.Replace(rest, span, " ", 1)
	}

	o.Verb = vm.Verb
	o.Rest = strings.TrimSpace(strings.Trim(reSpace.ReplaceAllString(rest, " "), ",.:;!? "))
	if o.Verb == OrderHelp {
		for _, w := range strings.Fields(o.Rest) {
			if t, ok := helpTopics[strings.Trim(w, ",.:;!?-")]; ok {
				o.Topic = t
				break
			}
		}
	}
	return o, true
}

// isVerbPhrase reports whether a word is itself one of the verb phrases.
func isVerbPhrase(w string) bool {
	for _, pv := range verbPhrases {
		if pv.Phrase == w {
			return true
		}
	}
	return false
}

// onlyLinkWords reports whether rest holds nothing but punctuation, "all", and the labels of
// the line's links.
func onlyLinkWords(rest string, linkWords []string) bool {
	for _, w := range linkWords {
		if w != "" {
			rest = removeWord(rest, w)
		}
	}
	rest = removeWord(rest, "all")
	return strings.Trim(rest, ",.:;!?- ") == ""
}

// matchSubjectName finds a pilot named in the text after a verb: the whole name anywhere, a
// first or last name on any word, or a prefix of three letters or more on the first word.
// Only an unambiguous match counts; "me" and the like name nobody, and the order falls back
// to the commander.
func matchSubjectName(tail string, pool []crewMember) (int64, string, bool) {
	for _, c := range pool {
		if c.Name != "" && containsWords(tail, strings.ToLower(c.Name)) {
			return c.CharID, c.Name, true
		}
	}
	words := strings.Fields(tail)
	for i, raw := range words {
		tok := strings.Trim(raw, ",.:;!?-")
		if tok == "" {
			continue
		}
		var hit crewMember
		n := 0
		for _, c := range pool {
			first, last := splitName(strings.ToLower(c.Name))
			if tok == first || tok == last {
				hit, n = c, n+1
			}
		}
		if n == 1 {
			return hit.CharID, hit.Name, true
		}
		if n > 1 || i > 0 || len(tok) < 3 {
			continue
		}
		n = 0
		for _, c := range pool {
			if strings.HasPrefix(strings.ToLower(c.Name), tok) {
				hit, n = c, n+1
			}
		}
		if n == 1 {
			return hit.CharID, hit.Name, true
		}
	}
	return 0, "", false
}

// matchGroupAlias finds "all", "miners" and the like at either end of the line.
func matchGroupAlias(low string) (string, string, bool) {
	words := strings.Fields(low)
	if len(words) == 0 {
		return "", "", false
	}
	for _, i := range []int{0, len(words) - 1} {
		w := strings.Trim(words[i], ",.:;!?-")
		// "you're the hauler": a seat named after an article is the order's argument, not who
		// it is for.
		if i > 0 && i == len(words)-1 {
			switch words[i-1] {
			case "the", "a", "an":
				continue
			}
		}
		if role, ok := groupAliases[w]; ok {
			return role, w, true
		}
	}
	return "", "", false
}

// matchBareName resolves an unlinked name in narrowing order: whole name, first name, last
// name, then a unique prefix of at least three characters. Ambiguity is never guessed: the
// fleet asks which pilot is meant.
func matchBareName(low string, crew []crewMember) ([]int64, []string, bool) {
	if len(crew) == 0 {
		return nil, nil, false
	}
	words := strings.Fields(low)
	if len(words) == 0 {
		return nil, nil, false
	}

	// Whole name first, anywhere in the line, so it beats the first-name pass that finds two.
	for _, c := range crew {
		if containsWords(low, strings.ToLower(c.Name)) {
			return []int64{c.CharID}, nil, true
		}
	}

	// A single token, at the front or after a comma at the end: a name mid-sentence is usually
	// somebody being talked about.
	cands := []string{strings.Trim(words[0], ",.:;!?-")}
	if len(words) > 1 {
		cands = append(cands, strings.Trim(words[len(words)-1], ",.:;!?-"))
	}
	for _, tok := range cands {
		if tok == "" {
			continue
		}
		if _, isAlias := groupAliases[tok]; isAlias {
			continue
		}
		var hits []int64
		var names []string
		for _, c := range crew {
			first, last := splitName(strings.ToLower(c.Name))
			if tok == first || tok == last {
				hits = append(hits, c.CharID)
				names = append(names, c.Name)
			}
		}
		if len(hits) == 1 {
			return hits, nil, true
		}
		if len(hits) > 1 {
			return nil, names, false
		}
		if len(tok) < 3 {
			continue
		}
		hits, names = nil, nil
		for _, c := range crew {
			if strings.HasPrefix(strings.ToLower(c.Name), tok) {
				hits = append(hits, c.CharID)
				names = append(names, c.Name)
			}
		}
		if len(hits) == 1 {
			return hits, nil, true
		}
		if len(hits) > 1 {
			return nil, names, false
		}
	}
	return nil, nil, false
}

// verbMatch is the verb phrase matchVerb chose: the verb, the phrase as matched, where it
// started, and the line with the phrase cut out.
type verbMatch struct {
	Verb, Phrase, Rest string
	At                 int
}

// matchVerb finds the longest verb phrase and reports where it started, so the caller can
// tell "mine here" from "we should mine here".
func matchVerb(low string) (verbMatch, bool) {
	best := verbMatch{At: -1}
	for _, pv := range verbPhrases {
		i := indexWords(low, pv.Phrase)
		if i < 0 {
			continue
		}
		// Longest wins; on a tie the earlier position wins.
		if len(pv.Phrase) > len(best.Phrase) || (len(pv.Phrase) == len(best.Phrase) && i < best.At) {
			best = verbMatch{Verb: pv.Verb, Phrase: pv.Phrase, At: i,
				Rest: strings.TrimSpace(low[:i] + " " + low[i+len(pv.Phrase):])}
		}
	}
	if best.At < 0 {
		return verbMatch{}, false
	}
	return best, true
}

// ── word-boundary helpers ───────────────────────────────────────────────────
//
// strings.Contains is wrong for these: "mine" is inside "determine" and "all" inside "wall".

func isWordByte(b byte) bool {
	return b == '\'' || b == '-' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// indexWords finds sub in s on word boundaries, or -1.
func indexWords(s, sub string) int {
	if sub == "" {
		return -1
	}
	from := 0
	for {
		i := strings.Index(s[from:], sub)
		if i < 0 {
			return -1
		}
		i += from
		leftOK := i == 0 || !isWordByte(s[i-1])
		end := i + len(sub)
		rightOK := end >= len(s) || !isWordByte(s[end])
		if leftOK && rightOK {
			return i
		}
		from = i + 1
	}
}

func containsWords(s, sub string) bool { return indexWords(s, sub) >= 0 }

// removeWord cuts one word-bounded occurrence out of a line.
func removeWord(s, word string) string {
	i := indexWords(s, word)
	if i < 0 {
		return s
	}
	return s[:i] + " " + s[i+len(word):]
}

func removeName(s, name string) string {
	s = removeWord(s, name)
	first, last := splitName(name)
	s = removeWord(s, first)
	if last != "" {
		s = removeWord(s, last)
	}
	return s
}

func removeNameCI(s, name string) string {
	low := strings.ToLower(s)
	i := indexWords(low, strings.ToLower(name))
	if i < 0 {
		first, _ := splitName(strings.ToLower(name))
		i = indexWords(low, first)
		if i < 0 {
			return s
		}
		return s[:i] + " " + s[i+len(first):]
	}
	return s[:i] + " " + s[i+len(name):]
}

func splitName(name string) (string, string) {
	if i := strings.IndexByte(name, ' '); i > 0 {
		return name[:i], strings.TrimSpace(name[i+1:])
	}
	return name, ""
}
