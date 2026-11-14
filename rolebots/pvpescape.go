package rolebots

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

// A ship cannot leave on the tick it is shot: it must react, align and spool
// its drive, which takes seconds that depend on the hull (alignSecByClass,
// hulltags.go). This file is that escape clock, plus the arithmetic the target
// pick uses to ask whether a patrol can kill a target before it is gone.

// ── dials ───────────────────────────────────────────────────────────────────

// pvpDial is one numeric knob with an explicit default, so an operator can set
// a real 0. The zero-means-unset sentinel of the integer caches cannot express that.
type pvpDial struct {
	key       string
	def       float64
	lo, hi    float64
	label     string  // the panel's caption
	step      float64 // the panel's input step
	bits      atomic.Uint64
	explicitV atomic.Bool
}

func (d *pvpDial) Get() float64 {
	if !d.explicitV.Load() {
		return d.def
	}
	return math.Float64frombits(d.bits.Load())
}

func (d *pvpDial) Set(v float64) {
	d.bits.Store(math.Float64bits(clampRange(v, d.lo, d.hi)))
	d.explicitV.Store(true)
}

var (
	// dialLethality is the master switch for this file and the kill-time target
	// pick. 0 uses the margin arithmetic only.
	dialLethality = &pvpDial{key: "rolebots.botPvP.lethality", def: 1, lo: 0, hi: 1, step: 1,
		label: "Lethality master switch (1 = on, 0 = the old fight arithmetic)"}
	// dialReactSec and dialWarpSpoolSec are the two fixed costs of leaving: the
	// pilot noticing, and the drive spooling once the ship points the right way.
	dialReactSec = &pvpDial{key: "rolebots.botPvP.reactSec", def: 2, lo: 0, hi: 30, step: 0.5,
		label: "Seconds to react before a target can leave"}
	dialWarpSpoolSec = &pvpDial{key: "rolebots.botPvP.warpSpoolSec", def: 2, lo: 0, hi: 30, step: 0.5,
		label: "Seconds for the drive to spool"}
	// dialAlignScale multiplies the hull's align time.
	dialAlignScale = &pvpDial{key: "rolebots.botPvP.alignScale", def: 1, lo: 0, hi: 5, step: 0.1,
		label: "Align-time multiplier"}
	// dialHeldEscape multiplies a held target's per-tick escape chance
	// (botPvPTackledFactor when lethality is off).
	dialHeldEscape = &pvpDial{key: "rolebots.botPvP.heldEscapeFactor", def: 0.04, lo: 0.001, hi: 1, step: 0.005,
		label: "Escape chance of a held target (x base)"}
	// dialMinKillOdds is the line a patrol needs to clear to start a fight.
	dialMinKillOdds = &pvpDial{key: "rolebots.botPvP.minKillOdds", def: 0.35, lo: 0, hi: 1, step: 0.05,
		label: "Kill odds a patrol needs to start a fight"}
	// dialWarMinKillOdds is the same line for a target the patrol's employer is
	// at war with. It never raises the line above minKillOdds.
	dialWarMinKillOdds = &pvpDial{key: "rolebots.botPvP.warMinKillOdds", def: 0.20, lo: 0, hi: 1, step: 0.05,
		label: "Kill odds a patrol needs to start a fight with a war enemy"}
	// dialWinMargin is the engageMargin a patrol needs against a target that
	// will stand and shoot back.
	dialWinMargin = &pvpDial{key: "rolebots.botPvP.winMargin", def: 1.0, lo: 0.1, hi: 10, step: 0.1,
		label: "Margin needed against a target that stands"}
	// dialMaxEngagements is the per-pass engagement cap.
	dialMaxEngagements = &pvpDial{key: "rolebots.botPvP.maxEngagements", def: botPvPMaxEngagements, lo: 1, hi: 64, step: 1,
		label: "Engagements per two-minute pass"}
	// dialPvPAmbush is the catch chance for a patrol whose whole trip is a hunt.
	dialPvPAmbush = &pvpDial{key: "rolebots.botPvP.pvpAmbushBase", def: 0.8, lo: 0, hi: 1, step: 0.05,
		label: "Base chance a hunting patrol catches what it found"}
	// dialConcordRate switches the abstract CONCORD stopwatch (pvplaw.go): 0 is
	// off, anything else on. The delay itself is core's concord.responseDelaySec.
	dialConcordRate = &pvpDial{key: "rolebots.botPvP.concordRate", def: 1, lo: 0, hi: 1, step: 1,
		label: "Abstract CONCORD (1 = on, 0 = off; delay is concord.responseDelaySec)"}
	// dialLawBounty is the ISK a record is worth per point below zero.
	dialLawBounty = &pvpDial{key: "rolebots.botPvP.lawBountyPerSec", def: 5000, lo: 0, hi: 100000, step: 500,
		label: "Law bounty per whole security point of a record at -2.0 or worse (ISK, 0 = off)"}
	// dialPackBudget is how many PvP packs the fleet formation sweep may raise per
	// pass, on top of its ordinary budget (gangform.go).
	dialPackBudget = &pvpDial{key: "rolebots.botPvP.packBudget", def: 1, lo: 0, hi: 8, step: 1,
		label: "PvP packs raised per formation pass (0 = none)"}
)

// pvpDials is every dial in this file, for the loader and the panel.
func pvpDials() []*pvpDial {
	out := []*pvpDial{dialLethality, dialReactSec, dialWarpSpoolSec, dialAlignScale,
		dialHeldEscape, dialMinKillOdds, dialWarMinKillOdds, dialWinMargin, dialMaxEngagements, dialPvPAmbush,
		dialConcordRate, dialLawBounty, dialPackBudget, dialPvPLens, dialFleetDoctrine}
	out = append(out, gankDials()...)
	out = append(out, campDials()...)
	out = append(out, raidDials()...)
	out = append(out, safeDials()...)
	return append(out, dialLaneTraffic)
}

// loadPvPDials reads every dial at boot. Never fatal.
func (m *Mod) loadPvPDials(ctx context.Context) {
	for _, d := range pvpDials() {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, d.key)
		if err != nil {
			rlogf("rolebots: read %s: %v (using the default)", d.key, err)
			continue
		}
		if !ok {
			continue
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			d.Set(f)
		}
	}
}

// lethalityOn reports the master switch.
func lethalityOn() bool { return dialLethality.Get() >= 0.5 }

// heldEscapeFactor is the per-tick multiplier on a held target's escape roll.
func heldEscapeFactor() float64 {
	if !lethalityOn() {
		return botPvPTackledFactor
	}
	return dialHeldEscape.Get()
}

// ── the escape clock ────────────────────────────────────────────────────────

// escapeWindowSec is how long a hull needs to leave after deciding to: react,
// align, spool. Pure. An unread align time (<=0) gives 0, meaning no window.
func escapeWindowSec(alignSec, reactSec, spoolSec, scale float64) float64 {
	if alignSec <= 0 {
		return 0
	}
	if scale < 0 {
		scale = 0
	}
	return alignSec*scale + math.Max(reactSec, 0) + math.Max(spoolSec, 0)
}

// sideEscapeWindow is the window of the side as a whole: its slowest living
// member, because the escape roll is side-wide (as sideWarpCore reads the
// weakest core).
func sideEscapeWindow(side []*pvpCombatant) float64 {
	if !lethalityOn() {
		return 0
	}
	react, spool, scale := dialReactSec.Get(), dialWarpSpoolSec.Get(), dialAlignScale.Get()
	w := 0.0
	for _, c := range side {
		if c == nil || c.EHP <= 0 {
			continue
		}
		if cw := escapeWindowSec(c.AlignSec, react, spool, scale); cw > w {
			w = cw
		}
	}
	return w
}

// escapeTickChance is one tick's chance a fleeing side gets away, given how
// many ticks it has been trying. Pure. Zero inside the window; after it, the
// base chance, cut to heldFactor of itself when the other side's points beat
// the weakest warp core on this one.
func escapeTickChance(ticksFleeing int, window, enemyPoints, ownCore, base, heldFactor float64) float64 {
	if float64(ticksFleeing) < window {
		return 0
	}
	p := base
	if enemyPoints > ownCore {
		p *= heldFactor
	}
	return p
}

// ── the target pick ─────────────────────────────────────────────────────────

// expectedTTK is how long the guns need to kill hp hit points against a target
// mending itself at rep a second. +Inf when the repair outruns the damage.
func expectedTTK(dps, hp, rep float64) float64 {
	if hp <= 0 {
		return 0
	}
	net := dps - rep
	if net <= 0 {
		return math.Inf(1)
	}
	return hp / net
}

// killOdds is the chance the guns finish the target before it is gone: certain
// if they need no longer than the window, otherwise the chance it fails to
// leave in every tick the guns still need after that. Pure.
func killOdds(ttkSec, windowSec, perTick float64) float64 {
	if math.IsInf(ttkSec, 1) {
		return 0
	}
	if ttkSec <= windowSec {
		return 1
	}
	if perTick <= 0 {
		return 1
	}
	if perTick >= 1 {
		return 0
	}
	extra := math.Ceil(ttkSec - windowSec)
	return math.Pow(1-perTick, extra)
}

// engageFloor is the kill odds a patrol needs before it starts a fight. A war
// enemy gets the war floor, but never a stricter line than everybody else. Pure.
func engageFloor(atWar bool, base, warFloor float64) float64 {
	if atWar && warFloor < base {
		return warFloor
	}
	return base
}

// engageVerdict is the odds a patrol takes a kill if it starts this fight.
// A target that will not stand is judged on whether it can be caught: time to
// kill its weakest hull against its escape window and odds. A target that
// stands is judged on engageMargin, since it shoots back. Damage is applied
// (application.go): each side's guns on the other's weakest hull, the one
// resolveTeamFight focuses.
func engageVerdict(ours, theirs []*pvpCombatant, foeFights bool) float64 {
	var oDPS, oEHP, tDPS, tEHP, tRep float64
	weakestOf := func(side []*pvpCombatant) *pvpCombatant {
		var w *pvpCombatant
		for _, c := range side {
			if c != nil && c.EHP > 0 && (w == nil || c.EHP < w.EHP) {
				w = c
			}
		}
		return w
	}
	weakest, ourWeakest := weakestOf(theirs), weakestOf(ours)
	if weakest == nil {
		return 0
	}
	for _, c := range ours {
		if c != nil && c.EHP > 0 {
			oDPS += c.dpsOn(weakest)
			oEHP += c.EHP
		}
	}
	for _, c := range theirs {
		if c == nil || c.EHP <= 0 {
			continue
		}
		tDPS += c.dpsOn(ourWeakest)
		tEHP += c.EHP
		tRep += c.RepPerSec + c.SelfRepPerSec
	}
	if oDPS <= 0 {
		return 0
	}
	if foeFights {
		m := engageMargin(oDPS, oEHP, tDPS, tEHP)
		if hopeless(m) || m < dialWinMargin.Get() {
			return 0
		}
		return 1
	}
	perTick := botPvPEscapePerTick
	if sideTacklePoints(ours) > sideWarpCore(theirs) {
		perTick *= heldEscapeFactor()
	}
	return killOdds(expectedTTK(oDPS, weakest.EHP, tRep), sideEscapeWindow(theirs), perTick)
}

// pvpOutcomeLabel names how a team fight ended, for the result line.
func pvpOutcomeLabel(out pvpOutcome) string {
	switch {
	case len(out.BDead) > 0:
		return "killed"
	case len(out.ADead) > 0:
		return "lost"
	case out.BEscaped:
		return "escaped"
	}
	return "timeout"
}

