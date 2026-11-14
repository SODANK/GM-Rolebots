package rolebots

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

// The grace ledger's dials, on the Relations page. The ledger is always on: there is no switch,
// and a stale rolebots.grace row is ignored. Same shape as the relations ledger's dials
// (relledgersettings.go); relDial is reused.

const (
	settingGraceKillImpulse     = "rolebots.grace.killImpulse"
	settingGraceNegCap          = "rolebots.grace.negCap"
	settingGracePosCap          = "rolebots.grace.posCap"
	settingGraceBountyMaxISK    = "rolebots.grace.bountyMaxISK"
	settingGracePlayerRegardMul = "rolebots.grace.playerRegardMul"
)

var (
	graceKillImpulseV, graceKillImpulseSet   atomic.Int64
	graceNegCapV, graceNegCapSet             atomic.Int64
	gracePosCapV, gracePosCapSet             atomic.Int64
	graceBountyMaxV, graceBountyMaxSet       atomic.Int64
	gracePlayerRegardV, gracePlayerRegardSet atomic.Int64
)

func graceDials() []relDial {
	return []relDial{
		{settingGraceKillImpulse, "Grace: violence impulse ×", 1.0, 0.25, 5, false, &graceKillImpulseV, &graceKillImpulseSet},
		{settingGraceNegCap, "Grace: grievances shown per bot, at most", 6, 1, 20, true, &graceNegCapV, &graceNegCapSet},
		{settingGracePosCap, "Grace: respects shown per bot, at most", 3, 1, 10, true, &gracePosCapV, &gracePosCapSet},
		{settingGraceBountyMaxISK, "Grace: a vengeful bounty, at most (ISK)", 5_000_000, 0, 1_000_000_000, true, &graceBountyMaxV, &graceBountyMaxSet},
		{settingGracePlayerRegardMul, "Grace: a player's own standing toward a bot counts ×", 0.5, 0, 2, false, &gracePlayerRegardV, &gracePlayerRegardSet},
	}
}

func graceDialValue(key string) float64 {
	for _, d := range graceDials() {
		if d.key == key {
			return d.get()
		}
	}
	return 0
}

func graceKillImpulse() float64     { return graceDialValue(settingGraceKillImpulse) }
func graceNegCap() int              { return int(graceDialValue(settingGraceNegCap)) }
func gracePosCap() int              { return int(graceDialValue(settingGracePosCap)) }
func graceBountyMaxISK() float64    { return graceDialValue(settingGraceBountyMaxISK) }
func gracePlayerRegardMul() float64 { return graceDialValue(settingGracePlayerRegardMul) }

func graceDialRows() []relDialRow {
	var out []relDialRow
	for _, d := range graceDials() {
		step := "0.01"
		if d.integer {
			step = "1"
		}
		out = append(out, relDialRow{Key: d.key, Label: d.label, Value: d.String(), Step: step})
	}
	return out
}

func isGraceDial(key string) bool {
	for _, d := range graceDials() {
		if d.key == key {
			return true
		}
	}
	return false
}

// loadGraceSettings reads the dials at boot. Never fatal.
func (m *Mod) loadGraceSettings(ctx context.Context) {
	for _, d := range graceDials() {
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

func (m *Mod) setGraceDial(ctx context.Context, key string, v float64) error {
	for _, d := range graceDials() {
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
			return fmt.Errorf("setGraceDial %s: %w", key, err)
		}
		d.cache.Store(int64(v * 1000))
		d.set.Store(1)
		return nil
	}
	return fmt.Errorf("setGraceDial: %s is not a grace dial", key)
}
