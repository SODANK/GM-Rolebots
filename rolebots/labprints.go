package rolebots

// The housekeeping half of the research routine: what a builder does with the blueprints it
// already owns, before it decides on a new science venture or a manufacturing project.
//
// A blueprint is visible to the module only where the pilot's factory is: ownedCandidates
// (buildbook.go) and labPickPrint (labwork.go) both scope that way. Prints left in old factories,
// a seller's station or a hangar a corp move abandoned are invisible to their owner, so they are
// repatriated first. A research job is charged at the counter after the flight, so the fee is
// quoted before travelling (labFeeQuote, labAffordable) and a pilot's last ISK is never spent on
// a job that produces nothing to sell.

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
	serverram "gurimoon/server/ram"
)

const (
	// labStrayCheckEverySec throttles the stray-print scan, a query per pilot plus a reach walk.
	// LabCheckedAt starts at zero, so every pilot is due a scan on its first idle pass after a restart.
	labStrayCheckEverySec = 30 * 60

	// labStrayCandidates bounds the scan; a pilot holds a handful of strays.
	labStrayCandidates = 60

	// labWalletFloorISK is what a research job may never dip into. Research makes a later build
	// cheaper but sells nothing, so it is paid from surplus only; a pilot under the floor goes and earns.
	labWalletFloorISK = 100_000.0

	// labFeeCushion covers drift between the quote and the charge at the counter (the line's queue
	// moves, standing is re-read, the trip takes minutes). It is small because the floor does the work.
	labFeeCushion = 1.5
)

// labStray is one blueprint of this pilot's sitting in the wrong hangar.
type labStray struct {
	ItemID    int64
	TypeID    int64
	Name      string
	StationID int64
	SysID     int64
	Jumps     int
}

// labStrayPrint finds the blueprint most worth fetching: the pilot's own, in a station hangar that
// is not its factory, nearest the factory first (not nearest the pilot: the errand is factory,
// print, factory). It skips the live manufacturing project's print and the one already sent to a
// lab (flag 111), so it never fights a running errand.
func (m *Mod) labStrayPrint(ctx context.Context, b *botRun, factoryID, factorySys int64) (*labStray, error) {
	if factoryID == 0 {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, t.typeName, e.locationID, s.solarSystemID
		  FROM entity e
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN staStations s ON s.stationID = e.locationID
		 WHERE e.ownerID = ? AND e.flag = 4 AND e.locationID <> ?
		 LIMIT ?`, b.CharID, factoryID, labStrayCandidates)
	if err != nil {
		return nil, fmt.Errorf("stray prints: %w", err)
	}
	defer rows.Close()

	var pool []labStray
	for rows.Next() {
		var s labStray
		if err := rows.Scan(&s.ItemID, &s.TypeID, &s.Name, &s.StationID, &s.SysID); err != nil {
			return nil, err
		}
		if b.State != nil {
			if s.ItemID == b.State.Data.IndBPItemID || s.ItemID == b.State.Data.LabBPItemID {
				continue
			}
		}
		pool = append(pool, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		return nil, nil
	}

	origin := factorySys
	if origin == 0 {
		origin = b.SystemID
	}
	reach, err := m.reachFrom(ctx, origin, m.economicRules(ctx, b, defaultBand(b)))
	if err != nil {
		return nil, err
	}
	var best *labStray
	for i := range pool {
		s := &pool[i]
		j, ok := reach[s.SysID]
		if !ok {
			continue // out of this pilot's world; nothing to do about it here
		}
		s.Jumps = j
		// itemID breaks the tie so the pick is stable across passes; a pilot that changed its mind
		// between two equidistant hangars would never reach either.
		if best == nil || s.Jumps < best.Jumps ||
			(s.Jumps == best.Jumps && s.ItemID < best.ItemID) {
			best = s
		}
	}
	return best, nil
}

// labResearchSkillsMet reports which of a pool of blueprint types this pilot may research, per
// activity. GetActivitySkillReqs is per type, so it asks once for the whole pool and compares in
// Go (the shape researchEnrol uses). It matters most for Tech II originals, which need Metallurgy
// V and a racial engineering skill before anybody may research them.
func (m *Mod) labResearchSkillsMet(ctx context.Context, charID int64, bpTypeIDs []int64) (map[int64]map[int64]bool, error) {
	out := map[int64]map[int64]bool{
		ramActivityMEResearch: {},
		ramActivityPEResearch: {},
		ramActivityCopying:    {},
	}
	if len(bpTypeIDs) == 0 {
		return out, nil
	}
	levels, err := m.charSkillsFor(ctx, charID)
	if err != nil {
		return nil, err
	}
	// Everything starts allowed and a missing skill takes it away: a print with no requirement
	// rows is researchable by anyone, not "no evidence, so no".
	for _, id := range bpTypeIDs {
		out[ramActivityMEResearch][id] = true
		out[ramActivityPEResearch][id] = true
		out[ramActivityCopying][id] = true
	}

	q := `
		SELECT m.typeID, m.activity, m.requiredTypeID, m.quantity
		  FROM tl2materialsfortypewithactivity m
		  JOIN invTypes t ON t.typeID = m.requiredTypeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE g.categoryID = 16 AND m.activity IN (?, ?, ?) AND m.typeID IN (`
	args := []any{ramActivityPEResearch, ramActivityMEResearch, ramActivityCopying}
	for i, id := range bpTypeIDs {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, id)
	}
	q += ")"

	rows, err := m.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("research skill reqs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bpType, activity, skillID, level int64
		if err := rows.Scan(&bpType, &activity, &skillID, &level); err != nil {
			return nil, err
		}
		if int64(levels[skillID]) < level {
			if byType, ok := out[activity]; ok {
				byType[bpType] = false
			}
		}
	}
	return out, rows.Err()
}

// labFeeQuote prices one research job against the line it would sit on and returns the fee with
// the duration it was priced from. It repeats ramsvc's and installResearchJob's arithmetic exactly,
// because TryChargeISK asks for this number minutes later and an approximate quote lets the pilot
// fly to a refusal.
func (m *Mod) labFeeQuote(ctx context.Context, b *botRun, line *manufactureLine, cand *labCandidate) (fee, durationSec float64) {
	bp := &evedb.BlueprintTypeInfo{
		ResearchMaterialTime:     cand.BaseTime,
		ResearchProductivityTime: cand.BaseTime,
		ResearchCopyTime:         cand.BaseTime,
		MaxProductionLimit:       cand.MaxRuns,
	}
	if cand.ActivityID == ramActivityCopying {
		durationSec = copyDurationSec(bp, cand.Copies, cand.CopyRuns, cand.Skill, line)
	} else {
		durationSec = researchDurationSec(bp, cand.ActivityID, cand.Level, max(cand.Levels, 1), cand.Skill, line)
	}
	raw := line.Info.CostInstall + line.Info.CostPerHour*durationSec/3600
	standing := m.npcCorpStanding(ctx, b.CharID, line.Info.OwnerID)
	return serverram.AdjustCostForStanding(raw, standing,
		line.Info.DiscountPerGoodStanding, line.Info.SurchargePerBadStanding), durationSec
}

// labAffordable is the wallet gate as one expression. needsTrip adds the shuttle: a lab that is
// not the pilot's factory is a round trip and ensureRunabout refuses below runaboutKeepISK, so a
// pilot that can pay the fee but not the ride would strand its print at the lab.
func labAffordable(balance, fee float64, needsTrip bool) bool {
	need := fee*labFeeCushion + labWalletFloorISK
	if needsTrip {
		need += runaboutKeepISK
	}
	return balance >= need
}
