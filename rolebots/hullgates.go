package rolebots

import (
	"context"
	"fmt"
	"strings"
)

// Hull gates: the rules that decide whether a cruiser, battlecruiser or battleship gets built at all.
//
// A pilot's first hull above a destroyer is out of reach while the only way in is the original:
// industryAffordMultiple times a 30-75M cruiser print, a 270M battlecruiser print or a billion-ISK
// battleship print, against a roster whose industrialists hold a few million. A copy on escrow is the
// way in (bpcopy.go), and these switches keep every gate between a copy and a job honest about it:
//
//   - copyAffords: the pre-draw wallet filter (dropUnaffordablePrints) keeps a candidate when a copy of
//     its print is on escrow in reach and the wallet covers that copy, not only when it owns the print.
//   - amort: a bought hull original is spread over industryBPOAmortProjectsHull projects, not
//     industryBPOAmortProjects: a battleship print charged a tenth of its price per batch can never pass
//     the sell-above-cost gate.
//   - thinFlood: a hull that has never sold in the region is a thin market, not a glut; one unsold unit
//     does not close it (historyFloodHull).
//
// Each is a server_settings key, default on, so a shard can put the old rule back.
const (
	hullGateCopyAffordsKey = "rolebots.hullGates.copyAffords"
	hullGateAmortKey       = "rolebots.hullGates.amort"
	hullGateThinFloodKey   = "rolebots.hullGates.thinFlood"
)

// industryBPOAmortProjectsHull is the working life of a bought hull original in projects. A hull batch is
// a handful of runs where an ammunition batch is hundreds, so a print outlives far more projects.
const industryBPOAmortProjectsHull = 40

// mktHullNoSaleListedMin is how many unsold units of a hull a region with no buying record must already
// show before the book calls it flooded (historyFloodHull). mktHullNoSaleWantedDiv scales that floor with
// the census: one listed unit per this many pilots who could fly it.
const (
	mktHullNoSaleListedMin = 3
	mktHullNoSaleWantedDiv = 20
)

func hullGateOn(ctx context.Context, m *Mod, key string) bool {
	return escrowSettingInt(ctx, m.db, key, 1) != 0
}

// copyableEscrowTypes is the blueprint types with at least one copy on escrow that this pilot could claim:
// in reach, not its own listing, runs left, and priced within budget. Pure.
func copyableEscrowTypes(listings []escrowBPO, reach map[int64]int, charID int64, budget float64) map[int64]bool {
	out := map[int64]bool{}
	for _, l := range listings {
		if !l.Copy || l.Runs <= 0 || l.IssuerID == charID || l.Price > budget {
			continue
		}
		if _, ok := reach[l.SystemID]; !ok {
			continue
		}
		out[l.TypeID] = true
	}
	return out
}

// bpoAmortProjects is how many projects a bought original of a product in this category is spread over.
// Pure.
func bpoAmortProjects(categoryID int64, hullOn bool) int {
	if hullOn && categoryID == catShip {
		return industryBPOAmortProjectsHull
	}
	return industryBPOAmortProjects
}

// bpoAmortProjectsFor reads the product's category and the setting.
func (m *Mod) bpoAmortProjectsFor(ctx context.Context, productTypeID int64) int {
	return bpoAmortProjects(m.typeCategoryOf(ctx, productTypeID), hullGateOn(ctx, m, hullGateAmortKey))
}

// historyFloodHull is historyFlood for a hull: a region that buys it is judged on cover as usual, but one
// with no buying record is flooded only once it already shows max(mktHullNoSaleListedMin,
// wanted/mktHullNoSaleWantedDiv) unsold units. A hull is a thin, lumpy market: the first cruiser listed in
// a region has not sold yet because there were none to buy. Pure.
func historyFloodHull(ready bool, perDay float64, listed, supply, wanted int64) (flooded, decided bool) {
	return historyFloodHullCover(ready, perDay, listed, supply, wanted, mktBuildCoverDays)
}

// historyFloodHullCover is historyFloodHull at a given cover in days (the cluster's record uses
// mktGlobalCoverDays). Pure.
func historyFloodHullCover(ready bool, perDay float64, listed, supply, wanted int64, coverDays float64) (flooded, decided bool) {
	switch {
	case !ready:
		return false, false
	case perDay > 0:
		return float64(supply) >= perDay*coverDays, true
	case listed > 0:
		floor := int64(mktHullNoSaleListedMin)
		if w := wanted / mktHullNoSaleWantedDiv; w > floor {
			floor = w
		}
		return listed >= floor, true
	}
	return false, false
}

// floodCover picks the flood rule: the thin-market hull rule for a hull above a frigate while
// hullGateThinFloodKey is on, else historyFloodCover. Pure.
func floodCover(thinHull, ready bool, perDay float64, listed, supply, wanted int64, coverDays float64) (flooded, decided bool) {
	if thinHull {
		return historyFloodHullCover(ready, perDay, listed, supply, wanted, coverDays)
	}
	return historyFloodCover(ready, perDay, listed, supply, coverDays)
}

// isThinHullGroup reports whether a ship group is a hull above a frigate that the thin-market rule covers:
// destroyer, cruiser, battlecruiser, battleship, industrial and transport. Pure.
func isThinHullGroup(groupID int64) bool {
	for _, g := range hullLadderGroupIDs {
		if g == groupID {
			return true
		}
	}
	return false
}

// hullPassOver collects why hull candidates were refused in a pass that went on to build something else, so
// the reason is not lost with the pass (industryStartProject logs only when every candidate fails).
type hullPassOver struct {
	reasons []string
	seen    map[int64]bool
}

func (h *hullPassOver) note(typeID int64, reason string) {
	if h.seen == nil {
		h.seen = map[int64]bool{}
	}
	if h.seen[typeID] || len(h.reasons) >= hullPassOverMax {
		return
	}
	h.seen[typeID] = true
	h.reasons = append(h.reasons, reason)
}

// hullPassOverMax is how many hull refusals one pass logs.
const hullPassOverMax = 3

// hullPassOverPrefix is the log line the industry choke panel buckets (espindustry_chokes.go).
const hullPassOverPrefix = "passed over hull(s): "

func (h *hullPassOver) line() string {
	if len(h.reasons) == 0 {
		return ""
	}
	return hullPassOverPrefix + strings.Join(h.reasons, "; ")
}

// droppedHullNote is the pre-draw filter's share of hullPassOver: the hulls it removed for want of wallet.
func droppedHullNote(c indCandidate, need float64) string {
	return fmt.Sprintf("%s: print needs ~%.0f ISK on hand and no copy is on escrow in reach", c.TypeName, need)
}
