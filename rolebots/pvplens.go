package rolebots

import (
	"context"
	"math"
	"sync"

	"gurimoon/seclaw"
)

// Combat fits were scored as belt ratting, which never asks whether a ship can
// hold a pilot. JobPiracy is the lens that does: point first, then prop, then
// web, then the tank. The lens is a property of the pilot: its employer's
// doctrine, its appetite for violence and whether its corporation is at war.
// JobPvP aliases JobPiracy so the fit generators need no new job string.

// JobPvP is the lens a PvP-leaning combat pilot flies under.
const JobPvP = JobPiracy

const (
	// settingPvPLens is the switch. 0 gives everybody the gunboat lens.
	settingPvPLens = "rolebots.combat.pvpLens"
	// pvpLensLine is the lean at which a pilot changes lens.
	pvpLensLine = 0.5
	// pvpLensTTLSec caches the verdict per pilot, since the planner asks several
	// times a pass.
	pvpLensTTLSec = 10 * 60
)

var dialPvPLens = &pvpDial{key: settingPvPLens, def: 1, lo: 0, hi: 1, step: 1,
	label: "PvP fit lens for hunting pilots (1 = on, 0 = everybody keeps the gunboat lens)"}

func pvpLensOn() bool { return dialPvPLens.Get() >= 0.5 }

// PvPLean is how much of a doctrine's flying is about other pilots, 0..1.
// Defence is the field corporation's guard; Security patrols and hunts the
// outlaws; Mercenary and Predation go looking.
func (d Doctrine) PvPLean() float64 {
	switch d {
	case DoctrineSecurity:
		return 0.55
	case DoctrineMercenary:
		return 0.75
	case DoctrinePredation:
		return 0.85
	case DoctrineDefence:
		return 0.10
	}
	return 0
}

// pilotPvPLean folds the pilot into its employer's lean. Pure. The square root
// keeps a timid pilot of a hunting corporation from being written off, a war
// floors the lean, and a pacifist (violence under 0.1) is never moved.
func pilotPvPLean(violence float64, doc Doctrine, atWar bool) float64 {
	if violence < 0.1 {
		return 0
	}
	lean := doc.PvPLean() * math.Sqrt(math.Min(violence, 1.5))
	if atWar && doc != DoctrineNone && lean < 0.7 {
		lean = 0.7
	}
	return math.Max(0, math.Min(1, lean))
}

type pvpLensEntry struct {
	job string
	at  int64
}

var pvpLensCache sync.Map // charID -> pvpLensEntry

// jobForPilot is jobFor with the pilot in it: ActCombat and ActSecurity resolve
// to the PvP lens for a pilot that leans that way and can fly it (canWorkJob),
// otherwise to the gunboat lens. Hauling activities take the freight lens for
// an Infrastructure-arm pilot (freightlens.go). Other activities are jobFor's answer.
func (m *Mod) jobForPilot(ctx context.Context, charID int64, activity string) string {
	job := jobFor(activity)
	if freightActivity(activity) {
		if m.freightLensFor(ctx, charID) {
			return freightJobFor(job, activity, true, true)
		}
		return job
	}
	if job != JobCombat || (activity != ActCombat && activity != ActSecurity) || !pvpLensOn() {
		return job
	}
	now := clock.Unix()
	if v, ok := pvpLensCache.Load(charID); ok {
		if e := v.(pvpLensEntry); now-e.at < pvpLensTTLSec {
			return e.job
		}
	}
	chosen := JobCombat
	if b, err := m.loadRun(ctx, charID); err == nil {
		doc := m.corpDoctrineFor(ctx, b.CorpID)
		// An outlaw's combat is rat-killing, the only thing that raises the record,
		// and the PvP gear is useless against rats, so it flies the gunboat.
		if b.SecStatus <= seclaw.OutlawStatus {
			pvpLensCache.Store(charID, pvpLensEntry{job: JobCombat, at: now})
			return JobCombat
		}
		if pilotPvPLean(b.Tune.ViolenceAppetite, doc, relCorpAtWar(b.CorpID)) >= pvpLensLine {
			if ok, err := m.canWorkJob(ctx, charID, JobPvP); err == nil && ok {
				chosen = JobPvP
			}
		}
	}
	pvpLensCache.Store(charID, pvpLensEntry{job: chosen, at: now})
	return chosen
}

func init() { registerCharForget("pvpLensCache", func(id int64) { pvpLensCache.Delete(id) }) }
