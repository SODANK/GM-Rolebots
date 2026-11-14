package rolebots

import (
	"context"
	"fmt"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/esp"
)

// Turning an order into work.
//
// This file is the abstract tier: where the pilot goes, what it buys, what phase it is in and
// who owns the ore. What it does once on grid beside its commander (station-keeping, target
// selection, when to shoot) belongs to conscript_embodied.go, which runs on the wall clock in
// front of a player. A conscript is the same bot with somebody else choosing: "mine" is the
// module's mining trip with the belt pinned, "dock" its own docking, the refit ensureActivityShip.

const (
	// conscriptOrderTTLSec is how long an unacted order stays worth acting on: ten minutes, after
	// which the fight is over and obeying would be worse than forgetting.
	conscriptOrderTTLSec = 600
	// conscriptHoldSec is how long a "stay" leg lasts before the pilot has another turn: long
	// enough not to spin, short enough to pick up a release or new order.
	conscriptHoldSec = 60
	// conscriptRegroupSec paces a pilot that has arrived and has nothing specific to do.
	conscriptRegroupSec = 15
)

// ── pre-emption ─────────────────────────────────────────────────────────────

// conscriptCallUp is the break-off, called from advance() beside the fleet muster's own. It
// reports whether it handled this pass. It is not in plan(), which is reached from the idle
// phase alone, so a pilot on a long errand would not see the order for twenty minutes.
func (m *Mod) conscriptCallUp(ctx context.Context, b *botRun) bool {
	if b.BossID == 0 {
		return false // free: one column already on the roster row
	}
	o := peekOrder(b.CharID)
	if o == nil {
		return false
	}
	if clock.Unix()-o.At > conscriptOrderTTLSec {
		dropOrder(b.CharID)
		return false
	}
	st := b.State

	// Already serving: a servant obeys the newest order, and nothing is stranded that the disposal
	// orders do not cover.
	if isConscriptPhase(st.Phase) {
		return m.takeConscriptOrder(ctx, b)
	}

	// Idle is plan()'s doorway, where conscription routes. Answering handled here would consume
	// the pass that was about to act on the order (the livelock the muster call guards against).
	if st.Phase == "" || TaskKind(st.Phase) == TaskIdle {
		return false
	}

	if interruptibleState(st) {
		return m.takeConscriptOrder(ctx, b)
	}

	// Mid-task and dropping it would strand the pilot. Say so once (the deferral stamp stops a
	// repeat every five seconds) and leave the order queued for the end of the errand.
	if st.Data.OrderDeferredAt != o.At {
		st.Data.OrderDeferredAt = o.At
		if err := saveState(ctx, m.db, st); err != nil {
			rlogf("rolebots: conscript defer stamp: %v", err)
		}
		label := phaseLabel(st)
		m.sayFleet(ctx, o.GangID, b.CharID, true, "Finishing up — %s. Right behind you.",
			strings.ToLower(label))
	}
	return false
}

// takeConscriptOrder consumes the pending order and puts the pilot in the phase that carries
// it out.
func (m *Mod) takeConscriptOrder(ctx context.Context, b *botRun) bool {
	o := takeOrder(b.CharID)
	if o == nil {
		return false
	}
	st := b.State
	st.Data = idleData(st.Data)
	st.Data.OrderVerb = o.Verb
	st.Data.OrderAt = o.At
	st.Data.OrderBossID = o.BossID
	st.Data.OrderDeferredAt = 0
	st.Data.OrderTargetChar = o.Subject
	st.Data.LegEndsAt = 0

	m.applyOrderArgs(ctx, b, o)

	// Hold-fire is a standing flag, not a phase, so it survives a following order: "hold fire"
	// then "follow me" must not re-arm the guns.
	switch o.Verb {
	case OrderHoldFire:
		st.Data.OrderHoldFire = true
	case OrderAttack, OrderTackle, OrderDefend:
		st.Data.OrderHoldFire = false
	}

	st.Activity = ActConscript
	st.Phase = string(TaskConscriptForm)
	if err := saveState(ctx, m.db, st); err != nil {
		rlogf("rolebots: conscript order save: %v", err)
		return false
	}
	m.logBot(b.CharID, logInfo, "conscript", "order %q from %d", o.Verb, o.BossID)
	return true
}

// applyOrderArgs resolves the words after the verb into the numbers the phase steps read. It
// is best-effort: an ore nobody can name is a mining trip with no preference.
func (m *Mod) applyOrderArgs(ctx context.Context, b *botRun, o *pendingOrder) {
	st := b.State
	boss := m.bossSession(o.BossID)

	// The work is pinned to where the commander is, not where the pilot stands.
	if boss != nil && boss.SolarSystemID != 0 {
		st.Data.OrderSystemID = boss.SolarSystemID
	} else {
		st.Data.OrderSystemID = b.SystemID
	}

	switch o.Verb {
	case OrderStockpile, OrderDock:
		// "stockpile here" means the station the commander is standing in.
		if boss != nil && boss.StationID != 0 {
			st.Data.OrderStationID = boss.StationID
		} else if b.StationID != 0 {
			st.Data.OrderStationID = b.StationID
		}
	case OrderMine, OrderPickOre:
		if id, name := m.oreNamed(ctx, o.Rest); id != 0 {
			st.Data.OrderOreTypeID = id
			st.Data.OreTypeID = id
			st.Data.OreName = name
		}
	}
}

// bossSession is where the commanding player is now. It is nil when they are not connected,
// which the release sweep notices. It uses FindByCharID, not the presence scan, which keeps
// synthetic pilots out: a bot cannot command a fleet.
func (m *Mod) bossSession(bossCharID int64) *esp.Session {
	if m.sessions == nil || bossCharID == 0 {
		return nil
	}
	return m.sessions.FindByCharID(bossCharID)
}

// oreNamed resolves "veldspar" or "dense veldspar" to a type. It is restricted to category 25
// (Asteroid) so a mineral name cannot become a mining order.
func (m *Mod) oreNamed(ctx context.Context, words string) (int64, string) {
	words = strings.TrimSpace(words)
	if words == "" {
		return 0, ""
	}
	var typeID int64
	var name string
	err := m.db.QueryRowContext(ctx, `
		SELECT t.typeID, t.typeName
		  FROM invTypes t
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE g.categoryID = 25 AND t.published = 1
		   AND LOWER(t.typeName) LIKE ?
		 ORDER BY LENGTH(t.typeName)
		 LIMIT 1`, "%"+strings.ToLower(words)+"%").Scan(&typeID, &name)
	if err != nil {
		return 0, ""
	}
	return typeID, name
}

// ── the planner gate ────────────────────────────────────────────────────────

// conscriptPlan is the branch plan() takes for a pilot in somebody's fleet. It reports whether
// it claimed the pass. A conscript with no standing order lands here too: its default is to be
// with the fleet, and planSolo would send it off to mine elsewhere.
func (m *Mod) conscriptPlan(ctx context.Context, b *botRun) (bool, error) {
	if b.BossID == 0 {
		return false, nil
	}
	k := conscriptOf(b.CharID)
	if k == nil {
		return false, nil
	}
	st := b.State
	if st.Data.OrderVerb == "" {
		st.Data.OrderVerb = OrderFollow
		st.Data.OrderBossID = k.BossCharID
		st.Data.OrderAt = clock.Unix()
	}
	st.Activity = ActConscript
	st.Phase = string(TaskConscriptForm)
	return true, saveState(ctx, m.db, st)
}

// ── the steps ───────────────────────────────────────────────────────────────

// conscriptStep dispatches all five conscript phases. They are stages of one errand, and the
// verb, not the phase, decides what happens next.
func (m *Mod) conscriptStep(ctx context.Context, b *botRun) (bool, error) {
	k := conscriptOf(b.CharID)
	if k == nil {
		// Released while mid-order: nothing to serve, so go back to being a bot.
		return false, m.endConscriptPhase(ctx, b)
	}
	switch TaskKind(b.State.Phase) {
	case TaskConscriptHold:
		return m.conscriptHoldStep(ctx, b, k)
	case TaskConscriptDeliver:
		return m.conscriptDeliverStep(ctx, b, k)
	default:
		return m.conscriptFormStep(ctx, b, k)
	}
}

// clearStandingOrder wipes the order block. It must be explicit: idleData carries the order,
// because a conscript's mining trip re-plans mid-trip. Every path that ends the service, not
// just the trip, must call it, or a released pilot acts on a stranger's instruction.
func clearStandingOrder(st *SimState) {
	if st == nil {
		return
	}
	st.Data.OrderVerb = ""
	st.Data.OrderAt = 0
	st.Data.OrderDeferredAt = 0
	st.Data.OrderBossID = 0
	st.Data.OrderTargetChar = 0
	st.Data.OrderSystemID = 0
	st.Data.OrderStationID = 0
	st.Data.OrderOreTypeID = 0
	st.Data.OrderHoldFire = false
}

// endConscriptPhase drops a pilot out of the conscript family back to idle.
func (m *Mod) endConscriptPhase(ctx context.Context, b *botRun) error {
	st := b.State
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	clearStandingOrder(st)
	return saveState(ctx, m.db, st)
}

// conscriptFormStep is where an order becomes movement. It runs once per order, then hands the
// pilot to the module's existing planner with the commander's destination.
func (m *Mod) conscriptFormStep(ctx context.Context, b *botRun, k *Conscript) (bool, error) {
	st := b.State
	verb := st.Data.OrderVerb
	gangID := k.GangID

	switch verb {
	case OrderStay:
		st.Phase = string(TaskConscriptHold)
		st.Data.LegEndsAt = clock.Unix() + conscriptHoldSec
		return false, saveState(ctx, m.db, st)

	case OrderResume:
		// Back to the pilot's own rotation but still in the fleet: it was asked to stop, not leave.
		st.Data.OrderVerb = OrderFollow
		return false, saveState(ctx, m.db, st)

	case OrderGoHome:
		m.release(ctx, b.CharID, "sent home")
		return false, nil

	case OrderDock:
		return m.conscriptDock(ctx, b, k)

	case OrderUndock:
		if b.StationID == 0 {
			return m.conscriptEscortReady(ctx, b, k)
		}
		if err := m.beginTravel(ctx, b, b.SystemID, 0, string(TaskConscriptEscort)); err != nil {
			return false, err
		}
		return true, nil

	case OrderMine, OrderPickOre, OrderJetcan:
		return m.conscriptMine(ctx, b, k)

	case OrderStockpile, OrderSell:
		return m.conscriptDeliverPlan(ctx, b, k)

	case OrderHaul:
		if err := m.setConscriptRole(ctx, b.CharID, RoleHauler); err != nil {
			rlogf("rolebots: conscript haul role: %v", err)
		}
		return m.conscriptDeliverPlan(ctx, b, k)

	case OrderLogi:
		// The logi seat: a refit to a remote-repair fit, then the escort, where support_embodied.go
		// mends the commander and the fleet.
		if err := m.setConscriptRole(ctx, b.CharID, RoleLogi); err != nil {
			rlogf("rolebots: conscript logi role: %v", err)
		}
		ready, err := m.ensureActivityShip(ctx, b, ActSeatLogi)
		if err != nil {
			m.sayFleet(ctx, gangID, b.CharID, true, "Can't get a logistics fit together — %v", err)
			return false, m.conscriptIdleWith(ctx, b)
		}
		if !ready {
			if st.Data.OrderDeferredAt == 0 {
				st.Data.OrderDeferredAt = clock.Unix()
				m.sayFleet(ctx, gangID, b.CharID, true, "Need to pick up a logistics fit first — won't be long.")
				if err := saveState(ctx, m.db, st); err != nil {
					return false, err
				}
			}
			return true, nil
		}
	}

	// Everything else (attack, defend, tackle, hold fire, flee, follow, align) is about being on
	// grid with the commander. Get there; the embodied tier does the rest.
	return m.conscriptEscortReady(ctx, b, k)
}

// conscriptEscortReady puts the pilot in the commander's system and then in the escort phase,
// where the projection takes over.
func (m *Mod) conscriptEscortReady(ctx context.Context, b *botRun, k *Conscript) (bool, error) {
	st := b.State
	want := st.Data.OrderSystemID
	if boss := m.bossSession(k.BossCharID); boss != nil && boss.SolarSystemID != 0 {
		// Re-read every pass: a commander that jumped mid-travel has moved the destination.
		want = boss.SolarSystemID
		st.Data.OrderSystemID = want
	}
	if want == 0 {
		return false, m.endConscriptPhase(ctx, b)
	}
	if b.SystemID != want || b.StationID != 0 {
		if err := m.beginTravel(ctx, b, want, 0, string(TaskConscriptEscort)); err != nil {
			return false, err
		}
		return true, nil
	}
	st.Phase = string(TaskConscriptEscort)
	st.Data.LegEndsAt = clock.Unix() + conscriptRegroupSec
	return false, saveState(ctx, m.db, st)
}

// conscriptDock sends the pilot inside: the commander's station when they are in one, else
// wherever it can get to in this system.
func (m *Mod) conscriptDock(ctx context.Context, b *botRun, k *Conscript) (bool, error) {
	st := b.State
	station := st.Data.OrderStationID
	sysID := st.Data.OrderSystemID
	if boss := m.bossSession(k.BossCharID); boss != nil {
		if boss.StationID != 0 {
			station = boss.StationID
		}
		if boss.SolarSystemID != 0 {
			sysID = boss.SolarSystemID
		}
	}
	if station == 0 {
		var err error
		station, err = m.anyStationIn(ctx, sysID)
		if err != nil || station == 0 {
			m.sayFleet(ctx, k.GangID, b.CharID, true, "Nowhere to dock here.")
			return false, m.conscriptIdleWith(ctx, b)
		}
	}
	if b.StationID == station {
		return false, m.conscriptIdleWith(ctx, b)
	}
	if err := m.beginTravel(ctx, b, sysID, station, string(TaskConscriptHold)); err != nil {
		return false, err
	}
	return true, nil
}

// conscriptMine is the fleet's mining order: fit for it if not already, then run the module's
// mining trip with the belt pinned to the commander's system and the disposal pointed at the
// commander.
func (m *Mod) conscriptMine(ctx context.Context, b *botRun, k *Conscript) (bool, error) {
	st := b.State

	// The refit: ensureActivityShip buys, boards and outfits, and returns false while it has taken
	// the pilot somewhere to do it.
	ready, err := m.ensureActivityShip(ctx, b, ActMining)
	if err != nil {
		m.sayFleet(ctx, k.GangID, b.CharID, true, "Can't get a mining fit together — %v", err)
		return false, m.conscriptIdleWith(ctx, b)
	}
	if !ready {
		if st.Data.OrderDeferredAt == 0 {
			st.Data.OrderDeferredAt = clock.Unix()
			m.sayFleet(ctx, k.GangID, b.CharID, true, "Need to pick up a mining fit first — won't be long.")
			if err := saveState(ctx, m.db, st); err != nil {
				return false, err
			}
		}
		return true, nil
	}

	// Where the rock is: the commander's system, not the pilot's own pick.
	want := st.Data.OrderSystemID
	if boss := m.bossSession(k.BossCharID); boss != nil && boss.SolarSystemID != 0 {
		want = boss.SolarSystemID
	}
	if want == 0 {
		want = b.SystemID
	}

	// Disposal follows the standing duty station and the ore is the commander's either way
	// (conscriptDeliverStep). Set here so mining does not resolve its own from the wallet.
	st.Data.MineDisposal = "stockpile"
	st.Data.MineRefineStationID = 0
	st.Activity = ActMining
	st.Data.OrderSystemID = want
	if st.Data.OrderVerb == OrderJetcan {
		st.Data.MineDisposal = "jetcan"
	}

	if b.SystemID != want || b.StationID != 0 {
		if err := m.beginTravel(ctx, b, want, 0, string(TaskMineWork)); err != nil {
			return false, err
		}
		return true, nil
	}
	st.Phase = string(TaskMineWork)
	st.Data.MineBeltID = m.parkAtBelt(ctx, b.ShipID, want, destiny.BaseOreType(st.Data.OreTypeID))
	return false, saveState(ctx, m.db, st)
}

// conscriptDeliverPlan routes to wherever the fleet's property is meant to go.
func (m *Mod) conscriptDeliverPlan(ctx context.Context, b *botRun, k *Conscript) (bool, error) {
	st := b.State
	station := st.Data.OrderStationID
	if station == 0 {
		station = k.DutyStationID
	}
	if station == 0 {
		if boss := m.bossSession(k.BossCharID); boss != nil && boss.StationID != 0 {
			station = boss.StationID
		}
	}
	if station == 0 {
		m.sayFleet(ctx, k.GangID, b.CharID, true,
			"Where do you want it? Dock up and say %q.", "stockpile here")
		return false, m.conscriptIdleWith(ctx, b)
	}
	// Remember it: "stockpile here" is said once and holds afterwards.
	if k.DutyStationID != station {
		if _, err := m.db.ExecContext(ctx,
			`UPDATE rolebot_conscripts SET dutyStationID = ? WHERE characterID = ?`,
			station, b.CharID); err != nil {
			rlogf("rolebots: conscript duty station: %v", err)
		}
		m.refreshConscripts(ctx)
	}
	st.Data.OrderStationID = station
	if b.StationID == station {
		st.Phase = string(TaskConscriptDeliver)
		return false, saveState(ctx, m.db, st)
	}
	sysID, err := m.stationSystem(ctx, station)
	if err != nil || sysID == 0 {
		return false, m.conscriptIdleWith(ctx, b)
	}
	if err := m.beginTravel(ctx, b, sysID, station, string(TaskConscriptDeliver)); err != nil {
		return false, err
	}
	return true, nil
}

// conscriptDeliverStep hands the load over: the ore goes into the commander's hangar, and if
// they asked for it sold, the ISK follows.
func (m *Mod) conscriptDeliverStep(ctx context.Context, b *botRun, k *Conscript) (bool, error) {
	st := b.State
	if b.StationID == 0 {
		return m.conscriptDeliverPlan(ctx, b, k)
	}
	// Every grade the load is made of: a belt of Veldspar pays out three invTypes rows
	// (beltfield.go), so handing over st.Data.OreTypeID alone would leave most of it in the hold.
	types := oreLotTypes(st.Data)
	if len(types) == 0 && st.Data.OrderOreTypeID != 0 {
		types = []int64{st.Data.OrderOreTypeID}
	}

	if st.Data.OrderVerb == OrderSell {
		before, _ := m.walletOf(ctx, b.CharID)
		for _, typeID := range types {
			if err := m.gangSellFromHangar(ctx, b, typeID); err != nil {
				m.sayFleet(ctx, k.GangID, b.CharID, true, "Couldn't shift it here — %v", err)
				return false, m.conscriptIdleWith(ctx, b)
			}
		}
		after, _ := m.walletOf(ctx, b.CharID)
		if gain := after - before; gain > 0 {
			if err := m.remitToBoss(ctx, b.CharID, k.BossCharID, gain); err != nil {
				rlogf("rolebots: remit to %d: %v", k.BossCharID, err)
			} else {
				m.sayFleet(ctx, k.GangID, b.CharID, true, "Sold. %s sent over.", iskWords(gain))
			}
		}
		return false, m.conscriptIdleWith(ctx, b)
	}

	// moveCargoToLeaderHangar takes a plain character ID and works for a human commander.
	for _, typeID := range types {
		if err := m.moveCargoToLeaderHangar(ctx, b, k.BossCharID, typeID); err != nil {
			m.sayFleet(ctx, k.GangID, b.CharID, true, "Couldn't hand it over — %v", err)
			return false, m.conscriptIdleWith(ctx, b)
		}
	}
	// The client caches an open hangar and nothing pushes an item change for a move made outside
	// its connection, so the pilot says where it went.
	m.sayFleet(ctx, k.GangID, b.CharID, true,
		"In your hangar here. You may need to reopen it to see it.")
	return false, m.conscriptIdleWith(ctx, b)
}

// conscriptHoldStep is "stay". Nothing happens: the leg expires and is renewed until the
// pilot is told something else.
func (m *Mod) conscriptHoldStep(ctx context.Context, b *botRun, _ *Conscript) (bool, error) {
	b.State.Data.LegEndsAt = clock.Unix() + conscriptHoldSec
	return false, saveState(ctx, m.db, b.State)
}

// conscriptIdleWith parks the pilot back on its default duty: being with the fleet, waiting
// to be told something.
func (m *Mod) conscriptIdleWith(ctx context.Context, b *botRun) error {
	st := b.State
	st.Activity = ActConscript
	st.Phase = string(TaskConscriptEscort)
	st.Data.OrderVerb = OrderFollow
	st.Data.OrderDeferredAt = 0
	st.Data.LegEndsAt = clock.Unix() + conscriptRegroupSec
	return saveState(ctx, m.db, st)
}

// ── the spoils ──────────────────────────────────────────────────────────────

// remitToBoss moves ISK from the pilot to the player it serves. The bot sells into its own
// wallet like every sell path, and the proceeds are forwarded in one transaction with both
// journals, refType 10 (Player Donation, already in market_reftypes so the client renders it).
func (m *Mod) remitToBoss(ctx context.Context, fromCharID, toCharID int64, amount float64) error {
	if amount <= 0 || fromCharID == 0 || toCharID == 0 {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var bal float64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`, fromCharID).Scan(&bal); err != nil {
		return err
	}
	if bal < amount {
		// Something else spent it between the sale and here: send what there is, not nothing.
		amount = bal
	}
	if amount <= 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = balance - ? WHERE characterID = ?`, amount, fromCharID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = balance + ? WHERE characterID = ?`, amount, toCharID); err != nil {
		return err
	}

	name, _ := m.charNameCorp(ctx, fromCharID)
	if name == "" {
		name = "fleet"
	}
	// Clamped: market_journal.reason is VARCHAR(43) and strict mode fails the transaction on
	// overflow.
	reason := evedb.JournalReason("Fleet proceeds from " + name)
	// transDate is a Win32 FILETIME, not unix seconds; a unix value renders as the year 1601.
	now := evedb.UnixToWin32(clock.Unix())

	const journalSQL = `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, 10, ?, ?, NULL, 0, ?, ?, ?, 1000)`
	for _, side := range []struct {
		owner int64
		delta float64
	}{{fromCharID, -amount}, {toCharID, amount}} {
		var after float64
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM character_ WHERE characterID = ?`, side.owner).Scan(&after); err != nil {
			return err
		}
		// ownerID1 and ownerID2 are payer then payee on every row: the client's "X donated to Y"
		// rendering of refType 10 reads that order.
		if _, err := tx.ExecContext(ctx, journalSQL,
			side.owner, now, fromCharID, toCharID, side.delta, after, reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (m *Mod) walletOf(ctx context.Context, charID int64) (float64, error) {
	var bal float64
	err := m.db.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&bal)
	return bal, err
}

// ── the law ─────────────────────────────────────────────────────────────────

// mayShootPilot decides whether a conscript may open fire on a player. A clean pilot in
// high-sec would bring CONCORD and flag the fleet, so the answer is no. Shootable means already
// criminal, outside empire law, or a declared war. Pure, for table tests.
func mayShootPilot(security float64, criminalSince int64, warTarget bool) bool {
	if warTarget || criminalSince != 0 {
		return true
	}
	return security <= 0.0
}

// ── small lookups ───────────────────────────────────────────────────────────

func (m *Mod) setConscriptRole(ctx context.Context, charID int64, role string) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_conscripts SET role = ? WHERE characterID = ?`, role, charID)
	if err == nil {
		m.refreshConscripts(ctx)
	}
	return err
}

func (m *Mod) anyStationIn(ctx context.Context, sysID int64) (int64, error) {
	var id int64
	err := m.db.QueryRowContext(ctx,
		`SELECT stationID FROM staStations WHERE solarSystemID = ? LIMIT 1`, sysID).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (m *Mod) stationSystem(ctx context.Context, stationID int64) (int64, error) {
	r, ok := m.stationStaticRec(ctx, stationID)
	if !ok {
		return 0, fmt.Errorf("station %d: not found", stationID)
	}
	return r.SystemID, nil
}

// detailConscript is the panel's one-line account of what a conscript is doing, mostly who it
// is doing it for.
func detailConscript(m *Mod, ctx context.Context, st *SimState) string {
	if st == nil {
		return ""
	}
	verb := st.Data.OrderVerb
	if verb == "" {
		verb = "standing by"
	}
	if st.Data.OrderBossID == 0 {
		return verb
	}
	name, _ := m.charNameCorp(ctx, st.Data.OrderBossID)
	if name == "" {
		return verb
	}
	return fmt.Sprintf("%s, for %s", verb, name)
}
