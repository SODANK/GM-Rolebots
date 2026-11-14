package rolebots

import (
	"context"
	"fmt"

	"gurimoon/modapi"
)

// ── one committed objective per fleet ───────────────────────────────────────
// Every writer of a combat fleet's sweep target (GangState.HuntSystemID) proposes an
// objective and arbitrate decides: the commander's help-call check (adoptHelpCall), the
// bounty board's intercept (huntOnce) and the fleet's own site pick (gangSite,
// gangPackSite). A proposal matching what the fleet is doing is a no-op. Anything else
// needs a strictly higher priority, a site the fleet is inside yields to nothing, and
// what is pushed aside is suppressed for objSuppressSec so the two do not trade places
// on the next pass.

const (
	objCall      = "call"      // answering rolebot_fleet_help (Ref = callID)
	objIntercept = "intercept" // a bountied pilot (Ref = charID)
	objSite      = "site"      // an open site the fleet picked itself (Ref = siteID)
	objOrder     = "order"     // a department head's standing order (Ref = systemID)
	objRelief    = "relief"    // the distress the fleet was raised for (Ref = systemID)
	objSweep     = "sweep"     // an ordinary belt sweep (Ref = systemID)

	// prioEngaged is a site the fleet has entered: nothing pre-empts it.
	prioEngaged = 4

	// objSuppressSec is how long a pre-empted objective stays off the table.
	objSuppressSec = 10 * 60
	// objSuppressMax bounds the suppression memory on the blackboard.
	objSuppressMax = 8
)

// GangObjective is what a combat fleet is committed to (GangState.Objective).
type GangObjective struct {
	Kind     string `json:"kind"`
	Ref      int64  `json:"ref,omitempty"`
	SystemID int64  `json:"systemID,omitempty"`
	Prio     int    `json:"prio"`
	Since    int64  `json:"since,omitempty"`
	// Until is the lock: past it the objective is lapsed and anything may
	// replace it. 0 = held until it is released.
	Until int64 `json:"until,omitempty"`
	// Engaged: the fleet is inside the site this objective names.
	Engaged bool `json:"engaged,omitempty"`

	// Announce asks commitObjective to bump SiteSeq when the target moves, so
	// members already flying somewhere else re-plan at once (a call). Not
	// stored.
	Announce bool `json:"-"`
}

func (o *GangObjective) key() string { return fmt.Sprintf("%s:%d", o.Kind, o.Ref) }

// prio is the objective's standing, Engaged included.
func (o *GangObjective) prio() int {
	if o.Engaged && o.Prio < prioEngaged {
		return prioEngaged
	}
	return o.Prio
}

// callObjectivePrio ranks a help call by its kind: the corporation's call to arms over a
// siege over a kin fleet in trouble. A kin's site call sits with the fleet's own site and
// an intercept; none of the three pre-empts another.
func callObjectivePrio(kind string) int {
	switch kind {
	case helpCallToArms:
		return 6
	case helpSiege:
		return 5
	}
	return 3
}

// objectivePrio is the standing of each self-chosen kind.
func objectivePrio(kind string) int {
	switch kind {
	case objIntercept, objSite:
		return 3
	case objOrder, objRelief:
		return 2
	}
	return 1
}

// objVerdict is arbitrate's answer.
type objVerdict int

const (
	objRefuse objVerdict = iota // the fleet keeps what it has
	objSame                     // already doing exactly this
	objMoved                    // the same objective, somewhere else (a mark that moved)
	objAccept                   // taken, displacing whatever was there
)

func (v objVerdict) String() string {
	switch v {
	case objSame:
		return "same"
	case objMoved:
		return "moved"
	case objAccept:
		return "accept"
	}
	return "refuse"
}

// arbitrate decides one proposal against the fleet's current objective. Pure.
func arbitrate(cur *GangObjective, prop GangObjective, suppressed map[string]int64, now int64) objVerdict {
	if cur != nil && cur.Kind == prop.Kind && cur.Ref == prop.Ref {
		if prop.SystemID != 0 && prop.SystemID != cur.SystemID {
			return objMoved
		}
		return objSame
	}
	if until, ok := suppressed[prop.key()]; ok && now < until {
		return objRefuse
	}
	if objectiveLapsed(cur, now) {
		return objAccept
	}
	if cur.Engaged {
		// A site the fleet is inside is finished (or given up by the run itself, siterun.go)
		// before the fleet goes anywhere else.
		return objRefuse
	}
	if prop.prio() > cur.prio() {
		return objAccept
	}
	return objRefuse
}

// objectiveLapsed reports whether cur no longer holds the fleet. Pure.
func objectiveLapsed(cur *GangObjective, now int64) bool {
	return cur == nil || (cur.Until != 0 && now >= cur.Until)
}

// objectiveMayTake is arbitrate as a question, for the callers that want to
// know before they spend a query finding the proposal's Ref (a mark, a call).
func objectiveMayTake(gs *GangState, kind string, prio int, now int64) bool {
	cur := currentObjective(gs, now)
	if objectiveLapsed(cur, now) {
		return true
	}
	if cur.Kind == kind {
		return true // same kind: the full proposal decides
	}
	return !cur.Engaged && prio > cur.prio()
}

// currentObjective is the objective the blackboard describes. A stored objective whose
// subject was cleared (intercept dropped, call closed, site done) no longer stands; a
// blackboard with no stored objective is read off its own fields. Pure.
func currentObjective(gs *GangState, now int64) *GangObjective {
	if gs == nil {
		return nil
	}
	if o := gs.Objective; o != nil && objectiveStands(gs, o) {
		return o
	}
	switch {
	case gs.HuntTargetCharID != 0 && now < gs.HuntETA:
		return &GangObjective{Kind: objIntercept, Ref: gs.HuntTargetCharID, SystemID: gs.HuntSystemID,
			Prio: objectivePrio(objIntercept), Until: gs.HuntETA}
	case gs.HelpCallID != 0:
		return &GangObjective{Kind: objCall, Ref: gs.HelpCallID, SystemID: gs.HuntSystemID,
			Prio: callObjectivePrio(""), Until: now + HelpTTLSec()}
	case gs.SiteID != 0:
		return &GangObjective{Kind: objSite, Ref: gs.SiteID, SystemID: gs.HuntSystemID,
			Prio: objectivePrio(objSite), Until: gs.HuntPickedAt + siteRunMaxSec}
	case gs.HuntSystemID != 0 && gs.HuntPickedAt != 0:
		return &GangObjective{Kind: objSweep, Ref: gs.HuntSystemID, SystemID: gs.HuntSystemID,
			Prio: objectivePrio(objSweep), Until: gs.HuntPickedAt + gangHuntSweepSec}
	}
	return nil
}

// objectiveStands reports whether the blackboard still carries o's subject.
func objectiveStands(gs *GangState, o *GangObjective) bool {
	switch o.Kind {
	case objIntercept:
		return gs.HuntTargetCharID == o.Ref
	case objCall:
		return gs.HelpCallID == o.Ref
	case objSite:
		return gs.SiteID == o.Ref
	}
	// A sweep, an order or a relief trip is the system itself.
	return gs.HuntSystemID == o.SystemID && gs.HuntPickedAt != 0
}

// displaceObjective clears what cur held on the blackboard, for the objective
// replacing it, and remembers it so it is not re-proposed straight back.
func displaceObjective(gs *GangState, gangID int64, cur *GangObjective, now int64) {
	if cur == nil {
		return
	}
	switch cur.Kind {
	case objIntercept:
		gs.HuntTargetCharID, gs.HuntETA, gs.HuntArriveAt = 0, 0, 0
	case objCall:
		gs.HelpCallID = 0
	}
	if gs.SiteID != 0 {
		releaseSite(gs.SiteID, 0, gangID)
		gs.SiteID, gs.SiteName = 0, ""
	}
	if gs.ObjSuppressed == nil {
		gs.ObjSuppressed = map[string]int64{}
	}
	for k, until := range gs.ObjSuppressed {
		if now >= until {
			delete(gs.ObjSuppressed, k)
		}
	}
	if len(gs.ObjSuppressed) >= objSuppressMax {
		// Oldest out: the one closest to lapsing anyway.
		var oldest string
		for k, until := range gs.ObjSuppressed {
			if oldest == "" || until < gs.ObjSuppressed[oldest] {
				oldest = k
			}
		}
		delete(gs.ObjSuppressed, oldest)
	}
	gs.ObjSuppressed[cur.key()] = now + objSuppressSec
}

// commitObjective is the one way a combat fleet's target changes. The proposal is
// arbitrated against the gang's current blackboard (re-read: the bounty pass and the
// members' plans write the same row from different goroutines) and only an accepted one
// runs apply. SiteSeq bumps only when an announced objective moves the fleet.
func (m *Mod) commitObjective(ctx context.Context, gangID int64, gs *GangState, prop GangObjective,
	apply func(*GangState)) (objVerdict, error) {
	now := clock.Unix()
	fresh, err := getGang(ctx, m.db, gangID)
	if err != nil {
		return objRefuse, err
	}
	st := *gs
	if fresh != nil {
		st = fresh.State
	}
	v, dirty := decideObjective(&st, gangID, prop, apply, now)
	if dirty {
		if err := saveGangState(ctx, m.db, gangID, &st); err != nil {
			return v, err
		}
	}
	*gs = st
	return v, nil
}

// decideObjective is commitObjective on a blackboard already in hand: the
// verdict, the writes it implies, and whether the blackboard changed. Pure
// but for the in-memory site claims a displaced site releases.
func decideObjective(st *GangState, gangID int64, prop GangObjective, apply func(*GangState), now int64) (objVerdict, bool) {
	cur := currentObjective(st, now)
	v := arbitrate(cur, prop, st.ObjSuppressed, now)
	switch v {
	case objRefuse:
		return v, false
	case objSame:
		// Refresh the lock only; the fleet is already on it.
		if st.Objective != nil && st.Objective.Until == prop.Until && prop.Prio <= st.Objective.Prio {
			return v, false
		}
		o := *cur
		if prop.Until != 0 {
			o.Until = prop.Until
		}
		if prop.Prio > o.Prio {
			o.Prio = prop.Prio
		}
		st.Objective = &o
		return v, true
	}
	wasSys, wasSite := st.HuntSystemID, st.SiteID
	o := prop
	o.Announce = false
	if v == objAccept {
		displaceObjective(st, gangID, cur, now)
		o.Since = now
	} else if cur != nil {
		o.Since, o.Engaged = cur.Since, cur.Engaged
	}
	apply(st)
	if o.SystemID == 0 {
		o.SystemID = st.HuntSystemID
	}
	st.Objective = &o
	if prop.Announce && (st.HuntSystemID != wasSys || st.SiteID != wasSite) {
		st.SiteSeq++
	}
	return v, true
}

// clearObjective releases the fleet's objective when apply's own condition says it is the
// one ending (a run over, an intercept resolved). Like commitObjective it re-reads; apply
// sees the fresh blackboard and reports whether it changed anything worth saving.
func (m *Mod) clearObjective(ctx context.Context, gangID int64, gs *GangState, apply func(*GangState) bool) error {
	fresh, err := getGang(ctx, m.db, gangID)
	if err != nil {
		return err
	}
	st := *gs
	if fresh != nil {
		st = fresh.State
	}
	if !apply(&st) {
		*gs = st
		return nil
	}
	st.Objective = nil
	if err := saveGangState(ctx, m.db, gangID, &st); err != nil {
		return err
	}
	*gs = st
	return nil
}

// markObjectiveEngaged records that the fleet is inside the site it is
// running: from here only the run's own end releases it. Written once.
func (m *Mod) markObjectiveEngaged(ctx context.Context, gangID int64, gs *GangState, siteID int64) error {
	now := clock.Unix()
	if o := gs.Objective; o != nil && o.Engaged {
		return nil
	}
	return m.writeHuntState(ctx, gangID, gs, func(st *GangState) {
		if st.SiteID != siteID {
			return
		}
		o := currentObjective(st, now)
		if o == nil || o.Engaged {
			return
		}
		e := *o
		e.Engaged = true
		e.Until = now + siteRunMaxSec
		st.Objective = &e
	})
}

// siteHeld reports whether the fleet's site still binds it: named, still standing in the
// system the fleet is sweeping, inside the run's own bound. The sweep clock does not end
// it, since a run takes as long as it takes (siteRunMaxSec).
func siteHeld(gs *GangState, now int64) bool {
	if gs == nil || gs.SiteID == 0 || gs.HuntSystemID == 0 {
		return false
	}
	if gs.HuntPickedAt != 0 && now-gs.HuntPickedAt >= siteRunMaxSec+siteMusterMaxSec {
		return false
	}
	return siteStandsIn(gs.SiteID, gs.HuntSystemID)
}

// siteStandsIn reports whether open site siteID is still live in sysID.
func siteStandsIn(siteID, sysID int64) bool {
	if !modapi.CanRunSites() {
		return false
	}
	s, ok := modapi.Sites().Get(siteID)
	return ok && s.SolarSystemID == sysID
}

// huntSweepHeld is the pickers' "keep what we have": the sweep clock still
// running, or a site the fleet is still bound to.
func huntSweepHeld(gs *GangState, now int64) bool {
	if gs == nil || gs.HuntSystemID == 0 {
		return false
	}
	return now-gs.HuntPickedAt < gangHuntSweepSec || siteHeld(gs, now)
}

// fileHuntRecent files the system a fleet is leaving on its recent list.
func fileHuntRecent(gs *GangState, next int64) {
	if gs.HuntSystemID == 0 || gs.HuntSystemID == next {
		return
	}
	gs.HuntRecent = append([]int64{gs.HuntSystemID}, gs.HuntRecent...)
	if len(gs.HuntRecent) > gangHuntRecentMax {
		gs.HuntRecent = gs.HuntRecent[:gangHuntRecentMax]
	}
}

// fleetSecPerJump is the slowest a hull of this align crosses one gate in the abstract
// tier (sim.go unwatchedLegSec at the top of its roll). A flat figure under a frigate's
// fastest leg would run a fleet's arrival clock out before its slowest member was there.
func fleetSecPerJump(alignSec float64) int64 {
	if alignSec <= 0 {
		alignSec = alignUnknownSec
	}
	return int64(jumpLegMaxSec) + int64(alignSec*alignLegFactor)
}
