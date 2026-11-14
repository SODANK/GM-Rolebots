package rolebots

import (
	"context"
	"fmt"
	"sort"

	evedb "gurimoon/db"
)

// The Infrastructure department: the corporation's towers, kept. The CEO decides whether,
// where and for what to plant a tower and when to tear one down (posceo.go, posceo_plan.go,
// posledger.go). Once a Director of Infrastructure is seated (armInfrastructure) the department
// does the rest on the staff desk sweep (infraDesk, every 15 minutes): it runs the plan's
// lifecycle (advanceTowerPlan); supplies fuel, strontium and guns (fuelerDesk, posDefenseDesk;
// Sales buys, members carry); refits lost structures on their old spot (refitTower: the core
// sends no word when a slave dies, so the plan is checked against the world); grows defences
// with the threat, never shrunk (growDefenses); expands production weekly (expandProduction);
// and keeps the grid (gridPlan: idle arrays go offline).
// With no Director seated the CEO's think runs the same passes on its slower cadence (infraRunsTowers).

const (
	infraRefitEverySec   = 30 * 60
	infraUpgradeEverySec = 86400
	infraExpandEverySec  = 7 * 86400
	infraIdleOfflineSec  = 7 * 86400
	infraSaturatedSec    = 3 * 86400
	infraLossWindowSec   = 14 * 86400
	infraLossLogMax      = 20
	// infraUpgradeCapitalMul: the corporation's free capital must cover an
	// upgrade this many times over.
	infraUpgradeCapitalMul = 3.0
	// infraUpgradeBatch is how many pieces one upgrade order adds.
	infraUpgradeBatch = 2
	infraThreatMax    = 6
	// infraHarvestPayback: a new harvester must earn this many times the
	// ozone and water its grid share burns.
	infraHarvestPayback = 2.0
)

// towerInfraBook is Infrastructure's book on one tower, kept in the plan's
// layout JSON (no column of its own).
type towerInfraBook struct {
	// LossAt is when structures were lost, newest last (infraLossLogMax).
	LossAt    []int64 `json:"l,omitempty"`
	UpgradeAt int64   `json:"u,omitempty"`
	ExpandAt  int64   `json:"e,omitempty"`
	// BusySince is since when every online assembly array has had a job.
	BusySince int64 `json:"b,omitempty"`
	// IdleSince is since when an assembly array (itemID) has had none.
	IdleSince map[int64]int64 `json:"i,omitempty"`
	// Threat is the last threat reading (towerThreat), for the board.
	Threat int `json:"t,omitempty"`
}

// recentLosses is how many structures the tower lost inside the window.
func (b towerInfraBook) recentLosses(now int64) int {
	n := 0
	for _, at := range b.LossAt {
		if now-at < infraLossWindowSec {
			n++
		}
	}
	return n
}

// ── who runs the towers ─────────────────────────────────────────────────────

type infraDeskKey struct{}

// withInfraDesk marks the context as Infrastructure's own pass: what it posts
// is labelled "infra" (posRequester).
func withInfraDesk(ctx context.Context) context.Context {
	return context.WithValue(ctx, infraDeskKey{}, true)
}

// posRequester is the label a starbase requisition or leg carries: "infra"
// from the department's desk, def otherwise (the CEO's own think, or the
// fallback desks).
func posRequester(ctx context.Context, def string) string {
	if v, _ := ctx.Value(infraDeskKey{}).(bool); v {
		return "infra"
	}
	return def
}

// infraRunsTowers reports whether a Director of Infrastructure is seated and logged in: then
// the department's desk runs the towers and the CEO's think and fallback desks leave them
// alone. While the Director is logged off its desk does not run (staffDesks), so the CEO and
// the seats keep the towers fuelled and their plans in step with the core meanwhile.
func (m *Mod) infraRunsTowers(ctx context.Context, c *corpCEO) bool {
	return m.staffFor(ctx, c).head(portInfrastructure).on()
}

// infraOwnsStatus is whether a plan in this status is the department's: from
// the moment the tower is in space. Planning, staging and the deploy leg stay
// the CEO's.
func infraOwnsStatus(status string) bool {
	return status != towerPlanned && status != towerStaging
}

// infraDesk is the Director of Infrastructure's desk (staffDesks).
func (m *Mod) infraDesk(ctx context.Context, c *corpCEO, ch Charter, d *staffMember) error {
	if !StarbasePower() {
		return nil
	}
	ctx = withInfraDesk(ctx)
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	plans, err := m.corpTowerPlans(ctx, c.CorpID, true)
	if err != nil || len(plans) == 0 {
		return err
	}
	m.towerOps(ctx, c, ch, facts, plans)
	m.fuelerDesk(ctx, c, ch, plans)
	m.posDefenseDesk(ctx, c, ch, plans)
	return nil
}

// towerOps runs every tower the department owns through its lifecycle and,
// once running, its upkeep.
func (m *Mod) towerOps(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts, plans []*towerPlan) {
	for _, p := range plans {
		if ctx.Err() != nil {
			return
		}
		if !infraOwnsStatus(p.Status) {
			continue
		}
		if err := m.advanceTowerPlan(ctx, c, ch, facts, p); err != nil {
			m.logBot(c.CharID, logError, "ceo", "tower %d: %v", p.PlanID, err)
			continue
		}
		if p.Status == towerRunning {
			m.maintainTower(ctx, c, ch, p)
		}
	}
}

// maintainTower is one running tower's upkeep, at most every
// infraRefitEverySec.
func (m *Mod) maintainTower(ctx context.Context, c *corpCEO, ch Charter, p *towerPlan) {
	if p.TowerID == 0 || !deskDue(c.CorpID, fmt.Sprintf("infra.%d", p.PlanID), infraRefitEverySec) {
		return
	}
	changed := m.refitTower(ctx, c, p)
	if t := m.towerThreat(ctx, c, p); t != p.Layout.Infra.Threat {
		p.Layout.Infra.Threat, changed = t, true
	}
	now := clock.Unix()
	if m.manageArrays(ctx, c, p) {
		changed = true
	}
	if now-p.Layout.Infra.UpgradeAt >= infraUpgradeEverySec && m.growDefenses(ctx, c, ch, p) {
		changed = true
	}
	if now-p.Layout.Infra.ExpandAt >= infraExpandEverySec {
		p.Layout.Infra.ExpandAt = now
		m.expandProduction(ctx, c, ch, p)
		changed = true
	}
	if changed {
		if err := m.saveTowerPlan(ctx, p); err != nil {
			m.logBot(c.CharID, logError, "ceo", "tower %d upkeep: %v", p.PlanID, err)
		}
	}
}

// ── refit ───────────────────────────────────────────────────────────────────

// refitTower checks every planned structure on a running tower against the
// world: a lost one goes back on the list and is re-ordered, an anchored one
// is put online (unless the department took it offline). changed = the plan
// needs saving.
func (m *Mod) refitTower(ctx context.Context, c *corpCEO, p *towerPlan) bool {
	changed, missing := false, false
	now := clock.Unix()
	for i := range p.Layout.Pieces {
		pc := &p.Layout.Pieces[i]
		if pc.Strip {
			continue // on its way out (posreconsider.go), not a loss
		}
		if pc.ItemID == 0 {
			missing = true
			continue
		}
		rec, ok, err := evedb.GetStarbaseStructure(ctx, m.db, pc.ItemID)
		if err != nil {
			return changed
		}
		if !ok {
			m.ceoLog(ctx, c, powerStarbase, "loss", pc.ItemID, 0, "lost a %s at %s; a replacement is ordered",
				m.typeNameOr(ctx, pc.TypeID), moonNameOf(ctx, m, p.MoonID))
			pc.ItemID, pc.Off = 0, false
			p.Layout.Infra.LossAt = append(p.Layout.Infra.LossAt, now)
			changed, missing = true, true
			continue
		}
		switch rec.State {
		case evedb.StarbaseStateAnchored:
			if pc.Off {
				continue
			}
			if _, err := evedb.OnlineStructureFor(ctx, m.db, pc.ItemID, nil); err != nil {
				m.logBot(c.CharID, logInfo, "ceo", "a %s at %s would not online: %v",
					m.typeNameOr(ctx, pc.TypeID), moonNameOf(ctx, m, p.MoonID), err)
			}
		case evedb.StarbaseStateOnline, evedb.StarbaseStateOnlining:
			if pc.Off {
				pc.Off, changed = false, true // somebody put it back up by hand
			}
		}
	}
	if n := len(p.Layout.Infra.LossAt); n > infraLossLogMax {
		p.Layout.Infra.LossAt = p.Layout.Infra.LossAt[n-infraLossLogMax:]
	}
	if missing {
		placeDefenses(p.Layout.Pieces, p.Size)
		if err := m.rigMissing(ctx, c, p); err != nil {
			m.logBot(c.CharID, logError, "ceo", "refit at %s: %v", moonNameOf(ctx, m, p.MoonID), err)
		}
	}
	return changed
}

// ── threat and defences ─────────────────────────────────────────────────────

// towerThreat reads how much a tower has to fear, 0..infraThreatMax: a recent
// reinforcement, structures lost, a siege on it, the corporation at war, and
// the measured danger of the system.
func (m *Mod) towerThreat(ctx context.Context, c *corpCEO, p *towerPlan) int {
	now := clock.Unix()
	in := threatInput{
		Reinforced: p.ReinforcedAt > 0 && now-p.ReinforcedAt < infraLossWindowSec,
		Losses:     p.Layout.Infra.recentLosses(now),
		Sieged:     len(m.openSieges(ctx, "towerID = ? AND phase <> 'muster'", p.TowerID)) > 0, // a muster is not visible to the owner
		War:        relCorpAtWar(c.CorpID),
		Danger:     m.towerDanger(ctx, c.CorpID, p.SystemID, m.systemSecurity(ctx, p.SystemID), 1),
	}
	return threatScore(in)
}

// threatInput is what towerThreat reads.
type threatInput struct {
	Reinforced bool
	Losses     int
	Sieged     bool
	War        bool
	Danger     float64
}

// threatScore folds a tower's troubles into steps. Pure.
func threatScore(in threatInput) int {
	t := 0
	if in.Reinforced {
		t += 2
	}
	switch {
	case in.Losses >= 3:
		t += 2
	case in.Losses > 0:
		t++
	}
	if in.Sieged {
		t += 2
	}
	if in.War {
		t++
	}
	switch {
	case in.Danger >= 2:
		t += 2
	case in.Danger >= 1:
		t++
	}
	return min(t, infraThreatMax)
}

// defenseCount is a tower's defences by kind.
type defenseCount struct{ Guns, Hardeners, EW int }

// defenseTarget is what a tower should carry at a threat: its base guns (a
// fortress twice that) plus one per step; the four hardening arrays from
// threat 2 (a fortress always); an EW battery per two guns from threat 3.
// Batteries share the compass slots, guns first. Pure.
func defenseTarget(size, purpose string, threat, slots int) defenseCount {
	g := towerGuns[size]
	if purpose == purposeFortress {
		g *= 2
	}
	g += threat
	h := 0
	if threat >= 2 || purpose == purposeFortress {
		h = 4
	}
	ew := 0
	if threat >= 3 {
		ew = (g + 1) / 2
	}
	g = min(g, slots)
	ew = max(0, min(ew, slots-g))
	return defenseCount{g, h, ew}
}

// defenseHave counts a plan's defences, standing or ordered.
func defenseHave(pieces []layoutPiece) (have defenseCount, hardeners, ew map[int64]bool) {
	hardeners, ew = map[int64]bool{}, map[int64]bool{}
	for _, pc := range pieces {
		switch {
		case isGunType(pc.TypeID):
			have.Guns++
		case isHardener(pc.TypeID):
			have.Hardeners++
			hardeners[pc.TypeID] = true
		case isDefenseType(pc.TypeID):
			have.EW++
			ew[pc.TypeID] = true
		}
	}
	return have, hardeners, ew
}

var hardenerTypes = []int64{typeHardenerEM, typeHardenerThermal, typeHardenerKin, typeHardenerExp}

func isHardener(t int64) bool {
	for _, h := range hardenerTypes {
		if h == t {
			return true
		}
	}
	return false
}

// defenseAdds is the next pieces a tower's defences want, at most n: guns, then the hardeners
// it lacks, then EW (a web and a scrambler to hold an attacker under the guns, then the ECMs
// in turn, then a damper). Never fewer than it has. Pure.
func defenseAdds(pieces []layoutPiece, race int64, size string, want defenseCount, n int) []int64 {
	have, hard, ew := defenseHave(pieces)
	var out []int64
	gun := raceGuns[race][size]
	for i := have.Guns; i < want.Guns && len(out) < n && gun != 0; i++ {
		out = append(out, gun)
	}
	for _, h := range hardenerTypes {
		if len(out) >= n || have.Hardeners >= want.Hardeners {
			break
		}
		if !hard[h] {
			out = append(out, h)
			have.Hardeners++
		}
	}
	order := append([]int64{typeWebBattery, typeScramBattery}, ecmBatteries...)
	order = append(order, 17180)
	for _, t := range order {
		if len(out) >= n || have.EW >= want.EW {
			break
		}
		if !ew[t] {
			out = append(out, t)
			have.EW++
		}
	}
	return out
}

// growDefenses orders the defences a tower's threat calls for, one batch a
// day, when the grid holds them (offlining what does least, gridPlan) and the
// corporation can afford them.
func (m *Mod) growDefenses(ctx context.Context, c *corpCEO, ch Charter, p *towerPlan) bool {
	threat := p.Layout.Infra.Threat
	if threat == 0 {
		return false
	}
	race, _ := towerRaceOf(p.TowerTypeID)
	want := defenseTarget(p.Size, p.Purpose, threat, gunSlotCapacity(p.Size))
	adds := defenseAdds(p.Layout.Pieces, race, p.Size, want, infraUpgradeBatch)
	if len(adds) == 0 {
		return false
	}
	return m.addPieces(ctx, c, ch, p, adds, nil, threat, fmt.Sprintf("threat %d", threat))
}

// addPieces puts new structures in a tower's plan, placed, and lets refit
// order and anchor them. Refused when the grid cannot hold them even after
// gridPlan, or the corporation cannot afford them. provision is a material
// per piece (nil for none).
func (m *Mod) addPieces(ctx context.Context, c *corpCEO, ch Charter, p *towerPlan, adds, provision []int64,
	threat int, why string) bool {
	need := structFit{}
	cost := 0.0
	for _, t := range adds {
		f := pieceFit(t)
		need.Power += f.Power
		need.CPU += f.CPU
		cost += evedb.TypeValueOf(ctx, m.db, t).Value
	}
	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil || free < cost*infraUpgradeCapitalMul {
		return false
	}
	race, _ := towerRaceOf(p.TowerTypeID)
	off, ok := gridPlanFor(race, p.Layout.Pieces, p.Size, need, threat, m.idleArrays(p))
	if !ok {
		// Grid-bound: the only limit on a tower is its own fitting. Order the cheapest booster that
		// makes room (Auxiliary Power for power, Shield Generation for CPU) ahead of what was asked for.
		boost := boostersToCover(race, p.Size, p.Layout.Pieces, need)
		if len(boost) == 0 {
			return false
		}
		for _, t := range boost {
			cost += evedb.TypeValueOf(ctx, m.db, t).Value
		}
		if free < cost*infraUpgradeCapitalMul {
			return false
		}
		withBoost := append([]layoutPiece(nil), p.Layout.Pieces...)
		for _, t := range boost {
			withBoost = append(withBoost, layoutPiece{TypeID: t})
		}
		if off, ok = gridPlanFor(race, withBoost, p.Size, need, threat, m.idleArrays(p)); !ok {
			return false
		}
		adds = append(append([]int64(nil), boost...), adds...)
		if provision != nil {
			provision = append(make([]int64, len(boost)), provision...)
		}
		why += " (with " + joinNames(m.typeNames(ctx, boost)) + " for room)"
	}
	for _, i := range off {
		m.offlinePiece(ctx, c, p, i, "to make room for "+why)
	}
	now := clock.Unix()
	var names []string
	for i, t := range adds {
		pc := layoutPiece{TypeID: t, Added: now}
		if i < len(provision) {
			pc.Provision = provision[i]
		}
		p.Layout.Pieces = append(p.Layout.Pieces, pc)
		names = append(names, m.typeNameOr(ctx, t))
	}
	kept, dropped := placeLayout(p.Layout.Pieces, p.Size, race, p.MoonID)
	p.Layout.Pieces = kept
	if dropped > 0 {
		m.logBot(c.CharID, logInfo, "ceo", "no room inside the field at %s for %d structure(s)",
			moonNameOf(ctx, m, p.MoonID), dropped)
	}
	p.Layout.Infra.UpgradeAt = now
	m.ceoLog(ctx, c, powerStarbase, "upgrade", p.TowerID, cost, "ordered %s for the tower at %s (%s)",
		joinNames(names), moonNameOf(ctx, m, p.MoonID), why)
	return true
}

func joinNames(names []string) string {
	if len(names) == 0 {
		return "nothing"
	}
	out := names[0]
	for i := 1; i < len(names); i++ {
		if i == len(names)-1 {
			out += " and " + names[i]
		} else {
			out += ", " + names[i]
		}
	}
	return out
}

// ── grid ────────────────────────────────────────────────────────────────────

// gridUse is what a plan's pieces draw, the ones the department took offline
// left out. Pure.
func gridUse(pieces []layoutPiece) structFit {
	var u structFit
	for _, pc := range pieces {
		if pc.Off {
			continue
		}
		f := pieceFit(pc.TypeID)
		u.Power += f.Power
		u.CPU += f.CPU
	}
	return u
}

// isWorkArray is a structure that runs jobs for the corporation: an assembly
// array or a mobile laboratory. Both are kept busy, taken offline when idle for
// a week and grown when every one is busy.
func isWorkArray(t int64) bool { return isAssemblyArray(t) || t == typeMobileLab }

func isAssemblyArray(t int64) bool {
	switch t {
	case typeAmmoAssembly, typeComponentAsm, typeSmallShipAsm, typeEquipmentAsm:
		return true
	}
	return false
}

// gridPlan answers whether `need` fits a plan's grid, and which pieces (indices) to take
// offline first: idle assembly arrays, then silos no harvester fills, then EW batteries when
// nothing threatens. Guns, hardeners, harvesters, a reactor and its silos are never taken
// offline. ok=false when even that frees too little. Pure.
func gridPlan(pieces []layoutPiece, size string, need structFit, threat int, idle map[int64]bool) (off []int, ok bool) {
	return gridPlanFor(0, pieces, size, need, threat, idle)
}

// gridPlanFor is gridPlan against a race's real grid plus the boosters in the
// plan (race 0: the size-only table).
func gridPlanFor(race int64, pieces []layoutPiece, size string, need structFit, threat int, idle map[int64]bool) (off []int, ok bool) {
	grid := boostedGrid(race, size, pieces)
	use := gridUse(pieces)
	fits := func() bool { return use.Power+need.Power <= grid.Power && use.CPU+need.CPU <= grid.CPU }
	if fits() {
		return nil, true
	}
	harvested := map[int64]bool{}
	reactor := false
	for _, pc := range pieces {
		if pc.TypeID == typeHarvester && !pc.Off {
			harvested[pc.Provision] = true
		}
		if isReactorType(pc.TypeID) {
			reactor = true
		}
	}
	var tiers [3][]int
	for i, pc := range pieces {
		// Only what stands can be switched off; a piece still on order is
		// simply not wanted.
		if pc.Off || pc.ItemID == 0 {
			continue
		}
		switch {
		case isAssemblyArray(pc.TypeID) && idle[pc.ItemID]:
			tiers[0] = append(tiers[0], i)
		case pc.TypeID == typeSilo && !reactor && !harvested[pc.Provision]:
			tiers[1] = append(tiers[1], i)
		case threat == 0 && isDefenseType(pc.TypeID) && !isGunType(pc.TypeID):
			tiers[2] = append(tiers[2], i)
		}
	}
	for _, tier := range tiers {
		// The biggest draw first: the fewest pieces offline for the room.
		sort.SliceStable(tier, func(a, b int) bool {
			return pieceFit(pieces[tier[a]].TypeID).Power > pieceFit(pieces[tier[b]].TypeID).Power
		})
		for _, i := range tier {
			if fits() {
				return off, true
			}
			f := pieceFit(pieces[i].TypeID)
			use.Power -= f.Power
			use.CPU -= f.CPU
			off = append(off, i)
		}
	}
	if fits() {
		return off, true
	}
	return nil, false
}

// offlinePiece takes one planned structure offline and marks it so.
func (m *Mod) offlinePiece(ctx context.Context, c *corpCEO, p *towerPlan, i int, why string) {
	pc := &p.Layout.Pieces[i]
	pc.Off = true
	if pc.ItemID == 0 {
		return
	}
	if err := evedb.OfflineStructureFor(ctx, m.db, pc.ItemID, nil); err != nil {
		m.logBot(c.CharID, logInfo, "ceo", "a %s at %s would not offline: %v",
			m.typeNameOr(ctx, pc.TypeID), moonNameOf(ctx, m, p.MoonID), err)
		return
	}
	m.ceoLog(ctx, c, powerStarbase, "offline", pc.ItemID, 0, "took a %s at %s offline %s",
		m.typeNameOr(ctx, pc.TypeID), moonNameOf(ctx, m, p.MoonID), why)
}

// idleArrays is the assembly arrays idle for infraIdleOfflineSec or longer.
func (m *Mod) idleArrays(p *towerPlan) map[int64]bool {
	out := map[int64]bool{}
	now := clock.Unix()
	for id, since := range p.Layout.Infra.IdleSince {
		if since > 0 && now-since >= infraIdleOfflineSec {
			out[id] = true
		}
	}
	return out
}

// manageArrays keeps a factory tower's assembly arrays matched to its work:
// a week idle and the array goes offline (one always stays up); every online
// array busy and an offline one comes back. changed = the plan needs saving.
func (m *Mod) manageArrays(ctx context.Context, c *corpCEO, p *towerPlan) bool {
	if p.Purpose != purposeFactory && p.Purpose != purposeLab {
		return false
	}
	b := &p.Layout.Infra
	if b.IdleSince == nil {
		b.IdleSince = map[int64]int64{}
	}
	now := clock.Unix()
	changed := false
	online, busy := 0, 0
	var offIdx []int
	for i, pc := range p.Layout.Pieces {
		if !isWorkArray(pc.TypeID) || pc.ItemID == 0 || pc.Strip {
			continue
		}
		if pc.Off {
			offIdx = append(offIdx, i)
			continue
		}
		online++
		tj, _ := m.loadTowerJob(ctx, pc.ItemID)
		if tj != nil {
			busy++
			if _, ok := b.IdleSince[pc.ItemID]; ok {
				delete(b.IdleSince, pc.ItemID)
				changed = true
			}
		} else if b.IdleSince[pc.ItemID] == 0 {
			b.IdleSince[pc.ItemID] = now
			changed = true
		}
	}
	allBusy := online > 0 && busy == online
	switch {
	case allBusy && b.BusySince == 0:
		b.BusySince, changed = now, true
	case !allBusy && b.BusySince != 0:
		b.BusySince, changed = 0, true
	}
	if allBusy && len(offIdx) > 0 {
		pc := &p.Layout.Pieces[offIdx[0]]
		if _, err := evedb.OnlineStructureFor(ctx, m.db, pc.ItemID, nil); err == nil {
			pc.Off = false
			b.IdleSince[pc.ItemID] = now
			m.ceoLog(ctx, c, powerStarbase, "online", pc.ItemID, 0, "brought a %s at %s back online: the work is back",
				m.typeNameOr(ctx, pc.TypeID), moonNameOf(ctx, m, p.MoonID))
			return true
		}
	}
	// A week idle goes offline, keeping one array up for the work to return to.
	idle := m.idleArrays(p)
	for i := range p.Layout.Pieces {
		pc := p.Layout.Pieces[i]
		if online <= 1 {
			break
		}
		if isWorkArray(pc.TypeID) && !pc.Off && pc.ItemID != 0 && idle[pc.ItemID] {
			m.offlinePiece(ctx, c, p, i, "after a week without work, to spare its fuel")
			online--
			changed = true
		}
	}
	return changed
}

// ── production ──────────────────────────────────────────────────────────────

// expandProduction grows what a running tower makes, weekly: a mining tower
// takes the moon's next material when a harvester there earns its fuel; a
// factory whose arrays have all been busy for days gets another.
func (m *Mod) expandProduction(ctx context.Context, c *corpCEO, ch Charter, p *towerPlan) {
	switch p.Purpose {
	case purposeMining:
		mat, ok := m.nextMoonMaterial(ctx, p)
		if !ok {
			return
		}
		m.addPieces(ctx, c, ch, p, []int64{typeHarvester, typeSilo}, []int64{mat, mat}, p.Layout.Infra.Threat,
			"to harvest "+m.typeNameOr(ctx, mat))
	case purposeFactory, purposeLab:
		m.growBusyArrays(ctx, c, ch, p)
	case purposeReactions:
		// No growth: a simple reaction eats exactly what its two harvesters make, so a third would
		// feed nothing, and a hub's intake is its feeders' business (ceoTowerNetwork).
	}
	// Every purpose: a tower's storage is not a fixed allowance. A hangar, refining array or
	// maintenance array that has filled gets another of its kind, until the grid or the money stops it.
	if t, frac := m.fullestStorage(ctx, p); t != 0 && storageWantsMore(frac) {
		m.addPieces(ctx, c, ch, p, []int64{t}, nil, p.Layout.Infra.Threat,
			fmt.Sprintf("its %s is %.0f%% full", m.typeNameOr(ctx, t), frac*100))
	}
}

// growBusyArrays adds one more array of the commonest kind once every array
// has been busy for days (factory: assembly arrays, lab: mobile laboratories).
func (m *Mod) growBusyArrays(ctx context.Context, c *corpCEO, ch Charter, p *towerPlan) {
	b := p.Layout.Infra
	if b.BusySince == 0 || clock.Unix()-b.BusySince < infraSaturatedSec {
		return
	}
	for _, pc := range p.Layout.Pieces {
		if isWorkArray(pc.TypeID) && pc.Off {
			return // an offline array comes back first (manageArrays)
		}
	}
	if t := busiestArrayType(p.Layout.Pieces); t != 0 {
		m.addPieces(ctx, c, ch, p, []int64{t}, nil, p.Layout.Infra.Threat, "its arrays are all busy")
	}
}

// infraStorageFullFrac is how full a tower's storage structure gets before the
// corporation orders another.
const infraStorageFullFrac = 0.8

// isStorageType is a structure whose fullness says the tower needs more room.
func isStorageType(t int64) bool {
	switch t {
	case typeCHA, typeSMA, typeMedRefinery, typeBigRefinery:
		return true
	}
	return false
}

// storageWantsMore is the growth test on a storage structure's fill fraction. Pure.
func storageWantsMore(frac float64) bool { return frac >= infraStorageFullFrac }

// fullestStorage is the type and fill fraction of the fullest standing storage
// structure on the tower (0, 0 when none stands or none reports a capacity).
func (m *Mod) fullestStorage(ctx context.Context, p *towerPlan) (int64, float64) {
	var best int64
	var bestFrac float64
	for _, pc := range p.Layout.Pieces {
		if pc.ItemID == 0 || pc.Off || pc.Strip || !isStorageType(pc.TypeID) {
			continue
		}
		capacity, used, err := evedb.StructureBayCapacity(ctx, m.db, pc.ItemID, -1)
		if err != nil || capacity <= 0 {
			continue
		}
		if f := used / capacity; best == 0 || f > bestFrac {
			best, bestFrac = pc.TypeID, f
		}
	}
	return best, bestFrac
}

// busiestArrayType is the assembly array type a factory tower carries most
// of: the work it was built for. Pure.
func busiestArrayType(pieces []layoutPiece) int64 {
	n := map[int64]int{}
	var best int64
	for _, pc := range pieces {
		if !isWorkArray(pc.TypeID) {
			continue
		}
		n[pc.TypeID]++
		if best == 0 || n[pc.TypeID] > n[best] || (n[pc.TypeID] == n[best] && pc.TypeID < best) {
			best = pc.TypeID
		}
	}
	return best
}

// nextMoonMaterial is the richest material the tower's moon holds that no
// harvester on the tower takes, when a harvester on it pays
// infraHarvestPayback times the fuel its grid share burns.
func (m *Mod) nextMoonMaterial(ctx context.Context, p *towerPlan) (int64, bool) {
	res, err := evedb.GetMoonResources(ctx, m.db, p.MoonID)
	if err != nil || len(res) == 0 {
		return 0, false
	}
	taken := map[int64]bool{}
	for _, pc := range p.Layout.Pieces {
		if pc.TypeID == typeHarvester {
			taken[pc.Provision] = true
		}
	}
	ids := make([]int64, 0, len(res))
	for _, r := range res {
		if !taken[r.TypeID] {
			ids = append(ids, r.TypeID)
		}
	}
	if len(ids) == 0 {
		return 0, false
	}
	vals, err := evedb.TypeValues(ctx, m.db, ids)
	if err != nil {
		return 0, false
	}
	sort.SliceStable(ids, func(i, j int) bool { return vals[ids[i]].Value > vals[ids[j]].Value })
	best := ids[0]
	earn := vals[best].Value * harvestUnitsPerHour
	burn := m.gridFuelPerHour(ctx, p, pieceFit(typeHarvester).Power+pieceFit(typeSilo).Power,
		pieceFit(typeHarvester).CPU+pieceFit(typeSilo).CPU)
	if earn < burn*infraHarvestPayback {
		return 0, false
	}
	return best, true
}

// gridFuelPerHour is the ISK of Liquid Ozone and Heavy Water an hour that a
// draw of (power, cpu) adds to a tower: the purpose-2 and -3 bill scaled by
// the share of the grid, the core's own rule (db.starbaseFuelNeeds).
func (m *Mod) gridFuelPerHour(ctx context.Context, p *towerPlan, power, cpu float64) float64 {
	race, _ := towerRaceOf(p.TowerTypeID)
	grid := boostedGrid(race, p.Size, p.Layout.Pieces)
	if grid.Power <= 0 || grid.CPU <= 0 {
		return 0
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT resourceTypeID, purpose, COALESCE(quantity, 0) FROM invControlTowerResources
		 WHERE controlTowerTypeID = ? AND purpose IN (2, 3)`, p.TowerTypeID)
	if err != nil {
		return 0
	}
	defer rows.Close()
	total := 0.0
	for rows.Next() {
		var t, purpose, q int64
		if rows.Scan(&t, &purpose, &q) != nil {
			continue
		}
		share := power / grid.Power
		if purpose == 3 {
			share = cpu / grid.CPU
		}
		total += float64(q) * share * evedb.TypeValueOf(ctx, m.db, t).Value
	}
	return total
}

// ── the review ──────────────────────────────────────────────────────────────

// infraMeasure is the Director of Infrastructure's week (staffMeasure): the
// share of the corporation's towers running, less every tower that went dark.
func (m *Mod) infraMeasure(ctx context.Context, c *corpCEO, sm *staffMember) (score, metric float64, why string) {
	plans, _ := m.corpTowerPlans(ctx, c.CorpID, true)
	total, running, dark, lost := 0, 0, 0, 0
	since := clock.Unix() - staffReviewEverySec
	for _, p := range plans {
		if p.TowerID == 0 {
			continue
		}
		total++
		if p.Status == towerRunning {
			running++
		}
		if p.UrgentAt >= since {
			dark++
		}
		lost += p.Layout.Infra.recentLosses(clock.Unix())
	}
	if total == 0 {
		return 1, 0, "no tower in space"
	}
	frac := float64(running) / float64(total)
	score = clampPerf(0.7 + 0.6*frac - 0.1*float64(dark))
	return score, frac, fmt.Sprintf("%d of %d tower(s) running, %d went dark this week, %d structure(s) lost",
		running, total, dark, lost)
}
