package rolebots

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"gurimoon/server/lsc"
)

// What a conscript says, and how much of it.
//
// Chat is the channel the humans use, so every line goes through a budget with three limits:
//
//   - one voice per fleet for anything said on behalf of the crew, so a group order gets one
//     answer from the fleet's speaker;
//   - a per-pilot cooldown for anything a pilot says about itself;
//   - a fleet-wide ceiling per minute, which nothing bypasses.
//
// Refusals and ambiguity questions skip the cooldown but count against the ceiling.

const (
	settingConscriptVoice       = "rolebots.conscript.voice"
	settingConscriptVoiceGap    = "rolebots.conscript.voiceGapSec"
	settingConscriptVoicePerMin = "rolebots.conscript.voicePerMin"

	defaultVoiceGapSec = 45
	defaultVoicePerMin = 6
	voiceRepeatWindow  = 15 * time.Minute
	voiceCeilingWindow = time.Minute
)

var (
	conscriptVoiceOn   atomic.Bool
	conscriptVoiceGap  atomic.Int64
	conscriptVoiceRate atomic.Int64
)

func setConscriptVoice(on bool) { conscriptVoiceOn.Store(on) }

func setConscriptVoiceGap(n int) {
	if n < 0 {
		n = defaultVoiceGapSec
	}
	conscriptVoiceGap.Store(int64(n))
}

func setConscriptVoiceRate(n int) {
	if n < 1 {
		n = defaultVoicePerMin
	}
	conscriptVoiceRate.Store(int64(n))
}

// ── the budget ──────────────────────────────────────────────────────────────

type voiceLedger struct {
	mu       sync.Mutex
	lastSaid map[int64]time.Time  // per pilot
	saidWhat map[string]time.Time // per pilot+line, for the repeat fold
	fleetAt  map[int64][]time.Time
}

var voices = &voiceLedger{
	lastSaid: map[int64]time.Time{},
	saidWhat: map[string]time.Time{},
	fleetAt:  map[int64][]time.Time{},
}

// allow decides whether one line may be said. urgent skips the per-pilot cooldown and the
// repeat fold but never the fleet ceiling.
func (v *voiceLedger) allow(gangID, charID int64, line string, urgent bool) bool {
	now := clock.Now()
	v.mu.Lock()
	defer v.mu.Unlock()

	// Fleet ceiling first: it protects the player's window and nothing is exempt from it.
	kept := v.fleetAt[gangID][:0]
	for _, t := range v.fleetAt[gangID] {
		if now.Sub(t) < voiceCeilingWindow {
			kept = append(kept, t)
		}
	}
	v.fleetAt[gangID] = kept
	if int64(len(kept)) >= conscriptVoiceRate.Load() {
		return false
	}

	if !urgent {
		if gap := conscriptVoiceGap.Load(); gap > 0 {
			if last, ok := v.lastSaid[charID]; ok && now.Sub(last) < time.Duration(gap)*time.Second {
				return false
			}
		}
		key := fmt.Sprintf("%d|%s", charID, line)
		if at, ok := v.saidWhat[key]; ok && now.Sub(at) < voiceRepeatWindow {
			return false
		}
		v.saidWhat[key] = now
	}
	v.lastSaid[charID] = now
	v.fleetAt[gangID] = append(v.fleetAt[gangID], now)
	return true
}

// sweepVoiceLedger drops entries nothing will read again. Called from the conscript lane;
// otherwise saidWhat grows one key per distinct line per pilot.
func sweepVoiceLedger() {
	now := clock.Now()
	voices.mu.Lock()
	defer voices.mu.Unlock()
	for k, t := range voices.saidWhat {
		if now.Sub(t) > voiceRepeatWindow {
			delete(voices.saidWhat, k)
		}
	}
	for id, t := range voices.lastSaid {
		if now.Sub(t) > voiceRepeatWindow {
			delete(voices.lastSaid, id)
		}
	}
	for id, ts := range voices.fleetAt {
		kept := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < voiceCeilingWindow {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(voices.fleetAt, id)
			continue
		}
		voices.fleetAt[id] = kept
	}
}

// ── saying it ───────────────────────────────────────────────────────────────

// sayFleet puts one line in a fleet's chat as this pilot, subject to the budget. It reports
// whether the line was said, so callers do not record "I told them" for an unheard line.
func (m *Mod) sayFleet(ctx context.Context, gangID, charID int64, urgent bool, format string, args ...any) bool {
	if !conscriptVoiceOn.Load() {
		return false
	}
	return m.speakFleet(ctx, gangID, charID, urgent, fmt.Sprintf(format, args...))
}

// speakFleet is the line itself, under the budget but outside the conscription switch: a fleet
// the module opened for its own members (fleetmirror.go) narrates through it.
func (m *Mod) speakFleet(ctx context.Context, gangID, charID int64, urgent bool, line string) bool {
	if m.lsc == nil || gangID == 0 || charID == 0 || line == "" {
		return false
	}
	if !voices.allow(gangID, charID, line, urgent) {
		return false
	}
	chID := lsc.GangChannelID(gangID)
	if chID == 0 {
		return false
	}
	name, corpID := m.charNameCorp(ctx, charID)
	m.lsc.SpeakBot(chID, charID, name, corpID, m.charTypeID(ctx, charID), line)
	return true
}

// speakerFor picks the one pilot that answers for the crew: the lowest character ID, which
// is stable while the crew is, so one voice answers every order.
func speakerFor(c *conscriptCrew) int64 {
	if c == nil || len(c.Members) == 0 {
		return 0
	}
	best := c.Members[0].CharID
	for _, k := range c.Members[1:] {
		if k.CharID < best {
			best = k.CharID
		}
	}
	return best
}

// sayCrew is the fleet's single answer to a group order.
func (m *Mod) sayCrew(ctx context.Context, gangID int64, urgent bool, format string, args ...any) {
	sp := speakerFor(crewOf(gangID))
	if sp == 0 {
		return
	}
	m.sayFleet(ctx, gangID, sp, urgent, format, args...)
}

// ── line banks ──────────────────────────────────────────────────────────────
// Lines are drawn at random, not round-robin, so two pilots given the same order do not chorus
// (pickLine, piracy_voice.go).

var ackLines = []string{
	"On it.", "Aye.", "Copy that.", "Understood.", "Moving.", "Roger.",
	"Wilco.", "Got it.",
}

var confusedLines = []string{
	"Say again?", "Didn't catch that one.",
}

// orderAck is what the crew says back. It is short: an acknowledgement is a receipt.
func orderAck() string { return pickLine(ackLines) }

// ── the briefing ────────────────────────────────────────────────────────────

// sendBriefing mails the conscript manual (fcmanual.go) to a player: once per manual version
// unless they ask again. The suppression gate is recipient-only (db/killmaildb.go), so a bot
// writing to a human always lands. rolebot_conscript_briefed keeps counting sends, a sign the
// command list is hard to find.
func (m *Mod) sendBriefing(ctx context.Context, botCharID, playerCharID int64, onRequest bool) {
	if !m.sendFCManual(ctx, botCharID, playerCharID, manualConscript, onRequest) {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_conscript_briefed (charID, sentAt, sends)
		VALUES (?, ?, 1)
		ON DUPLICATE KEY UPDATE sentAt = VALUES(sentAt), sends = sends + 1`,
		playerCharID, clock.Unix()); err != nil {
		rlogf("rolebots: briefing mark for %d: %v", playerCharID, err)
	}
}
