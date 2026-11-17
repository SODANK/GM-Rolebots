package rolebots

import (
	"context"
	"fmt"
	"sort"

	evedb "gurimoon/db"
)

// Reconsidering a tower: does it still serve the corporation? reviewTowers judges a tower by
// its money; this judges it by its point. Every reconsiderEverySec, and at once for a depot,
// the CEO asks whether the corporation still has a case for what the tower is (purposeSignals,
// the tower itself left out). If not, it picks the strongest case this moon and tower can carry
// and refits in place: structures the new purpose does not want are stripped (emptied,
// unanchored, carried to the staging office's Starbase Kit) and the ones it lacks are ordered
// like an upgrade (refitTower, rigMissing). Only when no case fits is the tower taken down
// (startTeardown); a depot still being bought is abandoned with its kit left on the shelf.
// Mining, reactions and fortress towers are not reconsidered here: reviewTowers judges mining by
// what its moon pays, a reactor by its feeders, a fortress by its sovereignty (fortressSystems).
// The one exception is a tower of any purpose whose work structures cannot anchor where it stands
// (towerWorkBarred): it is judged at once, its own purpose struck off, before it is ever finished.

const (
	reconsiderEverySec = 14 * 86400
	reconsiderWhy      = "refit"
)

// reconsiderable is a purpose this pass judges.
func reconsiderable(purpose string) bool {
	switch purpose {
	case purposeDepot, purposeRefinery, purposeLab, purposeFactory:
		return true
	}
	return false
}

// refitChoice is the verdict on one tower: keep it, refit it to another
// purpose, or (neither) take it down. Pure.
//
// carries says whether this tower can hold a purpose's core structure.
func refitChoice(cur string, sigs []purposeSignal, tags []string, moonClass int, carries func(string) bool) (keep bool, to string) {
	if cur != purposeDepot && signalsArgue(sigs, cur) {
		return true, ""
	}
	for _, s := range rankPurposes(sigs, tags, 1, 1) {
		switch s.Purpose {
		case purposeMining, purposeReactions, purposeDepot, cur:
			continue // a refit cannot make a moon tower or a reactor, and staying is not a refit
		}
		if !moonFitsPurpose(moonClass, s.Purpose) || !carries(s.Purpose) {
			continue
		}
		return false, s.Purpose
	}
	return false, ""
}

// layoutCarries reports whether a built layout holds the purpose's core structure, which is
// what makes it that kind of tower. Pure.
func layoutCarries(purpose string, lay towerLayout) bool {
	for _, pc := range lay.Pieces {
		switch purpose {
		case purposeRefinery:
			if pc.TypeID == typeMedRefinery || pc.TypeID == typeBigRefinery {
				return true
			}
		case purposeLab:
			if pc.TypeID == typeMobileLab {
				return true
			}
		case purposeFactory:
			if isAssemblyArray(pc.TypeID) {
				return true
			}
		case purposeFortress:
			if pc.TypeID == typeSMA {
				return true
			}
		}
	}
	return false
}

// layoutDiff is a refit: which of the old pieces the new layout does not want (indices into
// old) and which new pieces must be ordered. Batteries, hardeners and grid boosters are left out
// of both sides (Infrastructure grows and replaces those, posinfra.go), and so are pieces
// already being stripped. Pure.
func layoutDiff(old, want []layoutPiece) (strip []int, adds []layoutPiece) {
	need := map[int64]int{}
	for _, pc := range want {
		if isDefenseType(pc.TypeID) || isHardener(pc.TypeID) || isBoosterType(pc.TypeID) {
			continue
		}
		need[pc.TypeID]++
	}
	for i, pc := range old {
		if pc.Strip || isDefenseType(pc.TypeID) || isHardener(pc.TypeID) || isBoosterType(pc.TypeID) {
			continue
		}
		if need[pc.TypeID] > 0 {
			need[pc.TypeID]--
			continue
		}
		strip = append(strip, i)
	}
	types := make([]int64, 0, len(need))
	for t := range need {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	for _, t := range types {
		for n := 0; n < need[t]; n++ {
			adds = append(adds, layoutPiece{TypeID: t})
		}
	}
	return strip, adds
}

// reconsiderTowers is the CEO's pass: strips under way are carried along,
// and each reconsiderable tower that is due is judged.
func (m *Mod) reconsiderTowers(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts, plans []*towerPlan) {
	now := clock.Unix()
	var intel []moonIntel
	var floor float64
	loaded := false
	load := func() {
		if loaded {
			return
		}
		loaded = true
		hq := facts.StationID
		if hq == 0 {
			hq, _ = m.corpFactoryOffice(ctx, c.CorpID)
		}
		intel, _ = m.corpMoonIntel(ctx, c.CorpID, -1)
		floor = m.miningValueFloor(ctx, c, ch, hq)
	}
	u, _ := m.universe(ctx)
	for _, p := range plans {
		if ctx.Err() != nil {
			return
		}
		if !p.active() {
			continue
		}
		if m.advanceStrip(ctx, c, p) {
			_ = m.saveTowerPlan(ctx, p)
		}
		if why := m.towerWorkBarred(ctx, u, p); why != "" {
			if p.TowerID == 0 {
				// Nothing in space yet: drop the plan, the kit bought so far stays on the shelf for
				// the next tower (pullStash).
				m.ceoLog(ctx, c, powerStarbase, "reconsider", p.MoonID, 0,
					"dropped its %s tower plan at %s: %s; what kit it bought stays on the shelf",
					purposeLabel[p.Purpose], moonNameOf(ctx, m, p.MoonID), why)
				if err := m.startTeardown(ctx, c, p, why); err != nil {
					m.logBot(c.CharID, logError, "ceo", "reconsider: %v", err)
				}
				continue
			}
			load()
			m.reconsiderTower(ctx, c, facts, plans, p, intel, floor, why)
			continue
		}
		if !reconsiderable(p.Purpose) {
			continue
		}
		depot := p.Purpose == purposeDepot
		switch p.Status {
		case towerPlanned:
			if depot {
				m.voidReqsFor(ctx, c.CorpID, "tower", p.PlanID, "a depot serves no purpose")
				m.ceoLog(ctx, c, powerStarbase, "reconsider", p.MoonID, 0,
					"dropped its depot plan at %s: a depot does nothing its offices do not; what kit it bought stays on the shelf",
					moonNameOf(ctx, m, p.MoonID))
				p.Layout.ReconsiderAt, p.Layout.ReconsiderNote = now, "dropped: a depot serves no purpose"
				if err := m.startTeardown(ctx, c, p, "a depot serves no purpose"); err != nil {
					m.logBot(c.CharID, logError, "ceo", "reconsider: %v", err)
				}
			}
			continue
		case towerRigging, towerRunning:
		default:
			continue
		}
		if !depot {
			if now-p.Layout.ReconsiderAt < reconsiderEverySec || p.Status != towerRunning {
				continue
			}
			if p.OnlineAt != 0 && now-p.OnlineAt < towerReviewGraceSec {
				continue
			}
		}
		load()
		m.reconsiderTower(ctx, c, facts, plans, p, intel, floor, "")
	}
}

// towerWorkBarred says why a tower can never do its work where it stands, "" when it can: its
// system's raw security is above what its work structures anchor in (purposeMaxSec). Such a tower
// would sit in rigging for good. Only towers not yet running are judged: a running one already
// has its structures up. A reactions tower that harvests nothing is a hub.
func (m *Mod) towerWorkBarred(ctx context.Context, u *universe, p *towerPlan) string {
	switch p.Status {
	case towerPlanned, towerStaging, towerAnchored, towerRigging:
	default:
		return ""
	}
	if u == nil {
		return ""
	}
	mt, ok := u.meta[p.SystemID]
	if !ok {
		return ""
	}
	hub := p.Purpose == purposeReactions && !planHarvests(p)
	if ceil := m.purposeMaxSec(ctx, p.Purpose, hub); mt.Security > ceil {
		return fmt.Sprintf("its %s structures cannot anchor in %.2f-sec space (%.1f and below only)",
			purposeLabel[p.Purpose], mt.Security, ceil)
	}
	return ""
}

// workableSignals strikes a barred tower's own purpose off the signal list, and any purpose whose
// work structures cannot anchor at sec either.
func (m *Mod) workableSignals(ctx context.Context, sigs []purposeSignal, cur string, sec float64) []purposeSignal {
	out := sigs[:0:0]
	for _, s := range sigs {
		if s.Purpose == cur || sec > m.purposeMaxSec(ctx, s.Purpose, s.Hub) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// reconsiderTower judges one tower and acts on the verdict. barred, when set, is why the tower's
// work cannot be done where it stands (towerWorkBarred): its own purpose is struck off, so the
// verdict is a refit to something that works there or a teardown.
func (m *Mod) reconsiderTower(ctx context.Context, c *corpCEO, facts *CorpFacts, plans []*towerPlan, p *towerPlan,
	intel []moonIntel, floor float64, barred string) {
	now := clock.Unix()
	others := make([]*towerPlan, 0, len(plans))
	for _, q := range plans {
		if q.PlanID != p.PlanID {
			others = append(others, q)
		}
	}
	sigs := m.purposeSignals(ctx, c, facts, others, intel, floor)
	if barred != "" {
		if u, err := m.universe(ctx); err == nil && u != nil {
			sigs = m.workableSignals(ctx, sigs, p.Purpose, u.meta[p.SystemID].Security)
		}
	}
	class := -1
	for _, mi := range intel {
		if mi.MoonID == p.MoonID {
			class = mi.BestClass
			break
		}
	}
	race, _ := towerRaceOf(p.TowerTypeID)
	tags := facts.Policy.Tags
	layoutFor := func(purpose string) towerLayout {
		return buildLayout(layoutInput{Purpose: purpose, Size: p.Size, Race: race, Tags: tags,
			Arrays: m.purposeArrays(ctx, c.CorpID, purpose, tags), Seed: p.MoonID})
	}
	keep, to := refitChoice(p.Purpose, sigs, tags, class, func(purpose string) bool {
		return layoutCarries(purpose, layoutFor(purpose))
	})
	place := moonNameOf(ctx, m, p.MoonID)
	switch {
	case keep:
		p.Layout.ReconsiderAt, p.Layout.ReconsiderNote = now, "still serves: "+purposeLabel[p.Purpose]
		_ = m.saveTowerPlan(ctx, p)
	case to != "":
		strip, adds := layoutDiff(p.Layout.Pieces, layoutFor(to).Pieces)
		var gone, got []string
		for _, i := range strip {
			p.Layout.Pieces[i].Strip, p.Layout.Pieces[i].Off = true, true
			gone = append(gone, m.typeNameOr(ctx, p.Layout.Pieces[i].TypeID))
		}
		for _, pc := range adds {
			pc.Added = now
			p.Layout.Pieces = append(p.Layout.Pieces, pc)
			got = append(got, m.typeNameOr(ctx, pc.TypeID))
		}
		kept, _ := placeLayout(p.Layout.Pieces, p.Size, race, p.MoonID)
		p.Layout.Pieces = kept
		from := purposeLabel[p.Purpose]
		why := signalWhy(sigs, to)
		if barred != "" {
			why = barred + "; " + why
		}
		p.Purpose, p.Note = to, why
		p.Layout.ReconsiderAt = now
		p.Layout.ReconsiderNote = fmt.Sprintf("refit from %s to %s", from, purposeLabel[to])
		m.ceoLog(ctx, c, powerStarbase, "reconsider", p.TowerID, 0,
			"is refitting its %s tower at %s as a %s — %s (stripping %s; ordering %s)",
			from, place, purposeLabel[to], why, joinNames(gone), joinNames(got))
		m.queueCorpNews(ctx, c.CorpID, newsEstate, fmt.Sprintf("Our %s tower at %s is being refitted as a %s: %s.",
			from, place, purposeLabel[to], why))
		markPosWork(c.CorpID)
		_ = m.saveTowerPlan(ctx, p)
	default:
		why := fmt.Sprintf("nothing the corporation needs fits the %s tower at %s", purposeLabel[p.Purpose], place)
		if barred != "" {
			why = fmt.Sprintf("%s, and nothing else the corporation needs fits the tower at %s", barred, place)
		}
		p.Layout.ReconsiderAt, p.Layout.ReconsiderNote = now, "taken down: "+why
		if err := m.startTeardown(ctx, c, p, why); err != nil {
			m.logBot(c.CharID, logError, "ceo", "reconsider teardown: %v", err)
		}
	}
}

// signalWhy is the explanation a signal list gives for a purpose.
func signalWhy(sigs []purposeSignal, purpose string) string {
	for _, s := range sigs {
		if s.Purpose == purpose {
			return s.Why
		}
	}
	return purposeLabel[purpose]
}

// advanceStrip carries a refit's stripped structures home, one step a think: offline, emptied
// (a backhaul to the staging office's Production division), unanchored, then scooped into the
// staging office's Starbase Kit. A piece leaves the plan once it is no longer in space.
// changed means the plan needs saving.
func (m *Mod) advanceStrip(ctx context.Context, c *corpCEO, p *towerPlan) bool {
	changed := false
	var scoop []haulItem
	keep := p.Layout.Pieces[:0]
	for _, pc := range p.Layout.Pieces {
		if !pc.Strip {
			keep = append(keep, pc)
			continue
		}
		if pc.ItemID == 0 {
			changed = true // never anchored: nothing to carry
			continue
		}
		var loc int64
		if m.db.QueryRowContext(ctx, `SELECT locationID FROM entity WHERE itemID = ?`, pc.ItemID).Scan(&loc) != nil ||
			loc != p.SystemID {
			changed = true // home, or gone
			continue
		}
		keep = append(keep, pc)
		rec, anchored, err := evedb.GetStarbaseStructure(ctx, m.db, pc.ItemID)
		if err != nil {
			continue
		}
		if !anchored {
			scoop = append(scoop, haulItem{TypeID: pc.TypeID, Qty: 1, Flag: -1, ItemID: pc.ItemID})
			continue
		}
		switch rec.State {
		case evedb.StarbaseStateOnline, evedb.StarbaseStateOnlining:
			if err := evedb.OfflineStructureFor(ctx, m.db, pc.ItemID, nil); err != nil {
				m.logBot(c.CharID, logInfo, "ceo", "offlining a %s at %s for the refit: %v",
					m.typeNameOr(ctx, pc.TypeID), moonNameOf(ctx, m, p.MoonID), err)
			}
		case evedb.StarbaseStateAnchored:
			if m.stripBayEmpty(ctx, c, p, pc.ItemID) {
				_, _ = evedb.BeginStructureUnanchor(ctx, m.db, pc.ItemID)
			}
		}
	}
	p.Layout.Pieces = keep
	if len(scoop) > 0 && m.openHaulFor(ctx, p.PlanID, haulKindScoop) == nil {
		if office := m.officeAt(ctx, c.CorpID, p.StagingStationID); office != 0 {
			if _, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindScoop, PlanID: p.PlanID, FromKind: haulFromSpace,
				FromSystemID: p.SystemID, ToKind: haulToOffice, ToStationID: p.StagingStationID, ToFlag: corpDivStarbase,
				Items: scoop, PostedBy: "ceo", Note: "carrying refitted-out structures to the office"}); err != nil {
				m.logBot(c.CharID, logError, "ceo", "strip scoop: %v", err)
			}
		}
	}
	return changed
}

// stripBayEmpty reports whether a structure being stripped holds nothing,
// posting the backhaul that empties it when it does.
func (m *Mod) stripBayEmpty(ctx context.Context, c *corpCEO, p *towerPlan, itemID int64) bool {
	held := m.bayHeldAll(ctx, itemID)
	var items []haulItem
	for t, q := range held {
		if q > 0 {
			items = append(items, haulItem{TypeID: t, Qty: q, Flag: -1})
		}
	}
	if len(items) == 0 {
		return true
	}
	if m.openHaulFor(ctx, p.PlanID, haulKindBackhaul) != nil {
		return false
	}
	if p.StagingStationID != 0 {
		sort.Slice(items, func(i, j int) bool { return items[i].TypeID < items[j].TypeID })
		_, _ = m.postHaul(ctx, c, haulLeg{Kind: haulKindBackhaul, PlanID: p.PlanID, FromKind: haulFromStructure,
			FromID: itemID, FromSystemID: p.SystemID, ToKind: haulToOffice, ToStationID: p.StagingStationID,
			ToFlag: corpDivProduct, Items: items, PostedBy: "ceo", Note: "emptying a structure before the refit takes it out"})
	}
	return false
}
