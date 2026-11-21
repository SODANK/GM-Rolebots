package rolebots

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// TestIntegrationLogPrunePaced runs against the scratch database in GURIMOON_TEST_DSN.
// A large expired backlog must drain at the paced rate, not all at once.
func TestIntegrationLogPrunePaced(t *testing.T) {
	dsn := os.Getenv("GURIMOON_TEST_DSN")
	if dsn == "" {
		t.Skip("GURIMOON_TEST_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec(`DROP TABLE IF EXISTS rolebot_log`)
	if _, err := db.Exec(`CREATE TABLE rolebot_log (
		logID BIGINT NOT NULL AUTO_INCREMENT, ts BIGINT NOT NULL, characterID BIGINT NOT NULL DEFAULT 0,
		level VARCHAR(8) NOT NULL DEFAULT 'info', category VARCHAR(24) NOT NULL DEFAULT '', message TEXT NULL,
		PRIMARY KEY (logID), KEY idx_rblog_ts (ts))`); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP TABLE IF EXISTS rolebot_log`)
	// 10,000 rows from 30 days ago: all past the 7-day window.
	old := time.Now().AddDate(0, 0, -30).Unix()
	if _, err := db.Exec(`INSERT INTO rolebot_log (ts, message)
		SELECT ?, 'x' FROM seq_1_to_10000`, old); err != nil {
		t.Skipf("needs MariaDB's sequence engine: %v", err)
	}
	count := func() (n int) {
		db.QueryRow(`SELECT COUNT(*) FROM rolebot_log`).Scan(&n)
		return
	}
	m := &Mod{db: db}
	logPruneLastRoll, logPruneBackoff, logInsertedSincePrune = time.Time{}, time.Time{}, 0

	m.maybePruneLog()
	if got := count(); got != 10000-logPruneFloor {
		t.Fatalf("after one idle roll %d rows left, want %d", got, 10000-logPruneFloor)
	}
	// Flushes inside the interval do not prune again.
	for i := 0; i < 20; i++ {
		m.maybePruneLog()
	}
	if got := count(); got != 10000-logPruneFloor {
		t.Fatalf("rolls inside logPruneInterval deleted rows: %d left", got)
	}
	// A busy writer earns a bigger budget on its next roll.
	logPruneLastRoll = time.Now().Add(-logPruneInterval)
	logInsertedSincePrune = 3000
	m.maybePruneLog()
	if got := count(); got != 10000-logPruneFloor-4500 {
		t.Fatalf("after a busy roll %d rows left, want %d", got, 10000-logPruneFloor-4500)
	}
}

func TestIntegrationSchemaSnapshot(t *testing.T) {
	dsn := os.Getenv("GURIMOON_TEST_DSN")
	if dsn == "" {
		t.Skip("GURIMOON_TEST_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec(`DROP TABLE IF EXISTS rolebot_snap_t`)
	db.Exec(`CREATE TABLE rolebot_snap_t (a INT, b INT, KEY idx_b (b))`)
	defer db.Exec(`DROP TABLE IF EXISTS rolebot_snap_t`)
	snap := loadSchemaSnapshot(context.Background(), db)
	if snap == nil {
		t.Fatal("snapshot failed")
	}
	for q, want := range map[string]bool{
		"ALTER TABLE rolebot_snap_t ADD COLUMN IF NOT EXISTS a INT":  true,
		"ALTER TABLE rolebot_snap_t ADD COLUMN IF NOT EXISTS c INT":  false,
		"ALTER TABLE rolebot_snap_t DROP COLUMN IF EXISTS zz":        true,
		"ALTER TABLE rolebot_snap_t ADD KEY IF NOT EXISTS idx_b (b)": true,
		"ALTER TABLE rolebot_snap_t DROP INDEX IF EXISTS idx_b":      false,
	} {
		if got := snap.alterApplied(q); got != want {
			t.Errorf("alterApplied(%q) = %v, want %v", q, got, want)
		}
	}
}
