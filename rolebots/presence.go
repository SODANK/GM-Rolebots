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

// listEnabledPresence returns identity/location for every enabled rolebot.
func listEnabledPresence(ctx context.Context, db *sql.DB) ([]presenceRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName, c.corporationID,
		       COALESCE(co.allianceID, 0),
		       COALESCE(ce.typeID, 0), c.stationID, c.solarSystemID
		FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		LEFT JOIN corporation co ON co.corporationID = c.corporationID
		LEFT JOIN entity ce ON ce.itemID = c.characterID
		WHERE b.enabled = 1`)
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

	desired := map[int64]presenceRow{}
	if Master() {
		rows, err := listEnabledPresence(ctx, m.db)
		if err != nil {
			rlogf("rolebots: presence sync: %v", err)
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

	// Reconcile each surface on its own field. LeaveBot and JoinBot each broadcast to the whole
	// channel, so diffing the whole row would make a bot that merely docked leave and re-enter its corp channel.
	for charID, r := range desired {
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
}