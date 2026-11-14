//go:build scc

package rolebots

import (
	"math"
	"math/rand"
	"sort"

	evedb "gurimoon/db"
)

// The SCC Exchange investor: how a bot decides what a listed corporation's stock
// is worth and whether to trade it. Pure functions of a pilot's traits and an SCC
// filing; the exchange is the core's (db/sccengine.go, db/sccexchange.go).
//
//	V_i = (BookPS + EPS_week x H_i) x (1 - risk) x conviction_i
//	      x (1 + beta_i*shock) x (1 + 0.15*climate) x (1 - 0.15*rel_sector)
//
// The last three terms are the world: the decayed shock of material events
// (db/sccevents.go), the sector climate (db/sccmacro.go) and the sector price
// level (sccsector.go).

// sccStyle is one way of trading.
type sccStyle int

const (
	sccValue sccStyle = iota
	sccIncome
	sccMomentum
	sccLoyalist
	sccSpeculator
	sccStyleCount
)

var sccStyleNames = [sccStyleCount]string{"value", "income", "momentum", "loyalist", "speculator"}

func (s sccStyle) String() string {
	if s < 0 || s >= sccStyleCount {
		return "?"
	}
	return sccStyleNames[s]
}

// The investor's dials.
const (
	sccStockFracBase = 0.10
	sccStockFracMax  = 0.60

	// Opinions: lognormal spread by caution (careful to careless), and how much of
	// last week's opinion survives into this week's.
	sccConvictionSigmaCareful  = 0.08
	sccConvictionSigmaNeutral  = 0.15
	sccConvictionSigmaCareless = 0.25
	sccConvictionPersist       = 0.7

	// Portfolio review cadence in hours, by patience (unhurried to hurried).
	sccReviewHoursUnhurried = 48
	sccReviewHoursNeutral   = 24
	sccReviewHoursHurried   = 6

	// The value style's bands around V.
	sccValueBuyBelow  = 0.90
	sccValueSellAbove = 1.15
	// Momentum: a week up this much with rising volume is a buy; down from
	// the entry by the stop is a sell.
	sccMomentumEntry = 0.08
	sccMomentumStop  = -0.15
	// Income: a yearly yield worth holding for at an acceptable risk.
	sccIncomeYield   = 0.04
	sccIncomeMaxRisk = 0.12

	// The net signal must clear this before anything is placed.
	sccActThreshold = 0.25

	// News and the economy (sccSentiment). Beta by caution, careful to careless.
	sccShockBetaCareful  = 0.15
	sccShockBetaNeutral  = 0.30
	sccShockBetaCareless = 0.50
	sccShockCap          = 1.5 // a pile-up of events counts this much at most
	sccClimateBeta       = 0.15
	// sccSectorBeta is the price-level term: at the +-1 clamp a sector is marked
	// +-15%, inside the value style's 0.90/1.15 bands, so it alone never flips a
	// buy to a sell.
	sccSectorBeta     = 0.15
	sccSentimentFloor = 0.2
	// A speculator trades a shock past this, on any stock; a smaller one
	// only moves it on a small cap's bulletin, as before.
	sccShockSignal = 0.25
	// Makers widen their quotes by (1 + this × |shock|) while news is fresh.
	sccMakerShockWiden = 1.0

	// Stops (sccStopPolicy).
	sccStopTrailHurried   = 0.10
	sccStopTrailUnhurried = 0.20
	sccStopFixedFrac      = 0.20 // a careful holder's stop sits this far under cost
	sccStopLimitSlack     = 0.05 // a stop-limit accepts this far past its trigger
	sccMaxBotStops        = 5
	// sccStopReentrySec: a pilot stopped out of a stock does not buy it back for
	// this long, or a momentum trader would rebuy what its stop just sold.
	sccStopReentrySec = 3 * 86400
	// sccMaxOpenOrders is per pilot, across the whole exchange.
	sccMaxOpenOrders = 5
	// A single buy spends at most this share of the stock budget, so a
	// portfolio is built over reviews rather than in one lunge.
	sccBuyTranche = 0.35

	// The lottery ticket: a combat pilot after a big bounty puts this share
	// of it into a mercenary corporation at market.
	sccLotteryFrac = 0.05

	// Market makers quote V ± a half-spread in small size.
	sccMakerSpreadLo  = 0.03
	sccMakerSpreadHi  = 0.06
	sccMakerSkew      = 0.02 // extra half-spread per unit of inventory off target
	sccMakerMaxLists  = 3
	sccMakerQuoteFrac = 0.04 // of the stock budget per side

	// sccRequoteTolerance: a resting order within this fraction of the price a
	// review would now place stays, since every placement pays the broker fee.
	sccRequoteTolerance = 0.01
)

// sccTraitPoles splits the traits the investor reads into their two poles,
// each 0..1, by the module's polarity convention (Low is the docile end).
type sccTraitPoles struct {
	Careful, Careless   float64 // caution
	Generous, Greedy    float64 // greed
	Unhurried, Hurried  float64 // patience
	Pacifist, Violent   float64 // aggression
	Devoted, Mercenary  float64 // loyalty
	Leader              float64 // leadership
	Xenophobe, Xenophil float64 // tolerance
}

func sccPoles(traits map[string]int) sccTraitPoles {
	split := func(k string) (lo, hi float64) {
		t := traitScale(traits[k])
		return math.Max(0, -t), math.Max(0, t)
	}
	var p sccTraitPoles
	p.Careful, p.Careless = split(TraitCaution)
	p.Generous, p.Greedy = split(TraitGreed)
	p.Unhurried, p.Hurried = split(TraitPatience)
	p.Pacifist, p.Violent = split(TraitAggression)
	p.Devoted, p.Mercenary = split(TraitLoyalty)
	_, p.Leader = split(TraitLeadership)
	p.Xenophil, p.Xenophobe = split(TraitTolerance)
	return p
}

// sccInvestor is a pilot resolved for the exchange.
type sccInvestor struct {
	StockFrac       float64
	HorizonWeeks    float64
	ConvictionSigma float64
	ReviewHours     float64
	Styles          [sccStyleCount]float64 // weights, summing to 1
	Dominant        sccStyle
	HomeBias        float64 // 0 mercenary .. 1 devoted: how much "own corp" counts
	Patience        float64 // 0 hurried .. 1 unhurried, for order pricing and expiry
	MakerHalfSpread float64
	Xenophobe       float64
	ShockBeta       float64 // how far a material event moves this pilot's V
}

// sccStockFrac is the plan's participation rule.
//
//	stockFrac = clamp(0.10 + 0.25·greedy + 0.15·pacifist − 0.20·violent + 0.10·leader, 0, 0.60)
func sccStockFrac(p sccTraitPoles) float64 {
	f := sccStockFracBase + 0.25*p.Greedy + 0.15*p.Pacifist - 0.20*p.Violent + 0.10*p.Leader
	return math.Max(0, math.Min(sccStockFracMax, f))
}

// sccInvestorFor resolves a pilot's traits. Pure.
func sccInvestorFor(traits map[string]int) sccInvestor {
	p := sccPoles(traits)
	pat := traitScale(traits[TraitPatience])
	inv := sccInvestor{
		StockFrac:       sccStockFrac(p),
		HorizonWeeks:    bend(pat, 26, 16, 8),
		ConvictionSigma: bend(traitScale(traits[TraitCaution]), sccConvictionSigmaCareful, sccConvictionSigmaNeutral, sccConvictionSigmaCareless),
		ReviewHours:     bend(pat, sccReviewHoursUnhurried, sccReviewHoursNeutral, sccReviewHoursHurried),
		HomeBias:        0.5 + 0.5*p.Devoted - 0.5*p.Mercenary,
		Patience:        0.5 + 0.5*p.Unhurried - 0.5*p.Hurried,
		MakerHalfSpread: sccMakerSpreadLo + (sccMakerSpreadHi-sccMakerSpreadLo)*p.Greedy,
		Xenophobe:       p.Xenophobe,
		ShockBeta:       bend(traitScale(traits[TraitCaution]), sccShockBetaCareful, sccShockBetaNeutral, sccShockBetaCareless),
	}
	// A small base on value and income: a pilot with no opinions at all
	// still buys cheap stock and likes being paid.
	w := [sccStyleCount]float64{
		sccValue:      0.25 + (p.Unhurried+p.Careful)/2,
		sccIncome:     0.10 + (p.Careful+p.Generous)/2,
		sccMomentum:   (p.Hurried + p.Careless + p.Greedy) / 3,
		sccLoyalist:   p.Devoted,
		sccSpeculator: (p.Careless + p.Greedy) / 2,
	}
	var sum float64
	for _, x := range w {
		sum += x
	}
	best := sccValue
	for i := range w {
		w[i] /= sum
		if w[i] > w[best] {
			best = sccStyle(i)
		}
	}
	inv.Styles, inv.Dominant = w, best
	return inv
}

// sccValueOf is V_i. Pure. Never negative: a corporation worth less than
// nothing is worth nothing to a shareholder with limited liability.
func sccValueOf(bookPS, epsWeek, horizonWeeks, risk, conviction float64) float64 {
	return evedb.SCCFairValue(bookPS, epsWeek, horizonWeeks, risk) * math.Max(0, conviction)
}

// sccSentiment is what the news, the economy and the board's price level do
// to a pilot's V: the shock scaled by the pilot's β, the sector climate and
// the sector's relative valuation (sccSectorAggregates) each by a common β.
// Floored, so no pile-up of bad news values a going concern at nothing. Pure.
func sccSentiment(inv sccInvestor, shock, climate, sectorRel float64) float64 {
	shock = math.Max(-sccShockCap, math.Min(sccShockCap, shock))
	sectorRel = math.Max(-1, math.Min(1, sectorRel))
	f := (1 + inv.ShockBeta*shock) * (1 + sccClimateBeta*climate) * (1 - sccSectorBeta*sectorRel)
	return math.Max(sccSentimentFloor, f)
}

// sccViewValue is V for one view: the filing, the opinion, and the world.
func sccViewValue(inv sccInvestor, s sccStockView) float64 {
	return sccValueOf(s.BookPS, s.EPSWeek, inv.HorizonWeeks, s.Risk, s.Conviction) *
		sccSentiment(inv, s.Shock, s.Climate, s.SectorRel)
}

// sccNextConviction redraws a pilot's opinion of one corporation: lognormal
// around 1, mean-reverting, so it wanders week to week rather than being
// redrawn from scratch. prev 0 is "no opinion yet" and draws fresh.
func sccNextConviction(rng *rand.Rand, prev, sigma float64) float64 {
	z := rng.NormFloat64()
	if prev <= 0 {
		return math.Exp(sigma * z)
	}
	ln := sccConvictionPersist*math.Log(prev) + math.Sqrt(1-sccConvictionPersist*sccConvictionPersist)*sigma*z
	return math.Exp(ln)
}

// sccStockView is what a pilot sees of one listing on a review: the SCC filing,
// the quote, and its own position.
type sccStockView struct {
	CorpID       int64
	BookPS       float64
	EPSWeek      float64
	Risk         float64 // from the SCC rating
	RiskDelta    float64 // since last filing week; positive = downgraded
	YieldYear    float64 // dividends paid over the last 28d, annualised, over price
	DividendCut  bool
	Last         float64 // last trade, or the reference
	BestBid      float64 // 0 = none
	BestAsk      float64 // 0 = none
	Ret7         float64 // 7-day return
	VolumeRising bool
	Bulletin     float64 // +1 good news in the last day, −1 bad, 0 none
	SmallCap     bool
	Shock        float64 // the SCC's decayed material events on this stock
	Climate      float64 // the stock's sector climate, −1..1
	SectorRel    float64 // the sector's price/fair against the board's, −1..1 (sccSectorAggregates)
	HasStop      bool    // the pilot holds a stop on it: the stop is the momentum exit
	StoppedOut   bool    // a stop of the pilot's sold it lately: no buying it back yet
	Own          bool    // the pilot's own corporation (or alliance / home faction)
	HostileRace  bool    // CEO of a race this pilot's tolerance shuns
	Band         evedb.SCCBand

	Held       int64
	CostBasis  float64 // per share; 0 = none
	Conviction float64
}

// sccIntent is one order a review decided to place.
type sccIntent struct {
	CorpID     int64
	IsBuy      bool
	Qty        int64
	Price      float64
	ExpiryDays int // 0 = immediate-or-cancel
	Why        sccStyle
}

// sccSignals scores one listing for each style, −1 sell .. +1 buy. Pure.
func sccSignals(inv sccInvestor, s sccStockView, v float64) [sccStyleCount]float64 {
	var sig [sccStyleCount]float64
	px := s.Last
	if s.BestAsk > 0 {
		px = s.BestAsk
	}
	if px <= 0 || v <= 0 {
		return sig
	}
	// Value: cheap against V buys, rich against V sells.
	switch r := px / v; {
	case r < sccValueBuyBelow:
		sig[sccValue] = math.Min(1, (sccValueBuyBelow-r)/0.1+0.5)
	case s.BestBid > 0 && s.BestBid/v > sccValueSellAbove:
		sig[sccValue] = -1
	}
	// Income: buys and holds a yield at acceptable risk; leaves on a cut
	// or a downgrade.
	switch {
	case s.DividendCut || s.RiskDelta > 0.02:
		sig[sccIncome] = -1
	case s.YieldYear >= sccIncomeYield && s.Risk <= sccIncomeMaxRisk && px < v*1.1:
		sig[sccIncome] = 0.75
	}
	// Momentum: rides a rising week, stops out of a falling one unless a stop
	// order already guards the position (it exits at the trigger).
	switch {
	case !s.HasStop && s.Held > 0 && s.CostBasis > 0 && s.Last/s.CostBasis-1 <= sccMomentumStop:
		sig[sccMomentum] = -1
	case s.Ret7 >= sccMomentumEntry && s.VolumeRising:
		sig[sccMomentum] = 1
	case s.Ret7 <= -sccMomentumEntry:
		sig[sccMomentum] = -0.75
	}
	// Loyalist: its own outfit, near any price.
	if s.Own && px < v*1.5 {
		sig[sccLoyalist] = 1
	}
	// Speculator: fresh news on anything, or a bulletin on a small cap.
	switch {
	case s.Shock >= sccShockSignal:
		sig[sccSpeculator] = 1
	case s.Shock <= -sccShockSignal:
		sig[sccSpeculator] = -1
	case s.SmallCap && s.Bulletin != 0:
		sig[sccSpeculator] = s.Bulletin
	}
	return sig
}

// sccReview decides what one pilot does with one listing. budget is the ISK
// it may still put into stocks (StockFrac of surplus, less what it already
// holds at market), cash what it actually has free. Pure.
func sccReview(inv sccInvestor, s sccStockView, budget, cash float64) (sccIntent, bool) {
	if s.HostileRace && xenoPhobeStage(inv.Xenophobe) >= xenoStageAllied {
		if s.Held > 0 && s.BestBid > 0 {
			return sccSellIntent(inv, s, s.Held, sccValue), true
		}
		return sccIntent{}, false
	}
	v := sccViewValue(inv, s)
	sig := sccSignals(inv, s, v)
	own := inv.HomeBias
	var net, why float64
	whyStyle := inv.Dominant
	for i, x := range sig {
		w := inv.Styles[i]
		if sccStyle(i) == sccLoyalist {
			w *= own * 2
		}
		net += w * x
		if math.Abs(w*x) > why {
			why, whyStyle = math.Abs(w*x), sccStyle(i)
		}
	}
	switch {
	case net >= sccActThreshold && !s.StoppedOut:
		spend := math.Min(budget, cash) * sccBuyTranche * math.Min(1, net)
		price := sccBuyPrice(inv, s, v)
		if price <= 0 {
			return sccIntent{}, false
		}
		// The broker fee comes out of the same cash.
		qty := int64(spend / (price * (1 + evedb.SCCBrokerFeeDefault)))
		if qty <= 0 {
			return sccIntent{}, false
		}
		return sccIntent{CorpID: s.CorpID, IsBuy: true, Qty: qty, Price: price,
			ExpiryDays: sccExpiry(inv), Why: whyStyle}, true
	case net <= -sccActThreshold && s.Held > 0:
		qty := int64(math.Ceil(float64(s.Held) * math.Min(1, -net)))
		return sccSellIntent(inv, s, qty, whyStyle), true
	}
	return sccIntent{}, false
}

// sccBuyPrice is where a buy is placed: a patient pilot bids under value and
// waits, a hurried one takes the ask if it is anywhere near value. Clamped
// into the band and to whole ISK.
func sccBuyPrice(inv sccInvestor, s sccStockView, v float64) float64 {
	patientBid := v * (0.97 - 0.07*inv.Patience)
	p := patientBid
	if s.BestAsk > 0 && (s.BestAsk <= patientBid || inv.Patience < 0.4 && s.BestAsk <= v*1.05) {
		p = s.BestAsk
	}
	// A price under the band's floor is not raised to it: that would bid more than
	// the pilot's own valuation and fill the book with bids nobody believes in.
	if p = math.Floor(p); p < s.Band.Lo {
		return 0
	}
	return sccClampBand(p, s.Band)
}

func sccSellIntent(inv sccInvestor, s sccStockView, qty int64, why sccStyle) sccIntent {
	v := sccViewValue(inv, s)
	p := math.Max(s.Last, v) * (1.02 + 0.06*inv.Patience)
	if s.BestBid > 0 && (inv.Patience < 0.4 || why == sccMomentum) {
		p = s.BestBid
	}
	if qty > s.Held {
		qty = s.Held
	}
	return sccIntent{CorpID: s.CorpID, Qty: qty, Price: sccClampBand(math.Ceil(p), s.Band),
		ExpiryDays: sccExpiry(inv), Why: why}
}

func sccClampBand(p float64, b evedb.SCCBand) float64 {
	if p < b.Lo {
		p = b.Lo
	}
	if p > b.Hi {
		p = b.Hi
	}
	return p
}

// sccExpiry: a patient pilot leaves an order for a month, a hurried one for a
// day.
func sccExpiry(inv sccInvestor) int {
	switch {
	case inv.Patience >= 0.75:
		return 30
	case inv.Patience >= 0.5:
		return 7
	case inv.Patience >= 0.25:
		return 3
	}
	return 1
}

// sccShouldReplace reports whether a resting order at oldPrice is far enough
// from newPrice to be worth a fresh broker fee. Pure.
func sccShouldReplace(oldPrice, newPrice float64) bool {
	if oldPrice <= 0 {
		return true
	}
	return math.Abs(newPrice-oldPrice)/oldPrice > sccRequoteTolerance
}

// sccMakerQuote is a market maker's two-sided quote on one listing: around its
// own V, skewed away from inventory it carries (long means cheaper on both sides,
// so it sells more than it buys). Sizes are a slice of the maker's stock budget.
// Pure.
func sccMakerQuote(inv sccInvestor, v float64, held, targetHeld int64, budget float64, band evedb.SCCBand) (bidPx, askPx float64, bidQty, askQty int64) {
	if v <= 0 {
		return
	}
	skew := 0.0
	if targetHeld > 0 {
		skew = sccMakerSkew * math.Max(-1, math.Min(1, float64(held-targetHeld)/float64(targetHeld)))
	}
	hs := inv.MakerHalfSpread
	rawBid, rawAsk := math.Floor(v*(1-hs-skew)), math.Ceil(v*(1+hs-skew))
	bidPx = sccClampBand(rawBid, band)
	askPx = sccClampBand(rawAsk, band)
	if askPx <= bidPx {
		askPx = bidPx + 1
	}
	q := int64(budget * sccMakerQuoteFrac / v)
	bidQty = max(q, 0)
	askQty = min(q, held)
	// A side the band would move against the maker is not quoted: a bid raised to
	// the floor pays more than value, an ask lowered to the ceiling sells for less.
	if rawBid < band.Lo {
		bidQty = 0
	}
	if rawAsk > band.Hi {
		askQty = 0
	}
	return
}

// sccPickMakers chooses the market makers: Trader-archetype pilots that are
// greedy enough, richest first, capped. wealth is keyed by charID. Pure.
func sccPickMakers(cands map[int64]float64, cap int) []int64 {
	ids := make([]int64, 0, len(cands))
	for id := range cands {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if cands[ids[i]] != cands[ids[j]] {
			return cands[ids[i]] > cands[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > cap {
		ids = ids[:cap]
	}
	return ids
}

// sccIsMakerMaterial is the trait gate on a market maker.
func sccIsMakerMaterial(archetypeKey string, traits map[string]int) bool {
	return archetypeKey == "trader" && traitScale(traits[TraitGreed]) > 0.4
}

// ── reacting to news ─────────────────────────────────────────────────────────

// sccHolderReaction is what one holder does about one material event.
type sccHolderReaction struct {
	SellQty  int64 // 0 = no sale
	AtMarket bool  // sell at the band edge, now, rather than at the bid
	Add      bool  // good news: buy a tranche more
}

// sccReactHolder decides one holder's reaction to an event of severity sev, given
// roll in 0..1 and its free holding. The hurried and careless react more often and
// harder. Bad news sells a share of the holding (all of it for a careless pilot
// and a grave event); good news makes momentum and speculator pilots add. Pure.
func sccReactHolder(inv sccInvestor, p sccTraitPoles, sev, roll float64, held int64) sccHolderReaction {
	var r sccHolderReaction
	prob := math.Abs(sev) * (0.4 + 0.6*p.Hurried + 0.4*p.Careless - 0.4*p.Careful)
	if roll >= math.Max(0, math.Min(1, prob)) {
		return r
	}
	if sev < 0 {
		if held <= 0 {
			return r
		}
		frac := math.Max(0.1, math.Min(1, math.Abs(sev)*(0.5+p.Careless)))
		r.SellQty = int64(math.Ceil(float64(held) * frac))
		r.AtMarket = p.Hurried > 0.3
		return r
	}
	r.Add = inv.Dominant == sccMomentum || inv.Dominant == sccSpeculator
	return r
}

// Stop kinds a pilot keeps on a holding.
const (
	sccStopNone = iota
	sccStopFixed
	sccStopTrail
)

// sccStopPlan is the stop a pilot keeps on one holding.
type sccStopPlan struct {
	Kind     int
	Pct      float64 // trail, or how far under cost a fixed stop sits
	AtMarket bool    // a stop at best; otherwise a stop-limit sccStopLimitSlack past the trigger
}

// sccStopPolicy is which pilots guard a position with a stop, and how.
// Momentum traders trail the price (their review-time exit, made instant),
// careful holders keep a fixed floor under cost, everyone else rides it out.
// Pure.
func sccStopPolicy(inv sccInvestor, p sccTraitPoles) sccStopPlan {
	switch {
	case inv.Dominant == sccMomentum:
		pct := bend(p.Unhurried-p.Hurried, sccStopTrailHurried, -sccMomentumStop, sccStopTrailUnhurried)
		return sccStopPlan{Kind: sccStopTrail, Pct: pct, AtMarket: p.Hurried > 0.3}
	case p.Careful > 0.3:
		return sccStopPlan{Kind: sccStopFixed, Pct: sccStopFixedFrac, AtMarket: false}
	}
	return sccStopPlan{}
}

// sccStopRequest turns a plan into a stop ticket for a holding of qty with an
// average cost, the last trade at last. ok is false when the plan wants no
// stop, or a fixed stop would already be crossed (the pilot is past its
// floor; a stop now would only sell at the bottom). Pure.
func sccStopRequest(plan sccStopPlan, corpID, owner, qty int64, cost, last float64) (evedb.SCCStopRequest, bool) {
	req := evedb.SCCStopRequest{CorpID: corpID, OwnerID: owner, Qty: qty, ExpiryDays: 30, Source: evedb.SCCSourceBot}
	if qty <= 0 || last <= 0 {
		return req, false
	}
	switch plan.Kind {
	case sccStopTrail:
		req.TrailPct = plan.Pct
		req.Trigger = evedb.SCCTrailTrigger(false, last, plan.Pct)
	case sccStopFixed:
		if cost <= 0 {
			return req, false
		}
		req.Trigger = math.Floor(cost * (1 - plan.Pct))
		if req.Trigger >= last || req.Trigger < evedb.SCCMinPrice {
			return req, false
		}
	default:
		return req, false
	}
	if !plan.AtMarket {
		req.Limit = math.Max(evedb.SCCMinPrice, math.Floor(req.Trigger*(1-sccStopLimitSlack)))
	}
	return req, true
}
