package rolebots

import (
	"context"
	"fmt"
	"sync"

	evedb "gurimoon/db"
)

// The employer's outlook: where a pilot's opinion of a faction comes from.
//
// It comes from whoever employs the pilot and is never stored, so a change to
// the employer's row or to the pilot's employer moves the opinion. A bot keeps
// only its opinion of other pilots (pvpgrudge.go, gangbond.go, piracy.go).
// An NPC corporation's outlook is its faction's npcStandings row plus the
// faction itself at +10. A player corporation's is its crpStandings rows, then
// its alliance's allianceRelationships, then its pledged faction's row
// (allegiance.go) for whatever it has not addressed itself. Empty is normal
// and means neutral, not hostile (factionHostile reads faction 0 as hostile).

// allianceRelStanding maps an alliance's four-way relationship enum onto the
// +-10 scale, landing each value on one of the four tier constants (Enemy is
// "despised", the strongest statement this module has). An unrecognised value
// reads neutral, since the column is a plain INT.
func allianceRelStanding(rel int64) float64 {
	switch rel {
	case evedb.AllianceRelationshipEnemy:
		return StandingDespised
	case evedb.AllianceRelationshipCompetitor:
		return StandingDisliked
	case evedb.AllianceRelationshipNAP:
		return StandingLiked
	case evedb.AllianceRelationshipFriend:
		return StandingDevoted
	}
	return 0
}

// How long an employer's outlook is good for. One TTL covers an NPC
// corporation's static seed data and a player corporation's edited standings.
const employerOutlookTTLSec = 10 * 60

var (
	employerOutlookMu    sync.Mutex
	employerOutlookCache = map[int64]employerOutlookEntry{}
)

type employerOutlookEntry struct {
	list []Standing
	at   int64
}

// employerOutlook is a pilot's whole outlook (Profile.Standings), derived from
// its corporation and memoised per corporation. The slice is shared by every
// pilot there and clipped (len == cap) so an append reallocates; copy before
// modifying entries. An empty result is a real answer, never "hostile".
func (m *Mod) employerOutlook(ctx context.Context, corpID int64) ([]Standing, error) {
	if corpID == 0 {
		return nil, nil
	}
	now := clock.Unix()
	employerOutlookMu.Lock()
	if e, ok := employerOutlookCache[corpID]; ok && now-e.at < employerOutlookTTLSec {
		employerOutlookMu.Unlock()
		return e.list, nil
	}
	employerOutlookMu.Unlock()

	var (
		list []Standing
		err  error
	)
	if factionID := m.npcCorpFactionID(ctx, corpID); factionID != 0 {
		list, err = m.npcCorpOutlook(ctx, factionID)
	} else {
		list, err = m.playerCorpOutlook(ctx, corpID)
		if err == nil {
			if a := m.corpAllegiance(ctx, corpID); a != 0 {
				var fill []Standing
				if fill, err = m.npcCorpOutlook(ctx, a); err == nil {
					list = mergeOutlook(list, fill)
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}

	// Clip before caching: len == cap, so an append by a holder of the shared
	// slice allocates its own array rather than scribbling on the corporation's.
	list = list[:len(list):len(list)]

	employerOutlookMu.Lock()
	for id, old := range employerOutlookCache {
		if now-old.at >= employerOutlookTTLSec {
			delete(employerOutlookCache, id)
		}
	}
	employerOutlookCache[corpID] = employerOutlookEntry{list: list, at: now}
	employerOutlookMu.Unlock()
	return list, nil
}

// forgetEmployerOutlook drops one corporation's cached politics, so the next
// read re-derives them. For an operator who has just edited a corporation's
// standings and wants to see it take effect.
func forgetEmployerOutlook(corpID int64) {
	employerOutlookMu.Lock()
	if corpID == 0 {
		employerOutlookCache = map[int64]employerOutlookEntry{}
	} else {
		delete(employerOutlookCache, corpID)
	}
	employerOutlookMu.Unlock()
}

// outlookFor is one pilot's employer politics, resolved through whoever
// currently employs it. character_.corporationID is the source of truth, the
// same column the session, the corp window and corphome.go read.
func (m *Mod) outlookFor(ctx context.Context, charID int64) ([]Standing, error) {
	var corpID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(corporationID, 0) FROM character_ WHERE characterID = ?`,
		charID).Scan(&corpID); err != nil {
		return nil, nil // an unknown pilot has no employer and no politics
	}
	return m.employerOutlook(ctx, corpID)
}

// outlooksFor is outlookFor for a whole roster, in one query plus one memo read
// per distinct employer. The corp-ranking pass hydrates every rostered pilot at
// once, and a per-pilot lookup there would be thousands of queries.
func (m *Mod) outlooksFor(ctx context.Context, ids []int64) (map[int64][]Standing, error) {
	corpOf := map[int64]int64{}
	err := forEachChunk(ids, func(where string, args []any) error {
		rows, qerr := m.db.QueryContext(ctx, `
			SELECT characterID, COALESCE(corporationID, 0)
			  FROM character_ WHERE characterID IN `+where, args...)
		if qerr != nil {
			return fmt.Errorf("outlooksFor: %w", qerr)
		}
		defer rows.Close()
		for rows.Next() {
			var charID, corpID int64
			if serr := rows.Scan(&charID, &corpID); serr != nil {
				return fmt.Errorf("outlooksFor scan: %w", serr)
			}
			corpOf[charID] = corpID
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	byCorp := map[int64][]Standing{}
	out := make(map[int64][]Standing, len(corpOf))
	for charID, corpID := range corpOf {
		list, ok := byCorp[corpID]
		if !ok {
			list, err = m.employerOutlook(ctx, corpID)
			if err != nil {
				return nil, err
			}
			byCorp[corpID] = list
		}
		out[charID] = list
	}
	return out, nil
}

// employerIdentity names the corporation a pilot works for and the faction
// behind it, for the profile page. An empty faction name is a player
// corporation, which is a different sentence rather than a missing one.
func (m *Mod) employerIdentity(ctx context.Context, charID int64) (corpID int64, corpName, factionName string) {
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(c.corporationID, 0), COALESCE(co.corporationName, ''),
		       COALESCE(f.factionName, '')
		  FROM character_ c
		  LEFT JOIN corporation co ON co.corporationID = c.corporationID
		  LEFT JOIN crpNPCCorporations n ON n.corporationID = c.corporationID
		  LEFT JOIN chrFactions f ON f.factionID = n.factionID
		 WHERE c.characterID = ?`, charID).Scan(&corpID, &corpName, &factionName); err != nil {
		return 0, "", ""
	}
	if corpName == "" && corpID != 0 {
		corpName = fmt.Sprintf("corporation %d", corpID)
	}
	return corpID, corpName, factionName
}

// npcCorpOutlook is an NPC employer's politics: every entry in its faction's row
// of the static matrix, plus the faction itself at +10. The matrix comes through
// standingsMatrix, which refuses to cache a failed load, so an unreadable row
// returns an error: an empty outlook would un-fence every station.
func (m *Mod) npcCorpOutlook(ctx context.Context, factionID int64) ([]Standing, error) {
	mx, ok := m.standingsMatrix(ctx)
	if !ok {
		return nil, fmt.Errorf("npcCorpOutlook: faction standings unavailable")
	}
	out := []Standing{{ToID: factionID, Kind: standingFaction, Value: 10}}
	for pair, v := range mx {
		if pair[0] != factionID || pair[1] == factionID || v == 0 {
			continue
		}
		out = append(out, Standing{ToID: pair[1], Kind: standingFaction, Value: v})
	}
	return out, nil
}

// mergeOutlook is own, then every entry of fill whose ToID own does not
// address: a corporation's declared standings outrank its pledged faction's row.
func mergeOutlook(own, fill []Standing) []Standing {
	if len(fill) == 0 {
		return own
	}
	seen := make(map[int64]bool, len(own))
	for _, s := range own {
		seen[s.ToID] = true
	}
	out := append([]Standing(nil), own...)
	for _, s := range fill {
		if !seen[s.ToID] {
			out = append(out, s)
		}
	}
	return out
}

// playerCorpOutlook is a player employer's politics: what its directors have
// declared. The corporation's own crpStandings rows (already on the +-10 scale)
// come first; the alliance's relationships only fill in entities the
// corporation has not addressed. Empty is the normal case.
func (m *Mod) playerCorpOutlook(ctx context.Context, corpID int64) ([]Standing, error) {
	seen := map[int64]bool{}
	var out []Standing

	rows, err := m.db.QueryContext(ctx,
		`SELECT toID, standing FROM crpStandings WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("playerCorpOutlook: %w", err)
	}
	for rows.Next() {
		var toID int64
		var v float64
		if err := rows.Scan(&toID, &v); err != nil {
			rows.Close()
			return nil, fmt.Errorf("playerCorpOutlook: %w", err)
		}
		if v == 0 {
			continue
		}
		seen[toID] = true
		out = append(out, Standing{ToID: toID, Kind: standingFaction, Value: clampStanding(v)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("playerCorpOutlook: %w", err)
	}

	var allianceID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(allianceID, 0) FROM corporation WHERE corporationID = ?`,
		corpID).Scan(&allianceID); err != nil || allianceID == 0 {
		return out, nil
	}
	rows, err = m.db.QueryContext(ctx,
		`SELECT toID, relationship FROM allianceRelationships WHERE allianceID = ?`, allianceID)
	if err != nil {
		// The table is created by the core's EnsureAllianceTables and may not exist yet;
		// that is no reason to fail the corporation's own declared standings.
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var toID, rel int64
		if err := rows.Scan(&toID, &rel); err != nil {
			return nil, fmt.Errorf("playerCorpOutlook alliance: %w", err)
		}
		if seen[toID] {
			continue
		}
		if v := allianceRelStanding(rel); v != 0 {
			out = append(out, Standing{ToID: toID, Kind: standingFaction, Value: v})
		}
	}
	return out, rows.Err()
}

func init() { registerCorpForget("employerOutlookCache", forgetEmployerOutlook) }
