package rolebots

import "testing"

func TestVictimHadEngaged(t *testing.T) {
	engagedMu.Lock()
	engaged = map[int64]map[int64]int64{}
	engagedMu.Unlock()

	const (
		pirate, pirateCorp = 9001, 98000001
		mark, markCorp     = 9002, 98000002
		markMate           = 9003
		now                = int64(1_000_000)
	)
	// The pirate opens on the mark.
	noteEngagementAt(pirate, mark, markCorp, now)

	// The mark answers: lawful.
	if !victimHadEngagedAt(pirate, mark, markCorp, now+5) {
		t.Fatal("mark answering the pirate should be lawful")
	}
	// A corpmate of the mark answers: lawful, through the corp key, as in the core.
	if !victimHadEngagedAt(pirate, markMate, markCorp, now+5) {
		t.Fatal("mark's corpmate answering should be lawful")
	}
	// The pirate shooting the mark again is not excused by its own act.
	if victimHadEngagedAt(mark, pirate, pirateCorp, now+5) {
		t.Fatal("aggressor must not be excused by its own aggression")
	}
	// After the window the entry is gone.
	if victimHadEngagedAt(pirate, mark, markCorp, now+engagementWindowSec) {
		t.Fatal("engagement should lapse after the crimewatch window")
	}
	// Self and zero IDs never count.
	noteEngagementAt(mark, mark, 0, now)
	if victimHadEngagedAt(mark, mark, 0, now) || victimHadEngagedAt(0, mark, 0, now) {
		t.Fatal("self or zero IDs must not engage")
	}
}
