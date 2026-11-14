package rolebots

import (
	"context"
	"fmt"
)

// A half-built ship may fly for a job that gets shot at only if each band the plan uses is at
// least half filled and the fitted part reaches a share of the planned fit's power. A refusal
// takes the ordinary no-fit path. Other jobs only need fitted power above zero.

const (
	// shortFitMinBandShare is the share of a band's planned modules that must be fitted.
	shortFitMinBandShare = 0.5
	// shortFitMinPowerShare is the share of the planned fit's job power the fitted part must
	// reach. A rack of only guns passes the band rule, so strength is read too.
	shortFitMinPowerShare = 0.6
)

// shortFitStrictJob reports whether a job needs the whole rack fitted before it flies.
func shortFitStrictJob(job string) bool {
	switch job {
	case JobCombat, JobPiracy, JobTackle, JobEwar, JobScout:
		return true
	}
	return false
}

// shortFitBandHole returns the first band (high, mid, low) whose fitted share is under
// shortFitMinBandShare, given the planned and missing module counts per band. Pure.
func shortFitBandHole(planned, missing map[int64]int) (band int64, filled, want int, hole bool) {
	for _, b := range []int64{flagHiFirst, flagMedFirst, flagLoFirst} {
		p := planned[b]
		if p <= 0 {
			continue
		}
		f := p - missing[b]
		if f < 0 {
			f = 0
		}
		if float64(f) < shortFitMinBandShare*float64(p) {
			return b, f, p, true
		}
	}
	return 0, 0, 0, false
}

// shortFitVerdict reports whether a planned fit with some lines still missing may fly as it
// is, and gives the log reason when it may not. Charges that share a slot flag are skipped.
func (m *Mod) shortFitVerdict(ctx context.Context, planned *fitTemplate, missing []ShopItem) (bool, string) {
	if planned == nil || planned.Fit == nil {
		return false, "no fit to judge"
	}
	fitted := m.fittedPower(ctx, planned, missing)
	if fitted <= 0 {
		return false, "without them it cannot do the job"
	}
	if !shortFitStrictJob(planned.Job) {
		return true, ""
	}
	mods, _ := m.allModulesByID(ctx)
	plannedBands := map[int64]int{}
	for _, mod := range planned.Fit.Modules {
		if mods != nil && mods[mod.TypeID] == nil {
			continue // a charge riding a gun's flag, not a module
		}
		plannedBands[bandOf(mod.Flag)]++
	}
	missingBands := map[int64]int{}
	for _, it := range missing {
		if it.Dest == "module" {
			missingBands[bandOf(it.Flag)]++
		}
	}
	if b, f, p, hole := shortFitBandHole(plannedBands, missingBands); hole {
		return false, fmt.Sprintf("only %d of %d %s modules fitted", f, p, slotBandName(b))
	}
	full, err := m.fitPower(ctx, planned)
	if err == nil && full > 0 && fitted < shortFitMinPowerShare*full {
		return false, fmt.Sprintf("it flies at %.0f%% of its fit's strength", 100*fitted/full)
	}
	return true, ""
}

// plannedFromShip rebuilds a procurement trip's plan: the modules on the ship plus the missing
// module lines. procureAbandon uses it. FitID is zeroed so the power cache ignores it.
func plannedFromShip(ship *fitTemplate, missing []ShopItem) *fitTemplate {
	if ship == nil || ship.Fit == nil {
		return nil
	}
	// A line aimed at an occupied flag replaces its occupant, so the occupant is not in the plan.
	target := map[int64]bool{}
	for _, it := range missing {
		if it.Dest == "module" {
			target[it.Flag] = true
		}
	}
	fit := *ship.Fit
	fit.Modules = nil
	for _, mod := range ship.Fit.Modules {
		if !target[mod.Flag] {
			fit.Modules = append(fit.Modules, mod)
		}
	}
	for _, it := range missing {
		if it.Dest == "module" {
			fit.Modules = append(fit.Modules, FitModule{TypeID: it.TypeID, TypeName: it.Name, Flag: it.Flag})
		}
	}
	out := *ship
	out.Fit = &fit
	out.FitID = 0
	return &out
}
