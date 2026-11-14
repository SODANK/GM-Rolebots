package rolebots

import (
	"context"
	"sort"
)

// Fitting-capacity modules: Reactor Control Unit (+10% powergrid), Co-Processor (+7% CPU), Power
// Diagnostic System and Auxiliary Power Core (+10 MW flat). ComputeFitStats and the server credit
// them (db.GetShipCPUPGCapacityFitted). They are not a wish role: highs are placed first and lows
// last, so the grid is spent before a low is decided. Instead generateFitFor runs a second pass:
// the first pass notes when the grid refused a better module (fitState.noteGridRefusal), then one
// low is reserved for a grid module and the fit rebuilt. The rebuilt fit wins only if the job's
// power rose.
//
// Group 57 also holds capacitor relays, flux coils and the Shield Power Relay (which slows cap
// recharge), none with a grid attribute. isGridModule reads the credit, never the group alone.

const (
	grpPowerManager   = 57  // Reactor Control, Power Diagnostic, cap relays, flux coils
	grpCPUEnhancer    = 285 // Co-Processor
	grpAuxPowerCore   = 339 // Auxiliary Power Core
	gridReserveMargin = 1.02

	// roleGridMod is the role a reserved grid module is recorded under. It is in no roleGroups entry
	// and no wish, so nothing can pick it; it only names what fitState.add just counted.
	roleGridMod modRole = "gridmod"
)

// gridCredit is what a set of grid modules does to a hull's output, as the server folds it: CPU
// and powergrid multiply with no stacking penalty (db.GetShipPassiveBonuses), and an Auxiliary
// Power Core adds flat MW after the multiply.
type gridCredit struct {
	cpuMult, pgMult, pgAdd float64
}

func identityGrid() gridCredit { return gridCredit{cpuMult: 1, pgMult: 1} }

// moduleGridCredit reads one module's grid effect off its attributes: 202 CPU
// output multiplier, 145 powergrid output multiplier, 549 flat powergrid. ok is
// false when it carries none of them, which is every capacitor relay and flux
// coil in group 57.
func moduleGridCredit(attrs map[int64]float64) (c gridCredit, ok bool) {
	c = identityGrid()
	if v := attrs[attrCPUMultMod]; v > 1 {
		c.cpuMult, ok = v, true
	}
	if v := attrs[attrPGMult]; v > 1 {
		c.pgMult, ok = v, true
	}
	if v := attrs[attrPGAddFlat]; v > 0 {
		c.pgAdd, ok = v, true
	}
	return c, ok
}

// plus folds another module's credit in the way the server does.
func (g gridCredit) plus(o gridCredit) gridCredit {
	return gridCredit{cpuMult: g.cpuMult * o.cpuMult, pgMult: g.pgMult * o.pgMult, pgAdd: g.pgAdd + o.pgAdd}
}

// apply turns a skilled hull's CPU and powergrid into what the fitted modules
// make of it.
func (g gridCredit) apply(cpu, pg float64) (float64, float64) {
	return cpu * g.cpuMult, pg*g.pgMult + g.pgAdd
}

// isGridModule reports whether a catalogue module is a lows-slot grid module:
// low slot, one of the three groups, and a real grid credit.
func isGridModule(mm *marketModule) bool {
	if mm == nil || mm.Slot != flagLoFirst {
		return false
	}
	switch mm.GroupID {
	case grpPowerManager, grpCPUEnhancer, grpAuxPowerCore:
		_, ok := moduleGridCredit(mm.Attrs)
		return ok
	}
	return false
}

// gridRefusal records what the grid cost a generation: which of the two budgets
// refused a candidate that would have beaten what its slot got.
type gridRefusal struct {
	cpu, pg bool
}

func (r gridRefusal) any() bool { return r.cpu || r.pg }

// gridRefusalMargin is how much better the refused module must have scored than
// the pick before the grid is blamed. Under it the pick was simply a fair
// alternative and the slot was not short of anything.
const gridRefusalMargin = 1.10

// noteGridRefusal is called by bestForWish after it has scanned one role.
// refusedRaw is the best raw score among candidates refused for CPU or
// powergrid; bestRaw is the pick's, and picked says there was one. Nil-safe, so
// callers without a fit state (tests) need no guard.
func (fs *fitState) noteGridRefusal(refusedRaw, bestRaw float64, picked, refusedCPU, refusedPG bool) {
	if fs == nil || refusedRaw <= 0 {
		return
	}
	if picked && refusedRaw <= bestRaw*gridRefusalMargin {
		return
	}
	fs.gridRefused.cpu = fs.gridRefused.cpu || refusedCPU
	fs.gridRefused.pg = fs.gridRefused.pg || refusedPG
}

// gridReserveCandidates ranks the grid modules worth trying for a refusal, best first. Pure.
// A module's worth is the share of the refusing budget it adds, less the share of the other budget
// it spends. Shares are of the hull's own output because MW and CPU do not compare. A module that
// adds nothing to the refused budget, or costs more than it gives, is not offered.
func gridReserveCandidates(cat []*marketModule, cpuCap, pgCap float64, r gridRefusal) []*marketModule {
	if !r.any() || cpuCap <= 0 || pgCap <= 0 {
		return nil
	}
	type ranked struct {
		mm    *marketModule
		worth float64
	}
	var out []ranked
	for _, mm := range cat {
		if !isGridModule(mm) {
			continue
		}
		c, _ := moduleGridCredit(mm.Attrs)
		cpuGain := (c.cpuMult - 1)
		pgGain := (c.pgMult - 1) + c.pgAdd/pgCap
		worth := 0.0
		if r.cpu {
			worth += cpuGain
		}
		if r.pg {
			worth += pgGain
		}
		worth -= mm.CPU/cpuCap + mm.PG/pgCap
		if worth <= 0 || mm.CPU > cpuCap || mm.PG > pgCap {
			continue
		}
		out = append(out, ranked{mm, worth})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].worth > out[b].worth })
	res := make([]*marketModule, 0, len(out))
	for _, o := range out {
		res = append(res, o.mm)
	}
	return res
}

// gridReserveTries is how many distinct candidates the second pass builds a fit for. Each costs a
// full generation plus two stat readings.
const gridReserveTries = 2

// generateFitFor builds the best fit this pilot can assemble on one hull for one job. When the
// grid held a better module out, it retries with a low reserved for a fitting-capacity module (see
// the top-of-file comment) and keeps the retry only if it scores higher.
func (m *Mod) generateFitFor(ctx context.Context, charID int64, job string, h *HullProfile,
	skills map[int64]int, cat []*marketModule, dcat []*marketDrone, own *ownedLoadout,
	reach map[int64]bool, taste fitTaste) (*fitTemplate, []wantGap, error) {

	t, gaps, refusal, err := m.generateFitForOnce(ctx, charID, job, h, skills, cat, dcat, own, reach, taste, nil)
	if err != nil || t == nil || !refusal.any() || h == nil || h.LoSlots < 1 {
		return t, gaps, err
	}
	cpuCap := h.CPU * (1 + fittingSkillBonusPerLevel*float64(skills[skillElectronics]))
	pgCap := h.PG * (1 + fittingSkillBonusPerLevel*float64(skills[skillEngineering]))
	cands := gridReserveCandidates(cat, cpuCap, pgCap, refusal)
	if len(cands) == 0 {
		return t, gaps, nil
	}
	baseStats, serr := m.ComputeFitStats(ctx, t.Fit)
	if serr != nil || baseStats == nil {
		return t, gaps, nil
	}
	bestT, bestGaps, bestPower := t, gaps, jobPower(job, baseStats)
	tried := 0
	for _, c := range cands {
		if tried >= gridReserveTries {
			break
		}
		if ok, perr := m.pilotCanUse(ctx, c.TypeID, skills); perr != nil || !ok {
			continue
		}
		tried++
		alt, altGaps, _, aerr := m.generateFitForOnce(ctx, charID, job, h, skills, cat, dcat, own, reach, taste, c)
		if aerr != nil || alt == nil {
			continue
		}
		altStats, serr := m.ComputeFitStats(ctx, alt.Fit)
		if serr != nil || altStats == nil {
			continue
		}
		if p := jobPower(job, altStats); p > bestPower*gridReserveMargin {
			bestT, bestGaps, bestPower = alt, altGaps, p
		}
	}
	return bestT, bestGaps, nil
}
