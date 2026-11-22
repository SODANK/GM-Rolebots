package rolebots

import (
	"testing"
	"time"
)

// An engagement opener is urgent but rationed: one per speaker per opener gap, while
// replies and other urgent lines still skip every per-speaker floor.
func TestLocalOpenerGap(t *testing.T) {
	prevGap, prevSpeaker := chatterOpenerGapSec.Load(), chatterGapSec.Load()
	t.Cleanup(func() {
		chatterOpenerGapSec.Store(prevGap)
		chatterGapSec.Store(prevSpeaker)
	})
	setChatterOpenerGap(240)
	setChatterGap(30)

	const ch, camper, other = 30005304, 1, 2
	t0 := time.Unix(1_800_000_000, 0)
	l := newLocalLedger()
	steps := []struct {
		name           string
		at             time.Duration
		speaker        int64
		urgent, opener bool
		want           string
	}{
		{"first opener", 0, camper, true, true, ""},
		{"second opener 60s later", 60 * time.Second, camper, true, true, "opener"},
		{"urgent reply from the camper", 61 * time.Second, camper, true, false, ""},
		{"another pilot's opener", 62 * time.Second, other, true, true, ""},
		{"opener after the gap", 241 * time.Second, camper, true, true, ""},
	}
	for _, s := range steps {
		if got := l.allowAt(t0.Add(s.at), ch, s.speaker, 100, s.urgent, s.opener); got != s.want {
			t.Errorf("%s: allow = %q, want %q", s.name, got, s.want)
		}
	}

	// Gap 0 turns the floor off.
	setChatterOpenerGap(0)
	l = newLocalLedger()
	for i := 0; i < 3; i++ {
		if got := l.allowAt(t0.Add(time.Duration(i)*time.Second), ch, camper, 100, true, true); got != "" {
			t.Fatalf("gap 0: opener %d refused (%q)", i, got)
		}
	}

	// The channel ceiling still refuses an opener that has never spoken.
	setChatterOpenerGap(240)
	l = newLocalLedger()
	l.allowAt(t0, ch, other, 1, true, false)
	if got := l.allowAt(t0.Add(time.Second), ch, camper, 1, true, true); got != "local" {
		t.Fatalf("opener over the ceiling = %q, want %q", got, "local")
	}
}
