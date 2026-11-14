package rolebots

// The belt distress board: where the miners' refusals become the combat pilots' work orders.
//
// A miner whose whole search came up unwinnable is intelligence: belts full of ore sit behind rats its
// hull cannot fight. The mining search writes the nearest few systems it was refused (mining.go), the solo
// and gang hunts read the board (bounty.go, gangplan.go), gang formation sends fleets after the ones no
// solo hull can answer (gangform.go), and a full spawn clear settles the row beside the patrol cover it
// grants (combat_abstract.go). A row exits by settlement or by the TTL. Nothing here may block a bot's step:
// every write is best-effort, logged and swallowed, as in the demand ledgers (minerals.go).

import (
	"context"
	"math/rand"
	"sync"
)

const (
	// beltDistressTTLSec mirrors hostileMemorySec (hostilemem.go): a live starvation renews its rows every
	// planning pass (about 5 min), so a row unrenewed for a grudge's lifetime is not a signal.
	beltDistressTTLSec = 45 * 60

	// beltDistressMaxPerPass caps what one failing search may write. The belt search walks nearest first, so
	// the first few hopeless systems are the ones a combat sweep would hand back; recording all would make
	// the board a map of the galaxy.
	beltDistressMaxPerPass = 3

	// beltDistressRewriteSec is the per-process, per-system write throttle, matched to the planning cadence,
	// so co-located starving miners collapse to one row write per system per cycle.
	beltDistressRewriteSec = 5 * 60

	// The two ways a row is written, as wire values in the source column: search is the estimate refusing
	// everything from afar; belt is a pilot that stood in front of the rats and walked away, the confirmed sighting.
	distressSourceSearch = "search"
	distressSourceBelt   = "belt"
)

// distressCand is what the mining search hands over per refused system.
type distressCand struct {
	SysID    int64
	RegionID int64
	Sec      float64
}

var (
	distressWroteMu sync.Mutex
	distressWroteAt = map[int64]int64{} // systemID → unix of last row-write
)

// distressWriteAllowed is the throttle. It also prunes lazily, so the map
// tracks the live board rather than every system ever cried about.
func distressWriteAllowed(sysID, now int64) bool {
	distressWroteMu.Lock()
	defer distressWroteMu.Unlock()
	for id, at := range distressWroteAt {
		if now-at > beltDistressTTLSec {
			delete(distressWroteAt, id)
		}
	}
	if at, ok := distressWroteAt[sysID]; ok && now-at < beltDistressRewriteSec {
		return false
	}
	distressWroteAt[sysID] = now
	return true
}

// noteBeltDistress puts systems on the board. Best-effort: a starving miner never fails over bookkeeping.
// It reports how many rows it wrote, so the caller logs the cry only when one went out. corpID is who the
// cry is addressed to and is part of the key: a board with no owner would be answered by every hunter.
func (m *Mod) noteBeltDistress(ctx context.Context, charID, corpID int64, cands []distressCand, source string) int {
	now := clock.Unix()
	wrote := 0
	for _, c := range cands {
		if c.SysID == 0 || !distressWriteAllowed(c.SysID, now) {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_belt_distress
				(systemID, corpID, regionID, security, systemName, source, refusals, lastCharID, firstAt, lastAt)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				refusals   = refusals + 1,
				source     = VALUES(source),
				lastCharID = VALUES(lastCharID),
				lastAt     = VALUES(lastAt)`,
			c.SysID, corpID, c.RegionID, c.Sec, m.nameOfSystem(ctx, c.SysID), source,
			charID, now, now); err != nil {
			m.logBot(charID, logError, "threat", "belt distress board: %v", err)
			return wrote
		}
		wrote++
	}
	if wrote > 0 {
		// Cheap on the lastAt index, and it gives an unanswered row its second exit: the board does not keep
		// last month's starvations.
		m.expireBeltDistress(ctx)
	}
	return wrote
}

// expireBeltDistress is the TTL exit: a cry that aged out was not answered, and the log says so, since
// it would otherwise read like a settled one. It returns how many rows expired so the harness can pin it.
func (m *Mod) expireBeltDistress(ctx context.Context) int {
	now := clock.Unix()
	cutoff := now - beltDistressTTLSec
	rows, err := m.db.QueryContext(ctx, `
		SELECT systemID, systemName, refusals, firstAt
		FROM rolebot_belt_distress WHERE lastAt < ?`, cutoff)
	if err != nil {
		return 0
	}
	expired := 0
	for rows.Next() {
		var sysID, cry, firstAt int64
		var name string
		if rows.Scan(&sysID, &name, &cry, &firstAt) != nil {
			continue
		}
		expired++
		m.logBot(0, logInfo, "threat",
			"distress in %s expired unanswered after %dmin (%d refusals)",
			name, (now-firstAt)/60, cry)
	}
	rows.Close()
	if expired > 0 {
		_, _ = m.db.ExecContext(ctx,
			`DELETE FROM rolebot_belt_distress WHERE lastAt < ?`, cutoff)
	}
	return expired
}

// settleBeltDistress takes a system off the board once a clear answered the cry. It reports whether
// there was a row, so the caller logs the answer only when there was a question. It settles for the corp
// that cleared it, not everyone: another corporation's miners are still being eaten there.
func (m *Mod) settleBeltDistress(ctx context.Context, corpID, sysID int64) bool {
	res, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_belt_distress WHERE systemID = ? AND corpID = ?`, sysID, corpID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// distressedBeltSystems is the live board as a set, for the site searches:
// read once per pick, checked per candidate. The table is bounded to
// currently-starving systems under TTL, so the whole read stays small.
func (m *Mod) distressedBeltSystems(ctx context.Context, corpID int64) map[int64]bool {
	rows, err := m.db.QueryContext(ctx,
		`SELECT systemID FROM rolebot_belt_distress WHERE lastAt >= ? AND corpID = ?`,
		clock.Unix()-beltDistressTTLSec, corpID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// pickReliefSystem is formation's read: the loudest live cry inside the
// given region scope (empty scope = anywhere), ties split randomly so two
// fleets formed in the same pass do not both fly to the same system.
func (m *Mod) pickReliefSystem(ctx context.Context, corpID int64, regions map[int64]bool) (int64, string) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT systemID, regionID, systemName, refusals
		FROM rolebot_belt_distress WHERE lastAt >= ? AND corpID = ?`,
		clock.Unix()-beltDistressTTLSec, corpID)
	if err != nil {
		return 0, ""
	}
	defer rows.Close()
	var (
		bestID   int64
		bestName string
		bestCry  int64
		ties     int
	)
	for rows.Next() {
		var sysID, regionID, cry int64
		var name string
		if rows.Scan(&sysID, &regionID, &name, &cry) != nil {
			continue
		}
		if len(regions) > 0 && !regions[regionID] {
			continue
		}
		switch {
		case bestID == 0 || cry > bestCry:
			bestID, bestName, bestCry, ties = sysID, name, cry, 1
		case cry == bestCry:
			// Reservoir draw over the tied set: each tie replaces the pick with probability 1/n, a uniform choice
			// without holding the set.
			ties++
			if rand.Intn(ties) == 0 {
				bestID, bestName = sysID, name
			}
		}
	}
	return bestID, bestName
}

// anyBeltDistress reports whether any corporation's board is live. It is the one corp-agnostic read: the
// formation sweep sizes the roster's formation budget with it, and nothing is routed by it. A fleet picks
// its relief system from its own corp's board (pickReliefSystem).
func (m *Mod) anyBeltDistress(ctx context.Context) bool {
	var one int64
	err := m.db.QueryRowContext(ctx,
		`SELECT 1 FROM rolebot_belt_distress WHERE lastAt >= ? LIMIT 1`,
		clock.Unix()-beltDistressTTLSec).Scan(&one)
	return err == nil
}
