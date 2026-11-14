package rolebots

import (
	"fmt"
	"sync"
	"time"
)

// The chatter budget (chatter.go is the system; this file is only the arithmetic that keeps
// it quiet). Every limit is a window a human reads chat through. The checks run in the order
// of the operator's dials on /rolebots/chatter, so a drop counter means what its label says:
// a line refused for "hour" was refused by the hourly ceiling and nothing before it.
// Pure: no Mod, no DB, clock passed in. voiceLedger (conscript_voice.go) is the same shape
// for fleet chat; this one is channel-keyed.

// chatterClass is what the budget treats differently. Events are the bots' own narration and
// take the brunt of every limit; greetings and replies answer a person and skip the
// per-pilot gap but never the channel ceiling.
type chatterClass uint8

const (
	chatterClassEvent chatterClass = iota
	chatterClassGreet
	chatterClassReply
	// chatterClassFollow is the next beat of an exchange under way (chatter_voice.go). It skips
	// both gaps, but not the ceiling or the greeting reserve.
	chatterClassFollow
)

// chatterLimits is one evaluation's dials.
type chatterLimits struct {
	PerHour      int           // channel ceiling, sliding hour
	GreetReserve int           // lines of the hour events may not spend
	MinGap       time.Duration // floor between any two lines in a channel
	PilotGap     time.Duration // one pilot's floor between its own event lines
	KindCooldown time.Duration // per (channel, kind)
	RepeatWindow time.Duration // per (channel, template)
}

type chatterLedger struct {
	mu        sync.Mutex
	saidAt    map[int64][]time.Time // per channel: the sliding hour
	lastLine  map[int64]time.Time   // per channel: minGap
	lastPilot map[int64]time.Time   // per pilot: pilotGap
	kindAt    map[string]time.Time  // "ch|kind"
	tmplAt    map[string]time.Time  // "ch|tmplID"
	greeted   map[string]time.Time  // "ch|humanID"
}

func newChatterLedger() *chatterLedger {
	return &chatterLedger{
		saidAt:    map[int64][]time.Time{},
		lastLine:  map[int64]time.Time{},
		lastPilot: map[int64]time.Time{},
		kindAt:    map[string]time.Time{},
		tmplAt:    map[string]time.Time{},
		greeted:   map[string]time.Time{},
	}
}

// chatterBook is the live ledger. Swept on its own TTLs (sweep), so it is
// exempt from the forget lint like voices.
var chatterBook = newChatterLedger()

func chatterKey(ch int64, s string) string { return fmt.Sprintf("%d|%s", ch, s) }

// allow answers "" when the line may go, otherwise the first limit it fails:
// "hour", "mingap", "pilotgap", "kind" or "repeat". On "" every stamp is
// written, so the caller must then say the line or accept that the budget
// counted one it did not.
func (l *chatterLedger) allow(now time.Time, ch, pilot int64, kind, tmplID string, class chatterClass, lim chatterLimits) string {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 1. The channel ceiling. Nothing is exempt; events may not spend the reserve, so a person
	// logging in late in a busy hour is still greeted.
	kept := l.saidAt[ch][:0]
	for _, t := range l.saidAt[ch] {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	l.saidAt[ch] = kept
	ceiling := lim.PerHour
	if class == chatterClassEvent || class == chatterClassFollow {
		ceiling -= lim.GreetReserve
	}
	if len(kept) >= ceiling {
		return "hour"
	}

	// 2. The floor between lines. A reply to a person may come sooner than a narration, since
	// a long silence after a question reads as broken.
	gap := lim.MinGap
	switch class {
	case chatterClassReply:
		gap /= 3
	case chatterClassFollow:
		gap = 0
	}
	if last, ok := l.lastLine[ch]; ok && now.Sub(last) < gap {
		return "mingap"
	}

	// 3. One pilot's own pace, events only.
	if class == chatterClassEvent && lim.PilotGap > 0 {
		if last, ok := l.lastPilot[pilot]; ok && now.Sub(last) < lim.PilotGap {
			return "pilotgap"
		}
	}

	// 4. The kind's cooldown: the channel has heard this sort of thing lately.
	if lim.KindCooldown > 0 && kind != "" {
		if at, ok := l.kindAt[chatterKey(ch, kind)]; ok && now.Sub(at) < lim.KindCooldown {
			return "kind"
		}
	}

	// 5. The same wording twice in a day.
	if lim.RepeatWindow > 0 && tmplID != "" {
		if at, ok := l.tmplAt[chatterKey(ch, tmplID)]; ok && now.Sub(at) < lim.RepeatWindow {
			return "repeat"
		}
	}

	l.saidAt[ch] = append(l.saidAt[ch], now)
	l.lastLine[ch] = now
	if pilot != 0 {
		l.lastPilot[pilot] = now
	}
	if kind != "" {
		l.kindAt[chatterKey(ch, kind)] = now
	}
	if tmplID != "" {
		l.tmplAt[chatterKey(ch, tmplID)] = now
	}
	return ""
}

// heard reports whether a channel heard this template inside the window, without stamping
// anything. The exchange picker uses it to avoid a wording allow would refuse as a repeat.
func (l *chatterLedger) heard(now time.Time, ch int64, tmplID string, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.tmplAt[chatterKey(ch, tmplID)]
	return ok && now.Sub(at) < window
}

// stampForced records a line said past the budget (the panel's test line)
// so the next real line keeps its distance from it.
func (l *chatterLedger) stampForced(now time.Time, ch int64) {
	l.mu.Lock()
	l.saidAt[ch] = append(l.saidAt[ch], now)
	l.lastLine[ch] = now
	l.mu.Unlock()
}

// greetDue reports whether this person may be greeted in this channel, and
// stamps them when it says yes.
func (l *chatterLedger) greetDue(now time.Time, ch, human int64, every time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := chatterKey(ch, fmt.Sprint(human))
	if at, ok := l.greeted[k]; ok && now.Sub(at) < every {
		return false
	}
	l.greeted[k] = now
	return true
}

// greetedWithin reports whether this person was greeted in this channel inside
// the window, without stamping.
func (l *chatterLedger) greetedWithin(now time.Time, ch, human int64, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.greeted[chatterKey(ch, fmt.Sprint(human))]
	return ok && now.Sub(at) < window
}

// saidLastHour is the panel's number.
func (l *chatterLedger) saidLastHour(ch int64, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, t := range l.saidAt[ch] {
		if now.Sub(t) < time.Hour {
			n++
		}
	}
	return n
}

// sweep drops what nothing will read again.
func (l *chatterLedger) sweep(now time.Time, lim chatterLimits) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ch, ts := range l.saidAt {
		kept := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < time.Hour {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(l.saidAt, ch)
			continue
		}
		l.saidAt[ch] = kept
	}
	keep := lim.PilotGap
	if keep < time.Hour {
		keep = time.Hour
	}
	for k, t := range l.lastLine {
		if now.Sub(t) > keep {
			delete(l.lastLine, k)
		}
	}
	for k, t := range l.lastPilot {
		if now.Sub(t) > keep {
			delete(l.lastPilot, k)
		}
	}
	for k, t := range l.kindAt {
		if now.Sub(t) > 6*time.Hour {
			delete(l.kindAt, k)
		}
	}
	rw := lim.RepeatWindow
	if rw <= 0 {
		rw = 24 * time.Hour
	}
	for k, t := range l.tmplAt {
		if now.Sub(t) > rw {
			delete(l.tmplAt, k)
		}
	}
	for k, t := range l.greeted {
		if now.Sub(t) > chatterGreetOncePer {
			delete(l.greeted, k)
		}
	}
}
