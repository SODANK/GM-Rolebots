package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

// What the moon's output becomes: the network, reactions and the T2 tier.
// A miner+reactor is a Medium tower on a moon holding both inputs of a simple reaction, with a
// harvester per input, a Medium Reactor Array and an output silo. It is a research or
// manufacturing house's first tower (purposeTags); combat and trade corporations never react.
// A hub is a Large tower whose Reactor Array runs a complex reaction on intermediates the
// corporation's own towers make, routed on goo legs (rolebot_corp_chain). Nobody sells moon
// goo (the NPC market only bids), so every reactor input is the corporation's own harvest.
// What to react follows the T2 chain (reactionScore): the library's own T2 prints first, then a
// member's print it could buy, then its race's composite (raceComposite), then the best NPC bid.
// T2 has no invention: prints come from members or the library's purchase orders.

const (
	gooLegMinUnits = 500
	// siloSellFrac is how full a silo gets before the trader empties it home: a tenth is 2,000
	// units of intermediate, about four goo legs' worth.
	siloSellFrac       = 0.10
	t2OfferEverySec    = 3 * 86400
	t2OfferPremium     = 1.10
	componentPOEvery   = 2 * 86400
	componentGroup     = 334
	compositeGroup     = 429
	intermediateGroup  = 428
	moonMaterialGroup  = 427
	simpleReactionGrp  = 436
	complexReactionGrp = 484
)

// ── reactions ───────────────────────────────────────────────────────────────

// harvestedBy is every moon material the corporation's running towers pull: a mining
// tower's harvesters and a miner+reactor's alike.
func (m *Mod) harvestedBy(plans []*towerPlan) map[int64][]*towerPlan {
	out := map[int64][]*towerPlan{}
	for _, p := range plans {
		if (p.Purpose != purposeMining && p.Purpose != purposeReactions) || (p.Status != towerRunning && p.Status != towerRigging) {
			continue
		}
		for _, pc := range p.Layout.Pieces {
			if pc.TypeID == typeHarvester && pc.Provision != 0 {
				out[pc.Provision] = append(out[pc.Provision], p)
			}
		}
	}
	return out
}

// producedBy is harvestedBy plus what the corporation's running reactors
// make: the materials a hub could be fed, and the T2-chain test's "the
// corporation's own goo".
func (m *Mod) producedBy(ctx context.Context, plans []*towerPlan) map[int64][]*towerPlan {
	out := m.harvestedBy(plans)
	for _, p := range plans {
		if p.Purpose != purposeReactions || (p.Status != towerRunning && p.Status != towerRigging) {
			continue
		}
		for _, pc := range p.Layout.Pieces {
			if !isReactorType(pc.TypeID) || pc.Provision == 0 {
				continue
			}
			_, outs, err := evedb.ReactionRecipe(ctx, m.db, pc.Provision)
			if err != nil {
				continue
			}
			for _, o := range outs {
				out[o.TypeID] = append(out[o.TypeID], p)
			}
		}
	}
	return out
}

// planHarvests is whether a reactions plan mines its own inputs (a
// miner+reactor) rather than being fed by other towers (a hub). Pure.
func planHarvests(p *towerPlan) bool {
	for _, pc := range p.Layout.Pieces {
		if pc.TypeID == typeHarvester {
			return true
		}
	}
	return false
}

// reactionRecipe is one reaction the NPC market sells, as the planner reads
// it: its group (simple or complex), its single output and its inputs.
type reactionRecipe struct {
	Reaction int64
	Group    int64
	Output   int64
	Inputs   []int64
	InLines  []evedb.ReactionLine
	OutLines []evedb.ReactionLine
}

var (
	reactionRecipeOnce sync.Once
	reactionRecipeList []reactionRecipe
)

// reactionRecipes is every published simple and complex reaction an NPC
// corporation sells, with its recipe. Static data, read once.
func (m *Mod) reactionRecipes(ctx context.Context) []reactionRecipe {
	reactionRecipeOnce.Do(func() {
		rows, err := m.db.QueryContext(ctx, `
			SELECT t.typeID, t.groupID FROM invTypes t
			 WHERE t.groupID IN (?, ?) AND t.published = 1
			   AND EXISTS (SELECT 1 FROM crpnpccorporationtrades c WHERE c.typeID = t.typeID AND c.supplyDemand > 0)
			 ORDER BY t.typeID`, simpleReactionGrp, complexReactionGrp)
		if err != nil {
			return
		}
		type idg struct{ id, group int64 }
		var ids []idg
		for rows.Next() {
			var x idg
			if rows.Scan(&x.id, &x.group) == nil {
				ids = append(ids, x)
			}
		}
		rows.Close()
		for _, x := range ids {
			ins, outs, err := evedb.ReactionRecipe(ctx, m.db, x.id)
			if err != nil || len(ins) == 0 || len(outs) != 1 {
				continue
			}
			r := reactionRecipe{Reaction: x.id, Group: x.group, Output: outs[0].TypeID, InLines: ins, OutLines: outs}
			for _, in := range ins {
				r.Inputs = append(r.Inputs, in.TypeID)
			}
			reactionRecipeList = append(reactionRecipeList, r)
		}
	})
	return reactionRecipeList
}

// moonReactionPairs is the reactions a moon supports on its own: those whose
// every input it holds. Pure.
func moonReactionPairs(mats []moonMat, recipes []reactionRecipe) []reactionRecipe {
	on := map[int64]bool{}
	for _, mm := range mats {
		if mm.Qty > 0 {
			on[mm.TypeID] = true
		}
	}
	var out []reactionRecipe
	for _, r := range recipes {
		ok := len(r.Inputs) > 0
		for _, in := range r.Inputs {
			if !on[in] {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

// Where a reaction's output sits in the corporation's T2 chain: the library's
// own prints bill it (or what it makes), a member's print does, its race's
// component family does, or nothing but the NPC bid wants it.
const (
	reactionTierMarket = 0
	reactionTierRace   = 1
	reactionTierMember = 2
	reactionTierCorp   = 3
)

// raceComposite is each race's advanced material, the one every T2 construction component is
// built on (Tungsten Carbide Armor Plate, Titanium Diborite Armor Plate and so on). A
// corporation with no T2 print of its own works toward its race's.
var raceComposite = map[int64]int64{
	raceAmarr:    16672, // Tungsten Carbide
	raceCaldari:  16671, // Titanium Carbide
	raceMinmatar: 16673, // Fernite Carbide
	raceGallente: 16670, // Crystalline Carbonide
}

// reactionScoreInput is what ranks one reaction for a corporation.
type reactionScoreInput struct {
	Recipe reactionRecipe
	// Harvestable is every material the corporation could feed the reactor:
	// what its towers harvest and react, plus the candidate moon's own.
	Harvestable map[int64]bool
	// CorpChain, MemberChain and RaceChain are every node (goo, intermediate,
	// composite) of the chains under the library's T2 prints, under members'
	// T2 prints, and under the race's composite.
	CorpChain, MemberChain, RaceChain map[int64]bool
	// Margin is output value minus input value a cycle; MarginRef the best positive margin
	// among the candidates (the normaliser).
	Margin, MarginRef float64
}

// reactionScore ranks a reaction: its tier dominates (a reaction the corporation's own prints
// need beats one a member's print needs, which beats the race's composite, which beats a bare
// market margin) and the margin, scaled to the best on offer, breaks ties within a tier. ok is
// false when an input is out of reach, or nothing but the market wants it and it does not pay. Pure.
func reactionScore(in reactionScoreInput) (score float64, tier int, ok bool) {
	for _, t := range in.Recipe.Inputs {
		if !in.Harvestable[t] {
			return 0, 0, false
		}
	}
	out := in.Recipe.Output
	switch {
	case in.CorpChain[out]:
		tier = reactionTierCorp
	case in.MemberChain[out]:
		tier = reactionTierMember
	case in.RaceChain[out]:
		tier = reactionTierRace
	default:
		tier = reactionTierMarket
		if in.Margin <= 0 {
			return 0, tier, false
		}
	}
	frac := 0.0
	if in.MarginRef > 0 {
		frac = math.Max(0, math.Min(0.99, in.Margin/in.MarginRef))
	}
	return float64(tier) + frac, tier, true
}

// reactionChoice is a reaction the corporation could run and what it nets.
type reactionChoice struct {
	ReactionTypeID int64
	Group          int64
	Inputs         []evedb.ReactionLine
	Outputs        []evedb.ReactionLine
	Tier           int
	Score          float64
	Margin         float64 // output value − input value, per cycle
	OutValue       float64 // output value per cycle (an hour): the tower's gross
}

// reactionContext is the corporation's side of reactionScore, built once a
// think: the chain sets and what its towers make.
type reactionContext struct {
	Corp, Member, Race map[int64]bool
	Produced           map[int64][]*towerPlan
}

func (m *Mod) reactionContext(ctx context.Context, c *corpCEO, plans []*towerPlan) reactionContext {
	rc := reactionContext{Corp: map[int64]bool{}, Member: map[int64]bool{}, Race: map[int64]bool{},
		Produced: m.producedBy(ctx, plans)}
	prints, _ := int64Column(ctx, m, `
		SELECT l.bpTypeID FROM rolebot_corp_library l JOIN invBlueprintTypes bt ON bt.blueprintTypeID = l.bpTypeID
		 WHERE l.corporationID = ? AND bt.techLevel = 2`, c.CorpID)
	for _, bp := range prints {
		for t := range m.chainNodes(ctx, bp) {
			rc.Corp[t] = true
		}
	}
	for _, bp := range m.memberT2Prints(ctx, c.CorpID) {
		for t := range m.chainNodes(ctx, bp) {
			rc.Member[t] = true
		}
	}
	if comp := raceComposite[m.charRace(ctx, c.CharID)]; comp != 0 {
		rc.Race = m.compositeChain(ctx, comp)
	}
	return rc
}

// rankReactions scores the recipes against a corporation's context and what
// is harvestable, best first; recipes that cannot run are left out.
func (m *Mod) rankReactions(ctx context.Context, rc reactionContext, recipes []reactionRecipe, harvestable map[int64]bool) []reactionChoice {
	val := func(lines []evedb.ReactionLine) float64 {
		v := 0.0
		for _, l := range lines {
			units := evedb.ReactionUnits(l.Batches, evedb.MoonMiningAmount(ctx, m.db, l.TypeID))
			v += evedb.TypeValueOf(ctx, m.db, l.TypeID).Value * float64(units)
		}
		return v
	}
	type cand struct {
		r        reactionRecipe
		margin   float64
		outValue float64
	}
	var cands []cand
	ref := 0.0
	for _, r := range recipes {
		feasible := true
		for _, in := range r.Inputs {
			if !harvestable[in] {
				feasible = false
				break
			}
		}
		if !feasible {
			continue
		}
		ov := val(r.OutLines)
		mg := ov - val(r.InLines)
		cands = append(cands, cand{r, mg, ov})
		ref = math.Max(ref, mg)
	}
	var out []reactionChoice
	for _, cd := range cands {
		score, tier, ok := reactionScore(reactionScoreInput{Recipe: cd.r, Harvestable: harvestable,
			CorpChain: rc.Corp, MemberChain: rc.Member, RaceChain: rc.Race, Margin: cd.margin, MarginRef: ref})
		if !ok {
			continue
		}
		out = append(out, reactionChoice{ReactionTypeID: cd.r.Reaction, Group: cd.r.Group, Inputs: cd.r.InLines,
			Outputs: cd.r.OutLines, Tier: tier, Score: score, Margin: cd.margin, OutValue: cd.outValue})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ReactionTypeID < out[j].ReactionTypeID
	})
	return out
}

// simpleRecipes is the simple (group 436) half of the recipe list: what a
// Medium Reactor Array accepts. Pure.
func simpleRecipes(recipes []reactionRecipe) []reactionRecipe {
	var out []reactionRecipe
	for _, r := range recipes {
		if r.Group == simpleReactionGrp {
			out = append(out, r)
		}
	}
	return out
}

// moonReactionCase is the best reaction a miner+reactor on this moon could
// run for the corporation, ok=false when the moon supports none it can use.
func (m *Mod) moonReactionCase(ctx context.Context, rc reactionContext, mats []moonMat) (reactionChoice, bool) {
	pairs := moonReactionPairs(mats, simpleRecipes(m.reactionRecipes(ctx)))
	if len(pairs) == 0 {
		return reactionChoice{}, false
	}
	harvestable := map[int64]bool{}
	for t := range rc.Produced {
		harvestable[t] = true
	}
	for _, mm := range mats {
		if mm.Qty > 0 {
			harvestable[mm.TypeID] = true
		}
	}
	rx := m.rankReactions(ctx, rc, pairs, harvestable)
	if len(rx) == 0 {
		return reactionChoice{}, false
	}
	return rx[0], true
}

// complexHubCase is the complex reaction a Large hub would run: one whose composite a print
// bills (the library's, a member's, or the race's) and whose every input the corporation's
// towers already make. It is strict because a hub that waits on a feeder burns Large fuel idle.
func (m *Mod) complexHubCase(ctx context.Context, rc reactionContext) (reactionChoice, bool) {
	harvestable := map[int64]bool{}
	for t := range rc.Produced {
		harvestable[t] = true
	}
	var recipes []reactionRecipe
	for _, r := range m.reactionRecipes(ctx) {
		if r.Group == complexReactionGrp && (rc.Corp[r.Output] || rc.Member[r.Output] || rc.Race[r.Output]) {
			recipes = append(recipes, r)
		}
	}
	rx := m.rankReactions(ctx, rc, recipes, harvestable)
	if len(rx) == 0 {
		return reactionChoice{}, false
	}
	return rx[0], true
}

// recipeFor is a reaction's recipe out of the cached list.
func (m *Mod) recipeFor(ctx context.Context, reaction int64) (reactionRecipe, bool) {
	for _, r := range m.reactionRecipes(ctx) {
		if r.Reaction == reaction {
			return r, true
		}
	}
	return reactionRecipe{}, false
}

// memberT2Prints is the T2 originals the corporation's bot members hold at
// its factory office: prints it could buy (corpBuyMemberT2).
func (m *Mod) memberT2Prints(ctx context.Context, corpID int64) []int64 {
	stationID, _ := m.corpFactoryOffice(ctx, corpID)
	if stationID == 0 {
		return nil
	}
	ids, _ := int64Column(ctx, m, `
		SELECT DISTINCT e.typeID FROM entity e
		  JOIN character_ ch ON ch.characterID = e.ownerID AND ch.corporationID = ?
		  JOIN rolebot_characters b ON b.characterID = e.ownerID AND b.enabled = 1
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID AND bt.techLevel = 2
		  LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
		 WHERE e.locationID = ? AND e.flag = 4 AND COALESCE(bp.copy, 0) = 0
		 LIMIT 20`, corpID, stationID)
	return ids
}

func (m *Mod) sellerAnywhere(ctx context.Context, typeID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM market_orders WHERE typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0 LIMIT 1`,
		typeID).Scan(&n)
	return n > 0
}

// reactionLayoutInputs is what a reactions tower's silos are set to: the
// reaction's inputs then its outputs, and the reaction itself for the reactor.
func reactionLayoutInputs(rc reactionChoice) (reaction int64, mats []int64) {
	for _, l := range rc.Inputs {
		mats = append(mats, l.TypeID)
	}
	for _, l := range rc.Outputs {
		mats = append(mats, l.TypeID)
	}
	return rc.ReactionTypeID, mats
}

// reactionLinks wires a reactions tower's reactor: each input from its silo, or straight from
// its harvester where the shape gave it no buffer; the reactor into each output silo.
// (Harvester to silo is ensureTowerWiring's own rule.)
func (m *Mod) reactionLinks(ctx context.Context, p *towerPlan) []evedb.StructureLink {
	if p.Purpose != purposeReactions {
		return nil
	}
	var reaction int64
	for _, pc := range p.Layout.Pieces {
		if isReactorType(pc.TypeID) && pc.ItemID != 0 {
			reaction = pc.Provision
		}
	}
	if reaction == 0 {
		return nil
	}
	ins, outs, err := evedb.ReactionRecipe(ctx, m.db, reaction)
	if err != nil {
		return nil
	}
	var in, out []int64
	for _, l := range ins {
		in = append(in, l.TypeID)
	}
	for _, l := range outs {
		out = append(out, l.TypeID)
	}
	return reactionLinkSet(p.Layout.Pieces, in, out)
}

// reactionLinkSet is the reactor's wiring for a set of standing pieces: an
// input comes from the silo set to it when one stands, else from the
// harvester mining it; every output goes to the silo set to it. Nothing for
// a piece not yet anchored. Pure.
func reactionLinkSet(pieces []layoutPiece, ins, outs []int64) []evedb.StructureLink {
	var reactor int64
	silos := map[int64]int64{}      // material → silo
	harvesters := map[int64]int64{} // material → harvester
	for _, pc := range pieces {
		if pc.ItemID == 0 {
			continue
		}
		switch {
		case isReactorType(pc.TypeID):
			reactor = pc.ItemID
		case pc.TypeID == typeSilo && pc.Provision != 0:
			silos[pc.Provision] = pc.ItemID
		case pc.TypeID == typeHarvester && pc.Provision != 0:
			harvesters[pc.Provision] = pc.ItemID
		}
	}
	if reactor == 0 {
		return nil
	}
	var links []evedb.StructureLink
	for _, t := range ins {
		if s := silos[t]; s != 0 {
			links = append(links, evedb.StructureLink{TypeID: t, SourceID: s, DestID: reactor})
		} else if h := harvesters[t]; h != 0 {
			links = append(links, evedb.StructureLink{TypeID: t, SourceID: h, DestID: reactor})
		}
	}
	for _, t := range outs {
		if s := silos[t]; s != 0 {
			links = append(links, evedb.StructureLink{TypeID: t, SourceID: reactor, DestID: s})
		}
	}
	return links
}

// ── the network ─────────────────────────────────────────────────────────────

// ceoTowerNetwork keeps the chain view current and the hub fed: feeder silo to hub input silo
// on goo legs, inputs nobody harvests requisitioned, the reactor loaded with its reaction.
func (m *Mod) ceoTowerNetwork(ctx context.Context, c *corpCEO, ch Charter, plans []*towerPlan) {
	have := m.producedBy(ctx, plans)
	for _, hub := range plans {
		if hub.Purpose != purposeReactions || hub.Status != towerRunning {
			continue
		}
		var reaction int64
		inputSilo := map[int64]int64{}
		ownHarvest := map[int64]bool{}
		for _, pc := range hub.Layout.Pieces {
			if pc.TypeID == typeReactor || pc.TypeID == typeMediumReactor {
				reaction = pc.Provision
				m.ensureReactorLoaded(ctx, c, hub, pc)
			}
			if pc.TypeID == typeHarvester && pc.Provision != 0 {
				ownHarvest[pc.Provision] = true
			}
		}
		if reaction == 0 {
			continue
		}
		ins, _, err := evedb.ReactionRecipe(ctx, m.db, reaction)
		if err != nil {
			continue
		}
		for _, pc := range hub.Layout.Pieces {
			if pc.TypeID == typeSilo && pc.ItemID != 0 {
				inputSilo[pc.Provision] = pc.ItemID
			}
		}
		for _, in := range ins {
			if ownHarvest[in.TypeID] {
				continue // a miner+reactor's own harvester feeds it
			}
			dst := inputSilo[in.TypeID]
			if dst == 0 {
				continue
			}
			var feeders []*towerPlan
			for _, f := range have[in.TypeID] {
				if f.PlanID != hub.PlanID {
					feeders = append(feeders, f)
				}
			}
			if len(feeders) == 0 {
				m.requisitionReactionInput(ctx, c, hub, in.TypeID, dst)
				continue
			}
			for _, f := range feeders {
				_, _ = m.db.ExecContext(ctx, `
					INSERT IGNORE INTO rolebot_corp_chain (corporationID, fromPlanID, toPlanID, typeID) VALUES (?, ?, ?, ?)`,
					c.CorpID, f.PlanID, hub.PlanID, in.TypeID)
				src := siloFor(f, in.TypeID)
				if src == 0 {
					continue
				}
				units, _ := evedb.SiloQuantity(ctx, m.db, src, in.TypeID)
				if units < gooLegMinUnits || m.openGooLeg(ctx, f.PlanID, src) {
					continue
				}
				_, _ = m.postHaul(ctx, c, haulLeg{Kind: haulKindGoo, PlanID: f.PlanID, FromKind: haulFromStructure,
					FromID: src, FromSystemID: f.SystemID, ToKind: haulToTower, ToID: dst, ToSystemID: hub.SystemID,
					Items: []haulItem{{TypeID: in.TypeID, Qty: units, Flag: -1}}, PostedBy: "chain",
					Note: fmt.Sprintf("%s from %s to the reactions hub", m.typeNameOr(ctx, in.TypeID), moonNameOf(ctx, m, f.MoonID))})
			}
		}
	}
	m.corpBuyMemberT2(ctx, c, ch, plans)
	m.requestComponentPrints(ctx, c, plans)
	m.feedFactoryMaterials(ctx, c)
}

func siloFor(p *towerPlan, material int64) int64 {
	for _, pc := range p.Layout.Pieces {
		if pc.TypeID == typeSilo && pc.Provision == material && pc.ItemID != 0 {
			return pc.ItemID
		}
	}
	return 0
}

func (m *Mod) openGooLeg(ctx context.Context, planID, src int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_haul WHERE planID = ? AND fromID = ? AND status IN ('open','claimed')`,
		planID, src).Scan(&n)
	return n > 0
}

// requisitionReactionInput buys an input no feeder harvests, onto the hub's staging shelf, and
// hauls it to the silo, only while somebody sells it: the NPC market bids on goo and never
// asks, so the requisition would sit open and be posted again every think.
func (m *Mod) requisitionReactionInput(ctx context.Context, c *corpCEO, hub *towerPlan, typeID, silo int64) {
	office := m.officeAt(ctx, c.CorpID, hub.StagingStationID)
	if office == 0 || !m.sellerAnywhere(ctx, typeID) {
		return
	}
	units, _ := evedb.SiloQuantity(ctx, m.db, silo, typeID)
	want := int64(4000) - units
	if want <= 0 {
		return
	}
	shelf := m.divisionHeld(ctx, c.CorpID, office, corpDivStarbase)[typeID]
	if shelf >= gooLegMinUnits && !m.openGooLeg(ctx, hub.PlanID, office) {
		_, _ = m.postHaul(ctx, c, haulLeg{Kind: haulKindGoo, PlanID: hub.PlanID, FromKind: haulFromOffice,
			FromID: office, FromStationID: hub.StagingStationID, FromFlag: corpDivStarbase, ToKind: haulToTower,
			ToID: silo, ToSystemID: hub.SystemID, Items: []haulItem{{TypeID: typeID, Qty: min(shelf, want), Flag: corpDivStarbase}},
			PostedBy: "chain", Note: "bought input for the reactions hub"})
		return
	}
	if need := want - shelf - m.reqPending(ctx, c.CorpID, typeID, hub.StagingStationID, corpDivStarbase); need > 0 {
		_, _ = m.postRequisition(ctx, c, requisition{TypeID: typeID, Qty: need, DestStationID: hub.StagingStationID,
			DestFlag: corpDivStarbase, Requester: "industry", RefKind: "tower", RefID: hub.PlanID,
			Deadline: clock.Unix() + 5*86400, Note: "reaction input"})
	}
}

// ensureReactorLoaded makes sure the reactor holds its reaction: the reaction
// item comes with the kit (posceo.go) and the rig leg drops it in; a reactor
// still empty here asks for one.
func (m *Mod) ensureReactorLoaded(ctx context.Context, c *corpCEO, p *towerPlan, pc layoutPiece) {
	if pc.ItemID == 0 || pc.Provision == 0 {
		return
	}
	if _, rx, ok, err := evedb.ReactorReaction(ctx, m.db, pc.ItemID); err == nil && ok && rx == pc.Provision {
		return
	}
	office := m.officeAt(ctx, c.CorpID, p.StagingStationID)
	if office == 0 {
		return
	}
	if m.divisionHeld(ctx, c.CorpID, office, corpDivStarbase)[pc.Provision] > 0 {
		if m.openHaulFor(ctx, p.PlanID, haulKindRig) == nil {
			_, _ = m.postHaul(ctx, c, haulLeg{Kind: haulKindRig, PlanID: p.PlanID, FromKind: haulFromOffice,
				FromID: office, FromStationID: p.StagingStationID, FromFlag: corpDivStarbase, ToKind: haulToTower,
				ToID: p.TowerID, ToSystemID: p.SystemID,
				Items: []haulItem{{TypeID: pc.Provision, Qty: 1, Flag: corpDivStarbase}}, PostedBy: "chain",
				Note: "the reaction for the reactor"})
		}
		return
	}
	if m.reqPending(ctx, c.CorpID, pc.Provision, p.StagingStationID, corpDivStarbase) == 0 {
		_, _ = m.postRequisition(ctx, c, requisition{TypeID: pc.Provision, Qty: 1, DestStationID: p.StagingStationID,
			DestFlag: corpDivStarbase, Requester: "industry", RefKind: "tower", RefID: p.PlanID,
			Deadline: clock.Unix() + 4*86400, Note: "the reactor's reaction"})
	}
}

// insertReaction drops a carried reaction item into a reactor (rig leg).
func (m *Mod) insertReaction(ctx context.Context, b *botRun, p *towerPlan, l *haulLeg) int {
	n := 0
	for _, pc := range p.Layout.Pieces {
		if (pc.TypeID != typeReactor && pc.TypeID != typeMediumReactor) || pc.ItemID == 0 || pc.Provision == 0 {
			continue
		}
		if _, _, ok, _ := evedb.ReactorReaction(ctx, m.db, pc.ItemID); ok {
			continue
		}
		var itemID int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT itemID FROM entity WHERE locationID = ? AND flag = 5 AND ownerID = ? AND typeID = ? LIMIT 1`,
			b.ShipID, b.CorpID, pc.Provision).Scan(&itemID)
		if itemID == 0 {
			continue
		}
		if _, err := evedb.AddToStarbaseBay(ctx, m.db, pc.ItemID, itemID, 1, 0, b.CorpID, false); err == nil {
			n++
			for j := range l.Carrying {
				if l.Carrying[j].TypeID == pc.Provision && l.Carrying[j].Qty > 0 {
					l.Carrying[j].Qty--
					break
				}
			}
		}
	}
	return n
}

// ── the trader desk ─────────────────────────────────────────────────────────

// traderDesk disposes of tower output: a silo the chain does not route is
// emptied home when it is siloSellFrac full; what lands in the staging
// office's Production division is filed into Materials when the factory
// wants it (feedFactoryMaterials), else listed at the staging station.
func (m *Mod) traderDesk(ctx context.Context, c *corpCEO, ch Charter, plans []*towerPlan) {
	routed := map[int64]bool{}
	if rows, err := m.db.QueryContext(ctx, `SELECT fromPlanID, typeID FROM rolebot_corp_chain WHERE corporationID = ?`, c.CorpID); err == nil {
		for rows.Next() {
			var pid, t int64
			if rows.Scan(&pid, &t) == nil {
				routed[pid*1_000_000+t] = true
			}
		}
		rows.Close()
	}
	for _, p := range plans {
		if p.Status != towerRunning || p.StagingStationID == 0 {
			continue
		}
		for _, pc := range p.Layout.Pieces {
			if pc.TypeID != typeSilo || pc.ItemID == 0 || pc.Provision == 0 {
				continue
			}
			if routed[p.PlanID*1_000_000+pc.Provision] {
				continue // the chain moves it
			}
			if p.Purpose == purposeReactions && m.isReactionInput(ctx, p, pc.Provision) {
				continue
			}
			used, capacity, err := evedb.SiloUnits(ctx, m.db, pc.ItemID, typeSilo, pc.Provision)
			if err != nil || capacity <= 0 || float64(used) < float64(capacity)*siloSellFrac {
				continue
			}
			if m.openGooLeg(ctx, p.PlanID, pc.ItemID) {
				continue
			}
			_, _ = m.postHaul(ctx, c, haulLeg{Kind: haulKindBackhaul, PlanID: p.PlanID, FromKind: haulFromStructure,
				FromID: pc.ItemID, FromSystemID: p.SystemID, ToKind: haulToOffice, ToStationID: p.StagingStationID,
				ToFlag: corpDivProduct, Items: []haulItem{{TypeID: pc.Provision, Qty: used, Flag: -1}}, PostedBy: "trader",
				Note: fmt.Sprintf("%d× %s from %s", used, m.typeNameOr(ctx, pc.Provision), moonNameOf(ctx, m, p.MoonID))})
		}
	}
	m.listTowerGoods(ctx, c, plans)
}

func (m *Mod) isReactionInput(ctx context.Context, p *towerPlan, typeID int64) bool {
	for _, pc := range p.Layout.Pieces {
		if (pc.TypeID == typeReactor || pc.TypeID == typeMediumReactor) && pc.Provision != 0 {
			ins, _, err := evedb.ReactionRecipe(ctx, m.db, pc.Provision)
			if err != nil {
				return false
			}
			for _, in := range ins {
				if in.TypeID == typeID {
					return true
				}
			}
		}
	}
	return false
}

// listTowerGoods sells moon goo and reaction output in the staging offices' Production
// division: into a bid at the staging station when one pays the ask; else on a sale run to the
// best bidder in reach when that beats listing (posgoosale.go); else listed, one stack per
// order, a hair under the best regional ask.
func (m *Mod) listTowerGoods(ctx context.Context, c *corpCEO, plans []*towerPlan) {
	gooOpen, err := m.tradeLinesSweep(ctx, c, true)
	if err != nil {
		rlogf("rolebots: corp %d goo sale runs: %v", c.CorpID, err)
	}
	holdCap := -1.0
	hold := func() float64 {
		if holdCap < 0 {
			holdCap, _ = m.tradeDeskHold(ctx, c.CorpID)
		}
		return holdCap
	}
	stations := map[int64]bool{}
	for _, p := range plans {
		if p.StagingStationID != 0 {
			stations[p.StagingStationID] = true
		}
	}
	for st := range stations {
		office := m.officeAt(ctx, c.CorpID, st)
		if office == 0 {
			continue
		}
		rows, err := m.db.QueryContext(ctx, `
			SELECT e.itemID, e.typeID, e.quantity FROM entity e JOIN invTypes t ON t.typeID = e.typeID
			 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = ? AND e.singleton = 0
			   AND t.groupID IN (?, ?, ?)
			 ORDER BY e.quantity DESC LIMIT 6`, c.CorpID, office, corpDivProduct,
			moonMaterialGroup, intermediateGroup, compositeGroup)
		if err != nil {
			continue
		}
		type lot struct{ item, typeID, qty int64 }
		var lots []lot
		for rows.Next() {
			var l lot
			if rows.Scan(&l.item, &l.typeID, &l.qty) == nil {
				lots = append(lots, l)
			}
		}
		rows.Close()
		for _, l := range lots {
			if m.factoryWants(ctx, c.CorpID, l.typeID) {
				continue // feedFactoryMaterials files it
			}
			regionID, _ := evedb.GetRegionForStation(ctx, m.db, st)
			price := towerGoodsAsk(m.reqAskNear(ctx, l.typeID, st), evedb.TypeValueOf(ctx, m.db, l.typeID).Value,
				m.regionBestBid(ctx, l.typeID, regionID))
			if price <= 0 {
				continue
			}
			placer := m.corpOrderPlacer(ctx, c.CorpID, c.CharID)
			// A bidder at the staging station that pays the ask takes the lot outright.
			if bid, take := m.outsideBidTake(ctx, c.CorpID, st, l.typeID, l.qty); placer != 0 && take > 0 && bid >= price {
				out, err := evedb.PlaceSellOrder(ctx, m.db, placer, st, l.typeID, l.item, bid, take, -1, 1, 0, true)
				if err == nil && out != nil && out.ErrorMsg == "" && out.FilledQty > 0 {
					got := 0.0
					for _, f := range out.Fills {
						got += f.Price * float64(f.Quantity)
					}
					m.corpBook(ctx, c.CorpID, lineSales, got, l.typeID, fmt.Sprintf("%d× %s", out.FilledQty, m.typeNameOr(ctx, l.typeID)))
					m.ceoLog(ctx, c, powerStarbase, "sell", l.typeID, got,
						"sold %d× %s from its towers into a bid at %s, %s each", out.FilledQty,
						m.typeNameOr(ctx, l.typeID), m.stationLabel(ctx, st), iskString(bid))
					continue // the rest, if any, is listed on the next pass
				}
			}
			// None here: the Sales arm carries it to a bidder in reach when that beats the listing. This
			// runs before the open-order test, so stock behind a resting ask still goes.
			if m.gooSaleRun(ctx, c, st, l.typeID, price, &gooOpen, hold) {
				continue
			}
			var open int
			_ = m.db.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM rolebot_corp_orders WHERE corporationID = ? AND stationID = ? AND typeID = ? AND bid = 0`,
				c.CorpID, st, l.typeID).Scan(&open)
			if open > 0 {
				continue
			}
			if placer == 0 {
				return
			}
			out, err := evedb.PlaceSellOrder(ctx, m.db, placer, st, l.typeID, l.item, price, l.qty, -1, 1, 30, true)
			if err != nil || out == nil || out.ErrorMsg != "" {
				continue
			}
			filled := 0.0
			for _, f := range out.Fills {
				filled += f.Price * float64(f.Quantity)
			}
			if filled > 0 {
				m.corpBook(ctx, c.CorpID, lineSales, filled, l.typeID, fmt.Sprintf("%d× %s", out.FilledQty, m.typeNameOr(ctx, l.typeID)))
			}
			if out.RestingOrderID != 0 {
				_, _ = m.db.ExecContext(ctx, `
					INSERT INTO rolebot_corp_orders (orderID, corporationID, stationID, typeID, bid, price, volSeen, placedAt, placedBy)
					VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)`, out.RestingOrderID, c.CorpID, st, l.typeID, price,
					l.qty-out.FilledQty, clock.Unix(), placer)
			}
			m.ceoLog(ctx, c, powerStarbase, "list", l.typeID, price*float64(l.qty),
				"listed %d× %s from its towers at %s, %s each", l.qty, m.typeNameOr(ctx, l.typeID),
				m.stationLabel(ctx, st), iskString(price))
		}
	}
}

// towerGoodsAskUnderBid is how far under the region's best bid a tower's goo is listed: room
// for a trader to carry it to that bidder and still clear the sales tax and a margin.
const towerGoodsAskUnderBid = 0.92

// towerGoodsAsk is what a tower's goods are listed at: a hair under the region's cheapest ask,
// never under 90% of the type's value, and never under towerGoodsAskUnderBid of the region's
// best bid. Pure.
func towerGoodsAsk(regionAsk, value, regionBid float64) float64 {
	price := regionAsk * 0.99
	price = math.Max(price, value*0.9)
	price = math.Max(price, regionBid*towerGoodsAskUnderBid)
	return math.Floor(price*100) / 100
}

// regionBestBid is the best live bid for a type anywhere in a region, 0 for
// none.
func (m *Mod) regionBestBid(ctx context.Context, typeID, regionID int64) float64 {
	if regionID == 0 {
		return 0
	}
	var p float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(price), 0) FROM market_orders
		 WHERE regionID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0`,
		regionID, typeID).Scan(&p)
	return p
}

// ── T2 ──────────────────────────────────────────────────────────────────────

// componentInputs is every component a T2 print's product is built from.
func (m *Mod) componentInputs(ctx context.Context, bpTypeID int64) []int64 {
	ids, _ := int64Column(ctx, m, `
		SELECT m.requiredTypeID FROM tl2materialsfortypewithactivity m
		  JOIN invTypes t ON t.typeID = m.requiredTypeID
		 WHERE m.typeID = ? AND m.activity = 1 AND t.groupID = ?`, bpTypeID, componentGroup)
	return ids
}

// chainWalk is the T2 chain under a set of materials: every node reached (composites,
// intermediates and goo) and the goo at the bottom. A node's inputs are the inputs of the
// reactions that output it. Static data.
type chainWalk struct{ Nodes, Roots map[int64]bool }

func (m *Mod) walkChain(ctx context.Context, frontier []int64, depth int) chainWalk {
	w := chainWalk{Nodes: map[int64]bool{}, Roots: map[int64]bool{}}
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []int64
		for _, t := range frontier {
			if w.Nodes[t] {
				continue
			}
			w.Nodes[t] = true
			if m.typeGroupOf(ctx, t) == moonMaterialGroup {
				w.Roots[t] = true
				continue
			}
			// The reactions that output it: their inputs are one step down.
			ins, _ := int64Column(ctx, m, `
				SELECT r2.typeID FROM invtypereactions r1
				  JOIN invtypereactions r2 ON r2.reactionTypeID = r1.reactionTypeID AND r2.input = 1
				 WHERE r1.typeID = ? AND r1.input = 0`, t)
			next = append(next, ins...)
		}
		frontier = next
	}
	return w
}

// chainNodes is every node of a T2 print's chain (its components' composites, the
// intermediates that make those, and the goo under them) and gooRoots the goo alone.
// Cached: static data.
var gooRootCache sync.Map // bpTypeID to chainWalk

func (m *Mod) printChain(ctx context.Context, bpTypeID int64) chainWalk {
	if v, ok := gooRootCache.Load(bpTypeID); ok {
		return v.(chainWalk)
	}
	frontier := []int64{}
	for _, comp := range m.componentInputs(ctx, bpTypeID) {
		var compBP int64
		_ = m.db.QueryRowContext(ctx, `SELECT blueprintTypeID FROM invBlueprintTypes WHERE productTypeID = ? LIMIT 1`, comp).Scan(&compBP)
		if compBP == 0 {
			continue
		}
		mats, _ := int64Column(ctx, m, `
			SELECT requiredTypeID FROM tl2materialsfortypewithactivity WHERE typeID = ? AND activity = 1`, compBP)
		frontier = append(frontier, mats...)
	}
	w := m.walkChain(ctx, frontier, 4)
	gooRootCache.Store(bpTypeID, w)
	return w
}

func (m *Mod) chainNodes(ctx context.Context, bpTypeID int64) map[int64]bool {
	return m.printChain(ctx, bpTypeID).Nodes
}

func (m *Mod) gooRoots(ctx context.Context, bpTypeID int64) map[int64]bool {
	return m.printChain(ctx, bpTypeID).Roots
}

// compositeChain is every node of the chain under one composite, the
// composite itself included. Cached: static data.
var compositeChainCache sync.Map // compositeTypeID to map[int64]bool

func (m *Mod) compositeChain(ctx context.Context, compositeTypeID int64) map[int64]bool {
	if v, ok := compositeChainCache.Load(compositeTypeID); ok {
		return v.(map[int64]bool)
	}
	nodes := m.walkChain(ctx, []int64{compositeTypeID}, 4).Nodes
	compositeChainCache.Store(compositeTypeID, nodes)
	return nodes
}

// corpHoldsGooFor is whether the corporation harvests (or reacts) anything a T2 print's chain
// bottoms out in: the plan's goo test.
func (m *Mod) corpHoldsGooFor(ctx context.Context, plans []*towerPlan, bpTypeID int64) bool {
	roots := m.gooRoots(ctx, bpTypeID)
	if len(roots) == 0 {
		return false
	}
	for mat := range m.producedBy(ctx, plans) {
		if roots[mat] {
			return true
		}
	}
	return false
}

// corpHoldsT2Prints is whether the library holds any T2 original.
func (m *Mod) corpHoldsT2Prints(ctx context.Context, corpID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_library l JOIN invBlueprintTypes bt ON bt.blueprintTypeID = l.bpTypeID
		 WHERE l.corporationID = ? AND bt.techLevel = 2`, corpID).Scan(&n)
	return n > 0
}

// corpBuyMemberT2 makes a member holding a T2 original at the factory an offer for it, only
// for a print whose components the corporation's own goo makes, and pays from the corporation
// wallet when the member accepts. A human member is told of the offer in the CEO's log and
// bulletin; nothing moves a human's print without them.
func (m *Mod) corpBuyMemberT2(ctx context.Context, c *corpCEO, ch Charter, plans []*towerPlan) {
	if len(m.harvestedBy(plans)) == 0 || clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "buyt2") < t2OfferEverySec {
		return
	}
	stationID, officeID := m.corpFactoryOffice(ctx, c.CorpID)
	if stationID == 0 {
		return
	}
	// A type the library lacks, or a second original of a bottleneck type
	// (corpbottleneck.go).
	wants, wantArgs := libraryWantsTypeSQL(c.CorpID)
	args := append([]any{c.CorpID, stationID}, wantArgs...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID FROM entity e
		  JOIN character_ ch ON ch.characterID = e.ownerID AND ch.corporationID = ?
		  JOIN rolebot_characters b ON b.characterID = e.ownerID AND b.enabled = 1
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID AND bt.techLevel = 2
		  LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
		 WHERE e.locationID = ? AND e.flag = 4 AND COALESCE(bp.copy, 0) = 0 AND `+wants+`
		 LIMIT 10`, args...)
	if err != nil {
		return
	}
	type cand struct{ item, typeID, owner int64 }
	var cands []cand
	for rows.Next() {
		var cd cand
		if rows.Scan(&cd.item, &cd.typeID, &cd.owner) == nil {
			cands = append(cands, cd)
		}
	}
	rows.Close()
	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return
	}
	for _, cd := range cands {
		if !m.corpHoldsGooFor(ctx, plans, cd.typeID) {
			continue
		}
		value := evedb.TypeValueOf(ctx, m.db, cd.typeID).Value
		offer := math.Ceil(value * t2OfferPremium)
		if offer <= 0 || offer > free*ch.CapitalRisk {
			continue
		}
		traits, _ := getTraits(ctx, m.db, cd.owner)
		tn := resolveTuning(traits)
		// A loyal member sells at the offer; a mercenary only when it pays
		// over what it would ask on its own.
		if roll01(cd.owner, cd.item, clock.Unix()/86400) > 0.5+0.4*tn.Obedience {
			m.ceoLog(ctx, c, powerProduction, "buyt2", cd.owner, offer,
				"offered %s %s for its %s; it declined", m.charNameOr(ctx, cd.owner), iskString(offer), m.typeNameOr(ctx, cd.typeID))
			continue
		}
		if _, bal, err := m.corpWithdraw(ctx, c.CorpID, cd.owner, offer,
			"Purchase of a Tech II blueprint for the corporation"); err != nil {
			continue
		} else {
			m.pushBalance(cd.owner, bal)
		}
		if err := m.fileIntoLibrary(ctx, c.CorpID, officeID, stationID, cd.owner, cd.item, "purchase", offer); err != nil {
			continue
		}
		m.corpBook(ctx, c.CorpID, lineBPO, -offer, cd.typeID, "Tech II print from a member: "+m.typeNameOr(ctx, cd.typeID))
		m.ceoLog(ctx, c, powerProduction, "buyt2", cd.owner, offer,
			"bought %s's %s for %s — the corporation's own goo makes its components",
			m.charNameOr(ctx, cd.owner), m.typeNameOr(ctx, cd.typeID), iskString(offer))
		return // one a cycle
	}
}

// requestComponentPrints buys, one at a time, the component blueprints the
// library's T2 prints need (NPC-sold), through the library's purchase orders.
func (m *Mod) requestComponentPrints(ctx context.Context, c *corpCEO, plans []*towerPlan) {
	if !m.corpHoldsT2Prints(ctx, c.CorpID) || clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "po") < componentPOEvery {
		return
	}
	stationID, _ := m.corpFactoryOffice(ctx, c.CorpID)
	if stationID == 0 {
		return
	}
	var open int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_corp_po WHERE corporationID = ? AND status = 'open'`, c.CorpID).Scan(&open)
	if open >= poMaxOpen {
		return
	}
	prints, _ := int64Column(ctx, m, `
		SELECT l.bpTypeID FROM rolebot_corp_library l JOIN invBlueprintTypes bt ON bt.blueprintTypeID = l.bpTypeID
		 WHERE l.corporationID = ? AND bt.techLevel = 2`, c.CorpID)
	p, _ := m.loadCorpPolicy(ctx, c.CorpID)
	buyer := p.DeputyID
	if buyer == 0 {
		return
	}
	for _, bp := range prints {
		for _, comp := range m.componentInputs(ctx, bp) {
			var compBP int64
			_ = m.db.QueryRowContext(ctx, `SELECT blueprintTypeID FROM invBlueprintTypes WHERE productTypeID = ? LIMIT 1`, comp).Scan(&compBP)
			if compBP == 0 || m.libraryHas(ctx, c.CorpID, compBP) {
				continue
			}
			value := evedb.TypeValueOf(ctx, m.db, compBP).Value
			if value <= 0 {
				continue
			}
			if err := m.postPO(ctx, c, buyer, compBP, stationID, value, false); err == nil {
				return
			}
		}
	}
}

// factoryWants is whether a library T2 or component print's bill uses a type: tower output of
// it is then filed into Materials rather than sold.
func (m *Mod) factoryWants(ctx context.Context, corpID, typeID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_library l
		  JOIN tl2materialsfortypewithactivity mm ON mm.typeID = l.bpTypeID AND mm.activity = 1
		 WHERE l.corporationID = ? AND mm.requiredTypeID = ?`, corpID, typeID).Scan(&n)
	return n > 0
}

// feedFactoryMaterials files what the factory's prints need out of the
// Production division and the staging offices into the factory's Materials:
// components the corporation built, composites its hub reacted.
func (m *Mod) feedFactoryMaterials(ctx context.Context, c *corpCEO) {
	stationID, officeID := m.corpFactoryOffice(ctx, c.CorpID)
	if officeID == 0 {
		return
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity) FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = ? AND t.groupID IN (?, ?, ?, ?)
		 GROUP BY e.typeID`, c.CorpID, officeID, corpDivProduct, componentGroup, compositeGroup,
		intermediateGroup, moonMaterialGroup)
	if err != nil {
		return
	}
	type stack struct{ typeID, qty int64 }
	var stacks []stack
	for rows.Next() {
		var s stack
		if rows.Scan(&s.typeID, &s.qty) == nil {
			stacks = append(stacks, s)
		}
	}
	rows.Close()
	for _, s := range stacks {
		if !m.factoryWants(ctx, c.CorpID, s.typeID) {
			continue
		}
		if n, err := m.moveUnits(ctx, c.CorpID, officeID, corpDivProduct, s.typeID, s.qty, officeID, corpDivMaterials, c.CorpID); err == nil && n > 0 {
			m.ceoLog(ctx, c, powerProduction, "feed", s.typeID, 0,
				"filed %d× %s into the factory's Materials for its Tech II line at %s", n, m.typeNameOr(ctx, s.typeID),
				m.stationLabel(ctx, stationID))
		}
	}
}
