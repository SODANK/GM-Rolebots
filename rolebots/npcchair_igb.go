package rolebots

// The Corporate Portal's Chair page (/corp/chair) for an NPC corporation's
// own members (npcchair.go): who runs the corporation, what it asks of them,
// where the combat wing is sent, what it flies and who commands the fleets.
// Registered as a Chartered section, so the core shows it only to members of
// a chaired NPC corporation (esp.CorpPortalChartered).

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gurimoon/esp"
)

const npcChairPortalPath = "/corp/chair"

func (m *Mod) handleIGBCorpChair(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	_, corpID := m.portalViewer(ctx, r)
	if corpID == 0 || !npcChaired(corpID) {
		esp.WriteCorpPortalPage(w, r, "chair", igbF3("Open this page from your own terminal, as a member of a chartered corporation."))
		return
	}
	esp.WriteCorpPortalPage(w, r, "chair", m.npcChairBody(ctx, r, corpID))
}

// npcChairBody is the page.
func (m *Mod) npcChairBody(ctx context.Context, r *http.Request, corpID int64) string {
	pg := m.igbPageFor(r)
	host := esp.IGBHost()
	row, _, _ := m.getNPCChair(ctx, corpID)
	var b strings.Builder
	b.WriteString(pg.soft(`How the corporation is run. A chartered corporation keeps its members working and its `+
		`belts guarded, but it is not yours to steer: that is what a capsuleer corporation is for. `+
		`The ones taking members are on the <a href="`+host+`/corp/">Overview</a>.`) + "<br><br>\n")

	esp.Section(&b, pg.pal, "The chair")
	b.WriteString("<table width=100%>")
	chairRow := func(k, v string) {
		b.WriteString("<tr><td width=200>" + pg.dim(k) + "</td><td>" + igbF3(v) + "</td></tr>")
	}
	chairRow("Chief Executive Officer", esp.IGBText(m.charNameOr(ctx, row.CEOID)))
	head := "nobody yet"
	if row.SecurityHeadID != 0 {
		head = esp.IGBText(m.charNameOr(ctx, row.SecurityHeadID))
	}
	chairRow(portfolioTitle(portSecurity), head)
	if weights, _, err := m.loadCorpDirective(ctx, corpID); err == nil && len(weights) > 0 {
		chairRow("Directive", esp.IGBText(directiveText(weights)))
	}
	if sts := m.npcOfficeStations(ctx, corpID); len(sts) > 0 {
		names := make([]string, len(sts))
		for i, st := range sts {
			names[i] = esp.IGBText(stationNameOf(ctx, m, st))
		}
		chairRow("Offices", strings.Join(names, "<br>"))
	}
	b.WriteString("</table><br>\n")

	esp.Section(&b, pg.pal, "Standing order")
	if o := m.corpFleetOrderFor(ctx, corpID, portSecurity); o != nil && o.SystemID != 0 {
		b.WriteString(igbF3("The combat wing is sent to <b>"+esp.IGBText(m.nameOfSystem(ctx, o.SystemID))+"</b>: "+
			esp.IGBText(o.Reason)+".") + "<br><br>\n")
	} else {
		b.WriteString(pg.dim("No standing order: no member is being chased off a belt near headquarters.") + "<br><br>\n")
	}

	esp.Section(&b, pg.pal, "Doctrine")
	if docs, _, err := m.readFleetDoctrines(ctx, corpID); err == nil && len(docs) > 0 {
		b.WriteString("<table width=100%>")
		for _, d := range docs {
			b.WriteString("<tr><td width=200>" + pg.dim(esp.IGBText(string(d.Kind))) + "</td><td>" +
				igbF3(esp.IGBText(doctrineSummary(d))) + "</td></tr>")
		}
		b.WriteString("</table><br>\n")
	} else {
		b.WriteString(pg.dim("No doctrine yet: fleets fly whatever their pilots own.") + "<br><br>\n")
	}

	esp.Section(&b, pg.pal, "Fleet commanders")
	ranks, _ := m.listCorpRanks(ctx, corpID)
	if len(ranks) == 0 {
		b.WriteString(pg.dim("No seats dealt yet.") + "<br>\n")
		return b.String()
	}
	b.WriteString("<table width=100%>")
	for _, rr := range ranks {
		b.WriteString(fmt.Sprintf("<tr><td width=200>%s</td><td>%s</td></tr>",
			igbF3(esp.IGBText(rr.Name)), pg.dim(esp.IGBText(rr.Activity)+" fleet")))
	}
	b.WriteString("</table>\n")
	return b.String()
}
