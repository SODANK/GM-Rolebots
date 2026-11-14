package rolebots

// The dials of the relations ledger, the dynamic layer over the corp politics in
// relsettings.go. Each has a key const, an atomic cache for the hot path, a
// clamped accessor, a never-fatal boot loader and a setter that writes the
// setting before the cache. relDials is a table so the Relations page can loop
// it. A float dial is cached x1000 beside a set flag, because crowdFloor can be
// negative and 0 cannot mean unset.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

const (
	// settingRelLedger is the master switch for the dynamic layer. Off, no
	// incident is folded, nothing is flushed and no letter is sent; the
	// standings already written stay where they are.
	settingRelLedger = "rolebots.relations.ledger"
	// settingRelLetters: envoy letters at all (pacts, reparations, peace).
	settingRelLetters = "rolebots.relations.letters"
	// settingRelEnvoys: off, a letter is mailed from the desk with no trip.
	settingRelEnvoys = "rolebots.relations.envoys"

	settingRelFoldSec        = "rolebots.relations.foldSec"
	settingRelFlushMax       = "rolebots.relations.flushMax"
	settingRelIncidentDays   = "rolebots.relations.incidentDays"
	settingRelKillImpulse    = "rolebots.relations.killImpulse"
	settingRelAggressorShare = "rolebots.relations.aggressorShare"
	settingRelCrowdWindowMin = "rolebots.relations.crowdWindowMin"
	settingRelCrowdFloor     = "rolebots.relations.crowdFloor"
	settingRelPactMaxDays    = "rolebots.relations.pactMaxDays"
)

// Shipped defaults the pure arithmetic also reads (relDefaultParams).
const (
	relAggressorShareDefault = 0.15
	relCrowdFloorDefault     = -5.0
	relFoldSecDefault        = 600
	relIncidentDaysDefault   = 90
)

var (
	relLedgerV  atomic.Bool
	relLettersV atomic.Bool
	relEnvoysV  atomic.Bool

	relFoldSecV, relFoldSecSet               atomic.Int64
	relFlushMaxV, relFlushMaxSet             atomic.Int64
	relIncidentDaysV, relIncidentDaysSet     atomic.Int64
	relKillImpulseV, relKillImpulseSet       atomic.Int64
	relAggressorShareV, relAggressorShareSet atomic.Int64
	relCrowdWindowV, relCrowdWindowSet       atomic.Int64
	relCrowdFloorV, relCrowdFloorSet         atomic.Int64
	relPactMaxDaysV, relPactMaxDaysSet       atomic.Int64
)

func relLedgerOn() bool  { return relLedgerV.Load() }
func relLettersOn() bool { return relLettersV.Load() }
func relEnvoysOn() bool  { return relEnvoysV.Load() }

// relDial is one numeric ledger dial. Integer dials carry whole numbers;
// the cache holds every value ×1000.
type relDial struct {
	key           string
	label         string
	def, min, max float64
	integer       bool
	cache, set    *atomic.Int64
}

func relDials() []relDial {
	return []relDial{
		{settingRelFoldSec, "Fold incidents every (seconds)", relFoldSecDefault, 60, 3600, true, &relFoldSecV, &relFoldSecSet},
		{settingRelFlushMax, "Standings written per flush, at most", 200, 10, 1000, true, &relFlushMaxV, &relFlushMaxSet},
		{settingRelIncidentDays, "Keep incidents for (days)", relIncidentDaysDefault, 7, 365, true, &relIncidentDaysV, &relIncidentDaysSet},
		{settingRelKillImpulse, "Violence impulse ×", 1.0, 0, 5, false, &relKillImpulseV, &relKillImpulseSet},
		{settingRelAggressorShare, "Aggressor's own ledger takes ×", relAggressorShareDefault, 0, 1, false, &relAggressorShareV, &relAggressorShareSet},
		{settingRelCrowdWindowMin, "Belt crowding memory (minutes)", 60, 5, 720, true, &relCrowdWindowV, &relCrowdWindowSet},
		{settingRelCrowdFloor, "Crowding alone stops at", relCrowdFloorDefault, -7.4, 0, false, &relCrowdFloorV, &relCrowdFloorSet},
		{settingRelPactMaxDays, "Longest pact (days)", 30, 1, 365, true, &relPactMaxDaysV, &relPactMaxDaysSet},
	}
}

func (d relDial) clamp(v float64) float64 {
	v = clampRange(v, d.min, d.max)
	if d.integer {
		v = float64(int64(v + 0.5))
	}
	return v
}

func (d relDial) get() float64 {
	if d.set.Load() != 0 {
		return float64(d.cache.Load()) / 1000
	}
	return d.def
}

func (d relDial) String() string {
	if d.integer {
		return strconv.FormatInt(int64(d.get()), 10)
	}
	return strconv.FormatFloat(d.get(), 'f', 2, 64)
}

func relDialValue(key string) float64 {
	for _, d := range relDials() {
		if d.key == key {
			return d.get()
		}
	}
	return 0
}

func relFoldSec() int            { return int(relDialValue(settingRelFoldSec)) }
func relFlushMax() int           { return int(relDialValue(settingRelFlushMax)) }
func relIncidentDays() int       { return int(relDialValue(settingRelIncidentDays)) }
func relKillImpulse() float64    { return relDialValue(settingRelKillImpulse) }
func relAggressorShare() float64 { return relDialValue(settingRelAggressorShare) }
func relCrowdWindowSec() int64   { return int64(relDialValue(settingRelCrowdWindowMin)) * 60 }
func relCrowdFloor() float64     { return relDialValue(settingRelCrowdFloor) }
func relPactMaxDays() int        { return int(relDialValue(settingRelPactMaxDays)) }

// relParamsNow is the dials as a value, for the pure fold.
func relParamsNow() relParams {
	return relParams{KillImpulse: relKillImpulse(), AggressorShare: relAggressorShare(), CrowdFloor: relCrowdFloor()}
}

// relDialRow is one dial as the Relations page draws it (no funcMap:
// preformatted).
type relDialRow struct {
	Key, Label, Value, Step string
}

func relDialRows() []relDialRow {
	var out []relDialRow
	for _, d := range relDials() {
		step := "0.01"
		if d.integer {
			step = "1"
		}
		out = append(out, relDialRow{Key: d.key, Label: d.label, Value: d.String(), Step: step})
	}
	return out
}

// isRelLedgerSwitch / isRelLedgerDial route the Relations page's posts.
func isRelLedgerSwitch(key string) bool {
	return key == settingRelLedger || key == settingRelLetters || key == settingRelEnvoys
}

func isRelLedgerDial(key string) bool {
	for _, d := range relDials() {
		if d.key == key {
			return true
		}
	}
	return false
}

// loadRelLedgerSettings reads every ledger dial at boot. Never fatal. The three
// switches default on: the layer does nothing until a CEO is installed and
// something happens between two corporations.
func (m *Mod) loadRelLedgerSettings(ctx context.Context) {
	for _, sw := range []struct {
		key   string
		cache *atomic.Bool
	}{{settingRelLedger, &relLedgerV}, {settingRelLetters, &relLettersV}, {settingRelEnvoys, &relEnvoysV}} {
		sw.cache.Store(true)
		v, ok, err := evedb.GetServerSetting(ctx, m.db, sw.key)
		if err != nil {
			rlogf("rolebots: read %s: %v (using the default)", sw.key, err)
			continue
		}
		if ok {
			sw.cache.Store(strings.TrimSpace(v) == "1")
		}
	}
	for _, d := range relDials() {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, d.key)
		if err != nil {
			rlogf("rolebots: read %s: %v (using the default)", d.key, err)
			continue
		}
		if !ok {
			continue
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			d.cache.Store(int64(d.clamp(f) * 1000))
			d.set.Store(1)
		}
	}
}

// setRelLedgerSwitch persists one of the three switches.
func (m *Mod) setRelLedgerSwitch(ctx context.Context, key string, on bool) error {
	var cache *atomic.Bool
	switch key {
	case settingRelLedger:
		cache = &relLedgerV
	case settingRelLetters:
		cache = &relLettersV
	case settingRelEnvoys:
		cache = &relEnvoysV
	default:
		return fmt.Errorf("setRelLedgerSwitch: %s is not a ledger switch", key)
	}
	val := "0"
	if on {
		val = "1"
	}
	if err := evedb.SetServerSetting(ctx, m.db, key, val); err != nil {
		return fmt.Errorf("setRelLedgerSwitch %s: %w", key, err)
	}
	cache.Store(on)
	return nil
}

// setRelLedgerDial persists one numeric dial, clamped.
func (m *Mod) setRelLedgerDial(ctx context.Context, key string, v float64) error {
	for _, d := range relDials() {
		if d.key != key {
			continue
		}
		v = d.clamp(v)
		var s string
		if d.integer {
			s = strconv.FormatInt(int64(v), 10)
		} else {
			s = strconv.FormatFloat(v, 'f', 3, 64)
		}
		if err := evedb.SetServerSetting(ctx, m.db, key, s); err != nil {
			return fmt.Errorf("setRelLedgerDial %s: %w", key, err)
		}
		d.cache.Store(int64(v * 1000))
		d.set.Store(1)
		return nil
	}
	return fmt.Errorf("setRelLedgerDial: %s is not a ledger dial", key)
}
