package rolebots

// R&D collaboration: a pilot's stake in an agent's Tech II patent lottery.
// Research is a lane, not an activity: enrolment takes no travel, ISK or time,
// and points accrue on their own. The lane signs industry-minded pilots up with
// a research agent. The field skill is bought by the skill planner (skills.go,
// goalResearch); the lane names the field in StateData.RndFieldID and waits. A
// win is real work (collect, haul the blueprint to the factory), so it is a
// task chain hooked into plan() ahead of gang duty. Enrolment happens at a
// distance but applies the same standing gate (CanUseAgent), field-skill
// requirement and project cap as the dialogue.

import (
	"context"
	"database/sql"
	"fmt"
	"runtime/debug"
	"strconv"
	"time"

	evedb "gurimoon/db"
	"gurimoon/rates"
)

const (
	// researchEveryTicks is the lane's clock: ten minutes. A round is seven days
	// and enrolment is once per pilot, so anything tighter is query cost.
	researchEveryTicks = 600
	// researchPassBudget is what one pass may spend.
	researchPassBudget = 45 * time.Second
	// researchBatch is how many un-enrolled pilots one pass considers. The
	// per-pilot cost is a standing check per candidate agent, so it is small.
	researchBatch = 40

	// researchMinDisposition is how much of a researcher a pilot has to be before
	// a research project is in character: the research career bar (tuning.go
	// researchCareerMin).
	researchMinDisposition = researchCareerMin

	// skillResearchProjectManagement grants one extra concurrent project per
	// level, the same as it does for a player. Everyone gets one without it.
	skillResearchProjectManagement = 12179

	// researchCheckEverySec throttles the pending-patent lookup in plan(), which
	// runs for every idle pilot each pass: a pilot with none must cost no query.
	researchCheckEverySec = 600
	// researchNoRouteBackoffSec is how long a pilot that cannot reach its
	// patent's agent waits before looking again (it may have moved by then).
	researchNoRouteBackoffSec = 6 * 3600

	// researchAgentChecks is how many agents in a field are standing-checked in
	// one pick. researchAgentRefusalRun is the run of refusals that ends the
	// climb once something usable is found (see researchPickAgent).
	researchAgentChecks     = 24
	researchAgentRefusalRun = 6

	// settingResearchCursor is how far through the roster the lane has walked.
	// Most candidates cannot enrol on the pass that first sees them (they wait on
	// the field skill), so a plain first-N batch would never reach later pilots.
	settingResearchCursor = "rolebots.researchCursor"
)

// startResearch registers the enrolment lane.
func (m *Mod) startResearch() {
	m.registerPeriodic("rolebots-research", researchEveryTicks, func(tickNum uint64) bool {
		if !Master() {
			return true
		}
		// The tick manager runs everything on one goroutine, so the database
		// work goes off it and the pass guards its own overlap.
		if !m.researchBusy.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer m.researchBusy.Store(false)
			defer func() {
				if r := recover(); r != nil {
					rlogf("rolebots: research lane panicked: %v\n%s", r, debug.Stack())
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), researchPassBudget)
			defer cancel()
			m.researchPass(ctx)
		}()
		return true
	})
}

// researchPass signs up a batch of eligible pilots.
func (m *Mod) researchPass(ctx context.Context) {
	if m.db == nil {
		return
	}
	// Nothing to buy tickets for.
	if !evedb.PatentLotteryEnabled(ctx, m.db) {
		return
	}
	fields, err := evedb.NonEmptyResearchFields(ctx, m.db)
	if err != nil {
		rlogf("rolebots: research fields: %v", err)
		return
	}
	if len(fields) == 0 {
		// Every patent pool is empty: enrolling would sign pilots up for a lottery
		// with no prize.
		return
	}

	// Anyone holding a win looks at it on their next pass, due or not
	// (researchsweep.go). It walks the offers, not the roster.
	m.sweepUnclaimedPatents(ctx)

	runs, err := m.researchCandidates(ctx)
	if err != nil {
		rlogf("rolebots: research candidates: %v", err)
		return
	}
	for _, b := range runs {
		if ctx.Err() != nil {
			return
		}
		// Signing up with an agent is something a pilot does logged in.
		if !researchInclined(b.Tune) || popIsOffline(b.CharID) {
			continue
		}
		if err := m.researchEnrol(ctx, b, fields); err != nil {
			m.logBot(b.CharID, logError, "research", "signing up with a research agent: %v", err)
		}
	}
}

// researchInclined reports whether a research project is in character. It is
// the research career (tuning.go ResearchAppetite), the research tag's own
// expression (corps.go), so the CEO's lab, its members' sign-ups and the lab
// lane agree.
func researchInclined(tn Tuning) bool {
	return tn.ResearchAppetite >= researchMinDisposition
}

// researchCandidates returns the next batch of enabled pilots with no project,
// walking the roster by characterID from a persisted cursor and wrapping at the
// end. The cursor keeps a batch from re-offering the same pilots forever. A LEFT
// JOIN counts existing projects in SQL instead of one query per pilot.
func (m *Mod) researchCandidates(ctx context.Context) ([]*botRun, error) {
	cursor := int64(0)
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingResearchCursor); err == nil && ok {
		cursor, _ = strconv.ParseInt(v, 10, 64)
	}

	ids, err := m.researchCandidatePage(ctx, cursor)
	if err != nil {
		return nil, err
	}
	if len(ids) < researchBatch {
		// Off the end of the roster: wrap now, so a roster smaller than one batch
		// is walked every time.
		if err := evedb.SetServerSetting(ctx, m.db, settingResearchCursor, "0"); err != nil {
			rlogf("rolebots: research cursor reset: %v", err)
		}
	} else if err := evedb.SetServerSetting(ctx, m.db, settingResearchCursor,
		strconv.FormatInt(ids[len(ids)-1], 10)); err != nil {
		rlogf("rolebots: research cursor advance: %v", err)
	}

	var out []*botRun
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		b, err := m.loadRun(ctx, id)
		if err != nil {
			continue // disabled or purged between the two queries
		}
		out = append(out, b)
	}
	return out, nil
}

func (m *Mod) researchCandidatePage(ctx context.Context, after int64) ([]int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID
		FROM rolebot_characters c
		LEFT JOIN characterresearch cr ON cr.charID = c.characterID
		WHERE c.enabled = 1 AND c.characterID > ?
		GROUP BY c.characterID
		HAVING COUNT(cr.charID) = 0
		ORDER BY c.characterID
		LIMIT ?`, after, researchBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// researchEnrol signs one pilot up, or records the field it needs a skill for.
// StartResearch refuses an untrained field and the skill planner trains only
// what it is told to want, so the field is chosen first and written to
// StateData.RndFieldID (skills.go, goalResearch); the pilot enrols on a later pass.
func (m *Mod) researchEnrol(ctx context.Context, b *botRun, fields []int64) error {
	st := b.State
	if st == nil {
		return nil
	}
	// Concurrent-project cap, the same one agentStartResearch applies.
	count, err := evedb.CountCharacterResearch(ctx, m.db, b.CharID)
	if err != nil {
		return err
	}
	rpm, _ := m.charSkillLevelFor(ctx, b.CharID, skillResearchProjectManagement)
	if count >= 1+rpm {
		return nil
	}

	// One read of the whole sheet: the picker asks about every field, and
	// single-skill lookups per candidate would be a query storm.
	levels, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return err
	}
	fieldID, level := researchPickField(b, fields, levels)
	if fieldID == 0 {
		return nil
	}
	if st.Data.RndFieldID != fieldID {
		st.Data.RndFieldID = fieldID
		if err := saveState(ctx, m.db, st); err != nil {
			return err
		}
	}
	if level == 0 {
		// The planner's turn. Nothing to log about a pilot waiting on a skill.
		return nil
	}

	agent, ppd, err := m.researchPickAgent(ctx, b, fieldID, level)
	if err != nil {
		return err
	}
	if agent == nil {
		return nil // no agent in this field will speak to it yet
	}
	if err := evedb.StartResearch(ctx, m.db, b.CharID, agent.AgentID, fieldID, ppd); err != nil {
		return err
	}
	fieldName, _ := evedb.GetTypeName(ctx, m.db, fieldID)
	m.logBot(b.CharID, logInfo, "research",
		"signed up with agent %d for %s research at %.1f RP/day — its points now buy tickets in that agent's patent allocation",
		agent.AgentID, fieldName, ppd)
	m.chatterNoteBot(ctx, b.CharID, "lab.enrol", map[string]string{"field": fieldName}, agent.AgentID)
	return nil
}

// researchPickField chooses the science field this pilot collaborates in and
// returns it with the pilot's trained level (0 = not yet). A trained field wins;
// otherwise the previously chosen field is kept so the planner's training is not
// thrown away; only a pilot with neither picks fresh.
func researchPickField(b *botRun, fields []int64, levels map[int64]int) (int64, int) {
	best, bestLevel := int64(0), 0
	for _, f := range fields {
		if lvl := levels[f]; lvl > bestLevel {
			best, bestLevel = f, lvl
		}
	}
	if best != 0 {
		return best, bestLevel
	}
	if prev := b.State.Data.RndFieldID; prev != 0 {
		for _, f := range fields {
			if f == prev {
				return prev, 0
			}
		}
	}
	// Nothing trained and nothing chosen: spread the roster across the fields.
	// The charID keeps the choice stable between passes.
	return fields[int(b.CharID%int64(len(fields)))], 0
}

// researchPickAgent chooses the agent in a field to collaborate with and returns
// the pairing's RP/day: best rate, then nearest factory. The walk runs from the
// end of the list, where unknown pilots are welcome.
// See docs/comment-notes-b16.md#research-pick-agent.
func (m *Mod) researchPickAgent(ctx context.Context, b *botRun, fieldID int64, lvl int) (*evedb.AgentInfo, float64, error) {
	sites, err := evedb.ResearchAgentsForField(ctx, m.db, fieldID)
	if err != nil {
		return nil, 0, err
	}

	// The factory is where a won patent is hauled back to, so it is the origin of
	// the distance tiebreak. It is resolved once and used only for equal rates.
	factorySys := int64(0)
	if factoryID, sys, ferr := m.industryFactoryStation(ctx, b); ferr == nil && factoryID != 0 {
		factorySys = sys
	}
	const jumpsUnknown = 1 << 29
	jumpsTo := func(sysID int64) int {
		if factorySys == 0 || sysID == 0 {
			return jumpsUnknown
		}
		j, err := m.jumpsBetween(ctx, b, factorySys, sysID)
		if err != nil {
			return jumpsUnknown
		}
		return j
	}

	var best *evedb.AgentInfo
	var bestPPD float64
	bestSys, bestJumps := int64(0), jumpsUnknown
	bestJumpsKnown := false
	checks, refusals := 0, 0
	for i := len(sites) - 1; i >= 0; i-- {
		if ctx.Err() != nil || checks >= researchAgentChecks {
			break
		}
		s := sites[i]
		agent, err := evedb.GetAgentInfo(ctx, m.db, s.AgentID)
		if err != nil {
			continue
		}
		checks++
		// The same standing gate the conversation applies.
		if !evedb.CanUseAgent(ctx, m.db, b.CharID, agent).Allowed {
			refusals++
			if best != nil && refusals >= researchAgentRefusalRun {
				break
			}
			continue
		}
		refusals = 0
		ppd := evedb.ResearchPointsPerDay(agent, lvl, rates.ResearchPoints)
		if ppd <= 0 {
			continue
		}
		switch {
		case best == nil || ppd > bestPPD:
			best, bestPPD, bestSys = agent, ppd, s.SolarSystemID
			bestJumps, bestJumpsKnown = jumpsUnknown, false
		case ppd == bestPPD:
			if !bestJumpsKnown {
				bestJumps, bestJumpsKnown = jumpsTo(bestSys), true
			}
			if j := jumpsTo(s.SolarSystemID); j < bestJumps {
				best, bestSys, bestJumps = agent, s.SolarSystemID, j
			}
		}
	}
	return best, bestPPD, nil
}

// ── collecting a win ────────────────────────────────────────────────────────

// researchCollectDue is plan()'s hook, placed after industryCollectDue and
// ahead of gang duty: a won patent is capital on a claim deadline and nothing
// else fetches it. Reports handled=true when it has routed the bot somewhere.
func (m *Mod) researchCollectDue(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if st == nil {
		return false, nil
	}
	d := &st.Data
	now := clock.Unix()

	// Cost guard: this runs for every idle pilot each pass, and the case with no
	// pending patent must be free.
	if d.RndOfferID == 0 && now-d.RndCheckedAt < researchCheckEverySec {
		return false, nil
	}

	pending, err := evedb.ListPendingPatentOffersForChar(ctx, m.db, b.CharID)
	if err != nil {
		return false, err
	}
	if len(pending) == 0 {
		// Also the resume case: a trip whose patent was collected, voided or
		// lapsed. Clear it.
		if d.RndOfferID != 0 {
			m.clearPatentTrip(d)
		}
		d.RndCheckedAt = now
		return false, saveState(ctx, m.db, st)
	}

	p := pending[0] // soonest to lapse
	agent, err := evedb.GetAgentInfo(ctx, m.db, p.AgentID)
	if err != nil {
		return false, err
	}
	loc, err := evedb.GetStationLocation(ctx, m.db, agent.StationID)
	if err != nil {
		// A station the map cannot place is unreachable. Back off rather than
		// retry every pass.
		d.RndCheckedAt = now
		return false, saveState(ctx, m.db, st)
	}
	factoryID, factorySys, err := m.industryFactoryStation(ctx, b)
	if err != nil || factoryID == 0 {
		// No factory to take it to: collect it at the agent's station and stop.
		factoryID, factorySys = agent.StationID, loc.SolarSystemID
	}

	bpName, _ := evedb.GetTypeName(ctx, m.db, p.BlueprintID)
	announce := d.RndOfferID != p.OfferID
	d.RndOfferID = p.OfferID
	d.RndAgentID = p.AgentID
	d.RndStationID = agent.StationID
	d.RndStationSysID = loc.SolarSystemID
	d.RndBPTypeID = p.BlueprintID
	d.RndBPName = bpName
	d.RndFactoryID = factoryID
	d.RndFactorySysID = factorySys
	d.RndCheckedAt = now

	if announce {
		m.logBot(b.CharID, logInfo, "research",
			"won the %s patent from agent %d — going to collect it before it lapses",
			bpName, p.AgentID)
	}
	// Reuse the industry slug; the panel already knows it.
	st.Activity = ActIndustry
	if err := m.beginTravel(ctx, b, loc.SolarSystemID, agent.StationID, string(TaskRndCollect)); err != nil {
		if ctx.Err() != nil {
			return true, err
		}
		// No route from here. With RndOfferID set the cost guard above never
		// applies, so drop the trip and look again in researchNoRouteBackoffSec.
		m.clearPatentTrip(d)
		st.Activity = ""
		d.RndCheckedAt = now + researchNoRouteBackoffSec
		m.logBot(b.CharID, logInfo, "research",
			"cannot reach agent %d to collect the %s patent (%v) - trying again later", p.AgentID, bpName, err)
		return false, saveState(ctx, m.db, st)
	}
	return true, nil
}

// researchStep advances the collect-and-haul errand.
func (m *Mod) researchStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data

	switch TaskKind(st.Phase) {

	case TaskRndCollect:
		if d.RndOfferID == 0 {
			return false, m.finishPatentTrip(ctx, b, "")
		}
		if b.StationID != d.RndStationID {
			// A restart mid-phase, or the pilot was moved. Resume the trip.
			return false, m.beginTravel(ctx, b, d.RndStationSysID, d.RndStationID, string(TaskRndCollect))
		}
		itemID, typeID, err := evedb.ClaimPatentOffer(ctx, m.db, d.RndOfferID, b.CharID, b.StationID, clock.Now())
		if err != nil {
			// Already claimed, lapsed or voided: none of these is a broken bot.
			m.logBot(b.CharID, logInfo, "research", "could not collect its patent: %v", err)
			return false, m.finishPatentTrip(ctx, b, "")
		}
		m.logBot(b.CharID, logInfo, "research",
			"collected the %s original (item %d) from agent %d", d.RndBPName, itemID, d.RndAgentID)

		if d.RndFactoryID == 0 || d.RndFactoryID == b.StationID {
			// Nowhere else to take it. It is already in the right hangar.
			return false, m.finishPatentTrip(ctx, b, "")
		}
		// There is no station-to-station transfer: the blueprint goes in the hold
		// and the pilot flies it (tradePickup); ensureRunabout supplies a shuttle.
		if _, ok := m.ensureRunabout(ctx, b, "carrying the "+d.RndBPName+" to its factory"); !ok {
			// Nothing to load it into and nothing here sells one: it stays in the
			// hangar and the next plan fetches it once the pilot has a hull.
			m.logBot(b.CharID, logInfo, "research",
				"leaving the %s at the agent's station until it has a ship to carry it in", d.RndBPName)
			return false, m.finishPatentTrip(ctx, b, "")
		}
		if err := m.moveHangarToCargo(ctx, b, typeID); err != nil {
			m.logBot(b.CharID, logError, "research", "loading the %s: %v", d.RndBPName, err)
			return false, m.finishPatentTrip(ctx, b, "")
		}
		return false, m.beginTravel(ctx, b, d.RndFactorySysID, d.RndFactoryID, string(TaskRndHaul))

	case TaskRndHaul:
		if d.RndBPTypeID == 0 {
			return false, m.finishPatentTrip(ctx, b, "")
		}
		if b.StationID != d.RndFactoryID {
			return false, m.beginTravel(ctx, b, d.RndFactorySysID, d.RndFactoryID, string(TaskRndHaul))
		}
		if err := m.moveCargoToHangar(ctx, b, d.RndBPTypeID); err != nil {
			m.logBot(b.CharID, logError, "research", "unloading the %s at its factory: %v", d.RndBPName, err)
			return false, m.finishPatentTrip(ctx, b, "")
		}
		// Find the stowed blueprint in the factory hangar.
		var patentItemID int64
		m.db.QueryRowContext(ctx,
			`SELECT itemID FROM entity WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4
			 ORDER BY itemID DESC LIMIT 1`,
			b.CharID, b.StationID, d.RndBPTypeID).Scan(&patentItemID)
		if patentItemID == 0 {
			// moveCargoToHangar matches by type and moves nothing silently, so an empty
			// hold reaches here as success; say so.
			return false, m.finishPatentTrip(ctx, b,
				fmt.Sprintf("arrived at its factory without the %s — nothing to stow", d.RndBPName))
		}
		// It is now in the factory hangar, where industryPlan's blueprint search looks.
		return false, m.finishPatentTrip(ctx, b,
			fmt.Sprintf("stowed the %s at its factory", d.RndBPName))
	}
	return false, m.finishPatentTrip(ctx, b, "")
}

// clearPatentTrip drops the round trip but keeps the pilot-level facts:
// RndFieldID is what it researches and RndCheckedAt is a cooldown.
func (m *Mod) clearPatentTrip(d *StateData) {
	d.RndOfferID = 0
	d.RndAgentID = 0
	d.RndStationID = 0
	d.RndStationSysID = 0
	d.RndBPTypeID = 0
	d.RndBPName = ""
	d.RndFactoryID = 0
	d.RndFactorySysID = 0
}

// finishPatentTrip ends the errand and hands the pilot back to the planner.
func (m *Mod) finishPatentTrip(ctx context.Context, b *botRun, note string) error {
	st := b.State
	if note != "" {
		m.logBot(b.CharID, logInfo, "research", "%s", note)
	}
	// idleData keeps the field this pilot researches and the lookup cooldown
	// (TestIdleDataCarriesPatentTrip pins the set).
	m.clearPatentTrip(&st.Data)
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	return saveState(ctx, m.db, st)
}

// detailResearch is the panel's one-line description of a patent errand.
func detailResearch(m *Mod, ctx context.Context, st *SimState) string {
	d := st.Data
	if d.RndBPName == "" {
		return ""
	}
	switch st.Phase {
	case string(TaskRndCollect):
		return fmt.Sprintf("collecting the %s patent from its research agent", d.RndBPName)
	case string(TaskRndHaul):
		return fmt.Sprintf("hauling the %s to its factory", d.RndBPName)
	}
	return ""
}

// researchFieldsFor returns the field skills the skill planner should keep this
// pilot trained in: the fields it collaborates on plus the earmarked field. The
// earmark breaks a loop: StartResearch needs the skill, and the planner wants
// the skill only for a project.
func researchFieldsFor(ctx context.Context, db *sql.DB, charID int64, earmark int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	add := func(id int64) {
		if id == 0 || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT skillTypeID FROM characterresearch WHERE charID = ?`, charID)
	if err == nil {
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err == nil {
				add(id)
			}
		}
		rows.Close()
	}
	add(earmark)
	return out
}
