package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Player corporations: the second half of the corporation catalogue.
// rolebot_corps is keyed on corporationID and holds nothing NPC-specific, so a player
// corporation shares the NPC policy table, scorer and hard rules. What differs:
//   - FactionID is 0, so the race, tribe, pirate and standing rules fall through.
//   - There is no activity ID, so the operator types its tags.
//   - It owns no stations: its address is its headquarters (corporation.stationID) plus
//     its rented offices (crpoffices), as corpAnchorStations says.
// memberLimit and allowedMemberRaceIDs are the corporation's own recruitment settings and
// are hard rules. Any-race recruiting needs the CEO's Ethnic Relations skill (typeID 3368).

// playerCorpRefusal applies the gates a player corporation carries and an NPC one does not.
// Nil means no gate refused and corpEligibility still decides. It is pure (facts in, a
// verdict out), so it can be tested without a database.
func playerCorpRefusal(p pilotIdentity, c CorpFacts) *corpVerdict {
	// The member limit is counted against pilots on the books, not corporation.memberCount
	// (stored, never maintained). A pilot already inside is one of those counted, so it is not refused.
	inside := p.CorpID != 0 && p.CorpID == c.CorpID
	if !inside && c.MemberLimit > 0 && c.Members >= c.MemberLimit {
		return &corpVerdict{Rule: "full", Why: fmt.Sprintf(
			"%s is at its member limit — %d of %d members",
			c.Name, c.Members, c.MemberLimit)}
	}
	// The queue is the door a full desk shuts: applicants past the openings and the waiting list
	// can only be turned down (corpceo_hr.go triageDesk reads the same hrWaitlistSize).
	if !inside && c.MemberLimit > 0 && c.Pending > 0 &&
		c.Members+c.Pending >= c.MemberLimit+hrWaitlistSize(c.MemberLimit) {
		return &corpVerdict{Rule: "queue", Why: fmt.Sprintf(
			"%s already has %d applicants waiting for %d open places",
			c.Name, c.Pending, max(0, c.MemberLimit-c.Members))}
	}
	// allowedMemberRaceIDs is a bitmask and the playable raceIDs (1 Caldari, 2 Minmatar,
	// 4 Amarr, 8 Gallente) are powers of two, so the mask is its own test. Zero means no filter.
	if c.AllowedRaceMask != 0 && p.RaceID != 0 && c.AllowedRaceMask&p.RaceID == 0 {
		return &corpVerdict{Rule: "race", Why: fmt.Sprintf(
			"%s recruits from selected races only and this pilot is not one of them", c.Name)}
	}
	// Without Ethnic Relations the corporation recruits its founder's race only, as a state's
	// does. A corporation with raceID 0 is not gated here; the mask above decides.
	if c.CEOEthnicRelations < ethnicRelationsMinLevel &&
		c.RaceID != 0 && p.RaceID != 0 && p.RaceID != c.RaceID {
		return &corpVerdict{Rule: "race", Why: fmt.Sprintf(
			"%s is a %s corporation and its CEO has not trained Ethnic Relations, so it cannot recruit %s",
			c.Name, raceName(c.RaceID), raceName(p.RaceID))}
	}
	return nil
}

// ethnicRelationsMinLevel is the trained level that opens the door. A
// skillbook sitting at level 0 is a book, not a qualification.
const ethnicRelationsMinLevel = 1

// ── reading them ────────────────────────────────────────────────────────────

// playerCorpFactsQuery lists every corporation with no crpNPCCorporations row (not an ID
// band), with its policy row and the same three counts as the NPC catalogue.
// The counts are derived tables, as in corpFactsQuery, not a subquery per row.
const playerCorpFactsQuery = `
	SELECT c.corporationID, c.corporationName, c.tickerName,
	       COALESCE(c.raceID, 0), COALESCE(c.ceoID, 0), COALESCE(ceo.characterName, ''),
	       COALESCE(c.allianceID, 0), COALESCE(al.allianceName, ''),
	       COALESCE(c.memberLimit, 0), COALESCE(c.taxRate, 0),
	       COALESCE(c.allowedMemberRaceIDs, 0), COALESCE(er.level, 0),
	       COALESCE(c.stationID, 0), COALESCE(sta.stationName, ''),
	       COALESCE(sta.solarSystemID, 0), COALESCE(ms.solarSystemName, ''), COALESCE(mr.regionName, ''),
	       COALESCE(off.offices, 0), COALESCE(mem.members, 0), COALESCE(rb.bots, 0),
	       COALESCE(ap.pending, 0),
	       COALESCE(p.joinable, 0), COALESCE(p.restricted, 0), COALESCE(p.tags, ''),
	       COALESCE(p.raceRule, ''), COALESCE(p.minStanding, 0), COALESCE(p.weight, 1),
	       COALESCE(p.notes, ''), COALESCE(p.allegiance, 0), COALESCE(p.updatedAt, 0)
	FROM corporation c
	LEFT JOIN crpNPCCorporations n ON n.corporationID = c.corporationID
	LEFT JOIN character_ ceo ON ceo.characterID = c.ceoID
	LEFT JOIN alliances al ON al.allianceID = c.allianceID
	LEFT JOIN staStations sta ON sta.stationID = c.stationID
	LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = sta.solarSystemID
	LEFT JOIN mapRegions mr ON mr.regionID = ms.regionID
	LEFT JOIN (SELECT e.ownerID, MAX(COALESCE(a.valueInt, 0)) AS level
	             FROM entity e
	             LEFT JOIN entity_attributes a
	                    ON a.itemID = e.itemID AND a.attributeID = 280
	            WHERE e.typeID = 3368 AND e.flag IN (7, 61)
	            GROUP BY e.ownerID) er
	       ON er.ownerID = c.ceoID
	LEFT JOIN (SELECT corporationID, COUNT(*) AS offices FROM crpoffices GROUP BY corporationID) off
	       ON off.corporationID = c.corporationID
	LEFT JOIN (SELECT corporationID, COUNT(*) AS members FROM character_ WHERE accountID > 0 GROUP BY corporationID) mem
	       ON mem.corporationID = c.corporationID
	LEFT JOIN (SELECT ch.corporationID, COUNT(*) AS bots
	             FROM rolebot_characters r JOIN character_ ch ON ch.characterID = r.characterID
	            GROUP BY ch.corporationID) rb
	       ON rb.corporationID = c.corporationID
	LEFT JOIN (SELECT corporationID, COUNT(*) AS pending
	             FROM rolebot_corp_applications WHERE state = 'pending'
	            GROUP BY corporationID) ap
	       ON ap.corporationID = c.corporationID
	LEFT JOIN rolebot_corps p ON p.corporationID = c.corporationID
	WHERE n.corporationID IS NULL AND c.deleted = 0`

// playerCorpFactsOneQuery is playerCorpFactsQuery for one corporation (the argument): the same
// columns and values, with each count a subquery on that corporation alone. The derived tables
// group every character and office on the server before the join keeps one row, which is right
// for the catalogue and is most of the database's time when a pass asks about corporations one
// by one. Each subquery here is an index lookup (idx_char_corp, crpoffices' key, idx_rbapp_corp,
// idx_entity_owner_type).
const playerCorpFactsOneQuery = `
	SELECT c.corporationID, c.corporationName, c.tickerName,
	       COALESCE(c.raceID, 0), COALESCE(c.ceoID, 0), COALESCE(ceo.characterName, ''),
	       COALESCE(c.allianceID, 0), COALESCE(al.allianceName, ''),
	       COALESCE(c.memberLimit, 0), COALESCE(c.taxRate, 0),
	       COALESCE(c.allowedMemberRaceIDs, 0),
	       COALESCE((SELECT MAX(COALESCE(a.valueInt, 0))
	                   FROM entity e
	                   LEFT JOIN entity_attributes a
	                          ON a.itemID = e.itemID AND a.attributeID = 280
	                  WHERE e.ownerID = c.ceoID AND e.typeID = 3368 AND e.flag IN (7, 61)), 0),
	       COALESCE(c.stationID, 0), COALESCE(sta.stationName, ''),
	       COALESCE(sta.solarSystemID, 0), COALESCE(ms.solarSystemName, ''), COALESCE(mr.regionName, ''),
	       (SELECT COUNT(*) FROM crpoffices o WHERE o.corporationID = c.corporationID),
	       (SELECT COUNT(*) FROM character_ m WHERE m.corporationID = c.corporationID AND m.accountID > 0),
	       (SELECT COUNT(*) FROM rolebot_characters r JOIN character_ ch ON ch.characterID = r.characterID
	         WHERE ch.corporationID = c.corporationID),
	       (SELECT COUNT(*) FROM rolebot_corp_applications ap
	         WHERE ap.corporationID = c.corporationID AND ap.state = 'pending'),
	       COALESCE(p.joinable, 0), COALESCE(p.restricted, 0), COALESCE(p.tags, ''),
	       COALESCE(p.raceRule, ''), COALESCE(p.minStanding, 0), COALESCE(p.weight, 1),
	       COALESCE(p.notes, ''), COALESCE(p.allegiance, 0), COALESCE(p.updatedAt, 0)
	FROM corporation c
	LEFT JOIN crpNPCCorporations n ON n.corporationID = c.corporationID
	LEFT JOIN character_ ceo ON ceo.characterID = c.ceoID
	LEFT JOIN alliances al ON al.allianceID = c.allianceID
	LEFT JOIN staStations sta ON sta.stationID = c.stationID
	LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = sta.solarSystemID
	LEFT JOIN mapRegions mr ON mr.regionID = ms.regionID
	LEFT JOIN rolebot_corps p ON p.corporationID = c.corporationID
	WHERE n.corporationID IS NULL AND c.deleted = 0 AND c.corporationID = ?`

func scanPlayerCorpFacts(rows *sql.Rows) ([]CorpFacts, error) {
	var out []CorpFacts
	for rows.Next() {
		var c CorpFacts
		var joinable, restricted int64
		var tags string
		if err := rows.Scan(&c.CorpID, &c.Name, &c.Ticker,
			&c.RaceID, &c.CEOID, &c.CEOName,
			&c.AllianceID, &c.AllianceName,
			&c.MemberLimit, &c.TaxRate, &c.AllowedRaceMask, &c.CEOEthnicRelations,
			&c.StationID, &c.StationName, &c.SystemID, &c.SystemName, &c.RegionName,
			&c.Offices, &c.Members, &c.Bots, &c.Pending,
			&joinable, &restricted, &tags, &c.Policy.RaceRule,
			&c.Policy.MinStanding, &c.Policy.Weight, &c.Policy.Notes, &c.Policy.Allegiance,
			&c.Policy.UpdatedAt); err != nil {
			return nil, err
		}
		c.Player = true
		c.Policy.CorpID = c.CorpID
		c.Policy.Joinable = joinable != 0
		c.Policy.Restricted = restricted != 0
		c.Policy.Tags = parseTagList(tags)
		if c.Policy.Weight <= 0 {
			c.Policy.Weight = 1
		}
		c.DeskBar = corpDeskBar(c.CorpID)
		out = append(out, c)
	}
	return out, rows.Err()
}

// listPlayerCorpFacts reads every player corporation, for the catalogue page.
// No tribe resolution: a tribe is a Minmatar people and a player corporation
// is a company, whatever it called itself.
func listPlayerCorpFacts(ctx context.Context, db *sql.DB) ([]CorpFacts, error) {
	rows, err := db.QueryContext(ctx, playerCorpFactsQuery+`
		ORDER BY c.corporationName`)
	if err != nil {
		return nil, fmt.Errorf("listPlayerCorpFacts: %w", err)
	}
	defer rows.Close()
	return scanPlayerCorpFacts(rows)
}

// listOpenPlayerCorpFacts is the ranking set: the player corporations an operator opened.
// A shut one scores zero for every pilot and has no crowding weight, so it is left out.
func listOpenPlayerCorpFacts(ctx context.Context, db *sql.DB) ([]CorpFacts, error) {
	rows, err := db.QueryContext(ctx, playerCorpFactsQuery+`
		  AND p.joinable = 1
		ORDER BY c.corporationName`)
	if err != nil {
		return nil, fmt.Errorf("listOpenPlayerCorpFacts: %w", err)
	}
	defer rows.Close()
	return scanPlayerCorpFacts(rows)
}

func getPlayerCorpFacts(ctx context.Context, db *sql.DB, corpID int64) (*CorpFacts, error) {
	rows, err := db.QueryContext(ctx, playerCorpFactsOneQuery, corpID)
	if err != nil {
		return nil, fmt.Errorf("getPlayerCorpFacts: %w", err)
	}
	defer rows.Close()
	list, err := scanPlayerCorpFacts(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("corporation %d is not a player corporation", corpID)
	}
	return &list[0], nil
}

// getAnyCorpFacts resolves a corporation from either half of the catalogue: the
// corporation page edits the same policy row for both.
func getAnyCorpFacts(ctx context.Context, db *sql.DB, corpID int64) (*CorpFacts, error) {
	// A CEO think or desk pass reads its own corporation's row once
	// (thinkctx.go); the powers share the pointer.
	tc := thinkCacheOf(ctx, corpID)
	if tc != nil {
		tc.mu.Lock()
		f := tc.facts
		tc.mu.Unlock()
		if f != nil {
			return f, nil
		}
	}
	c, err := getCorpFacts(ctx, db, corpID)
	if err != nil {
		if c, err = getPlayerCorpFacts(ctx, db, corpID); err != nil {
			return nil, fmt.Errorf("corporation %d is in neither the NPC catalogue nor the player one", corpID)
		}
	}
	if tc != nil {
		tc.mu.Lock()
		tc.facts = c
		tc.mu.Unlock()
	}
	return c, nil
}

// playerCorpFactsShared is listPlayerCorpFacts shared for a minute: the diplomacy and war
// passes read the whole catalogue per corporation per think. It returns a copy; the
// snapshot is read-only.
func (m *Mod) playerCorpFactsShared(ctx context.Context) ([]CorpFacts, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	shared, err := snapshotFor(snapKey("player-corps"), time.Minute, func() ([]CorpFacts, error) {
		return listPlayerCorpFacts(sctx, m.db)
	})
	if err != nil {
		return nil, err
	}
	out := make([]CorpFacts, len(shared))
	copy(out, shared)
	return out, nil
}

// ── the ranking set ─────────────────────────────────────────────────────────

// corpRankingCatalogue is everything a pilot may be priced against: the whole NPC catalogue
// (shut rows included) plus the open player corporations, through the same scorer.
// It is shared for corpCatalogueTTL. saveCorpPolicy drops it, so an operator edit shows on
// the next read; counts and offices may be up to the TTL old.
func (m *Mod) corpRankingCatalogue(ctx context.Context) ([]CorpFacts, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	shared, err := snapshotFor(snapKey("corp-catalogue"), corpCatalogueTTL, func() ([]CorpFacts, error) {
		corps, err := listCorpFacts(sctx, m.db)
		if err != nil {
			return nil, err
		}
		players, err := listOpenPlayerCorpFacts(sctx, m.db)
		if err != nil {
			return nil, err
		}
		return append(corps, players...), nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]CorpFacts, len(shared))
	copy(out, shared)
	return out, nil
}

// corpCatalogueTTL is how long the ranking catalogue and the corporation
// address map are shared before being read again.
const corpCatalogueTTL = 5 * time.Minute

// forgetCorpCatalogue drops the shared catalogue and address map.
func forgetCorpCatalogue() {
	snapMu.Lock()
	delete(snapCache, snapKey("corp-catalogue"))
	delete(snapCache, snapKey("corp-systems"))
	snapMu.Unlock()
}

// corpSystemsAll maps every corporation in the ranking to the systems it has an address in:
// stations an NPC corporation owns, and the headquarters plus rented offices of a player
// corporation (as corpAnchorStations). Shared per corpCatalogueTTL and read-only once shared.
func (m *Mod) corpSystemsAll(ctx context.Context) (map[int64][]int64, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshotFor(snapKey("corp-systems"), corpCatalogueTTL, func() (map[int64][]int64, error) {
		return m.loadCorpSystemsAll(sctx)
	})
}

func (m *Mod) loadCorpSystemsAll(ctx context.Context) (map[int64][]int64, error) {
	out, err := corpStationSystems(ctx, m.db)
	if err != nil {
		return nil, err
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.corporationID, s.solarSystemID
		  FROM corporation c
		  JOIN staStations s ON s.stationID = c.stationID
		  LEFT JOIN crpNPCCorporations n ON n.corporationID = c.corporationID
		 WHERE n.corporationID IS NULL AND c.deleted = 0
		UNION
		SELECT o.corporationID, s.solarSystemID
		  FROM crpoffices o
		  JOIN staStations s ON s.stationID = o.stationID
		  LEFT JOIN crpNPCCorporations n2 ON n2.corporationID = o.corporationID
		 WHERE n2.corporationID IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("corpSystemsAll: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var corpID, sysID int64
		if err := rows.Scan(&corpID, &sysID); err != nil {
			return nil, err
		}
		out[corpID] = append(out[corpID], sysID)
	}
	return out, rows.Err()
}

// ── display ─────────────────────────────────────────────────────────────────

// TaxPercent is `corporation.taxRate` as a percentage. The column is a fraction, so a raw
// print would read a 20% tax as 0.2%.
func (c CorpFacts) TaxPercent() float64 { return c.TaxRate * 100 }

// playerCorpRaceNames spells an allowedMemberRaceIDs mask out for the page, so an operator
// can read the recruitment filter instead of decoding it.
func playerCorpRaceNames(mask int64) string {
	if mask == 0 {
		return ""
	}
	var out []string
	for _, r := range []struct {
		ID   int64
		Name string
	}{{1, "Caldari"}, {2, "Minmatar"}, {4, "Amarr"}, {8, "Gallente"}} {
		if mask&r.ID != 0 {
			out = append(out, r.Name)
		}
	}
	if len(out) == 0 {
		return fmt.Sprintf("mask %d", mask)
	}
	return strings.Join(out, ", ")
}

func init() { registerCorpForget("corp catalogue snapshot", func(int64) { forgetCorpCatalogue() }) }
