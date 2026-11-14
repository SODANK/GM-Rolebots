package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Agent missions: a combat pilot asks an agent for work, weighs the offer, and runs it. Nothing here
// knows any one mission: the template's fields pick the runner (missionKindOf). A kill pocket is
// served through the site seam under a negative site ID, so siteRunStep runs it; courier cargo and
// trade or mining goods are hauled. Offers, rewards, standings, LP, chains and cooldowns are the
// core's, through modapi.Missions() (handleAgentDoAction with no connection). A template enabled in
// ESP /missions is in rotation at once. docs/MISSIONS.md.

const (
	// tacticMission is the combat trip that works for an agent (combattactic.go).
	tacticMission = "mission"

	TaskMissionFetch  TaskKind = "mission.fetch"
	TaskMissionRun    TaskKind = "mission.run"
	TaskMissionHaul   TaskKind = "mission.haul"
	TaskMissionTurnIn TaskKind = "mission.turnin"

	missionKindKill     = "kill"
	missionKindKillItem = "killItem"
	missionKindCourier  = "courier"
	missionKindSupply   = "supply"

	settingMissionsEnabled   = "rolebots.missions.enabled"
	settingMissionsWeightPct = "rolebots.missions.weightPct"
	settingMissionsMaxJumps  = "rolebots.missions.maxJumps"

	defaultMissionsEnabled   = 1
	defaultMissionsWeightPct = 140
	defaultMissionsMaxJumps  = 6

	// missionAgentChecks bounds the standing and cooldown reads one agent pick makes.
	missionAgentChecks = 24
	// missionAgentTTL is how long a pilot's agent pick holds between the weighing and the trip.
	missionAgentTTL = 5 * time.Minute
	// missionOfferOddsMin is the least chance an agent's offer is one the pilot takes (its ship
	// finishes the pocket, the destination is in its band) for the agent to be worth the trip.
	missionOfferOddsMin = 0.5
	// missionAgentSkipSec is how long an agent that offered nothing worth taking is left alone.
	missionAgentSkipSec = 2 * 3600
	// missionLevelCapSec is how long a pilot keeps below a level whose fight it turned down.
	missionLevelCapSec = 6 * 3600
	// missionSupplyRewardShare is the most of the reward a supply mission may spend on the goods.
	missionSupplyRewardShare = 0.8
	// missionSupplyTries is how many empty books a supply run meets before it gives up.
	missionSupplyTries = 2
	// missionLaneBase keeps a pocket's arrival-lane key clear of every real ball and gate key.
	missionLaneBase = int64(1) << 40
)

var (
	missionsEnabledVal   atomic.Int64
	missionsWeightPctVal atomic.Int64
	missionsMaxJumpsVal  atomic.Int64
)

func init() {
	missionsEnabledVal.Store(defaultMissionsEnabled)
	missionsWeightPctVal.Store(defaultMissionsWeightPct)
	missionsMaxJumpsVal.Store(defaultMissionsMaxJumps)

	combatTactics = append(combatTactics, combatTactic{
		// Working for an agent. Weighed only when an agent this pilot may use is in reach; the offer
		// is judged at the agent's desk (missionEvaluate) and turned down when it is out of reach.
		Slug: tacticMission, Label: "agent mission", Task: TaskMissionFetch, Focus: focusPvE,
		StintSec: siteRunMaxSec,
		Weight: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) float64 {
			return m.missionTacticWeight(ctx, b, cfg)
		},
		Site: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error) {
			a := m.missionAgentFor(ctx, b, cfg)
			if a == nil {
				return 0, 0, "", nil
			}
			return a.SystemID, a.AgentID, fmt.Sprintf("%s (level %d) in %s",
				evedb.GetAgentName(ctx, m.db, a.AgentID), a.Level, m.nameOfSystem(ctx, a.SystemID)), nil
		},
	})

	registerTask(TaskDef{Kind: TaskMissionFetch, Label: "Asking an agent for work",
		Posture: PostureDocked, Detail: detailMission, Step: (*Mod).missionFetchStep})
	registerTask(TaskDef{Kind: TaskMissionRun, Label: "Running an agent mission",
		Posture: PostureCombatBelt, Exposed: true, Loots: true,
		Detail: detailMission, Step: (*Mod).missionRunStep})
	registerTask(TaskDef{Kind: TaskMissionHaul, Label: "Carrying cargo for an agent",
		Posture: PostureDocked, Detail: detailMission, Step: (*Mod).missionHaulStep})
	registerTask(TaskDef{Kind: TaskMissionTurnIn, Label: "Turning a mission in",
		Posture: PostureDocked, Detail: detailMission, Step: (*Mod).missionTurnInStep})

	registerCharForget("missionMemory", func(id int64) {
		missionPicks.Delete(id)
		missionLevelCaps.Delete(id)
		missionSkipMu.Lock()
		for k := range missionSkips {
			if k[0] == id {
				delete(missionSkips, k)
			}
		}
		missionSkipMu.Unlock()
	})
}

// MissionsEnabled is the master switch; it is also off on a core without either seam.
func MissionsEnabled() bool {
	return missionsEnabledVal.Load() != 0 && modapi.CanRunMissions() && modapi.CanRunSites()
}

// MissionsWeight is the tactic's weight against the belt sweep's 1.0.
func MissionsWeight() float64 { return float64(missionsWeightPctVal.Load()) / 100 }

// MissionsMaxJumps is how far a pilot looks for an agent, and how far it will fly for one
// mission's destination.
func MissionsMaxJumps() int { return int(missionsMaxJumpsVal.Load()) }

func (m *Mod) loadMissionSettings(ctx context.Context) {
	for _, s := range []struct {
		key      string
		val      *atomic.Int64
		min, max int64
	}{
		{settingMissionsEnabled, &missionsEnabledVal, 0, 1},
		{settingMissionsWeightPct, &missionsWeightPctVal, 0, 1000},
		{settingMissionsMaxJumps, &missionsMaxJumpsVal, 0, 30},
	} {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, s.key)
		if err != nil || !ok {
			continue
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= s.min && n <= s.max {
			s.val.Store(n)
		}
	}
}

// missionPocketLaneID is the arrival-lane key of a mission pocket, which has no beacon ball.
func missionPocketLaneID(missionID int64) int64 { return -(missionLaneBase + missionID) }

// ── what a template asks for ────────────────────────────────────────────────

// missionKindOf reads what a template's work is. An empty kind means the bots cannot run it; the
// second value says why, and the decline logs it so an author sees it.
func missionKindOf(t *evedb.MissionTemplate) (string, string) {
	if t == nil {
		return "", "the template is missing"
	}
	switch t.MissionType {
	case "kill":
		if t.DungeonVID <= 0 {
			return "", "a kill mission with no dungeon"
		}
		if t.DungeonRoomIdx > 1 {
			return "", fmt.Sprintf("the pocket starts in room %d, and runs start in room 1", t.DungeonRoomIdx)
		}
		switch t.CompletionType {
		case "killAll", "killTarget", "killStructure", "":
			return missionKindKill, ""
		case "killItem":
			if t.KillItemTypeID <= 0 || t.KillItemQty <= 0 {
				return "", "a killItem mission names no item"
			}
			if t.KillItemDropoffMode == "nearby" {
				return "", "the killItem drop-off is drawn anew at every turn-in"
			}
			return missionKindKillItem, ""
		}
		return "", fmt.Sprintf("completion type %q is unknown", t.CompletionType)
	case "courier":
		if t.CourierTypeID <= 0 || t.CourierQty <= 0 {
			return "", "a courier mission names no cargo"
		}
		return missionKindCourier, ""
	case "trade", "mining":
		if typeID, qty := missionSupplyOf(t); typeID <= 0 || qty <= 0 {
			return "", fmt.Sprintf("a %s mission names no goods", t.MissionType)
		}
		return missionKindSupply, ""
	}
	return "", fmt.Sprintf("mission type %q is unknown", t.MissionType)
}

// missionSupplyOf is what a trade or mining mission wants brought to the agent.
func missionSupplyOf(t *evedb.MissionTemplate) (typeID int64, qty int) {
	if t == nil {
		return 0, 0
	}
	switch t.MissionType {
	case "trade":
		return t.TradeTypeID, t.TradeQty
	case "mining":
		return t.MiningTypeID, t.MiningQty
	}
	return 0, 0
}

// missionCargoOf is the item a delivery run carries (courier, supply, killItem), or 0.
func missionCargoOf(kind string, t *evedb.MissionTemplate) (typeID int64, qty int) {
	if t == nil {
		return 0, 0
	}
	switch kind {
	case missionKindCourier:
		return t.CourierTypeID, t.CourierQty
	case missionKindSupply:
		return missionSupplyOf(t)
	case missionKindKillItem:
		return t.KillItemTypeID, t.KillItemQty
	}
	return 0, 0
}

// missionTurnInStation is where the work is handed in: the drop-off for a delivery (the core counts
// the cargo there), the agent's desk otherwise.
func missionTurnInStation(kind string, ms *evedb.CharacterMission, t *evedb.MissionTemplate, agentStation int64) int64 {
	switch kind {
	case missionKindCourier:
		if ms != nil && ms.DropoffStationID != 0 {
			return ms.DropoffStationID
		}
	case missionKindKillItem:
		if t != nil && t.KillItemDropoffMode == "specific" && t.KillItemDropoffStationID != 0 {
			return t.KillItemDropoffStationID
		}
	}
	return agentStation
}

// missionFactionWelcome is whether a pilot will work for an agent of faction fid: never a pirate
// faction, never its employer's enemy, never a faction it dislikes. A factionless agent is fine.
func missionFactionWelcome(mx map[[2]int64]float64, mine, fid int64, pirates []int64, p *Profile) bool {
	if fid == 0 {
		return true
	}
	for _, id := range pirates {
		if id == fid {
			return false
		}
	}
	if mine != 0 && mine != fid && factionAtOdds(mx, mine, fid) {
		return false
	}
	return p == nil || p.StandingTo(fid) > StandingDisliked
}

// ── finding an agent ────────────────────────────────────────────────────────

// missionAgent is one agent a pilot could ask, with what the gate needs.
type missionAgent struct {
	AgentID, DivisionID, CorpID, FactionID, StationID, SystemID int64
	Level, Quality                                              int
	Jumps                                                       int
	Odds                                                        float64 // missionOfferOdds
}

func (a missionAgent) info() *evedb.AgentInfo {
	return &evedb.AgentInfo{AgentID: a.AgentID, DivisionID: a.DivisionID, Level: a.Level,
		Quality: a.Quality, AgentTypeID: 2, CorporationID: a.CorpID,
		StationID: a.StationID, SolarSystemID: a.SystemID}
}

type missionPick struct {
	agent *missionAgent
	at    time.Time
}

type missionCap struct {
	level int
	until int64
}

var (
	// Every basic agent with a station: static data, read once an hour.
	missionAgentsMu  sync.Mutex
	missionAgentsAll []missionAgent
	missionAgentsAt  time.Time

	missionPicks     sync.Map // charID to missionPick
	missionLevelCaps sync.Map // charID to missionCap

	missionSkipMu sync.Mutex
	missionSkips  = map[[2]int64]int64{} // (charID, agentID) to until
)

func (m *Mod) missionAgentRows(ctx context.Context) ([]missionAgent, error) {
	missionAgentsMu.Lock()
	defer missionAgentsMu.Unlock()
	if missionAgentsAll != nil && clock.Now().Sub(missionAgentsAt) < time.Hour {
		return missionAgentsAll, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT a.agentID, COALESCE(a.divisionID, 0), COALESCE(a.corporationID, 0),
		       COALESCE(c.factionID, 0), a.stationID, s.solarSystemID,
		       COALESCE(a.level, 1), COALESCE(a.quality, 0)
		  FROM agtAgents a
		  JOIN staStations s ON s.stationID = a.stationID
		  LEFT JOIN crpNPCCorporations c ON c.corporationID = a.corporationID
		 WHERE a.agentTypeID = 2 AND COALESCE(a.level, 1) BETWEEN 1 AND 4`)
	if err != nil {
		return nil, fmt.Errorf("mission agents: %w", err)
	}
	defer rows.Close()
	var out []missionAgent
	for rows.Next() {
		var a missionAgent
		if err := rows.Scan(&a.AgentID, &a.DivisionID, &a.CorpID, &a.FactionID,
			&a.StationID, &a.SystemID, &a.Level, &a.Quality); err != nil {
			return nil, fmt.Errorf("mission agents scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	missionAgentsAll, missionAgentsAt = out, clock.Now()
	return out, nil
}

// missionLevelCapFor is the highest agent level this pilot asks at now.
func missionLevelCapFor(charID int64) int {
	if v, ok := missionLevelCaps.Load(charID); ok {
		c := v.(missionCap)
		if clock.Unix() < c.until {
			return c.level
		}
		missionLevelCaps.Delete(charID)
	}
	return 4
}

// noteMissionTooHard keeps the pilot below an agent level whose fight it turned down.
func noteMissionTooHard(charID int64, level int) {
	if level <= 1 {
		return
	}
	missionLevelCaps.Store(charID, missionCap{level: level - 1, until: clock.Unix() + missionLevelCapSec})
}

func missionSkipped(charID, agentID int64) bool {
	missionSkipMu.Lock()
	defer missionSkipMu.Unlock()
	until, ok := missionSkips[[2]int64{charID, agentID}]
	if ok && clock.Unix() >= until {
		delete(missionSkips, [2]int64{charID, agentID})
		return false
	}
	return ok
}

func noteMissionSkip(charID, agentID int64) {
	missionSkipMu.Lock()
	missionSkips[[2]int64{charID, agentID}] = clock.Unix() + missionAgentSkipSec
	missionSkipMu.Unlock()
	missionPicks.Delete(charID)
}

// missionAgentFor is the agent this pilot would ask now: in reach and in its security band, of a
// faction it will work for, at the highest level its standing opens and its record allows, nearest
// first. Nil when there is none. Held for missionAgentTTL so the weighing and the trip agree.
func (m *Mod) missionAgentFor(ctx context.Context, b *botRun, cfg combatConfig) *missionAgent {
	if v, ok := missionPicks.Load(b.CharID); ok {
		p := v.(missionPick)
		if clock.Now().Sub(p.at) < missionAgentTTL {
			return p.agent
		}
	}
	a := m.missionAgentPick(ctx, b, cfg)
	missionPicks.Store(b.CharID, missionPick{agent: a, at: clock.Now()})
	return a
}

func (m *Mod) missionAgentPick(ctx context.Context, b *botRun, cfg combatConfig) *missionAgent {
	all, err := m.missionAgentRows(ctx)
	if err != nil {
		rlogf("rolebots: %v", err)
		return nil
	}
	u, err := m.universe(ctx)
	if err != nil {
		return nil
	}
	// Only agents likely to offer work this pilot takes count (missiondeck.go): a level, faction or
	// division whose templates it cannot run, whose pockets its ship would not finish, or whose
	// destinations fall outside its band is never flown to, and the pick falls back to a level
	// that does better.
	deck, err := m.missionDeck(ctx)
	if err != nil {
		rlogf("rolebots: %v", err)
		deck = nil
	}
	near := hopsWithin(u, b.SystemID, MissionsMaxJumps())
	mx, mxOK := m.standingsMatrix(ctx)
	mine := b.FactionID
	if !mxOK {
		mine = 0
	}
	pirates := m.pirateFactionIDs(ctx)
	levelCap := missionLevelCapFor(b.CharID)
	cardOdds := m.missionCardOdds(ctx, b, cfg, u)
	var cands []missionAgent
	for _, a := range all {
		jumps, ok := near[a.SystemID]
		if !ok || a.Level > levelCap {
			continue
		}
		if ms, ok := u.meta[a.SystemID]; !ok || ms.Security < cfg.SecMin || ms.Security > cfg.SecMax {
			continue
		}
		if !missionFactionWelcome(mx, mine, a.FactionID, pirates, b.Profile) || missionSkipped(b.CharID, a.AgentID) {
			continue
		}
		odds := deck.odds(a.Level, a.FactionID, a.DivisionID, cardOdds(a.SystemID))
		if odds < missionOfferOddsMin {
			continue
		}
		a.Jumps, a.Odds = jumps, odds
		cands = append(cands, a)
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Level != cands[j].Level {
			return cands[i].Level > cands[j].Level
		}
		if bi, bj := missionOddsBucket(cands[i].Odds), missionOddsBucket(cands[j].Odds); bi != bj {
			return bi > bj
		}
		if cands[i].Jumps != cands[j].Jumps {
			return cands[i].Jumps < cands[j].Jumps
		}
		return cands[i].AgentID < cands[j].AgentID
	})
	checks := 0
	for i := range cands {
		if ctx.Err() != nil || checks >= missionAgentChecks {
			break
		}
		a := cands[i]
		checks++
		// The gate the conversation applies, then the agent's mission cap.
		if !evedb.CanUseAgent(ctx, m.db, b.CharID, a.info()).Allowed {
			continue
		}
		if onCD, _, err := evedb.AgentMissionCooldown(ctx, m.db, b.CharID, a.AgentID); err != nil || onCD {
			continue
		}
		// Only the first decline in the agent's window is free (RecentAgentDecline): an agent this
		// pilot turned down lately is left alone until the window passes, so the bots never pay.
		if recent, _, err := evedb.RecentAgentDecline(ctx, m.db, b.CharID, a.AgentID); err != nil || recent {
			continue
		}
		return &a
	}
	return nil
}

// missionOddsBucket groups offer odds coarsely, so a nearer agent wins over a farther one with
// nearly the same odds: [0.5, 0.75) and [0.75, 1].
func missionOddsBucket(odds float64) int {
	if odds >= 1 {
		odds = 0.999
	}
	return int(odds * 4)
}

// missionCardOdds answers, per agent system, the chance this pilot takes an offer of one
// template: the judgement missionEvaluate makes on a real offer, read from the template alone. A
// kill whose pocket its ship would not finish (missionKillReady's gate) is 0; a kill system or a
// courier's nearby pickup and drop-off counts its chance of falling in the pilot's band
// (destInBandShare). Pocket and band reads are kept for the one pick.
func (m *Mod) missionCardOdds(ctx context.Context, b *botRun, cfg combatConfig, u *universe) func(agentSys int64) func(deckCard) float64 {
	dps, ehp, rep, known := siteSidePower(m.effectiveHulls(ctx, b), b.Tune.BreakOffFrac)
	pocketOK := map[int64]bool{}
	finishes := func(vid int64) bool {
		if ok, seen := pocketOK[vid]; seen {
			return ok
		}
		ok := false
		if vid > 0 {
			if card, err := m.siteCardFor(ctx, vid); err == nil && card != nil && len(card.Rooms) > 0 {
				// An unread hull flies, as missionKillReady lets it.
				ok = !known || siteWouldRun(b.Tune, siteMarginRep(dps, ehp, rep, card))
			}
		}
		pocketOK[vid] = ok
		return ok
	}
	bandsBy := map[int64][][]int64{}
	return func(agentSys int64) func(deckCard) float64 {
		inBand := func(c deckCard) float64 {
			bands, ok := bandsBy[agentSys]
			if !ok {
				bands = jumpBands(u, agentSys)
				bandsBy[agentSys] = bands
			}
			return destInBandShare(bands, u.meta, c.MinSec, c.MaxSec, cfg.SecMin, cfg.SecMax)
		}
		return func(c deckCard) float64 {
			switch c.Type {
			case "kill":
				if !finishes(c.DungeonVID) {
					return 0
				}
				return inBand(c)
			case "courier":
				p := 1.0
				if c.PickupNearby {
					p *= inBand(c)
				}
				if c.DropoffNearby {
					p *= inBand(c)
				}
				return p
			}
			return 1 // trade and mining deliver to the agent's own station
		}
	}
}

// missionTacticWeight is the tactic's weight: the setting when an agent is in reach, else nothing.
// A gang member flies the gang's op instead.
func (m *Mod) missionTacticWeight(ctx context.Context, b *botRun, cfg combatConfig) float64 {
	if !MissionsEnabled() || b.State == nil || b.GangID != 0 || b.State.Data.GangID != 0 {
		return 0
	}
	if m.missionAgentFor(ctx, b, cfg) == nil {
		return 0
	}
	return MissionsWeight()
}

// ── the trip ────────────────────────────────────────────────────────────────

// missionBegin starts the trip to the agent the tactic picked (combatPlan).
func (m *Mod) missionBegin(ctx context.Context, b *botRun, agentID int64, label string) error {
	st := b.State
	missionPicks.Delete(b.CharID)
	info, err := evedb.GetAgentInfo(ctx, m.db, agentID)
	if err != nil {
		return err
	}
	if info.StationID == 0 {
		return fmt.Errorf("agent %d has no station", agentID)
	}
	st.Data.MissionState = MissionState{MissionAgentID: agentID, MissionAgentStation: info.StationID,
		MissionStartedAt: clock.Unix()}
	m.logBot(b.CharID, logInfo, "mission", "going to ask %s for work", label)
	return m.beginTravel(ctx, b, info.SolarSystemID, info.StationID, string(TaskMissionFetch))
}

// missionHeldResume is planSolo's check before the appetite draw: a pilot holding an open mission
// goes back to it through combatPlan, which boards the combat ship first and then resumes
// (missionResume). Without a ship to fly it in, the mission is handed back rather than left to
// expire. False when no mission is held.
func (m *Mod) missionHeldResume(ctx context.Context, b *botRun) (bool, error) {
	d := &b.State.Data
	if d.MissionID == 0 {
		if d.MissionAgentID != 0 {
			d.MissionState = MissionState{} // never got past the desk
		}
		return false, nil
	}
	ms, tmpl := m.missionLoad(ctx, d.MissionID)
	if ms == nil || (ms.State != "accepted" && ms.State != "offered") {
		m.logBot(b.CharID, logInfo, "mission", "mission %d is no longer open — letting it go", d.MissionID)
		d.MissionState = MissionState{}
		return false, nil
	}
	if ms.State == "accepted" && tmpl == nil {
		return m.missionGone(ctx, b, ms, tmpl) // nothing to go back to: hand it in now, no ship needed
	}
	err := m.combatPlan(ctx, b)
	if errors.Is(err, errNoFitAvailable) || errors.Is(err, errCannotAfford) {
		if ms.State == "offered" {
			d.MissionState = MissionState{}
			return false, nil
		}
		m.logBot(b.CharID, logInfo, "mission", "no ship to finish %s in (%v)", missionNameOf(tmpl, ms), err)
		return true, m.missionQuit(ctx, b, "it has no ship to finish it in")
	}
	return true, err
}

// missionResume puts a pilot with a mission in hand back on it (combatPlan, after a reset such as
// a lost hull or a sale run). False when there is nothing to resume.
func (m *Mod) missionResume(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	if d.MissionAgentID == 0 {
		return false, nil
	}
	if d.MissionID == 0 {
		// Never got past the desk: forget it, the next pick starts fresh.
		d.MissionState = MissionState{}
		return false, nil
	}
	ms, tmpl := m.missionLoad(ctx, d.MissionID)
	if ms == nil || (ms.State != "accepted" && ms.State != "offered") {
		m.logBot(b.CharID, logInfo, "mission", "mission %d is no longer open — letting it go", d.MissionID)
		d.MissionState = MissionState{}
		return false, nil
	}
	st.Activity = ActCombat
	if ms.State == "offered" {
		st.Phase = string(TaskMissionFetch)
		return true, saveState(ctx, m.db, st)
	}
	if d.MissionKind == missionKindKill || d.MissionKind == missionKindKillItem {
		// A pilot back from losing its hull fights the pocket again only if the new one can.
		if why := m.missionKillReady(ctx, b, tmpl); why != "" {
			return true, m.missionQuit(ctx, b, why)
		}
		st.Phase = string(TaskMissionRun)
	} else {
		st.Phase = string(TaskMissionHaul)
	}
	m.logBot(b.CharID, logInfo, "mission", "back to mission %d", d.MissionID)
	return true, saveState(ctx, m.db, st)
}

func (m *Mod) missionLoad(ctx context.Context, missionID int64) (*evedb.CharacterMission, *evedb.MissionTemplate) {
	ms, err := evedb.GetMissionByID(ctx, m.db, missionID)
	if err != nil || ms == nil {
		return nil, nil
	}
	tmpl, _ := evedb.GetTemplateByID(ctx, m.db, ms.TemplateID)
	return ms, tmpl
}

// missionFetchStep is the agent's desk: ask, weigh the offer, accept or decline.
func (m *Mod) missionFetchStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	if d.MissionAgentID == 0 || d.MissionAgentStation == 0 {
		return false, m.missionIdle(ctx, b)
	}
	if b.StationID != d.MissionAgentStation {
		return false, m.beginTravel(ctx, b, m.stationSystemOf(ctx, d.MissionAgentStation),
			d.MissionAgentStation, string(TaskMissionFetch))
	}
	h := modapi.Missions()
	if h.Request == nil {
		return false, m.missionIdle(ctx, b)
	}
	agentName := evedb.GetAgentName(ctx, m.db, d.MissionAgentID)
	mid, why, err := h.Request(b.CharID, 0, d.MissionAgentID, b.StationID)
	if err != nil {
		return false, err
	}
	if mid == 0 {
		m.logBot(b.CharID, logInfo, "mission", "%s has no work for it — %s", agentName, why)
		noteMissionSkip(b.CharID, d.MissionAgentID)
		// The deck said this agent had work: it changed since, so the next pick reads it afresh.
		invalidateMissionDeck()
		return false, m.missionIdle(ctx, b)
	}
	ms, tmpl := m.missionLoad(ctx, mid)
	if ms == nil {
		return false, m.missionIdle(ctx, b)
	}
	if ms.State == "accepted" && ms.MissionID != d.MissionID {
		// A mission from an earlier life nobody is flying: hand it back so the agent will talk.
		m.logBot(b.CharID, logInfo, "mission", "%s still holds an old mission for it (%d) — quitting it", agentName, mid)
		d.MissionID = mid
		return false, m.missionQuit(ctx, b, "an abandoned mission from before")
	}
	if ms.State == "accepted" {
		return m.missionResume(ctx, b)
	}

	kind, why := missionKindOf(tmpl)
	if kind != "" {
		why = m.missionEvaluate(ctx, b, kind, ms, tmpl)
	}
	name := missionNameOf(tmpl, ms)
	if why != "" {
		if err := h.Decline(b.CharID, 0, d.MissionAgentID, b.StationID); err != nil {
			m.logBot(b.CharID, logWarn, "mission", "declining %s: %v", name, err)
		}
		m.logBot(b.CharID, logInfo, "mission", "declined %s from %s — %s", name, agentName, why)
		m.recordMissionRun(ctx, b, ms, kind, "declined", why)
		noteMissionSkip(b.CharID, d.MissionAgentID)
		return false, m.missionIdle(ctx, b)
	}
	if err := h.Accept(b.CharID, 0, d.MissionAgentID, b.StationID); err != nil {
		m.logBot(b.CharID, logWarn, "mission", "accepting %s: %v", name, err)
		noteMissionSkip(b.CharID, d.MissionAgentID)
		return false, m.missionIdle(ctx, b)
	}
	ms, _ = m.missionLoad(ctx, mid)
	if ms == nil {
		return false, m.missionIdle(ctx, b)
	}
	d.MissionID, d.MissionTemplateID, d.MissionKind = mid, ms.TemplateID, kind
	d.MissionTurnIn = missionTurnInStation(kind, ms, tmpl, d.MissionAgentStation)
	d.MissionSource, d.MissionTries = 0, 0
	m.logBot(b.CharID, logInfo, "mission", "accepted %s from %s (%s, %s, %d LP)",
		name, agentName, kind, fmtISK(float64(ms.RewardISK)), ms.RewardLP)
	if kind == missionKindKill || kind == missionKindKillItem {
		st.Phase = string(TaskMissionRun)
	} else {
		st.Phase = string(TaskMissionHaul)
	}
	return false, saveState(ctx, m.db, st)
}

func missionNameOf(t *evedb.MissionTemplate, ms *evedb.CharacterMission) string {
	if t != nil && t.MissionName != "" {
		return fmt.Sprintf("%q (template %d)", t.MissionName, t.TemplateID)
	}
	if ms != nil {
		return fmt.Sprintf("mission %d (template %d)", ms.MissionID, ms.TemplateID)
	}
	return "the mission"
}

// missionEvaluate weighs an offer from its data alone. "" means take it; anything else says why
// it is turned down.
func (m *Mod) missionEvaluate(ctx context.Context, b *botRun, kind string,
	ms *evedb.CharacterMission, tmpl *evedb.MissionTemplate) string {
	cfg := m.combatConfigDerived(ctx, b)
	switch kind {
	case missionKindKill, missionKindKillItem:
		if ms.DestSystemID == 0 {
			return "the offer carries no destination (a chained mission)"
		}
		if why := m.missionDestOK(ctx, b, cfg, ms.DestSystemID); why != "" {
			return why
		}
		if why := m.missionKillReady(ctx, b, tmpl); why != "" {
			if agent, err := evedb.GetAgentInfo(ctx, m.db, ms.AgentID); err == nil {
				noteMissionTooHard(b.CharID, agent.Level)
			}
			return why
		}
		if kind == missionKindKillItem {
			if drop := missionTurnInStation(kind, ms, tmpl, b.State.Data.MissionAgentStation); drop != b.State.Data.MissionAgentStation {
				if why := m.missionDestOK(ctx, b, cfg, m.stationSystemOf(ctx, drop)); why != "" {
					return "the drop-off: " + why
				}
			}
		}
	case missionKindCourier:
		if ms.PickupStationID == 0 {
			return "the cargo has no pickup station"
		}
		for _, stn := range []int64{ms.PickupStationID, missionTurnInStation(kind, ms, tmpl, b.State.Data.MissionAgentStation)} {
			if why := m.missionDestOK(ctx, b, cfg, m.stationSystemOf(ctx, stn)); why != "" {
				return why
			}
		}
		if why := m.missionFits(ctx, b, tmpl.CourierTypeID, tmpl.CourierQty); why != "" {
			return why
		}
	case missionKindSupply:
		typeID, qty := missionSupplyOf(tmpl)
		if why := m.missionFits(ctx, b, typeID, qty); why != "" {
			return why
		}
		src, cost := m.missionSupplySource(ctx, b, typeID, int64(qty))
		if src == 0 {
			return fmt.Sprintf("nobody within %d jumps sells %d of type %d", MissionsMaxJumps(), qty, typeID)
		}
		reward := float64(ms.RewardISK + ms.BonusISK)
		if cost > reward*missionSupplyRewardShare {
			return fmt.Sprintf("the goods cost about %s against a %s reward", fmtISK(cost), fmtISK(reward))
		}
		if bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID); err != nil || bal < cost*1.25 {
			return fmt.Sprintf("it cannot afford the goods (about %s)", fmtISK(cost))
		}
	default:
		return fmt.Sprintf("kind %q has no runner", kind)
	}
	return ""
}

// missionDestOK is whether a mission system is in the pilot's security band and in reach.
func (m *Mod) missionDestOK(ctx context.Context, b *botRun, cfg combatConfig, sysID int64) string {
	if sysID == 0 {
		return "the destination is unknown"
	}
	u, err := m.universe(ctx)
	if err != nil {
		return "the map is unreadable"
	}
	if ms, ok := u.meta[sysID]; !ok || ms.Security < cfg.SecMin || ms.Security > cfg.SecMax {
		return fmt.Sprintf("%s is outside its security band", m.nameOfSystem(ctx, sysID))
	}
	jumps, err := m.jumpsBetween(ctx, b, b.SystemID, sysID)
	if err != nil {
		return fmt.Sprintf("no road it will fly to %s", m.nameOfSystem(ctx, sysID))
	}
	if jumps > 2*MissionsMaxJumps() {
		return fmt.Sprintf("%s is %d jumps away", m.nameOfSystem(ctx, sysID), jumps)
	}
	return ""
}

// missionKillReady is the site gate on the pocket (sitecard.go): "" when this pilot's ship would
// finish it.
func (m *Mod) missionKillReady(ctx context.Context, b *botRun, tmpl *evedb.MissionTemplate) string {
	if tmpl == nil || tmpl.DungeonVID <= 0 {
		return "the pocket is unknown"
	}
	card, err := m.siteCardFor(ctx, tmpl.DungeonVID)
	if err != nil || card == nil || len(card.Rooms) == 0 {
		return "the pocket has no rooms"
	}
	dps, ehp, rep, known := siteSidePower(m.effectiveHulls(ctx, b), b.Tune.BreakOffFrac)
	if !known {
		return "" // an unread hull flies, as the belt walk does
	}
	if margin := siteMarginRep(dps, ehp, rep, card); !siteWouldRun(b.Tune, margin) {
		return fmt.Sprintf("%s is too much for its ship (%.0f DPS / %.0f EHP)", card.Name, dps, ehp)
	}
	return ""
}

// missionFits is whether qty of a type goes in this ship's hold.
func (m *Mod) missionFits(ctx context.Context, b *botRun, typeID int64, qty int) string {
	var vol sql.NullFloat64
	if err := m.db.QueryRowContext(ctx, `SELECT volume FROM invTypes WHERE typeID = ?`, typeID).Scan(&vol); err != nil {
		return fmt.Sprintf("type %d is unknown", typeID)
	}
	need := vol.Float64 * float64(qty)
	if hold := m.shipCargoCapacity(ctx, b); need > hold {
		return fmt.Sprintf("the cargo is %.0f m³ and the hold %.0f m³", need, hold)
	}
	return ""
}

// missionSupplySource is the nearest station in reach selling qty of a type, and about what it
// costs (the best ask over the lot, plus the walk up the book).
func (m *Mod) missionSupplySource(ctx context.Context, b *botRun, typeID, qty int64) (int64, float64) {
	u, err := m.universe(ctx)
	if err != nil {
		return 0, 0
	}
	near := hopsWithin(u, b.SystemID, MissionsMaxJumps())
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, s.solarSystemID, SUM(o.volRemaining), MIN(o.price)
		  FROM market_orders o JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.typeID = ? AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		 GROUP BY o.stationID, s.solarSystemID`, typeID)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()
	best, bestJumps, bestCost := int64(0), 1<<30, 0.0
	for rows.Next() {
		var stn, sys, vol int64
		var price float64
		if rows.Scan(&stn, &sys, &vol, &price) != nil || vol < qty {
			continue
		}
		jumps, ok := near[sys]
		if !ok {
			continue
		}
		cost := price * float64(qty) * 1.1
		if jumps < bestJumps || (jumps == bestJumps && cost < bestCost) {
			best, bestJumps, bestCost = stn, jumps, cost
		}
	}
	return best, bestCost
}

// ── running it ──────────────────────────────────────────────────────────────

// missionRunStep flies a kill pocket. The site runner does the fighting; this decides when the
// work is done, which the mission says, not the room.
func (m *Mod) missionRunStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	ms, tmpl := m.missionLoad(ctx, d.MissionID)
	if done, err := m.missionGone(ctx, b, ms, tmpl); done {
		return false, err
	}
	// A targeted objective can be met with rats still standing: leave them.
	if ms.ObjectiveDone && d.MissionKind == missionKindKill && tmpl.CompletionType != "killAll" {
		return false, m.missionPocketDone(ctx, b, "the objective is met")
	}
	if d.SiteID == 0 {
		d.SiteID = modapi.MissionSiteID(ms.MissionID)
		d.HuntSystemID = ms.DestSystemID
		d.SiteStartedAt, d.SiteRoomIdx, d.SiteEntered, d.SiteKills = clock.Unix(), 0, false, 0
	}
	if d.HuntLootID != 0 {
		if handled, err := m.combatResumeLoot(ctx, b); err != nil || handled {
			return false, err
		}
	}
	if b.SystemID != ms.DestSystemID {
		return false, m.beginTravel(ctx, b, ms.DestSystemID, 0, string(TaskMissionRun))
	}
	if b.ShipID == 0 {
		return false, fmt.Errorf("mission runner has no ship boarded")
	}
	out, err := m.siteRunStep(ctx, b, d.SiteID, nil)
	if err != nil {
		return false, err
	}
	switch out {
	case siteRunning, siteWaiting:
		return false, saveState(ctx, m.db, st)
	case siteDead:
		return false, nil // loseHull has rewritten the state; missionResume picks it up
	}
	why := map[siteOutcome]string{siteCleared: "the pocket is clear", siteAborted: "it broke off",
		siteGone: "the pocket is gone"}[out]
	return false, m.missionPocketDone(ctx, b, why)
}

// missionPocketDone leaves the pocket and decides: turn in, or quit when the work is not done.
func (m *Mod) missionPocketDone(ctx context.Context, b *botRun, why string) error {
	st := b.State
	d := &st.Data
	if d.SiteID != 0 {
		releaseSite(d.SiteID, b.CharID, 0)
	}
	d.SiteID, d.SiteRoomIdx, d.SiteEntered, d.SiteStartedAt = 0, 0, false, 0
	ms, tmpl := m.missionLoad(ctx, d.MissionID)
	if ms == nil || ms.State != "accepted" {
		return m.missionIdle(ctx, b)
	}
	switch d.MissionKind {
	case missionKindKill:
		if !ms.ObjectiveDone {
			return m.missionQuit(ctx, b, why+", and the objective is not met")
		}
	case missionKindKillItem:
		typeID, qty := missionCargoOf(d.MissionKind, tmpl)
		if have := m.shipCargoUnits(ctx, b, typeID); have < int64(qty) {
			return m.missionQuit(ctx, b, fmt.Sprintf("%s with %d of %d of the item", why, have, qty))
		}
	}
	m.logBot(b.CharID, logInfo, "mission", "%s — heading to turn %s in", why, missionNameOf(tmpl, ms))
	return m.beginTravel(ctx, b, m.stationSystemOf(ctx, d.MissionTurnIn), d.MissionTurnIn, string(TaskMissionTurnIn))
}

// missionGone ends a run whose mission can no longer be worked: closed by the core (expired,
// completed elsewhere) means idle; still accepted but its template deleted means handed back, so
// it does not sit until expiry and cost standing anyway. True when the run is over.
func (m *Mod) missionGone(ctx context.Context, b *botRun, ms *evedb.CharacterMission, tmpl *evedb.MissionTemplate) (bool, error) {
	if ms == nil || ms.State != "accepted" {
		m.logBot(b.CharID, logInfo, "mission", "mission %d is no longer open — dropping it", b.State.Data.MissionID)
		return true, m.missionIdle(ctx, b)
	}
	if tmpl == nil {
		return true, m.missionQuit(ctx, b, fmt.Sprintf("its template %d was deleted", ms.TemplateID))
	}
	return false, nil
}

// shipCargoUnits is how many of a type ride in the boarded ship's hold.
func (m *Mod) shipCargoUnits(ctx context.Context, b *botRun, typeID int64) int64 {
	if b.ShipID == 0 || typeID == 0 {
		return 0
	}
	var n sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `
		SELECT SUM(quantity) FROM entity
		 WHERE locationID = ? AND ownerID = ? AND typeID = ? AND flag = 5`,
		b.ShipID, b.CharID, typeID).Scan(&n)
	return n.Int64
}

// loadHangarStacks moves every stack of a type this pilot has in the station hangar into the hold.
func (m *Mod) loadHangarStacks(ctx context.Context, b *botRun, typeID int64) (int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID FROM entity
		 WHERE ownerID = ? AND locationID = ? AND flag = 4 AND typeID = ?`,
		b.CharID, b.StationID, typeID)
	if err != nil {
		return 0, err
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
		if err := m.moveItemToCargo(ctx, b, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// missionHaulStep carries a delivery: the courier's cargo from its pickup, or goods bought for a
// trade or mining mission, to the turn-in station.
func (m *Mod) missionHaulStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	ms, tmpl := m.missionLoad(ctx, d.MissionID)
	if done, err := m.missionGone(ctx, b, ms, tmpl); done {
		return false, err
	}
	typeID, qty := missionCargoOf(d.MissionKind, tmpl)
	need := int64(qty)
	turnIn := d.MissionTurnIn
	if b.StationID == turnIn {
		if n, err := evedb.CountItemInHangar(ctx, m.db, b.CharID, turnIn, typeID); err == nil && int64(n) >= need {
			st.Phase = string(TaskMissionTurnIn)
			return false, saveState(ctx, m.db, st)
		}
	}
	if m.shipCargoUnits(ctx, b, typeID) >= need {
		return false, m.beginTravel(ctx, b, m.stationSystemOf(ctx, turnIn), turnIn, string(TaskMissionTurnIn))
	}
	switch d.MissionKind {
	case missionKindCourier:
		pickup := ms.PickupStationID
		if b.StationID != pickup {
			return false, m.beginTravel(ctx, b, m.stationSystemOf(ctx, pickup), pickup, string(TaskMissionHaul))
		}
		n, err := m.loadHangarStacks(ctx, b, typeID)
		if err != nil {
			return false, err
		}
		if m.shipCargoUnits(ctx, b, typeID) < need {
			return false, m.missionQuit(ctx, b, fmt.Sprintf("the cargo was not at the pickup (%d stack(s) found)", n))
		}
		m.logBot(b.CharID, logInfo, "mission", "loaded the cargo — hauling it to %s", evedb.GetStationName(ctx, m.db, turnIn))
		return false, m.beginTravel(ctx, b, m.stationSystemOf(ctx, turnIn), turnIn, string(TaskMissionTurnIn))

	case missionKindSupply:
		have := m.shipCargoUnits(ctx, b, typeID)
		if d.MissionSource == 0 {
			src, _ := m.missionSupplySource(ctx, b, typeID, need-have)
			if src == 0 {
				return false, m.missionQuit(ctx, b, "nobody in reach sells the goods any more")
			}
			d.MissionSource = src
		}
		if b.StationID != d.MissionSource {
			return false, m.beginTravel(ctx, b, m.stationSystemOf(ctx, d.MissionSource), d.MissionSource, string(TaskMissionHaul))
		}
		_, filled, spent, err := m.buyAtStation(ctx, b.CharID, b.StationID, typeID, need-have, "mission")
		if err != nil && !errors.Is(err, errNoSellerHere) {
			return false, m.missionQuit(ctx, b, "buying the goods failed: "+err.Error())
		}
		if filled > 0 {
			m.logBot(b.CharID, logInfo, "mission", "bought %d for the agent (%s)", filled, fmtISK(spent))
		}
		if _, err := m.loadHangarStacks(ctx, b, typeID); err != nil {
			return false, err
		}
		if m.shipCargoUnits(ctx, b, typeID) < need {
			d.MissionTries++
			d.MissionSource = 0
			if d.MissionTries >= missionSupplyTries {
				return false, m.missionQuit(ctx, b, "the market ran dry before the order was filled")
			}
			return false, saveState(ctx, m.db, st)
		}
		return false, m.beginTravel(ctx, b, m.stationSystemOf(ctx, turnIn), turnIn, string(TaskMissionTurnIn))
	}
	return false, m.missionQuit(ctx, b, fmt.Sprintf("kind %q has no hauler", d.MissionKind))
}

// missionTurnInStep hands the work in at the turn-in station. The core checks the objective and
// pays.
func (m *Mod) missionTurnInStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	if d.MissionID == 0 {
		return false, m.missionIdle(ctx, b)
	}
	if b.StationID != d.MissionTurnIn {
		return false, m.beginTravel(ctx, b, m.stationSystemOf(ctx, d.MissionTurnIn), d.MissionTurnIn, string(TaskMissionTurnIn))
	}
	ms, tmpl := m.missionLoad(ctx, d.MissionID)
	if ms == nil || ms.State != "accepted" {
		m.logBot(b.CharID, logInfo, "mission", "mission %d is no longer open — nothing to turn in", d.MissionID)
		return false, m.missionIdle(ctx, b)
	}
	h := modapi.Missions()
	done, why, err := h.Complete(b.CharID, 0, d.MissionAgentID, b.StationID)
	if err != nil {
		m.logBot(b.CharID, logWarn, "mission", "turning in %s: %v", missionNameOf(tmpl, ms), err)
		return false, m.missionIdle(ctx, b)
	}
	if !done {
		return false, m.missionQuit(ctx, b, "the agent would not take it: "+why)
	}
	bonus := int64(0)
	if ms.BonusDeadline.Valid && clock.Now().Before(ms.BonusDeadline.Time) {
		bonus = ms.BonusISK
	}
	m.logBot(b.CharID, logInfo, "mission", "turned in %s — %s%s, %d LP",
		missionNameOf(tmpl, ms), fmtISK(float64(ms.RewardISK)), missionBonusNote(bonus), ms.RewardLP)
	m.recordMissionRun(ctx, b, ms, d.MissionKind, "completed", "")
	// The core took the delivered item; the loot ledger must not try to sell it.
	if d.MissionKind == missionKindKillItem {
		if typeID, qty := missionCargoOf(d.MissionKind, tmpl); typeID != 0 {
			m.forgetLoot(ctx, b.CharID, typeID, int64(qty))
		}
	}
	d.MissionState = MissionState{}
	if rows, err := m.lootLedger(ctx, b.CharID); err == nil && len(rows) > 0 {
		return false, m.combatStartSale(ctx, b, "mission turned in")
	}
	return false, m.missionIdle(ctx, b)
}

func missionBonusNote(bonus int64) string {
	if bonus <= 0 {
		return ""
	}
	return fmt.Sprintf(" + %s bonus", fmtISK(float64(bonus)))
}

// forgetLoot takes units of a type off the pilot's loot ledger.
func (m *Mod) forgetLoot(ctx context.Context, charID, typeID, units int64) {
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_loot_hold SET units = GREATEST(units - ?, 0)
		 WHERE characterID = ? AND typeID = ?`, units, charID, typeID); err != nil {
		rlogf("rolebots: forget loot char=%d type=%d: %v", charID, typeID, err)
	}
}

// missionQuit hands the mission back (the core charges the standing loss and removes the pocket
// and any courier cargo), records why, and idles the pilot.
func (m *Mod) missionQuit(ctx context.Context, b *botRun, why string) error {
	st := b.State
	d := &st.Data
	ms, tmpl := m.missionLoad(ctx, d.MissionID)
	if ms != nil && ms.State == "accepted" {
		if h := modapi.Missions(); h.Quit != nil {
			if err := h.Quit(b.CharID, 0, ms.AgentID, b.StationID); err != nil {
				m.logBot(b.CharID, logWarn, "mission", "quitting %s: %v", missionNameOf(tmpl, ms), err)
			}
		}
		m.logBot(b.CharID, logInfo, "mission", "quit %s — %s", missionNameOf(tmpl, ms), why)
		m.recordMissionRun(ctx, b, ms, d.MissionKind, "quit", why)
		noteMissionSkip(b.CharID, ms.AgentID)
	}
	if d.SiteID != 0 {
		releaseSite(d.SiteID, b.CharID, 0)
		d.SiteID, d.SiteRoomIdx, d.SiteEntered, d.SiteStartedAt = 0, 0, false, 0
	}
	d.MissionState = MissionState{}
	if rows, err := m.lootLedger(ctx, b.CharID); err == nil && len(rows) > 0 {
		return m.combatStartSale(ctx, b, "mission dropped")
	}
	return m.missionIdle(ctx, b)
}

// missionIdle ends the trip: mission fields cleared, pilot idle.
func (m *Mod) missionIdle(ctx context.Context, b *botRun) error {
	st := b.State
	st.Data.MissionState = MissionState{}
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	return saveState(ctx, m.db, st)
}

// recordMissionRun writes one row of rolebot_mission_runs: what the bots made of a template, for
// its author and for tuning.
func (m *Mod) recordMissionRun(ctx context.Context, b *botRun, ms *evedb.CharacterMission, kind, outcome, note string) {
	if m.db == nil || b == nil || b.State == nil || ms == nil {
		return
	}
	tier := "abstract"
	if m.systemProjected(b.SystemID) {
		tier = "embodied"
	}
	started := b.State.Data.MissionStartedAt
	if started == 0 {
		started = clock.Unix()
	}
	isk, lp := int64(0), 0
	if outcome == "completed" {
		isk, lp = ms.RewardISK, ms.RewardLP
		if ms.BonusDeadline.Valid && clock.Now().Before(ms.BonusDeadline.Time) {
			isk += ms.BonusISK
		}
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_mission_runs (charID, missionID, templateID, agentID, kind, tier,
			startedAt, endedAt, outcome, note, rewardISK, rewardLP)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.CharID, ms.MissionID, ms.TemplateID, ms.AgentID, kind, tier,
		started, clock.Unix(), outcome, trunc(note, 255), isk, lp); err != nil {
		rlogf("rolebots: record mission run: %v", err)
	}
}

// detailMission is the panel's line for a pilot on a mission.
func detailMission(m *Mod, ctx context.Context, st *SimState) string {
	d := st.Data
	if d.MissionID == 0 {
		if d.MissionAgentID != 0 {
			return "on the way to " + evedb.GetAgentName(ctx, m.db, d.MissionAgentID)
		}
		return ""
	}
	ms, tmpl := m.missionLoad(ctx, d.MissionID)
	name := missionNameOf(tmpl, ms)
	if ms != nil && (d.MissionKind == missionKindKill || d.MissionKind == missionKindKillItem) {
		return fmt.Sprintf("%s — %s, %d/%d kills", name, d.MissionKind, ms.KillProgress, ms.KillRequired)
	}
	return fmt.Sprintf("%s — %s", name, d.MissionKind)
}
