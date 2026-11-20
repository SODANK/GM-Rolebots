package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// presenceRow is the identity/location info needed to project an enabled
// rolebot into the in-memory presence systems (station guest list, Local,
// corp and alliance chat).
type presenceRow struct {
	CharID        int64
	CharName      string
	CorpID        int64
	AllianceID    int64 // the corporation's alliance, 0 for none
	CharTypeID    int64
	StationID     int64 // 0 when in space
	SolarSystemID int64
}

var (
	presenceMu      sync.Mutex
	presenceApplied = map[int64]presenceRow{}
)

// presenceSelect is the presence row query; listEnabledPresence reads every enabled rolebot and
// listPresenceFor the ones named.
const presenceSelect = `
		SELECT c.characterID, c.characterName, c.corporationID,
		       COALESCE(co.allianceID, 0),
		       COALESCE(ce.typeID, 0), c.stationID, c.solarSystemID
		FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		LEFT JOIN corporation co ON co.corporationID = c.corporationID
		LEFT JOIN entity ce ON ce.itemID = c.characterID
		WHERE b.enabled = 1`

// listEnabledPresence returns identity/location for every enabled rolebot.
func listEnabledPresence(ctx context.Context, db *sql.DB) ([]presenceRow, error) {
	return queryPresence(ctx, db, "", nil)
}

// listPresenceFor is listEnabledPresence for the named pilots only: a pilot not returned is not
// an enabled rolebot.
func listPresenceFor(ctx context.Context, db *sql.DB, ids []int64) ([]presenceRow, error) {
	var out []presenceRow
	err := forEachChunk(ids, func(where string, args []any) error {
		rows, err := queryPresence(ctx, db, " AND b.characterID IN "+where, args)
		out = append(out, rows...)
		return err
	})
	return out, err
}

func queryPresence(ctx context.Context, db *sql.DB, where string, args []any) ([]presenceRow, error) {
	rows, err := db.QueryContext(ctx, presenceSelect+where, args...)
	if err != nil {
		return nil, fmt.Errorf("listEnabledPresence: %w", err)
	}
	defer rows.Close()
	var out []presenceRow
	for rows.Next() {
		var r presenceRow
		if err := rows.Scan(&r.CharID, &r.CharName, &r.CorpID, &r.AllianceID,
			&r.CharTypeID, &r.StationID, &r.SolarSystemID); err != nil {
			return nil, fmt.Errorf("listEnabledPresence scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// syncPresence reconciles in-memory rolebot presence with the DB: enabled rolebots (while the
// master switch is on) appear in their station's guest list, system Local, corp and alliance chat
// and the session registry; everything else is withdrawn. Safe from any goroutine; called at
// startup and after every ESP settings or sim location change. Sessions are Conn-keyed, so esp.JoinBot is the way in.
func (m *Mod) syncPresence() {
	if m.lsc == nil || m.stations == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	presenceSyncMu.Lock()
	defer presenceSyncMu.Unlock()

	// The moves recorded so far are covered by the read below, so the set is emptied before it: a
	// location written after this point stays marked for the next sync. On a failed read they go back.
	pending := takePresenceDirty()

	desired := map[int64]presenceRow{}
	if Master() {
		rows, err := listEnabledPresence(ctx, m.db)
		if err != nil {
			rlogf("rolebots: presence sync: %v", err)
			for _, id := range pending {
				markPresenceDirty(id)
			}
			return
		}
		for _, r := range rows {
			// A pilot the online cap has logged off holds no presence (no session, Local, corp channel or
			// guest list; popcap.go): enabled, on the roster, simply not connected.
			if popIsOffline(r.CharID) {
				continue
			}
			desired[r.CharID] = r
		}
	}

	presenceMu.Lock()
	defer presenceMu.Unlock()

	// Withdraw presence for rolebots no longer enabled. This is membership only: a bot that is
	// still enabled but has moved is handled by the reconcile loop below.
	for charID, old := range presenceApplied {
		if _, ok := desired[charID]; ok {
			continue
		}
		m.withdrawPresenceLocked(ctx, charID, old)
	}

	// A location the full sync has to correct is a move the scoped syncs never saw: a write made
	// outside the module (the core, a GM tool) or one that skipped updateCharacterStation. Said
	// once per sync, so a missing mark shows up in the log instead of as a stale guest list.
	marked := make(map[int64]bool, len(pending))
	for _, id := range pending {
		marked[id] = true
	}
	missed := 0
	for charID, r := range desired {
		if old, known := presenceApplied[charID]; known && !marked[charID] &&
			(old.StationID != r.StationID || old.SolarSystemID != r.SolarSystemID) {
			missed++
		}
		m.applyPresenceLocked(ctx, charID, r)
	}
	if missed > 0 {
		rlogf("rolebots: presence: full sync corrected %d location(s) the scoped syncs had not seen", missed)
	}
}

// syncPresenceFor is syncPresence for the named pilots only: each is reconciled exactly as a full
// sync would reconcile it (applied, moved, or withdrawn when it is no longer an enabled pilot on
// the server), and nobody else is touched. The roster pass and the grid lane call it with the
// pilots whose location was written since the last sync (markPresenceDirty), so a pass that moved
// a handful of pilots reads a handful of rows instead of the whole roster. Changes that are not a
// location write (a corporation or alliance change, the cap, ESP toggles) still run a full sync.
func (m *Mod) syncPresenceFor(ids []int64) {
	if m.lsc == nil || m.stations == nil || len(ids) == 0 {
		return
	}
	if !Master() {
		m.syncPresence() // the full sync withdraws everybody
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	presenceSyncMu.Lock()
	defer presenceSyncMu.Unlock()
	rows, err := listPresenceFor(ctx, m.db, ids)
	if err != nil {
		rlogf("rolebots: presence sync: %v", err)
		for _, id := range ids {
			markPresenceDirty(id) // retried with the next sync
		}
		return
	}
	desired := make(map[int64]presenceRow, len(rows))
	for _, r := range rows {
		if popIsOffline(r.CharID) {
			continue
		}
		desired[r.CharID] = r
	}

	presenceMu.Lock()
	defer presenceMu.Unlock()
	for _, id := range ids {
		if r, ok := desired[id]; ok {
			m.applyPresenceLocked(ctx, id, r)
		} else if old, known := presenceApplied[id]; known {
			m.withdrawPresenceLocked(ctx, id, old)
		}
	}
}

// withdrawPresenceLocked takes one pilot off every presence surface. presenceMu is held.
func (m *Mod) withdrawPresenceLocked(ctx context.Context, charID int64, old presenceRow) {
	m.stations.LeaveBot(charID)
	if m.sessions != nil {
		m.sessions.LeaveBot(charID)
	}
	if old.CorpID != 0 {
		m.lsc.LeaveBot(old.CorpID, charID)
	}
	if old.AllianceID != 0 {
		m.lsc.LeaveBot(old.AllianceID, charID)
	}
	if old.SolarSystemID != 0 {
		m.lsc.LeaveBot(old.SolarSystemID, charID)
		m.dropChannelChar(ctx, old.SolarSystemID, charID)
	}
	delete(presenceApplied, charID)
	rlogf("rolebots: presence withdrawn charID=%d (%s)", charID, old.CharName)
}

// applyPresenceLocked brings one enabled, online pilot's presence in line with r. presenceMu is
// held. Each surface is reconciled on its own field: LeaveBot and JoinBot each broadcast to the
// whole channel, so diffing the whole row would make a bot that merely docked leave and re-enter
// its corp channel.
func (m *Mod) applyPresenceLocked(ctx context.Context, charID int64, r presenceRow) {
	{
		old, known := presenceApplied[charID]

		if !known || old.StationID != r.StationID {
			if known && old.StationID != 0 {
				m.stations.LeaveBot(charID)
			}
			if r.StationID != 0 {
				m.stations.JoinBot(r.StationID, charID)
			}
		}

		// The session registry is keyed by charID and JoinBot updates an entry in place, so new values
		// need no leave. A bot with no system is nowhere: the overlays skip a zero solarSystemID.
		if m.sessions != nil && (!known || old != r) {
			if r.SolarSystemID != 0 {
				m.sessions.JoinBot(charID, r.CharName, r.CorpID, r.StationID, r.SolarSystemID)
			} else if known {
				m.sessions.LeaveBot(charID)
			}
		}

		if !known || old.CorpID != r.CorpID {
			if known && old.CorpID != 0 {
				m.lsc.LeaveBot(old.CorpID, charID)
			}
			if r.CorpID != 0 {
				name := evedb.ResolveChannelDisplayName(ctx, m.db, r.CorpID)
				if err := evedb.EnsureChannel(ctx, m.db, r.CorpID, r.CorpID, name); err != nil {
					rlogf("rolebots: EnsureChannel corp %d: %v", r.CorpID, err)
				}
				m.lsc.JoinBot(r.CorpID, charID, 0, r.CorpID, r.CharTypeID, 0, r.CharName)
			}
		}

		// The alliance channel is keyed on the allianceID as the corp channel is on the corpID (a human
		// is seated in it at login), not on the bot's location, so docking or jumping does not flicker it.
		if !known || old.AllianceID != r.AllianceID {
			if known && old.AllianceID != 0 {
				m.lsc.LeaveBot(old.AllianceID, charID)
			}
			if r.AllianceID != 0 {
				name := evedb.ResolveChannelDisplayName(ctx, m.db, r.AllianceID)
				if err := evedb.EnsureChannel(ctx, m.db, r.AllianceID, r.AllianceID, name); err != nil {
					rlogf("rolebots: EnsureChannel alliance %d: %v", r.AllianceID, err)
				}
				m.lsc.JoinBot(r.AllianceID, charID, 0, r.CorpID, r.CharTypeID, 0, r.CharName)
			}
		}

		if !known || old.SolarSystemID != r.SolarSystemID {
			if known && old.SolarSystemID != 0 {
				m.lsc.LeaveBot(old.SolarSystemID, charID)
				m.dropChannelChar(ctx, old.SolarSystemID, charID)
			}
			if r.SolarSystemID != 0 {
				name := evedb.ResolveChannelDisplayName(ctx, m.db, r.SolarSystemID)
				if err := evedb.EnsureChannel(ctx, m.db, r.SolarSystemID, r.SolarSystemID, name); err != nil {
					rlogf("rolebots: EnsureChannel system %d: %v", r.SolarSystemID, err)
				}
				m.lsc.JoinBot(r.SolarSystemID, charID, 0, r.CorpID, r.CharTypeID, 0, r.CharName)
				if err := evedb.UpsertChannelChar(ctx, m.db, r.SolarSystemID, charID, r.CorpID); err != nil {
					rlogf("rolebots: UpsertChannelChar(%d,%d): %v", r.SolarSystemID, charID, err)
				}
			}
		}

		if old != r {
			presenceApplied[charID] = r
			rlogf("rolebots: presence applied charID=%d (%s) station=%d system=%d corp=%d alliance=%d",
				charID, r.CharName, r.StationID, r.SolarSystemID, r.CorpID, r.AllianceID)
		}
	}
}

// presenceSyncMu runs one sync (full or scoped) at a time, read and apply together, so a sync that
// read the database earlier can never apply over one that read it later.
var presenceSyncMu sync.Mutex

var (
	presenceDirtyMu sync.Mutex
	// presenceDirty holds the pilots whose location was written since the last sync.
	presenceDirty = map[int64]struct{}{}
)

// markPresenceDirty records that a pilot's station or system was just written. Every module write
// of a pilot's location (evedb.UpdateCharacterStation) calls it, whichever lane or pilot's turn made
// the write, so a gangmate podded in another pilot's duel is synced as surely as the pilot that flew.
func markPresenceDirty(charID int64) {
	if charID == 0 {
		return
	}
	presenceDirtyMu.Lock()
	presenceDirty[charID] = struct{}{}
	presenceDirtyMu.Unlock()
}

// takePresenceDirty empties the set and returns what it held.
func takePresenceDirty() []int64 {
	presenceDirtyMu.Lock()
	defer presenceDirtyMu.Unlock()
	if len(presenceDirty) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(presenceDirty))
	for id := range presenceDirty {
		ids = append(ids, id)
	}
	presenceDirty = map[int64]struct{}{}
	return ids
}

// updateCharacterStation is evedb.UpdateCharacterStation for a rolebot: every module write of a
// pilot's location goes through here so the pilot is marked for the next presence sync. It is
// marked even when the write fails: a sync reconciles to whatever the row holds, so it costs one
// row read and can never be wrong.
func updateCharacterStation(ctx context.Context, db *sql.DB, charID, stationID, solarSystemID, constellationID, regionID int64) error {
	err := evedb.UpdateCharacterStation(ctx, db, charID, stationID, solarSystemID, constellationID, regionID)
	markPresenceDirty(charID)
	return err
}

// syncPresenceDirty syncs the pilots whose location was written since the last sync.
func (m *Mod) syncPresenceDirty() {
	m.syncPresenceFor(takePresenceDirty())
}

// dropChannelChar is the other half of UpsertChannelChar: the row written when a bot joins a
// system's Local must come out when it leaves, as a player's does (server/lsc leaveAndBroadcast),
// or the table gains a permanent row per (bot, system) visited. Corp and alliance membership is
// not deleted here: the module never writes a channelChars row for either.
func (m *Mod) dropChannelChar(ctx context.Context, systemID, charID int64) {
	if systemID == 0 || charID == 0 {
		return
	}
	if err := evedb.DeleteChannelChar(ctx, m.db, systemID, charID); err != nil {
		rlogf("rolebots: DeleteChannelChar(%d,%d): %v", systemID, charID, err)
	}
}

func init() {
	registerCharForget("presenceApplied", func(id int64) {
		presenceMu.Lock()
		delete(presenceApplied, id)
		presenceMu.Unlock()
	})
	registerCharForget("presenceDirty", func(id int64) {
		presenceDirtyMu.Lock()
		delete(presenceDirty, id)
		presenceDirtyMu.Unlock()
	})
}
