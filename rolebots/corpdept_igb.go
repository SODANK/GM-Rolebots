package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gurimoon/esp"
	"gurimoon/server/gangsta"
)

// The Mining and Infrastructure department pages of the Corporate Portal, and
// the department roster every department page draws. Like the R&D page
// (corprnd_igb.go) they are readable by every member; what a member can take is
// what is posted at their own base. Each gathers what the corporation already
// knows about the department on one page: the fleets and ore office for Mining,
// the starbases and supply runs for Infrastructure.

// deptRosterGrid is the roster panel's grid: face, who, doing, base.
var (
	deptRosterAlign = []string{"", "", "", ""}
	deptRosterWidth = []string{"36", "", "", "170"}
)

// memberBases is every member's Base, by character.
func (m *Mod) memberBases(ctx context.Context, corpID int64) map[int64]int64 {
	out := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.characterID, COALESCE(r.baseID, 0) FROM chrCorporationRoles r
		  JOIN character_ c ON c.characterID = r.characterID
		 WHERE c.corporationID = ?`, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, base int64
		if rows.Scan(&id, &base) == nil {
			out[id] = base
		}
	}
	return out
}

// writeDeptRoster is one department's people: who, what they are doing, and
// where they are posted. Present members first.
func (m *Mod) writeDeptRoster(ctx context.Context, b *strings.Builder, pg igbPage, pr *portalRoster, a corpArm, viewer int64) {
	const maxRows = 20
	ms := pr.ByArm[a]
	on, space := onDuty(ms)
	p := esp.Panel{Title: armIcon(a, pr.CorpID, 24) + " " + armName(a),
		Note:  fmt.Sprintf("%d member(s) &middot; %d on duty, %d in space", len(ms), on, space),
		Align: deptRosterAlign, Width: deptRosterWidth,
		Empty: "Nobody is in " + esp.IGBText(armLabel(a)) + " yet."}
	bases := m.memberBases(ctx, pr.CorpID)
	for i, pm := range ms {
		if i == maxRows {
			p.Rows = append(p.Rows, []string{"", pg.soft(fmt.Sprintf("&hellip; and %d more", len(ms)-maxRows)), "", ""})
			p.Shades = append(p.Shades, "")
			break
		}
		who := igbF3(esp.PilotLink(pm.CharID, esp.IGBText(pm.Name)))
		if pm.Tier == armTierHead {
			who += " " + pg.tint(armColor(a), "head")
		}
		if pm.Title != "" {
			who += "<br>" + pg.soft(esp.IGBText(pm.Title))
		} else if !pm.OnRoster {
			who += "<br>" + pg.soft("capsuleer")
		}
		doing := pg.soft("off duty")
		if pm.Present {
			doing = colored(armColor(a), esp.IGBText(m.memberDoing(ctx, pm)))
		}
		base := pg.soft("&mdash;")
		if st := bases[pm.CharID]; st != 0 {
			base = igbF3(esp.IGBText(stationNameOf(ctx, m, st)))
		}
		shade := ""
		if pm.CharID == viewer {
			shade = igbShadeMine
		}
		p.Rows = append(p.Rows, []string{esp.Portrait(pm.CharID, 32), who, doing, base})
		p.Shades = append(p.Shades, shade)
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{p}})
}

// haulBoardPanel is the corporation's haul board (poshaul.go): the legs open
// or being carried, with the wage each one pays. Drawn on Infrastructure as
// the supply runs and on Trade as the freight. Answers how many it drew.
func (m *Mod) haulBoardPanel(ctx context.Context, pg igbPage, corpID int64, title, empty string) (esp.Panel, int) {
	legs := esp.Panel{Title: title, Align: []string{"", "", "right"}, Width: []string{"", "150", "120"}, Empty: empty}
	open := 0
	lr, err := m.db.QueryContext(ctx, `
		SELECT kind, fromStationID, toStationID, toSystemID, m3, wage, carrierID, status
		  FROM rolebot_corp_haul
		 WHERE corporationID = ? AND status IN ('open', 'claimed')
		 ORDER BY postedAt LIMIT 20`, corpID)
	if err != nil {
		return legs, 0
	}
	defer lr.Close()
	for lr.Next() {
		var kind, status string
		var from, to, toSys, carrier int64
		var m3, wage float64
		if lr.Scan(&kind, &from, &to, &toSys, &m3, &wage, &carrier, &status) != nil {
			continue
		}
		open++
		dest := m.systemCell(ctx, toSys)
		if to != 0 {
			dest = esp.IGBText(stationNameOf(ctx, m, to))
		}
		route := igbF3(esp.IGBText(kind)+" "+pg.soft(fmt.Sprintf("%.0f m3", m3))) + "<br>" +
			pg.soft(esp.IGBText(stationNameOf(ctx, m, from))+" &rarr; "+dest)
		who := pg.tint(pg.pal.Amber, "waiting")
		if carrier != 0 {
			who = igbF3(esp.IGBText(m.charNameOr(ctx, carrier)))
		}
		legs.Rows = append(legs.Rows, []string{route, who, igbF3(esp.ISKShort(wage)) + " " + pg.dim("ISK")})
	}
	return legs, open
}

// ── Mining ──────────────────────────────────────────────────────────────────

// handleIGBCorpMining serves /corp/mining.
func (m *Mod) handleIGBCorpMining(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	charID, corpID := m.portalViewer(ctx, r)
	if corpID == 0 {
		esp.WriteCorpPortalPage(w, r, "mining", igbF3("Open this page from your own terminal to see your corporation's mining."))
		return
	}
	esp.WriteCorpPortalPage(w, r, "mining", m.miningBody(ctx, r, corpID, charID))
}

// miningBody is the page: the figures, the mining fleets with a seat in
// each, where the ore goes, and the department.
func (m *Mod) miningBody(ctx context.Context, r *http.Request, corpID, viewer int64) string {
	pg := m.igbPageFor(r)
	host := esp.IGBHost()
	pr := m.portalRosterFor(ctx, corpID)
	var fleets []*Gang
	for _, g := range pr.Fleets {
		if armOfSeatActivity(g.Activity) == armMining {
			fleets = append(fleets, g)
		}
	}
	oreAt := m.corpFleetOreStation(ctx, corpID)
	ms := pr.ByArm[armMining]
	on, space := onDuty(ms)

	var b strings.Builder
	b.WriteString(pg.soft(`The Mining department: the fleets out on the belts and where their ore goes. A seat in a fleet `+
		`is requested here or on <a href="`+host+`/corp/ops">Operations</a>; joining Mining is done on the `+
		`<a href="`+host+`/corp/">Overview</a>.`) + "<br><br>\n")
	ore := "&mdash;"
	if oreAt != 0 {
		ore = esp.IGBText(stationNameOf(ctx, m, oreAt))
	}
	pg.statStrip(&b, []igbStat{
		{Label: "Members", Value: strconv.Itoa(len(ms))},
		{Label: "On duty", Value: strconv.Itoa(on), Note: fmt.Sprintf("%d in space", space)},
		{Label: "Fleets out", Value: strconv.Itoa(len(fleets))},
		{Label: "Ore office", Value: "<font size=3>" + ore + "</font>"},
	})
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "Mining fleets")
	m.writeFleetsPanel(ctx, &b, pg, fleets, pr.Present, viewer, gangsta.GangOf(viewer), 0)
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "Where the ore goes")
	desk := esp.Panel{Title: "The ore desk", Align: []string{"", ""}, Width: []string{"150", ""}}
	if oreAt != 0 {
		desk.Rows = append(desk.Rows, []string{pg.dim3("Delivered to"),
			igbF3(esp.IGBText(stationNameOf(ctx, m, oreAt))) + " " + pg.soft("&mdash; the corporation hangar")})
	} else {
		desk.Rows = append(desk.Rows, []string{pg.dim3("Delivered to"), pg.soft("no ore office yet")})
	}
	if facts, err := getAnyCorpFacts(ctx, m.db, corpID); err == nil && facts != nil {
		bounds := oreBoundsFor(facts.Policy.Tags)
		use := "keeps its ore and refines it at the factory to build"
		if bounds.Max < 0.5 {
			use = "sells most of its ore where it lies"
		}
		desk.Rows = append(desk.Rows, []string{pg.dim3("The desk"), igbF3(use)})
	}
	if factory := m.corpFactoryStation(ctx, corpID); factory != 0 {
		desk.Rows = append(desk.Rows, []string{pg.dim3("The factory"), igbF3(esp.IGBText(stationNameOf(ctx, m, factory)))})
	}
	esp.PanelBand(&b, pg.pal, []esp.Panel{desk})
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "The department")
	m.writeDeptRoster(ctx, &b, pg, pr, armMining, viewer)
	m.writeArmRequests(ctx, &b, pg, corpID, armMining)
	return b.String()
}

// ── Infrastructure ──────────────────────────────────────────────────────────

// handleIGBCorpInfra serves /corp/infra.
func (m *Mod) handleIGBCorpInfra(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	charID, corpID := m.portalViewer(ctx, r)
	if corpID == 0 {
		esp.WriteCorpPortalPage(w, r, "infra", igbF3("Open this page from your own terminal to see your corporation's starbases."))
		return
	}
	esp.WriteCorpPortalPage(w, r, "infra", m.infraBody(ctx, r, corpID, charID))
}

// infraBody is the page: the figures, the starbases, the supply runs on the
// board, and the department.
func (m *Mod) infraBody(ctx context.Context, r *http.Request, corpID, viewer int64) string {
	pg := m.igbPageFor(r)
	pr := m.portalRosterFor(ctx, corpID)

	towers := esp.Panel{Title: "Starbases", Align: []string{"", "", "", "right"}, Width: []string{"36", "", "150", "130"},
		Empty: "The corporation runs no starbase."}
	running, planned := 0, 0
	rows, err := m.db.QueryContext(ctx, `
		SELECT towerTypeID, systemID, purpose, size, status, fuelerID, onlineAt, plannedAt
		  FROM rolebot_corp_towers
		 WHERE corporationID = ? AND status <> 'gone'
		 ORDER BY status = 'running' DESC, onlineAt DESC LIMIT 30`, corpID)
	if err == nil {
		for rows.Next() {
			var typeID, sysID, fueler, onlineAt, plannedAt int64
			var purpose, size, status string
			if rows.Scan(&typeID, &sysID, &purpose, &size, &status, &fueler, &onlineAt, &plannedAt) != nil {
				continue
			}
			if status == "running" {
				running++
			} else {
				planned++
			}
			what := igbF3(esp.IGBText(m.typeNameOr(ctx, typeID))) + "<br>" +
				pg.soft(esp.IGBText(purpose)+" &middot; size "+esp.IGBText(size))
			since := pg.soft("planned " + igbAgo(plannedAt))
			if onlineAt > 0 {
				since = pg.soft("online " + igbAgo(onlineAt))
			}
			fuel := pg.soft("no fueler")
			if fueler != 0 {
				fuel = igbF3(esp.IGBText(m.charNameOr(ctx, fueler))) + "<br>" + pg.soft("keeps it fuelled")
			}
			towers.Rows = append(towers.Rows, []string{esp.ShipIcon(typeID, 32),
				what + "<br>" + m.systemCell(ctx, sysID), fuel, pg.tint(pg.pal.Up, esp.IGBText(status)) + "<br>" + since})
		}
		rows.Close()
	}

	legs, open := m.haulBoardPanel(ctx, pg, corpID, "Supply runs on the board", "No supply run is waiting.")

	ms := pr.ByArm[armInfrastructure]
	on, _ := onDuty(ms)
	var b strings.Builder
	b.WriteString(pg.soft("The Infrastructure department: the corporation's starbases and the supply runs that keep them "+
		"fuelled and armed.") + "<br><br>\n")
	pg.statStrip(&b, []igbStat{
		{Label: "Members", Value: strconv.Itoa(len(ms)), Note: fmt.Sprintf("%d on duty", on)},
		{Label: "Running", Value: strconv.Itoa(running), Note: "starbases"},
		{Label: "Planned", Value: strconv.Itoa(planned), Note: "starbases"},
		{Label: "Supply runs", Value: strconv.Itoa(open), Note: "on the board"},
	})
	b.WriteString("<br>\n")
	esp.Section(&b, pg.pal, "Starbases")
	esp.PanelGrid(&b, pg.pal, [][]esp.Panel{{towers}})
	b.WriteString("<br>\n")
	esp.Section(&b, pg.pal, "Supply runs")
	esp.PanelGrid(&b, pg.pal, [][]esp.Panel{{legs}})
	b.WriteString("<br>\n")
	esp.Section(&b, pg.pal, "The department")
	m.writeDeptRoster(ctx, &b, pg, pr, armInfrastructure, viewer)
	return b.String()
}
