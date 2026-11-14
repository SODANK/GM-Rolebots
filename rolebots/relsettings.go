package rolebots

// The corp-relations dials. Each has a key const, an atomic cache read on the
// hot path, a clamped accessor, a never-fatal boot loader and a setter that
// writes the setting before the cache, so a failed write leaves the roster on
// the old value. Two dials are read from the embodied combat loop, which may
// not touch the database, so every read is atomic.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

const (
	// settingRelEnabled is the master switch for the corp-politics layer. Off:
	// both standing seams fall back to their FactionID-0 short-circuit, no system
	// is enemy-held, and no war lifts the policed-space refusal.
	settingRelEnabled = "rolebots.relations.enabled"
	// settingRelMutual makes a one-sided declaration a two-sided war of opinion:
	// the worse of what each corporation said governs both. Off, only the
	// declarer acts. Read at snapshot build, so a change applies on the next refresh.
	settingRelMutual = "rolebots.relations.mutual"
	// settingRelEnemyCost is what a system an enemy corporation works or keeps an
	// address in is worth flying around, in jumps (scoring.go). It is a penalty,
	// never a bonus: beltSearchFloor needs every unmodelled term non-negative.
	settingRelEnemyCost = "rolebots.relations.enemyCost"
	// settingRelEnemyHop is the same for a route: what passing through a system a
	// rival works adds to a leg, in jump-equivalents (router.go hopCost). It is
	// far smaller than the belt cost.
	settingRelEnemyHop = "rolebots.relations.enemyHopJumps"
	// settingRelRefreshSec is how often the snapshot is rebuilt.
	settingRelRefreshSec = "rolebots.relations.refreshSec"
	// settingWarEverywhere: on, a live war is a second path out of BandPoliced
	// beside the criminal flag, matching the core, where seclaw exempts a war
	// shot from the security ladder and from CONCORD.
	settingWarEverywhere = "rolebots.war.everywhere"
	// settingWarOpportunityMul multiplies the combat activity's opportunity for a
	// corporation at war. It does not bypass the wing cap.
	settingWarOpportunityMul = "rolebots.war.opportunityMul"
	// settingCEODiplomacy lets bot CEOs declare friends, allies and rivals
	// (corpceo_diplomacy.go). Off, the power does nothing and declarations
	// already written stay.
	settingCEODiplomacy = "rolebots.relations.ceoDiplomacy"
)

// Defaults and ranges.
const (
	// relEnemyCostDefault sits between beltHostileCost (25) and beltEncounterCost
	// (12): an enemy corporation working a system is firmer than a rumour and
	// softer than a scar.
	relEnemyCostDefault = 20.0
	relEnemyCostMin     = 0.0
	relEnemyCostMax     = 120.0

	// relEnemyHopDefault is a detour, not a wall: enough to take another lane
	// when one exists, not enough to strand a pilot whose only route runs
	// through an enemy system.
	relEnemyHopDefault = 3
	relEnemyHopMin     = 0
	relEnemyHopMax     = 50

	// relRefreshDefaultSec is the snapshot period. The core's war legality cache
	// has a 60-second TTL, so a two-minute view is no regression; relAtWar
	// re-runs War.Hostile against the clock on every read, so arming stays exact.
	relRefreshDefaultSec = 120
	relRefreshMinSec     = 30
	relRefreshMaxSec     = 3600

	// warOpportunityMulDefault must stay above 1 or the dial does nothing. It is
	// capped because opportunity is one factor of a product (appetite.go): a large
	// multiplier leaves a corporation unable to do anything else.
	warOpportunityMulDefault = 1.5
	warOpportunityMulMin     = 1.0
	warOpportunityMulMax     = 4.0
)

var (
	relEnabledV    atomic.Bool
	relMutualV     atomic.Bool
	warEverywhereV atomic.Bool
	ceoDiplomacyV  atomic.Bool
	relEnemyCostV  atomic.Int64 // hundredths; 0 = the shipped default
	relRefreshSecV atomic.Int64 // seconds; 0 = the shipped default
	relEnemyHopV   atomic.Int64 // jumps; -1 = explicitly zero, 0 = the default
	warOppMulV     atomic.Int64 // hundredths; 0 = the shipped default
)

func relationsEnabled() bool { return relEnabledV.Load() }
func relationsMutual() bool  { return relMutualV.Load() }
func warEverywhere() bool    { return warEverywhereV.Load() }
func ceoDiplomacyOn() bool   { return ceoDiplomacyV.Load() }

func relEnemyCost() float64 {
	if v := relEnemyCostV.Load(); v > 0 {
		return float64(v) / 100
	}
	return relEnemyCostDefault
}

// relEnemyHopJumps is the routing surcharge. The cache stores -1 for an
// explicit 0, because 0 in the cache means unset (as with the ore dials).
func relEnemyHopJumps() int {
	switch v := relEnemyHopV.Load(); {
	case v < 0:
		return 0
	case v > 0:
		return int(v)
	}
	return relEnemyHopDefault
}

func clampRelEnemyHop(v int) int {
	if v < relEnemyHopMin {
		return relEnemyHopMin
	}
	if v > relEnemyHopMax {
		return relEnemyHopMax
	}
	return v
}

func relRefreshSec() int {
	if v := relRefreshSecV.Load(); v > 0 {
		return int(v)
	}
	return relRefreshDefaultSec
}

func warOpportunityMul() float64 {
	if v := warOppMulV.Load(); v > 0 {
		return float64(v) / 100
	}
	return warOpportunityMulDefault
}

func clampRelEnemyCost(v float64) float64 { return clampRange(v, relEnemyCostMin, relEnemyCostMax) }
func clampWarOppMul(v float64) float64 {
	return clampRange(v, warOpportunityMulMin, warOpportunityMulMax)
}

func clampRelRefreshSec(v int) int {
	if v < relRefreshMinSec {
		return relRefreshMinSec
	}
	if v > relRefreshMaxSec {
		return relRefreshMaxSec
	}
	return v
}

// loadRelationsSettings reads every dial at boot. Never fatal: an unreadable
// dial reads as its default. The three switches default on, since the layer has
// no effect until a standing or war is declared and an off default would
// protect nothing.
func (m *Mod) loadRelationsSettings(ctx context.Context) {
	relEnabledV.Store(true)
	relMutualV.Store(true)
	warEverywhereV.Store(true)
	// Also on: it acts only through a CEO the operator installed, which has its
	// own per-power switch.
	ceoDiplomacyV.Store(true)

	readBool := func(key string, cache *atomic.Bool) {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, key)
		if err != nil {
			rlogf("rolebots: read %s: %v (using the default)", key, err)
			return
		}
		if ok {
			cache.Store(strings.TrimSpace(v) == "1")
		}
	}
	readBool(settingRelEnabled, &relEnabledV)
	readBool(settingRelMutual, &relMutualV)
	readBool(settingWarEverywhere, &warEverywhereV)
	readBool(settingCEODiplomacy, &ceoDiplomacyV)

	readFloat := func(key string, clamp func(float64) float64, cache *atomic.Int64) {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, key)
		if err != nil {
			rlogf("rolebots: read %s: %v (using the default)", key, err)
			return
		}
		if !ok {
			return
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			cache.Store(int64(clamp(f) * 100))
		}
	}
	readFloat(settingRelEnemyCost, clampRelEnemyCost, &relEnemyCostV)
	readFloat(settingWarOpportunityMul, clampWarOppMul, &warOppMulV)

	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingRelEnemyHop); err == nil && ok {
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err == nil && n >= 0 {
			n = clampRelEnemyHop(n)
			if n == 0 {
				relEnemyHopV.Store(-1)
			} else {
				relEnemyHopV.Store(int64(n))
			}
		}
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingRelRefreshSec); err == nil && ok {
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err == nil && n > 0 {
			relRefreshSecV.Store(int64(clampRelRefreshSec(n)))
		}
	}
	// The ledger's own switches and dials (relledgersettings.go).
	m.loadRelLedgerSettings(ctx)
	// The grace ledger's (gracesettings.go).
	m.loadGraceSettings(ctx)
}

// setRelationsSwitch persists one of the three switches.
func (m *Mod) setRelationsSwitch(ctx context.Context, key string, on bool) error {
	var cache *atomic.Bool
	switch key {
	case settingRelEnabled:
		cache = &relEnabledV
	case settingRelMutual:
		cache = &relMutualV
	case settingWarEverywhere:
		cache = &warEverywhereV
	case settingCEODiplomacy:
		cache = &ceoDiplomacyV
	default:
		return fmt.Errorf("setRelationsSwitch: %s is not a relations switch", key)
	}
	val := "0"
	if on {
		val = "1"
	}
	if err := evedb.SetServerSetting(ctx, m.db, key, val); err != nil {
		return fmt.Errorf("setRelationsSwitch %s: %w", key, err)
	}
	cache.Store(on)
	return nil
}

// setRelationsNumber persists one of the three numeric dials.
func (m *Mod) setRelationsNumber(ctx context.Context, key string, v float64) error {
	switch key {
	case settingRelEnemyCost:
		v = clampRelEnemyCost(v)
		if err := evedb.SetServerSetting(ctx, m.db, key,
			strconv.FormatFloat(v, 'f', 2, 64)); err != nil {
			return fmt.Errorf("setRelationsNumber %s: %w", key, err)
		}
		relEnemyCostV.Store(int64(v * 100))
	case settingWarOpportunityMul:
		v = clampWarOppMul(v)
		if err := evedb.SetServerSetting(ctx, m.db, key,
			strconv.FormatFloat(v, 'f', 2, 64)); err != nil {
			return fmt.Errorf("setRelationsNumber %s: %w", key, err)
		}
		warOppMulV.Store(int64(v * 100))
	case settingRelEnemyHop:
		n := clampRelEnemyHop(int(v))
		if err := evedb.SetServerSetting(ctx, m.db, key, strconv.Itoa(n)); err != nil {
			return fmt.Errorf("setRelationsNumber %s: %w", key, err)
		}
		if n == 0 {
			relEnemyHopV.Store(-1)
		} else {
			relEnemyHopV.Store(int64(n))
		}
	case settingRelRefreshSec:
		n := clampRelRefreshSec(int(v))
		if err := evedb.SetServerSetting(ctx, m.db, key, strconv.Itoa(n)); err != nil {
			return fmt.Errorf("setRelationsNumber %s: %w", key, err)
		}
		relRefreshSecV.Store(int64(n))
	default:
		return fmt.Errorf("setRelationsNumber: %s is not a relations dial", key)
	}
	return nil
}
