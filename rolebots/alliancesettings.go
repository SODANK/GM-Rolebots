package rolebots

// The alliance settings (alliance.go and its files).
//
// The alliance layer is always on: dues, votes, applications, diplomacy, sovereignty, fleets, command,
// the frontier and forward offices have no switch. The one switch is founding (settingAllianceFound,
// default off) and its cap. Same idiom as warsettings.go: a const key, an atomic cache for the hot path,
// a boot loader that is never fatal, and a setter that writes the setting before it updates the cache.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

const (
	// settingAllianceFound lets a bot CEO found an alliance (1B ISK out of
	// its corporation's wallet). The cap below bounds how many stand.
	settingAllianceFound = "rolebots.alliance.found"
	// settingAllianceFoundMax is the most bot-founded alliances that may
	// exist at once (rolebot_founded_alliances rows whose alliance stands).
	settingAllianceFoundMax = "rolebots.alliance.foundMax"
	// settingAllianceSeekAllyAt is the standing, both ways, at which another corporation counts as an
	// ally for seeking an alliance (allianceSeek) and for a founding cluster (allianceConsiderFounding).
	// Corporation diplomacy keeps its own StandingDevoted line. Read live through db.CachedSettingFloat.
	settingAllianceSeekAllyAt = "rolebots.alliance.seekAllyAt"
)

const (
	allianceFoundMaxDefault = 10
	allianceFoundMaxMax     = 200
	// allianceSeekAllyAtDefault is "liked" in the seeker's sense: under StandingDevoted, so a corporation
	// follows friends it has not sworn to.
	allianceSeekAllyAtDefault = 5.0
)

// allianceSeekAllyAt is the live seekAllyAt line, clamped to the standing range.
func allianceSeekAllyAt() float64 {
	return evedb.CachedSettingFloat(settingAllianceSeekAllyAt, allianceSeekAllyAtDefault, -10, 10)
}

var (
	allianceFoundV    atomic.Bool
	allianceFoundMaxV atomic.Int64
)

func init() { allianceFoundMaxV.Store(allianceFoundMaxDefault) }

func allianceFoundOn() bool { return allianceFoundV.Load() }
func allianceFoundMax() int { return int(allianceFoundMaxV.Load()) }

// loadAllianceSettings reads the founding switch and the cap. Never fatal.
func (m *Mod) loadAllianceSettings(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingAllianceFound); err != nil {
		rlogf("rolebots: read %s: %v (leaving it off)", settingAllianceFound, err)
	} else if ok {
		allianceFoundV.Store(strings.TrimSpace(v) == "1")
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingAllianceFoundMax); err == nil && ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= allianceFoundMaxMax {
			allianceFoundMaxV.Store(int64(n))
		}
	}
}

// setAllianceFound persists the founding switch.
func (m *Mod) setAllianceFound(ctx context.Context, on bool) error {
	val := "0"
	if on {
		val = "1"
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingAllianceFound, val); err != nil {
		return fmt.Errorf("setAllianceFound: %w", err)
	}
	allianceFoundV.Store(on)
	return nil
}

// setAllianceFoundMax persists the founding cap.
func (m *Mod) setAllianceFoundMax(ctx context.Context, n int) error {
	if n < 0 || n > allianceFoundMaxMax {
		return fmt.Errorf("a whole number from 0 to %d", allianceFoundMaxMax)
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingAllianceFoundMax, strconv.Itoa(n)); err != nil {
		return fmt.Errorf("setAllianceFoundMax: %w", err)
	}
	allianceFoundMaxV.Store(int64(n))
	return nil
}
