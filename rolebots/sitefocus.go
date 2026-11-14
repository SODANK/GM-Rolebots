package rolebots

import "context"

// ── focus fire ──────────────────────────────────────────────────────────────
//
// The duel does not fight a pack as one big hull. resolvePackDuel pours every
// rat's fire into weakestPackMember until it breaks off, so a pack's tank is
// its weakest hull's and only its guns pool. focusPower is the pair
// threatMargin reads, folded that way: the side's whole DPS against the weakest
// hull's hitpoints, stretched so that hull's break-off line (EHP - breakOff *
// MaxEHP) maps onto the margin scale siteWouldRun applies its finish rule to.
// A hull already under its break-off line folds to 0: it would align out on
// the first volley.

// hullPower is one hull's side of a fight: guns, hitpoints now and whole, and
// repair. SelfRep and RemoteRep are raw HP/s, capacitor-sustained
// (loadoutRepRates); PerHP is what one raw HP is worth here (ehpPerHP; 0 reads as 1).
type hullPower struct {
	DPS, EHP, MaxEHP   float64
	SelfRep, RemoteRep float64
	PerHP              float64
}

// focusRep is the repair put on the hull the rats focus, in that hull's EHP/s:
// its own booster or repairer plus every mate's remote repair (packRepairTick's
// rule, where the pooled remote goes to the most hurt hull). Its own remote
// repairer is left out: it cannot target itself.
func focusRep(hulls []hullPower) float64 {
	weakest := -1
	for i, h := range hulls {
		if h.EHP <= 0 || h.MaxEHP <= 0 {
			continue
		}
		if weakest < 0 || h.EHP < hulls[weakest].EHP {
			weakest = i
		}
	}
	if weakest < 0 {
		return 0
	}
	hp := hulls[weakest].SelfRep
	for i, h := range hulls {
		if i == weakest || h.EHP <= 0 || h.MaxEHP <= 0 {
			continue
		}
		hp += h.RemoteRep
	}
	per := hulls[weakest].PerHP
	if per <= 0 {
		per = 1
	}
	return hp * per
}

// siteSidePower is the triple every site gate reads: focusPower's pair plus
// focusRep.
func siteSidePower(hulls []hullPower, breakOff float64) (dps, ehp, rep float64, known bool) {
	dps, ehp, known = focusPower(hulls, breakOff)
	if !known {
		return 0, 0, 0, false
	}
	return dps, ehp, focusRep(hulls), true
}

// botHull is botPower hull-shaped, with the repair botPower does not carry:
// the full-hull reading of the boarded ship.
func (m *Mod) botHull(ctx context.Context, b *botRun) (hullPower, bool) {
	if b == nil || b.ShipID == 0 {
		return hullPower{}, false
	}
	c, err := m.botCombatant(ctx, b)
	if err != nil || c == nil || c.MaxEHP <= 0 {
		return hullPower{}, false
	}
	return hullPower{DPS: c.DPS, EHP: c.MaxEHP, MaxEHP: c.MaxEHP,
		SelfRep: c.SelfRepPerSec, RemoteRep: c.RepPerSec, PerHP: ehpPerHP(c)}, true
}

func focusPower(hulls []hullPower, breakOff float64) (dps, ehp float64, known bool) {
	if breakOff < 0 {
		breakOff = 0
	}
	if breakOff > 0.9 {
		breakOff = 0.9
	}
	weakest := -1
	for i, h := range hulls {
		if h.EHP <= 0 || h.MaxEHP <= 0 {
			continue
		}
		dps += h.DPS
		known = true
		if weakest < 0 || h.EHP < hulls[weakest].EHP {
			weakest = i
		}
	}
	if !known {
		return 0, 0, false
	}
	w := hulls[weakest]
	budget := w.EHP - breakOff*w.MaxEHP
	if budget < 0 {
		budget = 0
	}
	return dps, budget / (1 - breakOff), true
}

// effectiveHulls is effectivePower hull by hull: this pilot and every fighting
// gangmate wherever it is, each at full hull (a site search is about the ships,
// not their scratches).
func (m *Mod) effectiveHulls(ctx context.Context, b *botRun) []hullPower {
	if powerOverride != nil {
		if d, e, ok := powerOverride(b); ok {
			return []hullPower{{DPS: d, EHP: e, MaxEHP: e}}
		}
		return nil
	}
	var out []hullPower
	if h, ok := m.botHull(ctx, b); ok {
		out = append(out, h)
	}
	if b == nil || b.State == nil || b.State.Data.GangID == 0 {
		return out
	}
	mates, err := m.gangCombatRoster(ctx, b.State.Data.GangID, b.CharID)
	if err != nil {
		return out
	}
	for _, mate := range mates {
		if h, ok := m.botHull(ctx, mate); ok {
			out = append(out, h)
		}
	}
	return out
}

// packHulls is a prepared duel side as hulls, at their current hitpoints and
// with each hull's EHP over its landed fraction (prepareAbstractPack's standEHP
// rule), so the walk-away and the duel price suppression the same way.
func packHulls(pack []*packMember) []hullPower {
	out := make([]hullPower, 0, len(pack))
	for _, p := range pack {
		if p == nil || p.Bot == nil {
			continue
		}
		lf := p.LandedFrac
		if lf < 0.01 {
			lf = 0.01
		}
		// Hitpoints over the landed fraction, so one raw HP of repair is worth
		// the same stretch.
		out = append(out, hullPower{DPS: p.Bot.DPS, EHP: p.Bot.EHP / lf, MaxEHP: p.Bot.MaxEHP / lf,
			SelfRep: p.Bot.SelfRepPerSec, RemoteRep: p.Bot.RepPerSec, PerHP: ehpPerHP(p.Bot) / lf})
	}
	return out
}
