package rolebots

import (
	"context"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// A research house's scientists work for the corporation. A member of a bot-run corporation with an R&D arm
// does not list its blueprint copies on escrow itself: it hands them to the library at a corporation office,
// is paid a copy wage for each, and the Sales arm sells them for the corporation (corpprintsale.go). Before
// this, 570M ISK of copies a week went into members' wallets while the corporation's research lost money.
// Pilots in NPC corporations, and in corporations with no R&D arm, keep selling their own copies
// (listLibraryProducts).

// researchHouse reports whether a corporation keeps its members' research: a bot-run corporation with at
// least one member seated in its R&D arm.
func (m *Mod) researchHouse(ctx context.Context, corpID int64) bool {
	if corpID == 0 {
		return false
	}
	if _, ok := managedCEO(corpID); !ok {
		return false
	}
	return armCount(corpID, armResearch) > 0
}

// researchMember reports whether the pilot's copies are research for its house: a research career, or a
// seat in the corporation's R&D arm.
func (m *Mod) researchMember(b *botRun) bool {
	if researchCareer(b.Tune) {
		return true
	}
	s, ok := armOf(b.CharID, b.CorpID)
	return ok && s.Arm == armResearch
}

// ownOriginalTypes is the set of blueprint types the pilot holds an original of, anywhere.
func (m *Mod) ownOriginalTypes(ctx context.Context, charID int64) map[int64]bool {
	out := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT e.typeID FROM entity e
		  JOIN invBlueprints bp ON bp.blueprintID = e.itemID AND bp.copy = 0
		 WHERE e.ownerID = ?`, charID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t int64
		if rows.Scan(&t) == nil {
			out[t] = true
		}
	}
	return out
}

// researchDepositVerdict says what a research house's member does with the copies in its hangar here:
// deposit them at an office in this station, hold them for the office it works at (within reach), or
// sell them itself because no office is near. Pure.
type researchDepositVerdict int

const (
	researchSellOwn researchDepositVerdict = iota
	researchDeposit
	researchHold
)

func researchDepositFor(house bool, officeHere int64, workStationJumps int) researchDepositVerdict {
	switch {
	case !house:
		return researchSellOwn
	case officeHere != 0:
		return researchDeposit
	case workStationJumps >= 0 && workStationJumps <= corpWorkMaxJumps:
		return researchHold
	}
	return researchSellOwn
}

// researchCopyWage is what the corporation pays a member for one copy put on its shelf: the copy-order
// wage (corpcopy.go) on the copy's own price. Pure.
func researchCopyWage(copyPrice float64) float64 {
	return math.Max(corpCopyWageMin, math.Floor(corpCopyWageFrac*copyPrice))
}

// depositResearchProducts runs the hand-over for a member docked somewhere. It reports whether the
// pilot's copies are the corporation's business (deposited now or held for an office), in which case the
// caller must not list them on the pilot's own account.
func (m *Mod) depositResearchProducts(ctx context.Context, b *botRun) bool {
	if b.StationID == 0 || !m.researchHouse(ctx, b.CorpID) || !m.researchMember(b) {
		return false
	}
	officeID, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, b.CorpID, b.StationID)
	if officeID == b.StationID {
		officeID = 0
	}
	jumps := -1
	if officeID == 0 {
		if st, _ := m.corpWorkStation(ctx, b); st != 0 {
			if sysID, err := evedb.GetSolarSystemForStation(ctx, m.db, st); err == nil {
				jumps = m.jumpsTo(ctx, b, sysID)
			}
		}
	}
	switch researchDepositFor(true, officeID, jumps) {
	case researchHold:
		return true
	case researchSellOwn:
		return false
	}

	held, err := m.hangarCopies(ctx, b.CharID, b.StationID)
	if err != nil || len(held) == 0 {
		return true
	}
	inUse := map[int64]bool{}
	if b.State != nil {
		d := b.State.Data
		inUse[d.IndBPItemID], inUse[d.IndFetchItemID], inUse[d.LabBPItemID] = true, true, true
	}
	// Only copies of the pilot's own originals are research products. A copy bought on escrow to build
	// from stays with the builder.
	own := m.ownOriginalTypes(ctx, b.CharID)
	var copies int
	var wage float64
	for _, c := range held {
		if inUse[c.ItemID] || c.Runs <= 0 || !own[c.TypeID] {
			continue
		}
		res, err := m.db.ExecContext(ctx, `
			UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
			 WHERE itemID = ? AND ownerID = ? AND locationID = ? AND flag = 4`,
			b.CorpID, officeID, corpDivLibrary, c.ItemID, b.CharID, b.StationID)
		if err != nil {
			m.logBot(b.CharID, logError, "lab", "could not hand a copy to its corporation: %v", err)
			break
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		copies++
		wage += researchCopyWage(m.corpCopyPrice(ctx, b, c.TypeID, c.Runs, c.ME))
	}
	if copies == 0 {
		return true
	}
	why := fmt.Sprintf("%d research copies for the library", copies)
	m.payCorpWage(ctx, b.CorpID, b.CharID, wage, why)
	m.logBot(b.CharID, logInfo, "lab", "handed %d blueprint copies to the corporation library (%s in wages)",
		copies, iskString(wage))
	if c, _ := getCEO(ctx, m.db, b.CorpID); c != nil {
		m.ceoLog(ctx, c, powerProduction, "researchdeposit", b.CharID, wage,
			"%s put %d blueprint copies on the library shelf", m.charNameOr(ctx, b.CharID), copies)
	}
	return true
}
