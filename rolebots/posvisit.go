package rolebots

import (
	"context"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// Life around the tower: what a player sees. There is no new mechanism for it. A bot whose
// abstract state is "in space at tower T" while a player has the system loaded is embodied there
// like at a belt or a gate. Towers give bots reasons to be in space: fuel and backhaul runs,
// rig and deploy legs, survey passes, siege musters, and shelter (parkAtTower).
// An embodied visitor flies to the tower's moon for real before its cargo moves
// (embodyTowerPrep). Only pilots the force field admits (evedb.ForcefieldAdmits) are routed
// inside it: the server sim does not push a hostile ball out of it (only the client draws the
// bounce), so a hostile's approach point is always outside.

// embodyTowerPrep flies an embodied visitor to a tower's moon before its
// business there happens. True while it is still on its way.
func (m *Mod) embodyTowerPrep(charID, moonID int64) bool {
	eb := liveProjection(charID)
	if eb == nil || moonID == 0 {
		return false
	}
	return m.flyToCelestial(eb, moonID)
}

// havenTower is the nearest tower among towers that is online and admits the actor: the pick of
// towerHaven, apart from the policy lookups. admits answers for one tower. Nil for none. Pure.
func havenTower(towers []evedb.StarbaseRecord, pos destiny.Vec3, admits func(evedb.StarbaseRecord) bool) *evedb.StarbaseRecord {
	var best *evedb.StarbaseRecord
	bestD := 0.0
	for i := range towers {
		t := &towers[i]
		if t.ControlTowerID != 0 || t.State != evedb.StarbaseStateOnline || !admits(*t) {
			continue
		}
		d := pos.Sub(destiny.Vec3{X: t.X, Y: t.Y, Z: t.Z}).Len()
		if best == nil || d < bestD {
			best, bestD = t, d
		}
	}
	return best
}

// towerHaven is where a 0.0 pilot with no station in its system shelters: the nearest online
// tower there whose force field admits its corporation or alliance (evedb.ForcefieldAdmits),
// whoever owns it. Nil when the system is not 0.0 or has no such tower.
func (m *Mod) towerHaven(corpID, systemID int64, pos destiny.Vec3) *evedb.CelestialEntity {
	if corpID == 0 || systemID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if m.systemSecurity(ctx, systemID) >= 0 {
		return nil
	}
	towers, err := evedb.ListStarbasesForSystem(ctx, m.db, systemID)
	if err != nil || len(towers) == 0 {
		return nil
	}
	actor := evedb.StarbaseActor{CorpID: corpID, AllianceID: m.corpAllianceID(ctx, corpID)}
	t := havenTower(towers, pos, func(rec evedb.StarbaseRecord) bool {
		pol, err := evedb.GetStarbasePolicy(ctx, m.db, rec)
		return err == nil && evedb.ForcefieldAdmits(pol, rec, actor)
	})
	if t == nil {
		return nil
	}
	return &evedb.CelestialEntity{ItemID: t.EntityID, TypeID: t.TypeID, GroupID: t.GroupID, CategoryID: t.CategoryID,
		X: t.X, Y: t.Y, Z: t.Z, Radius: t.Radius, Name: t.ItemName}
}
