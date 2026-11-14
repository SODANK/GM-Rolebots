package rolebots

// The economy epoch. One seed, one row (rolebot_epoch, id = 1), redrawn in one place: the economy
// wipe's Learned branch (econpurge.go). The per-epoch stateless draws (fittaste.go) derive from it, so
// the seed is the epoch.
//
// The bot wipe (botpurge.go) never touches it: the epoch is shared learned state that survives the roster.

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"sync"
)

var (
	epochMu     sync.Mutex
	epochSeedV  int64
	epochLoaded bool
)

// drawEpochSeed rolls a fresh seed with crypto/rand, since it is a draw made once per economy wipe,
// not a sim roll, and nobody picks it. Zero is a legal seed; the fallback covers a crypto/rand read
// error only.
func drawEpochSeed() int64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return clock.Unix() * 2654435761
	}
	return int64(binary.LittleEndian.Uint64(b[:]))
}

// epochSeed answers the current epoch's seed, creating the row on first boot. It is process-cached:
// fit taste (fittaste.go) is the only reader, and the wipe invalidates the cache. A query error
// answers 0 without caching it, so the next sweep retries; that sweep's map uses seed 0 and heals
// on the next pass.
func (m *Mod) epochSeed(ctx context.Context) int64 {
	epochMu.Lock()
	defer epochMu.Unlock()
	if epochLoaded {
		return epochSeedV
	}
	var seed int64
	err := m.db.QueryRowContext(ctx,
		`SELECT seed FROM rolebot_epoch WHERE id = 1`).Scan(&seed)
	if errors.Is(err, sql.ErrNoRows) {
		// First boot. INSERT IGNORE + re-read so two nodes bootstrapping at
		// once both converge on whichever draw won the insert.
		if _, ierr := m.db.ExecContext(ctx, `
			INSERT IGNORE INTO rolebot_epoch (id, seed, epoch, drawnAt)
			VALUES (1, ?, 1, ?)`, drawEpochSeed(), clock.Unix()); ierr != nil {
			rlogf("rolebots: epoch bootstrap: %v", ierr)
			return 0
		}
		err = m.db.QueryRowContext(ctx,
			`SELECT seed FROM rolebot_epoch WHERE id = 1`).Scan(&seed)
	}
	if err != nil {
		rlogf("rolebots: epoch seed read: %v", err)
		return 0
	}
	epochSeedV, epochLoaded = seed, true
	return seed
}

// redrawEpoch rolls a new economy epoch: new seed, epoch counter up one. The economy wipe calls it
// beside clearLiquidityCache; the wipe empties what the last epoch learned and this changes the next.
func (m *Mod) redrawEpoch(ctx context.Context) error {
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_epoch (id, seed, epoch, drawnAt)
		VALUES (1, ?, 1, ?)
		ON DUPLICATE KEY UPDATE
			seed = VALUES(seed), epoch = epoch + 1, drawnAt = VALUES(drawnAt)`,
		drawEpochSeed(), clock.Unix()); err != nil {
		return err
	}
	clearEpochCache()
	return nil
}

// clearEpochCache mirrors clearLiquidityCache one shelf over.
func clearEpochCache() {
	epochMu.Lock()
	defer epochMu.Unlock()
	epochSeedV, epochLoaded = 0, false
}
