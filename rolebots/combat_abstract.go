package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/npc"
	"gurimoon/rates"
)

// Abstract-tier combat: the fights nobody watches. An unwatched bot working a belt
// periodically draws a spawn from the tables the live spawner reads (npcRegionSpawns for
// which rats, npcSpawnerRules for how many) and the encounter is resolved as a duel: both
// sides' damage per second against their effective hitpoints, ticked a second at a time.
// Armour lost here is written to the repair table the embodied tier uses. Capsule loss is
// not modelled: belt rats never shoot pods (the NPC brain skips group 29).
// Bounties are the attribute-481 payout through the refType-17 journal for rats that died
// (bountypay.go, scaled by rolebots.abstractBountyRate). Loot is rolled from npc_loot_entries
// with the same gates and rates.LootDrop (beltloot.go); drops go only to tasks that declare
// Loots, and no container is made.

const (
	// One encounter check per this much accumulated exposure. A bot working a
	// belt draws roughly one spawn every 10-15 minutes at the rate below,
	// which is about what a player sitting on the same belt would see.
	abstractExposureStep = 90 * time.Second
	// Chance a check actually pulls a spawn.
	abstractEncounterChance = 0.18

	// Ceiling on the duel simulation, so a pair of unkillable stalemates can
	// never spin. At one second per iteration this is 10 minutes of fighting,
	// far longer than any real belt engagement.
	abstractDuelMaxTicks = 600

	// A patrol or hunting pack is looking for rats, belt after belt, so its checks pull a spawn
	// far more often than a miner's.
	abstractPatrolEncounterChance = 0.5

	// The bot disengages below this share of total tank, mirroring the
	// embodied flee thresholds. Getting out is not free: see abstractEscape.
	abstractBreakOffFrac = 0.35
	// An unarmed hull tries to leave as soon as it is hit at all.
	abstractUnarmedBreakOffFrac = 0.85

	// Chance per tick that a bot trying to leave actually gets clear. Warping
	// out takes an align and a spool-up, and rats keep shooting through it.
	abstractEscapePerTick = 0.25

	// What a hull still hits for while it aligns out. Half is an approximation: the duel has no
	// hit roll, as rangeabstract.go notes of tracking. It must not be zero, since a declined
	// fight sets leaving before tick 0 and would fire no shots.
	abstractAligningFireFrac = 0.5
)

// ratThreat is one spawnable NPC reduced to the numbers a duel needs, plus the identity the
// killboard needs. CorpID, FactionID and Dealt are per spawn, not per type; everything above
// them is cached per typeID and read-only (drawSpawn copies the cached value first).
type ratThreat struct {
	TypeID       int64
	Name         string
	DPS          float64
	EHP          float64
	Weight       int
	WeaponTypeID int64 // missile/turret typeID, for the kill report
	// Bounty is the type's attribute-481 bounty at the server rate
	// (evedb.GetNPCBounty). Read for the hunt ladder (huntladder.go).
	Bounty float64

	CorpID int64 // owning NPC corporation, from the spawn row
	// FactionID is who this rat answers to (persisted, db/beltwavedb.go). Without it a pilot
	// employed by one pirate faction cannot tell its own rats from another's (faction.go).
	FactionID int64
	Dealt     float64 // EHP damage this rat has done to the bot

	// BeltRatID and Role identify the persisted rat this is a copy of. A rat killed here is
	// killed for everyone: the row is marked dead, the belt stamped when it empties, and an
	// officer hands its region slot back. Zero on the legacy path (no spawner manager).
	BeltRatID int64
	Role      string
	BeltID    int64
	RegionID  int64
	// SiteObjectID is the dunObjects row this rat stands for on an open site (siterun.go), what
	// the core is told died. Zero on a belt rat.
	SiteObjectID int64
	// MaxEHP is what this rat had when the fight started, so partial damage can be written back
	// to a survivor's row. HP is the raw shield/armor/structure split, spent in that order.
	MaxEHP float64
	HP     [3]float64

	// Reach and closing speed for the range model (rangeabstract.go), cached per typeID. ReachM
	// comes from NPCWeaponProfile.EffectiveRange, the accessor the NPC brain flies on.
	ReachM  float64
	SpeedMS float64
	// SigRadius is the rat's signature radius (attribute 552), what a bot's guns apply against
	// (application.go).
	SigRadius float64
}

var (
	ratThreatMu    sync.Mutex
	ratThreatCache = map[int64]*ratThreat{}

	exposureMu sync.Mutex
	exposureAt = map[int64]time.Time{}
)

// ── the pass ────────────────────────────────────────────────────────────────

// abstractCombatPass runs the unwatched-space risk for one bot. Called once per
// sim pass; cheap and returns immediately for every bot that is docked, in a
// watched system, or not exposed.
func (m *Mod) abstractCombatPass(ctx context.Context, b *botRun) {
	// A pilot inside a deadspace pocket fights what the pocket holds (siterun.go), not what
	// belts roll. Checked here, not in exposedInSpace, which packMatesFor reads.
	if b.State != nil && b.State.Data.SiteID != 0 {
		return
	}
	if !m.exposedInSpace(b) {
		exposureMu.Lock()
		delete(exposureAt, b.CharID)
		exposureMu.Unlock()
		return
	}

	// Accumulate exposure and only roll on the step boundary.
	exposureMu.Lock()
	last, seen := exposureAt[b.CharID]
	if !seen {
		exposureAt[b.CharID] = clock.Now()
		exposureMu.Unlock()
		return
	}
	if clock.Now().Sub(last) < abstractExposureStep {
		exposureMu.Unlock()
		return
	}
	exposureAt[b.CharID] = clock.Now()
	exposureMu.Unlock()

	chance := b.Tune.EncounterChance
	// Anything with its guns out and looking for a fight draws far more trouble than a miner.
	// Read off the task's posture (task.go), so a new hunting phase gets the hunter's rate.
	if postureFor(b.State.Phase).patrol() {
		chance = b.Tune.PatrolEncounterChance
	}
	switch b.State.Phase {
	case "gang.mine":
		// A miner whose gang has a live patrol in the op system works under cover: the patrol soaks
		// the spawns. The patrol bots run their own, hotter risk above.
		if m.gangPatrolCovers(ctx, b.State.Data.GangID, b.SystemID) {
			return
		}
	}
	// The solo version of the same cover: somebody cleared this system's spawn lately
	// (hostilemem.go), so a working ship draws far less trouble. Never for patrol postures.
	if p := postureFor(b.State.Phase); p.mining() && !p.patrol() && patrolCovered(botScope(b), b.SystemID) {
		chance *= patrolCoverEncounterMul
	}
	if rand.Float64() > chance {
		return
	}

	spawn, err := m.drawSpawn(ctx, b)
	if err != nil || len(spawn) == 0 {
		return
	}
	m.resolveAbstractEncounter(ctx, b, spawn)
}

// gangPatrolCovers reports whether a non-halted patrol of this gang is on station in the
// system. The gangID lives inside the state JSON, so the match is a LIKE over the blob.
func (m *Mod) gangPatrolCovers(ctx context.Context, gangID, systemID int64) bool {
	return m.gangPhaseInSystem(ctx, gangID, systemID, TaskGangPatrol)
}

// gangPhaseInSystem reports whether a non-halted member of this gang is in
// the system in one of the given phases. The hauler's cover gate asks it
// about the patrol, and about the MINERS: a belt the op's own miners are
// parked on unharmed is a belt a hauler can visit.
func (m *Mod) gangPhaseInSystem(ctx context.Context, gangID, systemID int64, phases ...TaskKind) bool {
	if gangID == 0 || len(phases) == 0 {
		return false
	}
	args := make([]any, 0, len(phases)+3)
	for _, p := range phases {
		args = append(args, string(p))
	}
	args = append(args, systemID,
		fmt.Sprintf(`%%"gangID":%d,%%`, gangID),
		fmt.Sprintf(`%%"gangID":%d}%%`, gangID))
	var one int64
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM rolebot_state s
		JOIN character_ c ON c.characterID = s.characterID
		WHERE s.phase IN (`+placeholders(len(phases))+`) AND s.halted = 0 AND c.solarSystemID = ?
		  AND (s.data LIKE ? OR s.data LIKE ?)
		LIMIT 1`, args...).Scan(&one)
	return err == nil
}

// systemHasLiveRats reports whether anything is actually on the belts of sysID (npcBeltRats
// is the real contents whether or not a client has the system loaded). It backs the hauler's
// cover gate: an op under four bodies has no combat seat (gangComposition), so
// gangPatrolCovers never holds, and with nothing on the belts there is nothing to wait for.
func (m *Mod) systemHasLiveRats(ctx context.Context, systemID int64) bool {
	if systemID == 0 {
		return false
	}
	var one int64
	err := m.db.QueryRowContext(ctx,
		`SELECT 1 FROM npcBeltRats WHERE solarSystemID = ? AND alive = 1 LIMIT 1`,
		systemID).Scan(&one)
	// An unreadable census reads as DANGEROUS, so a broken query cannot open
	// the gate it is here to keep shut.
	return err == nil || !errors.Is(err, sql.ErrNoRows)
}

// exposedInSpace reports whether this bot is somewhere a belt spawn could reach it and nobody
// is watching. Docked bots are safe, and a bot in a system a player has loaded is the
// embodied tier's problem: running both would damage it twice.
func (m *Mod) exposedInSpace(b *botRun) bool {
	if b.StationID != 0 || b.ShipID == 0 {
		return false
	}
	// systemProjected, not systemWatched: a ring bot has a real ball with the real combat guard,
	// so the narrow predicate would run both tiers against the same rats.
	if m.systemProjected(b.SystemID) {
		return false
	}
	// Rats live on belts, so only tasks that park a ship on one are exposed (TaskDef.Exposed in
	// task.go). A hauler loading cans is not: the patrol is what makes that safe.
	return exposedPhase(b.State.Phase)
}

// drawSpawn reads the rats actually at the bot's belt: the persisted wave (npc/beltwave.go,
// db/beltwavedb.go) that EnsureBeltWave rolls on the first visit, minus what has been killed
// and minus whatever this pilot's employer is on good terms with (faction.go). A belt with
// nobody willing to fight this pilot produces no encounter.
func (m *Mod) drawSpawn(ctx context.Context, b *botRun) ([]*ratThreat, error) {
	if m.spawner == nil {
		// No spawner manager (an older core, or a harness Mod built without one): there is nothing
		// authoritative to fight, so no wave is invented.
		return nil, nil
	}

	beltID, security, err := m.beltForBot(ctx, b)
	if err != nil {
		return nil, err
	}
	if beltID == 0 {
		return nil, nil // a system with no belts: nothing to be jumped at
	}

	spawn, err := m.hostileRatsAtBelt(ctx, b, beltID, b.SystemID, security)
	if err == nil && len(spawn) == 0 {
		// The belt this pilot was pointed at is empty, cleared by this pilot or another since the
		// pick. Drop the pick so the next roll walks again; a hunter holding an empty belt draws nothing.
		m.releaseHuntBelt(ctx, b)
		// And do not come back this stint: a belt still cooling down answers empty on every visit,
		// and huntFallbackBelt would pick it again.
		m.noteHuntSkip(ctx, b, beltID)
	}
	return spawn, err
}

// ratsAtBelt is what is standing at one belt right now, as combatants. It materialises the
// belt's wave if nothing has visited yet, so a bot can look before it commits.
func (m *Mod) ratsAtBelt(ctx context.Context, beltID, systemID int64, security float64) ([]*ratThreat, error) {
	if m.spawner == nil || beltID == 0 {
		return nil, nil
	}
	wave, rats, err := m.spawner.EnsureBeltWave(ctx, beltID, systemID, security)
	if err != nil {
		return nil, fmt.Errorf("ratsAtBelt %d: %w", beltID, err)
	}

	var spawn []*ratThreat
	for _, rat := range rats {
		if !rat.Alive {
			continue
		}
		t, err := m.ratThreatFor(ctx, rat.TypeID)
		if err != nil || t == nil {
			continue
		}
		r := ratThreatFromRow(t, rat, beltID, wave.RegionID)
		if r.EHP <= 0 {
			continue
		}
		spawn = append(spawn, r)
	}
	return spawn, nil
}

// ratThreatFromRow stamps one persisted rat's identity onto a copy of its cached per-type
// combat numbers. Each rat is its own combatant, so the slice holds copies and per-rat
// ownership and Dealt can be written without touching the shared cache. FactionID must be
// carried over, or every pilot fights every rat regardless of employer.
func ratThreatFromRow(t *ratThreat, rat evedb.BeltRatRow, beltID, regionID int64) *ratThreat {
	r := *t
	r.CorpID = rat.CorporationID
	r.FactionID = rat.FactionID
	r.BeltRatID = rat.BeltRatID
	r.Role = rat.Role
	r.BeltID = beltID
	r.RegionID = regionID
	// A rat someone already softened comes into the fight softened.
	r.EHP = t.EHP * ratHealthFrac(rat)
	r.MaxEHP = t.EHP
	return &r
}

// hostileRatsAtBelt is ratsAtBelt narrowed to what will fight this pilot. ratsAtBelt is a
// question about the belt (it materialises the wave and is the census the damage writeback
// settles against); this is a question about the pilot, and for a pilot on a pirate
// faction's payroll it is a subset (faction.go).
func (m *Mod) hostileRatsAtBelt(ctx context.Context, b *botRun, beltID, systemID int64,
	security float64) ([]*ratThreat, error) {

	spawn, err := m.ratsAtBelt(ctx, beltID, systemID, security)
	if err != nil || len(spawn) == 0 || b == nil || b.FactionID == 0 {
		return spawn, err
	}
	mx, ok := m.standingsMatrix(ctx)
	if !ok {
		// An unreadable standings table means every rat is hostile: wrong for a pirate-corp pilot
		// but survivable. Reading it as nothing hostile would leave a roster that never defends itself.
		return spawn, nil
	}
	out := filterHostileRats(mx, b.FactionID, spawn)
	if len(out) == 0 && b.State != nil && postureFor(b.State.Phase).patrol() {
		// One throttled log line: a hunter that earns nothing because every rat is a colleague looks
		// like a broken hunter.
		m.noteFriendlyBelt(b, len(spawn))
	}
	return out, nil
}

var (
	friendlyBeltMu   sync.Mutex
	friendlyBeltSaid = map[int64]time.Time{}
)

// friendlyBeltLogEvery throttles the "everyone here works for my employer"
// line to something an operator can read.
const friendlyBeltLogEvery = 30 * time.Minute

func (m *Mod) noteFriendlyBelt(b *botRun, held int) {
	if held == 0 {
		return
	}
	now := clock.Now()
	friendlyBeltMu.Lock()
	last, seen := friendlyBeltSaid[b.CharID]
	if seen && now.Sub(last) < friendlyBeltLogEvery {
		friendlyBeltMu.Unlock()
		return
	}
	friendlyBeltSaid[b.CharID] = now
	friendlyBeltMu.Unlock()
	m.logBot(b.CharID, logInfo, "combat",
		"%d rat(s) on this belt and every one of them flies for faction %d, which signs this pilot's paycheque — nothing to shoot here",
		held, b.FactionID)
}

// ratHealthFrac reduces a persisted rat's three health layers to one multiplier on its
// effective hitpoints. It is the mean, since the abstract duel has one EHP pool.
func ratHealthFrac(rat evedb.BeltRatRow) float64 {
	f := (rat.ShieldFrac + rat.ArmorFrac + rat.StructureFrac) / 3
	if f > 1 {
		return 1
	}
	if f < 0 {
		return 0
	}
	return f
}

// beltForBot resolves which belt an exposed bot is sitting at, and its system's security. A
// gang op names its belt; everything else is placed by parkAtBelt/parkAtCelestial with a real
// position, so the nearest belt is the belt it is at, as a player warping to it would see.
func (m *Mod) beltForBot(ctx context.Context, b *botRun) (beltID int64, security float64, err error) {
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(security, 0) FROM mapSolarSystems WHERE solarSystemID = ?`,
		b.SystemID).Scan(&security); err != nil {
		return 0, 0, fmt.Errorf("beltForBot system %d: %w", b.SystemID, err)
	}

	if id := b.State.Data.GangBeltID; id != 0 {
		return id, security, nil
	}
	// A solo miner records the belt it is working (mining.go), which beats geometry: it is the
	// belt the ore is debited from.
	if id := b.State.Data.MineBeltID; id != 0 {
		return id, security, nil
	}
	// A hunter picks its belt by what is standing on it (pickHuntBelt) and remembers the pick as
	// a miner remembers its rock. Geometry says where the pilot is, not where it should fight.
	if id := m.huntBeltFor(ctx, b, security); id != 0 {
		return id, security, nil
	}
	// A hunter with no winnable live belt does not fall through to geometry, which would redraw
	// the same refused spawn every 90 s. It tries an unseen belt; when there is none, the system is dry.
	if b.State != nil && postureFor(b.State.Phase).patrol() {
		if id := m.huntFallbackBelt(ctx, b); id != 0 {
			return id, security, nil
		}
		m.markHuntDry(ctx, b)
		return 0, security, nil
	}

	if b.ShipID != 0 {
		var x, y, z float64
		if err := m.db.QueryRowContext(ctx,
			`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
			b.ShipID).Scan(&x, &y, &z); err == nil && (x != 0 || y != 0 || z != 0) {
			var id int64
			if err := m.db.QueryRowContext(ctx, `
				SELECT itemID FROM mapDenormalize
				WHERE solarSystemID = ? AND groupID = 9
				ORDER BY POW(x-?,2) + POW(y-?,2) + POW(z-?,2)
				LIMIT 1`, b.SystemID, x, y, z).Scan(&id); err == nil {
				return id, security, nil
			}
		}
	}

	// No usable position: fall back to the belt the miner would have chosen.
	return m.beltWithMostOre(ctx, b.SystemID, 0), security, nil
}

// ── the hunter's belt ───────────────────────────────────────────────────────

const (
	// huntBeltLookMax bounds how many belts a hunter looks at before settling: systems average
	// 10.5 belts and the worst has 51, and a full walk costs every hunter on every exposure roll.
	huntBeltLookMax = 8

	// huntBeltHoldSec is how long a pick stands before it is made again. It is shorter than a
	// tactic stint (combattactic.go), so a pilot re-reads its ground at least once per trip.
	huntBeltHoldSec = 600
)

// huntBeltFor is the remembered pick, re-picked when it is stale or gone. Only patrol
// postures have one: a miner's belt is where its ore is and a hauler's is where the cans are.
func (m *Mod) huntBeltFor(ctx context.Context, b *botRun, security float64) int64 {
	if b == nil || b.State == nil || !postureFor(b.State.Phase).patrol() {
		return 0
	}
	d := &b.State.Data
	now := clock.Unix()
	// A pick made in another system is not a pick. HuntSystemID is the sweep's
	// own record of where it is working and it moves before this does.
	if huntPickHolds(d.HuntBeltID, d.HuntBeltAt, d.HuntSystemID, b.SystemID, now) {
		return d.HuntBeltID
	}

	// The pick is persisted: abstractCombatPass runs after the sim pass has saved, so an unsaved
	// field is gone by the next roll and the walk would run on every 90 s check.
	prev := d.HuntBeltID
	id := m.pickHuntBelt(ctx, b, security)
	d.HuntBeltID = id
	d.HuntBeltAt = 0
	if id != 0 {
		d.HuntBeltAt = now
	}
	// Saved only when there is something to save: a hunter in a swept system answers 0 on every
	// roll, and rewriting nothing each huntBeltHoldSec is a wasted update.
	if id != 0 || prev != 0 {
		m.saveHuntBelt(ctx, b)
	}
	// Zero falls through to geometry in the caller, so a hunter in a swept system still has
	// somewhere to be.
	return id
}

// huntPickHolds reports whether a stored pick may be used as it stands. Pure, since the walk
// needs a database and a spawner. A pick stops holding when there is none, when the sweep has
// moved system since it was made (HuntSystemID moves before this does), or when it is stale.
func huntPickHolds(beltID, beltAt, huntSysID, sysID, now int64) bool {
	if beltID == 0 {
		return false
	}
	if huntSysID != 0 && huntSysID != sysID {
		return false
	}
	return now-beltAt < huntBeltHoldSec
}

// saveHuntBelt persists a changed pick on its own context budget: the caller's may be close
// to its deadline, and an unsaved pick makes the walk run again.
func (m *Mod) saveHuntBelt(ctx context.Context, b *botRun) {
	if b == nil || b.State == nil {
		return
	}
	if m.db == nil {
		return
	}
	if err := saveState(ctx, m.db, b.State); err != nil {
		rlogf("rolebots: save hunt belt charID=%d: %v", b.CharID, err)
	}
}

// releaseHuntBelt drops a pick whose belt turned out to have nothing on it. Called from
// drawSpawn on an empty draw, at no extra cost. Without it a hunter that cleared its belt
// would keep pointing at it for the rest of huntBeltHoldSec.
func (m *Mod) releaseHuntBelt(ctx context.Context, b *botRun) {
	if b == nil || b.State == nil || b.State.Data.HuntBeltID == 0 {
		return
	}
	b.State.Data.HuntBeltID, b.State.Data.HuntBeltAt = 0, 0
	m.saveHuntBelt(ctx, b)
}

// huntSkipMax bounds the skip list: a pilot that turned down this many belts in one system
// has seen all the walk would show (huntBeltLookMax).
const huntSkipMax = 12

// pushHuntSkip records a turned-down belt, newest first. Pure. A list kept for another
// system is dropped, not extended.
func pushHuntSkip(list []int64, listSys, sysID, beltID int64) []int64 {
	if listSys != sysID {
		list = nil
	}
	out := make([]int64, 0, huntSkipMax)
	out = append(out, beltID)
	for _, id := range list {
		if id == beltID {
			continue
		}
		if len(out) >= huntSkipMax {
			break
		}
		out = append(out, id)
	}
	return out
}

// huntSkipped reports whether this pilot has already turned down beltID in
// the system it is in now.
func huntSkipped(d *StateData, sysID, beltID int64) bool {
	if d == nil || d.HuntSkipSys != sysID {
		return false
	}
	for _, id := range d.HuntSkipBelts {
		if id == beltID {
			return true
		}
	}
	return false
}

// noteHuntSkip puts a belt on this hunter's skip list and drops the pick if
// it was the pick, so the next roll walks somewhere else. Patrol postures
// only: a miner's belt is where its ore is, and it has no walk to redirect.
func (m *Mod) noteHuntSkip(ctx context.Context, b *botRun, beltID int64) {
	if b == nil || b.State == nil || beltID == 0 || !postureFor(b.State.Phase).patrol() {
		return
	}
	d := &b.State.Data
	d.HuntSkipBelts = pushHuntSkip(d.HuntSkipBelts, d.HuntSkipSys, b.SystemID, beltID)
	d.HuntSkipSys = b.SystemID
	if d.HuntBeltID == beltID {
		d.HuntBeltID, d.HuntBeltAt = 0, 0
	}
	m.saveHuntBelt(ctx, b)
}

// huntFallbackBelt is the belt a hunter tries when no live wave in its system is one it
// would fight: the nearest belt it has not turned down that has no uncleared wave (never
// visited, or cleared and waiting to reseed). The encounter roll there materialises what is
// really on it (drawSpawn). One belt per roll.
func (m *Mod) huntFallbackBelt(ctx context.Context, b *botRun) int64 {
	if b == nil || b.State == nil || m.db == nil {
		return 0
	}
	var sx, sy, sz float64
	if b.ShipID != 0 {
		_ = m.db.QueryRowContext(ctx,
			`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
			b.ShipID).Scan(&sx, &sy, &sz)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT d.itemID
		FROM mapDenormalize d
		LEFT JOIN npcBeltWaves w ON w.celestialID = d.itemID
		WHERE d.solarSystemID = ? AND d.groupID = 9
		  AND (w.celestialID IS NULL OR w.lastClearAt > 0)
		ORDER BY POW(d.x-?,2) + POW(d.y-?,2) + POW(d.z-?,2)
		LIMIT 64`, b.SystemID, sx, sy, sz)
	if err != nil {
		rlogf("rolebots: huntFallbackBelt %d: %v", b.SystemID, err)
		return 0
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) != nil {
			continue
		}
		if !huntSkipped(&b.State.Data, b.SystemID, id) {
			return id
		}
	}
	return 0
}

// markHuntDry records that this system has nothing left for this hunter: no live belt it
// would fight, nothing unvisited to try. The sweep reads it and moves on (combatStep,
// gangHuntStep). Stamped once; the stamp dies with the stint (idleData).
func (m *Mod) markHuntDry(ctx context.Context, b *botRun) {
	if b == nil || b.State == nil || b.State.Data.HuntDryAt != 0 {
		return
	}
	b.State.Data.HuntDryAt = clock.Unix()
	m.saveHuntBelt(ctx, b)
}

// pickHuntBelt walks this system's live belts nearest-first and takes the first one this
// pilot would fight. It looks only at belts already carrying an uncleared wave
// (lastClearAt = 0): EnsureBeltWave rolls and persists a wave on a virgin belt, so probing
// every belt would seed the world. See docs/comment-notes-b02.md#hunt-belt.
func (m *Mod) pickHuntBelt(ctx context.Context, b *botRun, security float64) int64 {
	if m.spawner == nil {
		return 0
	}
	// The ship's position, so the walk is nearest-first. With no readable position the order is
	// arbitrary.
	var sx, sy, sz float64
	if b.ShipID != 0 {
		_ = m.db.QueryRowContext(ctx,
			`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
			b.ShipID).Scan(&sx, &sy, &sz)
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT w.celestialID, COALESCE(d.x,0), COALESCE(d.y,0), COALESCE(d.z,0)
		FROM npcBeltWaves w
		JOIN mapDenormalize d ON d.itemID = w.celestialID
		WHERE w.solarSystemID = ? AND w.lastClearAt = 0
		ORDER BY POW(d.x-?,2) + POW(d.y-?,2) + POW(d.z-?,2)
		LIMIT ?`, b.SystemID, sx, sy, sz, huntBeltLookMax+huntSkipMax)
	if err != nil {
		rlogf("rolebots: pickHuntBelt %d: %v", b.SystemID, err)
		return 0
	}
	var belts []int64
	for rows.Next() {
		var id int64
		var x, y, z float64
		if rows.Scan(&id, &x, &y, &z) == nil {
			belts = append(belts, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		rlogf("rolebots: pickHuntBelt %d rows: %v", b.SystemID, err)
		return 0
	}

	botDPS, botEHP, powerKnown := m.effectivePower(ctx, b)
	looked := 0
	for _, beltID := range belts {
		// A belt this pilot already turned down is not re-offered: its spawn
		// persists, so the answer would be the same one.
		if huntSkipped(&b.State.Data, b.SystemID, beltID) {
			continue
		}
		// The look bound counts belts actually weighed; the query fetches a skip list's worth extra.
		if looked++; looked > huntBeltLookMax {
			break
		}
		// hostileRatsAtBelt, never ratsAtBelt: a belt whose whole wave is friendly to this pilot's
		// employer would be stood on drawing nothing (regionSpawnFactions, faction.go).
		spawn, err := m.hostileRatsAtBelt(ctx, b, beltID, b.SystemID, security)
		if err != nil || len(spawn) == 0 {
			continue
		}
		if !powerKnown {
			// Nothing known about this hull, so nothing to judge with. The
			// belt has hostiles on it, which is more than geometry knew.
			return beltID
		}
		if wouldEngage(b.Tune, threatMargin(botDPS, botEHP, spawnThreatOf(spawn))) {
			return beltID
		}
	}
	return 0
}

// spawnSizeFor reads the spawner rule covering this security band: the same row the live
// spawner sizes a belt from.
func (m *Mod) spawnSizeFor(ctx context.Context, security float64) (minShips, maxShips int) {
	minShips, maxShips = 1, 3
	var lo, hi int
	err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(minShips, 0), COALESCE(maxShips, 0) FROM npcSpawnerRules
		WHERE ? >= secMin AND ? <= secMax
		ORDER BY (secMax - secMin) LIMIT 1`, security, security).Scan(&lo, &hi)
	if err != nil {
		return minShips, maxShips
	}
	if hi > 0 {
		maxShips = hi
	}
	if lo > 0 {
		minShips = lo
	}
	if minShips > maxShips {
		minShips = maxShips
	}
	if minShips < 1 {
		minShips = 1
	}
	return minShips, maxShips
}

// ratThreatFor resolves one NPC type into damage per second and effective
// hitpoints, cached for the life of the process.
func (m *Mod) ratThreatFor(ctx context.Context, typeID int64) (*ratThreat, error) {
	ratThreatMu.Lock()
	if t, ok := ratThreatCache[typeID]; ok {
		ratThreatMu.Unlock()
		return t, nil
	}
	ratThreatMu.Unlock()

	wp, err := evedb.GetNPCWeaponProfile(ctx, m.db, typeID)
	if err != nil {
		return nil, err
	}
	hp, err := evedb.GetEntityHPByType(ctx, m.db, typeID)
	if err != nil {
		return nil, err
	}
	name, _ := evedb.GetTypeName(ctx, m.db, typeID)

	t := &ratThreat{TypeID: typeID, Name: name, WeaponTypeID: typeID}
	t.Bounty = evedb.GetNPCBounty(ctx, m.db, typeID)
	if wp != nil && wp.CycleTimeMs > 0 {
		perCycle := (wp.Em + wp.Explosive + wp.Kinetic + wp.Thermal) * wp.Multiplier
		t.DPS = perCycle / (float64(wp.CycleTimeMs) / 1000.0) * rates.NPCDamage
	}
	// What the kill report names as the weapon, in the live paths' order: the missile for a
	// missile rat (main.go's impact handler), else the derived turret (npc/manager.go), else the hull.
	if wp != nil {
		switch {
		case wp.MissileTypeID > 0:
			t.WeaponTypeID = wp.MissileTypeID
		case wp.TurretTypeID > 0:
			t.WeaponTypeID = wp.TurretTypeID
		}
	}
	if hp != nil {
		t.EHP = ehpOf(hp.ShieldHP, hp.ArmorHP, hp.StructureHP,
			hp.ShieldRes, hp.ArmorRes, hp.HullRes)
		t.HP = [3]float64{hp.ShieldHP, hp.ArmorHP, hp.StructureHP}
	}
	if t.EHP <= 0 {
		t.EHP = 100
	}

	// Reach and closing speed for the range model. The IsUnarmed gate keeps the number honest:
	// GetNPCWeaponProfile fills defaults (10 km optimal), so unarmed hauler rats would read as shooting to 10 km.
	if wp != nil && !npc.IsUnarmed(wp) {
		t.ReachM = wp.EffectiveRange()
	}
	if attrs, err := evedb.TypeAttrs(ctx, m.db, typeID); err == nil {
		t.SpeedMS = attrs[attrMaxVelocity]
		t.SigRadius = attrs[attrSignatureRadius]
	}

	ratThreatMu.Lock()
	ratThreatCache[typeID] = t
	ratThreatMu.Unlock()
	return t, nil
}

// spawnReachOf reduces a spawn to what the range model needs. Its own function
// so rangeAdvantage stays testable without a database, the way ewarEffect
// keeps ewarSuppression testable.
func spawnReachOf(spawn []*ratThreat) []ratReach {
	out := make([]ratReach, 0, len(spawn))
	for _, r := range spawn {
		if r == nil || r.EHP <= 0 {
			continue // already dead; it is not closing on anybody
		}
		out = append(out, ratReach{ReachM: r.ReachM, SpeedMS: r.SpeedMS, DPS: r.DPS})
	}
	return out
}

// ehpOf folds three HP layers and their resonances into one uniform-damage effective-hitpoint
// figure, the reduction fitstats.go uses.
func ehpOf(shield, armor, structure float64, sRes, aRes, hRes [4]float64) float64 {
	avg := func(r [4]float64) float64 {
		s := (r[0] + r[1] + r[2] + r[3]) / 4
		if s <= 0.01 {
			return 0.01
		}
		return s
	}
	return shield/avg(sRes) + armor/avg(aRes) + structure/avg(hRes)
}

// ── the duel ────────────────────────────────────────────────────────────────

// abstractCombatant is the bot's side of the fight.
type abstractCombatant struct {
	DPS    float64
	EHP    float64
	MaxEHP float64
	Armed  bool
	// RepPerSec is the remote repair this hull puts on its side; SelfRepPerSec what its own
	// boosters and repairers put back on itself. Raw hitpoints a second, scaled by what the
	// capacitor can sustain (repSustain). Zero on a hull with no repairer.
	RepPerSec     float64
	SelfRepPerSec float64
	MaxHP         [3]float64 // shield, armor, structure pools at full
	CurPool       [3]float64 // and where they are now
	// Ewar is the fitted electronic rack, reduced to what the duel can use (ewarabstract.go).
	// Carried, not folded into DPS or EHP, because its worth depends on how many rats shoot.
	Ewar []ewarEffect
	// Reach and lock range for the range model (rangeabstract.go), carried for the same reason
	// as Ewar. ReachM is the shortest armed weapon's (as FitStats.TackleRangeM).
	ReachM float64
	LockM  float64
	// SpeedMS is the hull's own maxVelocity, and Stance what it does with it (combatdoctrine.go).
	// Carried like Ewar and ReachM, since what legs are worth depends on the opponent.
	SpeedMS float64
	Stance  combatStance
	// Drones is the flight this pilot puts into space, already counted in DPS, carried so the
	// fight can cost it drones afterwards (droneloss.go).
	Drones []droneLaunchPick
	// Guns is every weapon's share of DPS with what application reads (application.go). DPS stays
	// the nominal sum, drones included; appliedDPS takes the guns' share back out and lands it
	// through the hit model. HoldOptM and AllMissile are what the embodied loop flies its range on
	// (loadoutHoldOptimal).
	Guns       []gunProfile
	HoldOptM   float64
	AllMissile bool
	// SigRadius is the hull's signature radius (attribute 552, FitStats.SigRadius), what the other
	// side's guns and missiles apply against.
	SigRadius float64
}

// botCombatant resolves what this bot brings to an unwatched fight, starting
// from the damage its hull is already carrying.
func (m *Mod) botCombatant(ctx context.Context, b *botRun) (*abstractCombatant, error) {
	var shipTypeID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ?`, b.ShipID).Scan(&shipTypeID); err != nil {
		return nil, fmt.Errorf("botCombatant hull type: %w", err)
	}

	base, err := evedb.GetEntityHPByType(ctx, m.db, shipTypeID)
	if err != nil {
		return nil, err
	}
	shieldAdd, armorAdd, _ := evedb.GetShipFittingHPBonuses(ctx, m.db, b.ShipID)
	shieldSkill, armorSkill, hullSkill, _, err := evedb.GetTankSkillMultipliers(ctx, m.db, b.CharID)
	if err != nil {
		shieldSkill, armorSkill, hullSkill = 1, 1, 1
	}
	hullB, _ := evedb.GetShipHullBonuses(ctx, m.db, b.CharID, shipTypeID)
	hullStats := hullB.StatMults()

	maxShield := (base.ShieldHP + shieldAdd) * shieldSkill * hullStats.ShieldHP
	maxArmor := (base.ArmorHP + armorAdd) * armorSkill * hullStats.ArmorHP
	maxStruct := base.StructureHP * hullSkill

	// Resistances, modules and hull traits included, so a tanked bot really
	// is harder to kill out here.
	res := [3][4]float64{base.ShieldRes, base.ArmorRes, base.HullRes}
	if mult, err := evedb.GetShipResistanceMultipliers(ctx, m.db, b.ShipID, m.fittedActiveSet(ctx, b.ShipID)); err == nil {
		for l := 0; l < 3; l++ {
			for d := 0; d < 4; d++ {
				res[l][d] *= mult[l][d]
			}
		}
	}
	if len(hullB) > 0 {
		hullRes := hullB.ResistMults()
		for l := 0; l < 2; l++ {
			for d := 0; d < 4; d++ {
				res[l][d] *= hullRes[l][d]
			}
		}
	}

	c := &abstractCombatant{
		MaxHP: [3]float64{maxShield, maxArmor, maxStruct},
	}
	c.MaxEHP = ehpOf(maxShield, maxArmor, maxStruct, res[0], res[1], res[2])

	// Start from the damage the ship is already carrying. Shields come back
	// full, as they do everywhere else.
	structDmg, armorDmg, _ := evedb.GetShipCombatDamage(ctx, m.db, b.ShipID)
	c.CurPool = [3]float64{maxShield, math.Max(0, maxArmor-armorDmg), math.Max(0, maxStruct-structDmg)}
	c.EHP = ehpOf(c.CurPool[0], c.CurPool[1], c.CurPool[2], res[0], res[1], res[2])

	// Offence: the real guns, on their real cycles.
	loadout, err := m.loadCombatLoadout(ctx, b.CharID, b.ShipID, shipTypeID)
	if err == nil && loadout != nil {
		for _, w := range loadout.weapons {
			if w.CycleSec > 0 {
				// cycleDamage, not Dmg.Total(): the same fold the shot applies (combat_embodied.go), so a
				// belt's verdict and the real hit agree.
				dps := w.cycleDamage() / w.CycleSec * rates.PlayerDamage
				c.DPS += dps
				c.Guns = append(c.Guns, gunProfileOf(w, dps))
			}
		}
		c.HoldOptM, c.AllMissile = loadoutHoldOptimal(loadout)
		// The drones in the bay, as many as the pilot may launch, on the core's basis for a drone in
		// space (drones.go).
		c.DPS += loadout.droneDPS() * rates.PlayerDamage
		c.Drones = planDroneLaunch(loadout.drones, grpCombatDrone, loadout.droneCap)
		c.Armed = loadout.armed()
		c.Ewar = ewarEffectsOf(loadout)
		c.ReachM = loadoutReachM(loadout)
		c.LockM = loadout.targetRnge
		c.RepPerSec, c.SelfRepPerSec = loadoutRepRates(loadout)
		c.SpeedMS = loadout.topSpeed
	}

	// The hull's legs, read off the attribute ratThreat reads. A miss leaves zero, which stanceFor
	// treats as slow. The loadout's topSpeed is preferred (engine lit, support_embodied.go loadoutTopSpeed).
	// The signature comes off the same row; a miss leaves zero, which applies every gun in full.
	if attrs, err := evedb.TypeAttrs(ctx, m.db, shipTypeID); err == nil {
		if c.SpeedMS <= 0 {
			c.SpeedMS = attrs[attrMaxVelocity]
		}
		c.SigRadius = attrs[attrSignatureRadius]
	}
	return c, nil
}

// loadoutReachM is the range this fit fights at: the shortest armed weapon's (as
// FitStats.TackleRangeM), since opening at the longest gun's range means firing one gun.
// botWeapon.MaxRange already folds optimal + 2x falloff for a turret and velocity x flight
// time for a missile, with hull and skill multipliers (combat_embodied.go).
func loadoutReachM(l *combatLoadout) float64 {
	if l == nil {
		return 0
	}
	shortest := 0.0
	for _, w := range l.weapons {
		if w.MaxRange <= 0 {
			continue
		}
		if shortest == 0 || w.MaxRange < shortest {
			shortest = w.MaxRange
		}
	}
	// A ship that fights with drones alone stands off at control range: its
	// drones fly the distance, not its hull.
	if shortest == 0 && l.droneDPS() > 0 {
		return droneControlRangeM
	}
	return shortest
}

// packPrep is a side assembled for one abstract fight: the drawer plus its gangmates on the
// same patrol (combat_pack.go) as combatants, each with its landed fraction resolved, and the
// side's sums as the pre-fight look reads them. Shared by resolveAbstractEncounter and siterun.go.
type packPrep struct {
	pack []*packMember
	self *packMember
	// sideDPS and sideEHP are the plain sums; standEHP is EHP with each hull's suppression
	// folded in, and the decision is judged on it.
	sideDPS, sideEHP, standEHP  float64
	armed                       bool
	selfSuppress, selfReachHold float64
}

// prepareAbstractPack builds the side for a fight against spawn. Per hull it resolves, before
// the pre-fight look, what its electronic rack keeps off it (ewarabstract.go) and what its
// reach keeps off it (rangeabstract.go); the two multiply with their own caps, and the stance
// (combatdoctrine.go) is resolved first as an input to reach.
func (m *Mod) prepareAbstractPack(ctx context.Context, b *botRun, spawn []*ratThreat) (*packPrep, error) {
	return m.prepareAbstractPackWith(ctx, b, spawn, nil)
}

// prepareAbstractPackWith is prepareAbstractPack with pilots from outside the gang fighting
// alongside: a site's co-runners (siteetiquette.go), who fly the holder's fight without
// joining its fleet.
func (m *Mod) prepareAbstractPackWith(ctx context.Context, b *botRun, spawn []*ratThreat, extra []*botRun) (*packPrep, error) {
	bot, err := m.botCombatant(ctx, b)
	if err != nil {
		return nil, err
	}
	self := &packMember{Run: b, Bot: bot}
	pack := []*packMember{self}
	in := map[int64]bool{b.CharID: true}
	for _, mate := range append(m.packMatesFor(ctx, b), extra...) {
		if mate == nil || in[mate.CharID] {
			continue
		}
		c, err := m.botCombatant(ctx, mate)
		if err != nil || c == nil || c.MaxEHP <= 0 {
			continue
		}
		in[mate.CharID] = true
		pack = append(pack, &packMember{Run: mate, Bot: c})
	}

	ratDPS := 0.0
	for _, r := range spawn {
		ratDPS += r.DPS
	}
	reach := spawnReachOf(spawn)

	pp := &packPrep{pack: pack, self: self}
	for _, p := range pack {
		suppress := ewarSuppression(p.Bot.Ewar, len(spawn))
		fightSec := math.MaxFloat64
		if ratDPS > 0 {
			fightSec = p.Bot.EHP / ratDPS
		}
		p.Bot.Stance = doctrineStance(p.Bot.ReachM, p.Bot.SpeedMS, spawnTopSpeed(reach))
		// What the guns land on these rats at that stance (application.go), before the side's sums.
		p.Bot.applyAgainstSpawn(spawn)
		reachHold := rangeAdvantage(p.Bot.ReachM, p.Bot.LockM, p.Bot.SpeedMS, p.Bot.Stance, reach, fightSec)
		p.LandedFrac = (1 - suppress) * (1 - reachHold)
		if p == self {
			pp.selfSuppress, pp.selfReachHold = suppress, reachHold
		}
	}
	for _, p := range pack {
		pp.sideDPS += p.Bot.DPS
		pp.sideEHP += p.Bot.EHP
		pp.standEHP += p.Bot.EHP / math.Max(0.01, p.LandedFrac)
		pp.armed = pp.armed || p.Bot.Armed
	}
	return pp, nil
}

func (m *Mod) resolveAbstractEncounter(ctx context.Context, b *botRun, spawn []*ratThreat) {
	pp, err := m.prepareAbstractPack(ctx, b, spawn)
	if err != nil {
		rlogf("rolebots: abstract combat %s: %v", b.Name, err)
		return
	}
	pack, self, bot := pp.pack, pp.self, pp.self.Bot
	sideDPS, sideEHP, standEHP, armed := pp.sideDPS, pp.sideEHP, pp.standEHP, pp.armed
	selfSuppress, selfReachHold := pp.selfSuppress, pp.selfReachHold
	ratDPS := 0.0
	for _, r := range spawn {
		ratDPS += r.DPS
	}

	ewarNote := ""
	if selfSuppress > 0 {
		ewarNote = fmt.Sprintf(", electronics holding off %.0f%%", selfSuppress*100)
	}
	if selfReachHold > 0 {
		if bot.Stance == stanceKite {
			ewarNote += fmt.Sprintf(", kiting them off %.0f%% of it", selfReachHold*100)
		} else {
			ewarNote += fmt.Sprintf(", outranging them for %.0f%% of it", selfReachHold*100)
		}
	}
	if len(pack) > 1 {
		ewarNote += fmt.Sprintf(", with %d gangmate(s) alongside", len(pack)-1)
	}
	m.logBot(b.CharID, logInfo, "combat",
		"jumped by %s at the belt — %.0f incoming DPS against %.0f EHP, answering with %.0f DPS%s",
		describeSpawn(spawn), ratDPS, sideEHP, sideDPS, ewarNote)
	// The belt search remembers being jumped here and steers later loads elsewhere
	// (hostilemem.go), but not for a patrol: it chose the fight, and marking it would pull more hunters in (combatSite).
	if !postureFor(b.State.Phase).patrol() {
		noteHostileEncounter(botScope(b), b.SystemID)
	}

	// Whose nerve the side fights on: the commander's when it is in the
	// fight, otherwise this pilot's (packDecider). Alone, always this pilot's.
	decider := b
	if len(pack) > 1 {
		if g, _, err := gangForChar(ctx, m.db, b.CharID); err == nil && g != nil {
			decider = packDecider(b, pack, g.LeaderCharID)
		}
	}
	markCommander(pack, decider) // its break-off is the side's (resolvePackDuel)

	// What this pilot came out to do decides whether it stands and fights (combatdoctrine.go).
	// Read once: the phase cannot change under a duel that resolves in one call.
	fights := postureFights(postureFor(b.State.Phase))

	// This side's nerve mirrors shouldBreakOff in the embodied tier, so the same bot fights the
	// same way watched or not.
	breakOff := decider.Tune.BreakOffFrac
	if !armed {
		breakOff = decider.Tune.UnarmedBreakOffFrac
	}
	// A pilot not out here for a fight leaves on the first hit, whatever it carries: a mining
	// frigate with a popgun is not a hunter (minerMargin asks whether a worker can leave).
	if !fights {
		breakOff = 1
	}

	leaving := false
	// The pre-fight look depends on what the pilot came out to do: a fighter asks whether it can
	// win, a worker whether it can leave.
	if fights {
		// A hunter's question: can this side kill what is standing there before it dies. A fight it
		// would not have signed up for is declined from tick 0 (aligning out); the spawn still shoots meanwhile.
		if margin := threatMargin(sideDPS, standEHP, spawnThreatOf(spawn)); !wouldEngage(decider.Tune, margin) {
			leaving = true
			// Filed under threat, not combat: this is the arithmetic refusing a fight, a line an
			// operator counts separately.
			m.logBot(b.CharID, logInfo, "threat",
				"this is not a fight worth having — aligning out from the start (%s)",
				marginVerdict(decider.Tune, margin))
			// And not coming back to it: the spawn is persistent, so the next look would give the same answer.
			if len(spawn) > 0 {
				m.noteHuntSkip(ctx, b, spawn[0].BeltID)
			}
		}
	} else if margin := minerMargin(sideDPS, standEHP, spawnThreatOf(spawn)); minerHopeless(margin) {
		// A worker's question is can it leave, not can it win (minerMargin, threat.go). Only the
		// hopeless case aligns from tick 0; otherwise breakOff takes it out on the first hit.
		leaving = true
		m.logBot(b.CharID, logInfo, "threat",
			"not out here for a fight and this one kills it — aligning out from the start (%s)",
			minerVerdict(margin))
	}

	out := resolvePackDuel(pack, spawn, breakOff, leaving, rand.Float64)
	if self.FleetCall {
		m.logBot(b.CharID, logInfo, "combat",
			"the commander calls the withdrawal — the fleet aligns out together")
	}
	if self.BrokeOffAt >= 0 {
		if fights {
			m.logBot(b.CharID, logInfo, "combat",
				"breaking off at %.0f%% — aligning out", self.BrokeOffAt*100)
		} else {
			// A different line for a different decision: this pilot is declining a fight, not losing
			// one, so a break-off count must not include every hauler that got shot at.
			m.logBot(b.CharID, logInfo, "combat",
				"took fire and it was not out here for a fight — aligning out at %.0f%%",
				self.BrokeOffAt*100)
		}
	}
	// One wave, fought once: the gangmates were in this fight and must not
	// each draw the same belt's survivors on their own next roll.
	stampPackExposure(pack, b.CharID)

	m.applyAbstractOutcome(ctx, b, pack, spawn, out)
}

// spawnStanding is how many rats are still up and what they collectively throw. Two numbers
// from one walk, kept apart because a pack that cannot hurt the pilot is still a pack: an
// unarmed rat is alive. Pure.
func spawnStanding(spawn []*ratThreat) (alive int, incoming float64) {
	for _, r := range spawn {
		if r != nil && r.EHP > 0 {
			alive++
			incoming += r.DPS
		}
	}
	return alive, incoming
}

// applyAbstractOutcome writes the fight back to the world: the hull damage that survived, or
// the loss of the ship, for every hull on the side. b is the pilot that drew the spawn; it
// speaks for the fight in the log and opens the wrecks. Gangmates' own sim passes own their state.
func (m *Mod) applyAbstractOutcome(ctx context.Context, b *botRun, pack []*packMember,
	spawn []*ratThreat, out packOutcome) {

	killed := out.Killed

	// Settle the belt first: whatever died here is dead for everyone, so a player warping in
	// finds the belt as the bot left it.
	m.settleAbstractSpawn(ctx, spawn)

	// The bounty on whatever died, before anything else (the core pays a dead player's last
	// kill too), split by what each hull put into the rats.
	if killed > 0 {
		m.payNPCBounties(ctx, packBountyShares(pack), spawn, b.SystemID)
	}

	self := m.settlePackHulls(ctx, b, pack, spawn)
	if self == nil || self.Bot.EHP <= 0 {
		return // the pilot that drew it is dead; its loss is already on the board
	}
	escaped := self.Escaped

	// The wrecks: nothing to open if this pilot ran or its task does not loot (task.go,
	// beltloot.go). A fleet member loots for itself and keeps or donates it (lootdonate.go).
	if killed > 0 && !escaped {
		m.lootAbstractKills(ctx, b, spawn)
	}

	switch {
	case escaped:
		m.logBot(b.CharID, logInfo, "combat",
			"disengaged and warped clear at %.0f%% hull integrity (%d rat(s) killed on the way out)",
			self.Bot.EHP/self.Bot.MaxEHP*100, killed)
	case killed >= len(spawn):
		// A cleared spawn buys the system a quiet spell for everyone (hostilemem.go); a distress
		// cry on the board is answered and its beacon comes down (beltdistress.go).
		notePatrolCover(botScope(b), b.SystemID)
		if m.settleBeltDistress(ctx, b.CorpID, b.SystemID) {
			m.logBot(b.CharID, logInfo, "combat",
				"distress answered — the belts here are open to miners again")
		}
		with := ""
		if len(pack) > 1 {
			with = fmt.Sprintf(" with %d gangmate(s)", len(pack)-1)
		}
		m.logBot(b.CharID, logInfo, "combat",
			"cleared the spawn%s — %d rat(s) killed, holding at %.0f%% hull integrity. "+
				"The belt is covered for a while.",
			with, killed, self.Bot.EHP/self.Bot.MaxEHP*100)
	default:
		// Partial credit: kills taken and ship intact lift the grudge although the belt is not clear
		// and no cover is granted; otherwise only a full wipe could clear a mark.
		if killed > 0 {
			noteHostilePartial(botScope(b), b.SystemID)
		}
		m.logBot(b.CharID, logInfo, "combat",
			"the engagement broke up — %d of %d rat(s) killed, %.0f%% hull integrity left",
			killed, len(spawn), self.Bot.EHP/self.Bot.MaxEHP*100)
	}
}

// settlePackHulls writes a fight back to every hull on the side: the loss of
// the ship, or the damage that survived it, and the drones it cost. Returns
// the drawer's own member (nil if it was not on the side). Shared by the belt
// encounter and the site walker (siterun.go).
func (m *Mod) settlePackHulls(ctx context.Context, b *botRun, pack []*packMember, spawn []*ratThreat) *packMember {
	var self *packMember
	for _, p := range pack {
		if p.Run == b {
			self = p
		}
		if p.Bot.EHP <= 0 {
			killer := describeSpawn(spawn)
			var x, y, z float64
			_ = m.db.QueryRowContext(ctx,
				`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
				p.Run.ShipID).Scan(&x, &y, &z)
			m.loseHull(ctx, p.Run.CharID, p.Run.Name, p.Run.ShipID, p.Run.SystemID, x, y, z, killer,
				abstractKillHits(spawn))
			continue
		}
		// Survived. Spread the damage back across armour and structure: shields recharge on their
		// own, so what persists is what a repairer would fix.
		lostFrac := 1 - p.Bot.EHP/p.Bot.MaxEHP
		armorDmg := p.Bot.MaxHP[1] * math.Max(0, (lostFrac-shieldShare(p.Bot))/math.Max(0.01, 1-shieldShare(p.Bot)))
		structDmg := 0.0
		if armorDmg > p.Bot.MaxHP[1] {
			structDmg = math.Min(p.Bot.MaxHP[2], armorDmg-p.Bot.MaxHP[1])
			armorDmg = p.Bot.MaxHP[1]
		}
		if err := evedb.SaveShipCombatDamage(ctx, m.db, p.Run.ShipID, structDmg, armorDmg); err != nil {
			rlogf("rolebots: abstract damage ship=%d: %v", p.Run.ShipID, err)
		}
		// And the drones the fight cost it (droneloss.go).
		m.abstractDroneAttrition(ctx, p.Run.CharID, p.Run.ShipID, p.Bot.Drones, lostFrac, p.Escaped)
	}
	return self
}

// settleAbstractSpawn writes the fight back to the belt: dead rats marked dead, survivors'
// damage persisted. Nothing here fails loudly: the duel and the pilot's outcome are decided,
// and a database error must not undo a paid kill.
func (m *Mod) settleAbstractSpawn(ctx context.Context, spawn []*ratThreat) {
	if m.spawner == nil {
		return
	}
	for _, r := range spawn {
		if r == nil || r.BeltRatID == 0 {
			continue
		}
		if r.EHP <= 0 {
			if err := m.spawner.KillBeltRat(ctx,
				evedb.BeltRatRow{BeltRatID: r.BeltRatID, Role: r.Role}, r.RegionID); err != nil {
				rlogf("rolebots: settle belt rat %d: %v", r.BeltRatID, err)
			}
			continue
		}
		if r.MaxEHP <= 0 || r.EHP >= r.MaxEHP {
			continue // untouched
		}
		s, a, st := layerFracs(r.HP, r.EHP/r.MaxEHP)
		if err := evedb.SaveBeltRatHealth(ctx, m.db, r.BeltRatID, s, a, st); err != nil {
			rlogf("rolebots: save belt rat health %d: %v", r.BeltRatID, err)
		}
	}
}

// layerFracs spends a loss across shield, armour and structure in that order and reports
// what is left of each. An even spread would make a rat at 60% materialise 40% into its hull.
func layerFracs(hp [3]float64, remaining float64) (shield, armor, structure float64) {
	total := hp[0] + hp[1] + hp[2]
	if total <= 0 {
		// No layer split known: report the plain remainder rather than inventing one.
		return remaining, remaining, remaining
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		remaining = 1
	}
	lost := (1 - remaining) * total
	out := [3]float64{1, 1, 1}
	for i := 0; i < 3; i++ {
		if hp[i] <= 0 {
			out[i] = 1
			continue
		}
		if lost <= 0 {
			break
		}
		eaten := lost
		if eaten > hp[i] {
			eaten = hp[i]
		}
		out[i] = (hp[i] - eaten) / hp[i]
		lost -= eaten
	}
	return out[0], out[1], out[2]
}

// shieldShare is how much of the ship's total tank the shield accounts for, to work out how
// much of the damage taken landed past it.
func shieldShare(c *abstractCombatant) float64 {
	total := c.MaxHP[0] + c.MaxHP[1] + c.MaxHP[2]
	if total <= 0 {
		return 0
	}
	return c.MaxHP[0] / total
}

// abstractKillHits turns a resolved spawn into the attacker snapshot the killboard stores, in
// the shape the core's damage paths build, one row per rat. Unlike a live report the damage
// is effective hitpoints (the duel has no damage types) and the final blow is the hardest
// hitter alive on the killing tick. Rats that never fired are left out.
func abstractKillHits(spawn []*ratThreat) []destiny.KillHit {
	var final *ratThreat
	for _, r := range spawn {
		if r.EHP > 0 && (final == nil || r.DPS > final.DPS) {
			final = r
		}
	}
	// Everything died and the bot still lost: credit the heaviest hitter of the pack so the final
	// blow is not dropped.
	if final == nil {
		for _, r := range spawn {
			if r.Dealt > 0 && (final == nil || r.Dealt > final.Dealt) {
				final = r
			}
		}
	}

	now := clock.Unix()
	hits := make([]destiny.KillHit, 0, len(spawn))
	for _, r := range spawn {
		if r.Dealt <= 0 {
			continue
		}
		hits = append(hits, destiny.KillHit{
			Name:         r.Name,
			CorpID:       r.CorpID,
			ShipTypeID:   r.TypeID,
			WeaponTypeID: r.WeaponTypeID,
			Damage:       r.Dealt,
			IsNPC:        true,
			FinalBlow:    r == final,
			Time:         now,
		})
	}
	return hits
}

// nextRatTarget is what a pilot shoots next: the armed rat with the most incoming damage per
// hitpoint left, and once every gun is silent, the weakest of the rest. It is the kill order
// seqDamageTaken prices, so the margin and the duel agree on what dies first.
func nextRatTarget(spawn []*ratThreat) *ratThreat {
	var armed, unarmed *ratThreat
	for _, r := range spawn {
		if r == nil || r.EHP <= 0 {
			continue
		}
		if r.DPS > 0 {
			if armed == nil || r.DPS/r.EHP > armed.DPS/armed.EHP {
				armed = r
			}
			continue
		}
		if unarmed == nil || r.EHP < unarmed.EHP {
			unarmed = r
		}
	}
	if armed != nil {
		return armed
	}
	return unarmed
}

// describeSpawn names a spawn the way a killmail would.
func describeSpawn(spawn []*ratThreat) string {
	counts := map[string]int{}
	for _, r := range spawn {
		name := r.Name
		if name == "" {
			name = fmt.Sprintf("type %d", r.TypeID)
		}
		counts[name]++
	}
	out := ""
	for name, n := range counts {
		if out != "" {
			out += ", "
		}
		out += fmt.Sprintf("%d× %s", n, name)
	}
	return out
}

// repCapShare is the share of a hull's peak capacitor regeneration its
// repairers may count on. Not all of it: the guns, the engine and the EWAR
// rack drink from the same pool.
const repCapShare = 0.6

// loadoutRepRates is a loadout's remote and self repair in raw HP a second,
// each scaled by repSustain. Energy transfer moves capacitor, not hitpoints,
// and is left out of both.
func loadoutRepRates(l *combatLoadout) (remote, self float64) {
	if l == nil || len(l.reps) == 0 {
		return 0, 0
	}
	var capPerSec float64
	for _, r := range l.reps {
		if r.CycleSec <= 0 || r.Kind == repCap {
			continue
		}
		hps := r.Amount / r.CycleSec
		if r.Remote {
			remote += hps
		} else {
			self += hps
		}
		capPerSec += r.CapNeed / r.CycleSec
	}
	f := repSustain(l.capMax, l.capTauSec, capPerSec)
	return remote * f, self * f
}

// repSustain is the fraction of a repair rack's duty cycle the capacitor can
// keep up: peak regeneration (2.5 × capacity ÷ recharge time, the standard
// capacitor curve's peak) times repCapShare, against what the rack draws.
// Pure.
func repSustain(capMax, tauSec, drawPerSec float64) float64 {
	if drawPerSec <= 0 {
		return 1
	}
	if capMax <= 0 || tauSec <= 0 {
		return 0
	}
	f := 2.5 * capMax / tauSec * repCapShare / drawPerSec
	if f > 1 {
		return 1
	}
	return f
}

func init() {
	registerCharForget("exposureAt", func(id int64) {
		exposureMu.Lock()
		delete(exposureAt, id)
		exposureMu.Unlock()
	})
	registerCharForget("friendlyBeltSaid", func(id int64) {
		friendlyBeltMu.Lock()
		delete(friendlyBeltSaid, id)
		friendlyBeltMu.Unlock()
	})
}
