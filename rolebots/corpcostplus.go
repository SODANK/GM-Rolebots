package rolebots

import (
	"context"
	"database/sql"

	evedb "gurimoon/db"
)

// Cost-plus valuation for big hulls the board builds for itself. A board row is
// valued off the trade tape, else the book, else basePrice (planRowAt ->
// pickTradeValue). No battleship has traded on this shard and hull basePrices
// are the old catalogue while minerals trade at two to three times theirs, so
// such a row fails Margin() > 0 and is never posted. For a hull the corporation
// builds for itself (a replacement owed, the store shelf, an Armory kit, a
// department requisition) that is the wrong question: it will own the ship, so
// the row is valued at cost plus the charter's store markup, what the store
// lists it at (corpstore.go never undercuts cost).
//
// Market rows take it too by default (rolebots.costPlusHulls = all): cruisers and up have so little tape
// that a market row valued off it never posts, and the roster has wanted them for months. Where the tape
// does pay for the hull, applyCostPlus leaves the row alone. "internal" keeps market rows on the tape;
// "off" switches it off.

const (
	costPlusScopeKey      = "rolebots.costPlusHulls"
	costPlusScopeInternal = "internal"
	costPlusScopeAll      = "all"
	costPlusScopeOff      = "off"
)

// costPlusHullGroups is the hull groups cost-plus applies to: Destroyer, Cruiser,
// Battlecruiser, Battleship, Freighter, Dreadnought, Carrier.
var costPlusHullGroups = map[int64]bool{420: true, 26: true, 419: true, 27: true, 513: true, 485: true, 547: true}

// costPlusScopeDefault is the scope when the setting is unset or unrecognised.
const costPlusScopeDefault = costPlusScopeAll

// costPlusScope reads the setting; anything unrecognised is the default.
func costPlusScope(ctx context.Context, db *sql.DB) string {
	if v, ok, _ := evedb.GetServerSetting(ctx, db, costPlusScopeKey); ok {
		switch v {
		case costPlusScopeAll, costPlusScopeOff, costPlusScopeInternal:
			return v
		}
	}
	return costPlusScopeDefault
}

// costPlusRow is the value a run must be booked at to clear its bill, wage and
// the ore desk's fee by markup. The wage is a share of the value (wageForRun):
// value = (mats + value*wageFrac) * (1 + markup) / (1 - fee). Answers (value,
// wage); a degenerate charter answers (0, 0). Pure.
func costPlusRow(mats, wageFrac, markup, fee float64) (float64, float64) {
	if mats <= 0 || markup < 0 || fee < 0 || fee >= 1 {
		return 0, 0
	}
	den := (1 - fee) - wageFrac*(1+markup)
	if den <= 0.05 {
		return 0, 0
	}
	value := mats * (1 + markup) / den
	return value, wageForRun(value, wageFrac)
}

// costPlusApplies reports whether a row of this purpose takes cost-plus under
// the scope: internal rows always (unless off), market rows only under "all",
// the roster's demand ledger never (a want is a reading, not an order). Pure.
func costPlusApplies(scope, purpose string) bool {
	switch scope {
	case costPlusScopeOff:
		return false
	case costPlusScopeAll:
		return purpose != corpDemandPurpose
	}
	return purpose != "market" && purpose != corpDemandPurpose
}

// applyCostPlus re-values a big-hull row at cost plus the charter's store
// markup when the tape and the book together value it under its own bill.
// Reports whether it did; the row carries CostPlus so the board's note says so.
func (m *Mod) applyCostPlus(ctx context.Context, ch Charter, r *buildPlanRow, group int64, scope string) bool {
	if r == nil || !costPlusHullGroups[group] || !costPlusApplies(scope, r.Purpose) {
		return false
	}
	if r.ValuePerRun*(1-oreDeskFeeFrac) > r.MatsPerRun+r.WagePerRun {
		return false // the market already pays for it
	}
	value, wage := costPlusRow(r.MatsPerRun, ch.WageFrac, ch.StoreMarkup, oreDeskFeeFrac)
	if value <= 0 {
		return false
	}
	r.ValuePerRun, r.WagePerRun, r.CostPlus = value, wage, true
	return true
}

// hullHasTape reports whether the product has traded anywhere in the tape
// window: the one case a cost-plus row should defer to the market.
func (m *Mod) hullHasTape(ctx context.Context, typeID int64) bool {
	refs := m.tradeRefsOf(ctx, typeID)
	return refs.Cluster.Units > 0
}
