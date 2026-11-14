package rolebots

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

// Market timing: traders act on a price that has left its usual level.
//
//	scrap floor  a refinable module or hull is worth at least its metal
//	             (npcanchor.go, deadstock.go)
//	hold         goods bought well under the usual price keep a high list price (tradeorders.go)
//	dip          buy an ask well under the usual price (dipboard.go)
//	surge        carry goods to a region paying well over it (tradesurge.go)
//
// The usual price is mktHistory.Norm, and recent demand is mktHistory.Recent,
// net of the roster's own resale buying. docs/TRADE_ECONOMY.md has the rules.

const (
	settingScrapFloorEnabled = "rolebots.scrapfloor.enabled"
	settingDipEnabled        = "rolebots.dip.enabled"
	settingDipEntry          = "rolebots.dip.entry"
	settingSurgeEnabled      = "rolebots.surge.enabled"
	settingSurgeTrigger      = "rolebots.surge.trigger"
	settingSurgeMaxMul       = "rolebots.surge.maxMul"
	settingSurgeMaxJumps     = "rolebots.surge.maxJumps"
)

// marketPlayConfig is the operator's dials.
type marketPlayConfig struct {
	ScrapFloor bool
	Dip        bool
	// DipEntry is the most of the usual price a dip pays. It stays under
	// holdFloorLo, or a flip's hold line would sit under its cost.
	DipEntry float64
	Surge    bool
	// SurgeTrigger is the recent price over the usual one that makes a surge,
	// SurgeMaxMul the most of the usual price a surge run lists at.
	SurgeTrigger, SurgeMaxMul float64
	// SurgeMaxJumps bounds a surge run: a surge fades while the goods fly.
	SurgeMaxJumps int
}

func defaultMarketPlayConfig() marketPlayConfig {
	return marketPlayConfig{
		ScrapFloor: true, Dip: true, DipEntry: 0.80,
		Surge: true, SurgeTrigger: 1.4, SurgeMaxMul: 2.5, SurgeMaxJumps: 12,
	}
}

var (
	marketPlayCfgV  atomic.Pointer[marketPlayConfig]
	marketPlayCfgAt atomic.Int64
)

// marketPlay is the current dials, re-read at most once a minute: the order
// sweep asks per order. Out-of-range values keep the default.
func (m *Mod) marketPlay(ctx context.Context) marketPlayConfig {
	now := clock.Unix()
	if c := marketPlayCfgV.Load(); c != nil && now-marketPlayCfgAt.Load() < sweepSettingsTTL {
		return *c
	}
	c := defaultMarketPlayConfig()
	if m != nil && m.db != nil {
		read := func(key string) (string, bool) {
			v, ok, err := evedb.GetServerSetting(ctx, m.db, key)
			return strings.TrimSpace(v), ok && err == nil
		}
		frac := func(key string, lo, hi float64, into *float64) {
			if v, ok := read(key); ok {
				if f, err := strconv.ParseFloat(v, 64); err == nil && f >= lo && f <= hi {
					*into = f
				}
			}
		}
		if v, ok := read(settingScrapFloorEnabled); ok {
			c.ScrapFloor = v != "0"
		}
		if v, ok := read(settingDipEnabled); ok {
			c.Dip = v != "0"
		}
		if v, ok := read(settingSurgeEnabled); ok {
			c.Surge = v != "0"
		}
		frac(settingDipEntry, 0.3, 0.8, &c.DipEntry)
		frac(settingSurgeTrigger, 1.1, 5, &c.SurgeTrigger)
		frac(settingSurgeMaxMul, 1.1, 10, &c.SurgeMaxMul)
		if v, ok := read(settingSurgeMaxJumps); ok {
			if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 40 {
				c.SurgeMaxJumps = n
			}
		}
	}
	marketPlayCfgV.Store(&c)
	marketPlayCfgAt.Store(now)
	return c
}

const (
	// holdCostFrac is what "bought well under the usual price" means: cost
	// below this share of it. Above it, ordinary cost-plus pricing stands.
	holdCostFrac = 0.85
	// The placement band for such goods, generous pilot to greedy one.
	marketListLo = 0.90
	marketListHi = 0.98
	// The hold band: how far such goods may be undercut down to. Per pilot, so
	// a race ends in a spread and not a row of tied prices.
	holdFloorLo = 0.85
	holdFloorHi = 0.90
	// holdPatienceMul is how much longer such a listing stands before it is
	// pulled: a crash takes days to recover and the plain patience is hours.
	holdPatienceMul = 3
)

// boughtCheap reports whether goods at this cost sit well under the usual
// price. A zero cost is no cost basis, and a zero usual price no evidence.
// Pure.
func boughtCheap(cost, usual float64) bool {
	return cost > 0 && usual > 0 && cost < usual*holdCostFrac
}

// marketListPrice is where goods bought cheap are listed: near the usual
// price, not at cost plus a markup, which would sell the crash back to the
// market at crash prices. 0 when the goods were not bought cheap. Pure.
func marketListPrice(cost, usual float64, tn Tuning) float64 {
	if !boughtCheap(cost, usual) {
		return 0
	}
	return marketListAt(usual, tn)
}

// marketListAt is this pilot's point in the placement band. Pure.
func marketListAt(usual float64, tn Tuning) float64 {
	return roundISK(usual * (marketListLo + personalT(tn)*(marketListHi-marketListLo)))
}

// underNPC caps a list price under the region's NPC shelf: no listing of goods
// bought for a flip waits behind unlimited NPC stock. npc 0 means no shelf.
// Pure.
func underNPC(list, npc float64) float64 {
	if npc > 0 && list > npc*npcAnchorFracHi {
		return roundISK(npc * npcAnchorFracHi)
	}
	return list
}

// holdFloor is the lowest such goods are undercut to, 0 when they were not
// bought cheap. It takes no order kind, so goods that come back as stock keep
// it. Pure.
func holdFloor(cost, usual float64, tn Tuning) float64 {
	if !boughtCheap(cost, usual) {
		return 0
	}
	return roundISK(usual * (holdFloorLo + personalT(tn)*(holdFloorHi-holdFloorLo)))
}

// usualPriceAt is the usual price of a type in the station's region.
func (m *Mod) usualPriceAt(ctx context.Context, stationID, typeID int64) float64 {
	return m.marketHistory(ctx).norm(typeID, m.stationRegionOf(ctx, stationID))
}

// cheapListPrice is marketListPrice at a station, under its region's NPC shelf.
func (m *Mod) cheapListPrice(ctx context.Context, cost float64, stationID, typeID int64, tn Tuning) float64 {
	lp := marketListPrice(cost, m.usualPriceAt(ctx, stationID, typeID), tn)
	if lp <= 0 {
		return 0
	}
	return underNPC(lp, m.npcAskAnchor(ctx, stationID, typeID))
}

// holdFloorAt is holdFloor at a station. Under an NPC shelf the anchor band
// already rules, so the hold stops at the top of its competitive line.
func (m *Mod) holdFloorAt(ctx context.Context, cost float64, stationID, typeID int64, tn Tuning) float64 {
	hf := holdFloor(cost, m.usualPriceAt(ctx, stationID, typeID), tn)
	if npc := m.npcAskAnchor(ctx, stationID, typeID); npc > 0 && hf > npc*npcCompeteFloorHi {
		return roundISK(npc * npcCompeteFloorHi)
	}
	return hf
}
