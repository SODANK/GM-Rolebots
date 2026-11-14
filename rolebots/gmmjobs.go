//go:build gmmordu

package rolebots

// Mordu's Network: the roster on the freelance board (modules/GMMordu-mod).
//
// The board credits a signed-on pilot from the core's world-event bus
// (worldevents.go). This lane signs working pilots on to open jobs for their trade
// within gmmSignOnJumps, leaves a job when the trade, cap or distance
// (gmmLeaveJumps) no longer fits, collects banked pay in one Collect-all, and
// pulls the planners toward a job's ground (gmmPulls), worth what the job still
// pays. Idle pilots are skipped, since leaving on every idle would re-stamp
// acceptedAt each pass. Bot sign-ons per job are capped at what the board's
// roster share can pay (gmm.botQuotaShare).

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"sort"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

const (
	gmmBotPassTicks  = 300      // 5 min
	gmmBotCollectMin = 250000.0 // ISK banked across jobs before a bot collects

	// Reach in jumps from the pilot. Beyond gmmSignOnJumps a job is not worth signing
	// on to; beyond gmmLeaveJumps it is let go. The gap stops a pilot on the edge from
	// signing on and off every pass.
	gmmSignOnJumps = 8
	gmmLeaveJumps  = 12

	// A job's pull on the planners: one jump per gmmPullISKPerJump the job still pays
	// this pilot, at most gmmPullMaxJumps. At the board's default 3M earn target a
	// fresh job is worth the full ten jumps.
	gmmPullISKPerJump = 300000.0
	gmmPullMaxJumps   = 10.0
)

var gmmBotPassBusy atomic.Bool

// gmmMethodsFor is what a working activity can contribute to, best first.
func gmmMethodsFor(activity string) []string {
	switch activity {
	case ActMining, ActIce:
		return []string{evedb.GMMMine, evedb.GMMSellNPC}
	case ActIndMining:
		return []string{evedb.GMMMine} // the haul goes to the commander, never to a bid
	case ActCombat, ActSecurity:
		return []string{evedb.GMMDestroyNPC}
	}
	return nil
}

func (m *Mod) startGMMJobs(_ context.Context) {
	m.registerPeriodic("rolebot-gmm-jobs", gmmBotPassTicks, func(uint64) bool {
		if !Master() || !gmmBotPassBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("gmm jobs", func() {
			defer gmmBotPassBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := m.gmmBotPass(ctx); err != nil {
				rlogf("rolebots: gmm jobs: %v", err)
			}
		})
		return true
	})
}

// ── the pulls the planners read ─────────────────────────────────────────────

// gmmPull is one signed-on job's draw on a pilot: its trade, its ground and
// what it is worth flying for.
type gmmPull struct {
	method  string
	systems map[int64]bool
	jumps   float64
}

// gmmPulls is every bot's pulls, rebuilt whole by each pass and swapped in, so
// a planner reads it without a lock or a query.
var gmmPulls atomic.Pointer[map[int64][]gmmPull]

func gmmPullsFor(charID int64, method string) []gmmPull {
	p := gmmPulls.Load()
	if p == nil {
		return nil
	}
	var out []gmmPull
	for _, g := range (*p)[charID] {
		if g.method == method {
			out = append(out, g)
		}
	}
	return out
}

// gmmMinePull is how many jumps cheaper a belt system is to this pilot for
// lying in a mine job's ground: the strongest such job's pull.
func gmmMinePull(charID, sysID int64) float64 {
	best := 0.0
	for _, g := range gmmPullsFor(charID, evedb.GMMMine) {
		if g.systems[sysID] && g.jumps > best {
			best = g.jumps
		}
	}
	return best
}

// gmmMinePullMax is the largest pull any system can carry for this pilot, which
// the belt search's pruning floor must allow for.
func gmmMinePullMax(charID int64) float64 {
	best := 0.0
	for _, g := range gmmPullsFor(charID, evedb.GMMMine) {
		best = max(best, g.jumps)
	}
	return best
}

// gmmCombatSite is a kill job's ground for a hunter: of the systems a signed-on
// destroy_npc job covers within gmmSignOnJumps, the one weigh accepts with the
// best pull net of the flying. 0 = no job, or nowhere in its ground this pilot
// would fight.
func (m *Mod) gmmCombatSite(ctx context.Context, b *botRun, u *universe, weigh func(sysID int64) (ok, engage bool)) int64 {
	pulls := gmmPullsFor(b.CharID, evedb.GMMDestroyNPC)
	if len(pulls) == 0 || u == nil {
		return 0
	}
	reach := gangReachMap(u, b.SystemID, gmmSignOnJumps)
	var best int64
	bestNet := math.Inf(-1)
	for _, g := range pulls {
		for sysID := range g.systems {
			j, ok := reach[sysID]
			if !ok {
				continue
			}
			net := g.jumps - float64(j)
			if net <= bestNet {
				continue
			}
			if ok, engage := weigh(sysID); !ok || !engage {
				continue
			}
			best, bestNet = sysID, net
		}
	}
	if best != 0 {
		m.logBot(b.CharID, logInfo, "gmm", "hunting %s for a Mordu's Network contract", m.nameOfSystem(ctx, best))
	}
	return best
}

// ── the pass ────────────────────────────────────────────────────────────────

type gmmBotJob struct {
	job      *evedb.GMMJob
	systems  map[int64]bool
	botRoom  int64 // units the roster may still fill
	botSlots int   // bot sign-ons the job takes, in all
	bots     int   // bot sign-ons now
}

type gmmSigned struct {
	jobID, contribution int64
}

type gmmBotAtWork struct {
	charID, systemID int64
	methods          []string
}

func (m *Mod) gmmBotPass(ctx context.Context) error {
	rules := evedb.LoadGMMRules(ctx, m.db)
	if !rules.Enabled {
		gmmPulls.Store(nil)
		return nil
	}
	m.gmmBotCollect(ctx, rules)
	u, err := m.universe(ctx)
	if err != nil {
		return err
	}

	// The open board, with what the roster may still fill of each job.
	list, err := evedb.GMMListJobs(ctx, m.db, evedb.GMMJobFilter{Limit: 500})
	if err != nil {
		return err
	}
	jobs := make(map[int64]*gmmBotJob, len(list))
	for _, j := range list {
		slots := 1
		if j.PerPilotCap > 0 {
			slots = max(1, int(math.Ceil(rules.BotQuotaShare*float64(j.Quota)/float64(j.PerPilotCap))))
		}
		sys := map[int64]bool{}
		for _, s := range evedb.GMMScopeSystems(ctx, m.db, j) {
			sys[s] = true
		}
		jobs[j.ID] = &gmmBotJob{job: j, systems: sys, botSlots: slots}
	}
	if rows, err := m.db.QueryContext(ctx, `SELECT jobID, botProgress FROM gmmJobs WHERE state = 'open'`); err == nil {
		for rows.Next() {
			var id, bp int64
			if rows.Scan(&id, &bp) == nil {
				if bj := jobs[id]; bj != nil {
					bj.botRoom = evedb.GMMBotRoom(bj.job.Quota, bp, rules.BotQuotaShare)
				}
			}
		}
		rows.Close()
	}

	// Who on the roster is signed on to what, and how far into its cap.
	signed := map[int64][]gmmSigned{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT p.charID, p.jobID, p.contribution
		FROM gmmParticipants p
		JOIN gmmJobs j ON j.jobID = p.jobID
		JOIN rolebot_characters rb ON rb.characterID = p.charID AND rb.enabled = 1
		WHERE j.state = 'open' AND p.leftAt = 0`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var charID int64
		var s gmmSigned
		if rows.Scan(&charID, &s.jobID, &s.contribution) == nil {
			signed[charID] = append(signed[charID], s)
			if bj := jobs[s.jobID]; bj != nil {
				bj.bots++
			}
		}
	}
	rows.Close()

	// Bots at a trade the board pays for, and where.
	rows, err = m.db.QueryContext(ctx, `
		SELECT s.characterID, s.activity, c.solarSystemID
		FROM rolebot_state s
		JOIN rolebot_characters rb ON rb.characterID = s.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = s.characterID
		WHERE s.halted = 0 AND s.activity IN (?, ?, ?, ?, ?)`,
		ActMining, ActIce, ActIndMining, ActCombat, ActSecurity)
	if err != nil {
		return err
	}
	var working []gmmBotAtWork
	for rows.Next() {
		var w gmmBotAtWork
		var act string
		// A logged-off pilot signs on and off nothing (timezone.go).
		if rows.Scan(&w.charID, &act, &w.systemID) == nil && !popIsOffline(w.charID) {
			w.methods = gmmMethodsFor(act)
			working = append(working, w)
		}
	}
	rows.Close()

	// A different order every pass, so the same pilots do not take every
	// bot slot on the board pass after pass.
	rand.Shuffle(len(working), func(i, j int) { working[i], working[j] = working[j], working[i] })

	pulls := map[int64][]gmmPull{}
	addPull := func(charID int64, bj *gmmBotJob, contribution int64) {
		left := min(bj.job.PerPilotCap-contribution, bj.botRoom)
		if jumps := gmmPullJumps(left, bj.job.RewardPerUnit); jumps > 0 {
			pulls[charID] = append(pulls[charID], gmmPull{method: bj.job.Method, systems: bj.systems, jumps: jumps})
		}
	}

	var joined, left int
	for _, w := range working {
		reach := gangReachMap(u, w.systemID, gmmLeaveJumps)
		// Leave what this pilot no longer works: another trade, a full cap,
		// ground it has wandered away from.
		var keep []int64
		for _, s := range signed[w.charID] {
			bj := jobs[s.jobID]
			if bj == nil {
				keep = append(keep, s.jobID) // closed this pass; nothing to leave
				continue
			}
			_, near := gmmNearest(bj.systems, reach)
			if gmmHasMethod(w.methods, bj.job.Method) && near && s.contribution < bj.job.PerPilotCap {
				keep = append(keep, s.jobID)
				addPull(w.charID, bj, s.contribution)
				continue
			}
			if err := evedb.GMMLeave(ctx, m.db, s.jobID, w.charID); err == nil {
				bj.bots--
				left++
				m.logBot(w.charID, logInfo, "gmm", "signed off Mordu's Network contract: %s", bj.job.Title)
			}
		}
		free := rules.MaxActive - len(keep)
		if free <= 0 {
			continue
		}
		for _, bj := range gmmBotCandidates(w, jobs, keep, reach) {
			if free <= 0 {
				break
			}
			err := evedb.GMMAccept(ctx, m.db, bj.job.ID, w.charID, rules.MaxActive)
			if errors.Is(err, evedb.ErrGMMTooMany) {
				break
			}
			if err != nil {
				continue
			}
			bj.bots++
			free--
			joined++
			addPull(w.charID, bj, 0)
			m.logBot(w.charID, logInfo, "gmm", "signed on to Mordu's Network contract: %s", bj.job.Title)
		}
	}
	gmmPulls.Store(&pulls)
	if joined+left > 0 {
		rlogf("rolebots: gmm jobs: %d sign-on(s), %d sign-off(s) across %d working pilot(s)", joined, left, len(working))
	}
	return nil
}

// gmmPullJumps is what a job still paying left units at reward each is worth
// flying for, in jumps.
func gmmPullJumps(left int64, reward float64) float64 {
	if left <= 0 || reward <= 0 {
		return 0
	}
	return min(gmmPullMaxJumps, float64(left)*reward/gmmPullISKPerJump)
}

// gmmNearest is how many jumps the nearest system of a job's ground is, within
// a reach map; ok is false when none of it is in reach.
func gmmNearest(systems map[int64]bool, reach map[int64]int) (jumps int, ok bool) {
	jumps = math.MaxInt
	for s := range systems {
		if j, in := reach[s]; in && j < jumps {
			jumps, ok = j, true
		}
	}
	return jumps, ok
}

// gmmBotCandidates ranks the open jobs a working pilot could sign on to: its
// trade, ground within gmmSignOnJumps, room left for the roster, a bot slot free.
// Pay is discounted by the flying to the nearest system; a sale job ranks below
// the trade itself, since nothing makes a miner sell to that corporation.
func gmmBotCandidates(w gmmBotAtWork, jobs map[int64]*gmmBotJob, have []int64, reach map[int64]int) []*gmmBotJob {
	type cand struct {
		bj    *gmmBotJob
		score float64
	}
	var cs []cand
	for _, bj := range jobs {
		j := bj.job
		if bj.botRoom <= 0 || bj.bots >= bj.botSlots || gmmHasJob(have, j.ID) {
			continue
		}
		rank := -1
		for i, meth := range w.methods {
			if meth == j.Method {
				rank = i
			}
		}
		if rank < 0 {
			continue
		}
		jumps, ok := gmmNearest(bj.systems, reach)
		if !ok || jumps > gmmSignOnJumps {
			continue
		}
		score := j.RewardPerUnit * float64(min(j.PerPilotCap, bj.botRoom)) / (1 + float64(jumps)/4)
		if rank > 0 {
			score *= 0.5
		}
		cs = append(cs, cand{bj, score})
	}
	sort.Slice(cs, func(i, k int) bool { return cs[i].score > cs[k].score })
	out := make([]*gmmBotJob, len(cs))
	for i, c := range cs {
		out[i] = c.bj
	}
	return out
}

// gmmBotCollect pays out every bot whose banked pay is worth collecting, or
// whose job has closed: one Collect-all, one journal row.
func (m *Mod) gmmBotCollect(ctx context.Context, rules evedb.GMMRules) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT p.charID
		FROM gmmParticipants p
		JOIN gmmJobs j ON j.jobID = p.jobID
		JOIN rolebot_characters rb ON rb.characterID = p.charID AND rb.enabled = 1
		WHERE j.state <> 'archived'
		GROUP BY p.charID
		HAVING SUM(GREATEST(p.bankedISK - p.claimedISK - p.forfeitISK, 0)) >= ?
		    OR SUM(j.state <> 'open' AND p.bankedISK - p.claimedISK - p.forfeitISK >= 0.01) > 0
		LIMIT 1000`, gmmBotCollectMin)
	if err != nil {
		rlogf("rolebots: gmm collect: %v", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		res, err := evedb.GMMCollect(ctx, m.db, id, 0, rules.StandingPer10Pct)
		if err != nil {
			if !errors.Is(err, evedb.ErrGMMNothingDue) {
				rlogf("rolebots: gmm collect char=%d: %v", id, err)
			}
			continue
		}
		if res.Paid > 0 {
			m.logBot(id, logInfo, "gmm", "collected %.0f ISK from Mordu's Network (%d contract(s))", res.Paid, res.Jobs)
		}
	}
}

func gmmHasMethod(methods []string, method string) bool {
	for _, m := range methods {
		if m == method {
			return true
		}
	}
	return false
}

func gmmHasJob(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
