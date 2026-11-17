package rolebots

import (
	"context"
	"sort"
	"sync"
	"time"

	"gurimoon/rates"
)

// Siege kits: the corporation's hulls lent to a siege. A ready kit on any member's Armory shelf
// counts toward a side's siege capacity for a pilot with no combat hull of its own
// (siegeKitCapacity), and the siege fleet lends it across the alliance (withKitPools). When the
// side falls short of a target's quota and buying hulls for its hull-less pilots would close the
// gap, the corporations buy them on demand (provisionSiegeKits): a kit fitted for that pilot's
// skills, its parts on urgent requisitions (funded buy trips, not patient buy orders). The siege
// opens once they are on the shelf and the capacity re-check passes.

const (
	// siegeKitsMax caps the kits outstanding (planned, ready or lent) for sieges across a side.
	siegeKitsMax = 8
	// siegeKitCapitalShare is the most of a corporation's free capital siege kits may take in one pass.
	siegeKitCapitalShare = 0.25
	// siegeKitDeadlineSec is the deadline a siege kit's requisitions carry: under the three days
	// that make a line patient, so its parts go out as funded buy trips.
	siegeKitDeadlineSec = 12 * 3600
	// siegeKitAnswerMin is the least a pilot's odds of answering may be for a hull to be bought for it.
	siegeKitAnswerMin = 0.5
	// siegeBuyDerate takes a template fit's all-V DPS (ComputeFitStats) down toward what the pilot's
	// real skills put out.
	siegeBuyDerate = 0.75
)

// siegeKitCapacity fills each hull-less pilot's KitDPS from the ready combat kits on the given
// corporations' shelves: a kit bought for that pilot first, else any it can fly, each kit to one
// pilot, the likeliest to answer first. The DPS is the kit's real fit under the pilot's skills.
func (m *Mod) siegeKitCapacity(ctx context.Context, corps []int64, ps []siegePilot) {
	var kits []corpKit
	for _, c := range corps {
		if got, err := m.corpKits(ctx, c, "kind = ? AND status = ? AND shipItemID <> 0", kitArmory, kitReady); err == nil {
			for _, k := range got {
				if siegeHullClass(k.HullClass) {
					kits = append(kits, k)
				}
			}
		}
	}
	if len(kits) == 0 {
		return
	}
	used := map[int64]bool{}
	for i := range ps {
		p := &ps[i]
		if p.OwnedDPS > 0 || p.Answer <= 0 {
			continue
		}
		for _, mine := range []bool{true, false} {
			if p.KitDPS > 0 {
				break
			}
			for _, k := range kits {
				if used[k.KitID] || (k.ForCharID == p.CharID) != mine || (k.ForCharID != 0 && k.ForCharID != p.CharID) {
					continue
				}
				if !m.canFlyKit(ctx, p.CharID, k) {
					continue
				}
				l, err := m.loadCombatLoadout(ctx, p.CharID, k.ShipItemID, k.HullTypeID)
				if err != nil {
					continue
				}
				if dps := loadoutDPS(l); dps > 0 {
					p.KitDPS = dps
					used[k.KitID] = true
					break
				}
			}
		}
	}
}

// siegeKitTried is when each lead corporation last weighed buying siege kits: the weighing builds
// fits, so it runs once per siegeCapacityTTL, not on every opening pass.
var siegeKitTried sync.Map // corpID → time.Time

// siegeKitPlan is one hull a side means to buy for a pilot.
type siegeKitPlan struct {
	pilot  siegePilot
	buyer  *corpCEO
	target kitTarget
	fit    *Fit
	hull   *HullProfile
	value  float64
	dps    float64
	st     int64
}

// provisionSiegeKits buys hulls for a side's hull-less pilots when that closes the gap to a
// target's quota: up to siegeKitsMax outstanding, within siegeKitCapitalShare of each buying
// corporation's free capital, each kit within its doctrine's cost ceiling and fitted for its
// pilot's skills. It buys nothing when even every hull it could buy falls short. Reports how many
// kits it planned.
func (m *Mod) provisionSiegeKits(ctx context.Context, lead *corpCEO, ps []siegePilot, gap float64, target string) int {
	if gap <= 0 || len(ps) == 0 || lead == nil {
		return 0
	}
	if at, ok := siegeKitTried.Load(lead.CorpID); ok && time.Since(at.(time.Time)) < siegeCapacityTTL {
		return 0
	}
	siegeKitTried.Store(lead.CorpID, time.Now())
	corpSet := map[int64]bool{}
	for _, p := range ps {
		corpSet[p.CorpID] = true
	}
	corps := mapKeys(corpSet)
	kitted := map[int64]bool{}
	outstanding := 0
	if rows, err := m.db.QueryContext(ctx, `
		SELECT forCharID FROM rolebot_corp_kit
		 WHERE forCharID > 0 AND status IN (?, ?, ?) AND corporationID IN (`+int64CSV(corps)+`)`,
		kitPlanned, kitReady, kitIssued); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				kitted[id] = true
				outstanding++
			}
		}
		rows.Close()
	}
	room := siegeKitsMax - outstanding
	if room <= 0 {
		return 0
	}
	cands := make([]siegePilot, 0, len(ps))
	for _, p := range ps {
		if p.OwnedDPS <= 0 && p.KitDPS <= 0 && p.Answer >= siegeKitAnswerMin && !kitted[p.CharID] {
			cands = append(cands, p)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Answer > cands[j].Answer })

	ceos := map[int64]*corpCEO{}
	buyerFor := func(corpID int64) (*corpCEO, int64) {
		for _, c := range []int64{corpID, lead.CorpID} {
			ceo, ok := ceos[c]
			if !ok {
				ceo, _ = getCEO(ctx, m.db, c)
				ceos[c] = ceo
			}
			if ceo == nil {
				continue
			}
			if _, ok := m.corpDivFlag(ctx, c, purArmory); !ok {
				continue
			}
			if st, _ := m.kitStation(ctx, c, kitArmory); st != 0 {
				return ceo, st
			}
		}
		return nil, 0
	}
	now := clock.Unix()
	var plans []siegeKitPlan
	covered := 0.0
	for _, p := range cands {
		if len(plans) >= room || covered >= gap {
			break
		}
		buyer, st := buyerFor(p.CorpID)
		if buyer == nil {
			continue
		}
		sheets, err := m.skillSheetsFor(ctx, []int64{p.CharID})
		if err != nil {
			continue
		}
		tank, ceiling := TankNone, 0.0
		if have := m.fleetDoctrinesFor(ctx, buyer.CorpID); len(have) > 0 {
			tank, ceiling = have[0].Tank, have[0].CostCeiling
		}
		t := kitTarget{Kind: kitArmory, Spec: siegeMusterDoctrine(buyer.CorpID, tank, ceiling).specFor(""), Job: JobCombat,
			Want: 1, ForCharID: p.CharID, Deadline: now + siegeKitDeadlineSec}
		fit, hull, value, err := m.kitFit(ctx, raceOf(p.CharID), t, sheets[p.CharID], ceiling)
		if err != nil || fit == nil || hull == nil || !siegeHullClass(hull.Class) {
			continue
		}
		stats, err := m.ComputeFitStats(ctx, fit)
		if err != nil || stats == nil {
			continue
		}
		dps := (stats.TurretDPS + stats.LauncherDPS + stats.DroneDPS) * siegeBuyDerate * rates.PlayerDamage
		if dps <= 0 {
			continue
		}
		plans = append(plans, siegeKitPlan{pilot: p, buyer: buyer, target: t, fit: fit, hull: hull, value: value, dps: dps, st: st})
		covered += dps * min(p.Answer, 1)
	}
	if covered < gap {
		if len(plans) > 0 && now-m.lastCEOActionAt(ctx, lead.CorpID, "siegekit.short") >= 86400 {
			m.ceoLog(ctx, lead, powerWar, "siegekit.short", 0, gap,
				"wants %s: even %d hull(s) bought for its pilots would bring ~%.0f of the %.0f DPS it is short",
				target, len(plans), covered, gap)
		}
		return 0
	}
	spent := map[int64]float64{}
	planned := 0
	for _, pl := range plans {
		ch, err := m.ceoCharterFor(ctx, pl.buyer)
		if err != nil {
			continue
		}
		free, _, err := m.corpFreeCapital(ctx, pl.buyer.CorpID, ch)
		if err != nil || spent[pl.buyer.CorpID]+pl.value > free*siegeKitCapitalShare {
			continue
		}
		if err := m.planKit(ctx, pl.buyer, pl.target, pl.fit, pl.hull, pl.value, pl.st); err != nil {
			rlogf("rolebots: siege kit for %d: %v", pl.pilot.CharID, err)
			continue
		}
		spent[pl.buyer.CorpID] += pl.value
		planned++
	}
	if planned > 0 {
		m.ceoLog(ctx, lead, powerWar, "siegekit", 0, float64(planned),
			"buying %d hull(s) for pilots with none, to field the DPS %s takes", planned, target)
	}
	return planned
}
