package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"

	"gurimoon/corp"
)

// The starbase seats: lower management, bot or human, appointed by the roles power beside the
// Industry Director and its Deputy (corpdirector.go).
//   - Fueler ("Fuel Technician"), one per fuelerPerTowers towers: keeps every tower at
//     FuelWeekReserve fuel-day batches in its bay and one more at the staging office's Fuel
//     division, plus strontium for the charter's StrontiumHours; posts the requisitions and
//     fuel legs and has first claim on every fuel leg (poshaul.go). Trust leans to the patient.
//   - Procurer ("Procurement Officer"): runs the requisitions (posreq.go), patient lines as
//     corporation bids and urgent ones as funded buy trips. Trust leans to the envious.
// A seat held by a human keeps its title and roles; the bot CEO flies the errands in the
// human's name (the staff pattern's executor), and the human is paid per fuel run they carry.

const (
	seatFueler   = "fueler"
	seatProcurer = "procurer"

	fuelerPerTowers = 4
	seatKeepSec     = 14 * 86400
	seatOutscore    = 1.3

	posSeatRoles = corp.RoleHangarCanQuery6 | corp.RoleHangarCanQuery7 |
		corp.RoleHangarCanTake6 | corp.RoleHangarCanTake7
	fuelerRoles   = posSeatRoles | corp.RoleStarbaseConfig
	procurerRoles = posSeatRoles | corp.RoleTrader
	posRoleMask   = fuelerRoles | procurerRoles
)

var posSeatTitles = map[string]string{seatFueler: "Fuel Technician", seatProcurer: "Procurement Officer"}

// posSeatOf is the starbase seat a member holds ("" for none).
func (m *Mod) posSeatOf(ctx context.Context, corpID, charID int64) string {
	return postIn(charID, corpID, famStarbase)
}

// posSeatsOf is every seat holder of a corporation.
func (m *Mod) posSeatsOf(ctx context.Context, corpID int64) map[int64]string {
	out := map[int64]string{}
	for id, p := range m.familyPostsOf(ctx, corpID, famStarbase) {
		out[id] = p.Post
	}
	return out
}

// seatHolders lists a seat's holders, oldest first.
func (m *Mod) seatHolders(ctx context.Context, corpID int64, seat string) []int64 {
	return m.postHolders(ctx, corpID, seat)
}

// seatCand is one member as the seat deal scores it.
type seatCand struct {
	CharID int64
	Human  bool
	Score  float64
	Since  int64 // in the seat, unix (0 = not holding it)
}

// fuelerScore leans to the patient supplier. Polarity: LOW patience is the
// unhurried pilot. Pure.
func fuelerScore(p pilotIdentity, days float64) float64 {
	v := (1 + 0.6*p.trait(TraitSupply) - 0.5*p.trait(TraitPatience)) * math.Sqrt(1+days)
	return math.Max(0.01, v)
}

// procurerScore leans to the envious (HIGH greed) and the patient. Pure.
func procurerScore(p pilotIdentity, days float64) float64 {
	v := (1 + 0.5*p.trait(TraitGreed) - 0.4*p.trait(TraitPatience)) * math.Sqrt(1+days)
	return math.Max(0.01, v)
}

// dealSeat keeps incumbents unless they are gone or, after seatKeepSec, a
// free candidate outscores them by seatOutscore; then fills vacancies best
// first. Pure.
func dealSeat(cands []seatCand, n int, now int64, exclude map[int64]bool) []int64 {
	sorted := append([]seatCand(nil), cands...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Score != sorted[j].Score {
			return sorted[i].Score > sorted[j].Score
		}
		return sorted[i].CharID < sorted[j].CharID
	})
	var out []int64
	taken := map[int64]bool{}
	bestFree := 0.0
	for _, c := range sorted {
		if c.Since == 0 && !exclude[c.CharID] {
			bestFree = math.Max(bestFree, c.Score)
		}
	}
	for _, c := range sorted {
		if len(out) >= n || c.Since == 0 || exclude[c.CharID] {
			continue
		}
		if now-c.Since >= seatKeepSec && bestFree > c.Score*seatOutscore {
			continue
		}
		out = append(out, c.CharID)
		taken[c.CharID] = true
	}
	for _, c := range sorted {
		if len(out) >= n {
			break
		}
		if taken[c.CharID] || exclude[c.CharID] {
			continue
		}
		out = append(out, c.CharID)
		taken[c.CharID] = true
	}
	return out
}

// ceoStarbaseStaff deals the Fueler and Procurer seats from the roles power,
// after the industry staff. A corporation with no tower plan holds neither.
func (m *Mod) ceoStarbaseStaff(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts) error {
	if !StarbasePower() {
		return nil
	}
	plans, err := m.corpTowerPlans(ctx, c.CorpID, true)
	if err != nil {
		return err
	}
	var reqOpen int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_requisition WHERE corporationID = ? AND status IN ('open','bidding','buying')`,
		c.CorpID).Scan(&reqOpen)
	nFuelers, nProcurers := 0, 0
	if len(plans) > 0 {
		nFuelers = (len(plans) + fuelerPerTowers - 1) / fuelerPerTowers
	}
	if len(plans) > 0 || reqOpen > 0 {
		nProcurers = 1
	}
	all, err := m.corpMembers(ctx, c.CorpID, c.CharID)
	if err != nil {
		return err
	}
	cur := m.familyPostsOf(ctx, c.CorpID, famStarbase)
	seats := map[int64]string{}
	sinceOf := map[int64]int64{}
	for id, p := range cur {
		seats[id], sinceOf[id] = p.Post, p.Since
	}
	pol, _ := m.loadCorpPolicy(ctx, c.CorpID)
	// The industry staff keeps its own jobs.
	exclude := map[int64]bool{pol.DirectorID: true, pol.DeputyID: true}
	now := win32Now()
	var fc, pc []seatCand
	for _, cm := range all {
		if !cm.Eligible() {
			continue
		}
		id := m.memberIdentity(ctx, cm)
		days := tenureDays(cm.Since, now)
		f := seatCand{CharID: cm.CharID, Human: cm.Human(), Score: fuelerScore(id, days)}
		p := seatCand{CharID: cm.CharID, Human: cm.Human(), Score: procurerScore(id, days)}
		if seats[cm.CharID] == seatFueler {
			f.Since = sinceOf[cm.CharID]
		}
		if seats[cm.CharID] == seatProcurer {
			p.Since = sinceOf[cm.CharID]
		}
		fc = append(fc, f)
		pc = append(pc, p)
	}
	unix := clock.Unix()
	procurers := dealSeat(pc, nProcurers, unix, exclude)
	for _, id := range procurers {
		exclude[id] = true
	}
	fuelers := dealSeat(fc, nFuelers, unix, exclude)
	want := map[int64]string{}
	for _, id := range fuelers {
		want[id] = seatFueler
	}
	for _, id := range procurers {
		want[id] = seatProcurer
	}
	// Seat rows.
	for id, seat := range seats {
		if want[id] != seat {
			m.ceoLog(ctx, c, powerRoles, "unseat", id, 0, "relieved %s as %s", m.charNameOr(ctx, id), posSeatTitles[seat])
		}
	}
	rows := map[int64]corpPost{}
	for id, seat := range want {
		p := corpPost{CorpID: c.CorpID, CharID: id, Post: seat, Since: unix, Perf: 1}
		if cp, ok := cur[id]; ok && cp.Post == seat {
			p = cp
		}
		rows[id] = p
		if seats[id] == seat {
			continue
		}
		m.ceoLog(ctx, c, powerRoles, "seat", id, 0, "made %s its %s", m.charNameOr(ctx, id), posSeatTitles[seat])
	}
	if err := m.savePosts(ctx, c.CorpID, famStarbase, rows); err != nil {
		return fmt.Errorf("starbase seats: %w", err)
	}
	// Roles: only bits this pass granted are ever taken back.
	grants := m.loadPosGrants(ctx, c.CorpID)
	for _, cm := range all {
		wantRoles := uint64(0)
		switch want[cm.CharID] {
		case seatFueler:
			wantRoles = fuelerRoles
		case seatProcurer:
			wantRoles = procurerRoles
		}
		prev := grants[cm.CharID]
		if prev == 0 && wantRoles == 0 {
			continue
		}
		wantRoles = m.stripGuardedTake(ctx, c.CorpID, cm.CharID, wantRoles)
		next, granted := industryRoleUpdate(cm.Roles, prev, wantRoles)
		if next != cm.Roles {
			if err := m.setMemberRoles(ctx, cm.CharID, next); err != nil {
				return fmt.Errorf("starbase seat roles %d: %w", cm.CharID, err)
			}
		}
		if granted != prev {
			m.savePosGrant(ctx, c.CorpID, cm.CharID, granted)
		}
	}
	// Each tower's Fueler: round-robin over the seat.
	if len(fuelers) > 0 {
		sort.Slice(plans, func(i, j int) bool { return plans[i].PlanID < plans[j].PlanID })
		for i, p := range plans {
			f := fuelers[i%len(fuelers)]
			if p.FuelerID != f {
				p.FuelerID = f
				_ = m.saveTowerPlan(ctx, p)
			}
		}
	}
	return nil
}

func (m *Mod) loadPosGrants(ctx context.Context, corpID int64) map[int64]uint64 {
	out := map[int64]uint64{}
	rows, err := m.db.QueryContext(ctx, `SELECT characterID, mask FROM rolebot_corp_pos_grants WHERE corporationID = ?`, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var mask uint64
		if rows.Scan(&id, &mask) == nil {
			out[id] = mask
		}
	}
	return out
}

func (m *Mod) savePosGrant(ctx context.Context, corpID, charID int64, mask uint64) {
	if mask == 0 {
		_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_pos_grants WHERE corporationID = ? AND characterID = ?`, corpID, charID)
		return
	}
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_pos_grants (corporationID, characterID, mask) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE mask = VALUES(mask)`, corpID, charID, mask)
}

// seatExecutor is who flies a seat's errands: the holder when it is an
// enabled bot, else the CEO in its name.
func (m *Mod) seatExecutor(ctx context.Context, c *corpCEO, holder int64) int64 {
	if holder == 0 {
		return c.CharID
	}
	if m.isRostered(ctx, holder) {
		if rb, err := getRolebot(ctx, m.db, holder); err == nil && rb != nil && rb.Enabled {
			return holder
		}
	}
	return c.CharID
}

// ── the desks ───────────────────────────────────────────────────────────────

// starbaseDesks runs the Fueler and Procurer desks for one corporation, from
// the staff desk sweep.
func (m *Mod) starbaseDesks(ctx context.Context, c *corpCEO, ch Charter) {
	if !StarbasePower() || !c.powerOn(powerStarbase) {
		// No towers, but requisitions that are not a tower's still need a buyer: the work board's trips
		// (corpbuyback_sweep.go) are posted whatever the starbase power says.
		buyer := c.CharID
		if procurers := m.seatHolders(ctx, c.CorpID, seatProcurer); len(procurers) > 0 {
			buyer = m.seatExecutor(ctx, c, procurers[0])
		}
		if err := m.procurerDeskLines(ctx, c, ch, buyer, true); err != nil {
			m.logBot(c.CharID, logError, "ceo", "procurer desk: %v", err)
		}
		return
	}
	plans, err := m.corpTowerPlans(ctx, c.CorpID, true)
	if err != nil {
		return
	}
	procurers := m.seatHolders(ctx, c.CorpID, seatProcurer)
	buyer := c.CharID
	if len(procurers) > 0 {
		buyer = m.seatExecutor(ctx, c, procurers[0])
	}
	// Fuel and the guns are Infrastructure's once a Director heads it
	// (infraDesk); until then the seats' desks run them here.
	infra := m.infraRunsTowers(ctx, c)
	if len(plans) > 0 && !infra {
		m.fuelerDesk(ctx, c, ch, plans)
	}
	if err := m.procurerDesk(ctx, c, ch, buyer); err != nil {
		m.logBot(c.CharID, logError, "ceo", "procurer desk: %v", err)
	}
	m.traderDesk(ctx, c, ch, plans)
	m.postStagingMoves(ctx, c, plans)
	m.outsourceStaleLegs(ctx, c)
	if !infra {
		m.posDefenseDesk(ctx, c, ch, plans)
	}
	m.siegeDesk(ctx, c, ch, plans)
	if m.corpHasOpenPosWork(ctx, c.CorpID) {
		markPosWork(c.CorpID)
	} else {
		clearPosWork(c.CorpID)
	}
}

// towerUpkeep is the part of starbaseDesks that runs while nobody is at the desks (staffDesks):
// stale legs swept, the towers' fuel legs and requisitions posted, and the board's lines handed
// to whoever is logged in. Without it a corporation's towers go dark while its officers sleep.
// Planning, defence, disposals and sieges still wait for an officer.
func (m *Mod) towerUpkeep(ctx context.Context, c *corpCEO, ch Charter) {
	if !StarbasePower() || !c.powerOn(powerStarbase) {
		return
	}
	plans, err := m.corpTowerPlans(ctx, c.CorpID, true)
	if err != nil || len(plans) == 0 {
		return
	}
	m.sweepHauls(ctx, c.CorpID)
	m.fuelerDesk(ctx, c, ch, plans)
	buyer := c.CharID
	if procurers := m.seatHolders(ctx, c.CorpID, seatProcurer); len(procurers) > 0 {
		buyer = m.seatExecutor(ctx, c, procurers[0])
	}
	if err := m.procurerDesk(ctx, c, ch, buyer); err != nil {
		m.logBot(c.CharID, logError, "ceo", "procurer desk: %v", err)
	}
	if m.corpHasOpenPosWork(ctx, c.CorpID) {
		markPosWork(c.CorpID)
	} else {
		clearPosWork(c.CorpID)
	}
}

// fuelLegBusy is whether a tower already has a fuel leg on the board: an open one, or a claimed
// one whose carrier is about. A claim whose carrier logged off with the fuel aboard does not
// hold up an urgent tower: a second leg is posted, and the first still delivers when its carrier
// is back.
func (m *Mod) fuelLegBusy(ctx context.Context, planID int64, urgent bool) bool {
	legs, err := m.queryHauls(ctx, `WHERE planID = ? AND kind = ? AND status IN ('open', 'claimed')`, planID, haulKindFuel)
	if err != nil {
		return true
	}
	for _, l := range legs {
		if !fuelClaimStranded(l.Status, l.CarrierID, urgent, carrierAway) {
			return true
		}
	}
	return false
}

// fuelClaimStranded is whether a fuel leg no longer counts as covering its tower: claimed, the
// tower urgent, and the carrier away (carrierAway: logged off or benched). Pure.
func fuelClaimStranded(status string, carrierID int64, urgent bool, away func(int64) string) bool {
	return status == "claimed" && urgent && carrierID != 0 && away(carrierID) != ""
}

// corpHasOpenPosWork is whether anything on the boards wants a pilot.
func (m *Mod) corpHasOpenPosWork(ctx context.Context, corpID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM rolebot_corp_haul WHERE corporationID = ? AND status IN ('open','claimed'))
		     + (SELECT COUNT(*) FROM rolebot_corp_requisition WHERE corporationID = ? AND status = 'buying')
		     + (SELECT COUNT(*) FROM rolebot_corp_survey WHERE corporationID = ? AND status IN ('open','claimed'))`,
		corpID, corpID, corpID).Scan(&n)
	return n > 0
}

// fuelerDesk keeps every tower supplied: the staging office's Fuel division
// topped up through requisitions, and fuel legs posted for whatever the bay
// is short of its reserve. Towers at or under fuelUrgentBatches go first and
// their requisitions are urgent (bought on a trip, not bid for).
func (m *Mod) fuelerDesk(ctx context.Context, c *corpCEO, ch Charter, plans []*towerPlan) {
	target, srHours := charterFuelTargets(ch)
	for _, p := range plans {
		if p.TowerID == 0 || p.Status == towerUnanchor || p.StagingStationID == 0 {
			continue
		}
		// An anchored tower whose anchoring is done is onlined here too: the
		// desk runs every 15 minutes, the CEO only every few hours.
		if p.Status == towerAnchored {
			m.tryOnlineTower(ctx, c, p)
		}
		// Strontium is its own line, in hours. The bay keeps as many whole days as fit beside it
		// (posbay.go); the office holds the rest of the charter's reserve.
		tb, err := m.towerBayOf(ctx, p, c.CorpID, target, srHours)
		if err != nil || len(tb.Batch) == 0 {
			continue
		}
		batch, sr, bay, bayDays := tb.Batch, tb.SR, tb.Held, tb.Days
		held := batchesHeld(batch, bay)
		office := m.officeAt(ctx, c.CorpID, p.StagingStationID)
		if office == 0 {
			continue
		}
		div := m.divisionHeld(ctx, c.CorpID, office, corpDivFuel)
		// What the division must hold so bay + division covers target + 1.
		combined := map[int64]int64{}
		for t, q := range bay {
			combined[t] += q
		}
		for t, q := range div {
			combined[t] += q
		}
		// A tower that went dark (UrgentAt, reconcileTowerPlan) is urgent whatever its bay reads: it
		// may lack strontium or a charter, not the batch.
		now := clock.Unix()
		dark := p.UrgentAt > 0 && now-p.UrgentAt < fuelDarkUrgentSec
		urgent := held <= fuelUrgentBatches || dark
		// A tower holding its alliance's sovereignty loses the system the hour it goes dark, so its
		// fuel turns urgent at twice the usual margin (frontier.go).
		if !urgent && held <= 2*fuelUrgentBatches && m.holdsSovFor(ctx, c.CorpID, p.SystemID) {
			urgent = true
		}
		deadline := clock.Unix() + 4*86400
		if urgent {
			deadline = clock.Unix() + 86400
		}
		requisition := func(t, q int64, note string) {
			var pending int64
			if !dark {
				pending = m.reqPending(ctx, c.CorpID, t, p.StagingStationID, corpDivFuel)
			} else {
				// A dark tower counts only lines still filling (reqPendingFresh). A line stalled
				// reqLineStallSec is closed and its quantity posted again below at the urgent deadline.
				if n := m.reqVoidStalled(ctx, c.CorpID, t, p.StagingStationID, corpDivFuel, now, "replaced: the tower is dark"); n > 0 {
					m.logBot(c.CharID, logInfo, "ceo", "replaced %d stalled %s line(s) for %s — the tower is dark",
						n, m.typeNameOr(ctx, t), moonNameOf(ctx, m, p.MoonID))
				}
				pending = m.reqPendingFresh(ctx, c.CorpID, t, p.StagingStationID, corpDivFuel, now)
			}
			short := q - pending
			if short > 0 {
				_, _ = m.postRequisition(ctx, c, requisition{TypeID: t, Qty: short, DestStationID: p.StagingStationID,
					DestFlag: corpDivFuel, Requester: posRequester(ctx, "fueler"), RefKind: "tower", RefID: p.PlanID, Deadline: deadline, Note: note})
			}
		}
		for t, q := range topUpUnits(batch, combined, target+fuelInTransitBatches) {
			requisition(t, q, "fuel for "+moonNameOf(ctx, m, p.MoonID))
		}
		for t, q := range topUpUnits(sr, combined, 1) {
			requisition(t, q, "strontium for "+moonNameOf(ctx, m, p.MoonID))
		}
		// A human Fueler gets the run as an assignment (posassign.go), and
		// while it is accepted no bot leg races them to it.
		if fa := m.openFuelAssignment(ctx, p.PlanID); fa != nil {
			m.progressFuelAssignment(ctx, fa, held)
			if fa.Status == asAccepted && !urgent {
				continue
			}
		} else if p.FuelerID != 0 && !m.isRostered(ctx, p.FuelerID) && held < bayDays {
			if send := min(bayDays-held, batchesHeld(batch, div)); send > 0 {
				m.offerFuelAssignment(ctx, c, p, send, held, batchVolume(batch))
			}
		}
		// The leg: each line's shortfall to the bay's days, scarcest first, strontium last, within the
		// room makeBayRoom makes. A full bay with nothing over plan gets no leg; posted once a line is a whole day under, or strontium is short.
		if (held >= bayDays && len(topUpUnits(sr, bay, 1)) == 0) || m.fuelLegBusy(ctx, p.PlanID, urgent) {
			continue
		}
		room := tb.free() + surplusVolume(baySurplus(batch, sr, bay, bayDays), tb.Vol)
		items := fuelLegLines(batch, sr, bay, div, bayDays, room, corpDivFuel)
		if len(items) == 0 {
			continue
		}
		dl := clock.Unix() + 2*86400
		if urgent {
			dl = clock.Unix() + 12*3600
		}
		_, _ = m.postHaul(ctx, c, haulLeg{Kind: haulKindFuel, PlanID: p.PlanID, FromKind: haulFromOffice,
			FromID: office, FromStationID: p.StagingStationID, FromFlag: corpDivFuel, ToKind: haulToTower,
			ToID: p.TowerID, ToSystemID: p.SystemID, Items: items, PostedBy: posRequester(ctx, "fueler"), Deadline: dl,
			Note: fmt.Sprintf("fuel for %s: %d line(s) toward %d day(s) in the bay (%d held)",
				moonNameOf(ctx, m, p.MoonID), len(items), bayDays, held)})
	}
}
