package rolebots

import (
	"context"
	"fmt"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// The fleet manual: what a human can say to bots in a fleet, written once as a table and
// rendered three ways. A player who invites bots into their own fleet gets the conscript
// manual; an officer in a corporation fleet gets the chair's (how to steer the op and how to
// take command); anyone else seated there gets the rider's. Each goes by mail once per player,
// kind and version (rolebot_fc_manual_sent), and again on "help". "help <topic>" answers in
// chat from the same table (manualChat).
//
// Every example in the table is parsed by a test (TestManualExamplesParse), so the manual
// cannot advertise a phrase the parser does not take, and every verb must appear in it.
// Raise fcManualVersion when the table changes and every past reader gets the new one once.

const fcManualVersion = 2

// manualKind is who a manual is for.
type manualKind string

const (
	manualConscript manualKind = "conscript"
	manualDirector  manualKind = "director"
	manualRider     manualKind = "rider"
)

// manualTopics is the section order.
var manualTopics = []string{"command", "fighting", "moving", "mining", "housekeeping"}

var manualTopicTitle = map[string]string{
	"command": "Who's in charge", "fighting": "Fighting", "moving": "Moving",
	"mining": "Mining and hauling", "housekeeping": "Housekeeping",
}

// manualEntry is one line of the manual. Say is an example the parser must read as Verb; Show
// is what the reader sees when it differs (a placeholder in square brackets).
type manualEntry struct {
	Topic string
	Say   string
	Show  string
	Verb  string
	Does  string
	For   []manualKind
}

var (
	forC   = []manualKind{manualConscript}
	forD   = []manualKind{manualDirector}
	forCD  = []manualKind{manualConscript, manualDirector}
	forAll = []manualKind{manualConscript, manualDirector, manualRider}
)

var manualEntries = []manualEntry{
	// Who's in charge.
	{"command", "Ravi you're the logi", "[name] you're the logi", OrderSetRole,
		"give one of us a seat: miner, hauler, guns, tackle, logi or ewar.", forC},
	{"command", "take command", "", OrderTakeCommand,
		"you lead the fleet. Warp Gang, Regroup and Kick are yours, and only your calls count.", forD},
	{"command", "hand back", "", OrderHandBack,
		"the commander takes the fleet back. Leaving or logging off does the same.", forD},
	{"command", "follow me", "", OrderFollow, "we warp to you and stay on you.", forC},
	{"command", "follow me", "", OrderFollow, "while you command, the op moves wherever you go.", forD},

	// Fighting.
	{"fighting", "attack", "", OrderAttack,
		"we shoot what you have locked; nothing locked, whatever is shooting at us.", forC},
	{"fighting", "primary pithi", "primary [rat or linked pilot]", OrderAttack,
		"that one is the primary.", forCD},
	{"fighting", "secondary", "secondary [rat]", OrderSecondary,
		"the next target; tag 2 does the same.", forCD},
	{"fighting", "hold fire", "", OrderHoldFire, "guns cold until you say otherwise.", forCD},
	{"fighting", "on call", "", OrderOnCall,
		"we shoot only what you call: a tag, a lock under attack, a name.", forCD},
	{"fighting", "weapons free", "", OrderWeaponsFree, "pick our own targets again.", forCD},
	{"fighting", "tackle", "", OrderTackle, "point and web it, guns cold.", forC},
	{"fighting", "split", "", OrderSplit, "tackle holds the secondary, guns kill the primary.", forC},
	{"fighting", "defend me", "defend me / guard [name]", OrderDefend,
		"stay on that pilot and shoot whatever shoots them.", forC},
	{"fighting", "rep me", "", OrderLogi, "a logi pilot fits remote repairs and mends you and the fleet.", forC},
	{"fighting", "anchor on Kaelen", "anchor on [name]", OrderAnchor,
		"the line orbits that pilot in a fight.", forD},
	{"fighting", "stay", "hold position", OrderStay, "the line anchors and fires on call only.", forD},

	// Moving.
	{"moving", "align out", "", OrderAlign, "we turn away from whatever is shooting.", forC},
	{"moving", "align to planet", "align to [linked planet, gate or station]", OrderAlign,
		"we turn toward it.", forC},
	{"moving", "warp to planet at 30", "warp to [linked place] at 30", OrderWarpTo,
		"we warp there and land that far off; a bare \"warp\" after an align goes where we aligned.", forC},
	{"moving", "warp", "", OrderWarpTo, "warp to what we aligned to.", forC},
	{"moving", "orbit me at 5", "orbit [name] at 5", OrderOrbit, "orbit at that many km.", forC},
	{"moving", "approach Kaelen", "approach [name]", OrderApproach, "close on them.", forC},
	{"moving", "keep at range 20", "keep at range [km]", OrderKeepRange, "hold that distance off you, or off a name.", forC},
	{"moving", "flee", "", OrderFlee, "everyone warps off grid. A pointed ship says so and fights.", forC},
	{"moving", "dock", "", OrderDock, "dock where you are, or anywhere in system.", forC},
	{"moving", "undock", "", OrderUndock, "get back into space.", forC},
	{"moving", "align out", "", OrderAlign, "the whole line breaks off to one celestial.", forD},
	{"moving", "hunt here", "", OrderHuntHere, "the hunt moves to your system.", forD},

	// Mining.
	{"mining", "mine", "", OrderMine, "we get a mining fit if we need one, come to your belt and work it.", forC},
	{"mining", "mine veldspar", "mine [ore]", OrderMine, "that ore only.", forCD},
	{"mining", "switch to scordite", "switch to [ore]", OrderPickOre, "change ore without moving.", forC},
	{"mining", "jetcan", "", OrderJetcan, "we can it rather than fill up and leave.", forC},
	{"mining", "haul", "", OrderHaul, "one of us runs the cans to the drop.", forC},
	{"mining", "stockpile here", "", OrderStockpile, "your station becomes the drop; the ore goes into your hangar.", forC},
	{"mining", "sell", "", OrderSell, "we sell it and the ISK comes to you.", forC},
	{"mining", "mine here", "", OrderMine, "the op moves to your system's belts.", forD},

	// Housekeeping.
	{"housekeeping", "status", "", OrderStatus, "what each of us is doing.", forAll},
	{"housekeeping", "roll call", "", OrderList, "who's flying.", forAll},
	{"housekeeping", "help moving", "help [topic]", OrderHelp,
		"one section in chat: command, fighting, moving, mining, housekeeping. \"help\" alone mails this again.", forAll},
	{"housekeeping", "wait", "stay / resume", OrderStay, "hold where you are, or carry on.", forC},
	{"housekeeping", "resume", "", OrderResume, "carry on.", forC},
	{"housekeeping", "dismissed", "", OrderDismiss, "we go back to our own business.", forC},
	{"housekeeping", "go home", "", OrderGoHome, "that pilot heads home.", forC},
	{"housekeeping", "stand down", "", OrderResume,
		"lifts a hold; with none standing, the op winds down and settles.", forD},
	{"housekeeping", "go home", "", OrderGoHome, "the op winds down and settles.", forD},
}

func (e manualEntry) shown() string {
	if e.Show != "" {
		return e.Show
	}
	return e.Say
}

func (e manualEntry) isFor(k manualKind) bool {
	for _, f := range e.For {
		if f == k {
			return true
		}
	}
	return false
}

// manualSubject is each kind's mail subject.
func manualSubject(k manualKind) string {
	switch k {
	case manualDirector:
		return mailSubject("Fleet orders: taking command of the corp fleet")
	case manualRider:
		return mailSubject("Fleet seat: how this fleet works")
	}
	return mailSubject("Fleet orders: commanding bots in your fleet")
}

// manualMail is the mail body, in EVE-mail HTML (<br> and <b> work, CSS does not).
func manualMail(k manualKind, botName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s here.<br><br>", botName)
	switch k {
	case manualConscript:
		b.WriteString("I'll fly with you and take orders in fleet chat. Just type, no slash: the client ")
		b.WriteString("eats those before they reach me. Say it plain and the whole crew acts; put a name ")
		b.WriteString("first and only that pilot does. You can also say <b>all</b>, <b>miners</b>, ")
		b.WriteString("<b>haulers</b>, <b>guns</b>, <b>tackle</b>, <b>logi</b> or <b>ewar</b>.<br><br>")
		b.WriteString("Your fleet window works too: <b>Warp Gang</b> and <b>Regroup</b> bring us along, and ")
		b.WriteString("a target you <b>tag</b> 1 in your overview is the primary (2 the secondary). ")
		b.WriteString("Whoever leads the fleet gives the orders.<br><br>")
	case manualDirector:
		b.WriteString("You hold a chair in this corporation, so this fleet takes your orders in fleet ")
		b.WriteString("chat. From the chair you steer the op: where it works and when it goes home. ")
		b.WriteString("Say <b>take command</b> and you lead it outright: the fleet window's ")
		b.WriteString("<b>Warp Gang</b>, <b>Regroup</b> and <b>Kick</b> are yours, a target you ")
		b.WriteString("<b>tag</b> 1 is the primary (2 the secondary), and only your calls count until ")
		b.WriteString("you <b>hand back</b>.<br><br>")
	case manualRider:
		b.WriteString("Welcome aboard. This is a corporation fleet: its commander runs it, and the ")
		b.WriteString("corporation's officers give it orders. You fly with us and share what we take.<br><br>")
	}
	for _, topic := range manualTopics {
		var lines []string
		for _, e := range manualEntries {
			if e.Topic == topic && e.isFor(k) {
				lines = append(lines, fmt.Sprintf("<b>%s</b> - %s", htmlEsc(e.shown()), e.Does))
			}
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "<b>%s</b><br>%s<br><br>", manualTopicTitle[topic], strings.Join(lines, "<br>"))
	}
	if k == manualConscript {
		b.WriteString("If something opens up on you, we don't wait to be told. We won't shoot a clean ")
		b.WriteString("pilot in empire space, though: CONCORD doesn't care whose fleet we're in.<br><br>")
	}
	b.WriteString("Fly safe.")
	return b.String()
}

func htmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// manualChatMax bounds a chat answer: three lines.
const (
	manualChatMax     = 3
	manualChatLineLen = 220
)

// manualChat is one topic as at most three chat lines: the phrases, comma separated.
func manualChat(k manualKind, topic string) []string {
	var phrases []string
	seen := map[string]bool{}
	for _, e := range manualEntries {
		if e.Topic != topic || !e.isFor(k) || seen[e.shown()] {
			continue
		}
		seen[e.shown()] = true
		phrases = append(phrases, e.shown())
	}
	if len(phrases) == 0 {
		return []string{"Nothing for you under " + topic + ". Try: command, fighting, moving, mining, housekeeping."}
	}
	var out []string
	cur := manualTopicTitle[topic] + ": "
	for i, p := range phrases {
		sep := ", "
		if i == 0 {
			sep = ""
		}
		if len(cur)+len(sep)+len(p) > manualChatLineLen && len(out) < manualChatMax-1 {
			out = append(out, cur+",")
			cur, sep = "", ""
		}
		cur += sep + p
	}
	return append(out, cur+".")
}

// sendFCManual mails a manual from a bot to a human: once per player, kind and version, or
// always when onRequest. It reports whether a mail went out. A messageID of 0 with a nil
// error means the mailbox was suppressed: a success, and 0 is not pushed.
func (m *Mod) sendFCManual(ctx context.Context, botCharID, playerCharID int64, k manualKind, onRequest bool) bool {
	if playerCharID == 0 || botCharID == 0 || m.isSynthetic(playerCharID) {
		return false
	}
	if !onRequest {
		var ver int
		err := m.db.QueryRowContext(ctx,
			`SELECT version FROM rolebot_fc_manual_sent WHERE charID = ? AND kind = ?`,
			playerCharID, string(k)).Scan(&ver)
		if err == nil && ver >= fcManualVersion {
			return false
		}
	}
	name, _ := m.charNameCorp(ctx, botCharID)
	if name == "" {
		name = "Your wingman"
	}
	// sentAt is Unix seconds; evemail.created is the client's FILETIME, or util.FmtDate fails
	// and aborts inbox.AddMessages.
	created := win32From(clock.Now())
	subject := manualSubject(k)
	msgID, err := evedb.SendEveMail(ctx, m.db, playerCharID, botCharID, subject, manualMail(k, name), created)
	if err != nil {
		rlogf("rolebots: %s manual to %d: %v", k, playerCharID, err)
		return false
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_fc_manual_sent (charID, kind, version, sentAt, sends)
		VALUES (?, ?, ?, ?, 1)
		ON DUPLICATE KEY UPDATE version = VALUES(version), sentAt = VALUES(sentAt), sends = sends + 1`,
		playerCharID, string(k), fcManualVersion, clock.Unix()); err != nil {
		rlogf("rolebots: %s manual mark for %d: %v", k, playerCharID, err)
	}
	if msgID != 0 {
		if h := modapi.Gangs(); h.PushMail != nil {
			h.PushMail(playerCharID, msgID, botCharID, subject, created)
		}
	}
	return true
}
