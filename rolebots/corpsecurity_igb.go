package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/server/gangsta"
)

// The Security department page of the Corporate Portal (/corp/security): how the
// corporation thinks about a fight. It shows the doctrine, the ladder the Director of
// Security climbs each pass (corpstaff_dept.go securityDesk), every war and where the
// enemy is out, the prices on heads filed, hunts running and what the wing brought home.
// It only reads: the ladder is rebuilt from the desk's reads, never by calling the desk.
// Money is drawn as bars (ratioBar, relBar), not numbers, since anyone who forges an
// identity header can read the portal (corpportal.go); a bounty on a head stays exact.

// handleIGBCorpSecurity serves /corp/security.
func (m *Mod) handleIGBCorpSecurity(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	charID, corpID := m.portalViewer(ctx, r)
	if corpID == 0 {
		esp.WriteCorpPortalPage(w, r, "security", igbF3("Open this page from your own terminal to see your corporation's security."))
		return
	}
	esp.WriteCorpPortalPage(w, r, "security", m.securityBody(ctx, r, corpID, charID))
}

// securityBody is the page: the figures, the posture and the desk's
// reasoning, the wars, the bounty board, and the department.
func (m *Mod) securityBody(ctx context.Context, r *http.Request, corpID, viewer int64) string {
	pg := m.igbPageFor(r)
	host := esp.IGBHost()
	pr := m.portalRosterFor(ctx, corpID)
	var fleets []*Gang
	for _, g := range pr.Fleets {
		if armOfSeatActivity(g.Activity) == armSecurity {
			fleets = append(fleets, g)
		}
	}
	ms := pr.ByArm[armSecurity]
	on, space := onDuty(ms)
	wars := m.securityWars(ctx, corpID)
	heads := m.securityHeads(ctx, corpID)

	var b strings.Builder
	b.WriteString(pg.soft(`The Security department: what the combat wing is for, where it is sent and why, the wars it `+
		`is fighting and the prices on heads. A seat in a fleet is requested on `+
		`<a href="`+host+`/corp/ops">Operations</a>; joining Security is done on the `+
		`<a href="`+host+`/corp/">Overview</a>.`) + "<br><br>\n")
	pg.statStrip(&b, []igbStat{
		{Label: "Members", Value: strconv.Itoa(len(ms))},
		{Label: "On duty", Value: strconv.Itoa(on), Note: fmt.Sprintf("%d in space", space)},
		{Label: "Fleets out", Value: strconv.Itoa(len(fleets))},
		{Label: "Wars", Value: strconv.Itoa(len(wars))},
		{Label: "Heads priced", Value: strconv.Itoa(len(heads)), Note: "14 days"},
	})
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "Posture")
	esp.PanelBand(&b, pg.pal, []esp.Panel{m.securityPosturePanel(ctx, pg, corpID), m.securityLadderPanel(ctx, pg, corpID)})
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "War targets")
	m.writeSecurityWars(ctx, &b, pg, corpID, wars)
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "Bounty board")
	b.WriteString(pg.soft("Any capsuleer collects a bounty: bring one of these pilots down and CONCORD pays whoever dealt the damage.") + "<br>\n")
	esp.PanelBand(&b, pg.pal, []esp.Panel{m.securityHeadsPanel(ctx, pg, heads), m.securityHuntsPanel(ctx, pg, fleets)})
	b.WriteString("<br>\n")
	esp.PanelBand(&b, pg.pal, []esp.Panel{m.securityTakingsPanel(ctx, pg, corpID), m.securityPatrolPanel(ctx, pg, corpID, viewer)})
	b.WriteString("<br>\n")

	if len(fleets) > 0 {
		esp.Section(&b, pg.pal, "Combat fleets")
		m.writeFleetsPanel(ctx, &b, pg, fleets, pr.Present, viewer, gangsta.GangOf(viewer), 0)
		b.WriteString("<br>\n")
	}

	esp.Section(&b, pg.pal, "The department")
	m.writeDeptRoster(ctx, &b, pg, pr, armSecurity, viewer)
	m.writeArmRequests(ctx, &b, pg, corpID, armSecurity)
	return b.String()
}

// ── posture ─────────────────────────────────────────────────────────────────

// securityPosturePanel is what the wing is for and how much of the
// corporation's effort it gets: the doctrine, the directive's combat shares,
// and the war footing the desk last measured.
func (m *Mod) securityPosturePanel(ctx context.Context, pg igbPage, corpID int64) esp.Panel {
	p := esp.Panel{Title: "Doctrine", Align: []string{"", ""}, Width: []string{"130", ""}}
	col := armColor(armSecurity)
	doc, wing, known := m.corpDoctrineOf(ctx, corpID)
	if known {
		p.Rows = append(p.Rows, []string{pg.dim3("Rules of engagement"),
			pg.tint(col, esp.IGBText(doc.Label())) + "<br>" + pg.soft(esp.IGBText(doc.Blurb()))})
		if doc != DoctrineNone {
			p.Rows = append(p.Rows, []string{pg.dim3("Wing"),
				esp.Bar(wing, 20, col) + " " + pg.soft(fmt.Sprintf("%.0f%% of the corporation may fly it", wing*100))})
		}
	} else {
		p.Rows = append(p.Rows, []string{pg.dim3("Rules of engagement"), pg.soft("set by the corporation's own pilots")})
	}
	if dir, _, err := m.loadCorpDirective(ctx, corpID); err == nil && len(dir) > 0 {
		var total float64
		for _, w := range dir {
			total += max(w, 0)
		}
		for _, a := range []struct{ act, label string }{
			{ActCombat, "Combat effort"}, {ActSecurity, "Patrol effort"}, {ActPiracy, "Piracy effort"},
		} {
			w := dir[a.act]
			if w <= 0 || total <= 0 {
				continue
			}
			p.Rows = append(p.Rows, []string{pg.dim3(a.label),
				esp.Bar(w/total, 20, col) + " " + pg.soft(fmt.Sprintf("%.0f%% of what the CEO asks for", 100*w/total))})
		}
	}
	demand, war := corpSeatDemand(corpID)
	footing := pg.tint(pg.pal.Green, "peacetime")
	if war {
		footing = pg.tint(pg.pal.Red, "war footing")
	}
	p.Rows = append(p.Rows, []string{pg.dim3("Footing"),
		footing + " " + esp.Bar(demand[portSecurity], 20, col) + " " + pg.soft("call on the wing")})
	return p
}

// ladderFacts is what the desk's ladder is read from.
type ladderFacts struct {
	SiegeSys, AllianceSys int64
	SiegeWhy, AllianceWhy string
	War                   bool
	WarEnemyOut           int // enemy pilots out in space, from the hunters' census
	WarEnemySystems       int // systems the enemy works or keeps an address in
	Distressed            int
}

// ladderRung is one step of the desk's ladder as the panel draws it.
type ladderRung struct {
	Name    string
	Detail  string
	Applies bool
	Active  bool
}

// securityLadderRungs is the Director of Security's order of business, top first, as
// securityDesk walks it: a siege outranks an alliance call, then the war, then the belts;
// with none the wing stands down. The first rung that applies is the one the desk is on. Pure.
func securityLadderRungs(f ladderFacts) []ladderRung {
	rungs := []ladderRung{
		{Name: "A siege", Applies: f.SiegeSys != 0, Detail: f.SiegeWhy},
		{Name: "An alliance call to arms", Applies: f.AllianceSys != 0, Detail: f.AllianceWhy},
		{Name: "The war", Applies: f.War && (f.WarEnemyOut > 0 || f.WarEnemySystems > 0)},
		{Name: "Our belts", Applies: f.Distressed > 0},
		{Name: "Stand down", Applies: true, Detail: "nothing calls for the wing; members fly their own rotations"},
	}
	switch {
	case !f.War:
		rungs[2].Detail = "not at war, or no war worth fighting"
	case f.WarEnemyOut > 0:
		rungs[2].Detail = fmt.Sprintf("go where the enemy is: %d of their pilots out in space", f.WarEnemyOut)
	case f.WarEnemySystems > 0:
		rungs[2].Detail = fmt.Sprintf("hunt the enemy where they work (%d known system(s))", f.WarEnemySystems)
	default:
		rungs[2].Detail = "at war, but nowhere to find the enemy"
	}
	if f.Distressed > 0 {
		rungs[3].Detail = fmt.Sprintf("%d system(s) where our miners are being chased off", f.Distressed)
	} else {
		rungs[3].Detail = "no miner of ours is being chased off"
	}
	if rungs[0].Detail == "" {
		rungs[0].Detail = "no tower of ours or theirs under the gun"
	}
	if rungs[1].Detail == "" {
		rungs[1].Detail = "no alliance member is calling for help"
	}
	for i := range rungs {
		if rungs[i].Applies {
			rungs[i].Active = true
			break
		}
	}
	return rungs
}

// securityLadderPanel draws the ladder and the standing order it produced.
func (m *Mod) securityLadderPanel(ctx context.Context, pg igbPage, corpID int64) esp.Panel {
	f := ladderFacts{}
	f.SiegeSys, f.SiegeWhy = m.siegeOrderFor(ctx, corpID)
	f.AllianceSys, f.AllianceWhy = m.allianceDefendOrderFor(ctx, corpID)
	_, f.War = corpSeatDemand(corpID)
	f.WarEnemySystems = len(relWarEnemySystemsOf(corpID))
	if f.War {
		for _, n := range m.warEnemyCensus(ctx, corpID) {
			f.WarEnemyOut += n
		}
	}
	f.Distressed = len(m.distressedBeltSystems(ctx, corpID))

	p := esp.Panel{Title: "The desk's reasoning", Note: "top rung first", Align: []string{"", ""}, Width: []string{"36", ""}}
	for _, rg := range securityLadderRungs(f) {
		mark, name := pg.soft("&middot;"), pg.soft(esp.IGBText(rg.Name))
		shade := ""
		switch {
		case rg.Active:
			mark, name, shade = pg.tint(armColor(armSecurity), "&gt;&gt;"), pg.accent(esp.IGBText(rg.Name))+" "+pg.dim("now"), pg.pal.Now
		case rg.Applies:
			name = igbF3(esp.IGBText(rg.Name))
		}
		p.Rows = append(p.Rows, []string{mark, name + "<br>" + pg.soft(esp.IGBText(rg.Detail))})
		p.Shades = append(p.Shades, shade)
	}
	if o := m.corpFleetOrderFor(ctx, corpID, portSecurity); o != nil && o.SystemID != 0 {
		who := "the Director of Security"
		if o.Source == fleetOrderHuman {
			who = "the chair"
		}
		p.Rows = append(p.Rows, []string{pg.dim3("Order"),
			"The wing is sent to " + m.systemCell(ctx, o.SystemID) + "<br>" +
				pg.soft(esp.IGBText(o.Reason)) + "<br>" +
				pg.dim(esp.IGBText(who)+" &middot; "+igbAgo(o.PostedAt))})
	} else {
		p.Rows = append(p.Rows, []string{pg.dim3("Order"), pg.soft("No standing order for the wing.")})
	}
	p.Shades = append(p.Shades, "")
	return p
}

// ── wars ────────────────────────────────────────────────────────────────────

// securityWar is one live war of the corporation (or its alliance), from our
// side of it.
type securityWar struct {
	WarID      int64
	Enemy      int64 // the owner on the other side: a corporation or an alliance
	EnemyName  string
	EnemyLogo  int64
	WeDeclared bool
	Hostile    bool
	Declared   int64 // unix
	Motive     string
	BillsPaid  float64
}

// securityWars lists the active wars this corporation or its alliance is a party to, the
// motive taken from the CEO's war book where it has one. It reads the wars table itself:
// evedb.ListWars walks every war ever declared, and this page wants only the live ones.
func (m *Mod) securityWars(ctx context.Context, corpID int64) []securityWar {
	aid := m.corpAllianceID(ctx, corpID)
	me := []any{corpID, corpID}
	if aid != 0 {
		me[1] = aid
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT w.warID, w.declaredByID, w.againstID, w.timeDeclared, w.timeFinished, w.retracted, w.mutual,
		       COALESCE(dc.corporationName, da.allianceName, ''), COALESCE(ac.corporationName, aa.allianceName, ''),
		       COALESCE(dc.corporationID, da.executorCorpID, 0), COALESCE(ac.corporationID, aa.executorCorpID, 0)
		  FROM wars w
		  LEFT JOIN corporation dc ON dc.corporationID = w.declaredByID
		  LEFT JOIN alliances da ON da.allianceID = w.declaredByID
		  LEFT JOIN corporation ac ON ac.corporationID = w.againstID
		  LEFT JOIN alliances aa ON aa.allianceID = w.againstID
		 WHERE w.timeFinished IS NULL AND (w.declaredByID IN (?, ?) OR w.againstID IN (?, ?))
		 ORDER BY w.timeDeclared DESC LIMIT 30`, me[0], me[1], me[0], me[1])
	if err != nil {
		return nil
	}
	var all []evedb.WarView
	for rows.Next() {
		var v evedb.WarView
		if rows.Scan(&v.WarID, &v.DeclaredByID, &v.AgainstID, &v.TimeDeclared, &v.TimeFinished, &v.Retracted, &v.Mutual,
			&v.DeclaredByName, &v.AgainstName, &v.DeclaredLogo, &v.AgainstLogo) == nil {
			all = append(all, v)
		}
	}
	rows.Close()
	book := map[int64]warBookRow{}
	if rows, err := m.ceoWarBook(ctx, corpID); err == nil {
		for _, r := range rows {
			book[r.WarID] = r
		}
	}
	isMe := func(id int64) bool { return id == corpID || (aid != 0 && id == aid) }
	nowTicks := uint64(win32Now())
	var out []securityWar
	for _, w := range all {
		sw := securityWar{WarID: w.WarID, Hostile: relWarHostile(w.War, nowTicks), Declared: win32ToUnix(w.TimeDeclared)}
		switch {
		case isMe(w.DeclaredByID):
			sw.WeDeclared, sw.Enemy, sw.EnemyName, sw.EnemyLogo = true, w.AgainstID, w.AgainstName, w.AgainstLogo
		case isMe(w.AgainstID):
			sw.Enemy, sw.EnemyName, sw.EnemyLogo = w.DeclaredByID, w.DeclaredByName, w.DeclaredLogo
		default:
			continue
		}
		if r, ok := book[w.WarID]; ok {
			sw.Motive, sw.BillsPaid = r.Motive, r.BillsPaid
		}
		out = append(out, sw)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Declared > out[j].Declared })
	return out
}

// warMotiveGloss is why a war was declared, in the CEO's words.
func warMotiveGloss(motive string, weDeclared bool) string {
	if !weDeclared {
		return "they declared on us"
	}
	switch motive {
	case warMotiveGrievance:
		return "a grievance: they have hurt us"
	case warMotiveGank:
		return "they ganked ours in policed space"
	case warMotivePredation:
		return "they are worth taking from"
	case warMotiveCompetition:
		return "they work the ground we work"
	case warMotiveAlly:
		return "an ally asked us in"
	case warMotiveDefence:
		return "to stop them hitting us"
	case warMotiveContract:
		return "a contract: we were hired"
	}
	return "declared by hand"
}

// warStanding words a war's ledger: ahead once what it destroyed covers what
// it lost plus what it cost, behind while it is under that line. Pure.
func warStanding(killed, lost, cost float64) (string, float64) {
	owed := lost + cost
	switch {
	case killed+owed <= 0:
		return "no blood yet", 0
	case owed <= 0 || killed >= owed:
		return "ahead", 1
	case killed >= 0.8*owed:
		return "even", killed / owed
	}
	return "behind", killed / owed
}

// warCensusGrid is the wars grid: enemy logo, enemy, the ledger, where they are.
var (
	warGridAlign = []string{"", "", "", ""}
	warGridWidth = []string{"36", "", "190", "190"}
)

// writeSecurityWars draws one row per war: who and why, how it is going, and
// where the enemy is out in space right now.
func (m *Mod) writeSecurityWars(ctx context.Context, b *strings.Builder, pg igbPage, corpID int64, wars []securityWar) {
	p := esp.Panel{Title: "Wars", Align: warGridAlign, Width: warGridWidth,
		Note:  "killed against lost, in green and red",
		Empty: "The corporation is at peace."}
	census := m.targetCensusNow(ctx)
	for _, w := range wars {
		who := igbF3(esp.CorpLink(w.EnemyLogo, esp.IGBText(w.EnemyName)))
		state := pg.dim("arming")
		if w.Hostile {
			state = pg.tint(pg.pal.Red, "shooting")
		}
		who += " " + state + "<br>" + pg.soft(esp.IGBText(warMotiveGloss(w.Motive, w.WeDeclared)))
		weeks := int((clock.Unix() - w.Declared) / (7 * 86400))
		who += "<br>" + pg.dim(fmt.Sprintf("since %s &middot; week %d", ycDate(time.Unix(w.Declared, 0)), weeks+1))
		if mc := m.warMercLine(ctx, w.WarID, corpID); mc != "" {
			who += "<br>" + pg.soft(esp.IGBText(mc))
		}

		killed, lost := m.warScore(ctx, corpID, w.Enemy, w.Declared)
		var cost float64
		if w.WeDeclared {
			_ = m.db.QueryRowContext(ctx,
				`SELECT COALESCE(SUM(spent), 0) FROM rolebot_corp_commit WHERE refKind = 'war' AND refID = ?`,
				w.WarID).Scan(&cost)
			cost = max(cost, w.BillsPaid)
		}
		verdict, frac := warStanding(killed, lost, cost)
		vcol := pg.pal.Amber
		switch verdict {
		case "ahead":
			vcol = pg.pal.Green
		case "behind":
			vcol = pg.pal.Red
		}
		ledger := pg.ratioBar(killed, lost, 20, pg.pal.Green, pg.pal.Red) + "<br>" +
			pg.tint(vcol, esp.IGBText(verdict))
		if verdict != "no blood yet" {
			ledger += " " + esp.Bar(frac, 12, vcol) + " " + pg.dim("to break even")
		}

		p.Rows = append(p.Rows, []string{esp.CorpLogo(w.EnemyLogo, 32), who, ledger,
			m.warSightings(ctx, pg, census, w.Enemy)})
		p.Shades = append(p.Shades, "")
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{p}})
	if len(wars) > 0 {
		if o := m.corpFleetOrderFor(ctx, corpID, portSecurity); o != nil && o.SystemID != 0 {
			b.WriteString(pg.soft("The combat wing is ordered to ") + m.systemCell(ctx, o.SystemID) +
				pg.soft(` &mdash; x-up for a seat on <a href="`+esp.IGBHost()+`/corp/ops">Operations</a>.`) + "<br>\n")
		}
	}
}

// warSightings is where an enemy has pilots out in space right now: the
// three busiest systems from the hunters' census.
func (m *Mod) warSightings(ctx context.Context, pg igbPage, census *targetCensus, enemy int64) string {
	if census == nil {
		return pg.soft("&mdash;")
	}
	corps := map[int64]bool{enemy: true}
	if rows, err := m.db.QueryContext(ctx, `SELECT corporationID FROM corporation WHERE allianceID = ?`, enemy); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				corps[id] = true
			}
		}
		rows.Close()
	}
	counts := warEnemyCounts(census.bySys, corps)
	type seen struct {
		sys int64
		n   int
	}
	var list []seen
	for sys, n := range counts {
		list = append(list, seen{sys, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].sys < list[j].sys
	})
	if len(list) == 0 {
		return pg.soft("none of them out")
	}
	var parts []string
	for i, s := range list {
		if i == 3 {
			break
		}
		parts = append(parts, m.systemCell(ctx, s.sys)+" "+pg.dim(fmt.Sprintf("%d out", s.n)))
	}
	return strings.Join(parts, "<br>")
}

// warMercLine names the mercenaries on a war: the outfit this corporation
// hired, or the client that hired it. "" when nobody is under contract.
func (m *Mod) warMercLine(ctx context.Context, warID, corpID int64) string {
	var client, merc int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT clientCorpID, mercCorpID FROM rolebot_merc_contracts
		 WHERE warID = ? AND status = 'active' LIMIT 1`, warID).Scan(&client, &merc); err != nil {
		return ""
	}
	switch corpID {
	case client:
		return "mercenaries hired: " + m.corpNameOf(ctx, merc)
	case merc:
		return "fighting it for " + m.corpNameOf(ctx, client)
	}
	return ""
}

// ── bounties ────────────────────────────────────────────────────────────────

// securityHead is one pilot the corporation put a price on.
type securityHead struct {
	CharID  int64
	FiledAt int64
	Bounty  float64 // the head's live bounty, public on its Show Info
	Filings int
}

// securityHeads lists the pilots the corporation filed bounties on in the last
// two weeks whose price still stands, highest first.
func (m *Mod) securityHeads(ctx context.Context, corpID int64) []securityHead {
	rows, err := m.db.QueryContext(ctx, `
		SELECT l.counterID, MAX(l.at), COUNT(*), COALESCE(c.bounty, 0)
		  FROM rolebot_corp_ledger l
		  JOIN character_ c ON c.characterID = l.counterID
		 WHERE l.corporationID = ? AND l.line = ? AND l.at >= ? AND c.bounty > 0
		 GROUP BY l.counterID, c.bounty
		 ORDER BY c.bounty DESC LIMIT 15`, corpID, lineBounties, clock.Unix()-14*86400)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []securityHead
	for rows.Next() {
		var h securityHead
		if rows.Scan(&h.CharID, &h.FiledAt, &h.Filings, &h.Bounty) == nil {
			out = append(out, h)
		}
	}
	return out
}

// securityHeadsPanel draws the heads with a price on them.
func (m *Mod) securityHeadsPanel(ctx context.Context, pg igbPage, heads []securityHead) esp.Panel {
	p := esp.Panel{Title: "Heads we put a price on", Align: []string{"", "", "right"}, Width: []string{"36", "", "110"},
		Empty: "No price of ours stands on anyone."}
	for _, h := range heads {
		icon, name := m.pilotCells(ctx, h.CharID, 32)
		where := pg.soft("whereabouts unknown")
		var sys int64
		_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(solarSystemID, 0) FROM character_ WHERE characterID = ?`, h.CharID).Scan(&sys)
		if sys != 0 {
			where = pg.soft("last seen ") + m.systemCell(ctx, sys)
		}
		filed := pg.dim(fmt.Sprintf("filed %s", igbAgo(h.FiledAt)))
		if h.Filings > 1 {
			filed = pg.dim(fmt.Sprintf("filed %d times, last %s", h.Filings, igbAgo(h.FiledAt)))
		}
		p.Rows = append(p.Rows, []string{icon, igbF3(name) + "<br>" + where + "<br>" + filed,
			igbF3(esp.ISKShort(h.Bounty)) + " " + pg.dim("ISK")})
	}
	return p
}

// securityHuntsPanel is the corporation's fleets that are running a bounty
// hunt: who they are after and when they reach them.
func (m *Mod) securityHuntsPanel(ctx context.Context, pg igbPage, fleets []*Gang) esp.Panel {
	p := esp.Panel{Title: "On the hunt", Align: []string{"", "", "right"}, Width: []string{"36", "", "90"},
		Empty: "No fleet of ours is running anyone down."}
	for _, g := range fleets {
		t := g.State.HuntTargetCharID
		if t == 0 {
			continue
		}
		icon, name := m.pilotCells(ctx, t, 32)
		eta := g.State.HuntArriveAt
		if eta == 0 {
			eta = g.State.HuntETA
		}
		p.Rows = append(p.Rows, []string{icon,
			igbF3(name) + "<br>" + pg.soft("hunted by "+esp.IGBText(g.Name)),
			igbF3(igbIn(eta))})
	}
	return p
}

// securityTakings is what the wing brought home in a window, by kind.
type securityTakings struct {
	HeadBounties, RatTax, Ransoms, GankLoot, Filed float64
}

// securityTakingsFor reads the takings since a time.
func (m *Mod) securityTakingsFor(ctx context.Context, corpID, since int64) securityTakings {
	var t securityTakings
	t.RatTax = m.corpLineIn(ctx, corpID, lineTax, since)
	t.GankLoot = m.corpLineIn(ctx, corpID, lineGankLoot, since)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(-SUM(amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND line = ? AND at >= ? AND amount < 0`,
		corpID, lineBounties, since).Scan(&t.Filed)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(isk), 0) FROM rolebot_corp_incidents
		 WHERE corporationID = ? AND kind = ? AND side = ? AND at >= ?`,
		corpID, incRobbery, relSideAggressor, since).Scan(&t.Ransoms)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(j.amount), 0)
		  FROM character_ c
		  JOIN market_journal j ON j.characterID = c.characterID
		 WHERE c.corporationID = ? AND j.refTypeID = ? AND j.amount > 0
		   AND j.reason LIKE 'Bounty collected%' AND j.transDate >= ?`,
		corpID, journalBountyRef, evedb.UnixToWin32(since)).Scan(&t.HeadBounties)
	return t
}

// securityTakingsPanel draws the week's takings against each other, and the bounty purse
// filed against collected, as bars.
func (m *Mod) securityTakingsPanel(ctx context.Context, pg igbPage, corpID int64) esp.Panel {
	t := m.securityTakingsFor(ctx, corpID, clock.Unix()-7*86400)
	p := esp.Panel{Title: "What the wing brought home", Note: "last 7 days, against each other",
		Align: []string{"", ""}, Width: []string{"150", ""}}
	most := max(t.HeadBounties, t.RatTax, t.Ransoms, t.GankLoot)
	col := armColor(armSecurity)
	for _, row := range []struct {
		label string
		v     float64
	}{
		{"Bounties on heads", t.HeadBounties},
		{"Rat bounties (corp tax)", t.RatTax},
		{"Ransoms and tolls", t.Ransoms},
		{"Gank loot", t.GankLoot},
	} {
		bar := relBar(row.v, most, 24, col)
		if row.v <= 0 {
			bar += " " + pg.dim("none")
		}
		p.Rows = append(p.Rows, []string{pg.dim3(row.label), bar})
	}
	if t.Filed > 0 || t.HeadBounties > 0 {
		p.Rows = append(p.Rows, []string{pg.dim3("Bounty purse"),
			pg.ratioBar(t.HeadBounties, t.Filed, 24, pg.pal.Green, pg.pal.Amber) + "<br>" +
				pg.soft("collected by members, against prices we filed")})
	}
	return p
}

// securityPatrolPanel is the viewer's own way in: their patrol assignments,
// when the desk hands them out, and otherwise where to start.
func (m *Mod) securityPatrolPanel(ctx context.Context, pg igbPage, corpID, viewer int64) esp.Panel {
	p := esp.Panel{Title: "Your part", Align: []string{""}}
	host := esp.IGBHost()
	if assignmentsOn() && viewer != 0 {
		as, _ := m.queryAssignments(ctx, `WHERE corporationID = ? AND characterID = ? AND kind = ? AND status IN (?, ?)
			ORDER BY offeredAt DESC LIMIT 5`, corpID, viewer, assignPatrol, asOffered, asAccepted)
		for _, a := range as {
			line := fmt.Sprintf("Patrol: collect %d bounties", a.Qty)
			if a.SystemID != 0 {
				line += " around " + m.systemLabel(ctx, a.SystemID)
			}
			state := pg.tint(pg.pal.Amber, "offered") + " " + pg.soft("&mdash; the letter carries the link")
			if a.Status == asAccepted {
				state = pg.tint(pg.pal.Green, fmt.Sprintf("%d of %d", a.Progress, a.Qty)) + " " + pg.soft(igbIn(a.ExpiresAt))
			}
			p.Rows = append(p.Rows, []string{igbF3(esp.IGBText(line)) + "<br>" + state})
		}
	}
	if assignmentsOn() {
		p.Rows = append(p.Rows, []string{pg.soft(`Patrol jobs, when Security needs the belts swept, are on ` +
			`<a href="` + host + assignBoardPortalPath + `">Assignments</a>.`)})
	}
	p.Rows = append(p.Rows,
		[]string{pg.soft(`Take a seat in a combat fleet on <a href="` + host + `/corp/ops">Operations</a>.`)},
		[]string{pg.soft("Hunt a priced head yourself: the bounty is paid to you, not the corporation.")},
		[]string{pg.soft("Rats you kill in our space pay the corporation its tax; the rest is yours.")})
	return p
}
