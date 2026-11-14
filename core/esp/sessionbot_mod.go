package esp

import (
	"strconv"
	"time"
)

// Synthetic sessions for rolebots, so the star map's pilot counts (they read the
// session registry only) include bots. A bot session has a nil Conn. It is in
// Snapshot and the FindByStationID, FindBySystemID and FindByCorpID lookups,
// but not in byCharID, so FindByCharID, NotifyCharacter and ClaimCharID still
// mean a real client is connected. Overlay build only.

// botSessionKey is the r.sessions key for a synthetic session. Real sessions are
// keyed by remote address, which no bot has.
func botSessionKey(charID int64) string { return "bot:" + strconv.FormatInt(charID, 10) }

// JoinBot registers a synthetic session for a rolebot, or updates the existing
// one. ConnectedAt is kept on update because the dashboard sorts on it. Idempotent.
func (r *SessionRegistry) JoinBot(charID int64, charName string, corpID, stationID, solarSystemID int64) {
	if charID == 0 {
		return
	}
	key := botSessionKey(charID)
	now := time.Now()

	r.mu.Lock()

	if s, ok := r.sessions[key]; ok && s.Bot {
		s.CharName = charName
		s.CorpID = corpID
		s.StationID = stationID
		s.SolarSystemID = solarSystemID
		s.LastActivity = now
		r.mu.Unlock()
		return
	}
	r.sessions[key] = &Session{
		AccountName:   "(rolebot)",
		CharID:        charID,
		CharName:      charName,
		CorpID:        corpID,
		StationID:     stationID,
		SolarSystemID: solarSystemID,
		Addr:          key,
		ConnectedAt:   now,
		LastActivity:  now,
		Bot:           true,
	}
	r.mu.Unlock()

	// Announce only a new join, outside the lock; the hook hits the DB.
	if PresenceChangedHook != nil {
		PresenceChangedHook(charID, true)
	}
}

// LeaveBot removes a synthetic session. Idempotent; the key is bot-only and the Bot
// flag is checked, so a real session is never removed.
func (r *SessionRegistry) LeaveBot(charID int64) {
	key := botSessionKey(charID)
	r.mu.Lock()
	s, ok := r.sessions[key]
	removed := ok && s.Bot
	if removed {
		delete(r.sessions, key)
	}
	r.mu.Unlock()

	if removed && PresenceChangedHook != nil {
		PresenceChangedHook(charID, false)
	}
}
