package rolebots

import (
	"context"
	"database/sql"
	"sort"

	evedb "gurimoon/db"
)

// Defending a tower: what its guns shoot and what they shoot it with.
// On onlining, and on every desk pass after (so an operator's edit to a bot tower's Defense
// tab is put back), the sentry settings are: engage war targets, anyone who aggresses it,
// security status below -5 (outlaws) and owner standing at -5 or worse. Its own corporation
// and alliance are never engaged (db.SentryShouldEngage refuses that anyway).
// The guns burn ammunition (lasers keep their crystals): Infrastructure's requisition (Sales
// buys it) puts rounds on the Kit shelf and an ammo leg loads every gun to towerAmmoPerGun,
// or what its ammo bay holds (gunAmmoCap). The core fires only what is loaded.

const (
	towerAmmoPerGun     = 1000
	towerCrystalsPerGun = 2
	towerAmmoLowFrac    = 0.3
	defenseStatusLine   = -5.0
	defenseStandingLine = -5.0
)

// applyTowerDefense writes the bot doctrine onto a tower's Defense tab.
func (m *Mod) applyTowerDefense(ctx context.Context, c *corpCEO, p *towerPlan) {
	if p.TowerID == 0 {
		return
	}
	rec, ok, err := evedb.GetStarbase(ctx, m.db, p.TowerID)
	if err != nil || !ok {
		return
	}
	pol, err := evedb.GetStarbasePolicy(ctx, m.db, rec)
	if err != nil {
		return
	}
	status, standing := defenseStatusLine, defenseStandingLine
	want := evedb.SentrySettings{War: true, Aggression: true, Status: &status, Standing: &standing}
	cur := pol.Sentry
	if cur.War == want.War && cur.Aggression == want.Aggression &&
		cur.Status != nil && *cur.Status == status && cur.Standing != nil && *cur.Standing == standing {
		return
	}
	pol.Sentry = want
	if err := evedb.SaveStarbasePolicy(ctx, m.db, pol); err != nil {
		m.logBot(c.CharID, logError, "ceo", "defence settings for %s: %v", moonNameOf(ctx, m, p.MoonID), err)
		return
	}
	m.ceoLog(ctx, c, powerStarbase, "defense", p.TowerID, 0,
		"set the guns at %s to engage war targets, aggressors, outlaws and anyone at −5 or worse", moonNameOf(ctx, m, p.MoonID))
}

// towerGunsOf lists a tower's anchored batteries as itemID to typeID.
func (m *Mod) towerGunsOf(ctx context.Context, p *towerPlan) map[int64]int64 {
	out := map[int64]int64{}
	for _, pc := range p.Layout.Pieces {
		if pc.ItemID != 0 && gunPower[pc.TypeID] > 0 {
			out[pc.ItemID] = pc.TypeID
		}
	}
	return out
}

// sentryChargeFor is the round a battery fires: the cheapest NPC-stocked
// charge of its group and size. Lasers take crystals (kept, not spent).
func (m *Mod) sentryChargeFor(ctx context.Context, gunTypeID int64) (chargeTypeID int64, crystals bool) {
	var group, size sql.NullFloat64
	_ = m.db.QueryRowContext(ctx, `
		SELECT (SELECT CAST(COALESCE(valueInt, valueFloat) AS SIGNED) FROM dgmTypeAttributes WHERE typeID = ? AND attributeID = 604),
		       (SELECT CAST(COALESCE(valueInt, valueFloat) AS SIGNED) FROM dgmTypeAttributes WHERE typeID = ? AND attributeID = 128)`,
		gunTypeID, gunTypeID).Scan(&group, &size)
	if !group.Valid {
		return 0, false
	}
	_ = m.db.QueryRowContext(ctx, `
		SELECT t.typeID FROM invTypes t
		  JOIN dgmTypeAttributes s ON s.typeID = t.typeID AND s.attributeID = 128
		 WHERE t.groupID = ? AND t.published = 1 AND COALESCE(s.valueInt, s.valueFloat) = ?
		   AND EXISTS (SELECT 1 FROM crpnpccorporationtrades c WHERE c.typeID = t.typeID AND c.supplyDemand > 0)
		 ORDER BY t.basePrice, t.typeID LIMIT 1`, int64(group.Float64), size.Float64).Scan(&chargeTypeID)
	return chargeTypeID, int64(group.Float64) == 86
}

// posDefenseDesk keeps every running tower's guns set and loaded.
func (m *Mod) posDefenseDesk(ctx context.Context, c *corpCEO, ch Charter, plans []*towerPlan) {
	for _, p := range plans {
		if p.Status != towerRunning || p.TowerID == 0 {
			continue
		}
		m.applyTowerDefense(ctx, c, p)
		guns := m.towerGunsOf(ctx, p)
		if len(guns) == 0 || p.StagingStationID == 0 {
			continue
		}
		office := m.officeAt(ctx, c.CorpID, p.StagingStationID)
		if office == 0 {
			continue
		}
		shelf := m.divisionHeld(ctx, c.CorpID, office, corpDivStarbase)
		short := map[int64]int64{} // charge → rounds wanted across the tower
		for gun, gunType := range guns {
			charge, crystals := m.sentryChargeFor(ctx, gunType)
			if charge == 0 {
				continue
			}
			want := min(int64(towerAmmoPerGun), m.gunAmmoCap(ctx, gunType, charge))
			if crystals {
				want = min(int64(towerCrystalsPerGun), m.gunAmmoCap(ctx, gunType, charge))
			}
			if want <= 0 {
				continue
			}
			var loaded sql.NullInt64
			_ = m.db.QueryRowContext(ctx, `
				SELECT SUM(quantity) FROM entity WHERE locationID = ? AND flag = ?`, gun, evedb.FlagSentryAmmo).Scan(&loaded)
			if float64(loaded.Int64) < float64(want)*towerAmmoLowFrac || (crystals && loaded.Int64 < want) {
				short[charge] += want - loaded.Int64
			}
		}
		if len(short) == 0 {
			continue
		}
		var items []haulItem
		for charge, q := range short {
			if have := shelf[charge]; have > 0 {
				items = append(items, haulItem{TypeID: charge, Qty: min(q, have), Flag: corpDivStarbase})
			}
			if need := q - shelf[charge] - m.reqPending(ctx, c.CorpID, charge, p.StagingStationID, corpDivStarbase); need > 0 {
				_, _ = m.postRequisition(ctx, c, requisition{TypeID: charge, Qty: need, DestStationID: p.StagingStationID,
					DestFlag: corpDivStarbase, Requester: posRequester(ctx, "security"), RefKind: "tower", RefID: p.PlanID,
					Deadline: clock.Unix() + 4*86400, Note: "ammunition for the tower guns"})
			}
		}
		if len(items) == 0 || m.openHaulFor(ctx, p.PlanID, haulKindAmmo) != nil {
			continue
		}
		sort.Slice(items, func(i, j int) bool { return items[i].TypeID < items[j].TypeID })
		_, _ = m.postHaul(ctx, c, haulLeg{Kind: haulKindAmmo, PlanID: p.PlanID, FromKind: haulFromOffice,
			FromID: office, FromStationID: p.StagingStationID, FromFlag: corpDivStarbase, ToKind: haulToTower,
			ToID: p.TowerID, ToSystemID: p.SystemID, Items: items, PostedBy: posRequester(ctx, "security"),
			Note: "load the guns at " + moonNameOf(ctx, m, p.MoonID)})
	}
}

// gunAmmoCap is how many of a charge a battery's ammo bay holds (invTypes.capacity divided
// by the charge's volume); towerAmmoPerGun when either is unknown.
func (m *Mod) gunAmmoCap(ctx context.Context, gunType, charge int64) int64 {
	var capM3, vol sql.NullFloat64
	_ = m.db.QueryRowContext(ctx, `
		SELECT (SELECT capacity FROM invTypes WHERE typeID = ?), (SELECT volume FROM invTypes WHERE typeID = ?)`,
		gunType, charge).Scan(&capM3, &vol)
	if !capM3.Valid || capM3.Float64 <= 0 || !vol.Valid || vol.Float64 <= 0 {
		return towerAmmoPerGun
	}
	return int64(capM3.Float64 / vol.Float64)
}

// loadTowerGuns puts the ammunition a carrier brought into each gun that
// fires it, as evenly as the hold allows and no gun past its bay. A charge the
// core would refuse the gun (db.ValidateChargeForModule, the pilot path's
// check) is not loaded into it.
func (m *Mod) loadTowerGuns(ctx context.Context, b *botRun, l *haulLeg, p *towerPlan) (int64, error) {
	guns := m.towerGunsOf(ctx, p)
	byCharge := map[int64][]int64{}
	for gun, gunType := range guns {
		charge, _ := m.sentryChargeFor(ctx, gunType)
		if charge == 0 || evedb.ValidateChargeForModule(ctx, m.db, charge, gunType) != nil {
			continue
		}
		byCharge[charge] = append(byCharge[charge], gun)
	}
	loaded := int64(0)
	for i := range l.Carrying {
		c := &l.Carrying[i]
		targets := byCharge[c.TypeID]
		sort.Slice(targets, func(a, b int) bool { return targets[a] < targets[b] })
		for gi, gun := range targets {
			if c.Qty <= 0 {
				break
			}
			share := c.Qty / int64(len(targets)-gi)
			if share <= 0 {
				share = c.Qty
			}
			var have sql.NullInt64
			_ = m.db.QueryRowContext(ctx, `
				SELECT SUM(quantity) FROM entity WHERE locationID = ? AND flag = ?`, gun, evedb.FlagSentryAmmo).Scan(&have)
			share = min(share, m.gunAmmoCap(ctx, guns[gun], c.TypeID)-have.Int64)
			if share <= 0 {
				continue
			}
			var itemID int64
			_ = m.db.QueryRowContext(ctx, `
				SELECT itemID FROM entity WHERE locationID = ? AND flag = 5 AND ownerID = ? AND typeID = ? ORDER BY itemID LIMIT 1`,
				b.ShipID, b.CorpID, c.TypeID).Scan(&itemID)
			if itemID == 0 {
				c.Qty = 0
				break
			}
			if _, err := evedb.AddToStarbaseBay(ctx, m.db, gun, itemID, share, evedb.FlagSentryAmmo, b.CorpID, false); err != nil {
				continue
			}
			c.Qty -= share
			loaded += share
		}
		if c.Qty > 0 && len(targets) > 0 {
			// Whatever no gun took stays in the hold and rides home.
			continue
		}
	}
	return loaded, nil
}
