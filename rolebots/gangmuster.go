package rolebots

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync/atomic"

	evedb "gurimoon/db"
)

// The muster's release rule: when a gathering fleet stops waiting. A fleet goes when a
// working core is in. Pilots still flying to it keep their seats and catch up; members
// that never started the trip are dropped at release. Each member is present (docked at
// staging, or out front in gang.formup), enRoute (its plan state carries this fleet's
// GangID) or stuck (anything else). musterRelease holds the rule, and the deadline still
// applies on top. See docs/comment-notes-b10.md#muster-release-rule.

const (
	settingMusterQuorum = "rolebots.gang.musterQuorumPct"
	settingMusterGrace  = "rolebots.gang.musterGraceMin"
	// settingRecruitBusy lets recruiting seat pilots that are mid-errand (gangRecruitFrom).
	// On by default, since most combat pilots are busy at any moment; busy pilots are asked
	// last and a muster drops the ones that never set out. 0 turns it off.
	settingRecruitBusy = "rolebots.gang.recruitBusy"

	// settingHaulerGrace is how long a live mining op may run with nobody answering in a
	// hauling seat before it winds down (liveOpCoversHaul).
	settingHaulerGrace = "rolebots.gang.haulerGraceMin"
	// settingQuietStandDown is how long a combat or security fleet may find nothing to do
	// on its ground before it stands down (fleetQuiet). 0 turns the rule off.
	settingQuietStandDown = "rolebots.gang.quietStandDownMin"
	// settingFormMin is the smallest auto-formed combat, security or piracy fleet
	// (GangFormMin); settingFormBackoff is the first wait after one dies young (formbackoff.go).
	settingFormMin     = "rolebots.gang.formMinMembers"
	settingFormBackoff = "rolebots.gang.formBackoffMin"
	// settingOpBeltAbandon is how long a watched mining op's miners may be kept off their
	// belt before the site is given up (gangMineStep).
	settingOpBeltAbandon = "rolebots.gang.opBeltAbandonMin"

	defaultMusterQuorumPct   = 60
	defaultMusterGraceMin    = 12
	defaultHaulerGraceMin    = 20
	defaultQuietStandDownMin = 15
	defaultFormMinMembers    = 3
	defaultFormBackoffMin    = 30
	// The abandon wait never falls below the bail memory (beltBailSec): a miner released from the
	// op belt before it may go back to a belt it bailed from would have nowhere to work.
	minOpBeltAbandonMin     = beltBailSec / 60
	defaultOpBeltAbandonMin = minOpBeltAbandonMin
)

var (
	musterQuorumPct   atomic.Int64
	musterGraceMin    atomic.Int64
	recruitBusyOn     atomic.Bool
	haulerGraceMin    atomic.Int64
	quietStandDownMin atomic.Int64
	formMinMembers    atomic.Int64
	formBackoffMin    atomic.Int64
	opBeltAbandonMin  atomic.Int64
)

// RecruitBusy reports whether busy pilots may be recruited.
func RecruitBusy() bool { return recruitBusyOn.Load() }

func init() {
	musterQuorumPct.Store(defaultMusterQuorumPct)
	musterGraceMin.Store(defaultMusterGraceMin)
	recruitBusyOn.Store(true)
	haulerGraceMin.Store(defaultHaulerGraceMin)
	quietStandDownMin.Store(defaultQuietStandDownMin)
	formMinMembers.Store(defaultFormMinMembers)
	formBackoffMin.Store(defaultFormBackoffMin)
	opBeltAbandonMin.Store(defaultOpBeltAbandonMin)
}

// HaulerGraceSec is how long a live mining op may go without a hauler answering.
func HaulerGraceSec() int64 { return haulerGraceMin.Load() * 60 }

// FleetQuietSec is how long a fleet may find nothing to do before it stands down; 0 = never.
func FleetQuietSec() int64 { return quietStandDownMin.Load() * 60 }

// GangFormMin is the smallest fleet the formation pass raises on its own for combat,
// security or piracy. It sits above gangMinMembers so one excusal does not end the fleet.
func GangFormMin() int { return int(formMinMembers.Load()) }

// FormBackoffSec is the first wait a corporation serves after one of its fleets dies young.
func FormBackoffSec() int64 { return formBackoffMin.Load() * 60 }

// OpBeltAbandonSec is how long watched miners may be held off the op belt before the
// op gives the site up.
func OpBeltAbandonSec() int64 { return opBeltAbandonMin.Load() * 60 }

// setMinutes stores n into dst, or def when n is below floor.
func setMinutes(dst *atomic.Int64, floor, def int) func(int) {
	return func(n int) {
		if n < floor {
			n = def
		}
		dst.Store(int64(n))
	}
}

// MusterQuorumFrac is the share of a fleet's roster that releases it early.
func MusterQuorumFrac() float64 { return float64(musterQuorumPct.Load()) / 100 }

// MusterGraceSec is how long a fleet waits before a smaller core may go.
func MusterGraceSec() int64 { return musterGraceMin.Load() * 60 }

func setMusterQuorumPct(n int) {
	if n < 1 || n > 100 {
		n = defaultMusterQuorumPct
	}
	musterQuorumPct.Store(int64(n))
}

func setMusterGraceMin(n int) {
	if n < 1 {
		n = defaultMusterGraceMin
	}
	musterGraceMin.Store(int64(n))
}

// loadMusterSettings reads the fleet lifecycle knobs. Missing or junk values keep the
// defaults.
func (m *Mod) loadMusterSettings(ctx context.Context) {
	for _, s := range []struct {
		key string
		set func(int)
	}{
		{settingMusterQuorum, setMusterQuorumPct},
		{settingMusterGrace, setMusterGraceMin},
		{settingRecruitBusy, func(n int) { recruitBusyOn.Store(n != 0) }},
		{settingHaulerGrace, setMinutes(&haulerGraceMin, 1, defaultHaulerGraceMin)},
		{settingQuietStandDown, setMinutes(&quietStandDownMin, 0, defaultQuietStandDownMin)},
		{settingFormMin, setMinutes(&formMinMembers, gangMinMembers, defaultFormMinMembers)},
		{settingFormBackoff, setMinutes(&formBackoffMin, 0, defaultFormBackoffMin)},
		{settingOpBeltAbandon, setMinutes(&opBeltAbandonMin, minOpBeltAbandonMin, defaultOpBeltAbandonMin)},
	} {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, s.key)
		if err != nil || !ok {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			s.set(n)
		}
	}
}

// musterRelease is the release rule described at the top of the file. Pure.
func musterRelease(present, enRoute, stuck int, elapsed, grace int64, quorum float64) bool {
	all := present + enRoute + stuck
	if all == 0 || present == 0 {
		return false
	}
	if enRoute+stuck == 0 {
		return true
	}
	core := max(gangMinMembers, int(math.Ceil(quorum*float64(all))))
	if present >= core {
		return true
	}
	if elapsed >= grace && present >= max(gangMinMembers, int(math.Ceil(float64(all)/3))) {
		return true
	}
	return false
}

// planAnswersGang reports whether a member's raw plan-state blob carries this fleet's
// GangID, the stamp gangPlan writes before routing a member anywhere and that an excusal
// or a solo errand clears.
func planAnswersGang(blob string, gangID int64) bool {
	if blob == "" || gangID == 0 {
		return false
	}
	var d struct {
		GangID int64 `json:"gangID"`
	}
	if json.Unmarshal([]byte(blob), &d) != nil {
		return false
	}
	return d.GangID == gangID
}

// musterElapsed is how long this fleet has been gathering. MusterStartedAt is written at
// formation; a row without the field reads its start off the deadline it was given.
func musterElapsed(gs GangState, now int64) int64 {
	start := gs.MusterStartedAt
	if start == 0 && gs.MusterDeadline > 0 {
		start = gs.MusterDeadline - MusterTimeoutSec()
	}
	if start == 0 || now < start {
		return 0
	}
	return now - start
}
