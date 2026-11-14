package rolebots

import (
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// Trade activity, abstract tier. The bot buys from a sell order, flies to another station and unloads into a
// buy order. The market legs use the real matching engine, so watched and unwatched runs produce the same books.
// A run is bounded by the activity config (activitycfg.go):
//
//	secFloor           where it is willing to fly
//	maxSpendISK        hard cap per buy (0 = whatever the wallet allows)
//	maxVolumeM3        per-haul volume cap (0 = whatever the hold allows)
//	marketGroupIDs     the market-group subtrees it may deal in
//	tradeWithHated     may it deal at stations owned by factions it hates
//	onlyLikedFactions  deal only at stations owned by factions it likes

const (
	// A run must clear this much over the ask to be worth the fuel and the hold. This is what the pilot asks
	// for, not the least it accepts: see tradeFloorMarginFrac.
	tradeMinMarginFrac = 0.15
	// The spread it settles for when nothing pays the asking spread. A run that clears this and the absolute
	// profit floor below is still worth flying. A greedy pilot's floor is its own asking price (tuning.go).
	tradeFloorMarginFrac = 0.05
	// And it has to be worth the trip in absolute terms.
	tradeMinProfitISK = 5_000.0
	// The wallet is not spent to the last ISK: the bot keeps enough to replace a module after a haul.
	tradeWalletReserveFrac = 0.15
	// How many raw candidate pairs the SQL pass hands back for costing. The scan is universe-wide, so the
	// fattest gaps are read first and unroutable ones are dropped afterwards in Go.
	tradeMaxCandidates = 400
	// How many surviving pairs get a real buy-to-sell route measured. The cheap estimate ranks them;
	// the exact haul decides the winner.
	tradeRefineTop = 12
	// Per type, how many stations each side of the book contributes, so a commodity seeded at thousands of
	// stations does not blow up the join. The best forty asks and bids leave enough choice after routing.
	tradeStationsPerType = 40
	// Per type, how many station pairs may enter the candidate budget (see the query), so one commodity's
	// pairings cannot fill all tradeMaxCandidates rows.
	tradePairsPerType = 4
	// Guard on the market-group filter: a pilot who allows a huge subtree
	// gets no filter at all rather than a megabyte of placeholders.
	tradeMaxGroupIDs = 2000
	// How many runs in a row a trader flies the same commodity before the planner looks at its next-best idea.
	tradeMaxStreak = 3
)

// tradeDeal is one costed run. Kind arb buys off an ask and hits a bid, both legs immediate. Kind spec buys
// where the goods exist and lists them where there are none at the pilot's own price: an order slot is
// committed and the sweep in tradebook.go books it when it sells.
type tradeDeal struct {
	Kind     string
	TypeID   int64
	TypeName string
	UnitVol  float64
	// CategoryID: only ships are treated specially (tradeCargoOK), but the scan must return the column.
	CategoryID int64

	BuyStationID  int64
	BuySystemID   int64
	SellStationID int64
	SellSystemID  int64

	Ask, Bid float64
	// The book on each side, from the scan (Units starts as the smaller). Kept apart because traders in
	// flight load the two ends differently (tradeLaneLoad).
	AskVol, BidVol int64
	Units          int64
	Outlay         float64
	Profit         float64
	Jumps          int
	// Crowd is how many other bots are hauling this commodity now. Profit stays honest; the ranking divides
	// by the crowd.
	Crowd int

	// Speculation. ListPrice is what the pilot intends to ask at the far end. Confidence discounts that margin
	// when ranking against a sure thing, since a listing is a bet. Evidence is the prose behind it, for the log.
	ListPrice  float64
	Confidence float64
	Evidence   string
	// Rank is what the ranking sorts on: profit against the flying it costs and the crowd. Stored so the
	// leg-refinement pass and the first pass cannot disagree about the order.
	Rank float64
	// HaulWage is what somebody else is paid to fly the run (tradeConfig.HaulCostPerM3Jump; 0 for a solo run).
	// Profit stays the gross margin that the desk's gate, logs and requisition read; expected() takes the wage off.
	HaulWage float64

	// BuyCap is the most a dip run pays a unit (dipboard.go); 0 otherwise.
	BuyCap float64
}

// expected is what the run is worth to the ranking: a sure margin at face
// value, a speculative one discounted by how much the pilot believes it,
// less any wage paid to have it flown.
func (d *tradeDeal) expected() float64 {
	if d.Kind == tradeDealSpec {
		return d.Profit*d.Confidence - d.HaulWage
	}
	return d.Profit - d.HaulWage
}

// tradeHaulWage is the wage for hauling units of unitVol over haulJumps at rate ISK per m3 per jump:
// haulWageFor's formula at any rate. Rate 0 is no wage at all, not the floor. Pure.
func tradeHaulWage(units int64, unitVol float64, haulJumps int, rate float64) float64 {
	if rate <= 0 || units <= 0 {
		return 0
	}
	return math.Max(haulMinWage, math.Ceil(float64(units)*unitVol*float64(max(haulJumps, 1))*rate))
}

// tradeSearchFloor is what a run has to clear, after any wage, to be
// priced at all: the solo floor for the budget, raised to the caller's own
// minimum when it has one (the desk's). Pure.
func tradeSearchFloor(budget float64, cfg tradeConfig) float64 {
	return math.Max(tradeProfitFloor(budget), cfg.MinProfitISK)
}

// tradeHaulEstimate is the buy-to-sell haul guessed from the pilot's own jump map before the real route is
// measured. low is a lower bound (the triangle inequality) and the floor is tested on it; est is the cheap
// over-count (here to buy plus here to sell) that the ranking uses. Pure.
func tradeHaulEstimate(toBuy, toSell int) (low, est int) {
	low = toSell - toBuy
	if low < 0 {
		low = -low
	}
	return max(low, 1), toBuy + toSell
}

// Deal kinds, also stored on the state row (state.go).
const (
	tradeDealArb   = "arb"
	tradeDealSpec  = "spec"
	tradeDealStock = "stock"
	// A standing bid for gear priced off the roster's demand ledger rather than an outlet (gearbids.go). Its own
	// kind because it is discounted: the under-value sweep (pullUnderValueBid) would pull every one at once.
	tradeDealGear = "gear"
	// A factory's standing bid for material it can neither mine nor buy in reach (industrybids.go). Never on a
	// tradeDeal, but the kind on the registry row stops the order sweep booking its fills as merchandise to haul.
	tradeDealSupply = "supply"
	// A factory's own output resting on the book (industry.go). Registering it lets bookFill see it and noteShelfSold
	// feed the shelf evidence tier of the build book (evidenceWeightShelf). The kind also stops the sweep booking the
	// output as merchandise to haul away and re-sell, as for a supply bid.
	tradeDealBuild = "build"
	// A blueprint resting on the book, carried to a region with none (bpotrade.go, findBPOArbRun) or a pilot's own
	// surplus print. Like a spec listing, but its buyer is a factory, so the upkeep sweep gives it bpoListPatienceMul
	// times the patience before it undercuts or pulls it (nudgeOrder).
	tradeDealBPO = "bpo"
)

// categoryShip is invCategories.categoryID for Ship, the one commodity class a hauler cannot simply take some of.
const categoryShip = 6

// tradeCargoOK checks what a pilot is willing to carry. Ammunition and modules fit any hold in some quantity;
// a packaged hull is thousands of m3, so for ships the pilot must be able to carry at least two. The volume
// arithmetic already refuses a hull that does not fit at all.
func tradeCargoOK(categoryID int64, units int64) bool {
	if categoryID == categoryShip {
		return units >= 2
	}
	return units >= 1
}

// ── planning ────────────────────────────────────────────────────────────────

// tradePlan is entered from idle: make sure the bot has its hauler, find a
// run worth flying, and set off for the buy station.
func (m *Mod) tradePlan(ctx context.Context, b *botRun) error {
	cfg := m.tradeConfigDerived(ctx, b)
	st := b.State
	st.Activity = ActTrade

	// Before fetching the hauler, check the pilot can fund a run. A pilot holding crates still goes: the stock
	// run turns them back into a float.
	if err := m.tradeStartGate(ctx, b); err != nil {
		return err
	}

	ready, err := m.ensureActivityShip(ctx, b, ActTrade)
	if err != nil || !ready {
		return err
	}

	// Before it goes anywhere, consider leaving a standing bid at this station (tradebids.go). A station action:
	// it does not consume the turn.
	m.maybeLeaveStandingBid(ctx, b)

	// Consider bidding for a real hold (haulerbid.go) when the trader has money and nobody sells an industrial.
	// Same seam: a station action that does not consume the turn.
	m.maybeBidForHauler(ctx, b)
	m.maybeBidForMiningCruiser(ctx, b)

	// Rock on this dock worth turning into metal (orearb.go) is the demand side of the miner's ore ask. A station
	// action that commits no order slot and does not consume the turn.
	m.maybeRefineArbitrage(ctx, b)

	// Goods the pilot already owns outrank any new idea: stock in a hangar is idle capital left by a filled
	// standing bid or a pulled listing, so it is sold before the pilot goes shopping again.
	if planned, err := m.tradeStockPlan(ctx, b); err != nil {
		return err
	} else if planned {
		return nil
	}

	deal, err := m.findTradeRun(ctx, b, cfg)
	if err != nil {
		return err
	}

	st.Data = idleData(st.Data)
	st.Data.TradeKind = deal.Kind
	st.Data.TradeTypeID = deal.TypeID
	st.Data.TradeName = deal.TypeName
	st.Data.TradeQty = deal.Units
	st.Data.TradeBuyStationID = deal.BuyStationID
	st.Data.TradeBuySystemID = deal.BuySystemID
	st.Data.TradeSellStationID = deal.SellStationID
	st.Data.TradeSellSystemID = deal.SellSystemID
	st.Data.TradeAsk = deal.Ask
	st.Data.TradeBid = deal.Bid
	st.Data.TradeListPrice = deal.ListPrice
	st.Data.TradeBuyCap = deal.BuyCap
	// Streak bookkeeping: the counter behind the variety rule.
	if deal.TypeID == st.Data.TradeLastTypeID {
		st.Data.TradeStreak++
	} else {
		st.Data.TradeLastTypeID, st.Data.TradeLastName, st.Data.TradeStreak = deal.TypeID, deal.TypeName, 1
	}

	if deal.Kind == tradeDealSpec {
		specClaims.claim(b.CharID, deal.TypeID, m.stationRegionOf(ctx, deal.SellStationID), deal.Units, clock.Unix())
		m.logBot(b.CharID, logInfo, "trade",
			"speculating — %d× %s: buy at %.2f (%s) and list at %.2f in %s (%s), "+
				"~%.0f ISK if it sells on %.0f ISK outlay",
			deal.Units, deal.TypeName, deal.Ask, m.stationLabel(ctx, deal.BuyStationID), deal.ListPrice,
			m.nameOfSystem(ctx, deal.SellSystemID), deal.Evidence,
			deal.Profit, deal.Outlay)
	} else {
		m.logBot(b.CharID, logInfo, "trade",
			"run — %d× %s: buy at %.2f (%s), sell at %.2f (%s), ~%.0f ISK profit on %.0f ISK outlay",
			deal.Units, deal.TypeName, deal.Ask, m.stationLabel(ctx, deal.BuyStationID),
			deal.Bid, m.stationLabel(ctx, deal.SellStationID),
			deal.Profit, deal.Outlay)
	}

	return m.beginTravel(ctx, b, deal.BuySystemID, deal.BuyStationID, "trade.buy")
}

// tradeMinStartISK is the smallest trading budget (wallet less the trade reserve) worth fetching a hauler for:
// five profit floors (tradeProfitFloorMin). It keeps the tiny-wallet trading tradefloor.go protects and stops
// only wallets no gap in the game can turn into a run.
const tradeMinStartISK = 5 * tradeProfitFloorMin

// canStartTrade is the gate tradePlan applies before travelling, pure.
func canStartTrade(balance float64, hasStock bool) bool {
	return hasStock || balance*(1-tradeWalletReserveFrac) >= tradeMinStartISK
}

// tradeStartGate refuses a trade plan the wallet cannot fund, with the money
// sentinel so planSolo sends the pilot to earn (broke.go) instead.
func (m *Mod) tradeStartGate(ctx context.Context, b *botRun) error {
	balance, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return fmt.Errorf("wallet: %w", err)
	}
	if canStartTrade(balance, false) {
		return nil
	}
	stock, err := m.pilotStock(ctx, b.CharID)
	if err != nil {
		return err
	}
	if canStartTrade(balance, len(stock) > 0) {
		return nil
	}
	return fmt.Errorf("%w: a %.0f ISK wallet cannot fund a run and it holds no stock to sell",
		errCannotAfford, balance)
}

// findTradeRun costs every candidate pair the market offers in range and
// returns the most profitable one this bot can actually pay for and carry.
func (m *Mod) findTradeRun(ctx context.Context, b *botRun, cfg tradeConfig) (*tradeDeal, error) {
	if b.ShipID == 0 {
		return nil, fmt.Errorf("no hauler boarded")
	}
	// What it can pay with, and what it can carry.
	budget, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return nil, fmt.Errorf("wallet: %w", err)
	}
	budget *= 1 - tradeWalletReserveFrac
	if cfg.MaxSpendISK > 0 && cfg.MaxSpendISK < budget {
		budget = cfg.MaxSpendISK
	}
	if budget <= 0 {
		return nil, fmt.Errorf("%w: wallet is empty — nothing to trade with", errCannotAfford)
	}
	hold, err := m.holdFreeM3(ctx, b.CharID, b.ShipID)
	if err != nil {
		return nil, fmt.Errorf("cargo hold: %w", err)
	}
	if cfg.MaxVolumeM3 > 0 && cfg.MaxVolumeM3 < hold {
		hold = cfg.MaxVolumeM3
	}
	if hold <= 0 {
		return nil, fmt.Errorf("no free cargo space to haul with")
	}

	// The spread this pilot asks for, and the thinner one it settles for when nothing pays that. A greedy
	// pilot's two are the same number, so the caller sends it to other work (tuning.go, planSolo).
	askMargin, floorMargin := b.Tune.TradeMinMarginFrac, b.Tune.TradeFloorMarginFrac
	if floorMargin > askMargin {
		floorMargin = askMargin
	}

	// A trader is not pinned to one region (residency.go): it opens at the adjacent band because a spread is
	// only visible from both ends, and it climbs when its own neighbourhood offers nothing.
	r := m.economicRules(ctx, b, startBand(b.CharID, bandAdjacent))
	if cfg.SecFloor != nil {
		r.SecFloor = *cfg.SecFloor
	}
	for {
		deal, err := m.tradeSearch(ctx, b, cfg, r, budget, hold, askMargin, floorMargin)
		if deal != nil {
			return deal, nil
		}
		// A refusal the wallet caused is not one a wider map fixes, so it returns at once and planSolo sends the
		// pilot to brokeEarn (sim.go).
		if errors.Is(err, errCannotAfford) {
			return nil, err
		}
		wider, ok := m.widen(ctx, b, r)
		if !ok {
			return nil, err
		}
		if noteScopeFail(b.CharID, r.Band) {
			m.logBot(b.CharID, logInfo, "residency",
				"no run worth flying inside %s — widening the search (%s)", m.scopeNote(ctx, r), wider.Band)
		}
		r = wider
	}
}

// tradeSearch is one rung of that climb: the whole arbitrage-plus-speculation
// search under exactly these rules, with no widening of its own.
func (m *Mod) tradeSearch(ctx context.Context, b *botRun, cfg tradeConfig, r travelRules,
	budget, hold, askMargin, floorMargin float64) (*tradeDeal, error) {

	jumps, err := m.reachFrom(ctx, b.SystemID, r)
	if err != nil {
		return nil, err
	}
	if len(jumps) == 0 {
		return nil, fmt.Errorf("this pilot will not enter any system it can reach from here (hard space limit%s)", r.limitNote())
	}

	// The speculative search runs alongside arbitrage and both compare on tradeDeal.expected. A bet is a share of the
	// float; a pilot whose float is too thin to bet any of it runs sure trade-good arbitrage (tradecapital.go).
	var spec *tradeDeal
	var specErr error
	if cfg.NoSpec {
		// The Trade desk's search (corptrade.go): sure runs only, and no "not speculating" log line.
	} else if bet := specBetBudget(budget); bet > 0 {
		spec, specErr = m.findSpecRun(ctx, b, cfg, r, jumps, bet, hold)
	} else if tradeInterest(b.Tune) >= specMinInterest {
		specErr = fmt.Errorf("a %.0f ISK float is too thin to bet any of it — sure runs only", budget)
	}
	// Log why it did not speculate even when the arbitrage search succeeds, so a dedicated trader that has
	// listed nothing is visible in the log.
	if spec == nil && specErr != nil && tradeInterest(b.Tune) >= specMinInterest {
		m.logBot(b.CharID, logInfo, "trade", "not speculating this time — %v", specErr)
	}
	// A blueprint carried to a region with none is a speculative run too; it competes on the same expected
	// value and the same bet budget.
	if bet := specBetBudget(budget); bet > 0 && !cfg.NoSpec {
		if bpo, _ := m.findBPOArbRun(ctx, b, cfg, r, jumps, bet); bpo != nil &&
			(spec == nil || bpo.expected() > spec.expected()) {
			spec, specErr = bpo, nil
		}
		// Market timing competes on the same terms (marketplay.go). A refusal
		// is no complaint: most of the time nothing has crashed or surged.
		for _, find := range []func(context.Context, *botRun, tradeConfig, travelRules, map[int64]int,
			float64, float64) (*tradeDeal, error){m.findDipRun, m.findSurgeRun} {
			if d, ferr := find(ctx, b, cfg, r, jumps, bet, hold); ferr != nil {
				m.logBot(b.CharID, logInfo, "trade", "market timing search: %v", ferr)
			} else if d != nil && (spec == nil || d.expected() > spec.expected()) {
				spec, specErr = d, nil
			}
		}
	}

	deal, err := m.tradeRunAtMargin(ctx, b, cfg, r, jumps, budget, hold, askMargin)
	if retryAtFloor(err, floorMargin, askMargin) {
		m.logBot(b.CharID, logInfo, "trade",
			"nothing pays the %.0f%% spread it wants (%v) — dropping its asking price to %.0f%% rather than sitting on its hands",
			askMargin*100, err, floorMargin*100)
		var ferr error
		deal, ferr = m.tradeRunAtMargin(ctx, b, cfg, r, jumps, budget, hold, floorMargin)
		if ferr != nil {
			err = fmt.Errorf("%w (and nothing at %.0f%% either: %w)", err, floorMargin*100, ferr)
		} else {
			err = nil
		}
	}

	switch {
	case deal != nil && spec != nil:
		if spec.expected() > deal.expected() {
			return spec, nil
		}
		return deal, nil
	case spec != nil:
		return spec, nil
	case deal != nil:
		return deal, nil
	case specErr != nil:
		// %w, not %v: the arbitrage refusal may carry errCannotAfford, which planSolo and findTradeRun must still see.
		return nil, fmt.Errorf("%w; and nothing worth speculating on either (%v)", err, specErr)
	default:
		return nil, err
	}
}

// retryAtFloor reports whether a refused search is worth repeating at the
// pilot's thinner floor spread. Not when the wallet refused it: a lower
// margin makes no gap any cheaper to buy into.
func retryAtFloor(err error, floorMargin, askMargin float64) bool {
	return err != nil && floorMargin < askMargin && !errors.Is(err, errCannotAfford)
}

// tradeRunAtMargin is one pass of the search at one asking spread: scan, cost, rank, and measure the real
// buy-to-sell haul for the best handful.
func (m *Mod) tradeRunAtMargin(ctx context.Context, b *botRun, cfg tradeConfig, r travelRules,
	jumps map[int64]int, budget, hold, margin float64) (*tradeDeal, error) {

	// What a run has to clear to be worth flying (tradefloor.go): a flat 5,000 ISK for solvent pilots, a
	// reachable number for a pilot with a tiny budget.
	profitFloor := tradeSearchFloor(budget, cfg)

	candidates, offMap, err := m.tradeCandidates(ctx, b, cfg, r, jumps, margin)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		// A dedicated trader has no market-group filter (activitycfg.go), so a refusal must not blame one.
		scope := "the whole market"
		if len(cfg.MarketGroupIDs) > 0 {
			scope = "the market groups this pilot deals in"
		}
		if offMap > 0 {
			// Name the gate that is actually shutting the pilot out, and say plainly when none is, rather than
			// blaming a setting.
			if !r.limited() {
				return nil, fmt.Errorf(
					"%s has %d run(s) worth hauling, but none of those stations can be reached "+
						"by gate from here — this pilot's own rules are not the reason (no security "+
						"limit, no scope)",
					scope, offMap)
			}
			return nil, fmt.Errorf(
				"%s has %d run(s) worth hauling, but every one of them "+
					"needs flying through space this pilot will not enter %s",
				scope, offMap, r.limitSummary())
		}
		return nil, fmt.Errorf(
			"nothing in %s is worth hauling anywhere in the database "+
				"(needs a bid at least %.0f%% over an ask at another station)",
			scope, margin*100)
	}

	// The rest of the roster shapes this bot: exact routes in flight are off the table, crowded commodities
	// rank lower, and a commodity flown tradeMaxStreak runs straight is set aside.
	byType, exact := m.tradeActiveRuns(ctx, b.CharID)
	load := m.tradeLaneLoad(ctx, b.CharID)
	streakType := int64(0)
	if b.State.Data.TradeStreak >= b.Tune.TradeMaxStreak {
		streakType = b.State.Data.TradeLastTypeID
	}

	// Rank on the cheap estimate first, then measure the real buy-to-sell haul for the top handful.
	// walletBound is set when the wallet cut a run short; price() runs twice and the till is the same in both.
	walletBound := false
	price := func(decorrelate bool) []*tradeDeal {
		var priced []*tradeDeal
		for i := range candidates {
			d := &candidates[i]
			d.Kind = tradeDealArb
			// The desk's gate first (empty for every solo caller): a run it could not staff is no run at all and
			// must not count toward the wallet verdict.
			if len(cfg.BuySystems) > 0 && !cfg.BuySystems[d.BuySystemID] {
				continue // no member would fly out that far to buy it
			}
			if decorrelate {
				if d.TypeID == streakType {
					continue
				}
				if exact[[3]int64{d.TypeID, d.BuyStationID, d.SellStationID}] {
					continue
				}
				d.Crowd = byType[d.TypeID]
			} else {
				d.Crowd = 0
			}
			if d.UnitVol <= 0 {
				d.UnitVol = 0.01
			}
			// Units are bounded four ways: what is on offer, what is bid for,
			// what the wallet covers, and what the hold takes.
			units := d.Units // stock, filled in by the SQL pass
			// Less what other traders already carry off that ask or into that bid, in both passes. A deal built
			// without the scan's volumes is left alone.
			if d.AskVol > 0 && d.BidVol > 0 {
				left := laneUnitsLeft(d.AskVol, d.BidVol,
					load.outbound[[2]int64{d.TypeID, d.BuyStationID}],
					load.inbound[[2]int64{d.TypeID, d.SellStationID}])
				if left < units {
					units = left
				}
			}
			if units <= 0 {
				continue
			}
			// Which bound bit matters: a run the wallet cut short is a money refusal a pilot can be funded out of,
			// while the hold is a fact about the ship. The message text alone cannot be tested for.
			byISK := int64(budget / d.Ask)
			byVol := int64(hold / d.UnitVol)
			if byISK < units {
				units = byISK
				if byISK <= byVol {
					walletBound = true
				}
			}
			if byVol < units {
				units = byVol
			}
			if !tradeCargoOK(d.CategoryID, units) {
				continue
			}
			d.Units = units
			d.Outlay = float64(units) * d.Ask
			d.Profit = float64(units) * (d.Bid - d.Ask)
			// A desk run is floored on what is left after its hauler's wage (zero for a solo pilot). The wage at the
			// haul's lower bound decides the floor, so a guess cannot refuse a run the real route would clear.
			low, est := tradeHaulEstimate(jumps[d.BuySystemID], jumps[d.SellSystemID])
			d.HaulWage = tradeHaulWage(units, d.UnitVol, low, cfg.HaulCostPerM3Jump)
			if d.expected() < profitFloor {
				continue
			}
			d.HaulWage = tradeHaulWage(units, d.UnitVol, est, cfg.HaulCostPerM3Jump)
			d.Jumps = est
			d.Rank = tradeRank(d)
			priced = append(priced, d)
		}
		return priced
	}
	priced := price(true)
	if len(priced) == 0 && (streakType != 0 || len(exact) > 0) {
		// Spreading out is a preference: when the usual route is the only thing that pays, fly it.
		m.logBot(b.CharID, logInfo, "trade",
			"nothing else on the market pays — going back to the usual run")
		priced = price(false)
	} else if streakType != 0 {
		m.logBot(b.CharID, logInfo, "trade",
			"hauled %s %d runs in a row — looking at something else this time",
			b.State.Data.TradeLastName, b.State.Data.TradeStreak)
	}
	if len(priced) == 0 {
		msg := fmt.Sprintf(
			"found %d price gap(s) it can reach but none clears %.0f ISK once the wallet (%.0f) and hold (%.0f m³) are taken into account",
			len(candidates), profitFloor, budget, hold)
		if cfg.HaulCostPerM3Jump > 0 {
			msg += fmt.Sprintf(", and the hauler's %.0f ISK/m³ a jump paid", cfg.HaulCostPerM3Jump)
		}
		// A trader whose runs were all cut short by the wallet is skint: the errCannotAfford sentinel keeps it off the halt
		// counter and sends it to brokeEarn. It carries no Need figure, since no single gap is the one the pilot would take.
		if walletBound {
			return nil, fmt.Errorf("%s: %w", msg, errCannotAfford)
		}
		return nil, errors.New(msg)
	}
	sort.Slice(priced, func(i, j int) bool { return tradeBetter(priced[i], priced[j]) })
	if len(priced) > tradeRefineTop {
		priced = priced[:tradeRefineTop]
	}

	legCache := map[int64]map[int64]int{}
	var best *tradeDeal
	wageRefused := 0
	for _, d := range priced {
		legs, ok := legCache[d.BuySystemID]
		if !ok {
			legs, err = m.reachFrom(ctx, d.BuySystemID, r)
			if err != nil {
				return nil, err
			}
			legCache[d.BuySystemID] = legs
		}
		haul, ok := legs[d.SellSystemID]
		if !ok {
			continue // the sell station is reachable from home but not from the buy
		}
		d.Jumps = jumps[d.BuySystemID] + haul
		// The scan priced the whole station book at its cheapest ask; keep only the units that still pay on the
		// real ladder at the buy station (sweep.go, tradeRunOnLadder).
		if !m.tradeRunOnLadder(ctx, b, d, budget) {
			continue
		}
		// The wage is charged on the measured buy-to-sell haul, the leg the desk pays for.
		d.HaulWage = tradeHaulWage(d.Units, d.UnitVol, haul, cfg.HaulCostPerM3Jump)
		if d.expected() < profitFloor {
			wageRefused++
			continue
		}
		d.Rank = tradeRank(d)
		if best == nil || tradeBetter(d, best) {
			best = d
		}
	}
	if best == nil {
		if wageRefused > 0 {
			return nil, fmt.Errorf(
				"%d candidate run(s) measured, and none clears %.0f ISK once the hauler's %.0f ISK/m³ a jump is paid",
				wageRefused, profitFloor, cfg.HaulCostPerM3Jump)
		}
		return nil, fmt.Errorf("no candidate run has a legal route from its buy station to its sell station")
	}
	return best, nil
}

// tradeRank prices one run for the ranking: what it is expected to pay, against the flying it costs and the
// crowd already working it. Whether goods move at the far end is already in expected(): a speculative listing
// is discounted by its own evidence. There is no hub-score term (traffic.go header).
func tradeRank(d *tradeDeal) float64 {
	return d.expected() / float64(1+d.Jumps) / float64(1+d.Crowd)
}

// tradeBetter compares two ranked runs.
func tradeBetter(cand, cur *tradeDeal) bool { return cand.Rank > cur.Rank }

// tradeActiveRuns reads what every other enabled trader is hauling, from persisted state: a count per commodity
// and the exact (type, buy station, sell station) routes in flight. The planner discounts the former and
// refuses the latter.
func (m *Mod) tradeActiveRuns(ctx context.Context, exceptCharID int64) (byType map[int64]int, exact map[[3]int64]bool) {
	byType, exact = map[int64]int{}, map[[3]int64]bool{}
	// The roster's shared claims board (claims.go), not a per-pilot read of
	// every state blob.
	forEachClaim(m.claimsBoardNow(ctx), func(charID int64, c botClaim) {
		if charID == exceptCharID || c.Halted || !c.tradeRunning() {
			return
		}
		byType[c.TradeTypeID]++
		exact[[3]int64{c.TradeTypeID, c.TradeBuyStationID, c.TradeSellStationID}] = true
	})
	return
}

// laneLoad is the volume other traders already have in flight against the
// book, keyed by (typeID, stationID).
type laneLoad struct {
	// inbound is what is being hauled to a station to be sold into its bids.
	inbound map[[2]int64]int64
	// outbound is what is about to be bought off a station's asks.
	outbound map[[2]int64]int64
}

// tradeLaneLoad reads the claims board for the units every other trader is about to take off an ask or put into
// a bid. tradeActiveRuns refuses only an exact route; two pilots from different sources converging on one bid
// would both be sized against its whole volume. Same exclusions as tradeActiveRuns.
func (m *Mod) tradeLaneLoad(ctx context.Context, exceptCharID int64) laneLoad {
	l := laneLoad{inbound: map[[2]int64]int64{}, outbound: map[[2]int64]int64{}}
	forEachClaim(m.claimsBoardNow(ctx), func(charID int64, c botClaim) {
		if charID == exceptCharID || c.Halted || !c.tradeRunning() {
			return
		}
		l.add(c)
	})
	return l
}

// add counts one claim, pure. A run that has bought has already taken its
// units off the ask, so counting them again would double the load; a
// speculative run lists at the far end rather than selling into a bid.
func (l laneLoad) add(c botClaim) {
	if !c.TradeBought && c.TradeBuyStationID != 0 {
		l.outbound[[2]int64{c.TradeTypeID, c.TradeBuyStationID}] += c.TradeQty
	}
	if c.TradeKind != tradeDealSpec && c.TradeSellStationID != 0 {
		l.inbound[[2]int64{c.TradeTypeID, c.TradeSellStationID}] += c.TradeQty
	}
}

// laneUnitsLeft is what a run may still take from the two ends of a lane once
// the volume already in flight is set against them, pure.
func laneUnitsLeft(askVol, bidVol, outbound, inbound int64) int64 {
	units := askVol - outbound
	if left := bidVol - inbound; left < units {
		units = left
	}
	if units < 0 {
		return 0
	}
	return units
}

// tradeCandidates asks the market for every (buy here, sell there) pair in the database, with no system, region
// or radius filter: best price per station per side, fattest gaps first. Pairs the pilot cannot route to are
// counted and dropped afterwards, so "no trade" and "your rules fence the trade off" stay distinguishable.
// The volume is the station's whole book on that side, an upper bound; a thinner book fills less.
func (m *Mod) tradeCandidates(ctx context.Context, b *botRun, cfg tradeConfig,
	r travelRules, jumps map[int64]int, margin float64) (deals []tradeDeal, offMap int, err error) {

	rows, err := m.tradeScan(ctx, b, cfg, r, margin)
	if err != nil {
		return nil, 0, err
	}
	for _, d := range rows {
		// Both ends must be somewhere this pilot may fly. The query dropped what its security floor rules out;
		// connectivity is left: a high-sec pocket behind a low-sec bridge reads as allowed but is unreachable.
		if _, ok := jumps[d.BuySystemID]; !ok {
			offMap++
			continue
		}
		if _, ok := jumps[d.SellSystemID]; !ok {
			offMap++
			continue
		}
		deals = append(deals, d)
	}
	return deals, offMap, nil
}

// tradeScanTTL is how long a sweep stands. Reading the whole market costs about a second and NPC-seeded books
// do not move in ninety; every trader flying the same rules shares the answer through the snapshot cache
// (marketsnap.go), which also single-flights the read. See tradeScan.
const tradeScanTTL = 90 * time.Second

// tradeScan is the universe-wide sweep, cached per (rules, filters). The gap pass limits the per-station work to
// types that could pay, the security floor is pushed into the query, and each side is cut to the best
// tradeStationsPerType stations for a type.
func (m *Mod) tradeScan(ctx context.Context, b *botRun, cfg tradeConfig, r travelRules, margin float64) ([]tradeDeal, error) {
	stationClause, stationArgs := m.tradeStationClause(ctx, b.Profile, b.Tune, cfg)

	groupClause, groupArgs := "", []any(nil)
	if groups := m.tradeGroupSubtree(ctx, cfg.MarketGroupIDs); len(groups) > 0 {
		groupClause = ` AND t.marketGroupID IN (` + placeholders(len(groups)) + `)`
		groupArgs = int64Args(groups)
	}

	// Where the pilot may deal, as a predicate rather than a system list. ROUND because mapSolarSystems.security
	// is truesec and the floor is what a player reads off the overview (dispSec in router.go).
	reachClause := ` AND ROUND(ms.security, 1) >= ?`
	reachArgs := []any{r.SecFloor}
	// Space no gate reaches is cut in the query, before the LIMIT, so a marooned region's fat spreads do not crowd out
	// runs a pilot can fly (router.go, maroonedRegions). A pilot inside a marooned pocket gets its own component.
	if u, uerr := m.universe(ctx); uerr == nil && u.maroonedRegions()[u.meta[b.SystemID].RegionID] {
		if comp := u.componentRegionsOf(b.SystemID); len(comp) > 0 {
			ids := make([]int64, 0, len(comp))
			for id := range comp {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			reachClause += ` AND ms.regionID IN (` + placeholders(len(ids)) + `)`
			reachArgs = append(reachArgs, int64Args(ids)...)
		}
	} else if uerr == nil {
		if maroon := u.maroonedRegions(); len(maroon) > 0 {
			ids := make([]int64, 0, len(maroon))
			for id := range maroon {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			reachClause += ` AND ms.regionID NOT IN (` + placeholders(len(ids)) + `)`
			reachArgs = append(reachArgs, int64Args(ids)...)
		}
	}

	// The residency scope is pushed into the query like the security floor, so a book the pilot will not fly
	// to is not ranked twice. market_orders.regionID is indexed.
	if scoped := regionIDs(r.Regions); len(scoped) > 0 {
		reachClause += ` AND ms.regionID IN (` + placeholders(len(scoped)) + `)`
		reachArgs = append(reachArgs, int64Args(scoped)...)
	}

	// One side of the book: best price per station, then the best handful per type. Own orders are dropped after
	// the read (dropOwnStops), never in this query, so one read serves every trader. See docs/comment-notes-b19.md#trade-scan.
	gapTypes, err := m.gapTypes(ctx, margin, regionIDs(r.Regions))
	if err != nil {
		return nil, err
	}
	if len(gapTypes) == 0 {
		return nil, nil
	}
	gapClause := ` AND o.typeID IN (` + placeholders(len(gapTypes)) + `)`

	side := func(bid, agg, dir string) string {
		return `
		SELECT o.typeID, o.stationID, o.solarSystemID,
		       ` + agg + `(o.price) AS price, SUM(o.volRemaining) AS vol,
		       ROW_NUMBER() OVER (PARTITION BY o.typeID ORDER BY ` + agg + `(o.price) ` + dir + `) AS rn
		FROM market_orders o
		JOIN mapSolarSystems ms ON ms.solarSystemID = o.solarSystemID
		JOIN staStations s ON s.stationID = o.stationID
		WHERE o.bid = ` + bid + ` AND o.orderState = 1 AND o.volRemaining > 0` +
			gapClause +
			reachClause + stationClause + `
		GROUP BY o.typeID, o.stationID, o.solarSystemID`
	}

	// tradePairsPerType keeps one commodity from owning the candidate budget: each side is cut to tradeStationsPerType,
	// so one type could offer 1,600 pairs. Ranking within the type first spreads the budget over many commodities.
	query := `
		WITH ask AS (` + side("0", "MIN", "ASC") + `),
		bidside AS (` + side("1", "MAX", "DESC") + `),
		pairs AS (
			SELECT a.typeID, t.typeName, COALESCE(t.volume, 0.01) AS unitVol,
			       COALESCE(g.categoryID, 0) AS categoryID,
			       a.stationID AS buyStation, a.solarSystemID AS buySystem, a.price AS ask, a.vol AS askVol,
			       d.stationID AS sellStation, d.solarSystemID AS sellSystem, d.price AS bid, d.vol AS bidVol,
			       (d.price - a.price) * LEAST(a.vol, d.vol) AS score,
			       ROW_NUMBER() OVER (PARTITION BY a.typeID
			           ORDER BY (d.price - a.price) * LEAST(a.vol, d.vol) DESC) AS prn
			FROM ask a
			JOIN bidside d ON d.typeID = a.typeID AND d.stationID <> a.stationID
			 AND d.price > a.price * ?
			JOIN invTypes t ON t.typeID = a.typeID
			LEFT JOIN invGroups g ON g.groupID = t.groupID
			WHERE a.rn <= ` + fmt.Sprint(tradeStationsPerType) + `
			  AND d.rn <= ` + fmt.Sprint(tradeStationsPerType) + groupClause + `
		)
		SELECT typeID, typeName, unitVol, categoryID,
		       buyStation, buySystem, ask, askVol,
		       sellStation, sellSystem, bid, bidVol
		FROM pairs
		WHERE prn <= ` + fmt.Sprint(tradePairsPerType) + `
		ORDER BY score DESC
		LIMIT ` + fmt.Sprint(tradeMaxCandidates)

	var args []any
	args = append(args, int64Args(gapTypes)...)
	args = append(args, reachArgs...)
	args = append(args, stationArgs...)
	args = append(args, int64Args(gapTypes)...)
	args = append(args, reachArgs...)
	args = append(args, stationArgs...)
	args = append(args, 1+margin)
	args = append(args, groupArgs...)

	// Hashed, not spelled out: the args carry the gap pass's typeID list. No pilot is in either half, so every
	// trader flying the same rules shares one read, and concurrent callers share the one in flight.
	key := snapKey("tradescan", query, args)
	sctx, cancel := snapContext(ctx)
	defer cancel()

	shared, err := snapshot(key, func() ([]tradeDeal, error) {
		rows, err := m.db.QueryContext(sctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("trade scan: %w", err)
		}
		defer rows.Close()

		var out []tradeDeal
		for rows.Next() {
			var d tradeDeal
			var askVol, bidVol int64
			if err := rows.Scan(&d.TypeID, &d.TypeName, &d.UnitVol, &d.CategoryID,
				&d.BuyStationID, &d.BuySystemID, &d.Ask, &askVol,
				&d.SellStationID, &d.SellSystemID, &d.Bid, &bidVol); err != nil {
				return nil, fmt.Errorf("trade scan row: %w", err)
			}
			d.AskVol, d.BidVol = askVol, bidVol
			d.Units = askVol
			if bidVol < d.Units {
				d.Units = bidVol
			}
			if d.Ask <= 0 || d.Units <= 0 {
				continue
			}
			out = append(out, d)
		}
		return out, rows.Err()
	})
	if err != nil {
		return nil, err
	}
	// The shared rowset is read-only to every caller. dropOwnStops returns
	// the pilot's own view of it, which is a fresh slice.
	return m.dropOwnStops(ctx, b.CharID, shared)
}

// dropOwnStops removes the deals whose buy or sell end is a book this pilot is itself standing in. The
// exclusion lives here and not in the scan SQL so the scan stays shareable (see tradeScan). A pilot with no
// orders pays one indexed lookup on (charID, orderState).
func (m *Mod) dropOwnStops(ctx context.Context, charID int64, deals []tradeDeal) ([]tradeDeal, error) {
	if len(deals) == 0 {
		return nil, nil
	}
	type ownKey struct {
		typeID, stationID int64
		bid               bool
	}
	own := map[ownKey]bool{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, stationID, bid FROM market_orders
		WHERE charID = ? AND orderState = 1 AND volRemaining > 0`, charID)
	if err != nil {
		return nil, fmt.Errorf("own orders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k ownKey
		var bid int64
		if err := rows.Scan(&k.typeID, &k.stationID, &bid); err != nil {
			return nil, fmt.Errorf("own orders row: %w", err)
		}
		k.bid = bid != 0
		own[k] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(own) == 0 {
		// Nothing of this pilot's is on the market: the shared rowset is its answer. It is copied anyway because the
		// caller annotates the slice and the original belongs to every other trader.
		out := make([]tradeDeal, len(deals))
		copy(out, deals)
		return out, nil
	}
	out := make([]tradeDeal, 0, len(deals))
	for _, d := range deals {
		// The buy end is somebody's ask (bid = false); the sell end is
		// somebody's bid.
		if own[ownKey{d.TypeID, d.BuyStationID, false}] ||
			own[ownKey{d.TypeID, d.SellStationID, true}] {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// tradeStationClause is the "will it deal here" filter (aliased on s = staStations) built from the pilot's
// standings and the activity's config. A xenophobe's race rule rides the same two lists (tolerance.go): the
// hard half is never dealt with, the soft half joins the merely disliked.
func (m *Mod) tradeStationClause(ctx context.Context, p *Profile, tn Tuning, cfg tradeConfig) (string, []any) {
	var clause strings.Builder
	var args []any

	// The despised are never dealt with. Everyone else the pilot dislikes is off the list too unless the
	// config says hated stations are fair game.
	xSoft, xHard := xenoStationFactions(raceOf(p.CharacterID), tn, p.LikedFactions())
	block := mergeIDs(p.DespisedFactions(), xHard)
	if !cfg.TradeWithHated {
		block = mergeIDs(p.AvoidedFactions(), xSoft, xHard)
	}
	if c, a := factionStationClause(block); c != "" {
		clause.WriteString(c)
		args = append(args, a...)
	}

	if cfg.OnlyLikedFactions {
		if liked := p.LikedFactions(); len(liked) > 0 {
			clause.WriteString(` AND s.corporationID IN (
				SELECT corporationID FROM crpNPCCorporations
				WHERE factionID IN (` + placeholders(len(liked)) + `))`)
			args = append(args, int64Args(liked)...)
		}
	}
	return clause.String(), args
}

// tradeGroupSubtree expands the configured market groups to every group
// beneath them. Returns nil when the config allows everything or the
// subtree is too large to be worth filtering on.
func (m *Mod) tradeGroupSubtree(ctx context.Context, roots []int64) []int64 {
	if len(roots) == 0 {
		return nil
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT marketGroupID, COALESCE(parentGroupID, 0) FROM invMarketGroups`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	children := map[int64][]int64{}
	for rows.Next() {
		var id, parent int64
		if rows.Scan(&id, &parent) == nil {
			children[parent] = append(children[parent], id)
		}
	}

	seen := map[int64]bool{}
	stack := append([]int64(nil), roots...)
	var out []int64
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) > tradeMaxGroupIDs {
			return nil // too broad to filter on: allow everything
		}
		stack = append(stack, children[id]...)
	}
	return out
}

// systemsWithin returns every system reachable from `from` under the rules, mapped to its distance in
// jump-equivalents: real gates plus what the pilot's comfort line charges for the space crossed (router.go,
// hopCost). Distance is priced, not forbidden, so personality becomes a preference. maxJumps bounds that
// cost, not the hop count.
func (m *Mod) systemsWithin(ctx context.Context, from int64, r travelRules, maxJumps int) (map[int64]int, error) {
	u, err := m.universe(ctx)
	if err != nil {
		return nil, err
	}
	m.primeGateDanger(ctx, r)
	m.primeOutlawDanger(ctx, r)
	dist := map[int64]int{from: 0}
	q := &costQueue{}
	heap.Push(q, [2]int64{from, 0})
	for q.Len() > 0 {
		e := heap.Pop(q).([2]int64)
		cur, cost := e[0], int(e[1])
		if cost > dist[cur] {
			continue // stale queue entry
		}
		for _, nb := range u.adj[cur] {
			if !u.allowed(nb, r) {
				continue
			}
			nc := cost + u.hopCost(cur, nb, r)
			if nc > maxJumps {
				continue
			}
			if seen, ok := dist[nb]; ok && seen <= nc {
				continue
			}
			dist[nb] = nc
			heap.Push(q, [2]int64{nb, int64(nc)})
		}
	}
	return dist, nil
}

// ── the run ─────────────────────────────────────────────────────────────────

// tradeStep advances the trade.* phases.
func (m *Mod) tradeStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	switch st.Phase {
	case "trade.buy":
		return false, m.tradeBuy(ctx, b)
	case "trade.pickup":
		return false, m.tradePickup(ctx, b)
	case "trade.load":
		// The dwell after the buy is over: undock and haul.
		return false, m.beginTravel(ctx, b, st.Data.TradeSellSystemID, st.Data.TradeSellStationID, "trade.sell")
	case "trade.sell":
		return true, m.tradeSell(ctx, b)
	}
	return false, fmt.Errorf("unknown trade phase %q", st.Phase)
}

// tradeBuy fills the hold at the buy station and sets off for the buyer.
func (m *Mod) tradeBuy(ctx context.Context, b *botRun) error {
	st := b.State
	if b.StationID != st.Data.TradeBuyStationID {
		return fmt.Errorf("trade.buy at station %d, expected %d", b.StationID, st.Data.TradeBuyStationID)
	}
	if b.ShipID == 0 {
		return fmt.Errorf("trade.buy without a ship to load")
	}

	// Prices move between planning and arrival. Re-read the ask and walk it
	// up a little rather than refusing over a tick of drift.
	var ask sql.NullFloat64
	if err := m.db.QueryRowContext(ctx, `
		SELECT MIN(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0`,
		b.StationID, st.Data.TradeTypeID).Scan(&ask); err != nil {
		return fmt.Errorf("trade.buy ask: %w", err)
	}
	if !ask.Valid {
		return m.tradeAbandon(ctx, b, "the sell order it came for is gone")
	}
	if st.Data.TradeBid > 0 && ask.Float64 >= st.Data.TradeBid {
		return m.tradeAbandon(ctx, b,
			fmt.Sprintf("the ask moved to %.2f, at or above the %.2f bid it was hauling to",
				ask.Float64, st.Data.TradeBid))
	}

	ceiling := m.tradeBuyCeiling(ctx, ask.Float64, st.Data.TradeBid)
	if st.Data.TradeBuyCap > 0 {
		// The fill takes any ask under the ceiling, so a dip's ceiling is what
		// keeps it off NPC, corp and factory shelves (dipboard.go).
		var ok bool
		if ceiling, ok = dipCeiling(st.Data.TradeBuyCap, m.protectedAskAt(ctx, b.StationID, st.Data.TradeTypeID), ask.Float64); !ok {
			return m.tradeAbandon(ctx, b, "the crashed ask it came for is gone")
		}
	}
	out, err := evedb.PlaceBuyOrder(ctx, m.db, b.CharID, b.StationID, st.Data.TradeTypeID,
		ceiling, st.Data.TradeQty, -1 /*station range*/, 1, 0 /*immediate*/, false)
	if err != nil {
		return fmt.Errorf("trade buy: %w", err)
	}
	if out.ErrorMsg != "" {
		return m.tradeAbandon(ctx, b, "buy refused: "+out.ErrorMsg)
	}
	if out.FilledQty <= 0 {
		return m.tradeAbandon(ctx, b, "nothing filled at the buy station")
	}
	var spent float64
	for _, f := range out.Fills {
		spent += f.Price * float64(f.Quantity)
	}
	// Several fills can land as several stacks; one row is easier to haul.
	st.Data.TradeItemID = 0
	if m.isBlueprintType(ctx, st.Data.TradeTypeID) {
		// A print is a singleton row, which moveHangarToCargo (packaged stacks only) never moves. The one to carry
		// is the newest untouched original of the type in this hangar.
		var itemID int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT e.itemID FROM entity e
			LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
			WHERE e.ownerID = ? AND e.locationID = ? AND e.typeID = ? AND e.flag = 4
			  AND COALESCE(bp.materialLevel, 0) = 0 AND COALESCE(bp.productivityLevel, 0) = 0
			  AND COALESCE(bp.copy, 0) = 0
			ORDER BY e.itemID DESC LIMIT 1`,
			b.CharID, b.StationID, st.Data.TradeTypeID).Scan(&itemID)
		if itemID == 0 {
			return m.tradeAbandon(ctx, b, "the blueprint it bought is not in the hangar")
		}
		if err := m.moveItemToCargo(ctx, b, itemID); err != nil {
			return fmt.Errorf("loading the hold: %w", err)
		}
		st.Data.TradeItemID = itemID
	} else {
		if len(out.Fills) > 1 {
			_, _ = m.mergeStacks(ctx, b.CharID, b.StationID, st.Data.TradeTypeID)
		}
		loaded, err := m.moveHangarToCargoN(ctx, b, st.Data.TradeTypeID)
		if err != nil {
			return fmt.Errorf("loading the hold: %w", err)
		}
		// What the hold could not take stays here on the stock ledger at its cost, so a sweep owns it. The run
		// carries, and is costed on, only what was loaded.
		if carry, shelf := splitCarry(out.FilledQty, loaded); shelf > 0 {
			unit := spent / float64(out.FilledQty)
			m.addStock(ctx, b.CharID, b.StationID, b.SystemID, st.Data.TradeTypeID,
				st.Data.TradeName, shelf, unit)
			if carry == 0 {
				return m.tradeAbandon(ctx, b, "the hold took none of what it bought — the lot waits on its stock ledger")
			}
			spent = unit * float64(carry)
			out.FilledQty = carry
		}
	}

	st.Data.TradeQty = out.FilledQty
	st.Data.TradeSpent = spent
	st.Data.TradeUnitCost = spent / float64(out.FilledQty)
	st.Data.TradeFresh = true
	st.Data.TradeBuySystemID = b.SystemID
	e := tradeEntry{
		CharID: b.CharID, Kind: tradeKindBuy, TypeID: st.Data.TradeTypeID, TypeName: st.Data.TradeName,
		Qty: out.FilledQty, Price: st.Data.TradeUnitCost, Value: spent,
		Station: b.StationID, System: b.SystemID,
		Note:   resaleNoteHaul + m.stationLabel(ctx, st.Data.TradeSellStationID),
		Resale: true,
	}
	m.bookTrade(ctx, e)
	m.notePeerFills(ctx, e, out.Fills, true, "haul")
	m.logBot(b.CharID, logInfo, "trade", "bought %d× %s for %.2f ISK at %s — hauling to %s",
		out.FilledQty, st.Data.TradeName, spent, m.stationLabel(ctx, b.StationID),
		m.stationLabel(ctx, st.Data.TradeSellStationID))

	// Loading a hold takes a while: the bot stays on the guest list while the cargo goes aboard, longer when
	// somebody is around to see it.
	st.Phase = "trade.load"
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return saveState(ctx, m.db, st)
}

// tradeSell unloads into the bids at the destination and books the run.
func (m *Mod) tradeSell(ctx context.Context, b *botRun) error {
	st := b.State
	if b.StationID == 0 {
		return fmt.Errorf("trade.sell while not docked")
	}
	if err := m.moveCargoToHangar(ctx, b, st.Data.TradeTypeID); err != nil {
		return err
	}
	var itemID, qty int64
	var err error
	if st.Data.TradeItemID != 0 {
		// A carried print (tradeBuy): a singleton, found by its own ID.
		err = m.db.QueryRowContext(ctx, `
			SELECT itemID, quantity FROM entity
			WHERE itemID = ? AND ownerID = ? AND locationID = ? AND flag = 4`,
			st.Data.TradeItemID, b.CharID, b.StationID).Scan(&itemID, &qty)
	} else {
		err = m.db.QueryRowContext(ctx, `
			SELECT itemID, quantity FROM entity
			WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0
			ORDER BY quantity DESC LIMIT 1`,
			b.CharID, b.StationID, st.Data.TradeTypeID).Scan(&itemID, &qty)
	}
	if err == sql.ErrNoRows {
		return m.tradeAbandon(ctx, b, "the cargo it hauled is not in the hangar")
	}
	if err != nil {
		return err
	}

	// A speculative run does not need a bid: it lists the goods at its own asking price and leaves, and the
	// order sweep books the money when somebody takes it (tradeorders.go).
	if st.Data.TradeKind == tradeDealSpec {
		return m.tradeListRun(ctx, b, itemID, qty)
	}

	// Sell down to the lowest live bid, so the whole load clears rather
	// than stopping at the best price level.
	var floor sql.NullFloat64
	_ = m.db.QueryRowContext(ctx, `
		SELECT MIN(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0`,
		b.StationID, st.Data.TradeTypeID).Scan(&floor)
	if !floor.Valid {
		// The bids evaporated in transit. With a free order slot the trader lists the goods at its own price; otherwise
		// it files them as stock and comes back for them (tradeStockPlan).
		if m.freeOrderSlots(ctx, b.CharID) > 0 {
			m.logBot(b.CharID, logInfo, "trade",
				"the bids for %s were gone by the time it docked — listing the %d unit(s) itself",
				st.Data.TradeName, qty)
			st.Data.TradeKind = tradeDealSpec
			return m.tradeListRun(ctx, b, itemID, qty)
		}
		m.logBot(b.CharID, logWarn, "trade",
			"no bid left for %s at %s and no order slot free — leaving %d unit(s) in the hangar",
			st.Data.TradeName, m.stationLabel(ctx, b.StationID), qty)
		m.addStock(ctx, b.CharID, b.StationID, b.SystemID, st.Data.TradeTypeID,
			st.Data.TradeName, qty, st.Data.TradeUnitCost)
		st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
		return saveState(ctx, m.db, st)
	}

	out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID, st.Data.TradeTypeID,
		itemID, floor.Float64, qty, -1 /*station*/, 1, 0 /*immediate*/, false)
	if err != nil {
		return fmt.Errorf("trade sell: %w", err)
	}
	if out.ErrorMsg != "" {
		m.logBot(b.CharID, logWarn, "trade", "sell refused (%s) — the load stays in the hangar", out.ErrorMsg)
		m.addStock(ctx, b.CharID, b.StationID, b.SystemID, st.Data.TradeTypeID,
			st.Data.TradeName, qty, st.Data.TradeUnitCost)
	} else {
		var gross float64
		for _, f := range out.Fills {
			gross += f.Price * float64(f.Quantity)
		}
		unit := 0.0
		if out.FilledQty > 0 {
			unit = gross / float64(out.FilledQty)
		}
		// Net of sales tax (tradefees.go). The buy leg was an immediate fill with no broker fee, so TradeSpent is the
		// whole cost. On a partial fill the unsold remainder goes on the stock ledger and only the sold share is charged.
		cost := st.Data.TradeSpent
		if unsold := qty - out.FilledQty; unsold > 0 {
			m.addStock(ctx, b.CharID, b.StationID, b.SystemID, st.Data.TradeTypeID,
				st.Data.TradeName, unsold, st.Data.TradeUnitCost)
			cost = st.Data.TradeUnitCost * float64(out.FilledQty)
		}
		profit := m.netSaleAt(ctx, b.CharID, b.StationID, gross) - cost
		m.takeStock(ctx, b.CharID, b.StationID, st.Data.TradeTypeID, out.FilledQty)
		e := tradeEntry{
			CharID: b.CharID, Kind: tradeKindSell, TypeID: st.Data.TradeTypeID, TypeName: st.Data.TradeName,
			Qty: out.FilledQty, Price: unit, Value: gross, Profit: profit,
			Station: b.StationID, System: b.SystemID, FromSys: st.Data.TradeBuySystemID,
			Note: "unloaded into the bids",
		}
		m.bookTrade(ctx, e)
		m.notePeerFills(ctx, e, out.Fills, false, "haul")
		// And the run itself, as one line against the lane it was flown over, so a route shows on the map as a route.
		m.bookTrade(ctx, tradeEntry{
			CharID: b.CharID, Kind: tradeKindHaul, TypeID: st.Data.TradeTypeID, TypeName: st.Data.TradeName,
			Qty: out.FilledQty, Price: unit, Value: gross, Profit: profit,
			Station: b.StationID, System: b.SystemID, FromSys: st.Data.TradeBuySystemID,
			Note: "run completed",
		})
		m.logBot(b.CharID, logInfo, "trade",
			"sold %d× %s for %.2f ISK at %s — %+.2f ISK on the run",
			out.FilledQty, st.Data.TradeName, gross, m.stationLabel(ctx, b.StationID), profit)
	}
	st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
	// Settle up: the next run is planned after the hangar business, not in the same breath as the sale.
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return saveState(ctx, m.db, st)
}

// splitCarry is how a buy splits between the hold and the hangar: the run
// carries what was loaded, up to what was bought, and the rest is shelved.
// Pure.
func splitCarry(filled, loaded int64) (carry, shelf int64) {
	if loaded >= filled {
		return filled, 0
	}
	if loaded < 0 {
		loaded = 0
	}
	return loaded, filled - loaded
}

// tradeAbandon drops a run that stopped making sense mid-flight. The market moving is ordinary business, not
// a bot failure, so it goes back to planning instead of counting toward the halt threshold.
func (m *Mod) tradeAbandon(ctx context.Context, b *botRun, why string) error {
	st := b.State
	specClaims.release(b.CharID)
	m.logBot(b.CharID, logInfo, "trade", "abandoning the run — %s", why)
	st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + idleRetrySec
	return saveState(ctx, m.db, st)
}

// moveHangarToCargo loads the hangar stacks of typeID at the bot's station
// into its ship's cargo hold, as far as the hold has room, and folds them
// into whatever of that type the hold already carries (stacking.go).
func (m *Mod) moveHangarToCargo(ctx context.Context, b *botRun, typeID int64) error {
	_, err := m.moveHangarToCargoN(ctx, b, typeID)
	return err
}

// moveHangarToCargoN is moveHangarToCargo reporting the units it loaded. It loads whole stacks that fit and
// splits the last one, so units the hold cannot take stay in the hangar even when an old stockpile of the
// type sits at the station.
func (m *Mod) moveHangarToCargoN(ctx context.Context, b *botRun, typeID int64) (int64, error) {
	if b.StationID == 0 {
		return 0, fmt.Errorf("not docked")
	}
	if b.ShipID == 0 {
		return 0, fmt.Errorf("not in a ship")
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE locationID = ? AND ownerID = ? AND typeID = ? AND flag = 4 AND singleton = 0
		ORDER BY quantity DESC, itemID`,
		b.StationID, b.CharID, typeID)
	if err != nil {
		return 0, err
	}
	var stacks []hangarStack
	for rows.Next() {
		var s hangarStack
		if err := rows.Scan(&s.ItemID, &s.Qty); err != nil {
			rows.Close()
			return 0, err
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	plan := hangarLoadPlan(stacks, m.typeVolume(ctx, typeID), m.shipHoldRoom(ctx, b))
	var moved int64
	for _, t := range plan {
		id := t.ItemID
		if t.Split {
			if id, err = evedb.SplitStackN(ctx, m.db, t.ItemID, t.Units); err != nil {
				return moved, err
			}
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE entity SET locationID = ?, flag = 5 WHERE itemID = ?`, b.ShipID, id); err != nil {
			return moved, err
		}
		moved += t.Units
	}
	if moved > 0 {
		m.stackCargo(ctx, b.CharID, b.ShipID)
	}
	return moved, nil
}

// hangarStack is one hangar row of a type a pilot is loading.
type hangarStack struct {
	ItemID, Qty int64
}

// hangarTake is one stack's part of a load: the whole row, or Units split
// off it.
type hangarTake struct {
	ItemID, Units int64
	Split         bool
}

// hangarLoadPlan picks the stacks (whole, then one split) that fit in free m3 at unitVol a unit. A type with
// no known volume is loaded whole. Pure, so the harness pins it.
func hangarLoadPlan(stacks []hangarStack, unitVol, free float64) []hangarTake {
	var out []hangarTake
	for _, s := range stacks {
		if s.Qty <= 0 {
			continue
		}
		if unitVol <= 0 {
			out = append(out, hangarTake{ItemID: s.ItemID, Units: s.Qty})
			continue
		}
		fit := int64(math.Floor(free/unitVol + 1e-9))
		if fit <= 0 {
			break
		}
		if fit >= s.Qty {
			out = append(out, hangarTake{ItemID: s.ItemID, Units: s.Qty})
			free -= float64(s.Qty) * unitVol
			continue
		}
		out = append(out, hangarTake{ItemID: s.ItemID, Units: fit, Split: true})
		break
	}
	return out
}
