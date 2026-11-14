package rolebots

// The unmet-demand ledger's arithmetic: decay, the write throttle, and the columns this file
// adds to rolebot_market_demand.
// `wanted` is a level, not a running total, through two rules. Throttle: one note per
// (pilot, type) per day, as noteHullDemand does for hulls. Decay: `wanted` halves every
// demandHalfLifeSec, folded forward to now on every write and decayed on every read through
// demandWantedSQL.
// The decay anchor is decayAt (when `wanted` was last folded forward), not lastAt (when someone
// last asked). Rows from before decayAt existed carry 0, so the anchor is GREATEST(decayAt,
// lastAt). lastRegionID is where the last ask was made (corpproduction.go ledgerDemand);
// 0 means unknown.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"
)

const (
	// demandHalfLifeSec is the ledger's half-life: a want nobody repeats is
	// worth half as much a week later.
	demandHalfLifeSec = 7 * 86400

	// demandNoteEverySec is the per-(pilot, type) write throttle.
	demandNoteEverySec = 24 * 3600

	// demandThrottlePruneAt is the map size at which expired throttle entries
	// are swept out.
	demandThrottlePruneAt = 50_000

	// demandChargeMaxRuns caps a charge's want at this many production runs:
	// a million rounds of 100-portion ammo, ten thousand crystals. See
	// demandCapUnits.
	demandChargeMaxRuns = 10_000
)

// demandCapUnits is the ceiling on one ledger row's want, 0 for none. Only charges are
// capped: a charge's qty is a pilot's whole ammunition load, so the level can outgrow an int
// scan (demandWantedSQL) and exceed what any builder could act on. Pure.
func demandCapUnits(categoryID, portion int64) int64 {
	if categoryID != catCharge {
		return 0
	}
	return demandChargeMaxRuns * max(portion, 1)
}

// demandCaps is demandCapUnits per type for the life of the process: the
// inputs are static rows. A failed read is not cached (see statics.go).
var demandCaps sync.Map // typeID to int64

// demandCap is the ceiling on typeID's want, 0 for none or unknown.
func (m *Mod) demandCap(ctx context.Context, typeID int64) int64 {
	if v, ok := demandCaps.Load(typeID); ok {
		return v.(int64)
	}
	var cat, portion int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT g.categoryID, COALESCE(t.portionSize, 1)
		  FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID
		 WHERE t.typeID = ?`, typeID).Scan(&cat, &portion); err != nil {
		return 0
	}
	c := demandCapUnits(cat, portion)
	demandCaps.Store(typeID, c)
	return c
}

// clampDemandLedger brings every row over its cap down to it, once per boot. The want is
// folded forward to now on the way. The WHERE admits only rows whose decayed want is over the
// cap, so LEAST answers the cap whichever order MariaDB runs the two SET assignments.
func clampDemandLedger(ctx context.Context, db *sql.DB) {
	now := clock.Unix()
	w := demandWantedSQL("d", now)
	_, _ = db.ExecContext(ctx, `
		UPDATE rolebot_market_demand d
		  JOIN invTypes t ON t.typeID = d.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		   SET d.wanted = LEAST(`+w+`, ? * GREATEST(COALESCE(t.portionSize, 1), 1)),
		       d.decayAt = ?
		 WHERE g.categoryID = ?
		   AND `+w+` > ? * GREATEST(COALESCE(t.portionSize, 1), 1)`,
		demandChargeMaxRuns, now, catCharge, demandChargeMaxRuns)
}

// ensureDemandLedger adds the decay anchor and the region column. It is called from Init
// after ensureTables (rolebots.go). The lastRegionID backfill joins mapSolarSystems, which a
// scratch schema may lack, so its failure is ignored.
func ensureDemandLedger(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		`ALTER TABLE rolebot_market_demand ADD COLUMN IF NOT EXISTS decayAt BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_market_demand ADD COLUMN IF NOT EXISTS lastRegionID BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_market_demand ADD INDEX IF NOT EXISTS idx_rbdemand_region (lastRegionID)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("ensureDemandLedger: %w", err)
		}
	}
	_, _ = db.ExecContext(ctx, `
		UPDATE rolebot_market_demand d
		  JOIN mapSolarSystems s ON s.solarSystemID = d.lastSystemID
		   SET d.lastRegionID = s.regionID
		 WHERE d.lastRegionID = 0 AND d.lastSystemID > 0`)
	clampDemandLedger(ctx, db)
	return nil
}

// demandWantedSQL returns the decayed `wanted` of a ledger row as of now, as a SQL expression.
// alias is the table alias ("" for none); now is spliced in as an int64 literal.
// In an UPDATE assign it before decayAt or lastAt: MariaDB evaluates SET left to right.
// The CAST is required: a DOUBLE reaches Scan as "2.48e+07" from a million up and fails to parse.
func demandWantedSQL(alias string, now int64) string {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	return fmt.Sprintf("CAST(FLOOR(%swanted * POW(0.5, GREATEST(0, %d - GREATEST(%sdecayAt, %slastAt)) / %d)) AS SIGNED)",
		p, now, p, p, demandHalfLifeSec)
}

// decayedWanted is demandWantedSQL in Go. Pure.
func decayedWanted(wanted, anchor, now int64) int64 {
	if wanted <= 0 {
		return 0
	}
	age := now - anchor
	if age <= 0 {
		return wanted
	}
	return int64(math.Floor(float64(wanted) * math.Pow(0.5, float64(age)/demandHalfLifeSec)))
}

// The per-(pilot, type) daily throttle on noteDemandRow. It is in memory, like noteHullDemand's.
var (
	demandNoteMu sync.Mutex
	demandNoted  = map[[2]int64]int64{}
)

// demandNoteDue reports whether this pilot may note this type now, and if so
// records that it has. A pilot of 0 (no bot behind the note) is never
// throttled.
func demandNoteDue(charID, typeID, now int64) bool {
	if charID == 0 {
		return true
	}
	k := [2]int64{charID, typeID}
	demandNoteMu.Lock()
	defer demandNoteMu.Unlock()
	if last, ok := demandNoted[k]; ok && now-last < demandNoteEverySec {
		return false
	}
	demandNoted[k] = now
	return true
}

func init() {
	// Expired throttle entries are swept here, not inside the write, so a note never walks the
	// whole map under the lock.
	registerSweep("demandNotedSweep", func() {
		now := clock.Unix()
		demandNoteMu.Lock()
		for k, at := range demandNoted {
			if now-at >= demandNoteEverySec {
				delete(demandNoted, k)
			}
		}
		demandNoteMu.Unlock()
	})
	registerCharForget("demandNoted", func(id int64) {
		demandNoteMu.Lock()
		for k := range demandNoted {
			if k[0] == id {
				delete(demandNoted, k)
			}
		}
		demandNoteMu.Unlock()
	})
}

// resetDemandThrottle forgets every throttle entry. For tests.
func resetDemandThrottle() {
	demandNoteMu.Lock()
	demandNoted = map[[2]int64]int64{}
	demandNoteMu.Unlock()
}

// commitCorpDemand takes a corporation's posted build off the want while the
// corporation keeps its claim (claimant = -corpID): the order is the supply,
// and the claim keeps a personal industrialist off the type until the order
// closes (keepCorpDemandClaims releases it then).
func (m *Mod) commitCorpDemand(ctx context.Context, claimant, typeID, units int64) {
	now := clock.Unix()
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_market_demand
		   SET wanted = GREATEST(0, `+demandWantedSQL("", now)+` - ?), decayAt = ?, claimedAt = ?
		 WHERE typeID = ? AND claimedBy = ?`, units, now, now, typeID, claimant)
}

// keepCorpDemandClaims renews the corporation's claims for every type an open "demand" order
// is still building, and releases every other claim it holds.
func (m *Mod) keepCorpDemandClaims(ctx context.Context, corpID int64, open []corpWork) {
	claimant := -corpID
	live := map[int64]bool{}
	for _, w := range open {
		if w.Kind == workKindBuild && w.Purpose == corpDemandPurpose {
			live[w.ProductTypeID] = true
			m.touchClaim(ctx, claimant, w.ProductTypeID)
		}
	}
	held, err := int64Column(ctx, m,
		`SELECT typeID FROM rolebot_market_demand WHERE claimedBy = ?`, claimant)
	if err != nil {
		return
	}
	for _, t := range held {
		if !live[t] {
			m.releaseDemand(ctx, claimant, t)
		}
	}
}
