package rolebots

// Two roster-wide levers over the skill planner, set from the Bot Population Skills page and
// stored in server_settings.
// Reconsider sweep: one timestamp, replanEpoch. duePlanners treats any pilot whose last plan
// predates the stamp as due, and the lane drains them at its own pace. Nothing is interrupted
// or deleted, and plannedAt is left alone so the freshness panel stays true. The stamp is
// cached in memory because duePlanners runs every pass.
// Plan horizon: how much training one plan lays down, the centre of the personality bend.
// Patience spreads pilots around it at fixed ratios (3.5x and 0.25x). It does not move
// skillMaxPlanSteps, which caps a plan at 24 steps whatever the horizon.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

const (
	// settingReplanEpoch is the reconsider stamp: plans built before this unix second are
	// treated as due. 0 (the default) means no sweep has been asked for.
	settingReplanEpoch = "rolebots.skills.replanEpoch"
	// settingPlanHorizon is the plan horizon in minutes, the centre of the patience bend.
	// Absent means the shipped default.
	settingPlanHorizon = "rolebots.skills.planHorizonMin"

	// The ends of the patience bend, as multiples of the centre. At the shipped two-day centre
	// they are seven days and twelve hours.
	skillHorizonPatientMul = 3.5
	skillHorizonHurriedMul = 0.25

	// What the horizon may be set to: an hour at the bottom (a shorter plan is under the replan
	// interval and the lane never catches up) to thirty days at the top.
	skillHorizonMinMinutes = 60.0
	skillHorizonMaxMinutes = 30 * 24 * 60.0

	// settingRefineFloor is a Refining level every pilot is forced to reach; 0 (the default) is
	// off. Refining V alone clears the refine-worth bar (1+0.02R)(1+0.04E) >= 1.0877 and
	// Refining IV does not, and Refining is rank 1, so the level is cheap to buy.
	settingRefineFloor = "rolebots.skills.refineFloor"

	// The floor is a skill level, so the range is a skill level.
	refineFloorMaxLevel = 5
)

var (
	// replanEpochValue is the cached stamp, read by duePlanners every pass.
	replanEpochValue atomic.Int64
	// planHorizonValue is the cached horizon in minutes, read by
	// resolveTuning for every pilot on every plan.
	planHorizonValue atomic.Int64
	// refineFloorValue is the forced Refining level, read by goals() for
	// every pilot on every plan. 0 is off.
	refineFloorValue atomic.Int64
)

// replanEpoch reports the reconsider stamp (0 = never asked).
func replanEpoch() int64 { return replanEpochValue.Load() }

// skillPlanHorizonMin reports the current plan horizon in minutes: the centre of the
// patience bend, and defaultTuning's value.
func skillPlanHorizonMin() float64 {
	if v := planHorizonValue.Load(); v > 0 {
		return float64(v)
	}
	return skillPlanHorizonMinDefault
}

// refineSkillFloor reports the forced Refining level (0 = off).
func refineSkillFloor() int { return int(refineFloorValue.Load()) }

// clampRefineFloor holds a requested floor inside 0..5.
func clampRefineFloor(level int) int {
	if level < 0 {
		return 0
	}
	if level > refineFloorMaxLevel {
		return refineFloorMaxLevel
	}
	return level
}

// clampPlanHorizon holds a requested horizon inside the range above.
func clampPlanHorizon(min float64) float64 {
	if min < skillHorizonMinMinutes {
		return skillHorizonMinMinutes
	}
	if min > skillHorizonMaxMinutes {
		return skillHorizonMaxMinutes
	}
	return min
}

// loadSkillSweepSettings reads the three settings at boot. A missing or unreadable one
// leaves the shipped behaviour, since a bad dial must not stop the module.
func (m *Mod) loadSkillSweepSettings(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingReplanEpoch); err != nil {
		rlogf("rolebots: read replan stamp: %v (ignoring it)", err)
	} else if ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n > 0 {
			replanEpochValue.Store(n)
		}
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingRefineFloor); err != nil {
		rlogf("rolebots: read refining floor: %v (leaving it off)", err)
	} else if ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			refineFloorValue.Store(int64(clampRefineFloor(n)))
		}
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingPlanHorizon); err != nil {
		rlogf("rolebots: read plan horizon: %v (using the default)", err)
	} else if ok {
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && n > 0 {
			planHorizonValue.Store(int64(clampPlanHorizon(n)))
		}
	}
}

// requestRosterReplan stamps the sweep: every pilot whose plan was built before now becomes
// due. The stamp is written to server_settings before the cache is updated, so a failed
// write leaves the lane unchanged.
func (m *Mod) requestRosterReplan(ctx context.Context) error {
	now := clock.Unix()
	if err := evedb.SetServerSetting(ctx, m.db, settingReplanEpoch,
		strconv.FormatInt(now, 10)); err != nil {
		return fmt.Errorf("requestRosterReplan: %w", err)
	}
	replanEpochValue.Store(now)
	return nil
}

// clearRosterReplan drops the stamp, leaving the ordinary hourly cadence. Pilots already
// replanned keep their new plans; the sweep is a queue position, not a transaction.
func (m *Mod) clearRosterReplan(ctx context.Context) error {
	if err := evedb.SetServerSetting(ctx, m.db, settingReplanEpoch, "0"); err != nil {
		return fmt.Errorf("clearRosterReplan: %w", err)
	}
	replanEpochValue.Store(0)
	return nil
}

// setPlanHorizon persists a new horizon and applies it to plans built from now on. Plans
// already written keep their length until the next replan.
func (m *Mod) setPlanHorizon(ctx context.Context, minutes float64) error {
	v := clampPlanHorizon(minutes)
	if err := evedb.SetServerSetting(ctx, m.db, settingPlanHorizon,
		strconv.FormatInt(int64(v), 10)); err != nil {
		return fmt.Errorf("setPlanHorizon: %w", err)
	}
	planHorizonValue.Store(int64(v))
	return nil
}

// setRefineFloor persists the forced Refining level, writing before the cache is updated
// as setPlanHorizon does. It replans nobody: the caller pairs it with the reconsider sweep
// when it wants the roster to act now, since a mass replan is a real load.
func (m *Mod) setRefineFloor(ctx context.Context, level int) error {
	v := clampRefineFloor(level)
	if err := evedb.SetServerSetting(ctx, m.db, settingRefineFloor,
		strconv.Itoa(v)); err != nil {
		return fmt.Errorf("setRefineFloor: %w", err)
	}
	refineFloorValue.Store(int64(v))
	return nil
}
