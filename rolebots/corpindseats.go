package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"

	"gurimoon/corp"
	evedb "gurimoon/db"
	serverram "gurimoon/server/ram"
)

// The Industry department: the lower management under the Director of Industry (corpdirector.go). Its two
// kinds of seat are dealt by the roles power on the starbase seats' rule (dealSeat: an incumbent keeps its seat
// for seatKeepSec and loses it only to a free member who outscores it by seatOutscore).
// Factory Administrator, one per corpBuilderPer members: Factory Manager plus QUERY on Materials, the Library
// and Production, enough to install the corporation's prints against its materials (db.ResolveStationCorpJob).
// Sales Agent, one in every desk corporation, two from salesSecondAt members: the corporation's output is
// listed under its name, so the agent is first in line for listings (corpOrderPlacerFor). It takes Trader plus
// take on Production and Store. A seat held by a human keeps its title and roles, and the module never places
// an order in a human's name. A human holder younger than officerTenureDays gets no take roles. Role bits stay
// in rolebot_corp_grants so a bit a human director ticked by hand is never taken back.

const (
	seatAdmin = "admin"
	seatSales = "sales"

	// salesSecondAt is the member count from which a corporation keeps a
	// second Sales Agent.
	salesSecondAt = 30
	// adminMassProdStep lifts an administrator's score per level of Mass
	// Production.
	adminMassProdStep = 0.1
	// indSeatHumanMul discounts a human for the Sales seat: it sells only by
	// hand, so its slots are not the corporation's to count on.
	indSeatHumanMul = 0.5
)

var indSeatTitles = map[string]string{seatAdmin: "Factory Administrator", seatSales: "Sales Agent"}

// salesRoles is what a Sales Agent holds.
const salesRoles = corp.RoleTrader |
	corp.RoleHangarCanQuery3 | corp.RoleHangarCanQuery4 |
	corp.RoleHangarCanTake3 | corp.RoleHangarCanTake4

// salesTakeRoles are the take bits a new human Sales Agent waits for.
const salesTakeRoles = corp.RoleHangarCanTake3 | corp.RoleHangarCanTake4

// adminScore ranks Factory Administrators. Pure.
func adminScore(c industryCand) float64 {
	return builderScore(c) * (1 + adminMassProdStep*float64(c.MassProd)) * c.Perf()
}

// salesScore ranks Sales Agents: the envious (HIGH greed) and the patient
// (LOW patience is unhurried), lifted by the order slots they bring. Pure.
func salesScore(c industryCand) float64 {
	v := (1 + 0.5*c.Greed - 0.3*c.Patience) * math.Sqrt(1+c.Days) * (1 + float64(c.FreeSlots)/5)
	if c.Human {
		v *= indSeatHumanMul
	}
	return math.Max(0.01, v*c.Perf())
}

// indSeatCounts is how many of each seat a corporation keeps. Pure.
func indSeatCounts(members int) (admins, sales int) {
	if members <= 0 {
		return 0, 0
	}
	admins = int(math.Ceil(float64(members) / corpBuilderPer))
	sales = 1
	if members >= salesSecondAt {
		sales = 2
	}
	return admins, sales
}

// indSeatRow is one seat holder as stored.
type indSeatRow struct {
	CharID   int64
	Seat     string
	Since    int64
	Perf     float64
	ReviewAt int64
	Metric   float64
	Credit   float64
}

// indSeatsOf is every Industry seat of a corporation, by holder.
func (m *Mod) indSeatsOf(ctx context.Context, corpID int64) map[int64]indSeatRow {
	out := map[int64]indSeatRow{}
	for id, p := range m.familyPostsOf(ctx, corpID, famIndustry) {
		out[id] = indSeatRow{CharID: id, Seat: p.Post, Since: p.Since, Perf: p.Perf, ReviewAt: p.ReviewAt, Metric: p.Metric, Credit: p.Credit}
	}
	return out
}

// indSeatHolders lists a seat's holders, oldest first.
func (m *Mod) indSeatHolders(ctx context.Context, corpID int64, seat string) []int64 {
	return m.postHolders(ctx, corpID, seat)
}

// orderPressureCorpOf is the corporation whose order pressure a pilot trains
// Trade and Retail against: the corporation it runs, or the one it sells for.
func orderPressureCorpOf(charID int64) (int64, bool) {
	if c, ok := managedCEOOf(charID); ok {
		return c, true
	}
	return salesAgentCorpOf(charID)
}

// ── the deal ────────────────────────────────────────────────────────────────

// fillIndustryCand reads the seat-specific facts of one candidate: Mass
// Production, the order slots a bot has free, and its greed and patience.
func (m *Mod) fillIndustryCand(ctx context.Context, cd *industryCand, id pilotIdentity) {
	cd.Greed = id.trait(TraitGreed)
	cd.Patience = id.trait(TraitPatience)
	if lvl, err := m.charSkillLevelFor(ctx, cd.CharID, serverram.SkillMassProduction); err == nil {
		cd.MassProd = lvl
	}
	if !cd.Human {
		cd.FreeSlots = m.freeOrderSlots(ctx, cd.CharID)
	}
}

// dealIndustrySeats deals the department's seats and records them, returning what each holder should hold.
// Sales first, the scarcer seat, then the administrators from who is left. The director and its deputy keep
// their own jobs and are dealt neither.
func (m *Mod) dealIndustrySeats(ctx context.Context, c *corpCEO, cands []industryCand, director, deputy int64,
	benched map[int64]bool) map[int64]string {
	cur := m.indSeatsOf(ctx, c.CorpID)
	exclude := map[int64]bool{director: true, deputy: true}
	for id := range benched {
		exclude[id] = true // a benched builder is not handed a seat at the factory
	}
	nAdmins, nSales := indSeatCounts(len(cands))
	var ac, sc []seatCand
	for _, cd := range cands {
		r := cur[cd.CharID]
		a := seatCand{CharID: cd.CharID, Human: cd.Human, Score: adminScore(cd)}
		s := seatCand{CharID: cd.CharID, Human: cd.Human, Score: salesScore(cd)}
		if r.Seat == seatAdmin {
			a.Since = r.Since
		}
		if r.Seat == seatSales {
			s.Since = r.Since
		}
		ac = append(ac, a)
		sc = append(sc, s)
	}
	now := clock.Unix()
	sales := dealSeat(sc, nSales, now, exclude)
	for _, id := range sales {
		exclude[id] = true
	}
	admins := dealSeat(ac, nAdmins, now, exclude)

	want := map[int64]string{}
	for _, id := range admins {
		want[id] = seatAdmin
	}
	for _, id := range sales {
		want[id] = seatSales
	}
	rows := map[int64]corpPost{}
	for id, seat := range want {
		p := corpPost{CorpID: c.CorpID, CharID: id, Post: seat, Since: now, Perf: 1}
		if r, ok := cur[id]; ok && r.Seat == seat {
			p.Since, p.Perf, p.ReviewAt, p.Metric, p.Credit = r.Since, r.Perf, r.ReviewAt, r.Metric, r.Credit
		}
		rows[id] = p
	}
	for id, r := range cur {
		if want[id] == r.Seat {
			continue
		}
		if r.Seat == seatSales {
			// A Sales Agent stood down takes no corporation orders with it (corpdirector.go): cancel now, while its
			// trading role still resolves them to the corporation.
			m.cancelCorpOrdersOf(ctx, id)
		}
		m.ceoLog(ctx, c, powerRoles, "unseat", id, 0, "relieved %s as %s", m.charNameOr(ctx, id), indSeatTitles[r.Seat])
	}
	for id, seat := range want {
		if cur[id].Seat == seat {
			continue
		}
		m.ceoLog(ctx, c, powerRoles, "seat", id, 0, "made %s its %s", m.charNameOr(ctx, id), indSeatTitles[seat])
	}
	if err := m.savePosts(ctx, c.CorpID, famIndustry, rows); err != nil {
		rlogf("rolebots: corp %d industry seats: %v", c.CorpID, err)
	}
	return want
}

// indSeatRoles is the role mask one seat holder should hold. Pure.
func indSeatRoles(seat string, human bool, days float64) uint64 {
	switch seat {
	case seatAdmin:
		return builderRoles
	case seatSales:
		if human && days < officerTenureDays {
			return salesRoles &^ salesTakeRoles
		}
		return salesRoles
	}
	return 0
}

// clearIndustrySeats drops every seat of a corporation that no longer runs
// an industry desk.
func (m *Mod) clearIndustrySeats(ctx context.Context, c *corpCEO) {
	for id, r := range m.indSeatsOf(ctx, c.CorpID) {
		if r.Seat == seatSales {
			m.cancelCorpOrdersOf(ctx, id)
		}
	}
	_ = m.savePosts(ctx, c.CorpID, famIndustry, nil)
}

// ── who places the output's orders ──────────────────────────────────────────

// salesAgentPlacer is the first enabled bot Sales Agent with a trading role
// and slots to spare, 0 for none.
func (m *Mod) salesAgentPlacer(ctx context.Context, corpID, ceoID int64) int64 {
	for _, id := range m.indSeatHolders(ctx, corpID, seatSales) {
		if id == ceoID || !m.isRostered(ctx, id) {
			continue
		}
		if rb, err := getRolebot(ctx, m.db, id); err != nil || rb == nil || !rb.Enabled {
			continue
		}
		info, err := evedb.GetCorporationMemberInfo(ctx, m.db, id)
		if err != nil || info == nil || info.Corprole&(corp.RoleTrader|corp.RoleDirector) == 0 {
			continue
		}
		if m.freeOrderSlots(ctx, id) > corpOrderSlotKeep {
			return id
		}
	}
	return 0
}

// isOutputOrderKind reports whether an order kind is the Sales Agent's
// business: selling what the factory made.
func isOutputOrderKind(kind string) bool {
	switch kind {
	case corpOrderProduct, corpOrderStore, corpOrderSpoils:
		return true
	}
	return false
}

// corpOrderPlacerFor is corpOrderPlacer with the department in front: the
// factory's output is placed by a Sales Agent when one has room.
func (m *Mod) corpOrderPlacerFor(ctx context.Context, corpID, ceoID int64, kind string) int64 {
	if isOutputOrderKind(kind) {
		if id := m.salesAgentPlacer(ctx, corpID, ceoID); id != 0 {
			return id
		}
	}
	return m.corpOrderPlacer(ctx, corpID, ceoID)
}

// creditSalesSeat adds what an order a Sales Agent placed has sold to its
// running tally (read by the weekly review). Best effort; a placer that holds
// no Sales seat is simply not credited.
func (m *Mod) creditSalesSeat(ctx context.Context, corpID, placer int64, value float64) {
	if placer == 0 || value <= 0 {
		return
	}
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_posts SET credit = credit + ?
		 WHERE corporationID = ? AND characterID = ? AND post = ?`, value, corpID, placer, seatSales)
}

// ── reviews ─────────────────────────────────────────────────────────────────

// reviewIndustrySeats is the weekly review of the department's seats: an administrator on the runs it
// delivered, a Sales Agent on what its orders sold, each against its own last week. The first review sets a
// baseline. The rating multiplies the seat score, so a poor holder is outscored out after seatKeepSec.
func (m *Mod) reviewIndustrySeats(ctx context.Context, c *corpCEO) {
	now := clock.Unix()
	since := now - staffReviewEverySec
	for _, r := range m.indSeatsOf(ctx, c.CorpID) {
		if r.ReviewAt != 0 && now-r.ReviewAt < staffReviewEverySec {
			continue
		}
		var metric float64
		var why string
		switch r.Seat {
		case seatAdmin:
			_ = m.db.QueryRowContext(ctx, `
				SELECT COALESCE(SUM(runs), 0) FROM rolebot_corp_wages
				 WHERE corporationID = ? AND characterID = ? AND paidAt >= ?`, c.CorpID, r.CharID, since).Scan(&metric)
			why = fmt.Sprintf("%.0f run(s) delivered this week (was %.0f)", metric, r.Metric)
		case seatSales:
			metric = r.Credit
			why = fmt.Sprintf("%s sold this week (was %s)", iskString(metric), iskString(r.Metric))
		default:
			continue
		}
		perf := r.Perf
		if r.ReviewAt != 0 {
			perf = clampPerf((1-staffPerfAlpha)*r.Perf + staffPerfAlpha*staffTrend(metric, r.Metric, true))
			m.ceoLog(ctx, c, powerRoles, "review", r.CharID, perf, "reviewed %s as %s: %.2f → %.2f — %s",
				m.charNameOr(ctx, r.CharID), indSeatTitles[r.Seat], r.Perf, perf, why)
		}
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_posts SET perf = ?, reviewAt = ?, metric = ?, credit = 0
			 WHERE corporationID = ? AND characterID = ? AND post = ?`,
			perf, now, metric, c.CorpID, r.CharID, r.Seat)
	}
}

// indSeatPerf is each seat holder's rating (1 for anybody unrated).
func (m *Mod) indSeatPerf(ctx context.Context, corpID int64) map[int64]float64 {
	out := map[int64]float64{}
	for id, r := range m.indSeatsOf(ctx, corpID) {
		if r.Perf > 0 {
			out[id] = r.Perf
		}
	}
	return out
}

// ── the department, for the pages ───────────────────────────────────────────

// indDeptRow is one seat holder for the CEO page and the IGB.
type indDeptRow struct {
	CharID int64
	Name   string
	Seat   string
	Title  string
	Human  bool
	Days   float64
	Perf   float64
}

// industryDepartment lists the department's seat holders, Sales first.
func (m *Mod) industryDepartment(ctx context.Context, corpID int64) []indDeptRow {
	var out []indDeptRow
	now := clock.Unix()
	for id, r := range m.indSeatsOf(ctx, corpID) {
		out = append(out, indDeptRow{CharID: id, Name: m.charNameOr(ctx, id), Seat: r.Seat,
			Title: indSeatTitles[r.Seat], Human: !m.isRostered(ctx, id),
			Days: float64(now-r.Since) / 86400, Perf: r.Perf})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seat != out[j].Seat {
			return out[i].Seat == seatSales
		}
		return out[i].Name < out[j].Name
	})
	return out
}
