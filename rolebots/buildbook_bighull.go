package rolebots

import (
	"context"
	"database/sql"
)

// Battlecruisers and battleships in the solo build book.
// They get the mining campaign budget (industryFeedableRuns) and the thin-market urgency
// boost only where the pilot can get a print: an escrow copy of the type's blueprint in
// reach of its factory, or an owned print. A battleship original costs more than the
// wallet gate (industryAffordMultiple) allows, so a copy is the way in. The bill gate still
// applies, so battleships are mostly built by corporations (corpfund.go).

// bigHullGroupIDs is Battlecruiser (419) and Battleship (27).
var bigHullGroupIDs = []int64{419, 27}

// bigHullBookKey switches the big hulls' campaign budget and urgency boost
// (server_settings, default on).
const bigHullBookKey = "rolebots.hullBook.bigHulls"

// bigHullBookOn reports whether battlecruisers and battleships take the
// campaign budget and the urgency boost.
func bigHullBookOn(ctx context.Context, db *sql.DB) bool {
	return escrowSettingInt(ctx, db, bigHullBookKey, 1) != 0
}

// campaignHullGroups is campaignHullGroupIDs, plus the big hulls while the
// setting is on. Pure over its inputs, for the test.
func campaignHullGroups(bigHulls bool) []int64 {
	out := append([]int64(nil), campaignHullGroupIDs...)
	if bigHulls {
		out = append(out, bigHullGroupIDs...)
	}
	return out
}

// campaignHullGroupsFor reads the setting and answers the groups the mining
// campaign budget covers.
func (m *Mod) campaignHullGroupsFor(ctx context.Context) []int64 {
	return campaignHullGroups(bigHullBookOn(ctx, m.db))
}

// bigHullPrintsNear returns the big-hull candidates the pilot can get a print for: an
// escrow copy listed within reach of its factory (roster snapshot, no query) or an owned
// print (evidenceOwned). Only those take the urgency boost.
func (m *Mod) bigHullPrintsNear(ctx context.Context, cands []indCandidate, big map[int64]bool,
	reach map[int64]int) map[int64]bool {
	out := map[int64]bool{}
	if len(big) == 0 {
		return out
	}
	bpOf := map[int64]int64{} // bp type → product type, big hulls only
	for _, c := range cands {
		if big[c.TypeID] {
			if c.Evidence == evidenceOwned {
				out[c.TypeID] = true
			}
			if c.BPTypeID != 0 {
				bpOf[c.BPTypeID] = c.TypeID
			}
		}
	}
	if len(bpOf) == 0 {
		return out
	}
	ls, err := m.escrowBPOListings(ctx)
	if err != nil {
		return out
	}
	for _, l := range ls {
		if !l.Copy || l.Runs <= 0 {
			continue
		}
		t, ok := bpOf[l.TypeID]
		if !ok {
			continue
		}
		if _, near := reach[l.SystemID]; near {
			out[t] = true
		}
	}
	return out
}
