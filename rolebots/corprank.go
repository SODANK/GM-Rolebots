package rolebots

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// The chain of command: fleets as corporation functions.
//
// A corporation fields fleets through pooled fleet-commander seats, one per
// FCSeatDivisor() enabled members (gangtier.go). Each seat gets an activity from
// the corporation's tags, and only a seated commander may start a fleet
// (gangform.go). Leadership decides who gets a seat; career skills only order
// equals. A bot CEO (corpceo_roles.go) or NPC chair (npcchair.go) deals the seats;
// the weekly pass below is the fallback. A manual seat (manual = 1) is never
// second-guessed by the pass.

const (
	// settingPromoteNextAt persists the weekly clock, so a restart neither
	// skips a due pass nor re-runs one that just finished.
	settingPromoteNextAt = "rolebots.promoteNextAt"

	// promoteEverySec: a working week between reviews of the hierarchy.
	promoteEverySec = 7 * 24 * 3600
	// promoteCheckTicks is how often the sweep looks at the clock (ticks are
	// one second). The pass itself runs weekly; the check is just a glance.
	promoteCheckTicks = 600

	// One pooled fleet-commander seat per FCSeatDivisor() members (a setting,
	// gangtier.go): fleets are sized by tier.

	// fcLeadershipWeight makes Leadership dominant in the score: a whole
	// career's worth of support skills sums to well under one level's worth
	// of command. See fcScore.
	fcLeadershipWeight = 1000.0
)

var (
	promoteBusy   atomic.Bool
	promoteNextAt atomic.Int64
)

// loadPromoteClock reads the weekly clock at boot. A missing value runs a pass at
// the first sweep check, so a fresh install starts with a hierarchy.
func (m *Mod) loadPromoteClock(ctx context.Context) {
	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingPromoteNextAt)
	if err != nil {
		rlogf("rolebots: read promotion clock: %v", err)
		return
	}
	if ok {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			promoteNextAt.Store(n)
		}
	}
	// Seats dealt under a different divisor are re-dealt now (gangtier.go), not at the
	// end of the week.
	m.loadGangTierSettings(ctx)
	if m.fcDivisorChanged(ctx) {
		rlogf("rolebots: fleet-commander seat divisor is now %d — running the promotion pass early", FCSeatDivisor())
		promoteNextAt.Store(0)
	}
}

// corpFleetActivities maps a corporation's tags to the fleet activities its
// hierarchy organizes: a mining op for mining, industry and shipbuilding tags, and
// a combat wing for any corporation with people in space (doctrine,
// corpdoctrine.go). No trade fleet: trade runs through corptrade.go.
func corpFleetActivities(p CorpPolicy) []string {
	var out []string
	// One mining seat, the co-op (fleetore.go): miners fly together for the shared
	// hauler and the guns and each keeps its own rock. ActIndMining is not dealt.
	if p.HasTag("mining") || p.HasTag("industry") || p.HasTag("shipbuilding") {
		out = append(out, ActMining)
	}
	// Combat seats follow the doctrine (corpdoctrine.go). A defence corporation seats
	// ActSecurity only; security is appended last because fleetSeatRotation deals in order.
	switch doctrineFromTags(p.Tags) {
	case DoctrinePredation:
		// One kind of combat seat: formFleetAround reads PiracyAppetite to form a raid or
		// a hunting pack. ActPiracy is not dealt (see migratePirateSeats).
		out = append(out, ActCombat)
	case DoctrineMercenary:
		out = append(out, ActCombat)
	case DoctrineSecurity:
		out = append(out, ActCombat, ActSecurity)
	case DoctrineDefence:
		out = append(out, ActSecurity)
	}
	return out
}

// fleetSeatRotation is the order one corporation's seats are dealt in, one turn
// of the wheel per pass. It is corpFleetActivities with ActMining entered twice,
// so the belts are crewed ahead of the corporation's guard.

func fleetSeatRotation(activities []string) []string {
	out := make([]string, 0, len(activities)+1)
	for _, act := range activities {
		out = append(out, act)
		if act == ActMining {
			out = append(out, act)
		}
	}
	return out
}

// settingSecuritySeats marks that the ActCombat to ActSecurity re-slug has run.
const settingSecuritySeats = "rolebots.securityActivityMigrated"

// migrateSecuritySeats moves ActCombat seats of defence-doctrine corporations to
// ActSecurity, once (settingSecuritySeats). Only the activity changes: the pilot
// keeps the seat, its manual flag and appointment. Live rolebot_gangs rows are left
// alone, since a fleet mid-op is not re-slugged under its members.
func (m *Mod) migrateSecuritySeats(ctx context.Context) error {
	have, _, err := evedb.GetServerSetting(ctx, m.db, settingSecuritySeats)
	if err != nil {
		return fmt.Errorf("migrateSecuritySeats: %w", err)
	}
	if have == "1" {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT k.corporationID, COALESCE(c.tags, '')
		FROM rolebot_corp_ranks k
		JOIN rolebot_corps c ON c.corporationID = k.corporationID
		WHERE k.activity = ?`, ActCombat)
	if err != nil {
		return fmt.Errorf("migrateSecuritySeats scan: %w", err)
	}
	var defence []int64
	for rows.Next() {
		var corpID int64
		var raw string
		if err := rows.Scan(&corpID, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("migrateSecuritySeats scan: %w", err)
		}
		if doctrineFromTags(parseTagList(raw)) == DoctrineDefence {
			defence = append(defence, corpID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("migrateSecuritySeats scan: %w", err)
	}
	rows.Close()

	moved := 0
	for _, corpID := range defence {
		res, err := m.db.ExecContext(ctx,
			`UPDATE rolebot_corp_ranks SET activity = ? WHERE corporationID = ? AND activity = ?`,
			ActSecurity, corpID, ActCombat)
		if err != nil {
			return fmt.Errorf("migrateSecuritySeats %d: %w", corpID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			moved += int(n)
		}
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingSecuritySeats, "1"); err != nil {
		return fmt.Errorf("migrateSecuritySeats marker: %w", err)
	}
	if moved > 0 {
		rlogf("rolebots: %d fleet commander seat(s) in %d industrial corporation(s) moved from bounty to security — their fleets now work home ground only",
			moved, len(defence))
	}
	return nil
}

// settingPirateSeats marks that the ActPiracy to ActCombat seat re-slug has run.
const settingPirateSeats = "rolebots.pirateSeatsFolded"

// migratePirateSeats moves ActPiracy seats to ActCombat, once (settingPirateSeats).
// Only the activity changes; formFleetAround decides raid or hunting pack from the
// commander. Live fleets are left to drain, as migrateSecuritySeats leaves them.
func (m *Mod) migratePirateSeats(ctx context.Context) error {
	have, _, err := evedb.GetServerSetting(ctx, m.db, settingPirateSeats)
	if err != nil {
		return fmt.Errorf("migratePirateSeats: %w", err)
	}
	if have == "1" {
		return nil
	}
	res, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_corp_ranks SET activity = ? WHERE activity = ?`, ActCombat, ActPiracy)
	if err != nil {
		return fmt.Errorf("migratePirateSeats: %w", err)
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingPirateSeats, "1"); err != nil {
		return fmt.Errorf("migratePirateSeats marker: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		rlogf("rolebots: %d piracy commander seat(s) re-slugged as combat - the raid is a way a combat fleet can go now", n)
	}
	return nil
}

// migrateIndMiningSeats moves ActIndMining seats to ActMining, once
// (settingIndMiningSeats), since formFleetAround refuses ActIndMining. Only the
// activity changes. Live fleets are left to drain.
func (m *Mod) migrateIndMiningSeats(ctx context.Context) error {
	have, _, err := evedb.GetServerSetting(ctx, m.db, settingIndMiningSeats)
	if err != nil {
		return fmt.Errorf("migrateIndMiningSeats: %w", err)
	}
	if have == "1" {
		return nil
	}
	res, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_corp_ranks SET activity = ? WHERE activity = ?`, ActMining, ActIndMining)
	if err != nil {
		return fmt.Errorf("migrateIndMiningSeats: %w", err)
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingIndMiningSeats, "1"); err != nil {
		return fmt.Errorf("migrateIndMiningSeats marker: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		rlogf("rolebots: %d industry-mining commander seat(s) re-slugged as co-op mining — the buy-everything fleet is retired", n)
	}
	return nil
}

// settingTradeConvoys marks that the trade convoy retirement has run.
const settingTradeConvoys = "rolebots.tradeConvoysRetired"

// migrateTradeConvoys retires the trade convoy, once (settingTradeConvoys): seats
// move to ActSecurity, live ActTrade fleets are disbanded with their members set
// idle, the charter ledger table is dropped, and the Convoy Master title slot is
// reset (name, member bits, title text) because syncCorpTitles walks titleSlots.
func (m *Mod) migrateTradeConvoys(ctx context.Context) error {
	have, _, err := evedb.GetServerSetting(ctx, m.db, settingTradeConvoys)
	if err != nil {
		return fmt.Errorf("migrateTradeConvoys: %w", err)
	}
	if have == "1" {
		return nil
	}
	res, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_corp_ranks SET activity = ? WHERE activity = ?`, ActSecurity, ActTrade)
	if err != nil {
		return fmt.Errorf("migrateTradeConvoys seats: %w", err)
	}
	seats, _ := res.RowsAffected()

	// Live convoys: members home, gang gone.
	var gangIDs []int64
	rows, err := m.db.QueryContext(ctx, `SELECT gangID FROM rolebot_gangs WHERE activity = ?`, ActTrade)
	if err != nil {
		return fmt.Errorf("migrateTradeConvoys gangs: %w", err)
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			gangIDs = append(gangIDs, id)
		}
	}
	rows.Close()
	freed := 0
	for _, gangID := range gangIDs {
		members, err := gangMembers(ctx, m.db, gangID)
		if err != nil {
			return err
		}
		for _, mem := range members {
			st, err := loadState(ctx, m.db, mem.CharacterID)
			if err != nil {
				continue
			}
			st.Activity, st.Phase = "", string(TaskIdle)
			st.Data = idleData(st.Data)
			st.Data.GangID, st.Data.GangBeltID = 0, 0
			if err := saveState(ctx, m.db, st); err == nil {
				freed++
			}
		}
		if err := deleteGang(ctx, m.db, gangID); err != nil {
			return err
		}
	}

	if _, err := m.db.ExecContext(ctx, `DROP TABLE IF EXISTS rolebot_gang_charter`); err != nil {
		return fmt.Errorf("migrateTradeConvoys drop charter: %w", err)
	}

	// The title.
	titled := 0
	trows, err := m.db.QueryContext(ctx,
		`SELECT corporationID, name FROM rolebot_corp_titles WHERE titleID = ?`, titleSlotConvoy)
	if err != nil {
		return fmt.Errorf("migrateTradeConvoys titles: %w", err)
	}
	type owned struct {
		corpID int64
		name   string
	}
	var owns []owned
	for trows.Next() {
		var o owned
		if trows.Scan(&o.corpID, &o.name) == nil {
			owns = append(owns, o)
		}
	}
	trows.Close()
	for _, o := range owns {
		if row, err := evedb.GetTitleRow(ctx, m.db, o.corpID, titleSlotConvoy); err == nil && row != nil && row.TitleName == o.name {
			row.TitleName = defaultTitleName(titleSlotConvoy)
			if err := evedb.UpsertTitle(ctx, m.db, o.corpID, *row); err != nil {
				return err
			}
		}
		if _, err := m.db.ExecContext(ctx, `
			UPDATE chrCorporationRoles r JOIN character_ ch ON ch.characterID = r.characterID
			   SET r.titleMask = r.titleMask & ~?
			 WHERE ch.corporationID = ? AND (r.titleMask & ?) <> 0`,
			titleSlotConvoy, o.corpID, titleSlotConvoy); err != nil {
			return fmt.Errorf("migrateTradeConvoys title bits: %w", err)
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE character_ SET title = '' WHERE corporationID = ? AND title = ?`, o.corpID, o.name); err != nil {
			return fmt.Errorf("migrateTradeConvoys title text: %w", err)
		}
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM rolebot_corp_titles WHERE corporationID = ? AND titleID = ?`, o.corpID, titleSlotConvoy); err != nil {
			return err
		}
		titled++
	}

	if err := evedb.SetServerSetting(ctx, m.db, settingTradeConvoys, "1"); err != nil {
		return fmt.Errorf("migrateTradeConvoys marker: %w", err)
	}
	if seats > 0 || len(gangIDs) > 0 || titled > 0 {
		rlogf("rolebots: trade convoys retired — %d seat(s) re-slugged as security, %d fleet(s) disbanded (%d pilot(s) sent home), %d Convoy Master title(s) handed back",
			seats, len(gangIDs), freed, titled)
	}
	return nil
}

// settingIndMiningSeats marks that the ActIndMining to ActMining re-slug has run.
const settingIndMiningSeats = "rolebots.indMiningSeatsRetired"

// fleetActivityJob is the fit-pool job behind each fleet activity: what a commander
// must itself be able to fly (the same probe formation uses).
func fleetActivityJob(activity string) string {
	switch activity {
	case ActMining, ActIndMining:
		// The industry fleet's commander mines WITH its fleet
		// (gangLeaderRole), so the probe is the same one.
		return JobMining
	case ActCombat, ActSecurity:
		// A security patrol flies the same gunboat a bounty pack does; what
		// differs is how far from home it takes it (gangSecuritySite).
		return JobCombat
	case ActPiracy:
		return JobPiracy
	}
	return JobCombat
}

// fcCareerScore values a pilot's supporting skills for one fleet activity: the
// support-lever list fit valuation uses (skillsupport.go), plus the refining pair
// for a mining commander. A plain sum of levels: Leadership decides the seat and
// this only orders equals.
func fcCareerScore(activity string, levels map[int64]int) float64 {
	skills := supportLeverSkills(fleetActivityJob(activity))
	if activity == ActMining || activity == ActIndMining {
		skills = append(append([]int64{}, skills...), skillIndRefining, skillIndRefineryEf)
	}
	if activity == ActIndMining {
		// The industry fleet's commander is an industrialist first: the ore feeds its
		// factory lines, so the factory skills order equals.
		skills = append(skills, skillIndustry, skillProductionEff)
	}
	total := 0.0
	for _, id := range skills {
		total += skillLevel(levels, id)
	}
	return total
}

// fcScore is the promotion score: Leadership dominant, career the tiebreak. drive
// is the pilot's Tuning.CommandDrive and scales the career term only, so a follower
// with a Leadership level still outranks a born leader with none.
func fcScore(leadershipLvl int, career, drive float64) float64 {
	if leadershipLvl < 0 {
		leadershipLvl = 0
	}
	if drive <= 0 {
		drive = 1
	}
	return float64(leadershipLvl)*fcLeadershipWeight + career*drive
}

// fcCandidate is one corp member as the promotion pass sees it.
type fcCandidate struct {
	CharID     int64
	Name       string
	Leadership int
	Career     map[string]float64 // activity to career score
	// CommandDrive is the pilot's appetite for command (leadership trait, tuning.go),
	// 1.0 for an even temperament.
	CommandDrive float64
}

// rankCorpFCs reads every enabled rostered member of one corporation with its
// Leadership level and per-activity career scores, one skill read per member.
// In a corporation a bot CEO runs, human members are candidates too (corpmembers.go);
// idleFleetCommanders never forms a fleet around a human seat.
func (m *Mod) rankCorpFCs(ctx context.Context, corpID int64) ([]fcCandidate, error) {
	humans := 0
	if leadsHumans(corpID) {
		humans = 1
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName
		FROM character_ c
		LEFT JOIN rolebot_characters b ON b.characterID = c.characterID
		WHERE c.corporationID = ? AND c.accountID > 0
		  AND (b.enabled = 1 OR (b.characterID IS NULL AND ? = 1))
		ORDER BY c.characterID`, corpID, humans)
	if err != nil {
		return nil, fmt.Errorf("rankCorpFCs: %w", err)
	}
	type row struct {
		id   int64
		name string
	}
	var members []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.name) == nil {
			members = append(members, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rankCorpFCs: %w", err)
	}

	out := make([]fcCandidate, 0, len(members))
	for _, r := range members {
		levels, err := evedb.TrainedSkillLevels(ctx, m.db, r.id)
		if err != nil {
			return nil, fmt.Errorf("rankCorpFCs skills for %d: %w", r.id, err)
		}
		traits, err := getTraits(ctx, m.db, r.id)
		if err != nil {
			return nil, fmt.Errorf("rankCorpFCs traits for %d: %w", r.id, err)
		}
		cand := fcCandidate{
			CharID:       r.id,
			Name:         r.name,
			Leadership:   levels[skillLeadership],
			Career:       map[string]float64{},
			CommandDrive: resolveTuning(traits).CommandDrive,
		}
		for _, act := range []string{ActMining, ActIndMining, ActCombat, ActSecurity, ActPiracy} {
			cand.Career[act] = fcCareerScore(act, levels)
		}
		out = append(out, cand)
	}
	return out, nil
}

// corpRankRow is one seat as stored.
type corpRankRow struct {
	CorpID      int64
	CharID      int64
	Name        string
	Activity    string
	Seat        int
	Score       float64
	Manual      bool
	AppointedAt int64
}

// listCorpRanks reads one corporation's chain of command, seats in order.
func (m *Mod) listCorpRanks(ctx context.Context, corpID int64) ([]corpRankRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.corporationID, r.characterID, COALESCE(c.characterName, ''),
		       r.activity, r.seat, r.score, r.manual, r.appointedAt
		FROM rolebot_corp_ranks r
		LEFT JOIN character_ c ON c.characterID = r.characterID
		WHERE r.corporationID = ?
		ORDER BY r.seat, r.characterID`, corpID)
	if err != nil {
		return nil, fmt.Errorf("listCorpRanks: %w", err)
	}
	defer rows.Close()
	var out []corpRankRow
	for rows.Next() {
		var r corpRankRow
		var manual int64
		if err := rows.Scan(&r.CorpID, &r.CharID, &r.Name, &r.Activity,
			&r.Seat, &r.Score, &manual, &r.AppointedAt); err != nil {
			return nil, err
		}
		r.Manual = manual != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// pruneCorpRanks removes seats whose holder no longer qualifies for any seat here:
// left the corporation or the roster, or disabled. Manual rows go too. Returns the
// rows removed. In a corporation a bot CEO runs, a human member's seat stands.
func (m *Mod) pruneCorpRanks(ctx context.Context, corpID int64) (int64, error) {
	humans := 0
	if leadsHumans(corpID) {
		humans = 1
	}
	res, err := m.db.ExecContext(ctx, `
		DELETE r FROM rolebot_corp_ranks r
		LEFT JOIN character_ c ON c.characterID = r.characterID
		LEFT JOIN rolebot_characters b ON b.characterID = r.characterID
		WHERE r.corporationID = ?
		  AND (c.characterID IS NULL OR c.corporationID <> r.corporationID
		       OR (b.characterID IS NULL AND ? = 0)
		       OR (b.characterID IS NOT NULL AND b.enabled = 0))`, corpID, humans)
	if err != nil {
		return 0, fmt.Errorf("pruneCorpRanks: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// promoteCorp runs one corporation's promotion pass and answers a short summary
// (the slash command and the panel both print it). It covers NPC corporations and
// player corporations run by a bot CEO (corpceo_roles.go); for any other player
// corporation it only prunes departed pilots.
func (m *Mod) promoteCorp(ctx context.Context, corpID int64) (string, error) {
	facts, err := getAnyCorpFacts(ctx, m.db, corpID)
	if err != nil {
		return "", err
	}
	pruned, err := m.pruneCorpRanks(ctx, corpID)
	if err != nil {
		return "", err
	}
	if _, managed := managedCEO(corpID); facts.Player && !managed {
		return fmt.Sprintf("%s is a player corporation — its commanders are appointed by hand (%d departed seat(s) cleared).",
			facts.Name, pruned), nil
	}

	// Rank the roster first: one seat per FCSeatDivisor() enabled members, which is
	// also the candidate pool (facts.Bots counts disabled ones too).
	cands, err := m.rankCorpFCs(ctx, corpID)
	if err != nil {
		return "", err
	}
	activities := corpFleetActivities(facts.Policy)
	seats := len(cands) / FCSeatDivisor()
	now := clock.Unix()
	// A bot-run or NPC-chaired corporation's seats follow its departments' demand
	// (corpstaff_dept.go); a war footing adds seats, dealt mostly to the security wing.
	rotation := fleetSeatRotation(activities)
	if _, _, chaired := chairOf(corpID); chaired {
		demand, war := corpSeatDemand(corpID)
		rotation = demandRotation(activities, demand)
		seats += warSurgeSeats(seats, war, activities)
	}

	existing, err := m.listCorpRanks(ctx, corpID)
	if err != nil {
		return "", err
	}
	manualHeld := map[int64]bool{}
	autoHeld := map[int64]corpRankRow{}
	manualCount := 0
	for _, r := range existing {
		if r.Manual {
			manualHeld[r.CharID] = true
			manualCount++
		} else {
			autoHeld[r.CharID] = r
		}
	}

	// The corp organizes nothing, or is too small to: every auto seat folds.
	autoSeats := seats - manualCount
	if autoSeats < 0 {
		autoSeats = 0
	}
	if len(activities) == 0 {
		autoSeats = 0
	}

	// Choose the commanders.
	type pair struct {
		cand     fcCandidate
		activity string
		score    float64
	}
	var pairs []pair
	eligible := 0
	for _, c := range cands {
		if manualHeld[c.CharID] {
			continue
		}
		if c.Leadership < 1 {
			continue // no Leadership, no command
		}
		eligible++
		for _, act := range activities {
			pairs = append(pairs, pair{c, act, fcScore(c.Leadership, c.Career[act], c.CommandDrive)})
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].score > pairs[j].score })

	// Seats are dealt across the activities in rotation, not by highest score: a
	// highest-first sweep would never seat a mining commander (ActIndMining outscores it).
	byAct := map[string][]pair{}
	for _, p := range pairs {
		byAct[p.activity] = append(byAct[p.activity], p)
	}
	head := map[string]int{} // per-activity read head into its sorted list
	chosen := map[int64]pair{}
	for len(chosen) < autoSeats {
		dealt := false
		for _, act := range rotation {
			if len(chosen) >= autoSeats {
				break
			}
			list := byAct[act]
			for head[act] < len(list) {
				p := list[head[act]]
				head[act]++
				if _, taken := chosen[p.cand.CharID]; taken {
					continue // seated already on an earlier turn of the wheel
				}
				chosen[p.cand.CharID] = p
				dealt = true
				break
			}
		}
		if !dealt {
			break // every activity is out of unseated candidates
		}
	}

	// Demote auto holders who lost their seat, write or reconfirm the rest.
	demoted, promoted := 0, 0
	for charID := range autoHeld {
		if _, keeps := chosen[charID]; keeps {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `
			DELETE FROM rolebot_corp_ranks
			WHERE corporationID = ? AND characterID = ? AND manual = 0`,
			corpID, charID); err != nil {
			return "", fmt.Errorf("promoteCorp demote: %w", err)
		}
		demoted++
		m.logBot(charID, logInfo, "corp",
			"stood down as a fleet commander of %s — outranked at this week's review", facts.Name)
	}
	for charID, p := range chosen {
		prev, held := autoHeld[charID]
		appointedAt := now
		if held && prev.Activity == p.activity {
			appointedAt = prev.AppointedAt // reconfirmed, not re-appointed
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_ranks
				(corporationID, characterID, activity, seat, score, manual, appointedAt, updatedAt)
			VALUES (?, ?, ?, 0, ?, 0, ?, ?)
			ON DUPLICATE KEY UPDATE
				activity = VALUES(activity), score = VALUES(score),
				appointedAt = VALUES(appointedAt), updatedAt = VALUES(updatedAt)`,
			corpID, charID, p.activity, p.score, appointedAt, now); err != nil {
			return "", fmt.Errorf("promoteCorp appoint: %w", err)
		}
		if !held {
			promoted++
			m.logBot(charID, logInfo, "corp",
				"promoted to %s fleet commander of %s (Leadership %s, score %.0f)",
				p.activity, facts.Name, romanLevel(p.cand.Leadership), p.score)
		} else if prev.Activity != p.activity {
			m.logBot(charID, logInfo, "corp",
				"reassigned to %s fleet command of %s", p.activity, facts.Name)
		}
	}
	if err := m.renumberCorpSeats(ctx, corpID); err != nil {
		return "", err
	}

	// Scarcity: fewer command-capable members than seats. The CEO trains the best
	// careers lacking Leadership, through the Genolution shop path (real ISK and journal rows).
	trained := 0
	if eligible < autoSeats && len(activities) > 0 {
		trained = m.corpLeadershipNudge(ctx, corpID, cands, activities)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d member(s), %d seat(s)", facts.Name, len(cands), seats)
	if manualCount > 0 {
		fmt.Fprintf(&b, " (%d held by appointment)", manualCount)
	}
	if len(activities) == 0 {
		b.WriteString(", no fleet activities in its tags")
	} else {
		fmt.Fprintf(&b, " over %s", strings.Join(activities, "/"))
	}
	fmt.Fprintf(&b, " — %d promoted, %d stood down", promoted, demoted)
	if pruned > 0 {
		fmt.Fprintf(&b, ", %d departed cleared", pruned)
	}
	if trained > 0 {
		fmt.Fprintf(&b, ", %d sent for Leadership training", trained)
	}
	b.WriteString(".")
	return b.String(), nil
}

// renumberCorpSeats rewrites seat numbers 1..N for display: manual seats
// first (the operator's appointments head the table), then by score.
func (m *Mod) renumberCorpSeats(ctx context.Context, corpID int64) error {
	ranks, err := m.listCorpRanks(ctx, corpID)
	if err != nil {
		return err
	}
	sort.SliceStable(ranks, func(i, j int) bool {
		if ranks[i].Manual != ranks[j].Manual {
			return ranks[i].Manual
		}
		return ranks[i].Score > ranks[j].Score
	})
	for i, r := range ranks {
		if r.Seat == i+1 {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_ranks SET seat = ? WHERE corporationID = ? AND characterID = ?`,
			i+1, corpID, r.CharID); err != nil {
			return fmt.Errorf("renumberCorpSeats: %w", err)
		}
	}
	return nil
}

// corpLeadershipNudge is the corp-scoped scarcity investment: the members
// with the best careers for the corp's own activities who hold no Leadership
// (and automate their skills, and are docked) buy the book and queue level I.
// Returns how many actually bought one.
func (m *Mod) corpLeadershipNudge(ctx context.Context, corpID int64, cands []fcCandidate, activities []string) int {
	type prospect struct {
		charID int64
		career float64
	}
	var prospects []prospect
	for _, c := range cands {
		if c.Leadership >= 1 {
			continue
		}
		best := 0.0
		for _, act := range activities {
			if s := c.Career[act]; s > best {
				best = s
			}
		}
		// A follower is rarely sent for the book: the corporation invests in pilots who
		// want to lead.
		drive := c.CommandDrive
		if drive <= 0 {
			drive = 1
		}
		prospects = append(prospects, prospect{c.CharID, best * drive})
	}
	sort.SliceStable(prospects, func(i, j int) bool { return prospects[i].career > prospects[j].career })

	trained := 0
	for _, p := range prospects {
		if trained >= gangLeadTrainPerPass {
			break
		}
		// Only pilots who automate their skills and are sitting in a station.
		var ok int
		if err := m.db.QueryRowContext(ctx, `
			SELECT 1 FROM rolebot_characters b
			JOIN character_ c ON c.characterID = b.characterID
			LEFT JOIN rolebot_profiles pr ON pr.characterID = b.characterID
			WHERE b.characterID = ? AND b.enabled = 1
			  AND COALESCE(pr.autoSkills, 1) = 1 AND c.stationID <> 0`, p.charID).Scan(&ok); err != nil {
			continue
		}
		// An NPC chair pays for the book out of its weekly budget
		// (npcchair.go); otherwise the member buys its own.
		if m.npcChairBuysBook(ctx, corpID, p.charID) {
			trained++
			continue
		}
		forgetCharSkills(p.charID) // the sheet just changed (skillcache.go)
		if _, err := evedb.PurchaseAndInjectSkill(ctx, m.db, p.charID, skillLeadership); err != nil {
			m.logBot(p.charID, logInfo, "skills",
				"the corp suggested a Leadership book but it did not buy one: %v", err)
			continue
		}
		if _, err := evedb.AddSkillToQueue(ctx, m.db, p.charID, skillLeadership); err != nil {
			m.logBot(p.charID, logError, "skills", "queueing Leadership: %v", err)
			continue
		}
		trained++
		m.logBot(p.charID, logInfo, "skills",
			"bought a Leadership book at the corp's urging — command material once trained")
		m.chatterNoteBot(ctx, p.charID, "pers.skillbook", map[string]string{"skill": "Leadership"}, skillLeadership)
	}
	return trained
}

// promotionPass runs promoteCorp over every NPC corporation with rostered bots and
// no NPC chair, resets the weekly clock and answers the combined summary. A chaired
// corporation's chair deals its own seats (npcchair.go); this is the fallback.
func (m *Mod) promotionPass(ctx context.Context) (string, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.corporationID
		FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		JOIN crpNPCCorporations n ON n.corporationID = c.corporationID
		WHERE b.enabled = 1
		GROUP BY c.corporationID`)
	if err != nil {
		return "", fmt.Errorf("promotionPass: %w", err)
	}
	var corps []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil && !npcChaired(id) {
			corps = append(corps, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("promotionPass: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Promotion pass over %d corporation(s):\n", len(corps))
	for _, corpID := range corps {
		if ctx.Err() != nil {
			return b.String(), ctx.Err()
		}
		summary, err := m.promoteCorp(ctx, corpID)
		if err != nil {
			rlogf("rolebots: promotion pass corp %d: %v", corpID, err)
			fmt.Fprintf(&b, "  corp %d: %v\n", corpID, err)
			continue
		}
		fmt.Fprintf(&b, "  %s\n", summary)
	}

	next := clock.Unix() + promoteEverySec
	promoteNextAt.Store(next)
	if err := evedb.SetServerSetting(ctx, m.db, settingPromoteNextAt,
		fmt.Sprintf("%d", next)); err != nil {
		rlogf("rolebots: persist promotion clock: %v", err)
	}
	return b.String(), nil
}

// startPromotionSweep registers the weekly clock check. Same shape as the formation
// sweep: off the tick goroutine, behind an overlap guard, gated on the master switch.
func (m *Mod) startPromotionSweep() {
	m.registerPeriodic("rolebot-corp-promote", promoteCheckTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		if clock.Unix() < promoteNextAt.Load() {
			return true
		}
		if !promoteBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("promotion pass", func() {
			defer promoteBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			summary, err := m.promotionPass(ctx)
			if err != nil {
				rlogf("rolebots: promotion pass: %v", err)
				return
			}
			for _, line := range strings.Split(strings.TrimRight(summary, "\n"), "\n") {
				rlogf("rolebots: %s", line)
			}
		})
		return true
	})
}

// countEnabledCorpBots is how many enabled rostered pilots wear this corporation's
// badge: the number the seat arithmetic runs on.
func (m *Mod) countEnabledCorpBots(ctx context.Context, corpID int64) (int, error) {
	var n int
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		WHERE b.enabled = 1 AND c.corporationID = ?`, corpID).Scan(&n); err != nil {
		return 0, fmt.Errorf("countEnabledCorpBots: %w", err)
	}
	return n, nil
}

// corpFleetOreStation reads a player corporation's mining drop-off, 0 when
// its fleets sell as usual.
func (m *Mod) corpFleetOreStation(ctx context.Context, corpID int64) int64 {
	var id int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(fleetOreStationID, 0) FROM rolebot_corps WHERE corporationID = ?`,
		corpID).Scan(&id)
	return id
}

// setCorpFleetOreStation writes the drop-off. A targeted UPDATE-or-INSERT
// rather than saveCorpPolicy, which would clear the row's seeded flag as a
// side effect of a setting that is not policy.
func (m *Mod) setCorpFleetOreStation(ctx context.Context, corpID, stationID int64) error {
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corps SET fleetOreStationID = ?, updatedAt = ? WHERE corporationID = ?`,
		stationID, clock.Unix(), corpID)
	if err != nil {
		return fmt.Errorf("setCorpFleetOreStation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// A player corporation may have no policy row yet: nothing seeds one.
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corps (corporationID, fleetOreStationID, seeded, updatedAt)
			VALUES (?, ?, 0, ?)
			ON DUPLICATE KEY UPDATE fleetOreStationID = VALUES(fleetOreStationID), updatedAt = VALUES(updatedAt)`,
			corpID, stationID, clock.Unix()); err != nil {
			return fmt.Errorf("setCorpFleetOreStation: %w", err)
		}
	}
	return nil
}

// demoteCorpRank removes one seat, manual or not: it is an operator action, not
// the pass's.
func (m *Mod) demoteCorpRank(ctx context.Context, corpID, charID int64) error {
	if _, err := m.db.ExecContext(ctx, `
		DELETE FROM rolebot_corp_ranks WHERE corporationID = ? AND characterID = ?`,
		corpID, charID); err != nil {
		return fmt.Errorf("demoteCorpRank: %w", err)
	}
	return m.renumberCorpSeats(ctx, corpID)
}
