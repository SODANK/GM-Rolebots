package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// Wiping generated bots: delete everything between RoleBotN and RoleBotM.
//
// The range is over account sequence numbers, the only handle an operator can see and type. A
// character is many rows (entity tree, wallet, standings, orders, chat channel, skills), so the
// purge walks an ordered step list that tolerates a missing table. The order is entity_attributes
// before entity, everything before character_, character_ before account, so a failed run can be
// re-run. Killmails are not purged: they store snapshot names, so history stays readable.

// purgeStep is one delete in the ordered list. The query takes exactly one
// placeholder, bound to the characterID (or accountID, for the account step).
type purgeStep struct {
	table string
	query string
}

// moduleCharacterSteps is every table this module owns that names a character. A purge runs it, and
// so does a migration to a player account (migrate.go) where the character survives. Each row must go
// in both cases, or a later bot handed the same sequence number inherits it.
var moduleCharacterSteps = []purgeStep{
	{"rolebot_state", `DELETE FROM rolebot_state WHERE characterID = ?`},
	{"rolebot_profiles", `DELETE FROM rolebot_profiles WHERE characterID = ?`},
	{"rolebot_traits", `DELETE FROM rolebot_traits WHERE characterID = ?`},
	{"rolebot_grievances", `DELETE FROM rolebot_grievances WHERE botID = ? OR pilotID = ?`},
	{"rolebot_scc_conviction", `DELETE FROM rolebot_scc_conviction WHERE characterID = ?`},
	{"rolebot_fit_ships", `DELETE FROM rolebot_fit_ships WHERE characterID = ?`},
	{"rolebot_gang_members", `DELETE FROM rolebot_gang_members WHERE characterID = ?`},
	// A gang is its leader's operation, so a gang whose leader is gone is over. Its jetcans go with it;
	// the can entity goes with the owner's item tree.
	{"rolebot_gangs", `DELETE FROM rolebot_gangs WHERE leaderCharID = ?`},
	{"rolebot_gang_cans", `DELETE FROM rolebot_gang_cans WHERE ownerCharID = ? OR bossCharID = ?`},
	// Service in a player's fleet, from both ends. bossCharID is cleared even for a human boss, so no
	// row names a purged pilot as somebody's crew.
	{"rolebot_conscripts", `DELETE FROM rolebot_conscripts WHERE characterID = ? OR bossCharID = ?`},
	{"rolebot_conscript_briefed", `DELETE FROM rolebot_conscript_briefed WHERE charID = ?`},
	{"rolebot_fc_manual_sent", `DELETE FROM rolebot_fc_manual_sent WHERE charID = ?`},
	{"rolebot_orders", `DELETE FROM rolebot_orders WHERE characterID = ?`},
	{"rolebot_trades", `DELETE FROM rolebot_trades WHERE characterID = ?`},
	{"rolebot_trade_stock", `DELETE FROM rolebot_trade_stock WHERE characterID = ?`},
	{"rolebot_industry_shelf", `DELETE FROM rolebot_industry_shelf WHERE characterID = ?`},
	{"rolebot_bpc_listings", `DELETE FROM rolebot_bpc_listings WHERE characterID = ?`},
	{"rolebot_loot_hold", `DELETE FROM rolebot_loot_hold WHERE characterID = ?`},
	{"rolebot_mission_runs", `DELETE FROM rolebot_mission_runs WHERE charID = ?`},
	{"rolebot_gang_haul", `DELETE FROM rolebot_gang_haul WHERE characterID = ?`},
	// Ore a fleet delivered into this pilot's hangar and it never disposed of.
	// The hangar goes with the character; the note about it has to as well.
	{"rolebot_ore_hold", `DELETE FROM rolebot_ore_hold WHERE characterID = ?`},
	{"rolebot_mineral_hold", `DELETE FROM rolebot_mineral_hold WHERE characterID = ?`},
	{"rolebot_corp_applications", `DELETE FROM rolebot_corp_applications WHERE characterID = ?`},
	{"rolebot_corp_reviews", `DELETE FROM rolebot_corp_reviews WHERE characterID = ?`},
	// An officer's department and performance, and invitations to it (corpstaff.go), both keyed on the pilot.
	{"rolebot_corp_posts", `DELETE FROM rolebot_corp_posts WHERE characterID = ?`},
	{"rolebot_corp_ranks", `DELETE FROM rolebot_corp_ranks WHERE characterID = ?`},
	{"rolebot_corp_bench", `DELETE FROM rolebot_corp_bench WHERE characterID = ?`},
	{"rolebot_corp_grants", `DELETE FROM rolebot_corp_grants WHERE characterID = ?`},
	{"rolebot_corp_pos_grants", `DELETE FROM rolebot_corp_pos_grants WHERE characterID = ?`},
	{"rolebot_corp_invites", `DELETE FROM rolebot_corp_invites WHERE characterID = ?`},
	// A member's rank, merit trail, assignments and offer clock
	// (corpmerit.go, corpassign.go, corphuman.go).
	{"rolebot_corp_merit", `DELETE FROM rolebot_corp_merit WHERE characterID = ?`},
	// Its morale and the events behind it (corpmorale.go).
	{"rolebot_corp_morale", `DELETE FROM rolebot_corp_morale WHERE characterID = ?`},
	{"rolebot_corp_morale_events", `DELETE FROM rolebot_corp_morale_events WHERE characterID = ?`},
	// Its place in an arm (corparm.go).
	{"rolebot_corp_arm_members", `DELETE FROM rolebot_corp_arm_members WHERE characterID = ?`},
	// The title text the module last wrote it (corptitles.go).
	{"rolebot_title_text", `DELETE FROM rolebot_title_text WHERE characterID = ?`},
	{"rolebot_corp_merit_log", `DELETE FROM rolebot_corp_merit_log WHERE characterID = ?`},
	{"rolebot_corp_assignments", `DELETE FROM rolebot_corp_assignments WHERE characterID = ?`},
	{"rolebot_human_offers", `DELETE FROM rolebot_human_offers WHERE characterID = ?`},
	{"rolebot_skill_plan", `DELETE FROM rolebot_skill_plan WHERE characterID = ?`},
	{"rolebot_skill_prefs", `DELETE FROM rolebot_skill_prefs WHERE characterID = ?`},
	{"rolebot_log", `DELETE FROM rolebot_log WHERE characterID = ?`},
	{"rolebot_stats", `DELETE FROM rolebot_stats WHERE characterID = ?`},
	{"rolebot_generated", `DELETE FROM rolebot_generated WHERE characterID = ?`},
	{"rolebot_characters", `DELETE FROM rolebot_characters WHERE characterID = ?`},
	// The legacy hand-assignment roster. A generated bot is never on it, but a purge removes every row
	// that names the character ID.
	{"bot_characters", `DELETE FROM bot_characters WHERE characterID = ?`},
}

// characterPurgeSteps is every table that references a character, in delete order. A step's column
// must be checked against information_schema: execTolerant swallows an unknown column, so a wrong
// name deletes nothing and reports success. See docs/comment-notes-b01.md#purge-steps.
var characterPurgeSteps = []purgeStep{
	// Chat. A character owns a channel whose ID is its characterID and which is also its contact list
	// (the ACL is in channelMods), so both sides of the ACL go.
	{"channelmods", `DELETE FROM channelMods WHERE channelID = ? OR accessor = ?`},
	{"channelchars", `DELETE FROM channelChars WHERE charID = ?`},
	{"channels", `DELETE FROM channels WHERE ownerID = ?`},

	// Mail, both directions, bodies before headers: the details row is found through the header.
	// Bots receive no new mail (mailsuppress.go).
	{"evemaildetails", `DELETE FROM evemailDetails WHERE messageID IN (
		SELECT messageID FROM evemail WHERE channelID = ? OR senderID = ?)`},
	{"evemail", `DELETE FROM evemail WHERE channelID = ? OR senderID = ?`},

	// Market. Orders outlive their owner by design; transactions and journal
	// rows are the history of a pilot that is about to stop having existed.
	{"market_orders", `DELETE FROM market_orders WHERE charID = ?`},
	{"market_transactions", `DELETE FROM market_transactions WHERE characterID = ?`},
	{"market_journal", `DELETE FROM market_journal WHERE characterID = ?`},

	// Standings, both directions: the pilot's opinions and anyone's opinion
	// of the pilot.
	{"chrstandings", `DELETE FROM chrStandings WHERE characterID = ? OR toID = ?`},
	// A corporation's declared opinion of the pilot (crpStandings is keyed
	// by corporation, so only the toID direction can name a character).
	{"crpstandings", `DELETE FROM crpStandings WHERE toID = ?`},
	{"chrnpcstandings", `DELETE FROM chrNPCStandings WHERE characterID = ? OR fromID = ?`},
	// The two standing ledgers have no characterID column: they are (fromID, toID) event logs with
	// original and effective pairs.
	{"chrstandingchanges", `DELETE FROM chrStandingChanges
		WHERE fromID = ? OR toID = ? OR originalFromID = ? OR originalToID = ?`},
	{"chrstandingtransactions", `DELETE FROM chrStandingTransactions
		WHERE fromID = ? OR toID = ? OR originalFromID = ? OR originalToID = ?`},

	// Career, agents, missions.
	{"chremployment", `DELETE FROM chrEmployment WHERE characterID = ?`},
	{"chrapplications", `DELETE FROM chrApplications WHERE characterID = ?`},
	{"chrcorporationroles", `DELETE FROM chrCorporationRoles WHERE characterID = ?`},
	// crpcharshares is not here: a pilot's shares go back to the issuing treasuries (returnSharesOf),
	// which keeps SUM(holdings) equal to corporation.shares.
	{"crpvotes", `DELETE FROM crpVotes WHERE characterID = ?`},
	{"charactermissions", `DELETE FROM characterMissions WHERE charID = ?`},
	{"chrmissionstate", `DELETE FROM chrMissionState WHERE characterID = ?`},
	{"missiondungeoninstances", `DELETE FROM missionDungeonInstances WHERE charID = ?`},
	{"agentloyaltypoints", `DELETE FROM agentLoyaltyPoints WHERE charID = ?`},
	{"agtlocatorjobs", `DELETE FROM agtLocatorJobs WHERE charID = ?`},
	{"chroffers", `DELETE FROM chrOffers WHERE characterID = ?`},
	{"chroffernotices", `DELETE FROM chrOfferNotices WHERE charID = ?`},
	{"chrstorylineprogress", `DELETE FROM chrStorylineProgress WHERE charID = ?`},
	{"characterresearch", `DELETE FROM characterResearch WHERE charID = ?`},

	// Money owed and work in flight. A bill outlives its debtor, and an undelivered job holds its
	// blueprint at flag 111, which a wiped installer would strand.
	{"bills", `DELETE FROM bills WHERE debtorID = ? OR creditorID = ?`},
	{"couriermissions", `DELETE FROM courierMissions
		WHERE issuerID = ? OR assigneeID = ? OR acceptorID = ?`},
	{"ramjobs", `DELETE FROM ramJobs WHERE installerID = ?`},
	// Exact match only: a character ID never collides with the corporation IDs that own station lines.
	{"ramassemblylines", `DELETE FROM ramAssemblyLines WHERE ownerID = ?`},
	// The client's own owner-name cache row. Left behind, the name of a deleted
	// pilot keeps resolving in every window that looks a name up by ID.
	{"cacheowners", `DELETE FROM cacheOwners WHERE ownerID = ?`},

	// Skills, clones, the small per-pilot state tables.
	{"chrskillqueue", `DELETE FROM chrSkillQueue WHERE characterID = ?`},
	{"chrskilltraining", `DELETE FROM chrSkillTraining WHERE characterID = ?`},
	{"chrjumpclones", `DELETE FROM chrJumpClones WHERE characterID = ?`},
	{"chrclonejumptime", `DELETE FROM chrCloneJumpTime WHERE characterID = ?`},
	{"chrtutorials", `DELETE FROM chrTutorials WHERE characterID = ?`},
	{"chrtutorialdisabled", `DELETE FROM chrTutorialDisabled WHERE characterID = ?`},
	{"chrsolarsystemvisits", `DELETE FROM chrSolarSystemVisits WHERE characterID = ?`},
	{"chrrecustomization", `DELETE FROM chrRecustomization WHERE characterID = ?`},
	{"chrnotes", `DELETE FROM chrNotes WHERE ownerID = ?`},
	{"chrownernote", `DELETE FROM chrOwnerNote WHERE ownerID = ?`},
	{"bookmarks", `DELETE FROM bookmarks WHERE ownerID = ?`},
	{"insurance", `DELETE FROM insurance WHERE ownerID = ?`},
	{"dronestate", `DELETE FROM droneState WHERE ownerID = ?`},

	// The module's own tables (moduleCharacterSteps) are spliced in below by init.

	// Last of the character's own rows.
	{"character_", `DELETE FROM character_ WHERE characterID = ?`},
}

// init splices moduleCharacterSteps into characterPurgeSteps after the game's per-pilot rows and
// before character_, so a failed run can be re-run. A migration runs the module half on its own.
func init() {
	tail := characterPurgeSteps[len(characterPurgeSteps)-1:]
	head := characterPurgeSteps[:len(characterPurgeSteps)-1]
	merged := make([]purgeStep, 0, len(head)+len(moduleCharacterSteps)+len(tail))
	merged = append(merged, head...)
	merged = append(merged, moduleCharacterSteps...)
	merged = append(merged, tail...)
	characterPurgeSteps = merged
}

// characterDetachSteps are the roster-wide tables that mention a character without belonging to it.
// They are updated, never deleted: a demand row is the roster's shared shopping list. Zero means
// nobody in all four columns, and a stale ID would read as a claim held forever.
var characterDetachSteps = []purgeStep{
	{"rolebot_market_demand", `UPDATE rolebot_market_demand SET claimedBy = 0, claimedAt = 0 WHERE claimedBy = ?`},
	{"rolebot_market_demand", `UPDATE rolebot_market_demand SET lastCharID = 0 WHERE lastCharID = ?`},
	{"rolebot_mineral_demand", `UPDATE rolebot_mineral_demand SET lastCharID = 0 WHERE lastCharID = ?`},
	{"rolebot_belt_distress", `UPDATE rolebot_belt_distress SET lastCharID = 0 WHERE lastCharID = ?`},
}

// execTolerant runs one delete and swallows a missing table or column. Any other error is reported:
// a foreign-key refusal means the order in the step list is wrong.
func execTolerant(ctx context.Context, db *sql.DB, step purgeStep, args ...any) error {
	_, err := db.ExecContext(ctx, step.query, args...)
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "doesn't exist") || strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "unknown column") {
		return nil
	}
	return fmt.Errorf("%s: %w", step.table, err)
}

// argCount counts placeholders, so a step written with `? OR ... ?` gets the character ID once per
// placeholder without declaring an arity.
func argCount(q string) int { return strings.Count(q, "?") }

// ── player corporations ─────────────────────────────────────────────────────

// releaseFromPlayerCorp settles a pilot's membership of a player corporation before its rows are
// deleted. An ordinary member leaves; a CEO hands over to the longest-serving non-bot member, else to
// a rostered member (succeedCEO), else the corporation is closed. NPC corps need nothing.
// See docs/comment-notes-b01.md#release-from-corp.
func (m *Mod) releaseFromPlayerCorp(ctx context.Context, charID int64) error {
	var corpID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(corporationID, 0) FROM character_ WHERE characterID = ?`,
		charID).Scan(&corpID); err != nil || corpID == 0 {
		return nil // no character, or no corp: nothing to settle
	}
	npc, err := isNPCCorp(ctx, m.db, corpID)
	if err != nil {
		return err
	}
	if npc {
		return nil
	}

	var ceoID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(ceoID, 0) FROM corporation WHERE corporationID = ?`,
		corpID).Scan(&ceoID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // corp already gone
		}
		return fmt.Errorf("releaseFromPlayerCorp read: %w", err)
	}

	if ceoID != charID {
		// An ordinary member. memberCount is a stored column that nothing else
		// maintains, so it is decremented here or it drifts upward forever.
		_, _ = m.db.ExecContext(ctx, `
			UPDATE corporation SET memberCount = GREATEST(0, memberCount - 1)
			WHERE corporationID = ?`, corpID)
		return nil
	}

	// The CEO. Look for a human successor: a pilot with accountID > 0 and no rolebot roster row,
	// longest-serving first.
	var heir int64
	err = m.db.QueryRowContext(ctx, `
		SELECT c.characterID
		  FROM character_ c
		  LEFT JOIN rolebot_characters b ON b.characterID = c.characterID
		 WHERE c.corporationID = ? AND c.characterID <> ?
		   AND c.accountID > 0 AND b.characterID IS NULL
		 ORDER BY c.corporationDateTime ASC, c.characterID ASC
		 LIMIT 1`, corpID, charID).Scan(&heir)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("releaseFromPlayerCorp heir: %w", err)
	}
	// No human left: the rostered member best suited takes over (corpceo.go) rather than the corporation
	// closing under the pilots still in it.
	if heir == 0 && m.succeedCEO(ctx, corpID, charID) {
		_, _ = m.db.ExecContext(ctx, `
			UPDATE corporation SET memberCount = GREATEST(0, memberCount - 1)
			WHERE corporationID = ?`, corpID)
		rlogf("rolebots: corp %d CEO %d purged — the chair passed to a rostered member", corpID, charID)
		return nil
	}
	if heir != 0 {
		if _, err := evedb.SetCorpCEO(ctx, m.db, corpID, heir); err != nil {
			return fmt.Errorf("hand corp %d to %d: %w", corpID, heir, err)
		}
		_, _ = m.db.ExecContext(ctx, `
			UPDATE corporation SET memberCount = GREATEST(0, memberCount - 1)
			WHERE corporationID = ?`, corpID)
		rlogf("rolebots: corp %d CEO %d purged — handed to player %d", corpID, charID, heir)
		return nil
	}

	res, err := evedb.ESPDeleteCorp(ctx, m.db, corpID)
	if err != nil {
		// The close can refuse (a starbase, NPC agents). The character is deleted anyway, so vacate the seat:
		// a corp with no CEO can be adopted from the panel, one led by a ghost cannot.
		_, _ = m.db.ExecContext(ctx,
			`UPDATE corporation SET ceoID = 0, memberCount = GREATEST(0, memberCount - 1)
			 WHERE corporationID = ?`, corpID)
		return fmt.Errorf("close corp %d (seat vacated): %w", corpID, err)
	}
	forgetCorp(corpID) // the corporation is closed; its volatile memory goes with it (forgetreg.go)
	rlogf("rolebots: corp %s (%d) closed — its CEO was a rolebot and no player remained; %d member(s) kicked",
		res.Name, corpID, len(res.Kicked))
	return nil
}

// returnSharesOf empties a pilot's holdings back into each issuer's treasury
// and re-weighs their votes (the core's own DeleteCharacter does the same).
func (m *Mod) returnSharesOf(ctx context.Context, charID int64) error {
	var olds []evedb.ShareDelta
	var corps []int64
	if rows, err := m.db.QueryContext(ctx, `
		SELECT corporationID, shares FROM crpcharshares
		 WHERE characterID = ? AND corporationID <> characterID`, charID); err == nil {
		for rows.Next() {
			var corpID, n int64
			if rows.Scan(&corpID, &n) == nil {
				olds = append(olds, evedb.ShareDelta{HolderID: charID, Old: n})
				corps = append(corps, corpID)
			}
		}
		rows.Close()
	}
	if len(corps) == 0 {
		return nil
	}
	if _, err := evedb.ReturnSharesToIssuers(ctx, m.db, charID); err != nil {
		return err
	}
	for i, corpID := range corps {
		announceShares(corpID, []evedb.ShareDelta{olds[i]}, 0, 0)
	}
	return nil
}

// purgeCharacter removes one character and everything it owns.
func (m *Mod) purgeCharacter(ctx context.Context, charID int64) error {
	var problems []string

	// Corporation-account orders come off the book first (corpstock.go), while character_ still names
	// the corporation: the purge deletes every market_orders row by charID.
	m.cancelCorpOrdersOf(ctx, charID)

	// Its stock goes back to the corporations that issued it, while the
	// register still shows what it held.
	if err := m.returnSharesOf(ctx, charID); err != nil {
		problems = append(problems, "shares: "+err.Error())
	}

	// Settle player-corp membership first, while the character row exists: the CEO check, succession and
	// the corp close read character_ and corporation together.
	if err := m.releaseFromPlayerCorp(ctx, charID); err != nil {
		problems = append(problems, "player corp: "+err.Error())
	}

	// The item tree first: entity rows the character owns plus its own row (itemID equals characterID).
	// Delete entity_attributes before entity, or the attribute rows become unreachable.
	ids, err := int64Column(ctx, m,
		`SELECT itemID FROM entity WHERE ownerID = ? OR itemID = ?`, charID, charID)
	if err != nil {
		problems = append(problems, "enumerate items: "+err.Error())
	}
	for _, chunk := range chunkIDs(ids, 500) {
		ph := placeholders(len(chunk))
		args := make([]any, len(chunk))
		for i, v := range chunk {
			args[i] = v
		}
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM entity_attributes WHERE itemID IN (`+ph+`)`, args...); err != nil {
			problems = append(problems, "entity_attributes: "+err.Error())
		}
		// A corporation's lock on one of these items: crpLockedItems.ownerID is always the corporation, so
		// find the rows by item before the items go.
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM crpLockedItems WHERE itemID IN (`+ph+`)`, args...); err != nil {
			problems = append(problems, "crpLockedItems: "+err.Error())
		}
	}
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM entity WHERE ownerID = ? OR itemID = ?`, charID, charID); err != nil {
		problems = append(problems, "entity: "+err.Error())
	}

	run := func(steps []purgeStep) {
		for _, step := range steps {
			args := make([]any, argCount(step.query))
			for i := range args {
				args[i] = charID
			}
			if err := execTolerant(ctx, m.db, step, args...); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}
	// Detach before delete, so an interrupted purge leaves the shared ledgers usable rather than holding
	// a claim for a half-deleted pilot.
	run(characterDetachSteps)
	forgetSavedState(charID) // rolebot_state is in the steps (statesave.go)
	run(characterPurgeSteps)

	// The roster this pilot was on is the mail gate's input (mailsuppress.go).
	invalidateBotMailboxes()

	// The Stats board (espstats.go) keeps an assembled copy for a minute; clear it so a purged pilot
	// leaves the leaderboard.
	clearStatsMemo()
	// The Industry board (espindustry.go) decodes rolebot_state, so its memo would keep the purged pilot's project.
	clearIndustryMemo()
	// And the Economy pages' wallet totals (especonomy.go), which sum the
	// roster's balances live.
	clearEconomyMemo()

	// The pilot's volatile bookkeeping goes with its rows (forget.go), whether or not a delete failed.
	forgetCharacter(charID)

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// chunkIDs splits a slice so an IN () list never grows past what the server
// will parse. 500 is far below any limit and keeps the statement readable in
// a slow-query log.
func chunkIDs(ids []int64, size int) [][]int64 {
	var out [][]int64
	for len(ids) > size {
		out = append(out, ids[:size])
		ids = ids[size:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}

// ── the range wipe ──────────────────────────────────────────────────────────

// wipeTarget is one account inside the requested range with every character on it. It is resolved
// before anything is deleted, so the account row is deleted once rather than once per pilot.
type wipeTarget struct {
	Seq         int
	AccountID   int64
	AccountName string
	Chars       []wipeChar
}

type wipeChar struct {
	CharacterID int64
	Name        string
	Archetype   string
}

// resolveWipeRange finds every RoleBotN account with from <= N <= to and the characters on them.
// Characters are found through the account, not rolebot_generated, so a bot whose marker row never
// wrote is still wipeable.
func (m *Mod) resolveWipeRange(ctx context.Context, from, to int) ([]wipeTarget, error) {
	if from > to {
		from, to = to, from
	}
	if from < 0 {
		from = 0
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT a.accountID, a.accountName,
		       COALESCE(c.characterID, 0), COALESCE(c.characterName, ''),
		       COALESCE(g.archetype, '')
		FROM account a
		LEFT JOIN character_ c ON c.accountID = a.accountID
		LEFT JOIN rolebot_generated g ON g.characterID = c.characterID
		WHERE a.accountName LIKE ?
		ORDER BY a.accountName`, botAccountPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("resolveWipeRange: %w", err)
	}
	defer rows.Close()

	// One row per character comes back, so they are folded into one target per account. Otherwise the
	// account row would be deleted once per pilot and the progress count would run ahead.
	var out []wipeTarget
	byAcct := map[int64]int{}
	for rows.Next() {
		var acctID int64
		var acctName string
		var wc wipeChar
		if err := rows.Scan(&acctID, &acctName, &wc.CharacterID, &wc.Name, &wc.Archetype); err != nil {
			return nil, fmt.Errorf("resolveWipeRange scan: %w", err)
		}
		n, ok := botSeqOf(acctName)
		if !ok || n < from || n > to {
			continue
		}
		idx, seen := byAcct[acctID]
		if !seen {
			out = append(out, wipeTarget{Seq: n, AccountID: acctID, AccountName: acctName})
			idx = len(out) - 1
			byAcct[acctID] = idx
		}
		// An account with no characters LEFT JOINs to a zero row: still a real account in range, still deleted.
		if wc.CharacterID != 0 {
			out[idx].Chars = append(out[idx].Chars, wc)
		}
	}
	return out, rows.Err()
}

// StartWipe purges a sequence range in the background, as a seed run works. Presence and embodiment
// are torn down first, per character: deleting the row under a projected bot leaves a ball in space
// that nothing can later remove.
func (m *Mod) StartWipe(from, to int) error {
	if from > to {
		from, to = to, from
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)

	if err := m.purgeGate(ctx, 0, "purge"); err != nil {
		cancel()
		return err
	}
	targets, err := m.resolveWipeRange(ctx, from, to)
	if err != nil {
		cancel()
		return err
	}
	if len(targets) == 0 {
		cancel()
		return fmt.Errorf("no %s accounts in the range %d–%d", botAccountPrefix, from, to)
	}
	return m.startWipeTargets(ctx, cancel,
		fmt.Sprintf("wiping %s%d–%s%d", botAccountPrefix, from, botAccountPrefix, to), targets)
}

// StartWipeAll purges the entire generated population: every account this module made and nothing
// else. resolveWipeRange selects accountName LIKE 'RoleBot%' and requires the rest of the name to be
// digits (botSeqOf), so a hand-built roster bot is unreachable from here.
func (m *Mod) StartWipeAll() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)

	if err := m.purgeGate(ctx, 0, "purge"); err != nil {
		cancel()
		return err
	}
	targets, err := m.resolveWipeRange(ctx, 0, math.MaxInt32)
	if err != nil {
		cancel()
		return err
	}
	if len(targets) == 0 {
		cancel()
		return fmt.Errorf("there are no %s accounts to purge", botAccountPrefix)
	}
	return m.startWipeTargets(ctx, cancel, "purging every generated Capsuleer", targets)
}

// purgeGate refuses to delete anything still switched on. charID 0 means the whole generated
// population. An active bot takes sim turns and writes rows, so deleting it mid-turn leaves orphans;
// the operator turns the population off first. verb ("purge", "migrate") names the blocked job.
func (m *Mod) purgeGate(ctx context.Context, charID int64, verb string) error {
	q := `SELECT COUNT(*) FROM rolebot_generated g
	      JOIN rolebot_characters b ON b.characterID = g.characterID
	      WHERE b.enabled = 1`
	var args []any
	if charID != 0 {
		q = `SELECT COUNT(*) FROM rolebot_characters WHERE characterID = ? AND enabled = 1`
		args = append(args, charID)
	}
	var live int
	if err := m.db.QueryRowContext(ctx, q, args...).Scan(&live); err != nil {
		return fmt.Errorf("purgeGate: %w", err)
	}
	if live == 0 {
		return nil
	}
	if charID != 0 {
		return fmt.Errorf("this Capsuleer is still active — deactivate it first, then %s", verb)
	}
	return fmt.Errorf(
		"%d generated Capsuleer(s) are still active — press Deactivate all first, then %s. "+
			"An active bot is taking sim turns while the delete runs, and anything it writes back "+
			"mid-purge is an orphan the wipe has already walked past", live, verb)
}

// playerCorpCEOs is the set of characters in the wipe that lead a player corporation. It is read in
// one statement because a total purge asks it of thousands of pilots.
func (m *Mod) playerCorpCEOs(ctx context.Context) (map[int64]bool, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT co.ceoID FROM corporation co
		 WHERE co.ceoID > 0
		   AND NOT EXISTS (SELECT 1 FROM crpNPCCorporations n
		                    WHERE n.corporationID = co.corporationID)`)
	if err != nil {
		return nil, fmt.Errorf("playerCorpCEOs: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// orderWipeTargets works up the roster: every ordinary member is purged before any CEO, within an
// account and across the run, so releaseFromPlayerCorp decides against the emptiest roster. The sort
// is stable, so accounts otherwise keep sequence order.
func orderWipeTargets(targets []wipeTarget, ceos map[int64]bool) {
	holdsCEO := func(t wipeTarget) bool {
		for _, c := range t.Chars {
			if ceos[c.CharacterID] {
				return true
			}
		}
		return false
	}
	for i := range targets {
		chars := targets[i].Chars
		sort.SliceStable(chars, func(a, b int) bool {
			return !ceos[chars[a].CharacterID] && ceos[chars[b].CharacterID]
		})
	}
	sort.SliceStable(targets, func(a, b int) bool {
		return !holdsCEO(targets[a]) && holdsCEO(targets[b])
	})
}

// startWipeTargets is the run loop both wipes share, so deleting an account means the same in both.
func (m *Mod) startWipeTargets(ctx context.Context, cancel context.CancelFunc, what string, targets []wipeTarget) error {
	pilots := 0
	for _, t := range targets {
		pilots += len(t.Chars)
	}
	// Work up the roster: ordinary members first, CEOs last. A failed CEO-set read costs only the ordering.
	ceos, err := m.playerCorpCEOs(ctx)
	if err != nil {
		rlogf("rolebots: %v — purging in sequence order", err)
	} else if len(ceos) > 0 {
		orderWipeTargets(targets, ceos)
	}
	label := fmt.Sprintf("%s — %d account(s), %d Capsuleer(s)", what, len(targets), pilots)
	if !currentRun.begin(label, len(targets)) {
		cancel()
		return errors.New("a generation or wipe run is already in progress")
	}

	goSafe("wipe run", func() {
		defer cancel()
		defer currentRun.end()
		rlogf("rolebots: %s", label)

		for _, t := range targets {
			var problems []string
			for _, wc := range t.Chars {
				// Stop the sim owning it, withdraw any ball it has in space,
				// then delete.
				_ = setRolebotEnabled(ctx, m.db, wc.CharacterID, false)
				m.withdrawProjection(wc.CharacterID)
				if err := m.purgeCharacter(ctx, wc.CharacterID); err != nil {
					problems = append(problems, wc.Name+": "+err.Error())
				}
			}
			// The account goes last, and only once, however many pilots it
			// held.
			if _, aerr := m.db.ExecContext(ctx,
				`DELETE FROM account WHERE accountID = ?`, t.AccountID); aerr != nil {
				problems = append(problems, "account: "+aerr.Error())
			}
			var err error
			if len(problems) > 0 {
				err = fmt.Errorf("%s: %s", t.AccountName, strings.Join(problems, "; "))
			}
			currentRun.step(err == nil,
				fmt.Sprintf("%s — %d Capsuleer(s)", t.AccountName, len(t.Chars)), err)
			if err != nil {
				rlogf("rolebots: wipe %v", err)
			}
		}
		v := currentRun.snapshot()
		rlogf("rolebots: wipe finished — %d/%d account(s) removed, %d problem(s)", v.Created, v.Total, len(v.Errs))
		m.syncPresence()
	})
	return nil
}

// PurgeOneCharacter deletes one generated character and leaves the others on its account. The account
// row goes only when the character was the last on it. It runs inline, not as a background run.
func (m *Mod) PurgeOneCharacter(ctx context.Context, charID int64) error {
	if err := m.purgeGate(ctx, charID, "purge"); err != nil {
		return err
	}
	var acctID int64
	var name string
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(accountID, 0), characterName FROM character_ WHERE characterID = ?`,
		charID).Scan(&acctID, &name); err != nil {
		return fmt.Errorf("no such character %d: %w", charID, err)
	}

	// Same order as the range wipe: stop the sim owning it, take its ball out
	// of space, then delete rows.
	_ = setRolebotEnabled(ctx, m.db, charID, false)
	m.withdrawProjection(charID)
	if err := m.purgeCharacter(ctx, charID); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}

	if acctID != 0 {
		var left int64
		_ = m.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM character_ WHERE accountID = ?`, acctID).Scan(&left)
		if left == 0 {
			if _, err := m.db.ExecContext(ctx,
				`DELETE FROM account WHERE accountID = ?`, acctID); err != nil {
				return fmt.Errorf("%s deleted, but its now-empty account was not: %w", name, err)
			}
		}
	}
	m.syncPresence()
	return nil
}

// withdrawProjection takes a bot's ball out of space if it has one. Safe to
// call for a bot that is not embodied, and safe to call on a core with no
// embodiment support (the map is simply empty).
func (m *Mod) withdrawProjection(charID int64) {
	// Stamped before the registry changes, so a roster-wide reconcile holding an older roster does
	// not build the ball again (touchedSince).
	noteProjectionTouched(charID)
	embodyMu.Lock()
	eb, ok := embodyLive[charID]
	if ok {
		delete(embodyLive, charID)
	}
	embodyMu.Unlock()
	if !ok {
		return
	}
	// Off the lock: deEmbody persists the hull damage, and embodyMu is not held across a database call.
	m.deEmbody(eb)
}

// genPopulation counts the live generated bots and the sequence range they occupy.
type genPopulation struct {
	// Count is pilots. Accounts is how many RoleBot account rows exist and SeqAccounts how many distinct
	// numbers those pilots claim; a mismatch means an interrupted run or a hand-made account.
	Count       int
	MinSeq      int
	MaxSeq      int
	Enabled     int
	Accounts    int
	SeqAccounts int
	// PerAccount is charsPerAccount, so the page can do the arithmetic
	// without hardcoding it in a template.
	PerAccount int
}

func (m *Mod) generatedPopulation(ctx context.Context) (genPopulation, error) {
	p := genPopulation{PerAccount: charsPerAccount}
	var minSeq, maxSeq sql.NullInt64
	err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT g.seq), MIN(g.seq), MAX(g.seq),
		       COALESCE(SUM(CASE WHEN b.enabled = 1 THEN 1 ELSE 0 END), 0)
		FROM rolebot_generated g
		LEFT JOIN rolebot_characters b ON b.characterID = g.characterID`,
	).Scan(&p.Count, &p.SeqAccounts, &minSeq, &maxSeq, &p.Enabled)
	if err != nil {
		return p, fmt.Errorf("generatedPopulation: %w", err)
	}
	p.MinSeq, p.MaxSeq = int(minSeq.Int64), int(maxSeq.Int64)

	// Counted against the account table rather than inferred, so a half-wiped
	// or half-seeded state is visible rather than hidden.
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM account WHERE accountName LIKE ?`, botAccountPrefix+"%").Scan(&p.Accounts)
	return p, nil
}
