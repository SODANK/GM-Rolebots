package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// A player corporation's faction allegiance: the side its members fly for.
// Every reader of a bot's side (faction.go, sites.go, standings.go, the core's
// rats) takes the employer's faction row; a player corporation has none and
// reads as faction 0, hostile to every rat and site. The allegiance is that
// number, kept in rolebot_corps.allegiance and read wherever the NPC row would
// be (the COALESCE in sim.go and embody.go, corpFactionID, the outlook merge,
// npc.StandingsMap through SetCorpFaction). Derived once, at founding or by
// the boot backfill, never rewritten by a CEO; the operator changes it on the
// corporation page. docs/ALLEGIANCE.md has the rule and its consequences.

// factionNames is what deriveAllegiance needs from chrFactions, resolved once
// by the Mod and built by hand in tests. No faction ID is written in the code.
type factionNames struct {
	EmpireByRace map[int64]int64  // raceEmpireFaction resolved
	PirateByRace map[int64]int64  // racePirateFaction resolved
	EmpireBloc   map[int64]bool   // empireBlocFactions rows a corporation may pledge to
	Name         map[int64]string // every row, for labels
}

// deriveAllegiance is the whole rule. Jove (raceJove, skills.go): none. An
// outlaw-tagged corporation whose CEO's Loyalty sits on the mercenary side
// (traitScale >= 0, zero included) is a true pirate and pledges to its race's
// native pirate faction; a devoted outlaw is a privateer and, like every lawful
// or mercenary-tagged corporation, pledges to the HQ's sovereign when that is a
// pledgeable empire-bloc faction (Khanid, Ammatar, CONCORD, an empire; never
// the Jove), else to its race's empire. A race the maps lack is unaligned.
func deriveAllegiance(raceID int64, tags []string, loyalty int, hqSovFactionID int64, names factionNames) int64 {
	if raceID == raceJove {
		return 0
	}
	if hasTag(tags, tagOutlaw) && posPole(-traitScale(loyalty)) == 0 {
		return names.PirateByRace[raceID]
	}
	if hqSovFactionID != 0 && names.EmpireBloc[hqSovFactionID] {
		return hqSovFactionID
	}
	return names.EmpireByRace[raceID]
}

// allegianceKind names the branch deriveAllegiance took, for the log line.
func allegianceKind(tags []string, loyalty int) string {
	switch {
	case !hasTag(tags, tagOutlaw):
		return "lawful"
	case posPole(-traitScale(loyalty)) == 0:
		return "pirate"
	default:
		return "privateer"
	}
}

var (
	factionNamesMu  sync.Mutex
	factionNamesVal *factionNames // nil until the first successful load
)

// loadFactionNames resolves the maps from one read of chrFactions, matched
// through normFactionName (the stored Blood Raider name carries an article).
// Static data, memoised for the process; a failed load is not cached.
func (m *Mod) loadFactionNames(ctx context.Context) (factionNames, error) {
	factionNamesMu.Lock()
	defer factionNamesMu.Unlock()
	if factionNamesVal != nil {
		return *factionNamesVal, nil
	}
	rows, err := m.db.QueryContext(ctx, `SELECT factionID, factionName FROM chrFactions`)
	if err != nil {
		return factionNames{}, fmt.Errorf("loadFactionNames: %w", err)
	}
	defer rows.Close()
	fn := factionNames{
		EmpireByRace: map[int64]int64{}, PirateByRace: map[int64]int64{},
		EmpireBloc: map[int64]bool{}, Name: map[int64]string{},
	}
	byNorm := map[string]int64{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return factionNames{}, fmt.Errorf("loadFactionNames: %w", err)
		}
		fn.Name[id] = name
		byNorm[normFactionName(name)] = id
		if pledgeableEmpire(name) {
			fn.EmpireBloc[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return factionNames{}, fmt.Errorf("loadFactionNames: %w", err)
	}
	if len(fn.Name) == 0 {
		return factionNames{}, errors.New("loadFactionNames: chrFactions is empty")
	}
	for race, name := range raceEmpireFaction {
		if id, ok := byNorm[normFactionName(name)]; ok {
			fn.EmpireByRace[race] = id
		}
	}
	for race, name := range racePirateFaction {
		if id, ok := byNorm[normFactionName(name)]; ok {
			fn.PirateByRace[race] = id
		}
	}
	factionNamesVal = &fn
	return fn, nil
}

// pledgeableEmpire is whether an empire-bloc faction may be pledged to. CONCORD
// is: a corporation headquartered in Yulai serves the Assembly, and the matrix
// has CONCORD at peace with every empire, so its loyalists spare them all. The
// Jove are not: no Jove empire takes loyalists, and race 16 is unaligned too.
func pledgeableEmpire(name string) bool {
	return empireBlocFactions[normFactionName(name)] && normFactionName(name) != "Jove Empire"
}

// factionLabel is a faction's name for a page or a log line; 0 is "unaligned".
func (m *Mod) factionLabel(ctx context.Context, factionID int64) string {
	if factionID == 0 {
		return "unaligned"
	}
	if fn, err := m.loadFactionNames(ctx); err == nil {
		if name := fn.Name[factionID]; name != "" {
			return name
		}
	}
	return fmt.Sprintf("faction %d", factionID)
}

// deriveCorpAllegiance gathers one corporation's inputs and runs the rule:
// raceID (the founder's when given, else corporation.raceID), the tags (the
// founder's charter when given, else the policy row's), the CEO's Loyalty
// trait, and the HQ station's system sovereign. It returns the faction and
// the branch name.
func (m *Mod) deriveCorpAllegiance(ctx context.Context, corpID, raceID int64, tags []string, traits map[string]int) (int64, string, error) {
	if raceID == 0 {
		if err := m.db.QueryRowContext(ctx,
			`SELECT COALESCE(raceID, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&raceID); err != nil {
			return 0, "", fmt.Errorf("deriveCorpAllegiance race: %w", err)
		}
	}
	if tags == nil {
		var tagCSV string
		if err := m.db.QueryRowContext(ctx,
			`SELECT COALESCE(tags, '') FROM rolebot_corps WHERE corporationID = ?`, corpID).Scan(&tagCSV); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, "", fmt.Errorf("deriveCorpAllegiance tags: %w", err)
		}
		tags = parseTagList(tagCSV)
	}
	var hqSystem int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(s.solarSystemID, 0)
		  FROM corporation c
		  LEFT JOIN staStations s ON s.stationID = c.stationID
		 WHERE c.corporationID = ?`, corpID).Scan(&hqSystem); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, "", fmt.Errorf("deriveCorpAllegiance HQ: %w", err)
	}
	var sov int64
	if hqSystem != 0 {
		if u, err := m.universe(ctx); err == nil {
			sov = u.sovFaction(ctx, m.db, hqSystem)
		}
	}
	names, err := m.loadFactionNames(ctx)
	if err != nil {
		return 0, "", err
	}
	loyalty := traits[TraitLoyalty]
	return deriveAllegiance(raceID, tags, loyalty, sov, names), allegianceKind(tags, loyalty), nil
}

// setCorpAllegiance writes a player corporation's allegiance (0 = unaligned),
// drops the memos that read it and pushes it into the NPCManager. An NPC
// corporation is refused: its faction is its row, and a 0 here would delete
// the core's mapping for it.
func (m *Mod) setCorpAllegiance(ctx context.Context, corpID, factionID int64) error {
	if corpID == 0 {
		return errors.New("setCorpAllegiance: no corporation")
	}
	if m.npcCorpFactionID(ctx, corpID) != 0 {
		return fmt.Errorf("corporation %d is an NPC corporation; its faction is not a pledge", corpID)
	}
	now := clock.Unix()
	res, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_corps SET allegiance = ?, updatedAt = ? WHERE corporationID = ?`,
		factionID, now, corpID)
	if err != nil {
		return fmt.Errorf("setCorpAllegiance: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corps (corporationID, allegiance, seeded, updatedAt) VALUES (?, ?, 0, ?)
			ON DUPLICATE KEY UPDATE allegiance = VALUES(allegiance), updatedAt = VALUES(updatedAt)`,
			corpID, factionID, now); err != nil {
			return fmt.Errorf("setCorpAllegiance: %w", err)
		}
	}
	forgetCorpFaction(corpID)
	forgetEmployerOutlook(corpID)
	forgetCorpCatalogue()
	m.pushCorpAllegiance(corpID, factionID)
	return nil
}

// pushCorpAllegiance hands one allegiance to the core's corp-to-faction map
// (core/npc/corpfaction_mod.go), so its rats spare the corporation's members
// and the embodied tier reads their side. A nil manager or map (the harness)
// is a no-op.
func (m *Mod) pushCorpAllegiance(corpID, factionID int64) {
	if m.npc == nil || m.npc.Standings == nil {
		return
	}
	m.npc.Standings.SetCorpFaction(corpID, factionID)
}

// pushAllAllegiances loads every pledged corporation into the NPCManager at
// boot. Only player corporations hold a nonzero value (setCorpAllegiance
// refuses NPC ones), so the table is read alone.
func (m *Mod) pushAllAllegiances(ctx context.Context) error {
	if m.npc == nil || m.npc.Standings == nil {
		return nil
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT corporationID, allegiance FROM rolebot_corps WHERE allegiance <> 0`)
	if err != nil {
		return fmt.Errorf("pushAllAllegiances: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var corpID, fid int64
		if err := rows.Scan(&corpID, &fid); err != nil {
			return fmt.Errorf("pushAllAllegiances: %w", err)
		}
		m.npc.Standings.SetCorpFaction(corpID, fid)
		n++
	}
	if n > 0 {
		rlogf("rolebots: %d corporation allegiance(s) declared to the NPC manager", n)
	}
	return rows.Err()
}

// allegianceSeedMarker is the one-shot backfill's rolebot_traits marker. The
// trait column is VARCHAR(16): a longer name is truncated on write and never
// matches the probe, so the backfill would run on every boot.
const allegianceSeedMarker = "_allegseed"

// migrateCorpAllegiance pledges every bot-run player corporation that has no
// allegiance yet, once, from its sitting CEO's race, tags and Loyalty. Behind
// a trait marker; a schema without the game's corporation table (scratch) is
// skipped unmarked for a later boot.
func (m *Mod) migrateCorpAllegiance(ctx context.Context) error {
	if done, err := traitMarkerSet(ctx, m.db, allegianceSeedMarker); err != nil {
		return fmt.Errorf("migrateCorpAllegiance probe: %w", err)
	} else if done {
		return nil
	}
	if ok, err := haveColumn(ctx, m.db, "corporation", "stationID"); err != nil || !ok {
		return err
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT ce.corporationID, ce.characterID, co.corporationName, COALESCE(co.raceID, 0)
		  FROM rolebot_ceos ce
		  JOIN corporation co ON co.corporationID = ce.corporationID
		  LEFT JOIN crpNPCCorporations n ON n.corporationID = ce.corporationID
		  LEFT JOIN rolebot_corps rc ON rc.corporationID = ce.corporationID
		 WHERE n.corporationID IS NULL AND COALESCE(rc.allegiance, 0) = 0`)
	if err != nil {
		return fmt.Errorf("migrateCorpAllegiance read: %w", err)
	}
	type corpRow struct {
		corpID, ceoID, raceID int64
		name                  string
	}
	var corps []corpRow
	for rows.Next() {
		var r corpRow
		if err := rows.Scan(&r.corpID, &r.ceoID, &r.name, &r.raceID); err != nil {
			rows.Close()
			return fmt.Errorf("migrateCorpAllegiance scan: %w", err)
		}
		corps = append(corps, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	pledged, unaligned := 0, 0
	for _, r := range corps {
		traits, err := getTraits(ctx, m.db, r.ceoID)
		if err != nil {
			return err
		}
		fid, kind, err := m.deriveCorpAllegiance(ctx, r.corpID, r.raceID, nil, traits)
		if err != nil {
			return err
		}
		if fid == 0 {
			unaligned++
			continue
		}
		if err := m.setCorpAllegiance(ctx, r.corpID, fid); err != nil {
			return err
		}
		pledged++
		m.notePledge(ctx, r.corpID, r.ceoID, r.name, fid, kind)
	}
	rlogf("rolebots: corporation allegiance backfill: %d pledged, %d unaligned", pledged, unaligned)
	// The first deployment wrote an 18-character marker into the 16-wide column; the
	// truncated row never matched a probe and is swept here.
	m.db.ExecContext(ctx, `DELETE FROM rolebot_traits WHERE characterID = 0 AND trait = 'corpAllegianceSe'`)
	return setTraitMarker(ctx, m.db, allegianceSeedMarker)
}

// notePledge is the log line and the CEO ledger entry for a pledge, shared by
// founding, the backfill and the operator's write. charID 0 writes no pilot
// line; a corporation with no bot CEO gets no ledger entry.
func (m *Mod) notePledge(ctx context.Context, corpID, charID int64, corpName string, factionID int64, kind string) {
	label := m.factionLabel(ctx, factionID)
	if charID != 0 {
		if factionID == 0 {
			m.logBot(charID, logInfo, "corp", "%s renounced its allegiance", corpName)
		} else {
			m.logBot(charID, logInfo, "corp", "pledged %s to %s (%s)", corpName, label, kind)
		}
	}
	if c, err := getCEO(ctx, m.db, corpID); err == nil && c != nil {
		m.ceoLog(ctx, c, powerIdentity, "pledge", charID, 0, "%s flies for %s (%s)", corpName, label, kind)
	}
}

// allegianceChoices lists the factions a corporation may pledge to on the
// corporation page: the empire bloc minus CONCORD and the Jove, plus the
// outlaw bloc, by name.
func (m *Mod) allegianceChoices(ctx context.Context) ([]namedID, error) {
	fn, err := m.loadFactionNames(ctx)
	if err != nil {
		return nil, err
	}
	var out []namedID
	for id, name := range fn.Name {
		if pledgeableEmpire(name) || outlawBlocFactions[normFactionName(name)] {
			out = append(out, namedID{ID: id, Name: name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
