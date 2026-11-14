package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gurimoon/esp"
)

// The Requests page (/corp/requests): every open request between the corporation's
// departments, who asked, who does it, and the hangar it goes into (corprequest.go). The
// department pages carry their own half (writeArmRequests). A capsuleer building for the
// corporation reads "deliver to" and picks that division in the S&I window's Output dropdown;
// delivered elsewhere it is still paid and the corporation files it where it was asked for.

const requestsPath = "/corp/requests"

func (m *Mod) handleIGBCorpRequests(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	charID, corpID := m.portalViewer(ctx, r)
	if corpID == 0 {
		esp.WriteCorpPortalPage(w, r, "requests", igbF3("Open this page from your own terminal to see your corporation's requests."))
		return
	}
	esp.WriteCorpPortalPage(w, r, "requests", m.requestsBody(ctx, r, corpID, charID))
}

// requestsBody is the page: the hangars and who owns them, the week's
// envelopes, then what is being made and what is being got.
func (m *Mod) requestsBody(ctx context.Context, r *http.Request, corpID, _ int64) string {
	pg := m.igbPageFor(r)
	work, _ := m.openWorkFor(ctx, corpID)
	reqs := m.openReqSummary(ctx, corpID)

	var b strings.Builder
	b.WriteString(pg.soft(`Every department keeps its own hangar division, and asks the others for what it needs: `+
		`Security its Armory, Sales goods for its shelf, Industry prints from Research. A request names the `+
		`hangar the result goes into. Building one of these yourself? Install it as a corporation job and set `+
		`<b>Output</b> to the division shown. Delivered anywhere else it is still paid, and filed where it was asked for.`) +
		"<br><br>\n")
	pg.statStrip(&b, []igbStat{
		{Label: "To make", Value: fmt.Sprint(len(work)), Note: "work orders"},
		{Label: "To get", Value: fmt.Sprint(len(reqs)), Note: "requisitions"},
	})
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "Hangars")
	esp.PanelBand(&b, pg.pal, []esp.Panel{m.hangarOwnersPanel(ctx, pg, corpID)})
	b.WriteString("<br>\n")

	if env := m.corpEnvelopes(ctx, corpID); len(env) > 0 {
		esp.Section(&b, pg.pal, "This week's budgets")
		esp.PanelBand(&b, pg.pal, []esp.Panel{m.envelopePanel(pg, env)})
		b.WriteString("<br>\n")
	}

	esp.Section(&b, pg.pal, "To make")
	esp.PanelBand(&b, pg.pal, []esp.Panel{m.requestWorkPanel(ctx, pg, corpID, "Work orders",
		"Built by Industry, researched and copied by R&amp;D.", work, "Nothing is waiting to be made.")})
	b.WriteString("<br>\n")
	esp.Section(&b, pg.pal, "To get")
	esp.PanelBand(&b, pg.pal, []esp.Panel{m.requestReqPanel(ctx, pg, corpID, "Requisitions",
		"Bought by the corporation's buyers, or built when that is cheaper.", reqs, "Nothing is on order.")})
	return b.String()
}

// writeArmRequests is a department page's half of the lists: what the arm
// asked for, and what others asked of it. Nothing at all when both are empty.
func (m *Mod) writeArmRequests(ctx context.Context, b *strings.Builder, pg igbPage, corpID int64, arm corpArm) {
	work, _ := m.openWorkFor(ctx, corpID)
	reqs := m.openReqSummary(ctx, corpID)
	var askedW, doingW []corpWork
	for _, w := range work {
		if workRequesterArm(w) == arm {
			askedW = append(askedW, w)
		} else if workFulfillerArm(w) == arm {
			doingW = append(doingW, w)
		}
	}
	var askedR []requisition
	for _, r := range reqs {
		if reqRequesterArm(r) == arm {
			askedR = append(askedR, r)
		}
	}
	if len(askedW)+len(doingW)+len(askedR) == 0 {
		return
	}
	host := esp.IGBHost()
	esp.Section(b, pg.pal, "Requests")
	b.WriteString(pg.soft(`What `+esp.IGBText(armLabel(arm))+` asked the other departments for, and what they asked of it. `+
		`Every department's are on <a href="`+host+requestsPath+`">Requests</a>.`) + "<br>\n")
	var panels []esp.Panel
	if len(askedW) > 0 {
		panels = append(panels, m.requestWorkPanel(ctx, pg, corpID, "Asked of the factory", "", askedW, ""))
	}
	if len(doingW) > 0 {
		panels = append(panels, m.requestWorkPanel(ctx, pg, corpID, "Asked of "+esp.IGBText(armLabel(arm)), "", doingW, ""))
	}
	if len(askedR) > 0 {
		panels = append(panels, m.requestReqPanel(ctx, pg, corpID, "On order", "", askedR, ""))
	}
	for _, p := range panels {
		esp.PanelBand(b, pg.pal, []esp.Panel{p})
	}
	b.WriteString("<br>\n")
}

// requestGridAlign / requestGridWidth: icon, what and where, progress and pay.
var (
	requestGridAlign = []string{"", "", "right"}
	requestGridWidth = []string{"36", "", "140"}
)

// deliverLine is "for Security, deliver to [SEC] Armory at Station". Shared by both panels
// so a work order and the requisition behind it read alike.
func (m *Mod) deliverLine(ctx context.Context, pg igbPage, corpID int64, arm corpArm, flag, stationID int64) string {
	where := esp.IGBText(m.divLabelAt(ctx, corpID, flag))
	if stationID != 0 {
		where += " at " + esp.IGBText(m.stationLabel(ctx, stationID))
	}
	return pg.soft("for "+esp.IGBText(armLabel(arm))+" &mdash; deliver to ") + pg.tint(pg.pal.Accent, where)
}

func (m *Mod) requestWorkPanel(ctx context.Context, pg igbPage, corpID int64, title, note string,
	work []corpWork, empty string) esp.Panel {
	p := esp.Panel{Title: title, Note: note, Align: requestGridAlign, Width: requestGridWidth, Empty: empty}
	for _, w := range work {
		what := esp.IGBText(m.typeNameOr(ctx, w.ProductTypeID))
		icon, unit := w.ProductTypeID, "run"
		switch w.Kind {
		case workKindME:
			what, icon, unit = "ME on "+esp.IGBText(m.typeNameOr(ctx, w.BPTypeID)), w.BPTypeID, "level"
		case workKindPE:
			what, icon, unit = "PE on "+esp.IGBText(m.typeNameOr(ctx, w.BPTypeID)), w.BPTypeID, "level"
		case workKindCopy:
			what, icon, unit = "copy "+esp.IGBText(m.typeNameOr(ctx, w.BPTypeID)), w.BPTypeID, "copy"
		case workKindRefine:
			what = "refine " + what
		}
		by := armLabel(workFulfillerArm(w))
		frac := 0.0
		if w.RunsWanted > 0 {
			frac = float64(w.RunsDone) / float64(w.RunsWanted)
		}
		p.Rows = append(p.Rows, []string{
			esp.ShipIcon(icon, 32),
			igbF3(what) + pg.dim(" &middot; "+esp.IGBText(by)) + "<br>" +
				m.deliverLine(ctx, pg, corpID, workRequesterArm(w), workDestFlag(w), workDestStation(w)),
			esp.Bar(frac, 10, pg.pal.Up) + "<br>" +
				pg.soft(fmt.Sprintf("%d of %d %s(s)", w.RunsDone, w.RunsWanted, unit)) + "<br>" +
				pg.dim(esp.ISKShort(w.WagePerRun)+" per "+unit),
		})
	}
	return p
}

// reqStatusWords is how a requisition's state reads to a member.
func reqStatusWords(status string) string {
	switch status {
	case "bidding":
		return "a bid is on the market"
	case "buying":
		return "a buyer is fetching it"
	case reqStatusBuilding:
		return "Industry is building it"
	}
	return "waiting for a buyer"
}

func (m *Mod) requestReqPanel(ctx context.Context, pg igbPage, corpID int64, title, note string,
	reqs []requisition, empty string) esp.Panel {
	p := esp.Panel{Title: title, Note: note, Align: requestGridAlign, Width: requestGridWidth, Empty: empty}
	for _, r := range reqs {
		frac := 0.0
		if r.Qty > 0 {
			frac = float64(r.QtyDone) / float64(r.Qty)
		}
		p.Rows = append(p.Rows, []string{
			esp.ShipIcon(r.TypeID, 32),
			igbF3(esp.IGBText(m.typeNameOr(ctx, r.TypeID))) + pg.dim(" &middot; "+reqStatusWords(r.Status)) + "<br>" +
				m.deliverLine(ctx, pg, corpID, reqRequesterArm(r), r.DestFlag, r.DestStationID),
			esp.Bar(frac, 10, pg.pal.Up) + "<br>" +
				pg.soft(fmt.Sprintf("%d of %d", r.QtyDone, r.Qty)) + "<br>" +
				pg.dim("up to "+esp.ISKShort(r.Budget)),
		})
	}
	return p
}

// hangarOwnersPanel is the seven divisions: name, owner and what it is for.
func (m *Mod) hangarOwnersPanel(ctx context.Context, pg igbPage, corpID int64) esp.Panel {
	p := esp.Panel{Title: "Who keeps which division",
		Note:  "Every member can see into its own department's hangars. Taking from one is earned.",
		Align: []string{"right", "", ""}, Width: []string{"36", "", "160"}}
	names, _ := m.divisionNames(ctx, corpID)
	prof := m.corpDivs(ctx, corpID)
	for d := 1; d <= 7; d++ {
		owner := divisionArm(prof, d)
		p.Rows = append(p.Rows, []string{
			igbF3(fmt.Sprint(d)),
			igbF3(esp.IGBText(divDisplayName(prof, d, names[d]))),
			pg.soft(esp.IGBText(armLabel(owner))),
		})
	}
	return p
}

// envelopePanel is the week's split: allotted, used, left, per arm.
func (m *Mod) envelopePanel(pg igbPage, env []corpEnvelope) esp.Panel {
	p := esp.Panel{Title: "Finance's envelopes",
		Note:  "Each department's share of the week. A request is paid from the asking department's envelope.",
		Align: []string{"", "", "right"}, Width: []string{"", "120", "140"}}
	for _, e := range env {
		frac := 0.0
		if e.Allotted > 0 {
			frac = e.Used / e.Allotted
		}
		capNote := ""
		if !envelopeCapped(e.Arm) {
			capNote = pg.dim(" &middot; never held back")
		}
		p.Rows = append(p.Rows, []string{
			igbF3(esp.IGBText(armLabel(e.Arm))) + capNote,
			esp.Bar(min(frac, 1), 10, pg.pal.Up),
			igbF3(esp.ISKShort(e.Room())) + pg.dim(" of "+esp.ISKShort(e.Allotted)),
		})
	}
	return p
}
