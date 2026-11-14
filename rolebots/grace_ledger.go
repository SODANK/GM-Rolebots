package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// The grace ledger: what a bot holds against, or owes, a named pilot. rolebot_grievances is the
// truth, one row per (bot, pilot) with a grudge and a regard channel, decayed by its own periodic.
// The client sees a band (grace_math.go) in chrNPCStandings, toward real players only, with one
// chrStandingTransactions line per crossing. Regard toward the bot's own leadership is read by the
// staff deal and the corp review, never by morale. docs/GRACE.md.

func ensureGraceTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS rolebot_grievances (
		botID     BIGINT NOT NULL,
		pilotID   BIGINT NOT NULL,
		grudge    DOUBLE NOT NULL DEFAULT 0,
		regard    DOUBLE NOT NULL DEFAULT 0,
		band      TINYINT NOT NULL DEFAULT 0,
		written   DOUBLE NOT NULL DEFAULT 0,
		lastKind  VARCHAR(12) NOT NULL DEFAULT '',
		lastAt    BIGINT NOT NULL DEFAULT 0,
		decayedAt BIGINT NOT NULL DEFAULT 0,
		bountyAt  BIGINT NOT NULL DEFAULT 0,
		events    INT NOT NULL DEFAULT 0,
		PRIMARY KEY (botID, pilotID),
		KEY idx_rbgr_pilot (pilotID),
		KEY idx_rbgr_decayed (decayedAt)
	)`)
	return err
}

type graceRow struct {
	BotID, PilotID              int64
	Grudge, Regard              float64
	Band                        int
	Written                     float64
	LastKind                    string
	LastAt, DecayedAt, BountyAt int64
	Events                      int
}

const graceRowCols = `botID, pilotID, grudge, regard, band, written, lastKind, lastAt, decayedAt, bountyAt, events`

func scanGraceRow(sc interface{ Scan(...any) error }) (graceRow, error) {
	var r graceRow
	err := sc.Scan(&r.BotID, &r.PilotID, &r.Grudge, &r.Regard, &r.Band, &r.Written,
		&r.LastKind, &r.LastAt, &r.DecayedAt, &r.BountyAt, &r.Events)
	return r, err
}

func (m *Mod) graceRowOf(ctx context.Context, botID, pilotID int64) (graceRow, bool) {
	r, err := scanGraceRow(m.db.QueryRowContext(ctx,
		`SELECT `+graceRowCols+` FROM rolebot_grievances WHERE botID = ? AND pilotID = ?`, botID, pilotID))
	return r, err == nil
}

func (m *Mod) graceSaveRow(ctx context.Context, r graceRow) error {
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_grievances (`+graceRowCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE grudge = VALUES(grudge), regard = VALUES(regard), band = VALUES(band),
		  written = VALUES(written), lastKind = VALUES(lastKind), lastAt = VALUES(lastAt),
		  decayedAt = VALUES(decayedAt), bountyAt = VALUES(bountyAt), events = VALUES(events)`,
		r.BotID, r.PilotID, r.Grudge, r.Regard, r.Band, r.Written, r.LastKind, r.LastAt, r.DecayedAt, r.BountyAt, r.Events)
	return err
}

// ── the in-memory view, read on the combat goroutine ────────────────────────

// graceOpinionFloor is the smallest |value| the map carries; under it a pair reads as nobody.
const graceOpinionFloor = 2.0

var (
	graceMu sync.RWMutex
	// graceOpinion is bot → pilot effective value (character IDs), |v| >= graceOpinionFloor only.
	graceOpinion = map[grudgeKey]float64{}
	// graceWorst is the lowest opinion each bot (character ID) holds; absent means none under zero.
	graceWorst = map[int64]float64{}
	// playerRegard is a player's own chrStandings value toward a bot, keyed (bot, player) by character ID.
	playerRegard = map[grudgeKey]float64{}
	// gracePairSeen is the pair cooldown, (bot, pilot, kind) → unix; swept by the periodic.
	gracePairSeen sync.Map
)

type gracePairKey struct {
	bot, pilot int64
	kind       graceKind
}

func init() {
	registerCharForget("graceOpinion/graceWorst/playerRegard/gracePairSeen", func(charID int64) {
		graceMu.Lock()
		for k := range graceOpinion {
			if k.bot == charID || k.pilot == charID {
				delete(graceOpinion, k)
			}
		}
		for k := range playerRegard {
			if k.bot == charID || k.pilot == charID {
				delete(playerRegard, k)
			}
		}
		delete(graceWorst, charID)
		for bot := range graceWorst {
			graceWorst[bot] = graceWorstOfLocked(bot)
		}
		graceMu.Unlock()
		gracePairSeen.Range(func(k, _ any) bool {
			if pk := k.(gracePairKey); pk.bot == charID || pk.pilot == charID {
				gracePairSeen.Delete(k)
			}
			return true
		})
	})
}

func graceWorstOfLocked(bot int64) float64 {
	worst := 0.0
	for k, v := range graceOpinion {
		if k.bot == bot && v < worst {
			worst = v
		}
	}
	return worst
}

// graceRemember updates the map for one pair; |eff| under the floor drops it.
func graceRemember(bot, pilot int64, eff float64) {
	graceMu.Lock()
	defer graceMu.Unlock()
	k := grudgeKey{bot, pilot}
	old, had := graceOpinion[k]
	if math.Abs(eff) < graceOpinionFloor {
		delete(graceOpinion, k)
	} else {
		graceOpinion[k] = eff
	}
	switch {
	case eff < graceWorst[bot]:
		graceWorst[bot] = eff
	case had && old == graceWorst[bot]:
		if w := graceWorstOfLocked(bot); w < 0 {
			graceWorst[bot] = w
		} else {
			delete(graceWorst, bot)
		}
	}
}

// graceOpinionOf is the bot's own ledger value for a pilot, 0 for none. Map read only.
func graceOpinionOf(bot, pilot int64) float64 {
	graceMu.RLock()
	defer graceMu.RUnlock()
	if len(graceOpinion) == 0 {
		return 0
	}
	return graceOpinion[grudgeKey{bot, pilot}]
}

func graceWorstFor(bot int64) float64 {
	graceMu.RLock()
	defer graceMu.RUnlock()
	return graceWorst[bot]
}

// personalHasRivals opens the sighted-hostiles gate for a bot with a disliked pilot on its ledger.
func personalHasRivals(bot int64) bool { return graceWorstFor(bot) <= StandingDisliked }

func playerRegardOf(bot, player int64) float64 {
	graceMu.RLock()
	defer graceMu.RUnlock()
	if len(playerRegard) == 0 {
		return 0
	}
	return playerRegard[grudgeKey{bot, player}]
}

// gracePersonal is the personal arm both standing folds read: the ledger plus a share of what
// the pilot declared toward this bot. Map reads only.
func gracePersonal(bot, other int64) float64 {
	v := graceOpinionOf(bot, other)
	if p := playerRegardOf(bot, other); p != 0 {
		v += gracePlayerRegardMul() * p
	}
	return math.Max(-10, math.Min(10, v))
}

// loadGraceOpinions fills the map from the rows that clear the floor, once at boot.
func (m *Mod) loadGraceOpinions(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `SELECT botID, pilotID, grudge, regard FROM rolebot_grievances
		WHERE ABS(grudge + regard) >= ?`, graceOpinionFloor)
	if err != nil {
		rlogf("rolebots: grace opinions: %v", err)
		return
	}
	defer rows.Close()
	graceMu.Lock()
	defer graceMu.Unlock()
	graceOpinion = map[grudgeKey]float64{}
	graceWorst = map[int64]float64{}
	n := 0
	for rows.Next() {
		var bot, pilot int64
		var g, r float64
		if rows.Scan(&bot, &pilot, &g, &r) != nil {
			continue
		}
		eff := graceEff(g, r)
		graceOpinion[grudgeKey{bot, pilot}] = eff
		if eff < graceWorst[bot] {
			graceWorst[bot] = eff
		}
		n++
	}
	if n > 0 {
		rlogf("rolebots: grace ledger — %d opinion(s) loaded", n)
	}
}

// refreshPlayerRegard reads what players declared toward bots (chrStandings), whole, every pass.
func (m *Mod) refreshPlayerRegard(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT cs.characterID, cs.toID, cs.standing
		  FROM chrStandings cs
		  JOIN rolebot_characters rb ON rb.characterID = cs.toID
		  LEFT JOIN rolebot_characters rp ON rp.characterID = cs.characterID
		 WHERE rp.characterID IS NULL AND cs.standing <> 0`)
	if err != nil {
		return
	}
	defer rows.Close()
	next := map[grudgeKey]float64{}
	for rows.Next() {
		var player, bot int64
		var v float64
		if rows.Scan(&player, &bot, &v) == nil {
			next[grudgeKey{bot, player}] = v
		}
	}
	graceMu.Lock()
	playerRegard = next
	graceMu.Unlock()
}

// graceClaim is the pair cooldown: the same kind between the same two within the window is one event.
func graceClaim(bot, pilot int64, kind graceKind, now int64) bool {
	win := int64(600)
	switch kind {
	case gkAggress:
		win = 3600
	case gkFavour, gkRansomOK, gkRobbery:
		win = 60
	}
	k := gracePairKey{bot, pilot, kind}
	if v, ok := gracePairSeen.Load(k); ok && now-v.(int64) < win {
		return false
	}
	gracePairSeen.Store(k, now)
	return true
}

func graceSweepPairs(now int64) {
	gracePairSeen.Range(func(k, v any) bool {
		if now-v.(int64) > 3600 {
			gracePairSeen.Delete(k)
		}
		return true
	})
}

// ── writes ──────────────────────────────────────────────────────────────────

var graceWriteMu sync.Mutex

// noteGraceEvent files one event on a bot's ledger toward a pilot and projects the result. Runs off
// the combat goroutine (callers wrap it in goSafe).
func (m *Mod) noteGraceEvent(ctx context.Context, botID, pilotID int64, kind graceKind, f fightFacts, systemID, shipTypeID int64) {
	if m.db == nil || botID == 0 || pilotID == 0 || botID == pilotID || !m.isSynthetic(botID) {
		return
	}
	now := clock.Unix()
	if !graceClaim(botID, pilotID, kind, now) {
		return
	}
	traits, err := getTraits(ctx, m.db, botID)
	if err != nil {
		return
	}
	tn := resolveTuning(traits)
	dG, dR := graceDelta(kind, tn, f, graceKillImpulse())
	if dG == 0 && dR == 0 {
		return
	}
	graceWriteMu.Lock()
	defer graceWriteMu.Unlock()
	row, found := m.graceRowOf(ctx, botID, pilotID)
	if found {
		row.Grudge, row.Regard = graceDecayed(tn, row.Grudge, row.Regard, float64(now-row.DecayedAt))
	}
	row.BotID, row.PilotID = botID, pilotID
	row.Grudge = math.Max(graceGrudgeFloor, row.Grudge+dG)
	row.Regard = math.Min(graceRegardCeil, row.Regard+dR)
	row.LastKind, row.LastAt, row.DecayedAt = string(kind), now, now
	row.Events++
	if err := m.graceSaveRow(ctx, row); err != nil {
		rlogf("rolebots: grace save %d→%d: %v", botID, pilotID, err)
		return
	}
	m.logBot(botID, logInfo, "grace", "%s by %s: grudge %.1f, regard %.1f",
		kind, m.charName(ctx, pilotID), row.Grudge, row.Regard)
	m.projectGrace(ctx, &row, tn, kind, systemID, shipTypeID)
}

// gracePlayerChar: a character with an account that is not on the roster.
func (m *Mod) gracePlayerChar(ctx context.Context, charID int64) bool {
	var player bool
	err := m.db.QueryRowContext(ctx, `
		SELECT c.accountID > 0 AND rb.characterID IS NULL
		  FROM character_ c LEFT JOIN rolebot_characters rb ON rb.characterID = c.characterID
		 WHERE c.characterID = ?`, charID).Scan(&player)
	return err == nil && player
}

// projectGrace refreshes the map and, on a band crossing, the client's rows for a player.
func (m *Mod) projectGrace(ctx context.Context, row *graceRow, tn Tuning, kind graceKind, systemID, shipTypeID int64) {
	eff := graceEff(row.Grudge, row.Regard)
	graceRemember(row.BotID, row.PilotID, eff)
	band := graceBand(eff)
	if band == row.Band {
		return
	}
	old := row.Band
	want := graceBandValue(band)
	row.Written = 0
	if m.gracePlayerChar(ctx, row.PilotID) {
		if band != 0 && (old == 0 || (old < 0) != (band < 0)) &&
			!m.graceMakeRoom(ctx, row.BotID, row.PilotID, band < 0, math.Abs(eff)) {
			// No room: the pair stays off the client table, as an evicted row does.
			if old != 0 {
				_ = evedb.SetNPCStandingAbs(ctx, m.db, row.PilotID, row.BotID, 0)
				row.Band = 0
				_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_grievances SET band = 0, written = 0 WHERE botID = ? AND pilotID = ?`,
					row.BotID, row.PilotID)
			}
			return
		}
		if err := evedb.SetNPCStandingAbs(ctx, m.db, row.PilotID, row.BotID, want); err != nil {
			rlogf("rolebots: grace standing %d→%d: %v", row.BotID, row.PilotID, err)
			return
		}
		ship := ""
		if shipTypeID != 0 {
			ship, _ = evedb.GetTypeName(ctx, m.db, shipTypeID)
		}
		system := ""
		if systemID != 0 {
			system = evedb.GetSolarSystemName(ctx, m.db, systemID)
		}
		_ = evedb.InsertStandingTransaction(ctx, m.db, evedb.EventStandingPlayerSetStanding,
			row.BotID, row.PilotID, want-graceBandValue(old), systemID, shipTypeID, 0, graceMsg(kind, ship, system))
		row.Written = want
	}
	row.Band = band
	if _, err := m.db.ExecContext(ctx, `UPDATE rolebot_grievances SET band = ?, written = ? WHERE botID = ? AND pilotID = ?`,
		row.Band, row.Written, row.BotID, row.PilotID); err != nil {
		rlogf("rolebots: grace band %d→%d: %v", row.BotID, row.PilotID, err)
	}
	label := graceBandLabel(band)
	if label == "" {
		label = "nobody in particular"
	}
	m.logBot(row.BotID, logInfo, "grace", "%s is now %s (%.1f)", m.charName(ctx, row.PilotID), label, eff)
	m.graceCrossed(ctx, row, tn, old, band, systemID)
}

// graceMakeRoom drops the weakest projected rows on one side so a new one of |eff| strength fits
// under the cap, and reports whether it fits. A newcomer no stronger than every row it would drop
// does not fit and nothing is dropped; otherwise an evicted row, re-read by the decay pass, would
// push out a stronger one each hour.
func (m *Mod) graceMakeRoom(ctx context.Context, botID, keep int64, negative bool, strength float64) bool {
	cmp, cap := "> 0", gracePosCap()
	if negative {
		cmp, cap = "< 0", graceNegCap()
	}
	rows, err := m.db.QueryContext(ctx, `SELECT pilotID, LEAST(ABS(grudge + regard), 10) FROM rolebot_grievances
		WHERE botID = ? AND band `+cmp+` AND pilotID <> ? ORDER BY ABS(grudge + regard) ASC`, botID, keep)
	if err != nil {
		return true
	}
	var pilots []int64
	var strengths []float64
	for rows.Next() {
		var p int64
		var s float64
		if rows.Scan(&p, &s) == nil {
			pilots = append(pilots, p)
			strengths = append(strengths, s)
		}
	}
	rows.Close()
	n := min(len(pilots), graceEvictN(len(pilots), cap))
	if n == 0 {
		return true
	}
	if !graceDisplaces(strength, strengths[n-1]) {
		return false
	}
	for _, p := range pilots[:n] {
		_ = evedb.SetNPCStandingAbs(ctx, m.db, p, botID, 0)
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_grievances SET band = 0, written = 0 WHERE botID = ? AND pilotID = ?`, botID, p)
	}
	return true
}

// graceCrossed is what a crossing into the feud or blood band sets off beyond the client row: a
// word to the CEO's relations ledger, and from a vengeful pilot a price on a player's head.
func (m *Mod) graceCrossed(ctx context.Context, row *graceRow, tn Tuning, old, band int, systemID int64) {
	if band > -2 || band >= old {
		return
	}
	botCorp, pilotCorp := m.charCorpID(ctx, row.BotID), m.charCorpID(ctx, row.PilotID)
	weight := -1.0
	if band == -3 {
		weight = -2.0
	}
	m.noteCorpIncident(ctx, botCorp, pilotCorp, incFeud, relSideVictim, weight, 0, systemID, row.PilotID,
		fmt.Sprintf("feud:%d:%d:%d", row.BotID, row.PilotID, band), m.charName(ctx, row.PilotID))

	if tn.Vindictiveness < graceVengeful || row.Written == 0 || clock.Unix()-row.BountyAt < 7*86400 {
		return
	}
	bal, err := evedb.GetCharBalance(ctx, m.db, row.BotID)
	if err != nil {
		return
	}
	price := math.Min(float64(PirateBountyISK())*(1+tn.Vindictiveness), math.Min(graceBountyMaxISK(), bal*0.10))
	price = math.Floor(price/1000) * 1000
	if price < 1000 {
		return
	}
	if err := evedb.AddToBounty(ctx, m.db, row.BotID, row.PilotID, price); err != nil {
		return
	}
	name := m.charName(ctx, row.PilotID)
	// The journal balance is read after the debit, so a credit landing between the two reads is in it.
	if after, err := evedb.GetCharBalance(ctx, m.db, row.BotID); err == nil {
		bal = after + price
	}
	_ = evedb.InsertJournalEntry(ctx, m.db, row.BotID, 17, row.BotID, row.PilotID, -price, bal-price,
		evedb.JournalReason("Bounty placed on "+name))
	row.BountyAt = clock.Unix()
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_grievances SET bountyAt = ? WHERE botID = ? AND pilotID = ?`,
		row.BountyAt, row.BotID, row.PilotID)
	m.logBot(row.BotID, logInfo, "grace", "put %.0f ISK on %s's head", price, name)
}

// ── the periodic ────────────────────────────────────────────────────────────

var graceSweepBusy atomic.Bool

// startGraceSweep registers the decay: rows not touched for an hour fade on their half-lives, bands
// that fall are re-projected, and empty rows go.
func (m *Mod) startGraceSweep() {
	m.registerPeriodic("rolebot-grace-decay", 600, func(uint64) bool {
		if !Master() {
			return true
		}
		if !graceSweepBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("grace decay", func() {
			defer graceSweepBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			m.graceDecayPass(ctx)
			m.refreshPlayerRegard(ctx)
			graceSweepPairs(clock.Unix())
		})
		return true
	})
}

func (m *Mod) graceDecayPass(ctx context.Context) {
	now := clock.Unix()
	rows, err := m.db.QueryContext(ctx, `SELECT `+graceRowCols+` FROM rolebot_grievances
		WHERE decayedAt < ? ORDER BY decayedAt LIMIT 500`, now-3600)
	if err != nil {
		return
	}
	var due []graceRow
	for rows.Next() {
		if r, err := scanGraceRow(rows); err == nil {
			due = append(due, r)
		}
	}
	rows.Close()
	tunes := map[int64]Tuning{}
	for _, r := range due {
		tn, ok := tunes[r.BotID]
		if !ok {
			traits, err := getTraits(ctx, m.db, r.BotID)
			if err != nil {
				continue
			}
			tn = resolveTuning(traits)
			tunes[r.BotID] = tn
		}
		r.Grudge, r.Regard = graceDecayed(tn, r.Grudge, r.Regard, float64(now-r.DecayedAt))
		r.DecayedAt = now
		if r.Band == 0 && math.Abs(graceEff(r.Grudge, r.Regard)) < 0.1 {
			_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_grievances WHERE botID = ? AND pilotID = ?`, r.BotID, r.PilotID)
			graceRemember(r.BotID, r.PilotID, 0)
			continue
		}
		if _, err := m.db.ExecContext(ctx, `UPDATE rolebot_grievances SET grudge = ?, regard = ?, decayedAt = ?
			WHERE botID = ? AND pilotID = ?`, r.Grudge, r.Regard, r.DecayedAt, r.BotID, r.PilotID); err != nil {
			continue
		}
		m.projectGrace(ctx, &r, tn, "", 0, 0)
	}
}
