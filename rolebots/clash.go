//go:build mindclash

package rolebots

// Mind Clash: the bot crowd (docs/MIND_CLASH_BOTS.md). The sport is the MindClash module's; the
// rolebots population takes part as bettors on the tote and fans in the house, through the core API.
//
//	clash.go         the lane: knobs, hooks, events, the stats block
//	clash_travel.go  the venue errand: fly to Clash Night and stand in the house
//	clash_bets.go    the bettors and the fans
//	espclash.go      the ESP tab
//
// Traits are stored -100..+100 with the docile end low (traitCatalog); read them through the
// helpers carefulOf, patientOf, outlawOf, greedOf and aggressionOf (each 0..1).

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

const (
	settingClashEnabled     = "rolebots.clashEnabled"
	settingClashMaxFans     = "rolebots.clashMaxFans"
	settingClashBetMul      = "rolebots.clashBetMul"
	clashMaxFansDefault     = 30
	clashErrandRefreshTicks = 60
	clashBetTicks           = 600
)

var (
	clashOn      atomic.Bool
	clashMaxFans atomic.Int64
	clashBetMulX atomic.Int64 // ×1000
	clashBusy    atomic.Bool
	clashBetBusy atomic.Bool
)

func clashBetMul() float64 { return float64(clashBetMulX.Load()) / 1000 }

// ── the trait helpers (0..1 each) ──────────────────────────────────────────

type clashTraits map[string]int

func (t clashTraits) aggression() float64 { return posPole(traitScale(t[TraitAggression])) }
func (t clashTraits) careful() float64    { return posPole(-traitScale(t[TraitCaution])) }
func (t clashTraits) patient() float64    { return posPole(-traitScale(t[TraitPatience])) }
func (t clashTraits) outlaw() float64     { return posPole(traitScale(t[TraitLawfulness])) }
func (t clashTraits) lawful() float64     { return posPole(-traitScale(t[TraitLawfulness])) }
func (t clashTraits) greed() float64      { return posPole(traitScale(t[TraitGreed])) }

// clashTraitsOf reads one pilot's traits (cached for ten minutes).
func (m *Mod) clashTraitsOf(ctx context.Context, id int64) clashTraits {
	if v, ok := clashTraitCache.Load(id); ok {
		e := v.(clashTraitEntry)
		if clock.Now().Sub(e.at) < 10*time.Minute {
			return e.t
		}
	}
	tr, _ := loadTraitsBulk(ctx, m.db, []int64{id})
	t := clashTraits(tr[id])
	if t == nil {
		t = clashTraits{}
	}
	clashTraitCache.Store(id, clashTraitEntry{t: t, at: clock.Now()})
	return t
}

type clashTraitEntry struct {
	t  clashTraits
	at time.Time
}

var clashTraitCache sync.Map

// ── module-side state ──────────────────────────────────────────────────────

// rolebot_clash is the fan errand: which card a pilot is travelling to, and
// where it stands.
const clashTableDDL = `CREATE TABLE IF NOT EXISTS rolebot_clash (
	characterID BIGINT NOT NULL PRIMARY KEY,
	errandSlot  BIGINT NOT NULL DEFAULT 0,
	errandCard  BIGINT NOT NULL DEFAULT 0,
	errandKind  VARCHAR(8) NOT NULL DEFAULT '',
	venueStation BIGINT NOT NULL DEFAULT 0,
	venueSystem BIGINT NOT NULL DEFAULT 0,
	firstBell   BIGINT NOT NULL DEFAULT 0,
	errandState VARCHAR(10) NOT NULL DEFAULT '',
	joinedAt    BIGINT NOT NULL DEFAULT 0,
	note        VARCHAR(255) NOT NULL DEFAULT ''
) ENGINE=InnoDB`

const clashBetsDDL = `CREATE TABLE IF NOT EXISTS rolebot_clash_bets (
	cardID      BIGINT NOT NULL,
	characterID BIGINT NOT NULL,
	phase       TINYINT NOT NULL,
	staked      DOUBLE NOT NULL DEFAULT 0,
	at          BIGINT NOT NULL,
	PRIMARY KEY (cardID, characterID, phase)
) ENGINE=InnoDB`

// clashRow is a bot's rolebot_clash row.
type clashRow struct {
	CharID       int64
	ErrandSlot   int64
	ErrandCard   int64
	ErrandKind   string // "fan"
	VenueStation int64
	VenueSystem  int64
	FirstBell    int64
	ErrandState  string // due, travel, docked, done
	JoinedAt     int64
}

func (m *Mod) clashRowOf(ctx context.Context, id int64) clashRow {
	r := clashRow{CharID: id}
	_ = m.db.QueryRowContext(ctx, `SELECT errandSlot, errandCard, errandKind, venueStation, venueSystem,
		firstBell, errandState, joinedAt FROM rolebot_clash WHERE characterID = ?`, id).
		Scan(&r.ErrandSlot, &r.ErrandCard, &r.ErrandKind, &r.VenueStation, &r.VenueSystem,
			&r.FirstBell, &r.ErrandState, &r.JoinedAt)
	return r
}

func (m *Mod) clashTouch(ctx context.Context, id int64) {
	_, _ = m.db.ExecContext(ctx, `INSERT IGNORE INTO rolebot_clash (characterID, joinedAt) VALUES (?, ?)`, id, clock.Unix())
}

// ── the lane ───────────────────────────────────────────────────────────────

// startClash wires the module into Mind Clash. Safe on a core without the
// circuit's tables yet (they are created by the core after module init):
// every pass checks evedb.MCEnabled first.
func (m *Mod) startClash(ctx context.Context) {
	for _, ddl := range []string{clashTableDDL, clashBetsDDL} {
		if _, err := m.db.ExecContext(ctx, ddl); err != nil {
			rlogf("rolebots: clash table: %v", err)
		}
	}
	clashOn.Store(true)
	clashMaxFans.Store(clashMaxFansDefault)
	clashBetMulX.Store(1000)
	m.loadClashSettings(ctx)

	// The core asks which of the module's pilots are docked at a venue: a bot has no socket for
	// the session registry to see, and a fan in the house counts toward the Crowd.
	evedb.MCBotDockedAt = m.clashBotsDockedAt
	esp.MCBotStatsHook = m.clashStatLines
	evedb.SubscribeMindClash(m.onClashEvent)

	m.tick.RegisterPeriodic("rolebots-clash-errands", clashErrandRefreshTicks, func(uint64) bool {
		m.clashGo("errand refresh", &clashBusy, m.clashRefreshErrands)
		return true
	})
	m.tick.RegisterPeriodic("rolebots-clash-bets", clashBetTicks, func(uint64) bool {
		m.clashGo("betting", &clashBetBusy, m.clashBetPass)
		return true
	})
}

func (m *Mod) clashGo(name string, busy *atomic.Bool, fn func(context.Context)) {
	if !clashOn.Load() || !masterEnabled.Load() {
		return
	}
	if !busy.CompareAndSwap(false, true) {
		return
	}
	goSafe("clash "+name, func() {
		defer busy.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if !evedb.MCEnabled(ctx, m.db) {
			return
		}
		fn(ctx)
	})
}

func (m *Mod) loadClashSettings(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingClashEnabled); err == nil && ok {
		clashOn.Store(truthySetting(v))
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingClashMaxFans); err == nil && ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			clashMaxFans.Store(int64(n))
		}
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingClashBetMul); err == nil && ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0 {
			clashBetMulX.Store(int64(f * 1000))
		}
	}
}

// ── events ─────────────────────────────────────────────────────────────────

// onClashEvent answers the circuit: a settled card sends the fans home.
func (m *Mod) onClashEvent(ev evedb.MCEvent) {
	if !clashOn.Load() || !masterEnabled.Load() {
		return
	}
	if ev.Kind != evedb.MCEvCardSettled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m.clashCardSettled(ctx, ev.CardID)
}

// ── hooks the core calls ───────────────────────────────────────────────────

// clashBotsDockedAt lists the module's pilots docked at a station.
func (m *Mod) clashBotsDockedAt(stationID int64) []int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := m.db.QueryContext(ctx, `SELECT c.characterID FROM character_ c JOIN rolebot_characters r ON r.characterID = c.characterID
		WHERE c.stationID = ? AND r.enabled = 1`, stationID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil && !popIsOffline(id) {
			out = append(out, id)
		}
	}
	return out
}

// clashStatLines feeds the core admin page.
func (m *Mod) clashStatLines(ctx context.Context) []esp.SCCStatLine {
	var fans int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_clash WHERE errandSlot > 0 AND errandState <> 'done'`).Scan(&fans)
	var bettors int
	var staked float64
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT characterID), COALESCE(SUM(staked), 0) FROM rolebot_clash_bets WHERE at >= ?`,
		clock.Unix()-7*86400).Scan(&bettors, &staked)
	return []esp.SCCStatLine{
		{Label: "Fans travelling to a venue", Value: strconv.Itoa(fans)},
		{Label: "Bot bettors (7 days)", Value: fmt.Sprintf("%d staking %s", bettors, iskString(staked))},
		{Label: "Lane", Value: map[bool]string{true: "on", false: "off"}[clashOn.Load()]},
	}
}

func init() { registerCharForget("clashTraitCache", func(id int64) { clashTraitCache.Delete(id) }) }
