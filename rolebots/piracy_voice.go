package rolebots

import (
	"context"
	"fmt"
	"math/rand"
	"time"
)

// Pirate speech: the demand goes in Local, with a price and the victim's name as a link.
//
//   - A name link is `<url=showinfo:<characterTypeID>//<charID>>Name</url>`, the form
//     the client sends for "Show Info". The HTML `<a href>` form is not rendered by chat.
//     The typeID must be a character bloodline type (1373-1386): a ship type or 0 raises
//     KeyError in the client's GetBloodlineByTypeID and kills the clicker's tasklet.
//   - The channel is the solar system ID (Local), sent in the typed form by SpeakBot's
//     channelWire.
//   - A bot only reaches channels it has been JoinBot'd into (syncPresence does Local
//     and its corporation).

// The pirate's lines live in chatter_lines_pirate.go and are drawn through the chatter
// pools by pirateSay. A ransom demand skips the chatter budget and its off switch,
// because it is the only way the victim learns the price.

// charLink renders a character as a chat link the client opens a Show Info window for.
// typeID must be a character bloodline type; anything else (a 0, a hull) is replaced,
// because the client raises an exception for whoever clicks it.
func charLink(charID, typeID int64, name string) string {
	if name == "" {
		name = fmt.Sprintf("pilot %d", charID)
	}
	if charID == 0 {
		return name
	}
	if typeID < 1373 || typeID > 1386 {
		typeID = 1374
	}
	return fmt.Sprintf(`<url=showinfo:%d//%d>%s</url>`, typeID, charID, name)
}

// typeLink renders an item type as a chat link: the same tag with no "//itemID", the
// form the client writes for a type dragged in from the market. A zero type is plain
// text.
func typeLink(typeID int64, name string) string {
	if name == "" {
		name = fmt.Sprintf("type %d", typeID)
	}
	if typeID <= 0 {
		return name
	}
	return fmt.Sprintf(`<url=showinfo:%d>%s</url>`, typeID, name)
}

// iskWords formats a price the way a person says it: "400k" and "1.2m", not "400000.00".
func iskWords(isk float64) string {
	switch {
	case isk >= 1_000_000_000:
		return fmt.Sprintf("%.1fb ISK", isk/1_000_000_000)
	case isk >= 1_000_000:
		return fmt.Sprintf("%.1fm ISK", isk/1_000_000)
	case isk >= 1_000:
		return fmt.Sprintf("%.0fk ISK", isk/1_000)
	}
	return fmt.Sprintf("%.0f ISK", isk)
}

// sayLocal puts one line in a system's Local channel as this bot. It does nothing on a
// core with no SpeakBot or without the chat registry; the pirate still robs, quietly.
func (m *Mod) sayLocal(charID, corpID, sysID int64, charName, text string) {
	if m.lsc == nil || sysID == 0 || text == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.lsc.SpeakBot(sysID, charID, charName, corpID, m.charTypeID(ctx, charID), text)
}

// charTypeID returns a character's own bloodline type: the chat speaker icon and the
// type half of a name link. It falls back to a valid character type, never 0, because
// every consumer treats the number as a bloodline and raises on anything else.
func (m *Mod) charTypeID(ctx context.Context, charID int64) int64 {
	var typeID int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ?`, charID).Scan(&typeID)
	if typeID < 1373 || typeID > 1386 {
		return 1374
	}
	return typeID
}

// pirateSay puts one of a pirate's lines in Local. target is the victim's name, already
// a link (or "" for the camp line); isk fills the demand. A line said to a pilot opens
// the window in which their reply is heard (chatter_retort.go).
func (m *Mod) pirateSay(eb *embodiedBot, ev chatEvent, victimCharID int64, target string, isk float64) {
	if eb == nil {
		return
	}
	self := partyFromEmbodied(eb)
	s := chatSlots{"self": eb.charName}
	if target != "" {
		s["target"] = target
	}
	if isk > 0 {
		s["isk"] = iskWords(isk)
	}
	line, tpl := pickChatLineFor(eb.systemID, victimCharID, ev, motivePiracy, self.Voice, s)
	if line == "" {
		return
	}
	localBook.noteSaid(eb.systemID, tpl)
	localBook.noteHeardBy(victimCharID, tpl)
	m.sayLocal(eb.charID, eb.corpID, eb.systemID, eb.charName, line)
	if victimCharID != 0 {
		victim := partyFromID(victimCharID, "", 0, 0, false)
		extra := chatSlots{}
		if isk > 0 {
			extra["isk"] = iskWords(isk)
		}
		noteBark(eb.systemID, self, victim, motivePiracy, extra)
	}
}

// pirateSayRun is the camp line from the abstract tier, which has no
// projection to speak through.
func (m *Mod) pirateSayRun(b *botRun, ev chatEvent) {
	if b == nil {
		return
	}
	self := partyFromRun(b, true)
	line, tpl := pickChatLine(b.SystemID, ev, motivePiracy, self.Voice, chatSlots{"self": b.Name})
	if line == "" {
		return
	}
	localBook.noteSaid(b.SystemID, tpl)
	m.sayLocal(b.CharID, b.CorpID, b.SystemID, b.Name, line)
}

// pickLine draws one line from a set with a fresh roll, not round-robin, so two pilots
// do not chorus.
func pickLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[rand.Intn(len(lines))]
}
