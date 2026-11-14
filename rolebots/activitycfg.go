package rolebots

import (
	"context"
)

// Activity configs.
//
// Solo bots store none: each activity's config is computed from the pilot's own flags every time it
// plans (the *Derived builders below). Gangs still store one config blob, so the JSON struct shapes
// and the form bridges stay for the gang page.

// ── derived solo configs ────────────────────────────────────────────────────

// miningConfigDerived derives mining behaviour from the pilot's own flags. What a full hold becomes
// is decided by why the trip was flown, in the case order below. Nothing stockpiles at an operator's
// station; the stockpiles left are a project's materials, a full factory's standby stock and the
// no-bidder fallback in miningDispose (mining.go).
func miningConfigDerived(b *botRun) miningConfig {
	// 1. A mining trip inside an industry project (industry.go) is a materials run. Only pre-install
	// projects mine; a cooking job frees the pilot.
	if b.State.Data.IndFactoryID != 0 && b.State.Data.IndJobID == 0 {
		// It is after a named ore: the one that refines into what the bill of materials lacks
		// (oresYieldingMinerals). The generic hierarchy is the fallback.
		ore := b.State.Data.IndOreWanted
		strict := len(ore) > 0
		if !strict {
			ore = defaultOrePriority
		}
		return miningConfig{
			OrePriority:        ore,
			Strict:             strict,
			AvoidHostileBelts:  true,
			Disposal:           "stockpile",
			StockpileStationID: b.State.Data.IndFactoryID,
		}
	}
	// 1.5. Standby: lines busy or a cold book, so the load goes to the pilot's own factory hangar
	// (resolveStandbyMining, mining.go). It precedes the refine case, or a skilled pilot would never feed its hangar.
	if d := b.State.Data; d.StandbyFactoryID != 0 {
		return miningConfig{
			OrePriority:        defaultOrePriority,
			ByValue:            true,
			AvoidHostileBelts:  true,
			Disposal:           "stockpile",
			StockpileStationID: d.StandbyFactoryID,
		}
	}
	// 2. A trip that decided to refine (resolveMiningDisposal, mining.go): the load is listed as minerals.
	// RefineStationID 0 means wherever it lands. Skills and the wallet gate it, so most rock goes to a buyer.
	if d := b.State.Data; d.MineDisposal == "refine" {
		return miningConfig{
			OrePriority:       defaultOrePriority,
			ByValue:           true,
			AvoidHostileBelts: true,
			Disposal:          "refine",
			RefineStationID:   d.MineRefineStationID,
		}
	}
	// 2.5. List the rock: the same travel and search as selling, carried by one flag. miningSell tries the
	// listing first and falls through to a sale. It is not its own TaskKind: ranking by ore demand needs factory bids.
	if b.State.Data.MineDisposal == "list" {
		return miningConfig{
			OrePriority:       defaultOrePriority,
			ByValue:           true,
			AvoidHostileBelts: true,
			Disposal:          "sell",
			ListOre:           true,
		}
	}
	// 3. Ore for money: whatever rock in reach is worth the most per m³
	// (oreDigValues), sold to a bid for cash.
	return miningConfig{
		OrePriority:       defaultOrePriority,
		ByValue:           true,
		AvoidHostileBelts: true, // the pilot's HostileCost prices how much it cares
		Disposal:          "sell",
	}
}

// tradeConfigDerived derives trade behaviour from flags. There are no spend or volume caps (the wallet
// nerve in Tuning limits), and station manners come from standings and loyalty (stationFilterPasses).
// A pilot that trades for a living deals in everything (nil filter), because the planner prices each
// candidate (tradeCargoOK) and a market-group whitelist would answer first. Others keep to Trade Goods.
func (m *Mod) tradeConfigDerived(ctx context.Context, b *botRun) tradeConfig {
	cfg := tradeConfig{
		TradeWithHated: true, // the soft station pass already prefers otherwise
	}
	if tradeInterest(b.Tune) < specMinInterest {
		cfg.MarketGroupIDs = m.defaultTradeGroupIDs(ctx)
	}
	return cfg
}

// combatConfigDerived derives who this pilot hunts and where: the pirate spawn factions minus the
// pilot's employer, its friends and anyone it likes; the security band comes from its caution. The
// list is advisory: only huntTargetSummary and the gang checkboxes read it, and the restraint is in faction.go.
func (m *Mod) combatConfigDerived(ctx context.Context, b *botRun) combatConfig {
	mine := b.FactionID
	mx, ok := m.standingsMatrix(ctx)
	if !ok {
		// No readable matrix means no employer filtering: a nil matrix would read every pair as standing 0 and
		// empty the hunt list of every employed pilot.
		mine = 0
	}
	targets := huntableFactions(mx, mine, m.pirateFactionIDs(ctx), b.Profile)
	return combatConfig{
		SecMin:     b.Tune.HuntSecFloor,
		SecMax:     b.Tune.HuntSecCeil,
		FactionIDs: targets,
		LootField:  true,
	}
}

// ── the combat patrol ─────────────────────────────────────────────────────────

// combatConfig is where the bot hunts and what it shoots. A gang stores it as JSON: secMin and secMax
// (the hunted band), factionIDs (pirate factions worth hunting) and lootField (loot cans after a fight).
type combatConfig struct {
	SecMin     float64 `json:"secMin"`
	SecMax     float64 `json:"secMax"`
	FactionIDs []int64 `json:"factionIDs"`
	LootField  bool    `json:"lootField"`
}

// pirateFactionNames are default-checked hunt targets, keyed as people say the names, so look up through
// normFactionName (corps.go): the Blood Raiders ship as The Blood Raider Covenant. Rogue Drones has no
// chrFactions row in this database and stays inert.
var pirateFactionNames = map[string]bool{
	"Angel Cartel": true, "Blood Raider Covenant": true, "Guristas Pirates": true,
	"Sansha's Nation": true, "Serpentis": true, "Rogue Drones": true,
}

func combatConfigFor(blob string) combatConfig {
	cfg := combatConfig{SecMin: 0.1, SecMax: 0.8, LootField: true}
	_ = gangConfig(blob, &cfg)
	if cfg.SecMax < cfg.SecMin {
		cfg.SecMin, cfg.SecMax = cfg.SecMax, cfg.SecMin
	}
	return cfg
}

// ── trade ───────────────────────────────────────────────────────────────────

// tradeConfig is what the bot may flip and with whom. A gang stores it as JSON: secFloor (0 is the global
// profile floor), maxSpendISK and maxVolumeM3 (0 is no cap), marketGroupIDs, tradeWithHated (despised
// are refused either way) and onlyLikedFactions (trade only where standing is at least +2.5).
type tradeConfig struct {
	SecFloor          *float64 `json:"secFloor,omitempty"`
	MaxSpendISK       float64  `json:"maxSpendISK"`
	MaxVolumeM3       float64  `json:"maxVolumeM3"`
	MarketGroupIDs    []int64  `json:"marketGroupIDs"`
	TradeWithHated    bool     `json:"tradeWithHated"`
	OnlyLikedFactions bool     `json:"onlyLikedFactions"`

	// The two fields the corporation's Trade desk sets on itself before searching (corptrade.go); neither is
	// persisted. BuySystems gates the buy end to runs whose seller is in those systems; the sell end is
	// open (sellTradeRunHere). Empty is no gate.
	BuySystems map[int64]bool `json:"-"`
	// NoSpec keeps speculative searches (a listing bet, a blueprint carried to a region without one) out of
	// the result. The desk sets it: a bet is sized to a wallet that sits on it, and the desk advances
	// money to a member paid to carry.
	NoSpec bool `json:"-"`
	// HaulCostPerM3Jump is the wage the desk pays its hauler per m3 per jump of the haul (floored at
	// haulMinWage). When set, the search ranks and floors runs on the margin left after the wage, as the
	// desk's gate (tradeDeskClears) does. Zero for solo callers.
	HaulCostPerM3Jump float64 `json:"-"`
	// MinProfitISK raises the search's profit floor (tradeProfitFloor) to what the desk will post, so a run
	// the desk's minimum refuses is not ranked first. Zero leaves the solo floor. The gate still charges
	// the listing fees.
	MinProfitISK float64 `json:"-"`
}

// ── shared pickers ──────────────────────────────────────────────────────────

// defaultTradeGroupIDs finds the "Trade Goods" root for the default
// allowance.
func (m *Mod) defaultTradeGroupIDs(ctx context.Context) []int64 {
	var id int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT marketGroupID FROM invMarketGroups
		WHERE marketGroupName = 'Trade Goods' AND parentGroupID IS NULL LIMIT 1`,
	).Scan(&id); err == nil && id != 0 {
		return []int64{id}
	}
	return nil
}
