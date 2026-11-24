package rolebots

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// Security's fleets on the Management page: who may be given the command of one when it is raised, and
// the fleets out now, where each is and what it was sent to do.

// fleetLeader is a member the chair may put in command of a fleet.
type fleetLeader struct {
	CharID     int64
	Name       string
	Leadership int    // the Leadership skill, trained level
	Busy       string // why it cannot lead now, "" when it can
}

// label is a leader as the list shows it: the name, the Leadership skill, and what keeps them. Pure.
func (l fleetLeader) label() string {
	s := fmt.Sprintf("%s (Leadership %d)", l.Name, l.Leadership)
	if l.Busy != "" {
		s += " - " + l.Busy
	}
	return s
}

// sortFleetLeaders puts whoever can lead now first, the best Leadership ahead. Pure.
func sortFleetLeaders(ls []fleetLeader) {
	sort.SliceStable(ls, func(i, j int) bool {
		a, b := ls[i], ls[j]
		if (a.Busy == "") != (b.Busy == "") {
			return a.Busy == ""
		}
		if a.Leadership != b.Leadership {
			return a.Leadership > b.Leadership
		}
		return a.Name < b.Name
	})
}

// fleetLeaders is a division's bots, for the leader list. A capsuleer is not offered: the fleet is
// flown by the command's own orders, and a person takes a seat in it instead (Operations).
func (p *mgPage) fleetLeaders(a corpArm) []fleetLeader {
	var out []fleetLeader
	for _, pm := range p.people().ByArm[a] {
		if !p.m.botFlown(pm.CharID) {
			continue
		}
		l := fleetLeader{CharID: pm.CharID, Name: pm.Name, Leadership: p.m.leadershipOf(p.ctx, pm.CharID)}
		switch {
		case pm.Fleet != "":
			l.Busy = "out with " + pm.Fleet
		case popIsOffline(pm.CharID), tzShortHours(pm.CharID):
			l.Busy = "off duty, or nearly"
		}
		out = append(out, l)
	}
	sortFleetLeaders(out)
	return out
}

// leadershipOf is a pilot's trained Leadership level.
func (m *Mod) leadershipOf(ctx context.Context, charID int64) int {
	lv, _ := m.charSkillLevelFor(ctx, charID, skillLeadership)
	return lv
}

// people is the corporation's roster, read once a request.
func (p *mgPage) people() *portalRoster {
	if p.roster == nil {
		p.roster = p.m.portalRosterFor(p.ctx, p.corpID)
	}
	return p.roster
}

// leaderLine is the leader list of a form.
func (p *mgPage) leaderLine(a corpArm, form string) string {
	cur := p.cur(form, "leader", "")
	sel := `<select name="leader">` + selOpt("", "whoever is best placed", cur == "")
	for _, l := range p.fleetLeaders(a) {
		v := strconv.FormatInt(l.CharID, 10)
		sel += selOpt(v, l.label(), cur == v)
	}
	return p.lineHTML("Led by", sel+`</select>`, "Members of "+armLabel(a)+", with their Leadership skill.")
}

// leaderField reads the leader list: 0 for whoever is best placed.
func (p *mgPage) leaderField(a corpArm) (fleetLeader, string) {
	v := p.q.Get("leader")
	if v == "" {
		return fleetLeader{}, ""
	}
	id, _ := strconv.ParseInt(v, 10, 64)
	for _, l := range p.fleetLeaders(a) {
		if l.CharID == id {
			why := p.m.leaderUnfit(p.ctx, p.corpID, id)
			if why == "" && l.Busy != "" {
				why = l.Name + " is " + l.Busy
			}
			if why != "" {
				return fleetLeader{}, why + ". Name somebody else, or leave it to whoever is best placed."
			}
			return l, ""
		}
	}
	return fleetLeader{}, "Pick a member of " + armLabel(a) + " to lead it."
}

// leaderUnfit is why a member cannot take command of a fleet now, "" when it can. Their hull is the
// desk's to look at (leaderSeat): rating a pilot's fits is too slow for a page.
func (m *Mod) leaderUnfit(ctx context.Context, corpID, charID int64) string {
	name := m.charNameOr(ctx, charID)
	if in, err := evedb.GetCharCorpID(ctx, m.db, charID); err == nil && in != corpID {
		return name + " has left the corporation"
	}
	if !m.botFlown(charID) {
		return name + " is not a pilot that takes command of a fleet"
	}
	if popIsOffline(charID) || tzShortHours(charID) {
		return name + " is off duty, or nearly"
	}
	if g, _, _ := gangForChar(ctx, m.db, charID); g != nil {
		return name + " is out with " + g.Name
	}
	if st, err := loadState(ctx, m.db, charID); err == nil && st != nil && st.Halted {
		return name + " is not flying at the moment"
	}
	return ""
}

// leaderSeat is the seat of the commander the chair named, or why they cannot take it. The formation
// code (formFleetAround) holds the doctrine's own hull and fit tests.
func (m *Mod) leaderSeat(ctx context.Context, corpID, charID int64, activity string) (fcSeat, string) {
	if why := m.leaderUnfit(ctx, corpID, charID); why != "" {
		return fcSeat{}, why
	}
	if ok, err := m.hasSeatShip(ctx, charID, seatActivityFor(activity, gangLeaderRole(activity))); err == nil && !ok {
		return fcSeat{}, m.charNameOr(ctx, charID) + " owns no hull fitted for the commander's seat"
	}
	return fcSeat{corpID: corpID, charID: charID, activity: activity}, ""
}

// ── the fleets out ──────────────────────────────────────────────────────────

// armFleets is the corporation's own fleets of a division that are out.
func armFleets(pr *portalRoster, a corpArm) []*Gang {
	var out []*Gang
	for _, g := range pr.Fleets {
		if g.CorpID == pr.CorpID && armOfSeatActivity(g.Activity) == a {
			out = append(out, g)
		}
	}
	return out
}

// fleetWhere is where a fleet is and where it works: its commander's system with how many are there,
// the system it sweeps or musters for, and the place the chair sent it to.
func (p *mgPage) fleetWhere(g *Gang, aboard int) string {
	ctx, m := p.ctx, p.m
	where, _, sysID := m.memberWhere(ctx, g.LeaderCharID, p.people().Present)
	s := igbF3(where)
	if sysID != 0 && aboard > 0 {
		var there int
		if m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM rolebot_gang_members gm
			  JOIN character_ c ON c.characterID = gm.characterID
			 WHERE gm.gangID = ? AND c.solarSystemID = ?`, g.GangID, sysID).Scan(&there) == nil && there > 0 {
			s += "<br>" + p.pg.dim(fmt.Sprintf("%d of %d in that system", there, aboard))
		}
	}
	switch {
	case g.State.CheckpointSystemID != 0:
		s += "<br>" + p.pg.soft("holding a gate in ") + m.systemCell(ctx, g.State.CheckpointSystemID)
	case g.State.OpPhase == gangOpMuster && g.State.StagingStationID != 0:
		s += "<br>" + p.pg.soft("mustering at "+esp.IGBText(m.stationLabel(ctx, g.State.StagingStationID)))
	case fleetSiteSystem(g) != 0:
		s += "<br>" + p.pg.soft("working ") + m.systemCell(ctx, fleetSiteSystem(g))
	}
	if g.State.OrderAreaID != 0 {
		s += "<br>" + p.pg.soft("sent to the "+esp.IGBText(m.placeName(ctx, g.State.OrderAreaID)))
	}
	return s
}

// writeFleets is a division's fleets that are out: the hull and the commander, who is aboard, where it
// is, and when it comes home, with a way to call back one the viewer raised.
func (p *mgPage) writeFleets(b *strings.Builder, a corpArm) {
	ctx, m := p.ctx, p.m
	fleets := armFleets(p.people(), a)
	raised := map[int64]corpCommand{}
	if len(fleets) > 0 {
		for _, c := range m.listCorpCommands(ctx, p.corpID, 50) {
			if c.Kind == cmdKindFleet && c.Open() {
				raised[c.CommandID] = c
			}
		}
	}
	t := esp.Panel{Title: "Fleets out", Note: "where they are now", Align: []string{"", "", "", "", "right"},
		Width: []string{"36", "36", "", "240", "130"}, Empty: "No fleet of the division is out."}
	for _, g := range fleets {
		mems, _ := gangMembers(ctx, m.db, g.GangID)
		byRole := map[string]int{}
		for _, mem := range mems {
			byRole[mem.Role]++
		}
		hull, hullName := m.shipTypeOf(ctx, g.LeaderCharID)
		face, leader := m.pilotCells(ctx, g.LeaderCharID, 32)
		who := igbF3("<b>"+esp.IGBText(g.Name)+"</b>") + "<br>" +
			p.pg.dim(esp.IGBText(activityLabel(g.Activity))+" &middot; "+plural(len(mems), "pilot", "pilots")+" &middot; "+rosterSummary(byRole)) + "<br>" +
			p.pg.soft("led by ") + igbF3(leader) + " " +
			p.pg.soft(fmt.Sprintf("(Leadership %d), %s", m.leadershipOf(ctx, g.LeaderCharID), esp.IGBText(hullName)))
		c, ordered := raised[g.State.OrderID]
		if ordered {
			who += "<br>" + p.pg.dim("ordered by "+esp.IGBText(m.charNameOr(ctx, c.PosterID)))
		}
		right := p.pg.tint(fleetPhaseColor(p.pg, g), gangStageLabel(g))
		switch {
		case g.State.OpPhase == gangOpMuster:
			right += "<br>" + p.pg.dim("sets out "+igbIn(g.State.MusterDeadline))
		case g.State.OpPhase == gangOpWindDown:
			right += "<br>" + p.pg.dim("settles "+igbIn(g.State.WindDownDeadline))
		case g.ExpiresAt > 0:
			right += "<br>" + p.pg.dim("winds down "+igbIn(g.ExpiresAt))
		}
		id := strconv.FormatInt(g.GangID, 10)
		right += "<br>" + igbF3(`<a href="`+esp.IGBHost()+`/corp/ops?fleet=`+id+`">roster</a>`)
		if ordered && mayCancelCommand(p.scope, c) {
			right += " &nbsp; " + igbF3(p.link(p.div, "withdraw", "form", "cancel", "id", strconv.FormatInt(c.CommandID, 10)))
		}
		t.Rows = append(t.Rows, []string{esp.ShipIcon(hull, 32), face, who, p.fleetWhere(g, len(mems)), right})
		t.Shades = append(t.Shades, fleetShade(g))
	}
	esp.PanelGrid(b, p.pg.pal, [][]esp.Panel{{t}})
	b.WriteString("<br>\n")
}
