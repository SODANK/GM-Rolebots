package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
)

// The Director of Industry runs a corporation's factories and heads its
// Industry department (corpindseats.go). In every bot-CEO corporation whose tags
// let its ore desk build (oreBoundsFor.Max > 0) the roles pass keeps:
//   - one Director, scored by build drive x days served and kept until it leaves,
//     is disabled, or is outscored by 30% after a fortnight. It holds Factory
//     Manager, slot rental, Trader and query+take on divisions 1-3, and is paid;
//   - Factory Administrators (one per corpBuilderPer members): Factory Manager
//     plus query only, so the core's job gate (db/ramcorp.go) lets them use the
//     library but not carry it out;
//   - a deputy for chores the module must fly (a human director keeps the title).

const (
	corpBuilderPer        = 6
	directorKeepSec       = 14 * 86400
	directorOutscore      = 1.3
	directorSalaryEvery   = 7 * 86400
	corpIndustryDirector  = "Director of Industry"
	corpIndustryRolesBase = corp.RoleFactoryManager | corp.RoleHangarCanQuery1 |
		corp.RoleHangarCanQuery2 | corp.RoleHangarCanQuery3
)

// directorRoles is what the Director of Industry holds: the factory, the
// slot rentals, a trading role, and query+take on Materials, the Library and
// Production.
const directorRoles = corpIndustryRolesBase | corp.RoleCanRentFactorySlot |
	corp.RoleCanRentResearchSlot | corp.RoleTrader |
	corp.RoleHangarCanTake1 | corp.RoleHangarCanTake2 | corp.RoleHangarCanTake3

// directorTakeRoles are the take bits a new human director waits for.
const directorTakeRoles = corp.RoleHangarCanTake1 | corp.RoleHangarCanTake2 |
	corp.RoleHangarCanTake3

// builderRoles is what a Factory Administrator holds (corpindseats.go).
const builderRoles = corpIndustryRolesBase

// deputyRoles is an administrator's plus a trading role and take on the
// divisions output is listed from, so the deputy can place corporation orders
// when other slots are full (corporders.go, db.ResolveMarketAccount).
const deputyRoles = builderRoles | corp.RoleTrader |
	corp.RoleHangarCanTake3 | corp.RoleHangarCanTake4 | corp.RoleHangarCanTake5

// industryCand is one member as the industry pass scores it.
type industryCand struct {
	CharID int64
	Name   string
	Human  bool
	Roles  uint64
	Drive  float64 // SupplyDrive × IndustryAppetite
	Days   float64

	// The seat deal's extras (corpindseats.go).
	Greed, Patience float64 // trait poles, -1..1
	MassProd        int     // Mass Production level
	FreeSlots       int64   // order slots a bot has free (0 for a human)
	Rating          float64 // the seat's weekly review, 0 = unrated
}

// Perf is the candidate's seat rating, 1 when unrated.
func (c industryCand) Perf() float64 {
	if c.Rating <= 0 {
		return 1
	}
	return c.Rating
}

// directorScore is the overseer's score: build drive × days served, 0 for
// anybody less keen than an even temperament.
func directorScore(c industryCand) float64 {
	if c.Drive < supplyDriveBase {
		return 0
	}
	return c.Drive * (1 + c.Days)
}

// builderScore ranks contract builders.
func builderScore(c industryCand) float64 {
	return c.Drive * math.Sqrt(1+c.Days)
}

// pickDirector keeps the sitting director unless it is gone or, after
// directorKeepSec in the seat, somebody outscores it by directorOutscore.
// Pure, for the test.
func pickDirector(cands []industryCand, current, since, now int64) int64 {
	best, bestScore := int64(0), 0.0
	curScore := -1.0
	for _, c := range cands {
		s := directorScore(c)
		if c.CharID == current {
			curScore = s
		}
		if s > bestScore || (s == bestScore && s > 0 && c.CharID < best) {
			best, bestScore = c.CharID, s
		}
	}
	if curScore > 0 {
		if now-since < directorKeepSec || bestScore < curScore*directorOutscore {
			return current
		}
	}
	return best
}

// pickDeputy is the bot that flies the director's chores: the director itself
// when it is a bot, else the best-scoring bot.
func pickDeputy(cands []industryCand, director int64) int64 {
	best, bestScore := int64(0), -1.0
	for _, c := range cands {
		if c.CharID == director && !c.Human {
			return director
		}
		if c.Human {
			continue
		}
		if s := directorScore(c) + builderScore(c)*1e-6; s > bestScore {
			best, bestScore = c.CharID, s
		}
	}
	return best
}

// ceoIndustryStaff runs the industry staffing pass from the roles power, after
// the officer pass. It re-reads the members so the officer bits just written
// are not overwritten from a stale mask.
func (m *Mod) ceoIndustryStaff(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts) error {
	p, err := m.loadCorpPolicy(ctx, c.CorpID)
	if err != nil {
		return err
	}
	desk := hasIndustryDesk(facts.Policy.Tags)
	all, err := m.corpMembers(ctx, c.CorpID, c.CharID)
	if err != nil {
		return err
	}

	now := win32Now()
	perf := m.indSeatPerf(ctx, c.CorpID)
	var cands []industryCand
	for _, cm := range all {
		if !cm.Eligible() {
			continue
		}
		id := m.memberIdentity(ctx, cm)
		cd := industryCand{
			CharID: cm.CharID, Name: cm.Name, Human: cm.Human(), Roles: cm.Roles,
			Drive:  id.Tune.SupplyDrive * id.Tune.IndustryAppetite,
			Days:   tenureDays(cm.Since, now),
			Rating: perf[cm.CharID],
		}
		if desk {
			m.fillIndustryCand(ctx, &cd, id)
		}
		cands = append(cands, cd)
	}

	director, deputy := int64(0), int64(0)
	seats := map[int64]string{}
	if desk {
		director = pickDirector(cands, p.DirectorID, p.DirectorSince, clock.Unix())
		deputy = pickDeputy(cands, director)
		// The chair's own appointment (corpcommanddesk_org.go) stands for
		// its hold, whoever the pass would have picked.
		if m.humanHoldOn(ctx, c.CorpID, cmdKindIndustry) {
			if p.DirectorID != 0 && m.charCorpID(ctx, p.DirectorID) == c.CorpID {
				director = p.DirectorID
			}
			if p.DeputyID != 0 && m.charCorpID(ctx, p.DeputyID) == c.CorpID {
				deputy = p.DeputyID
			} else if p.DeputyID == 0 {
				deputy = pickDeputy(cands, director)
			}
		}
		seats = m.dealIndustrySeats(ctx, c, cands, director, deputy, m.benchedBuilders(ctx, c.CorpID))
		m.reviewIndustrySeats(ctx, c)
	} else {
		m.clearIndustrySeats(ctx, c)
	}

	// Orders a stood-down director or deputy placed go back to the office or wallet
	// before its roles are stripped, while its trading role still resolves to the corporation.
	for _, old := range []int64{p.DirectorID, p.DeputyID} {
		if old != 0 && old != c.CharID && old != director && old != deputy {
			m.cancelCorpOrdersOf(ctx, old)
		}
	}

	// Roles. Only bits this pass granted are ever taken back (grants).
	grants := m.loadIndustryGrants(ctx, c.CorpID)
	for _, cd := range cands {
		want := uint64(0)
		switch {
		case cd.CharID == director && cd.Human && cd.Days < officerTenureDays:
			// The take roles open the library to carrying prints out; a
			// director that new runs the factory without them.
			want = directorRoles &^ directorTakeRoles
		case cd.CharID == director && cd.Human:
			// The head of a division holds Director (headHoldsDirector): Management asks for it.
			want = directorRoles | corp.RoleDirector
		case cd.CharID == director:
			want = directorRoles
		case cd.CharID == deputy:
			// The deputy flies the chores and places corporation orders when the CEO's
			// slots are full (corporders.go); the core's market account needs a trading role.
			want = deputyRoles
		default:
			want = indSeatRoles(seats[cd.CharID], cd.Human, cd.Days)
			// A bot Sales Agent also sells the Spoils (corpspoils.go), which the core's sale
			// draws only from a division it may take from. stripGuardedTake keeps a human off it.
			if seats[cd.CharID] == seatSales {
				if d := m.corpDivs(ctx, c.CorpID).Div(purSpoils); d > 0 {
					want |= divTakeBit(d) | divQueryBit(d)
				}
			}
		}
		want = m.stripGuardedTake(ctx, c.CorpID, cd.CharID, want)
		prev := grants[cd.CharID]
		next, granted := industryRoleUpdate(cd.Roles, prev, want)
		if next != cd.Roles {
			if err := m.setMemberRoles(ctx, cd.CharID, next); err != nil {
				return fmt.Errorf("industry roles %d: %w", cd.CharID, err)
			}
		}
		if granted != prev {
			m.saveIndustryGrant(ctx, c.CorpID, cd.CharID, granted)
		}
	}

	// The seat.
	if director != p.DirectorID {
		name := m.charNameOr(ctx, director)
		switch {
		case director == 0:
			m.ceoLog(ctx, c, powerRoles, "director", p.DirectorID, 0,
				"%s has nobody running its factories", facts.Name)
		case p.DirectorID == 0:
			m.ceoLog(ctx, c, powerRoles, "director", director, 0,
				"made %s %s of %s", name, corpIndustryDirector, facts.Name)
		default:
			m.ceoLog(ctx, c, powerRoles, "director", director, 0,
				"replaced %s with %s as %s", m.charNameOr(ctx, p.DirectorID), name, corpIndustryDirector)
		}
		p.DirectorID, p.DirectorSince = director, clock.Unix()
		p.SalaryAt = 0
	}
	p.DeputyID = deputy

	// The salary is the Treasurer's to pay when there is one
	// (corpstaff_treasurer.go); the CEO pays it otherwise.
	if !m.staffCovers(ctx, c, postTreasurer, "") {
		if err := m.payDirectorSalary(ctx, c, ch, &p); err != nil {
			return err
		}
	}
	if err := m.saveCorpPolicyRow(ctx, p); err != nil {
		return err
	}
	m.nameCorpDivisions(ctx, c.CorpID, facts.Name, deskDivisionNames(facts.Policy.Tags))
	return nil
}

// payDirectorSalary pays the Industry Director's weekly salary when due, scaled
// to what the corporation holds (directorSalaryFor). It stamps p.SalaryAt; the
// caller saves the row.
func (m *Mod) payDirectorSalary(ctx context.Context, c *corpCEO, ch Charter, p *corpPolicyRow) error {
	director := p.DirectorID
	if director == 0 || ch.DirectorSalary <= 0 || clock.Unix()-p.SalaryAt < directorSalaryEvery {
		return nil
	}
	free, balance, ferr := m.corpFreeCapital(ctx, c.CorpID, ch)
	salary := directorSalaryFor(ch.DirectorSalary, balance)
	if ferr != nil || salary <= 0 || free < salary {
		return nil
	}
	_, bal, err := m.corpWithdraw(ctx, c.CorpID, director, salary, "Industry Director salary")
	if err != nil {
		if errors.Is(err, evedb.ErrInsufficientFunds) {
			return nil
		}
		return err
	}
	m.pushBalance(director, bal)
	m.corpBook(ctx, c.CorpID, lineSalary, -salary, director, "salary to "+m.charNameOr(ctx, director))
	m.ceoLog(ctx, c, powerTreasury, "salary", director, salary,
		"paid %s its weekly %s salary of %s", m.charNameOr(ctx, director), corpIndustryDirector,
		iskString(salary))
	p.SalaryAt = clock.Unix()
	return nil
}

// benchedBuilders is who is benched from corporation work right now.
func (m *Mod) benchedBuilders(ctx context.Context, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	ids, err := int64Column(ctx, m, `
		SELECT characterID FROM rolebot_corp_bench WHERE corporationID = ? AND untilAt > ?`,
		corpID, clock.Unix())
	if err == nil {
		for _, id := range ids {
			out[id] = true
		}
	}
	return out
}

// nameCorpDivisions writes the desks' names (deskDivisionNames) on divisions
// nobody else named. A division already carrying one of the desk names is the
// module's to rename; anything else is a human's and is left alone.
func (m *Mod) nameCorpDivisions(ctx context.Context, corpID int64, corpName string, names map[int]string) {
	for div, want := range names {
		// Named for the arm that owns the slot (corpdeptdivs.go).
		want = taggedDivName(deskDivArm[div], want)
		col := fmt.Sprintf("division%d", div)
		var cur sql.NullString
		if err := m.db.QueryRowContext(ctx,
			`SELECT `+col+` FROM corporation WHERE corporationID = ?`, corpID).Scan(&cur); err != nil {
			continue
		}
		if cur.Valid && cur.String == want {
			continue
		}
		owned := !cur.Valid || cur.String == ""
		for _, n := range corpDivisionNames {
			if untagDivName(cur.String) == n {
				owned = true
			}
		}
		if !owned {
			continue // a human named it; it is theirs
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE corporation SET `+col+` = ? WHERE corporationID = ?`, want, corpID); err != nil {
			continue
		}
		var old any
		if cur.Valid {
			old = cur.String
		}
		pushCorpChanged(corpID, corpName, esp.CorpFieldChange{Column: col, Old: old, New: want})
	}
}

// corpDirectorOf is a corporation's Industry Director (0 for none).
func (m *Mod) corpDirectorOf(ctx context.Context, corpID int64) int64 {
	p, err := m.loadCorpPolicy(ctx, corpID)
	if err != nil {
		return 0
	}
	return p.DirectorID
}

// charNameOr is a best-effort character name.
func (m *Mod) charNameOr(ctx context.Context, charID int64) string {
	if charID == 0 {
		return "nobody"
	}
	if name := m.charNameOf(ctx, charID); name != "" {
		return name
	}
	return fmt.Sprintf("pilot %d", charID)
}

// loadIndustryGrants reads the industry role bits the pass granted, by member.
func (m *Mod) loadIndustryGrants(ctx context.Context, corpID int64) map[int64]uint64 {
	out := map[int64]uint64{}
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID, mask FROM rolebot_corp_grants WHERE corporationID = ?`, corpID)
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

// saveIndustryGrant records what the pass holds responsible for; 0 forgets.
func (m *Mod) saveIndustryGrant(ctx context.Context, corpID, charID int64, mask uint64) {
	if mask == 0 {
		_, _ = m.db.ExecContext(ctx,
			`DELETE FROM rolebot_corp_grants WHERE corporationID = ? AND characterID = ?`, corpID, charID)
		return
	}
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_grants (corporationID, characterID, mask) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE mask = VALUES(mask)`, corpID, charID, mask)
}

// industryRoleUpdate is the role arithmetic: take back only what the pass
// granted before, add what it wants now, and hold itself responsible only
// for the bits the member did not already have from someone else. Pure.
func industryRoleUpdate(roles, prev, want uint64) (next, granted uint64) {
	others := roles &^ prev
	return others | want, want &^ others
}

const (
	// directorSalaryShare caps the weekly salary at this share of the
	// corporation's wallet; directorSalaryMin is the least worth paying.
	directorSalaryShare = 0.02
	directorSalaryMin   = 50000
)

// directorSalaryFor is the week's salary: the charter's figure, never more than
// directorSalaryShare of the wallet, and nothing below directorSalaryMin. Pure,
// for the test.
func directorSalaryFor(charter, balance float64) float64 {
	s := math.Floor(math.Min(charter, balance*directorSalaryShare))
	if s < directorSalaryMin {
		return 0
	}
	return s
}
