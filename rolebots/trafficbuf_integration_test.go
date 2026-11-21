package rolebots

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// TestIntegrationTrafficFlush runs against the scratch database in GURIMOON_TEST_DSN. Two
// flushes must add up on the same rows, the way the per-event upserts did.
func TestIntegrationTrafficFlush(t *testing.T) {
	dsn := os.Getenv("GURIMOON_TEST_DSN")
	if dsn == "" {
		t.Skip("GURIMOON_TEST_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tables := []string{"rolebot_traffic_systems", "rolebot_traffic_lanes", "rolebot_traffic_stations", "mapHistory"}
	for _, tb := range tables {
		db.Exec(`DROP TABLE IF EXISTS ` + tb)
	}
	defer func() {
		for _, tb := range tables {
			db.Exec(`DROP TABLE IF EXISTS ` + tb)
		}
	}()
	for _, q := range []string{
		`CREATE TABLE rolebot_traffic_systems (systemID BIGINT NOT NULL, jumps DOUBLE NOT NULL DEFAULT 0,
			docks DOUBLE NOT NULL DEFAULT 0, buys DOUBLE NOT NULL DEFAULT 0, sells DOUBLE NOT NULL DEFAULT 0,
			iskMoved DOUBLE NOT NULL DEFAULT 0, decayedAt BIGINT NOT NULL DEFAULT 0, lastAt BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (systemID))`,
		`CREATE TABLE rolebot_traffic_stations (stationID BIGINT NOT NULL, docks DOUBLE NOT NULL DEFAULT 0,
			decayedAt BIGINT NOT NULL DEFAULT 0, lastAt BIGINT NOT NULL DEFAULT 0, PRIMARY KEY (stationID))`,
		`CREATE TABLE rolebot_traffic_lanes (fromSystemID BIGINT NOT NULL, toSystemID BIGINT NOT NULL,
			jumps DOUBLE NOT NULL DEFAULT 0, hauls DOUBLE NOT NULL DEFAULT 0, iskMoved DOUBLE NOT NULL DEFAULT 0,
			decayedAt BIGINT NOT NULL DEFAULT 0, lastAt BIGINT NOT NULL DEFAULT 0, PRIMARY KEY (fromSystemID, toSystemID))`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	m := &Mod{db: db}
	takeTraffic()
	for round := 0; round < 2; round++ {
		m.recordJump(ctx, 30000001, 30000002)
		m.recordJump(ctx, 30000002, 30000001)
		m.recordMarketFlow(ctx, 30000002, true, 100)
		m.recordHaul(ctx, 30000001, 30000002, 50)
		m.recordStationDock(ctx, 60000001)
		bufMapJump(30000002)
		m.flushTraffic(ctx)
	}

	var jumps, buys, isk float64
	if err := db.QueryRow(`SELECT jumps, buys, iskMoved FROM rolebot_traffic_systems WHERE systemID = 30000002`).
		Scan(&jumps, &buys, &isk); err != nil || jumps != 2 || buys != 2 || isk != 200 {
		t.Fatalf("system row: jumps=%v buys=%v isk=%v err=%v", jumps, buys, isk, err)
	}
	var lj, hauls float64
	if err := db.QueryRow(`SELECT jumps, hauls FROM rolebot_traffic_lanes WHERE fromSystemID = 30000001 AND toSystemID = 30000002`).
		Scan(&lj, &hauls); err != nil || lj != 4 || hauls != 2 {
		t.Fatalf("lane row: jumps=%v hauls=%v err=%v", lj, hauls, err)
	}
	var docks float64
	if err := db.QueryRow(`SELECT docks FROM rolebot_traffic_stations WHERE stationID = 60000001`).
		Scan(&docks); err != nil || docks != 2 {
		t.Fatalf("dock row: %v err=%v", docks, err)
	}
	var mh int
	if err := db.QueryRow(`SELECT SUM(count) FROM mapHistory WHERE solarSystemID = 30000002 AND statType = 4`).
		Scan(&mh); err != nil || mh != 2 {
		t.Fatalf("mapHistory: %v err=%v", mh, err)
	}
}
