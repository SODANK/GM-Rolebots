package rolebots

// Bot escrow buying: integrated into existing procurement.
//
// Entry point: checkEscrowForBPO, called from industry BPO purchasing. Before buying a BPO from the market it
// checks whether this station's escrow board has a blueprint (usually a researcher's copy) worth taking
// instead. Ships are bought through escrow_ship.go. Both call db.ClaimEscrowMission, the same one-step TX
// players use.

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// ── BPO buying ──────────────────────────────────────────────────────────────

// checkEscrowForBPO looks for a blueprint of bpTypeID on escrow at the given station, a copy or an original,
// judged by the rules the planner's hunt uses (bestEscrowBPO) so the two agree on a print's worth. Returns
// the escrow listing's ID if one here is worth buying, or 0.
func (m *Mod) checkEscrowForBPO(ctx context.Context, b *botRun,
	bpTypeID int64, stationID int64) int64 {

	if !escrowBuyingEnabled(ctx, m.db) {
		return 0
	}
	sysID, err := evedb.GetSolarSystemForStation(ctx, m.db, stationID)
	if err != nil || sysID == 0 {
		return 0
	}
	want := 1
	if b.State != nil && b.State.Data.IndRuns > 0 {
		want = b.State.Data.IndRuns
	}
	es := m.bestEscrowBPO(ctx, b, bpTypeID, map[int64]int{sysID: 0}, nil, want)
	if es == nil || es.StationID != stationID {
		return 0
	}
	// A corporation purchase order takes a copy only when it says it may (a shipyard's hull order,
	// postLibraryPurchase). Every other order buys the library an original, which research needs.
	if es.Copy && b.State != nil && b.State.Data.IndCorpPO != 0 {
		if po, _ := m.openPOFor(ctx, b.CharID); po == nil || !po.AllowCopy {
			return 0
		}
	}
	return es.EscrowID
}

// claimEscrowListing claims an escrow listing for the bot.
func (m *Mod) claimEscrowListing(ctx context.Context, b *botRun, escrowID int64) error {
	corpID, _ := evedb.GetCharCorpID(ctx, m.db, b.CharID)
	if _, err := evedb.ClaimEscrowMission(ctx, m.db,
		escrowID, b.CharID, corpID, b.StationID, true); err != nil {
		return fmt.Errorf("claim escrow %d: %w", escrowID, err)
	}
	m.logBot(b.CharID, logInfo, "escrow",
		"claimed escrow listing %d at %s", escrowID, m.stationLabel(ctx, b.StationID))
	return nil
}

// ── step function ───────────────────────────────────────────────────────────

// escrowBuyStep advances an escrow buy task. Like escrowListStep, the actual
// claim happens at the entry point; this is the dwell.
func (m *Mod) escrowBuyStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return false, saveState(ctx, m.db, st)
}
