package rolebots

// The ore economy's two numeric dials: how much margin an ore ask leaves for
// the refiner who buys it, and how much cheaper rock has to be than metal
// before a factory takes it. Both are tuning an operator may move without a
// deploy; the other ore behaviours are unconditional.
//
// Same idiom as skillsweep.go: a const for the key, an atomic cache read on the
// hot path, a clamped accessor, one boot loader that is never fatal, and a
// setter that writes the setting before it updates the cache, so a failed write
// leaves the roster on the dial it had. Old rolebots.ore.* rows stay in
// server_settings; nothing reads them.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

const (
	// settingOreAskEdge is the margin an ore ask leaves for whoever buys it,
	// as a multiple. Stored in hundredths so the cache can be an atomic int.
	settingOreAskEdge = "rolebots.ore.askEdge"
	// settingOreSubEdge is how much cheaper rock has to be than metal before
	// an industrialist buys it, as a multiple. 1.00 is "strictly cheaper".
	settingOreSubEdge = "rolebots.ore.subEdge"

	// oreSubEdgeDefault is "strictly cheaper": a factory takes rock over metal
	// only when the rock actually costs less per unit of metal delivered.
	oreSubEdgeDefault = 1.00

	// The ranges. An ask edge under 1.0 would have a miner asking more than a
	// refiner can pay, a listing nobody can act on; over 1.5 it gives the rock away.
	oreAskEdgeMin = 1.00
	oreAskEdgeMax = 1.50
	// The substitution edge may sit under 1.0 (an operator saying "buy rock even at
	// a small premium, I want the refining to happen") but not far under, or a
	// factory overpays for feedstock it could have bought as metal.
	oreSubEdgeMin = 0.80
	oreSubEdgeMax = 1.50
)

var (
	oreAskEdgeV atomic.Int64 // hundredths; 0 = the shipped default
	oreSubEdgeV atomic.Int64 // hundredths; 0 = the shipped default
)

// oreAskRefinerEdge is the margin an ore ask leaves on the table.
func oreAskRefinerEdge() float64 {
	if v := oreAskEdgeV.Load(); v > 0 {
		return float64(v) / 100
	}
	return oreAskRefinerEdgeDefault
}

// oreSubEdge is how much cheaper rock must be than metal to be worth buying.
func oreSubEdge() float64 {
	if v := oreSubEdgeV.Load(); v > 0 {
		return float64(v) / 100
	}
	return oreSubEdgeDefault
}

func clampOreAskEdge(v float64) float64 { return clampRange(v, oreAskEdgeMin, oreAskEdgeMax) }
func clampOreSubEdge(v float64) float64 { return clampRange(v, oreSubEdgeMin, oreSubEdgeMax) }

func clampRange(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// loadOreSettings reads both dials at boot. Never fatal: an unparseable dial
// takes the shipped default, since refusing to start would take the server
// with it.
func (m *Mod) loadOreSettings(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingOreAskEdge); err != nil {
		rlogf("rolebots: read %s: %v (using the default)", settingOreAskEdge, err)
	} else if ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			oreAskEdgeV.Store(int64(clampOreAskEdge(f) * 100))
		}
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingOreSubEdge); err != nil {
		rlogf("rolebots: read %s: %v (using the default)", settingOreSubEdge, err)
	} else if ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			oreSubEdgeV.Store(int64(clampOreSubEdge(f) * 100))
		}
	}
}

// setOreEdge persists one of the two numeric dials.
func (m *Mod) setOreEdge(ctx context.Context, key string, v float64) error {
	var cache *atomic.Int64
	switch key {
	case settingOreAskEdge:
		v, cache = clampOreAskEdge(v), &oreAskEdgeV
	case settingOreSubEdge:
		v, cache = clampOreSubEdge(v), &oreSubEdgeV
	default:
		return fmt.Errorf("setOreEdge: %s is not an ore edge", key)
	}
	if err := evedb.SetServerSetting(ctx, m.db, key,
		strconv.FormatFloat(v, 'f', 2, 64)); err != nil {
		return fmt.Errorf("setOreEdge %s: %w", key, err)
	}
	cache.Store(int64(v * 100))
	return nil
}
