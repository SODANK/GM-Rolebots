package rolebots

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// What a bot may say, and the words it says it in. A kind is one sort of thing worth a line;
// a pool is a set of wordings that serve one or more kinds, optionally flavoured for a
// dominant trait. A line is picked by filtering the pools to those whose every {slot} the
// event can fill, weighting the speaker's trait pool over the neutral one, and rolling (the
// model of corpbrand.go).
// Rules the pools must keep, held by chatter_lines_test.go: every kind has at least four
// neutral wordings; every {slot} in a line is one the kind declares; no line begins with "/"
// (lscchannel.py routes those to the slash service).

// chatterKind is one sort of event.
type chatterKind struct {
	Kind     string
	Label    string        // the panel
	Class    string        // panel grouping: ceo, industry, fleet, personal, social
	Priority int           // 1 low .. 5 high; wins the queue when it is full
	Cooldown time.Duration // per (channel, kind)
	Slots    []string      // every slot a line of this kind may use
	Wired    bool          // false = catalogued, no producer yet
}

const (
	chatterClassCEO      = "ceo"
	chatterClassIndustry = "industry"
	chatterClassFleet    = "fleet"
	chatterClassPersonal = "personal"
	chatterClassSocial   = "social"
)

var chatterKinds = map[string]chatterKind{
	// CEO & officer decisions (corpceo.go ceoLog).
	"ceo.war.declare":     {Kind: "ceo.war.declare", Label: "War declared", Class: chatterClassCEO, Priority: 5, Cooldown: time.Hour, Slots: []string{"corp"}, Wired: true},
	"ceo.war.retract":     {Kind: "ceo.war.retract", Label: "War retracted", Class: chatterClassCEO, Priority: 4, Cooldown: time.Hour, Slots: []string{"corp"}, Wired: true},
	"ceo.war.over":        {Kind: "ceo.war.over", Label: "War over", Class: chatterClassCEO, Priority: 4, Cooldown: time.Hour, Slots: []string{"corp"}, Wired: true},
	"ceo.war.mutual":      {Kind: "ceo.war.mutual", Label: "War made mutual", Class: chatterClassCEO, Priority: 4, Cooldown: time.Hour, Slots: []string{"corp"}, Wired: true},
	"ceo.war.unmutual":    {Kind: "ceo.war.unmutual", Label: "Mutuality withdrawn", Class: chatterClassCEO, Priority: 3, Cooldown: time.Hour, Slots: []string{"corp"}, Wired: true},
	"ceo.war.hire":        {Kind: "ceo.war.hire", Label: "Mercenaries hired", Class: chatterClassCEO, Priority: 3, Cooldown: 2 * time.Hour, Slots: []string{"corp", "isk"}, Wired: true},
	"ceo.war.siege":       {Kind: "ceo.war.siege", Label: "Siege", Class: chatterClassCEO, Priority: 4, Cooldown: time.Hour, Slots: []string{"detail"}, Wired: true},
	"ceo.dip.pact":        {Kind: "ceo.dip.pact", Label: "Pact signed", Class: chatterClassCEO, Priority: 4, Cooldown: time.Hour, Slots: []string{"corp"}, Wired: true},
	"ceo.dip.peace":       {Kind: "ceo.dip.peace", Label: "Peace made", Class: chatterClassCEO, Priority: 4, Cooldown: time.Hour, Slots: []string{"corp"}, Wired: true},
	"ceo.dip.stance":      {Kind: "ceo.dip.stance", Label: "Stance changed", Class: chatterClassCEO, Priority: 3, Cooldown: 30 * time.Minute, Slots: []string{"corp", "stance"}, Wired: true},
	"ceo.dip.letter":      {Kind: "ceo.dip.letter", Label: "Letter sent", Class: chatterClassCEO, Priority: 2, Cooldown: 30 * time.Minute, Slots: []string{"corp"}, Wired: true},
	"ceo.dip.envoy":       {Kind: "ceo.dip.envoy", Label: "Envoy", Class: chatterClassCEO, Priority: 2, Cooldown: 30 * time.Minute, Slots: []string{"who"}, Wired: true},
	"ceo.dip.envoylost":   {Kind: "ceo.dip.envoylost", Label: "Envoy lost", Class: chatterClassCEO, Priority: 3, Cooldown: 30 * time.Minute, Slots: []string{"who"}, Wired: true},
	"ceo.pos.anchor":      {Kind: "ceo.pos.anchor", Label: "Tower anchored", Class: chatterClassCEO, Priority: 4, Cooldown: time.Hour, Slots: []string{"detail"}, Wired: true},
	"ceo.pos.online":      {Kind: "ceo.pos.online", Label: "Tower online", Class: chatterClassCEO, Priority: 3, Cooldown: time.Hour, Slots: []string{"detail"}, Wired: true},
	"ceo.pos.lost":        {Kind: "ceo.pos.lost", Label: "Tower lost", Class: chatterClassCEO, Priority: 5, Cooldown: time.Hour, Slots: []string{"detail"}, Wired: true},
	"ceo.pos.teardown":    {Kind: "ceo.pos.teardown", Label: "Tower torn down", Class: chatterClassCEO, Priority: 3, Cooldown: time.Hour, Slots: []string{"detail"}, Wired: true},
	"ceo.hr.hire":         {Kind: "ceo.hr.hire", Label: "Member hired", Class: chatterClassCEO, Priority: 2, Cooldown: 15 * time.Minute, Slots: []string{"who", "first"}, Wired: true},
	"ceo.estate.hq":       {Kind: "ceo.estate.hq", Label: "HQ moved", Class: chatterClassCEO, Priority: 3, Cooldown: time.Hour, Slots: []string{"station"}, Wired: true},
	"ceo.estate.factory":  {Kind: "ceo.estate.factory", Label: "Factory rented", Class: chatterClassCEO, Priority: 3, Cooldown: time.Hour, Slots: []string{"station"}, Wired: true},
	"ceo.estate.branch":   {Kind: "ceo.estate.branch", Label: "Branch office", Class: chatterClassCEO, Priority: 3, Cooldown: time.Hour, Slots: []string{"station"}, Wired: true},
	"ceo.shares.dividend": {Kind: "ceo.shares.dividend", Label: "Dividend paid", Class: chatterClassCEO, Priority: 3, Cooldown: 6 * time.Hour, Slots: []string{"isk"}, Wired: true},
	"ceo.prod.post":       {Kind: "ceo.prod.post", Label: "Work posted", Class: chatterClassCEO, Priority: 2, Cooldown: 20 * time.Minute, Slots: []string{"what"}, Wired: true},
	"ceo.prod.bpo":        {Kind: "ceo.prod.bpo", Label: "Print bought", Class: chatterClassCEO, Priority: 2, Cooldown: 20 * time.Minute, Slots: []string{"isk"}, Wired: true},
	"ceo.prod.printsale":  {Kind: "ceo.prod.printsale", Label: "Print sold", Class: chatterClassCEO, Priority: 2, Cooldown: 20 * time.Minute, Slots: []string{"what", "isk"}, Wired: true},
	"ceo.prod.library":    {Kind: "ceo.prod.library", Label: "Library plan", Class: chatterClassCEO, Priority: 1, Cooldown: 20 * time.Minute, Slots: []string{}, Wired: true},
	"ceo.roles.promote":   {Kind: "ceo.roles.promote", Label: "Promotion", Class: chatterClassCEO, Priority: 2, Cooldown: 20 * time.Minute, Slots: []string{"who"}, Wired: true},
	"ceo.roles.seat":      {Kind: "ceo.roles.seat", Label: "Officer seated", Class: chatterClassCEO, Priority: 2, Cooldown: 20 * time.Minute, Slots: []string{"who"}, Wired: true},
	"ceo.roles.order":     {Kind: "ceo.roles.order", Label: "Standing order", Class: chatterClassCEO, Priority: 3, Cooldown: 20 * time.Minute, Slots: []string{"system", "detail"}, Wired: true},

	// Industry & research, per pilot.
	// The personal kinds are the pilot's OWN work and say so; the ind.corp*
	// kinds are the corporation's, each counted in its own units (runs,
	// levels, copies), so a member reading corp chat can tell the two apart.
	"ind.install":      {Kind: "ind.install", Label: "Own job installed", Class: chatterClassIndustry, Priority: 2, Cooldown: 10 * time.Minute, Slots: []string{"work", "what", "eta"}, Wired: true},
	"ind.corpjob":      {Kind: "ind.corpjob", Label: "Corporation build installed", Class: chatterClassIndustry, Priority: 2, Cooldown: 10 * time.Minute, Slots: []string{"work", "what"}, Wired: true},
	"ind.corpresearch": {Kind: "ind.corpresearch", Label: "Corporation research installed", Class: chatterClassIndustry, Priority: 2, Cooldown: 10 * time.Minute, Slots: []string{"what", "activity", "levels"}, Wired: true},
	"ind.corpcopy":     {Kind: "ind.corpcopy", Label: "Corporation copy installed", Class: chatterClassIndustry, Priority: 2, Cooldown: 10 * time.Minute, Slots: []string{"what", "copies"}, Wired: true},
	"lab.install":      {Kind: "lab.install", Label: "Own lab job installed", Class: chatterClassIndustry, Priority: 2, Cooldown: 10 * time.Minute, Slots: []string{"what", "activity"}, Wired: true},
	"lab.enrol":        {Kind: "lab.enrol", Label: "R&D agent", Class: chatterClassIndustry, Priority: 1, Cooldown: 30 * time.Minute, Slots: []string{"field"}, Wired: true},

	// Fleets.
	"fleet.formed": {Kind: "fleet.formed", Label: "Fleet formed", Class: chatterClassFleet, Priority: 5, Cooldown: 15 * time.Minute, Slots: []string{"fleet", "activity", "system", "n"}, Wired: true},
	"fleet.down":   {Kind: "fleet.down", Label: "Fleet stood down", Class: chatterClassFleet, Priority: 2, Cooldown: 15 * time.Minute, Slots: []string{"fleet", "why"}, Wired: true},

	// Personal.
	"pers.loss":        {Kind: "pers.loss", Label: "Ship lost", Class: chatterClassPersonal, Priority: 4, Cooldown: 15 * time.Minute, Slots: []string{"ship", "system", "who", "rats"}, Wired: true},
	"pers.ransom":      {Kind: "pers.ransom", Label: "Ransom demanded", Class: chatterClassPersonal, Priority: 3, Cooldown: 15 * time.Minute, Slots: []string{"isk", "victim"}, Wired: true},
	"pers.ransom.paid": {Kind: "pers.ransom.paid", Label: "Ransom paid", Class: chatterClassPersonal, Priority: 3, Cooldown: 15 * time.Minute, Slots: []string{"isk", "victim"}, Wired: true},
	"corp.leave":       {Kind: "corp.leave", Label: "Leaving the corporation", Class: chatterClassPersonal, Priority: 3, Cooldown: time.Hour, Slots: []string{"corp"}, Wired: true},
	"pers.skillbook":   {Kind: "pers.skillbook", Label: "Skillbook bought", Class: chatterClassPersonal, Priority: 1, Cooldown: time.Hour, Slots: []string{"skill"}, Wired: true},
	"pers.sale":        {Kind: "pers.sale", Label: "Print listed", Class: chatterClassPersonal, Priority: 1, Cooldown: time.Hour, Slots: []string{"what", "isk"}, Wired: true},
	"pers.skill.done":  {Kind: "pers.skill.done", Label: "Skill finished", Class: chatterClassPersonal, Priority: 1, Cooldown: time.Hour, Slots: []string{"skill"}},
	"pers.kill":        {Kind: "pers.kill", Label: "Kill", Class: chatterClassPersonal, Priority: 3, Cooldown: 15 * time.Minute, Slots: []string{"ship", "system", "who"}},
	"pers.bigsale":     {Kind: "pers.bigsale", Label: "Big sale", Class: chatterClassPersonal, Priority: 1, Cooldown: time.Hour, Slots: []string{"what", "isk"}},

	// Social: greetings and answers. No cooldown of their own; the ceiling
	// and the per-person reply gap hold them.
	"social.greet":  {Kind: "social.greet", Label: "Greeting", Class: chatterClassSocial, Priority: 5, Slots: []string{"who", "first", "n"}, Wired: true},
	"social.ack":    {Kind: "social.ack", Label: "Acknowledgement", Class: chatterClassSocial, Priority: 5, Slots: []string{"first"}, Wired: true},
	"social.gf":     {Kind: "social.gf", Label: "gf", Class: chatterClassSocial, Priority: 5, Slots: []string{"first"}, Wired: true},
	"social.thanks": {Kind: "social.thanks", Label: "You're welcome", Class: chatterClassSocial, Priority: 5, Slots: []string{"first"}, Wired: true},
	"social.status": {Kind: "social.status", Label: "Status answer", Class: chatterClassSocial, Priority: 5, Slots: []string{"status", "first"}, Wired: true},
	"social.join":   {Kind: "social.join", Label: "Invite sent", Class: chatterClassSocial, Priority: 5, Slots: []string{"first"}, Wired: true},
}

// chatterKindList is the catalogue in a stable order for the panel.
func chatterKindList() []chatterKind {
	out := make([]chatterKind, 0, len(chatterKinds))
	for _, k := range chatterKinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Class != out[j].Class {
			return chatterClassRank(out[i].Class) < chatterClassRank(out[j].Class)
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func chatterClassRank(c string) int {
	switch c {
	case chatterClassCEO:
		return 0
	case chatterClassIndustry:
		return 1
	case chatterClassFleet:
		return 2
	case chatterClassPersonal:
		return 3
	}
	return 4
}

// chatterCEOKind maps a ledger row (corpceo.go ceoLog) to a kind. "" means the row is the
// operator's ledger only, such as a stipend or a hangar shuffle.
func chatterCEOKind(power, verb string) string {
	switch power {
	case powerWar:
		switch verb {
		case "declare":
			return "ceo.war.declare"
		case "retract":
			return "ceo.war.retract"
		case "warover", "lapse":
			return "ceo.war.over"
		case "mutual":
			return "ceo.war.mutual"
		case "unmutual":
			return "ceo.war.unmutual"
		case "hire":
			return "ceo.war.hire"
		case "siege":
			return "ceo.war.siege"
		}
	case powerDiplomacy:
		switch verb {
		case "pact":
			return "ceo.dip.pact"
		case "peace", "reparations":
			return "ceo.dip.peace"
		case "ally", "friend", "rival", "enemy", "withdraw", "pactbreak":
			return "ceo.dip.stance"
		case "letter", "answer":
			return "ceo.dip.letter"
		case "envoy":
			return "ceo.dip.envoy"
		case "envoylost":
			return "ceo.dip.envoylost"
		}
	case powerAlliance:
		switch verb {
		case "found", "joined", "leave", "kick":
			return "ceo.dip.stance"
		case "declare":
			return "ceo.war.declare"
		}
	case powerStarbase:
		switch verb {
		case "anchor":
			return "ceo.pos.anchor"
		case "online":
			return "ceo.pos.online"
		case "lost":
			return "ceo.pos.lost"
		case "teardown":
			return "ceo.pos.teardown"
		}
	case powerHR:
		switch verb {
		case "accept", "welcome":
			return "ceo.hr.hire"
		}
	case powerEstate:
		switch verb {
		case "hq":
			return "ceo.estate.hq"
		case "factory":
			return "ceo.estate.factory"
		case "branch":
			return "ceo.estate.branch"
		}
	case powerShares:
		if verb == "dividend" {
			return "ceo.shares.dividend"
		}
	case powerProduction:
		switch verb {
		case "post":
			return "ceo.prod.post"
		case "bpo":
			return "ceo.prod.bpo"
		case "printsale":
			return "ceo.prod.printsale"
		case "library":
			return "ceo.prod.library"
		}
	case powerRoles:
		switch verb {
		case "promote":
			return "ceo.roles.promote"
		case "seat":
			return "ceo.roles.seat"
		case "order":
			return "ceo.roles.order"
		}
	}
	return ""
}

// ── the pools ───────────────────────────────────────────────────────────────

// chatterPool is a set of wordings. kinds is space-separated; trait "" is the neutral pool
// every speaker may draw from, anything else is drawn only by a speaker whose dominant trait
// it names, three times as often as the neutral lines. A wording may hold "[a|b]"
// alternation groups (expandAlt, chatter_voice.go).
type chatterPool struct {
	kinds string
	trait string
	lines []string
}

var chatterPools = []chatterPool{
	// ── CEO & officers ──
	{kinds: "ceo.war.declare", lines: []string{
		"We're at war with {corp} as of now. Fly accordingly.",
		"War declared on {corp}. It goes live in a day — check your overview settings.",
		"Paperwork's filed with CONCORD: {corp} are a legal target from tomorrow.",
		"The bill's paid and {corp} are on it. [Insure your ships.|Check your clone is up to date.|Fit for a fight.]",
	}},
	{kinds: "ceo.war.declare", trait: TraitAggression, lines: []string{
		"Declared on {corp}. Finally.",
		"{corp} wanted a fight. They've got one from tomorrow.",
	}},
	{kinds: "ceo.war.declare", trait: TraitCaution, lines: []string{
		"Declared on {corp}. Keep your heads down until we know their numbers.",
		"We're at war with {corp} from tomorrow. Nobody undocks alone until I say.",
	}},
	{kinds: "ceo.war.retract", lines: []string{
		"Stood the war with {corp} down.",
		"Retracted against {corp}. Back to business.",
		"I've pulled our declaration on {corp}. [Not worth another bill.|We've better uses for the ISK.|They've had enough.]",
		"Called off the war with {corp}. It winds down in a day — don't start anything you can't finish by then.",
	}},
	{kinds: "ceo.war.mutual", lines: []string{
		"The war with {corp} is mutual now. They want it as much as we do — no more bills, no more quarter.",
		"{corp} and I agree on one thing: this isn't finished. War's mutual.",
		"Made it mutual with {corp}. No end date on this one.",
		"{corp} took mutual. [It runs until one side gives up.|Nobody's paying CONCORD to keep this going now.]",
	}},
	{kinds: "ceo.war.mutual", trait: TraitAggression, lines: []string{
		"{corp} want to keep going. Good. So do we. It's mutual.",
	}},
	{kinds: "ceo.war.unmutual", lines: []string{
		"The war with {corp} isn't mutual any more. CONCORD bills again from here.",
		"Mutuality with {corp} is off. If nobody pays to keep it going, it ends.",
		"The {corp} war is back on the weekly bill. Mutual's gone.",
		"No longer mutual with {corp}. [The next bill decides whether it carries on.|Somebody pays CONCORD or it lapses.]",
	}},
	{kinds: "ceo.war.over", lines: []string{
		"It's over with {corp}. Back to work.",
		"The war with {corp} has lapsed. Overview back to normal.",
		"We're done with {corp}. [Stand down.|Undock as normal.|Get back to the belts.]",
		"{corp} aren't war targets any more. Shoot one now and CONCORD will have you.",
	}},
	{kinds: "ceo.war.hire", lines: []string{
		"Hired {corp} for the war. {isk} well spent, I hope.",
		"{corp} are on retainer for {isk}. Let them earn it.",
		"Signed {corp} as mercenaries — {isk} up front.",
		"We've [hired|bought] some help: {corp}, for {isk}. Don't shoot them.",
	}},
	{kinds: "ceo.war.hire", trait: TraitGreed, lines: []string{
		"{isk} to {corp} for the war. Every one of those had better come back as a wreck.",
	}},
	{kinds: "ceo.war.siege", lines: []string{
		"Siege news: {detail}",
		"From the war desk — {detail}",
		"Where the tower fight stands: {detail}",
		"[Heads up|Update], war business: {detail}",
	}},
	{kinds: "ceo.dip.pact", lines: []string{
		"Pact signed with {corp}. Treat them as ours.",
		"{corp} and us have an understanding now. Blue them.",
		"We've a pact with {corp}. [Help them if they call.|Don't shoot them.|Their enemies are ours.]",
		"Shook hands with {corp}. They're on our side until I say otherwise.",
	}},
	{kinds: "ceo.dip.pact", trait: TraitLoyalty, lines: []string{
		"{corp} are friends of the corporation as of today. Act like it.",
	}},
	{kinds: "ceo.dip.peace", lines: []string{
		"Made peace with {corp}.",
		"It's settled with {corp}. Guns down.",
		"Peace with {corp}. Whatever happened, it's done.",
		"{corp} and us are square [now|again]. Leave them be.",
	}},
	{kinds: "ceo.dip.stance", lines: []string{
		"I've marked {corp} down as {stance}.",
		"For the record: {corp} are {stance} to us now.",
		"Standings updated: {corp}, {stance}.",
		"{corp} — {stance}. [Check your overview.|Set your own standings to match.]",
	}},
	{kinds: "ceo.dip.stance", trait: TraitLoyalty, lines: []string{
		"{corp} are {stance} to us now. Everyone fly like it.",
	}},
	{kinds: "ceo.dip.letter", lines: []string{
		"Sent a letter to {corp}. We'll see what comes back.",
		"Wrote to {corp} on the corporation's behalf.",
		"Mail's gone to {corp}. [Ball's in their court.|Waiting on a reply.]",
		"Had words with {corp} by mail. Nothing to report yet.",
	}},
	{kinds: "ceo.dip.envoy", lines: []string{
		"{who} is carrying a letter for us. Give them a clear road.",
		"Sent {who} out as envoy.",
		"{who} is flying our letter over. [Escort them if you're near.|Leave them be.|Don't get them killed.]",
		"Our envoy is {who}. If you see them in trouble, help.",
	}},
	{kinds: "ceo.dip.envoylost", lines: []string{
		"We lost {who} on the road. The letter went with them.",
		"{who} didn't make it to the meeting. Somebody knew the route.",
		"{who} got popped on the way. The letter never arrived.",
		"Envoy's down — {who} was caught [en route|on a gate|halfway there].",
	}},
	{kinds: "ceo.dip.envoylost", trait: TraitAggression, lines: []string{
		"They killed our envoy, {who}. That gets answered.",
	}},
	{kinds: "ceo.pos.anchor", lines: []string{
		"Tower's anchored. {detail}",
		"We've got a tower in the ground: {detail}",
		"Starbase news: {detail}",
		"Anchoring's done — {detail}. [Fuel comes next.|Onlining next.]",
	}},
	{kinds: "ceo.pos.online", lines: []string{
		"Tower's online. {detail}",
		"Shields up on the tower — {detail}",
		"The tower is up and running: {detail}",
		"Force field's up — {detail}. [Keep the fuel bay full.|Fuel runs go on the board.]",
	}},
	{kinds: "ceo.pos.lost", lines: []string{
		"We lost the tower. {detail}",
		"Bad news from the field: {detail}",
		"Tower's down for good. {detail}",
		"[Ugly|Rough] day for the corporation — {detail}",
	}},
	{kinds: "ceo.pos.lost", trait: TraitAggression, lines: []string{
		"Tower's gone. {detail} — somebody owes us for that.",
		"They took our tower. {detail}. [We'll remember who.|We'll be back for that moon.]",
	}},
	{kinds: "ceo.pos.teardown", lines: []string{
		"Pulling a tower down. {detail}",
		"Tower's coming out of the ground: {detail}",
		"Unanchoring a tower: {detail}",
		"We're packing up a tower — {detail}. [Haulers wanted.|Somebody bring a hauler.]",
	}},
	{kinds: "ceo.hr.hire", lines: []string{
		"Welcome aboard, {who}.",
		"{who} is one of us now. Say hello.",
		"New name on the roster: {who}. o7",
		"{who} just signed on. [Show {first} the ropes.|Be nice.|Make them welcome.]",
	}},
	{kinds: "ceo.estate.hq", lines: []string{
		"HQ's moving to {station}.",
		"We've a new address: {station}. Update your bookmarks.",
		"Corporation headquarters is {station} from today.",
		"Packed up the head office — we live at {station} now. [Move your clone.|Set your home station.|]",
	}},
	{kinds: "ceo.estate.factory", lines: []string{
		"Rented factory space at {station}.",
		"We've lines at {station} now. Bring prints.",
		"Took a factory lease at {station}. [Build there when you can.|Corporation slots, use them.]",
		"{station} has manufacturing slots for us now.",
	}},
	{kinds: "ceo.estate.factory", trait: TraitGreed, lines: []string{
		"New factory at {station}. More lines, more margin.",
	}},
	{kinds: "ceo.estate.branch", lines: []string{
		"Opened a branch office at {station}.",
		"We've a second desk now — {station}.",
		"We've an office at {station} as well now. Corporation hangars too.",
		"Branch office open at {station}. [Saves a few jumps.|Use the hangars.]",
	}},
	{kinds: "ceo.shares.dividend", lines: []string{
		"Dividend's out — {isk} across the shareholders.",
		"Paid {isk} in dividends today. Check your wallets.",
		"Shareholders: {isk} paid out. [Good quarter.|Enjoy it.|]",
		"The treasury paid {isk} back to the people who own it.",
	}},
	{kinds: "ceo.shares.dividend", trait: TraitGreed, lines: []string{
		"{isk} out the door in dividends. Don't spend it all on ammo.",
	}},
	{kinds: "ceo.prod.post", lines: []string{
		"Posted work for {what} on the board.",
		"There's a {what} order up on the production board for anyone with a free line.",
		"New job on the board: {what}. [First come, first served.|Grab it if you're idle.]",
		"We need {what}. It's on the production board.",
	}},
	{kinds: "ceo.prod.post", trait: TraitToil, lines: []string{
		"{what} is on the board. Lines don't fill themselves.",
	}},
	{kinds: "ceo.prod.bpo", lines: []string{
		"Bought a print for the library — {isk}.",
		"The library grew today. {isk} worth of blueprint.",
		"Spent {isk} on a new original for the corporation.",
		"Another BPO in the hangar — {isk}. [Research comes next.|Worth it.]",
	}},
	{kinds: "ceo.prod.printsale", lines: []string{
		"Sold a {what} print for {isk}.",
		"Moved a spare {what} print — {isk} to the treasury.",
		"The corporation's {what} blueprint is sold. {isk} in the wallet.",
		"Let the {what} print go for {isk}. [We had two.|It wasn't earning its keep.]",
	}},
	{kinds: "ceo.prod.library", lines: []string{
		"Went over the blueprint library today. A few gaps to fill.",
		"Library's been reviewed. Research list is longer than I'd like.",
		"Audited the prints. [Half of them need ME work.|Too many originals sitting unresearched.|We're short a few hulls.]",
		"Spent the evening in the blueprint library. The list goes on the board.",
	}},
	{kinds: "ceo.roles.promote", lines: []string{
		"{who} has been promoted. Well earned.",
		"A rank up for {who}.",
		"Promotion for {who}. [Congratulations.|Keep it up.|]",
		"{who} gets more roles as of today.",
	}},
	{kinds: "ceo.roles.seat", lines: []string{
		"{who} takes a seat on the staff.",
		"{who} is one of the officers now.",
		"{who} has an officer's seat. [Listen to them.|Same rules apply.]",
		"Gave {who} a place on the staff. They've earned it.",
	}},
	{kinds: "ceo.roles.order", lines: []string{
		"Standing order: {detail}",
		"Orders for {system}: {detail}",
		"Word from the chair — {detail}",
		"New orders [are out|in effect]: {detail}",
	}},

	// ── industry & research ──
	// Personal work says it is the pilot's own; corporation work names the corporation.
	{kinds: "ind.install", lines: []string{
		"Put {work} of {what} on my own line. Ready in {eta}.",
		"{work} of {what} cooking on my own print. {eta} to go.",
		"My line's busy with {what} for the next {eta}.",
		"Installed {what} for myself, {work}.",
	}},
	{kinds: "ind.install", trait: TraitToil, lines: []string{
		"{what}, {work}, my own account. Back to it.",
		"Another {work} of {what} on my line.",
	}},
	{kinds: "ind.install", trait: TraitGreed, lines: []string{
		"{work} of {what} on my own line. Should sell.",
		"{what} on my line — margin's decent this week.",
	}},
	{kinds: "ind.corpjob", lines: []string{
		"Installed the corporation's {what} build — {work} on the corporation's print.",
		"{work} of {what} for the corporation, on the line now.",
		"Picked up the corporation's {what} order. {work} installed.",
		"Building {what} for the corporation, {work}. [Corp print, corp minerals.|Off the production board.]",
	}},
	{kinds: "ind.corpjob", trait: TraitLoyalty, lines: []string{
		"Corporation build's in: {work} of {what}. Happy to do it.",
	}},
	{kinds: "ind.corpresearch", lines: []string{
		"Started {activity} on the corporation's {what} — {levels}.",
		"The corporation's {what} is in for {activity}, {levels}.",
		"Put the corporation's {what} in for {levels} of {activity}.",
		"Corp print in the lab: {what}, {activity}, {levels}. [Worth the wait.|Should pay off on the next build.]",
	}},
	{kinds: "ind.corpresearch", trait: TraitToil, lines: []string{
		"Corporation print {what}: {activity}, {levels}. It'll be a while.",
	}},
	{kinds: "ind.corpcopy", lines: []string{
		"Copying the corporation's {what} — {copies} for the library.",
		"The corporation's {what} is in the copier. {copies}.",
		"Running {copies} off the corporation's {what}.",
		"{copies} of the corporation's {what} [on the way|in the works] for the builders.",
	}},
	{kinds: "lab.install", lines: []string{
		"My {what} is in for {activity}.",
		"Started {activity} on my own {what}.",
		"Lab slot's taken — {activity} on my {what}.",
		"Booked a lab slot for my [own |]{what}: {activity}.",
	}},
	{kinds: "lab.install", trait: TraitCaution, lines: []string{
		"Started {activity} on my {what}. Slow and steady.",
	}},
	{kinds: "lab.install", trait: TraitToil, lines: []string{
		"My {what}: {activity}. Won't see it back for a while.",
	}},
	{kinds: "lab.enrol", lines: []string{
		"Signed on with an R&D agent for {field}. My own project.",
		"Got a research agent working {field} for me now.",
		"Found myself an R&D agent in {field}. [Points start today.|Slow points, but they add up.]",
		"Started piling up {field} research points with an agent. Mine, not the corporation's.",
	}},

	// ── fleets ──
	{kinds: "fleet.formed", lines: []string{
		"Taking {fleet} out — {activity} out of {system}, {n} of us.",
		"{fleet} is forming up in {system}. {activity}, {n} pilots.",
		"Fleet's up: {fleet}, {activity}, {n} aboard.",
		"Rolling out with {fleet} — {activity}.",
	}},
	{kinds: "fleet.formed", trait: TraitLeadership, lines: []string{
		"{fleet} forms on me in {system}. {activity}, {n} ships.",
		"I've {n} pilots for {fleet}. {activity}. Undocking shortly.",
	}},
	{kinds: "fleet.formed", trait: TraitAggression, lines: []string{
		"{fleet} is going out hunting — {activity}, {n} of us. Should be fun.",
	}},
	{kinds: "fleet.down", lines: []string{
		"{fleet} is standing down — {why}.",
		"{fleet} is done for the day: {why}.",
		"Bringing {fleet} home.",
		"{fleet} is [over|finished] — {why}. Thanks all.",
	}},

	// ── personal ──
	{kinds: "pers.loss", lines: []string{
		"Lost my {ship} in {system}.",
		"Well. That {ship} is scrap. {system}.",
		"{who} took my {ship} in {system}.",
		"Well, I'm in a pod. {system}. {who} got me.",
		"{who} killed me in {system}. Watch out.",
		"Need a new ship. Lost the {ship} in {system}.",
		"That's my {ship} gone. {system}.",
		"Avoid {system}. Lost my {ship} there.",
		"Down in {system}. The {rats} got me.",
		"Belt {rats} ate my {ship} in {system}. Embarrassing.",
		"Don't rat in {system} solo. The {rats} are nasty. Ask me how I know.",
	}},
	{kinds: "pers.loss", trait: TraitAggression, lines: []string{
		"Lost the {ship} in {system}. I'll be back for them.",
		"{who} got my {ship}. Noted.",
	}},
	{kinds: "pers.loss", trait: TraitCaution, lines: []string{
		"Lost my {ship} in {system}. Should have left sooner.",
	}},
	{kinds: "pers.loss", trait: TraitGreed, lines: []string{
		"There goes a {ship}. Insurance had better pay out.",
	}},
	{kinds: "pers.ransom", lines: []string{
		"Holding {victim} for {isk}.",
		"Got {victim} pointed. {isk} and they walk.",
		"{victim} has a choice: {isk}, or a trip back to their clone.",
		"Asked {victim} for {isk}. [Clock's ticking.|We'll see if they pay.|Waiting on the wallet.]",
	}},
	{kinds: "pers.ransom.paid", lines: []string{
		"{victim} paid. {isk}, easiest money all week.",
		"{isk} from {victim}. A pleasure doing business.",
		"{victim} paid up. Let them go.",
		"Ransom's in: {isk} from {victim}. [A deal's a deal.|They flew off in one piece.]",
	}},
	// A member on its way out of the corporation (corpleave.go), said before presence drops the channel.
	{kinds: "corp.leave", lines: []string{
		"That's it for me and {corp}. Fly safe, all.",
		"Handing in my badge at {corp}. It's been a time.",
		"Done with {corp}. Don't wait up.",
		"o7 {corp}. I'm out.",
	}},
	{kinds: "pers.skillbook", lines: []string{
		"Picked up a {skill} book.",
		"Bought {skill}. Training it next.",
		"Bought the {skill} book [at last|today|finally].",
		"{skill} book's in my hangar. [Training starts when this one's done.|Long train ahead.]",
	}},
	{kinds: "pers.skillbook", trait: TraitLoyalty, lines: []string{
		"Corp wanted me on {skill}. Book's bought.",
	}},
	{kinds: "pers.sale", lines: []string{
		"Listed a spare print of my own, {what}, for {isk}.",
		"Selling my {what} — I don't build from it any more. {isk}.",
		"Put my {what} print on the market at {isk}.",
		"A {what} print of mine is up for {isk}. [Make an offer.|Corpmates get first look.]",
	}},
	{kinds: "pers.skill.done", lines: []string{
		"{skill} finished training.",
		"Done with {skill}.",
		"{skill} trained. [One more off the list.|On to the next.|]",
		"Finally finished {skill}. That one took forever.",
	}},
	{kinds: "pers.kill", lines: []string{
		"Dropped a {ship} in {system}.",
		"{who} lost a {ship} to me in {system}.",
		"Killed {who}'s {ship} in {system}. [Pod got away.|gf.|Easy.]",
		"One {ship} fewer in {system}.",
	}},
	{kinds: "pers.bigsale", lines: []string{
		"Sold {what} for {isk}.",
		"{isk} for a {what}. Good day.",
		"Finally sold the {what}. {isk} in the wallet.",
		"Moved a {what} for {isk}. [Not bad.|Best sale this month.|Can't complain.]",
	}},

	// ── social ──
	{kinds: "social.greet", lines: []string{
		"o7 {first}.",
		"{first}. Good to see you.",
		"Hey {first}.",
		"o7",
		"{first}. {n} of us about today.",
	}},
	{kinds: "social.greet", trait: TraitLoyalty, lines: []string{
		"Chair's in. o7 {first}.",
		"Welcome back, {first}.",
	}},
	{kinds: "social.greet", trait: TraitToil, lines: []string{
		"{first}. Lines are running, nothing on fire.",
	}},
	{kinds: "social.greet", trait: TraitLeadership, lines: []string{
		"{first}, o7. Shout if you want a fleet.",
	}},
	{kinds: "social.ack", lines: []string{
		"o7",
		"o7 {first}",
		"Hey.",
		"o/",
	}},
	{kinds: "social.gf", lines: []string{
		"gf",
		"gf {first}",
		"Good fight, {first}. [o7|Well flown.|Same again soon.]",
		"[o7 gf|gf all|Good fights, everyone.]",
	}},
	{kinds: "social.thanks", lines: []string{
		"np",
		"Any time, {first}.",
		"No problem.",
		"[Happy to help, {first}.|Sure thing.|Anytime.]",
	}},
	{kinds: "social.status", lines: []string{
		"{status}.",
		"Me? {status}.",
		"Right now: {status}.",
		"Currently {status}. [You?|Need something?|]",
	}},
	{kinds: "social.status", trait: TraitToil, lines: []string{
		"{status}. Why?",
	}},
	{kinds: "social.join", lines: []string{
		"Invite's out, {first} — accept it in your fleet window.",
		"Sent you an invite, {first}.",
		"Check your invites, {first}. [Should be there now.|It's waiting for you.]",
		"Invite's on its way, {first}. [Accept and form up on me.|See you in fleet.|]",
	}},
}

// chatterOfferSuffix is appended to a fleet.formed line when seats are open
// to the corporation's humans (fleetmirror.go). Not part of the template ID.
var chatterOfferSuffix = []string{
	"X up for a seat.",
	"Room for one more — x up.",
	"X up and I'll send an invite.",
	"X up if you're coming.",
}

// chatterLine is one candidate wording.
type chatterLine struct {
	ID    string
	Text  string
	Trait string
}

var reChatterSlot = regexp.MustCompile(`\{(\w+)\}`)

// chatterCandidates lists the wordings a speaker of this trait may use for a
// kind: neutral once, the trait's own pool three times.
func chatterCandidates(kind, trait string) []chatterLine {
	var out []chatterLine
	for pi, p := range chatterPools {
		if !poolServes(p, kind) {
			continue
		}
		weight := 0
		switch p.trait {
		case "":
			weight = 1
		case trait:
			weight = 3
		}
		for li, text := range p.lines {
			c := chatterLine{ID: kind + "#" + itoa(pi) + "#" + itoa(li), Text: text, Trait: p.trait}
			for w := 0; w < weight; w++ {
				out = append(out, c)
			}
		}
	}
	return out
}

func poolServes(p chatterPool, kind string) bool {
	for _, k := range strings.Fields(p.kinds) {
		if k == kind {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// lineSlotsFilled reports whether every {slot} in text has a non-empty value.
func lineSlotsFilled(text string, slots map[string]string) bool {
	for _, g := range reChatterSlot.FindAllStringSubmatch(text, -1) {
		if slots[g[1]] == "" {
			return false
		}
	}
	return true
}

// renderChatter picks and fills one wording. roll selects among the usable
// candidates (a random number in production, a fixed one in tests). ok is
// false when no wording can be filled from the slots given, or the result
// would start with "/".
func renderChatter(kind, trait string, slots map[string]string, roll int) (tmplID, text string, ok bool) {
	cands := chatterCandidates(kind, trait)
	usable := cands[:0:0]
	for _, c := range cands {
		if lineSlotsFilled(c.Text, slots) {
			usable = append(usable, c)
		}
	}
	if len(usable) == 0 {
		return "", "", false
	}
	n := len(usable)
	c := usable[((roll%n)+n)%n]
	// The alternation options are drawn from the same roll, so a fixed roll renders the
	// same text.
	state := uint64(roll)
	text = expandAlt(c.Text, func(k int) int {
		state = splitmix64(state + 1)
		return int(state % uint64(k))
	})
	out := reChatterSlot.ReplaceAllStringFunc(text, func(m string) string {
		g := reChatterSlot.FindStringSubmatch(m)
		return slots[g[1]]
	})
	out = strings.Join(strings.Fields(out), " ")
	if out == "" || strings.HasPrefix(out, "/") {
		return "", "", false
	}
	return c.ID, out, true
}

// chatterFlavourTraits are the six a wording may lean on, in tie-break order.
var chatterFlavourTraits = []string{
	TraitToil, TraitGreed, TraitCaution, TraitAggression, TraitLoyalty, TraitLeadership,
}

// chatterTraitFloor is how far from even a trait must sit before it colours
// the voice. Traits run -100..+100 (tuning.go); only the positive side has a
// pool, because "not greedy" is not a way of talking.
const chatterTraitFloor = 15

// dominantTrait is the trait a speaker's lines lean on, or "" for an even
// temper.
func dominantTrait(traits map[string]int) string {
	best, bestV := "", chatterTraitFloor-1
	for _, k := range chatterFlavourTraits {
		if v := traits[k]; v > bestV {
			best, bestV = k, v
		}
	}
	return best
}

// firstName is the half of a pilot's name a friend uses.
func firstName(name string) string {
	f, _ := splitName(strings.TrimSpace(name))
	return f
}
