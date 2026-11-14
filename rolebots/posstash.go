package rolebots

import (
	"context"
	"sort"
)

// The kit stash: structures a corporation already owns, used before any are bought. A teardown
// or a refit (posreconsider.go) carries what it takes down into an office's Starbase Kit division.
// pullStash finds a spare in any of the corporation's offices within towerStashJumps of the
// staging office and posts a move leg (office to staging office, Starbase Kit) for it, which covers
// that much of the shortfall. Units already promised to an open move leg are not offered again, so
// two plans never count the same structure.

// stashSource is one office holding spare kit of a type.
type stashSource struct {
	StationID, OfficeID int64
	Jumps               int
	Free                int64 // held, less what open move legs already take
}

// stashTakes splits a shortfall over the sources, nearest first. Pure.
func stashTakes(need int64, srcs []stashSource) map[int64]int64 {
	out := map[int64]int64{}
	s := append([]stashSource(nil), srcs...)
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Jumps != s[j].Jumps {
			return s[i].Jumps < s[j].Jumps
		}
		return s[i].StationID < s[j].StationID
	})
	for _, src := range s {
		if need <= 0 {
			break
		}
		take := min(need, src.Free)
		if take <= 0 {
			continue
		}
		out[src.StationID] = take
		need -= take
	}
	return out
}

// pullStash covers up to short units of a structure type for a plan from the
// corporation's other offices: what is already moving to the staging office
// for this plan, plus what it now sends. Returns the units covered.
func (m *Mod) pullStash(ctx context.Context, c *corpCEO, p *towerPlan, typeID, short int64) int64 {
	if short <= 0 || p.StagingStationID == 0 {
		return 0
	}
	inFlight := m.stashInFlight(ctx, c.CorpID, p.PlanID, p.StagingStationID, typeID)
	need := short - inFlight
	if need <= 0 {
		return short
	}
	srcs := m.stashSources(ctx, c.CorpID, p.StagingStationID, typeID)
	if len(srcs) == 0 {
		return inFlight
	}
	byStation := map[int64]stashSource{}
	for _, s := range srcs {
		byStation[s.StationID] = s
	}
	sent := int64(0)
	for st, take := range stashTakes(need, srcs) {
		src := byStation[st]
		if _, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindMove, PlanID: p.PlanID, FromKind: haulFromOffice,
			FromID: src.OfficeID, FromStationID: st, FromFlag: corpDivStarbase, ToKind: haulToOffice,
			ToStationID: p.StagingStationID, ToFlag: corpDivStarbase,
			Items:    []haulItem{{TypeID: typeID, Qty: take, Flag: corpDivStarbase}},
			PostedBy: "ceo", Deadline: clock.Unix() + 4*86400,
			Note:     "spare kit from another office for " + moonNameOf(ctx, m, p.MoonID)}); err != nil {
			continue
		}
		sent += take
		m.ceoLog(ctx, c, powerStarbase, "stash", typeID, 0, "is moving %d spare %s from %s rather than buying",
			take, m.typeNameOr(ctx, typeID), m.stationLabel(ctx, st))
	}
	return inFlight + sent
}

// stashInFlight is the units of a type already on open move legs to the
// staging office for this plan.
func (m *Mod) stashInFlight(ctx context.Context, corpID, planID, stationID, typeID int64) int64 {
	legs, err := m.queryHauls(ctx, `WHERE corporationID = ? AND kind = ? AND planID = ? AND toStationID = ?
		   AND status IN ('open','claimed','contract')`, corpID, haulKindMove, planID, stationID)
	if err != nil {
		return 0
	}
	n := int64(0)
	for _, l := range legs {
		for _, it := range l.Items {
			if it.TypeID == typeID {
				n += it.Qty
			}
		}
		for _, cr := range l.Carrying {
			if cr.TypeID == typeID {
				n += cr.Qty
			}
		}
	}
	return n
}

// stashSources is every other office within towerStashJumps of the staging
// station holding the type in its Starbase Kit, less what open move legs out
// of it already take.
func (m *Mod) stashSources(ctx context.Context, corpID, stagingStation, typeID int64) []stashSource {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return nil
	}
	near := bfsJumps(u, m.stationSystemID(ctx, stagingStation), towerStashJumps)
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, o.itemID, s.solarSystemID FROM crpoffices o
		  JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ? AND o.stationID <> ?`, corpID, stagingStation)
	if err != nil {
		return nil
	}
	type off struct{ st, office, sys int64 }
	var offices []off
	for rows.Next() {
		var o off
		if rows.Scan(&o.st, &o.office, &o.sys) == nil {
			offices = append(offices, o)
		}
	}
	rows.Close()
	var out []stashSource
	for _, o := range offices {
		j, ok := near[o.sys]
		if !ok {
			continue
		}
		held := m.divisionHeld(ctx, corpID, o.office, corpDivStarbase)[typeID]
		if held <= 0 {
			continue
		}
		free := held - m.stashPromised(ctx, corpID, o.st, typeID)
		if free > 0 {
			out = append(out, stashSource{StationID: o.st, OfficeID: o.office, Jumps: j, Free: free})
		}
	}
	return out
}

// stashPromised is what open move legs out of an office's Starbase Kit
// already take of a type.
func (m *Mod) stashPromised(ctx context.Context, corpID, stationID, typeID int64) int64 {
	legs, err := m.queryHauls(ctx, `WHERE corporationID = ? AND kind = ? AND fromStationID = ? AND fromFlag = ?
		   AND status IN ('open','claimed','contract')`, corpID, haulKindMove, stationID, corpDivStarbase)
	if err != nil {
		return 0
	}
	n := int64(0)
	for _, l := range legs {
		for _, it := range l.Items {
			if it.TypeID == typeID {
				n += it.Qty
			}
		}
	}
	return n
}
