package rolebots

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/rates"
)

// The siege quota. A structure's own numbers set what it takes to break it: the hit points the
// bash must deal (its shield to the reinforcement floor for a tower, its whole shield for a
// conquerable station), the shield's peak regeneration the fleet must out-shoot first, and a
// window to do it in. The quota is that DPS, with a margin for stragglers and losses. A siege
// opens on a target whose quota the side can field (siegeCapacityFor), and its stages read the
// fleet's DPS against the regeneration (siegeStageNext), not a head count.

const (
	// siegeWindowSec is the bash window the quota is sized for.
	siegeWindowSec = 2 * 3600
	// siegeQuotaMargin is the quota's head-room over the bare arithmetic: stragglers, losses, the
	// regeneration peak sitting at a quarter shield.
	siegeQuotaMargin = 1.15
	// siegeRegenPeak is a shield's peak regeneration as a multiple of capacity over recharge time.
	siegeRegenPeak = 2.5
	// siegeMusterFormSec is how long a siege may sit in muster before its fleet is given up on.
	siegeMusterFormSec = 2 * 3600
	// siegeLowGraceSec is how long a bash may run with the fleet's DPS at or under the shield's
	// regeneration (gone, short, or out-shot) before it is abandoned.
	siegeLowGraceSec = 30 * 60
	// siegeBashOverrun is how many windows a bash or kill stage may run before it is abandoned.
	siegeBashOverrun = 2.0
	// siegeCapacityPilots caps the pilots a capacity estimate reads, most likely to answer first.
	siegeCapacityPilots = 200
	// siegeCapacityTTL and siegePilotDPSTTL are how long the estimates are reused.
	siegeCapacityTTL = 10 * time.Minute
	siegePilotDPSTTL = 30 * time.Minute
)

// structureHP is what a siege must burn through: hit points, mean resonance per layer (1 = no
// resist), the shield's recharge time in seconds, and the reinforcement floor as a fraction of
// the shield (0 for a station, which is captured when its shield is gone).
type structureHP struct {
	Shield, Armor, Hull float64
	SRes, ARes, HRes    float64
	Tau                 float64
	Floor               float64
}

// siegeHP is the tower bash's name for a structure's numbers.
type siegeHP = structureHP

// siegeQuotaOut is the quota for one structure: the EHP of the bash (to the floor, or the whole
// shield of a station) and of the kill (what is left after the floor), the shield's peak
// regeneration in EHP per second, and the DPS each stage needs inside the window.
type siegeQuotaOut struct {
	BashEHP, KillEHP float64
	Regen            float64
	BashDPS, KillDPS float64
}

// OpenDPS is the DPS a side must field to open a siege on the structure.
func (q siegeQuotaOut) OpenDPS() float64 { return max(q.BashDPS, q.KillDPS) }

// meanRes is the mean of four resonances; a missing or nonsense mean reads as 1 (no resist). Pure.
func meanRes(r [4]float64) float64 {
	s := (r[0] + r[1] + r[2] + r[3]) / 4
	if s <= 0 || s > 1 {
		return 1
	}
	return s
}

// resOr is a layer resonance, or fallback when it was never read. Pure.
func resOr(r, fallback float64) float64 {
	if r <= 0 {
		return fallback
	}
	return r
}

// peakRegenEHP is a shield's peak regeneration in EHP per second. Pure.
func peakRegenEHP(shield, tau, res float64) float64 {
	if tau <= 0 || shield <= 0 {
		return 0
	}
	return siegeRegenPeak * shield / tau / resOr(res, 1)
}

// siegeQuota sizes the fleet a structure needs to break inside window seconds, with margin. The
// bash out-shoots the peak regeneration; the kill stage (a tower's last quarter shield, armor and
// hull, after its reinforcement) does not, since the fleet that got it there already does. Pure.
func siegeQuota(hp structureHP, window, margin float64) siegeQuotaOut {
	if window <= 0 {
		window = siegeWindowSec
	}
	if margin <= 0 {
		margin = 1
	}
	sres := resOr(hp.SRes, 1)
	q := siegeQuotaOut{Regen: peakRegenEHP(hp.Shield, hp.Tau, sres)}
	floor := min(max(hp.Floor, 0), 1)
	q.BashEHP = hp.Shield * (1 - floor) / sres
	if floor > 0 {
		q.KillEHP = hp.Shield*floor/sres + hp.Armor/resOr(hp.ARes, 1) + hp.Hull/resOr(hp.HRes, 1)
	}
	q.BashDPS = (q.BashEHP/window + q.Regen) * margin
	q.KillDPS = q.KillEHP / window * margin
	return q
}

// structureHPOf reads a structure type's numbers. A tower carries the server's reinforcement
// floor; a station has none.
func (m *Mod) structureHPOf(ctx context.Context, typeID int64, tower bool) structureHP {
	hp := structureHP{Shield: 1_000_000, SRes: 1, ARes: 1, HRes: 1}
	if e, err := evedb.GetEntityHPByType(ctx, m.db, typeID); err == nil && e != nil {
		if e.ShieldHP > 0 {
			hp.Shield = e.ShieldHP
		}
		hp.Armor, hp.Hull, hp.Tau = e.ArmorHP, e.StructureHP, e.ShieldTau
		hp.SRes, hp.ARes, hp.HRes = meanRes(e.ShieldRes), meanRes(e.ArmorRes), meanRes(e.HullRes)
	}
	if tower {
		hp.Floor = evedb.StarbaseReinforceShieldPct(ctx, m.db) / 100
	}
	return hp
}

// loadoutDPS is what a loadout puts out per second at the server's damage rate: the same fold
// botCombatant gives the abstract tier and the grid's guns land.
func loadoutDPS(l *combatLoadout) float64 {
	if l == nil {
		return 0
	}
	return l.dps() * rates.PlayerDamage
}

// ── capacity ───────────────────────────────────────────────────────────────

// siegeHullClass reports whether a hull class can sit in a siege line: a combat hull, frigate to
// battleship. Industrials, barges, freighters, rookies and shuttles cannot. Pure.
func siegeHullClass(c HullClass) bool {
	switch c {
	case ClassFrigate, ClassDestroyer, ClassCruiser, ClassBattlecruiser, ClassBattleship:
		return true
	}
	return false
}

var (
	siegePilotDPSMu sync.Mutex
	siegePilotDPSOf = map[int64]struct {
		dps float64
		at  time.Time
	}{}
)

// pilotSiegeDPS is the DPS a pilot can bring to a siege in a ship it owns: its largest combat
// hull (the next when that one brings nothing), read through its real fit and skills
// (loadCombatLoadout). A pilot
// with no combat hull brings nothing of its own. Cached siegePilotDPSTTL.
func (m *Mod) pilotSiegeDPS(ctx context.Context, charID int64) float64 {
	siegePilotDPSMu.Lock()
	if e, ok := siegePilotDPSOf[charID]; ok && time.Since(e.at) < siegePilotDPSTTL {
		siegePilotDPSMu.Unlock()
		return e.dps
	}
	siegePilotDPSMu.Unlock()

	best := 0.0
	byID, _, err := m.loadHullProfiles(ctx)
	if err == nil {
		rows, qerr := m.db.QueryContext(ctx, `
			SELECT e.itemID, e.typeID FROM entity e
			  JOIN invTypes t ON t.typeID = e.typeID
			  JOIN invGroups g ON g.groupID = t.groupID
			 WHERE e.ownerID = ? AND g.categoryID = 6 AND t.groupID <> 29 AND e.singleton = 1`, charID)
		if qerr == nil {
			type hull struct {
				itemID, typeID int64
				class          HullClass
			}
			var hulls []hull
			for rows.Next() {
				var h hull
				if rows.Scan(&h.itemID, &h.typeID) != nil {
					continue
				}
				p := byID[h.typeID]
				if p == nil || !siegeHullClass(p.Class) {
					continue
				}
				h.class = p.Class
				hulls = append(hulls, h)
			}
			rows.Close()
			sort.Slice(hulls, func(i, j int) bool {
				if hulls[i].class != hulls[j].class {
					return hulls[i].class > hulls[j].class
				}
				return hulls[i].itemID < hulls[j].itemID
			})
			// The largest hull first; the next only when that one brings nothing (unfitted, dry).
			for i := 0; i < len(hulls) && i < 2 && best == 0; i++ {
				if l, lerr := m.loadCombatLoadout(ctx, charID, hulls[i].itemID, hulls[i].typeID); lerr == nil {
					best = max(best, loadoutDPS(l))
				}
			}
		}
	}
	siegePilotDPSMu.Lock()
	siegePilotDPSOf[charID] = struct {
		dps float64
		at  time.Time
	}{best, time.Now()}
	siegePilotDPSMu.Unlock()
	return best
}

// siegePilot is one pilot's part in a capacity estimate: what it brings in its own ship, and the
// odds it answers the call.
type siegePilot struct {
	CharID, CorpID int64
	OwnedDPS       float64
	// KitDPS is what the pilot brings in a ready corporation kit, for a pilot with no combat hull
	// of its own (siegeKitCapacity).
	KitDPS float64
	Answer float64
}

// siegeCapacity is the DPS a side can expect to muster: each pilot's own-ship DPS by its odds,
// and with the kits on the shelves lent to the pilots without one.
type siegeCapacity struct {
	Owned    float64
	WithKits float64
	Pilots   int // pilots that bring any DPS
}

// sumSiegeCapacity adds up a capacity estimate. Pure.
func sumSiegeCapacity(ps []siegePilot) siegeCapacity {
	var c siegeCapacity
	for _, p := range ps {
		if p.Answer <= 0 {
			continue
		}
		a := min(p.Answer, 1)
		switch {
		case p.OwnedDPS > 0:
			c.Owned += p.OwnedDPS * a
			c.WithKits += p.OwnedDPS * a
		case p.KitDPS > 0:
			c.WithKits += p.KitDPS * a
		default:
			continue
		}
		c.Pilots++
	}
	return c
}

// siegeAnswerOdds is how likely a pilot is to answer a siege's call: certain for the Security
// arm, a pilot online, or one already held by a page; otherwise armsOdds from its hours.
func siegeAnswerOdds(charID, corpID int64) float64 {
	if !popIsOffline(charID) || armsHeld(charID) {
		return 1
	}
	if seat, ok := armOf(charID, corpID); ok && seat.Arm == armSecurity {
		return 1
	}
	ts := tzNow(charID)
	return armsOdds(min(ts.NextMin, ts.SinceEndMin), 0, false)
}

var (
	siegeCapacityMu sync.Mutex
	siegeCapacityOf = map[string]struct {
		cap siegeCapacity
		ps  []siegePilot
		at  time.Time
	}{}
)

// siegeCapacityFor estimates the DPS the given corporations can muster for a siege: every enabled,
// running pilot not tied up in an op that is not combat (a mining or trade fleet keeps its own),
// the siegeCapacityPilots most likely to answer, each by its best own-ship DPS (pilotSiegeDPS).
// Cached siegeCapacityTTL per set of corporations.
func (m *Mod) siegeCapacityFor(ctx context.Context, corps []int64) (siegeCapacity, []siegePilot) {
	if len(corps) == 0 {
		return siegeCapacity{}, nil
	}
	sorted := slices.Clone(corps)
	slices.Sort(sorted)
	key := int64CSV(sorted)
	siegeCapacityMu.Lock()
	if e, ok := siegeCapacityOf[key]; ok && time.Since(e.at) < siegeCapacityTTL {
		siegeCapacityMu.Unlock()
		return e.cap, e.ps
	}
	siegeCapacityMu.Unlock()

	rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID, ch.corporationID
		  FROM rolebot_characters b
		  JOIN character_ ch ON ch.characterID = b.characterID
		  LEFT JOIN rolebot_state s ON s.characterID = b.characterID
		 WHERE b.enabled = 1 AND COALESCE(s.halted, 0) = 0
		   AND ch.corporationID IN (`+key+`)
		   AND NOT EXISTS (
		       SELECT 1 FROM rolebot_gang_members gm
		         JOIN rolebot_gangs g ON g.gangID = gm.gangID AND g.enabled = 1
		        WHERE gm.characterID = b.characterID AND g.activity NOT IN (?, ?))`,
		ActSecurity, ActCombat)
	if err != nil {
		return siegeCapacity{}, nil
	}
	var ps []siegePilot
	for rows.Next() {
		var p siegePilot
		if rows.Scan(&p.CharID, &p.CorpID) == nil {
			p.Answer = siegeAnswerOdds(p.CharID, p.CorpID)
			ps = append(ps, p)
		}
	}
	rows.Close()
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Answer != ps[j].Answer {
			return ps[i].Answer > ps[j].Answer
		}
		return ps[i].CharID < ps[j].CharID
	})
	if len(ps) > siegeCapacityPilots {
		// Cloned: the cache keeps this slice, and a re-slice would pin every enabled pilot read.
		ps = slices.Clone(ps[:siegeCapacityPilots])
	}
	for i := range ps {
		ps[i].OwnedDPS = m.pilotSiegeDPS(ctx, ps[i].CharID)
	}
	m.siegeKitCapacity(ctx, sorted, ps)
	c := sumSiegeCapacity(ps)
	siegeCapacityMu.Lock()
	siegeCapacityOf[key] = struct {
		cap siegeCapacity
		ps  []siegePilot
		at  time.Time
	}{c, ps, time.Now()}
	siegeCapacityMu.Unlock()
	return c, ps
}

// siegeShortLine is the CEO's note when the side cannot field a target's quota. Pure.
func siegeShortLine(target string, have, quota float64) string {
	return fmt.Sprintf("wants %s, but the wing can muster ~%.0f DPS of the %.0f it takes to break it in %s",
		target, have, quota, (time.Duration(siegeWindowSec) * time.Second).String())
}

// mateJoinsTowerSiege reports whether an alliance mate's pilots may join a member's tower siege:
// only a mate at war with the tower's owner, or anyone in 0.0, where no CONCORD answers. Pure.
func mateJoinsTowerSiege(mateAtWarWithDefender bool, sec float64) bool {
	return mateAtWarWithDefender || sec < 0
}

// towerSiegeCorps is the corporations whose pilots fight corpID's tower siege on a tower owned by
// defender in a system of the given security: corpID and the mates mateJoinsTowerSiege lets in.
func (m *Mod) towerSiegeCorps(ctx context.Context, corpID, defender int64, sec float64) []int64 {
	out := []int64{corpID}
	for _, mate := range m.allianceMemberCorps(ctx, corpID) {
		if mate == corpID {
			continue
		}
		if mateJoinsTowerSiege(slices.Contains(relWarEnemiesOf(mate), defender), sec) {
			out = append(out, mate)
		}
	}
	return out
}

// ── stages ─────────────────────────────────────────────────────────────────

// siegeStageIn is what the stage machine reads about one siege pass.
type siegeStageIn struct {
	Stage                          string
	StageAt, OpenedAt, Now, LowSince int64
	FleetLive                      bool    // the siege's fleet is out (not still mustering)
	FleetDPS                       float64 // the attackers present, all of them
	Regen                          float64 // the shield's peak regeneration, EHP per second
	Window                         float64 // the bash window, seconds
}

// siegeStageOut is the stage machine's verdict: the next stage, the low-DPS clock, and a note
// when the siege is to be abandoned.
type siegeStageOut struct {
	Stage    string
	LowSince int64
	Abandon  string
}

// siegeStageNext moves a muster, bash or kill stage along on DPS, not heads. A muster opens fire
// once the fleet is out and out-shoots the shield's regeneration, and is given up after
// siegeMusterFormSec. A bash or kill never falls back to a muster (that flapping reset every
// clock): it runs while the fleet out-shoots the regeneration, is abandoned once it has not for
// siegeLowGraceSec, and is abandoned when it overruns siegeBashOverrun windows. Pure.
func siegeStageNext(in siegeStageIn) siegeStageOut {
	out := siegeStageOut{Stage: in.Stage, LowSince: in.LowSince}
	start := in.StageAt
	if start == 0 {
		start = in.OpenedAt
	}
	window := in.Window
	if window <= 0 {
		window = siegeWindowSec
	}
	ahead := in.FleetDPS > 0 && in.FleetDPS > in.Regen
	switch in.Stage {
	case "muster":
		if in.FleetLive && ahead {
			out.Stage, out.LowSince = "bash", 0
			return out
		}
		if start > 0 && in.Now-start > siegeMusterFormSec {
			out.Abandon = "the fleet never formed"
		}
		return out
	case "bash", "kill":
		if start > 0 && float64(in.Now-start) > window*siegeBashOverrun {
			out.Abandon = fmt.Sprintf("the %s overran its window — %s without breaking it",
				in.Stage, (time.Duration(in.Now-start) * time.Second).Round(time.Minute))
			return out
		}
		if ahead {
			out.LowSince = 0
			return out
		}
		if out.LowSince == 0 {
			out.LowSince = in.Now
			return out
		}
		if in.Now-out.LowSince > siegeLowGraceSec {
			if in.FleetDPS <= 0 {
				out.Abandon = "the fleet left before the shield broke"
			} else {
				out.Abandon = fmt.Sprintf("the fleet could not out-shoot the shield's regeneration (%.0f of %.0f DPS)",
					in.FleetDPS, in.Regen)
			}
		}
	}
	return out
}

// siegeFleetDPS is the DPS of the attackers present. Pure.
func siegeFleetDPS(att []*pvpCombatant) float64 {
	t := 0.0
	for _, a := range att {
		t += a.DPS
	}
	return t
}

// siegeSidesBusy reports whether an alliance already has an attacking siege open: one alliance op
// at a time, so its pilots are not split between two targets.
func (m *Mod) siegeSidesBusy(ctx context.Context, corps []int64) bool {
	if len(corps) == 0 {
		return false
	}
	return len(m.openSieges(ctx, `attackerCorpID IN (`+int64CSV(corps)+`)`)) > 0
}

// siegeQuotaNote is the quota part of a siege announcement. Pure.
func siegeQuotaNote(q siegeQuotaOut) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%.0f DPS for %s", q.OpenDPS(), (time.Duration(siegeWindowSec) * time.Second).String())
	if q.Regen > 0 {
		fmt.Fprintf(&b, " (shield regenerates %.0f/s)", q.Regen)
	}
	return b.String()
}
