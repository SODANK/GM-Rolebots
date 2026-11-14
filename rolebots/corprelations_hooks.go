package rolebots

// Where incidents come from. noteCorpIncident is the one door into the ledger: cheap,
// lock-free, never failing its caller. It files nothing unless the viewing corporation
// is run by a bot CEO (managedCEO), the counterparty is a player corporation and the two
// are not kin. The stored weight is the base impulse (relBaseWeight); the viewer's
// charter weighs it at fold time. Hooks: every bot death through fileKillmail
// (killboard.go), with relKillboardCatchUp for kills the module never saw; a landed
// non-lethal shot; a paid hold-up; a bounty; a tower reinforced or destroyed by a siege;
// a war change (the snapshot diff in relLoadWars, the only seam that sees a human's
// declaration); and a belt worked by two corporations inside one hour.

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// relHookTimeout bounds a hook's own write: a hook runs on somebody else's
// path and must not hold it behind a slow database.
const relHookTimeout = 5 * time.Second

// relPlayerCorpTTL is how long "is this a player corporation" is remembered.
const relPlayerCorpTTL = 10 * time.Minute

var (
	relPlayerCorpMu sync.Mutex
	relPlayerCorp   = map[int64]struct {
		ok bool
		at time.Time
	}{}
)

// isPlayerCorpCached answers whether a corporation is a player one, with a
// ten-minute memory per corporation. A failed read answers false: no
// incident is filed against a corporation the module could not classify.
func (m *Mod) isPlayerCorpCached(ctx context.Context, corpID int64) bool {
	if corpID == 0 || m.db == nil {
		return false
	}
	now := clock.Now()
	relPlayerCorpMu.Lock()
	e, ok := relPlayerCorp[corpID]
	relPlayerCorpMu.Unlock()
	if ok && now.Sub(e.at) < relPlayerCorpTTL {
		return e.ok
	}
	npc, err := isNPCCorp(ctx, m.db, corpID)
	if err != nil {
		return false
	}
	relPlayerCorpMu.Lock()
	relPlayerCorp[corpID] = struct {
		ok bool
		at time.Time
	}{!npc, now}
	relPlayerCorpMu.Unlock()
	return !npc
}

// relKin reports whether two corporations are one side already.
func relKin(a, b int64) bool {
	if a == b {
		return true
	}
	if s := loadRelSnapshot(); s != nil {
		if aa, ok := s.ally[a]; ok && aa != 0 && aa == s.ally[b] {
			return true
		}
	}
	return false
}

// noteCorpIncident files one incident from fromCorp's point of view about
// toCorp. dedupeKey "" = never deduped. Safe from any goroutine and either
// tier; the write has its own context and deadline.
func (m *Mod) noteCorpIncident(ctx context.Context, fromCorp, toCorp int64, kind, side string,
	weight, isk float64, systemID, subjectID int64, dedupeKey, detail string) {

	if m.db == nil || !relLedgerOn() || !relationsEnabled() || weight == 0 {
		return
	}
	if fromCorp == 0 || toCorp == 0 || relKin(fromCorp, toCorp) {
		return
	}
	if _, managed := managedCEO(fromCorp); !managed {
		return
	}
	if !m.isPlayerCorpCached(ctx, toCorp) {
		return
	}
	wctx, cancel := context.WithTimeout(context.Background(), relHookTimeout)
	defer cancel()
	var dedupe sql.NullString
	if dedupeKey != "" {
		dedupe = sql.NullString{String: truncate(dedupeKey, 48), Valid: true}
	}
	if _, err := m.db.ExecContext(wctx, `
		INSERT IGNORE INTO rolebot_corp_incidents
			(corporationID, toID, kind, side, weight, isk, systemID, subjectID, dedupeKey, at, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fromCorp, toCorp, kind, side, weight, isk, systemID, subjectID, dedupe, clock.Unix(),
		truncate(detail, 255)); err != nil {
		rlogf("rolebots: relations incident %s corp %d → %d: %v", kind, fromCorp, toCorp, err)
	}
}

// noteCorpIncidentBoth files the victim's row on victimCorp and the
// aggressor's share on aggressorCorp. Either side files only if managed.
func (m *Mod) noteCorpIncidentBoth(ctx context.Context, victimCorp, aggressorCorp int64, kind string,
	isk float64, systemID, subjectID int64, dedupeKey, detail string) {

	p := relParamsNow()
	m.noteCorpIncident(ctx, victimCorp, aggressorCorp, kind, relSideVictim,
		relBaseWeight(kind, relSideVictim, isk, p), isk, systemID, subjectID, dedupeKey, detail)
	m.noteCorpIncident(ctx, aggressorCorp, victimCorp, kind, relSideAggressor,
		relBaseWeight(kind, relSideAggressor, isk, p), isk, systemID, subjectID, dedupeKey, detail)
}

// noteCorpIncidentMutual files one incident on both ledgers, the same way
// round: crowding, a pact, a peace.
func (m *Mod) noteCorpIncidentMutual(ctx context.Context, a, b int64, kind string,
	isk float64, systemID, subjectID int64, dedupeKey, detail string) {

	w := relBaseWeight(kind, relSideMutual, isk, relParamsNow())
	m.noteCorpIncident(ctx, a, b, kind, relSideMutual, w, isk, systemID, subjectID, dedupeKey, detail)
	m.noteCorpIncident(ctx, b, a, kind, relSideMutual, w, isk, systemID, subjectID, dedupeKey, detail)
}

// ── kills ───────────────────────────────────────────────────────────────────

// noteKillIncidents files a recorded killmail: one incident per distinct
// attacking player corporation, on the victim's ledger and the attacker's. A
// starbase gun counts for its owner's corporation (isStarbaseAttacker, pod.go).
// Called from fileKillmail's own goroutine after the record is written, with
// the killID as the dedupe key the catch-up below shares.
func (m *Mod) noteKillIncidents(ctx context.Context, killID int64, rec evedb.KillmailRecord) {
	if killID == 0 || rec.VictimCorpID == 0 || !relLedgerOn() {
		return
	}
	var value float64
	_ = m.db.QueryRowContext(ctx, `SELECT totalValue FROM killmails WHERE killID = ?`, killID).Scan(&value)
	kind := incKill
	if rec.IsPod {
		kind = incPod
	}
	// A called gank (gankrel.go) is a heavier line than the kill it also is,
	// and it is filed even against a ganker in an NPC corporation.
	gank := !rec.IsPod && m.isGankKill(ctx, rec.VictimCharID, rec.Time)
	seen := map[int64]bool{}
	playerCorp := func(id int64) bool { return m.isPlayerCorpCached(ctx, id) }
	for _, a := range rec.Attackers {
		if a.CorpID == 0 || a.CorpID == rec.VictimCorpID || seen[a.CorpID] {
			continue
		}
		if a.IsNPC && !isStarbaseAttacker(a.IsNPC, a.CharID, a.CorpID, playerCorp) {
			continue
		}
		seen[a.CorpID] = true
		k := kind
		if gank {
			if !m.isPlayerCorpCached(ctx, a.CorpID) {
				m.noteGankIncidentNPC(ctx, rec.VictimCorpID, a.CorpID, value, rec.SolarSystemID, killID, rec.VictimName)
				continue
			}
			k = m.gankKindFor(ctx, rec.VictimCorpID, a.CorpID, false)
		}
		m.noteCorpIncidentBoth(ctx, rec.VictimCorpID, a.CorpID, k, value, rec.SolarSystemID, killID,
			fmt.Sprintf("kill:%d", killID), rec.VictimName)
	}
}

// relKillboardCatchUp files, on one corporation's ledger, every killmail since its
// watermark that the hooks never saw: a bot killing a human files through the core's own
// death path, and a human-filed mail never passes fileKillmail. The watermark is the
// newest killID already on this ledger, floored at windowDays back; the dedupe key is the hook's.
func (m *Mod) relKillboardCatchUp(ctx context.Context, corpID int64, windowDays float64) {
	if !relLedgerOn() {
		return
	}
	var mark sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `
		SELECT MAX(subjectID) FROM rolebot_corp_incidents
		 WHERE corporationID = ? AND kind IN ('kill', 'gank', 'pod')`, corpID).Scan(&mark)
	since := clock.Unix() - int64(windowDays*86400)
	var minKill sql.NullInt64
	if err := m.db.QueryRowContext(ctx,
		`SELECT MIN(killID) FROM killmails WHERE killTime >= ?`, since).Scan(&minKill); err != nil || !minKill.Valid {
		return
	}
	from := minKill.Int64
	if mark.Valid && mark.Int64+1 > from {
		from = mark.Int64 + 1
	}
	type hit struct {
		killID, sys, corp int64
		pod               bool
		value             float64
		name              string
		victim            bool
		victimChar, when  int64
	}
	var hits []hit
	// Our losses.
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT k.killID, k.solarSystemID, a.corpID, k.isPod, k.totalValue, k.victimName,
		       k.victimCharID, k.killTime
		  FROM killmails k
		  JOIN killmail_attackers a ON a.killID = k.killID
		 WHERE k.victimCorpID = ? AND k.killID >= ? AND k.killTime >= ?
		   AND a.isNPC = 0 AND a.corpID <> 0 AND a.corpID <> ?`, corpID, from, since, corpID)
	if err != nil {
		return
	}
	for rows.Next() {
		var h hit
		var pod int
		if rows.Scan(&h.killID, &h.sys, &h.corp, &pod, &h.value, &h.name, &h.victimChar, &h.when) == nil {
			h.pod, h.victim = pod != 0, true
			hits = append(hits, h)
		}
	}
	rows.Close()
	// Our kills.
	rows, err = m.db.QueryContext(ctx, `
		SELECT DISTINCT k.killID, k.solarSystemID, k.victimCorpID, k.isPod, k.totalValue, k.victimName,
		       k.victimCharID, k.killTime
		  FROM killmail_attackers a
		  JOIN killmails k ON k.killID = a.killID
		 WHERE a.corpID = ? AND a.killID >= ? AND a.isNPC = 0 AND k.killTime >= ?
		   AND k.victimCorpID <> 0 AND k.victimCorpID <> ?`, corpID, from, since, corpID)
	if err != nil {
		return
	}
	for rows.Next() {
		var h hit
		var pod int
		if rows.Scan(&h.killID, &h.sys, &h.corp, &pod, &h.value, &h.name, &h.victimChar, &h.when) == nil {
			h.pod = pod != 0
			hits = append(hits, h)
		}
	}
	rows.Close()
	p := relParamsNow()
	for _, h := range hits {
		kind, side := incKill, relSideAggressor
		if h.pod {
			kind = incPod
		}
		if h.victim {
			side = relSideVictim
		}
		// A called gank (gankrel.go): the one way a human victim's loss is read as more than
		// a kill, since the core files that death.
		if !h.pod && m.gankLogged(ctx, h.victimChar, h.when) {
			if h.victim {
				kind = m.gankKindFor(ctx, corpID, h.corp, false)
			} else {
				kind = incGank
			}
		}
		m.noteCorpIncident(ctx, corpID, h.corp, kind, side, relBaseWeight(kind, side, h.value, p),
			h.value, h.sys, h.killID, fmt.Sprintf("kill:%d", h.killID), h.name)
	}
}

// relRecentHostility is what each counterparty has done to this corporation
// lately, in kill units (the base weights summed, violence only), for the
// war power's grievance. Ganks are left out: they are the war power's own
// term (relRecentGanks, gankrel.go), outside the grievance cap.
func (m *Mod) relRecentHostility(ctx context.Context, corpID int64, sinceSec int64) map[int64]float64 {
	out := map[int64]float64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT toID, SUM(-weight) FROM rolebot_corp_incidents
		 WHERE corporationID = ? AND at >= ? AND weight < 0 AND side = ? AND kind NOT IN (?, ?, ?)
		 GROUP BY toID`, corpID, clock.Unix()-sinceSec, relSideVictim, incCrowd, incContest, incGank)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var v float64
		if rows.Scan(&id, &v) == nil {
			out[id] = v
		}
	}
	return out
}

// ── aggression, robbery, bounties ───────────────────────────────────────────

// noteAggressionIncidents files the shots that landed and killed nobody:
// one per (victim corp, aggressor corp, system, hour).
func (m *Mod) noteAggressionIncidents(ctx context.Context, aggressors, victims []*pvpCombatant, sysID int64) {
	hour := clock.Unix() / 3600
	for _, a := range aggressors {
		if a == nil || a.CorpID == 0 || a.Dealt <= 0 {
			continue
		}
		for _, v := range victims {
			if v == nil || v.CorpID == 0 || v.CorpID == a.CorpID || v.Taken <= 0 {
				continue
			}
			m.noteCorpIncidentBoth(ctx, v.CorpID, a.CorpID, incAggress, 0, sysID, a.CharID,
				fmt.Sprintf("aggress:%d:%d", sysID, hour), v.Name+" was fired on by "+a.Name)
		}
	}
}

// noteRobberyIncident files a paid hold-up against every pirate corporation
// in the pack.
func (m *Mod) noteRobberyIncident(ctx context.Context, victimCharID, victimCorpID int64, victimName string,
	pirateCorps []int64, isk float64, sysID int64) {
	hour := clock.Unix() / 3600
	seen := map[int64]bool{}
	for _, pc := range pirateCorps {
		if pc == 0 || seen[pc] {
			continue
		}
		seen[pc] = true
		m.noteCorpIncidentBoth(ctx, victimCorpID, pc, incRobbery, isk, sysID, victimCharID,
			fmt.Sprintf("rob:%d:%d", victimCharID, hour), victimName+" paid a ransom")
	}
}

// noteBountyIncident: a corporation priced one of ours. The target's
// corporation holds it against the filer; filing costs the filer nothing.
func (m *Mod) noteBountyIncident(ctx context.Context, filerCorpID int64, target *pvpCombatant, price float64) {
	if target == nil {
		return
	}
	m.noteCorpIncidentBoth(ctx, target.CorpID, filerCorpID, incBounty, price, 0, target.CharID,
		fmt.Sprintf("bounty:%d:%d", target.CharID, clock.Unix()/3600), "a price on "+target.Name)
}

// ── sieges ──────────────────────────────────────────────────────────────────

// noteSiegeIncident files a tower's reinforcement (kind incSiege) or loss
// (incTowerKill) against the besieger.
func (m *Mod) noteSiegeIncident(ctx context.Context, s *siegeRow, kind string, isk float64) {
	if s == nil || s.AttackerCorpID == 0 || s.DefenderCorpID == 0 {
		return
	}
	m.noteCorpIncidentBoth(ctx, s.DefenderCorpID, s.AttackerCorpID, kind, isk, s.SystemID, s.TowerID,
		fmt.Sprintf("%s:%d:%d", kind, s.TowerID, s.SiegeID), "the tower in "+systemNameOf(ctx, m, s.SystemID))
}

// ── belt crowding ───────────────────────────────────────────────────────────

var (
	relPresenceMu sync.Mutex
	// relPresence maps systemID to corporationID to the unix time last seen mining there.
	relPresence = map[int64]map[int64]int64{}
	// relCrowdFiled is {system, corp, other} to the hour a crowding incident was last filed
	// for that pair there, so an ignored INSERT does not burn a round trip and an id. Both
	// orders of a pair are stamped.
	relCrowdFiled = map[[3]int64]int64{}
)

func init() { registerSweep("relCrowdFiled", sweepRelCrowdFiled) }

// sweepRelCrowdFiled forgets stamps from earlier hours, and presence that
// has lapsed (the write only trims the system it is in).
func sweepRelCrowdFiled() {
	now := clock.Unix()
	hour := now / 3600
	window := relCrowdWindowSec()
	relPresenceMu.Lock()
	for k, h := range relCrowdFiled {
		if h < hour {
			delete(relCrowdFiled, k)
		}
	}
	for sysID, sys := range relPresence {
		for corp, at := range sys {
			if now-at > window {
				delete(sys, corp)
			}
		}
		if len(sys) == 0 {
			delete(relPresence, sysID)
		}
	}
	relPresenceMu.Unlock()
}

// noteBeltPresence records a corporation's pilot starting to mine in a system and files
// a crowding incident against every other corporation that mined there inside the
// crowding window: one per pair per system per hour, both ways. In memory, a map read.
func (m *Mod) noteBeltPresence(ctx context.Context, corpID, sysID int64) {
	if corpID == 0 || sysID == 0 || !relLedgerOn() {
		return
	}
	now := clock.Unix()
	window := relCrowdWindowSec()
	var others []int64
	relPresenceMu.Lock()
	sys := relPresence[sysID]
	if sys == nil {
		sys = map[int64]int64{}
		relPresence[sysID] = sys
	}
	hour := now / 3600
	for other, at := range sys {
		if now-at > window {
			delete(sys, other)
			continue
		}
		if other == corpID || relCrowdFiled[[3]int64{sysID, corpID, other}] == hour {
			continue
		}
		relCrowdFiled[[3]int64{sysID, corpID, other}] = hour
		relCrowdFiled[[3]int64{sysID, other, corpID}] = hour
		others = append(others, other)
	}
	sys[corpID] = now
	relPresenceMu.Unlock()
	for _, other := range others {
		m.noteCorpIncidentMutual(ctx, corpID, other, incCrowd, 0, sysID, 0,
			fmt.Sprintf("crowd:%d:%d", sysID, now/3600), "working the same belts in "+systemNameOf(ctx, m, sysID))
	}
}

// relForgetPresence drops a corporation's presence everywhere (CEO removal).
func relForgetPresence(corpID int64) {
	relPresenceMu.Lock()
	for _, sys := range relPresence {
		delete(sys, corpID)
	}
	for k := range relCrowdFiled {
		if k[1] == corpID || k[2] == corpID {
			delete(relCrowdFiled, k)
		}
	}
	relPresenceMu.Unlock()
}

func init() { registerCorpForget("relPresence", relForgetPresence) }

// ── wars ────────────────────────────────────────────────────────────────────

type relWarMark struct {
	retracted, finished bool
}

var (
	relWarSeenMu sync.Mutex
	relWarSeen   = map[int64]relWarMark{}
	relWarSeeded bool
)

// relDiffWars files war declarations, retractions and endings the snapshot
// build sees for the first time. The first build after boot seeds silently:
// a restart must not re-file every war on the shard. Called from
// relLoadWars with the full war list, before the snapshot is installed.
func (m *Mod) relDiffWars(ctx context.Context, s *relSnapshot, wars []evedb.WarView) {
	// The same transitions open and close the war's Summit thread
	// (summit_war.go), whether or not the ledger is on. Registered before
	// the lock, so it runs after the unlock.
	var summitDiffs []relWarDiff
	defer func() {
		for _, d := range summitDiffs {
			m.summitWarChange(ctx, d.war, d.tag)
		}
	}()
	relWarSeenMu.Lock()
	defer relWarSeenMu.Unlock()
	if !relWarSeeded {
		for _, w := range wars {
			relWarSeen[w.WarID] = relWarMark{retracted: w.Retracted.Valid, finished: w.TimeFinished.Valid}
		}
		relWarSeeded = true
		return
	}
	if !relLedgerOn() {
		for _, w := range wars {
			prev, seen := relWarSeen[w.WarID]
			cur := relWarMark{retracted: w.Retracted.Valid, finished: w.TimeFinished.Valid}
			relWarSeen[w.WarID] = cur
			if tag := relWarTransition(prev, seen, cur); tag != "" {
				summitDiffs = append(summitDiffs, relWarDiff{w.War, tag})
			}
		}
		return
	}
	// alliance to member corporations, for expanding an owner to the
	// corporations it binds.
	members := map[int64][]int64{}
	for corp, ally := range s.ally {
		members[ally] = append(members[ally], corp)
	}
	corpsOf := func(owner int64) []int64 {
		if cs := members[owner]; len(cs) > 0 {
			return cs
		}
		return []int64{owner}
	}
	present := map[int64]bool{}
	for _, w := range wars {
		present[w.WarID] = true
		prev, seen := relWarSeen[w.WarID]
		cur := relWarMark{retracted: w.Retracted.Valid, finished: w.TimeFinished.Valid}
		relWarSeen[w.WarID] = cur
		against, declarer := corpsOf(w.AgainstID), corpsOf(w.DeclaredByID)
		tag := relWarTransition(prev, seen, cur)
		switch tag {
		case "dec":
			m.fileWarIncident(ctx, w.WarID, incWarDec, against, declarer, "dec")
		case "ret":
			m.fileWarIncident(ctx, w.WarID, incWarRetract, against, declarer, "ret")
		case "end":
			m.fileWarIncident(ctx, w.WarID, incWarEnd, against, declarer, "end")
		}
		if tag != "" {
			summitDiffs = append(summitDiffs, relWarDiff{w.War, tag})
		}
	}
	// A war gone from the list ended.
	for id, prev := range relWarSeen {
		if !present[id] {
			delete(relWarSeen, id)
			_ = prev
		}
	}
}

// relWarDiff is one war transition: "dec", "ret" or "end".
type relWarDiff struct {
	war evedb.War
	tag string
}

// relWarTransition names what changed in a war since the last snapshot:
// "dec" a new live war, "ret" a retraction, "end" a war that finished.
func relWarTransition(prev relWarMark, seen bool, cur relWarMark) string {
	switch {
	case !seen && !cur.finished && !cur.retracted:
		return "dec"
	case seen && !prev.retracted && cur.retracted:
		return "ret"
	case seen && !prev.finished && cur.finished:
		return "end"
	}
	return ""
}

// fileWarIncident files one war event: the Against side is the victim of a
// declaration and the beneficiary of a retraction; an ending is mutual.
func (m *Mod) fileWarIncident(ctx context.Context, warID int64, kind string, against, declarer []int64, tag string) {
	key := fmt.Sprintf("war:%d:%s", warID, tag)
	p := relParamsNow()
	for _, v := range against {
		for _, a := range declarer {
			if kind == incWarEnd {
				m.noteCorpIncidentMutual(ctx, v, a, kind, 0, 0, warID, key, "the war ended")
				continue
			}
			m.noteCorpIncident(ctx, v, a, kind, relSideVictim, relBaseWeight(kind, relSideVictim, 0, p),
				0, 0, warID, key, "war "+tag)
			m.noteCorpIncident(ctx, a, v, kind, relSideAggressor, relBaseWeight(kind, relSideAggressor, 0, p),
				0, 0, warID, key, "war "+tag)
		}
	}
}
