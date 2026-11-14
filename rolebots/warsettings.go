package rolebots

// The war dials: CEO war declarations (corpceo_war.go) and the mercenary market (merc.go).
//
// Same idiom as relsettings.go: a const key, an atomic cache for the hot path, a clamped accessor,
// a boot loader that is never fatal, and a setter that writes the setting before the cache.
//
// CEO wars default on and the mercenary market defaults off. A declaration charges a real CONCORD
// fee, mails every member of both corporations (human members included) and opens highsec between
// them a day later. Every declaration is still gated by the CEO's charter (WarAt, MaxWars, the war
// chest, the reserve). Set rolebots.war.ceoWars = 0 to stop new wars.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

const (
	// settingCEOWars lets bot CEOs declare, pay for and retract wars. Off, the war power still settles
	// running wars (it pays nothing new and lets every bill lapse), which ends them on the core's timer.
	settingCEOWars = "rolebots.war.ceoWars"
	// settingMercMarket lets corporations hire mercenary corporations and lets
	// mercenary CEOs take the work. Off, open jobs are withdrawn and running
	// contracts are settled out at their next payday rather than renewed.
	settingMercMarket = "rolebots.war.mercMarket"
	// settingGankHires lets a suicide-ganked corporation in policed space put out a mercenary tender,
	// and a mercenary CEO answer it, while the market itself is shut (gankrel.go). Default on.
	settingGankHires = "rolebots.war.gankHires"
	// settingMutualWars lets two bot CEOs in a two-way fight make the war mutual (corpceo_warmutual.go):
	// both consent, the weekly bill stops, and either side may withdraw. Default on. Off, every mutual
	// war a bot side consented to returns to the bill on its next think.
	settingMutualWars = "rolebots.war.mutualWars"
	// settingWarChestMax is the most of a corporation's free capital (wallet less reserve less open
	// commitments) any one war or merc job may commit. It caps the charter's own WarChestFrac.
	settingWarChestMax = "rolebots.war.chestMaxFrac"
	// settingMercWage is the per-pilot weekly wage a mercenary prices its
	// labour from, before risk, politics and market.
	settingMercWage = "rolebots.war.mercWagePerPilot"
	// settingWarAvoidMul is how many times the enemy-hop surcharge a system held by a war enemy costs a
	// non-combat route. War enemies may shoot in highsec, a rival only outside it.
	settingWarAvoidMul = "rolebots.war.avoidMul"
	// settingWarMarkMul is the same for a hunter's choice of ground: a war
	// enemy's system is worth this many times a declared rival's.
	settingWarMarkMul = "rolebots.war.markMul"
)

const (
	warChestMaxDefault = 0.5
	warChestMaxMin     = 0.05
	warChestMaxMax     = 0.9

	mercWageDefault = 1500000.0
	mercWageMin     = 10000.0
	mercWageMax     = 100000000.0

	warAvoidMulDefault = 3.0
	warAvoidMulMin     = 1.0
	warAvoidMulMax     = 10.0

	warMarkMulDefault = 2.0
	warMarkMulMin     = 1.0
	warMarkMulMax     = 10.0
)

var (
	ceoWarsV     atomic.Bool
	mercMarketV  atomic.Bool
	gankHiresV   atomic.Bool
	mutualWarsV  atomic.Bool
	warChestMaxV atomic.Int64 // thousandths; 0 = default
	mercWageV    atomic.Int64 // whole ISK; 0 = default
	warAvoidMulV atomic.Int64 // hundredths; 0 = default
	warMarkMulV  atomic.Int64 // hundredths; 0 = default
)

func ceoWarsOn() bool    { return ceoWarsV.Load() }
func mercMarketOn() bool { return mercMarketV.Load() }
func gankHiresOn() bool  { return gankHiresV.Load() }
func mutualWarsOn() bool { return mutualWarsV.Load() }

func warChestMaxFrac() float64 {
	if v := warChestMaxV.Load(); v > 0 {
		return float64(v) / 1000
	}
	return warChestMaxDefault
}

func mercWagePerPilot() float64 {
	if v := mercWageV.Load(); v > 0 {
		return float64(v)
	}
	return mercWageDefault
}

func warAvoidMul() float64 {
	if v := warAvoidMulV.Load(); v > 0 {
		return float64(v) / 100
	}
	return warAvoidMulDefault
}

func warMarkMul() float64 {
	if v := warMarkMulV.Load(); v > 0 {
		return float64(v) / 100
	}
	return warMarkMulDefault
}

func clampWarChestMax(v float64) float64 { return clampRange(v, warChestMaxMin, warChestMaxMax) }
func clampMercWage(v float64) float64    { return clampRange(v, mercWageMin, mercWageMax) }
func clampWarAvoidMul(v float64) float64 { return clampRange(v, warAvoidMulMin, warAvoidMulMax) }
func clampWarMarkMul(v float64) float64  { return clampRange(v, warMarkMulMin, warMarkMulMax) }

// loadWarSettings reads every war dial at boot. Never fatal.
func (m *Mod) loadWarSettings(ctx context.Context) {
	ceoWarsV.Store(true)
	mercMarketV.Store(false)
	gankHiresV.Store(true)
	mutualWarsV.Store(true)
	for _, sw := range []struct {
		key   string
		cache *atomic.Bool
	}{{settingCEOWars, &ceoWarsV}, {settingMercMarket, &mercMarketV}, {settingGankHires, &gankHiresV},
		{settingMutualWars, &mutualWarsV}} {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, sw.key)
		if err != nil {
			rlogf("rolebots: read %s: %v (using the default)", sw.key, err)
			continue
		}
		if ok {
			sw.cache.Store(strings.TrimSpace(v) == "1")
		}
	}
	for _, d := range warDials() {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, d.key)
		if err != nil {
			rlogf("rolebots: read %s: %v (using the default)", d.key, err)
			continue
		}
		if !ok {
			continue
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			d.cache.Store(int64(d.clamp(f) * d.scale))
		}
	}
}

// warDial is one numeric war setting.
type warDial struct {
	key   string
	clamp func(float64) float64
	scale float64
	cache *atomic.Int64
}

func warDials() []warDial {
	return []warDial{
		{settingWarChestMax, clampWarChestMax, 1000, &warChestMaxV},
		{settingMercWage, clampMercWage, 1, &mercWageV},
		{settingWarAvoidMul, clampWarAvoidMul, 100, &warAvoidMulV},
		{settingWarMarkMul, clampWarMarkMul, 100, &warMarkMulV},
	}
}

// isWarSwitch / isWarDial let the Relations page route a form to the right
// setter without knowing which file owns the key.
func isWarSwitch(key string) bool {
	return key == settingCEOWars || key == settingMercMarket || key == settingGankHires ||
		key == settingMutualWars
}

func isWarDial(key string) bool {
	for _, d := range warDials() {
		if d.key == key {
			return true
		}
	}
	return false
}

// setWarSwitch persists one of the war switches.
func (m *Mod) setWarSwitch(ctx context.Context, key string, on bool) error {
	var cache *atomic.Bool
	switch key {
	case settingCEOWars:
		cache = &ceoWarsV
	case settingMercMarket:
		cache = &mercMarketV
	case settingGankHires:
		cache = &gankHiresV
	case settingMutualWars:
		cache = &mutualWarsV
	default:
		return fmt.Errorf("setWarSwitch: %s is not a war switch", key)
	}
	val := "0"
	if on {
		val = "1"
	}
	if err := evedb.SetServerSetting(ctx, m.db, key, val); err != nil {
		return fmt.Errorf("setWarSwitch %s: %w", key, err)
	}
	cache.Store(on)
	return nil
}

// setWarNumber persists one of the numeric war dials.
func (m *Mod) setWarNumber(ctx context.Context, key string, v float64) error {
	for _, d := range warDials() {
		if d.key != key {
			continue
		}
		v = d.clamp(v)
		if err := evedb.SetServerSetting(ctx, m.db, key, strconv.FormatFloat(v, 'f', -1, 64)); err != nil {
			return fmt.Errorf("setWarNumber %s: %w", key, err)
		}
		d.cache.Store(int64(v * d.scale))
		return nil
	}
	return fmt.Errorf("setWarNumber: %s is not a war dial", key)
}
