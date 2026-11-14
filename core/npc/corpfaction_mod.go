package npc

// A module's road into the corp-to-faction map. LoadStandings fills it from
// crpNPCCorporations; a player corporation has no row there and reads as 0,
// which IsHostile and isPlayerHostile treat as "hostile to everything". The
// rolebots module declares a player corporation's faction allegiance here
// (modules/GM-RoleBot-mod/docs/ALLEGIANCE.md). Overlay build only.

// SetCorpFaction declares corpID's faction for CorpFaction and IsHostile: an
// NPC corporation's row, or a player corporation's allegiance pushed by a
// module. 0 removes the entry. A zero StandingsMap gets its map made here.
func (s *StandingsMap) SetCorpFaction(corpID, factionID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if factionID == 0 {
		delete(s.corpFaction, corpID)
		return
	}
	if s.corpFaction == nil {
		s.corpFaction = make(map[int64]int64)
	}
	s.corpFaction[corpID] = factionID
}
