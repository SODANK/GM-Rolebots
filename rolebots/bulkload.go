package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Roster hydration: one query per table instead of five per pilot.
// loadRunsWhere reads the roster in one statement, then the bulk readers below fill the
// boarded ship, profile, state row and traits for every pilot at once. The single-pilot
// loaders stay for one-character callers (the panel, the planners) and share the column
// lists and row decoders, so the two paths cannot drift.

// bulkChunk caps how many IDs go into one IN list. Well inside
// max_allowed_packet at any roster size, and it keeps the planner's job on
// each chunk small.
const bulkChunk = 1000

// idChunks splits ids into runs of at most n.
func idChunks(ids []int64, n int) [][]int64 {
	var out [][]int64
	for len(ids) > n {
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}

// inClause renders "(?,?,?)" and the matching args for one chunk.
func inClause(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return "(" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")", args
}

// forEachChunk runs fn over every chunk of ids, stopping at the first error.
func forEachChunk(ids []int64, fn func(where string, args []any) error) error {
	for _, chunk := range idChunks(ids, bulkChunk) {
		where, args := inClause(chunk)
		if err := fn(where, args); err != nil {
			return err
		}
	}
	return nil
}

// hydrateRuns fills in everything a botRun needs beyond the roster row. shipCand maps a
// character to the entity its locationID points at, confirmed here against ownership.
// Every pilot comes out with a non-nil Profile and State, as the single-pilot loaders give.
func (m *Mod) hydrateRuns(ctx context.Context, runs []*botRun, shipCand map[int64]int64) error {
	if len(runs) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(runs))
	for _, b := range runs {
		ids = append(ids, b.CharID)
	}

	ships, err := loadShipsBulk(ctx, m.db, shipCand)
	if err != nil {
		return err
	}
	profiles, err := m.loadProfilesBulk(ctx, ids)
	if err != nil {
		return err
	}
	states, err := loadStatesBulk(ctx, m.db, ids)
	if err != nil {
		return err
	}
	traits, err := loadTraitsBulk(ctx, m.db, ids)
	if err != nil {
		return err
	}

	for _, b := range runs {
		b.ShipID = ships[b.CharID]
		b.Profile = profiles[b.CharID]
		b.State = states[b.CharID]
		t := traits[b.CharID]
		if t == nil {
			t = map[string]int{}
		}
		b.Traits = t
		b.Tune = resolveTuning(t)
	}
	return nil
}

// botTraits is the pilot's traits off the run when the bulk load carried
// them, else one read. Five home-visit and employer-review sites re-read
// what the pass had already loaded.
func (m *Mod) botTraits(ctx context.Context, b *botRun) (map[string]int, error) {
	if b != nil && b.Traits != nil {
		out := make(map[string]int, len(b.Traits))
		for k, v := range b.Traits {
			out[k] = v
		}
		return out, nil
	}
	return getTraits(ctx, m.db, b.CharID)
}

// loadShipsBulk confirms which of the candidate locations really are ships
// their character owns. cand is charID → the entity itemID the character
// sits inside; the result carries only the confirmed ones.
func loadShipsBulk(ctx context.Context, db *sql.DB, cand map[int64]int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(cand) == 0 {
		return out, nil
	}
	// Keyed by the item, since the entity row is found by it; a ship's ownerID is then
	// matched back against the character that named it.
	items := make([]int64, 0, len(cand))
	owner := make(map[int64]int64, len(cand))
	for charID, itemID := range cand {
		items = append(items, itemID)
		owner[itemID] = charID
	}
	err := forEachChunk(items, func(where string, args []any) error {
		rows, err := db.QueryContext(ctx,
			`SELECT itemID, ownerID FROM entity WHERE itemID IN `+where, args...)
		if err != nil {
			return fmt.Errorf("loadShipsBulk: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var itemID, ownerID int64
			if err := rows.Scan(&itemID, &ownerID); err != nil {
				return fmt.Errorf("loadShipsBulk scan: %w", err)
			}
			// A corporation kit lent for a fleet op stays the corporation's
			// while the pilot flies it (corparmory.go).
			if charID, ok := owner[itemID]; ok && (ownerID == charID || loanedTo(itemID) == charID) {
				out[charID] = itemID
			}
		}
		return rows.Err()
	})
	return out, err
}

// loadProfilesBulk reads every pilot's identity, outlook included. The outlook is the
// employer's opinion of the factions (outlook.go), memoised across the roster, so this is a
// *Mod method. It does not read rolebot_standings: that table is too large to load on
// every pass.
func (m *Mod) loadProfilesBulk(ctx context.Context, ids []int64) (map[int64]*Profile, error) {
	db := m.db
	out := make(map[int64]*Profile, len(ids))
	for _, id := range ids {
		out[id] = newProfile(id)
	}
	err := forEachChunk(ids, func(where string, args []any) error {
		rows, err := db.QueryContext(ctx, `SELECT characterID, `+profileCols+`
			FROM rolebot_profiles WHERE characterID IN `+where, args...)
		if err != nil {
			return fmt.Errorf("loadProfilesBulk: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var charID int64
			p := newProfile(0)
			err := scanProfileInto(func(dest ...any) error {
				return rows.Scan(append([]any{&charID}, dest...)...)
			}, p)
			if err != nil {
				return fmt.Errorf("loadProfilesBulk scan: %w", err)
			}
			if _, ok := out[charID]; ok {
				p.CharacterID = charID
				out[charID] = p
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	employer, err := m.outlooksFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for charID, p := range out {
		p.Standings = employer[charID]
	}
	return out, nil
}

// loadStatesBulk reads every pilot's FSM row; a pilot with none gets the
// same fresh idle state loadState would have handed back.
func loadStatesBulk(ctx context.Context, db *sql.DB, ids []int64) (map[int64]*SimState, error) {
	out := make(map[int64]*SimState, len(ids))
	for _, id := range ids {
		out[id] = newSimState(id)
	}
	err := forEachChunk(ids, func(where string, args []any) error {
		rows, err := db.QueryContext(ctx, `SELECT characterID, `+stateCols+`
			FROM rolebot_state WHERE characterID IN `+where, args...)
		if err != nil {
			return fmt.Errorf("loadStatesBulk: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var charID int64
			st := newSimState(0)
			err := scanStateInto(func(dest ...any) error {
				return rows.Scan(append([]any{&charID}, dest...)...)
			}, st)
			if err != nil {
				return fmt.Errorf("loadStatesBulk scan: %w", err)
			}
			if _, ok := out[charID]; ok {
				st.CharID = charID
				out[charID] = st
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// loadTraitsBulk reads every pilot's personality. A pilot with no rows is
// absent from the result; resolveTuning reads a nil map as all-defaults,
// which is what getTraits' empty map meant.
func loadTraitsBulk(ctx context.Context, db *sql.DB, ids []int64) (map[int64]map[string]int, error) {
	out := map[int64]map[string]int{}
	err := forEachChunk(ids, func(where string, args []any) error {
		rows, err := db.QueryContext(ctx,
			`SELECT characterID, trait, value FROM rolebot_traits
			 WHERE characterID IN `+where, args...)
		if err != nil {
			return fmt.Errorf("loadTraitsBulk: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var charID int64
			var k string
			var v int
			if err := rows.Scan(&charID, &k, &v); err != nil {
				return fmt.Errorf("loadTraitsBulk scan: %w", err)
			}
			if out[charID] == nil {
				out[charID] = map[string]int{}
			}
			out[charID][k] = v
		}
		return rows.Err()
	})
	return out, err
}
