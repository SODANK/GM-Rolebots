package rolebots

// The Corporate Portal's Alliance section: /corp/alliance. It shows what a member of an allied
// corporation may know about the bloc: who runs it and who is in it, whom each backs for executor, its
// holdings, wars, tower orders and tithes, and the last things done in its name (rolebot_alliance_book).
//
// Public information only (esp/corpportal.go): an identity header is a display hint, never authorization.
// The writes are an executor officer's orders (sovereignty target, war request, defend call), each a
// rolebot_alliance_requests row the alliance power consumes (alliancesov.go, alliancewar.go, gangalliance.go).
// They take the portal key (portalkey.go) through preview-then-confirm GET forms outside any table.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

const alliancePortalPath = "/corp/alliance"

// handleIGBCorpAlliance serves /corp/alliance.
func (m *Mod) handleIGBCorpAlliance(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	charID, corpID := m.portalViewer(ctx, r)
	if corpID == 0 {
		esp.WriteCorpPortalPage(w, r, "alliance", igbF3("Open this page from your own terminal."))
		return
	}
	pg := m.igbPageFor(r)
	seat, err := m.allianceOf(ctx, corpID)
	if err != nil || seat == nil {
		esp.WriteCorpPortalPage(w, r, "alliance", igbF3("Your corporation flies under no alliance's banner.")+"<br>"+
			pg.dim3("An alliance is a group of corporations run by its executor corporation. Your CEO applies to one, or founds one, from the Corporate Registry."))
		return
	}
	q := r.URL.Query()
	officer := charID != 0 && !m.isSynthetic(charID) && m.isCorpOfficer(ctx, corpID, charID)
	execOfficer := officer && seat.IsExecutor

	var top strings.Builder
	if q.Get("form") != "" {
		top.WriteString(m.allianceOrderSubmit(ctx, pg, charID, corpID, seat, execOfficer, q))
	}
	esp.WriteCorpPortalPage(w, r, "alliance", top.String()+m.allianceBody(ctx, pg, corpID, seat, execOfficer, q))
}

// allianceBody is the page.
func (m *Mod) allianceBody(ctx context.Context, pg igbPage, corpID int64, seat *allianceSeat, execOfficer bool, q url.Values) string {
	var b strings.Builder
	role := "a member"
	if seat.IsExecutor {
		role = "the executor"
	}
	b.WriteString(igbF3(fmt.Sprintf("<b>%s</b> [%s] — your corporation is %s.", esp.IGBText(seat.Name), esp.IGBText(seat.Ticker), role)) + "<br>")
	b.WriteString(pg.dim3(fmt.Sprintf("Executor: %s. %s", esp.IGBText(ownerNameOf(ctx, m.db, seat.ExecutorCorpID)),
		map[bool]string{true: "The seat is fixed by charter.", false: "The seat is elected: one corporation, one vote."}[seat.Dictatorial])) + "<br><br>\n")

	members, _ := m.allianceMemberFactsOf(ctx, seat.AllianceID, corpID)
	bal := m.titheBalances(ctx, seat.AllianceID)
	mp := esp.Panel{Title: "Member corporations", Width: []string{"", "60", "", "90", "90"},
		Align: []string{"", "right", "", "right", "right"}, Empty: "No members."}
	mp.Rows = append(mp.Rows, []string{pg.dim("Corporation"), pg.dim("Pilots"), pg.dim("Backs"), pg.dim("Owes"), pg.dim("Paid")})
	for _, f := range members {
		name := esp.IGBText(f.Name)
		if f.IsExecutor {
			name += " " + pg.dim("executor")
		}
		backs := pg.dim("secret")
		if f.ChosenExec != 0 {
			backs = esp.IGBText(ownerNameOf(ctx, m.db, f.ChosenExec))
		}
		owed, paid := pg.dim("—"), pg.dim("—")
		if bl := bal[f.CorpID]; bl != nil {
			if bl.Owed > 0 {
				owed = iskString(bl.Owed)
			}
			if bl.Paid > 0 {
				paid = iskString(bl.Paid)
			}
		}
		mp.Rows = append(mp.Rows, []string{name, strconv.FormatInt(f.Pilots, 10), backs, owed, paid})
	}
	esp.PanelGrid(&b, pg.pal, [][]esp.Panel{{mp}})

	// Sovereignty and targets.
	sp := esp.Panel{Title: "Sovereignty", Width: []string{"", "110"}, Empty: "The alliance holds no system."}
	if ids, err := evedb.ListSovForAlliance(ctx, m.db, seat.AllianceID); err == nil {
		for _, id := range ids {
			sp.Rows = append(sp.Rows, []string{esp.IGBText(systemNameOf(ctx, m, id)), pg.tint(pg.pal.Green, "held")})
		}
	}
	for sys := range m.allianceTargetSystems(ctx, seat.AllianceID) {
		sp.Rows = append(sp.Rows, []string{esp.IGBText(systemNameOf(ctx, m, sys)), pg.dim("named for towers")})
	}
	// Wars.
	wp := esp.Panel{Title: "Wars", Width: []string{"", "90"}, Empty: "The alliance is at peace."}
	if wars, err := evedb.ListWars(ctx, m.db); err == nil {
		for _, w := range wars {
			if !w.Active {
				continue
			}
			state := pg.dim("arming")
			if w.HostileNow {
				state = pg.tint(pg.pal.Red, "shooting")
			}
			switch {
			case w.DeclaredByID == seat.AllianceID:
				wp.Rows = append(wp.Rows, []string{esp.IGBText("at war with " + w.AgainstName), state})
			case w.AgainstID == seat.AllianceID:
				wp.Rows = append(wp.Rows, []string{esp.IGBText(w.DeclaredByName + " declared on us"), state})
			}
		}
	}
	esp.PanelGrid(&b, pg.pal, [][]esp.Panel{{sp, wp}})

	// Relationships.
	rp := esp.Panel{Title: "Declared relationships", Width: []string{"", "100"}, Empty: "None declared."}
	if rels, err := evedb.ListAllianceRelationships(ctx, m.db, seat.AllianceID); err == nil {
		for _, r := range rels {
			rp.Rows = append(rp.Rows, []string{esp.IGBText(r.ToName), esp.IGBText(evedb.AllianceRelationshipName(r.Relationship))})
		}
	}
	// Open requests.
	qp := esp.Panel{Title: "Before the executor", Width: []string{"", "90"}, Empty: "Nothing awaits a decision."}
	for _, kind := range []string{allianceReqWar, allianceReqDefend, allianceReqSov, allianceReqMuster} {
		reqs, _ := m.allianceRequestsPending(ctx, seat.AllianceID, kind)
		for _, r := range reqs {
			qp.Rows = append(qp.Rows, []string{esp.IGBText(allianceRequestLine(ctx, m, r)), pg.dim(fmtDuration(clock.Unix()-r.PostedAt) + " ago")})
		}
	}
	esp.PanelGrid(&b, pg.pal, [][]esp.Panel{{rp, qp}})

	// The executor officer's desk.
	if execOfficer {
		m.writeAllianceDesk(ctx, &b, pg, seat)
	}

	// The book.
	bp := esp.Panel{Title: "In the alliance's name", Width: []string{"", "100"}, Empty: "Nothing has been done in its name yet."}
	if book, err := m.allianceBookRecent(ctx, seat.AllianceID, 20); err == nil {
		for _, e := range book {
			bp.Rows = append(bp.Rows, []string{esp.IGBText(allianceBookLine(e.Verb, e.Detail)), pg.dim(ycDate(time.Unix(e.At, 0)))})
		}
	}
	esp.PanelGrid(&b, pg.pal, [][]esp.Panel{{bp}})
	return b.String()
}

// allianceRequestLine words one open request. Pure given its reads.
func allianceRequestLine(ctx context.Context, m *Mod, r allianceRequest) string {
	from := ownerNameOf(ctx, m.db, r.FromCorp)
	switch r.Kind {
	case allianceReqWar:
		return from + " asks for war on " + ownerNameOf(ctx, m.db, r.TargetID)
	case allianceReqDefend:
		return "defend " + systemNameOf(ctx, m, r.SystemID) + " — " + r.Note
	case allianceReqSov:
		return "hold " + systemNameOf(ctx, m, r.SystemID) + " (ordered)"
	case allianceReqMuster:
		return "muster in " + systemNameOf(ctx, m, r.SystemID) + " — " + r.Note
	}
	return r.Kind
}

// writeAllianceDesk is the executor officer's three orders.
func (m *Mod) writeAllianceDesk(ctx context.Context, b *strings.Builder, pg igbPage, seat *allianceSeat) {
	esp.Section(b, pg.pal, "The executor's desk")
	host := esp.IGBHost()
	b.WriteString(pg.dim3("Each order is put before the member corporations' CEOs, who carry it out as their own charters allow. Every form asks for your portal key.") + "<br><br>")
	b.WriteString(`<form action="` + host + alliancePortalPath + `" method="get"><input type="hidden" name="form" value="sov">` +
		igbF3("Name a system to hold: ") + `<input type="text" name="system" size="14"> <input type="submit" value="Name it"></form><br>`)
	b.WriteString(`<form action="` + host + alliancePortalPath + `" method="get"><input type="hidden" name="form" value="war">` +
		igbF3("Ask for war on a corporation or alliance: ") + `<input type="text" name="target" size="24"> <input type="submit" value="Put it to the executor"></form><br>`)
	b.WriteString(`<form action="` + host + alliancePortalPath + `" method="get"><input type="hidden" name="form" value="muster">` +
		igbF3("Call the alliance to a system: ") + `<input type="text" name="system" size="14"> <input type="submit" value="Call them"></form><br>`)
	_ = seat
}

// allianceOrderSubmit is the write: preview, then on confirm a request row.
func (m *Mod) allianceOrderSubmit(ctx context.Context, pg igbPage, charID, corpID int64, seat *allianceSeat, execOfficer bool, q url.Values) string {
	if !execOfficer {
		return pg.ordersBanner("Only the executor corporation's chair — its CEO and Directors — orders the alliance.", true)
	}
	form := q.Get("form")
	var kind, line string
	var sysID, targetID int64
	switch form {
	case "sov", "muster":
		name := strings.TrimSpace(q.Get("system"))
		sysID = m.systemIDByName(ctx, name)
		if sysID == 0 {
			return pg.ordersBanner("No such solar system: "+esp.IGBText(name)+".", true)
		}
		if form == "sov" {
			kind, line = allianceReqSov, "name <b>"+esp.IGBText(systemNameOf(ctx, m, sysID))+"</b> a system the alliance holds; its corporations' towers go there"
		} else {
			kind, line = allianceReqMuster, "call every member corporation's security wing to <b>"+esp.IGBText(systemNameOf(ctx, m, sysID))+"</b>"
		}
	case "war":
		name := strings.TrimSpace(q.Get("target"))
		targetID = m.ownerIDByName(ctx, name)
		if targetID == 0 || targetID == seat.AllianceID {
			return pg.ordersBanner("No such corporation or alliance: "+esp.IGBText(name)+".", true)
		}
		kind, line = allianceReqWar, "put a war on <b>"+esp.IGBText(ownerNameOf(ctx, m.db, targetID))+"</b> before the executor's CEO, who declares it when the alliance can carry it"
	default:
		return pg.ordersBanner("Unknown order.", true)
	}
	if q.Get("confirm") != "1" {
		var b strings.Builder
		esp.Section(&b, pg.pal, "Preview")
		b.WriteString(igbF3("You will "+line+".") + "<br>")
		b.WriteString(`<form action="` + esp.IGBHost() + alliancePortalPath + `" method="get">`)
		for k, v := range map[string]string{"form": form, "system": q.Get("system"), "target": q.Get("target"), "confirm": "1"} {
			if v != "" {
				b.WriteString(`<input type="hidden" name="` + k + `" value="` + attrEsc(v) + `">`)
			}
		}
		b.WriteString(mgText("Portal key:", "key", q.Get("key"), 22, "") + "<br>")
		b.WriteString(`<input type="submit" value="Give the order"></form><br>`)
		return b.String()
	}
	if err := m.verifyPortalKey(ctx, charID, corpID, strings.TrimSpace(q.Get("key"))); err != nil {
		return pg.keyAskBanner()
	}
	now := clock.Unix()
	note := "ordered on the Corporate Portal by " + m.charNameOr(ctx, charID)
	payload := ""
	if kind == allianceReqWar {
		payload = fmt.Sprintf(`{"motive":"grievance","score":%.1f,"why":%q}`, 10.0, note)
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_alliance_requests
		  (allianceID, fromCorpID, posterID, kind, targetID, systemID, payload, note, postedAt, expiresAt, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
		seat.AllianceID, corpID, charID, kind, targetID, sysID, payload, truncate(note, 255), now, now+allianceWarRequestTTLSec); err != nil {
		rlogf("rolebots: portal alliance order: %v", err)
		return pg.ordersBanner("The order could not be filed. Try again shortly.", true)
	}
	m.allianceBook(ctx, seat.AllianceID, corpID, "order", targetID+sysID, 0, "%s — %s", allianceRequestLine(ctx, m, allianceRequest{Kind: kind, FromCorp: corpID, TargetID: targetID, SystemID: sysID, Note: note}), note)
	if kind == allianceReqMuster {
		// A muster is answered by every member on its next think, like a
		// defend call; file it under the defend kind so the same duty takes it.
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_alliance_requests SET kind = ? WHERE allianceID = ? AND kind = ? AND systemID = ? AND status = 'pending'`,
			allianceReqDefend, seat.AllianceID, allianceReqMuster, sysID)
	}
	return pg.ordersBanner("Ordered. The CEOs read it on their next review.", false)
}

// systemIDByName and ownerIDByName resolve typed names, case-folded.
func (m *Mod) systemIDByName(ctx context.Context, name string) int64 {
	if name == "" {
		return 0
	}
	var id int64
	_ = m.db.QueryRowContext(ctx, `SELECT solarSystemID FROM mapSolarSystems WHERE LOWER(solarSystemName) = LOWER(?) LIMIT 1`, name).Scan(&id)
	return id
}

func (m *Mod) ownerIDByName(ctx context.Context, name string) int64 {
	if name == "" {
		return 0
	}
	var id int64
	if m.db.QueryRowContext(ctx, `SELECT corporationID FROM corporation WHERE LOWER(corporationName) = LOWER(?) OR LOWER(tickerName) = LOWER(?) LIMIT 1`, name, name).Scan(&id) == nil && id != 0 {
		return id
	}
	_ = m.db.QueryRowContext(ctx, `SELECT allianceID FROM alliances WHERE deleted = 0 AND (LOWER(allianceName) = LOWER(?) OR LOWER(shortName) = LOWER(?)) LIMIT 1`, name, name).Scan(&id)
	return id
}
