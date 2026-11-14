package lsc

import (
	"log"
	"strings"
)

// SpeakBot sends a chat line from a synthetic member, which has no connection for
// handleSendMessage to read. It reuses the player path's builders and per-member
// loop: Local needs the typed channelWire identifier, and each recipient needs its
// own account ID and sequence number. No persistence and no ACL check.
func (r *Registry) SpeakBot(channelID, senderCharID int64, senderName string,
	corpID, typeID int64, message string) int {

	if r == nil || channelID == 0 || senderCharID == 0 || message == "" {
		return 0
	}
	if typeID == 0 {
		typeID = 1374 // the generic character typeID the player path falls back to
	}

	members := r.Members(channelID)
	args := buildSendMessageArgs(channelWire(channelID), len(members),
		senderCharID, CP1252(senderName), corpID, typeID, 3 /*ROLE_LOGIN|ROLE_PLAYER*/, CP1252(message))

	sent := 0
	for _, m := range members {
		if m.Conn == nil {
			continue // another synthetic member: nothing to send to
		}
		notify := buildOnLSCNotification(args, uint64(m.AccountID), r.NextSN(m.Conn))
		if notify == nil {
			continue
		}
		if err := m.Conn.SendPacket(notify); err != nil {
			log.Printf("lsc: SpeakBot channel=%d → %s: %v", channelID, m.Conn.RemoteAddr(), err)
			continue
		}
		sent++
	}
	return sent
}

// HasLiveListener reports whether any member of a channel has a connection, a
// human who would see a line said there. Ask before rendering a line, since most
// channels hold nobody who can read.
func (r *Registry) HasLiveListener(channelID int64) bool {
	if r == nil || channelID == 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.channels[channelID] {
		if m.Conn != nil {
			return true
		}
	}
	return false
}

// CP1252 re-encodes a UTF-8 string as the bytes the client reads. A chat line is a
// PyString of raw bytes decoded as cp1252, so UTF-8 input shows as mojibake.
// ASCII and Latin-1 map as is, 0x80..0x9F symbols to their slot, a few others to
// an ASCII spelling, and anything else to "?".
func CP1252(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteByte(byte(r))
		case r >= 0xA0 && r <= 0xFF:
			b.WriteByte(byte(r))
		default:
			if c, ok := cp1252High[r]; ok {
				b.WriteByte(c)
			} else if alt, ok := cp1252Spelled[r]; ok {
				b.WriteString(alt)
			} else {
				b.WriteByte('?')
			}
		}
	}
	return b.String()
}

// cp1252High is the 0x80..0x9F block: the Unicode code point each byte means.
var cp1252High = map[rune]byte{
	'€': 0x80, '‚': 0x82, 'ƒ': 0x83, '„': 0x84, '…': 0x85, '†': 0x86, '‡': 0x87,
	'ˆ': 0x88, '‰': 0x89, 'Š': 0x8A, '‹': 0x8B, 'Œ': 0x8C, 'Ž': 0x8E,
	'‘': 0x91, '’': 0x92, '“': 0x93, '”': 0x94, '•': 0x95, '–': 0x96, '—': 0x97,
	'˜': 0x98, '™': 0x99, 'š': 0x9A, '›': 0x9B, 'œ': 0x9C, 'ž': 0x9E, 'Ÿ': 0x9F,
}

// cp1252Spelled are symbols cp1252 has no byte for, written out.
var cp1252Spelled = map[rune]string{
	'→': "->", '←': "<-", '↔': "<->", '≥': ">=", '≤': "<=", '≠': "!=", '≈': "~",
	'\u2212': "-", '\u2010': "-", '\u2011': "-", '\u2032': "'", '\u2033': "\"", '\u2009': " ", '\u202f': " ",
}
