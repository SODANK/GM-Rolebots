package rolebots

import (
	"context"
	"sync"
)

// Local 0.0: a corporation's 0.0 is measured from its headquarters and the offices it holds for 0.0
// (forward, front), never from whichever office lies nearest. Surveys, tower sites, the forward
// office and the alliance's targets read it. NPC null counts only when it is the corporation's
// nearest null anyway. Design note: docs/ALLIANCES.md, "The frontier".

const (
	// localNullSlack is how many jumps further than the nearest unclaimed contestable system an NPC
	// null system may lie from the headquarters and still count as local.
	localNullSlack = 2
	// localFrontierJumps is how far from the headquarters unclaimed 0.0 counts as the corporation's
	// own frontier.
	localFrontierJumps = 15
	// nullViewTTL is how long one corporation's view is reused.
	nullViewTTL = 10 * 60
)

// nullView is what one corporation knows about 0.0 around it.
type nullView struct {
	// HQJumps is gate jumps from the headquarters system.
	HQJumps map[int64]int
	// AnchorJumps is gate jumps from the headquarters, forward office and front office.
	AnchorJumps map[int64]int
	// NPCNull is every 0.0 system an NPC faction holds; Unclaimed every contestable one no alliance holds.
	NPCNull   map[int64]bool
	Unclaimed map[int64]bool
	// NearestContest is the headquarters' jumps to the nearest unclaimed system, -1 for none.
	NearestContest int
	at             int64
	key            [3]int64
}

var nullViews sync.Map // corpID -> *nullView

func init() {
	registerCorpForget("nullViews", forgetNullView)
}

// forgetNullView drops a corporation's cached view, after its anchors change.
func forgetNullView(corpID int64) { nullViews.Delete(corpID) }

// localNullOK reports whether a 0.0 system is local to a corporation. Contestable 0.0 always is; NPC
// null only within localNullSlack jumps of the headquarters' nearest unclaimed contestable system,
// or anywhere when no contestable system is reachable. Pure.
func localNullOK(npcNull bool, hqJumps, nearestContest int) bool {
	if !npcNull {
		return true
	}
	if hqJumps < 0 {
		return false
	}
	if nearestContest < 0 {
		return true
	}
	return hqJumps <= nearestContest+localNullSlack
}

// nearestJumps is the smallest distance in dist over the systems of set, -1 when none is reached. Pure.
func nearestJumps(dist map[int64]int, set map[int64]bool) int {
	best := -1
	for sys := range set {
		if d, ok := dist[sys]; ok && (best < 0 || d < best) {
			best = d
		}
	}
	return best
}

// localizeReach rewrites a platform reach for 0.0: NPC null that is not local leaves it, and every
// other 0.0 system is measured from the corporation's anchors. Lowsec keeps its office distance.
// Returns a new map. Pure.
func localizeReach(reach map[int64]int, sec func(int64) float64, v *nullView) map[int64]int {
	if v == nil {
		return reach
	}
	out := make(map[int64]int, len(reach))
	for sys, d := range reach {
		if sec(sys) >= 0 {
			out[sys] = d
			continue
		}
		hq, ok := v.HQJumps[sys]
		if !ok {
			hq = -1
		}
		if !localNullOK(v.NPCNull[sys], hq, v.NearestContest) {
			continue
		}
		if a, ok := v.AnchorJumps[sys]; ok {
			out[sys] = a
		}
	}
	return out
}

// localFrontierSet is the corporation's own frontier: unclaimed systems within localFrontierJumps of
// the headquarters, plus pull (its alliance's targets and stake). When none lies that close, the
// unclaimed systems of the constellation nearest the headquarters. Pure.
func localFrontierSet(v *nullView, pull map[int64]bool, conOf func(int64) int64) map[int64]bool {
	out := map[int64]bool{}
	if v == nil {
		return out
	}
	for sys := range v.Unclaimed {
		if d, ok := v.HQJumps[sys]; ok && d <= localFrontierJumps {
			out[sys] = true
		}
	}
	for sys := range pull {
		out[sys] = true
	}
	if len(out) > 0 || v.NearestContest < 0 {
		return out
	}
	var nearest int64
	for sys := range v.Unclaimed {
		if d, ok := v.HQJumps[sys]; ok && d == v.NearestContest && (nearest == 0 || sys < nearest) {
			nearest = sys
		}
	}
	con := conOf(nearest)
	for sys := range v.Unclaimed {
		if con != 0 && conOf(sys) == con {
			out[sys] = true
		}
	}
	if len(out) == 0 && nearest != 0 {
		out[nearest] = true
	}
	return out
}

// corpNullView is a corporation's 0.0 view, read at most every nullViewTTL. hq is its headquarters
// station (the factory office when it has none). Nil when the map cannot be read.
func (m *Mod) corpNullView(ctx context.Context, corpID, hq int64) *nullView {
	sysOf := func(st int64) int64 {
		if st == 0 {
			return 0
		}
		return m.stationSystemID(ctx, st)
	}
	hqSys := sysOf(hq)
	if hqSys == 0 {
		return nil
	}
	fwd := sysOf(m.forwardOfficeOf(ctx, corpID))
	front := sysOf(m.frontOfficeOf(ctx, corpID))
	key := [3]int64{hqSys, fwd, front}
	now := clock.Unix()
	if v, ok := nullViews.Load(corpID); ok {
		if nv := v.(*nullView); nv.key == key && now-nv.at < nullViewTTL {
			return nv
		}
	}
	hqJ, err := m.corpJumpMap(ctx, hqSys)
	if err != nil {
		return nil
	}
	anchorJ := hqJ
	if fwd != 0 || front != 0 {
		if anchorJ, err = m.corpJumpMap(ctx, hqSys, fwd, front); err != nil {
			return nil
		}
	}
	npc, err := m.npcNullSystems(ctx)
	if err != nil {
		return nil
	}
	unclaimed := m.unclaimedNullSystems(ctx)
	nv := &nullView{HQJumps: hqJ, AnchorJumps: anchorJ, NPCNull: npc, Unclaimed: unclaimed,
		NearestContest: nearestJumps(hqJ, unclaimed), at: now, key: key}
	nullViews.Store(corpID, nv)
	return nv
}

// localReach is corpPlatformReach's answer made local (localizeReach).
func (m *Mod) localReach(ctx context.Context, corpID, hq int64, u *universe, reach map[int64]int) map[int64]int {
	v := m.corpNullView(ctx, corpID, hq)
	if v == nil || u == nil {
		return reach
	}
	return localizeReach(reach, func(sys int64) float64 { return u.meta[sys].Security }, v)
}
