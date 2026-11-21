package rolebots

import "testing"

func TestAlterApplied(t *testing.T) {
	s := &schemaSnapshot{
		cols: map[string]bool{"rolebot_profiles.forcharid": true},
		idx:  map[string]bool{"rolebot_log.idx_rblog_ts": true},
	}
	cases := []struct {
		q    string
		want bool
	}{
		{"ALTER TABLE rolebot_profiles ADD COLUMN IF NOT EXISTS forCharID BIGINT NOT NULL DEFAULT 0", true},
		{"ALTER TABLE rolebot_profiles ADD COLUMN IF NOT EXISTS other BIGINT", false},
		{"ALTER TABLE rolebot_profiles DROP COLUMN IF EXISTS oreDisposal", true},
		{"ALTER TABLE rolebot_profiles DROP COLUMN IF EXISTS forCharID", false},
		{"ALTER TABLE rolebot_log ADD KEY IF NOT EXISTS idx_rblog_ts (ts)", true},
		{"ALTER TABLE rolebot_log ADD UNIQUE KEY IF NOT EXISTS uq_x (a, b)", false},
		{"ALTER TABLE rolebot_log DROP INDEX IF EXISTS idx_gone", true},
		// Two clauses: never judged.
		{"ALTER TABLE rolebot_profiles ADD COLUMN IF NOT EXISTS forCharID BIGINT, ADD COLUMN IF NOT EXISTS y INT", false},
		{"ALTER TABLE rolebot_profiles MODIFY forCharID BIGINT", false},
		{"DROP TABLE IF EXISTS rolebot_belt_state", false},
		// A comma inside a type or default is not a second clause.
		{"ALTER TABLE rolebot_profiles ADD COLUMN IF NOT EXISTS forCharID DECIMAL(10,2) DEFAULT 'a,b'", true},
	}
	for _, c := range cases {
		if got := s.alterApplied(c.q); got != c.want {
			t.Errorf("alterApplied(%q) = %v, want %v", c.q, got, c.want)
		}
	}
	var nilSnap *schemaSnapshot
	if nilSnap.alterApplied(cases[0].q) {
		t.Error("nil snapshot must judge nothing")
	}
}

func TestLogPruneBudget(t *testing.T) {
	if got := logPruneBudget(0); got != logPruneFloor {
		t.Errorf("idle budget = %d, want floor %d", got, logPruneFloor)
	}
	if got := logPruneBudget(4000); got != 6000 {
		t.Errorf("budget(4000) = %d, want 6000", got)
	}
	if got := logPruneBudget(1 << 30); got != logPruneMaxPerRoll {
		t.Errorf("busy budget = %d, want cap %d", got, logPruneMaxPerRoll)
	}
}
