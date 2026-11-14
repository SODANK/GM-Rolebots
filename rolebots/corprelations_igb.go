package rolebots

// The Corporate Portal's Relations section: /corp/relations (old /ig/corprelations). It
// shows what members may know of the corporation's politics: who it counts an ally, friend,
// rival or enemy, who counts it one, the pacts and wars standing, recent events, and for an
// officer the letters on the desk with the answer to give. Public information only: an
// identity header is a display hint, never authorization, so it shows tiers, never numbers.
// The answer is the one write and takes the portal key (portalkey.go) through the
// preview-then-confirm shape of the Orders page, with GET forms each outside any table.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gurimoon/esp"
)

const relationsPath = "/corp/relations"

// handleIGBCorpRelations serves /corp/relations and /ig/corprelations.
func (m *Mod) handleIGBCorpRelations(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	charID, corpID := m.portalViewer(ctx, r)
	if corpID == 0 {
		esp.WriteCorpPortalPage(w, r, "relations", igbF3("Open this page from your own terminal."))
		return
	}
	pg := m.igbPageFor(r)
	q := r.URL.Query()
	officer := charID != 0 && !m.isSynthetic(charID) && m.isCorpOfficer(ctx, corpID, charID)

	var top strings.Builder
	if q.Get("form") == "answer" {
		top.WriteString(m.relationsAnswerSubmit(ctx, pg, charID, corpID, officer, q))
	}
	esp.WriteCorpPortalPage(w, r, "relations", top.String()+m.relationsBody(ctx, pg, charID, corpID, officer, q))
}

// relationsBody is the page: the standing, the desk, the record.
func (m *Mod) relationsBody(ctx context.Context, pg igbPage, charID, corpID int64, officer bool, q url.Values) string {
	var b strings.Builder
	m.writeRelationsStanding(ctx, &b, pg, corpID)
	m.writeRelationsDesk(ctx, &b, pg, charID, corpID, officer, q)
	m.writeRelationsRecord(ctx, &b, pg, corpID)
	return b.String()
}

// writeRelationsStanding is who the corporation counts what, and who counts
// it what, by tier: names only.
func (m *Mod) writeRelationsStanding(ctx context.Context, b *strings.Builder, pg igbPage, corpID int64) {
	s := loadRelSnapshot()
	ours := map[string][]string{}
	theirs := map[string][]string{}
	if s != nil {
		for pair, v := range s.corp {
			tier := standingTier(v)
			if tier == "neutral" {
				continue
			}
			switch {
			case pair[0] == corpID:
				ours[tier] = append(ours[tier], corpNameOf(ctx, m.db, pair[1]))
			case pair[1] == corpID:
				theirs[tier] = append(theirs[tier], corpNameOf(ctx, m.db, pair[0]))
			}
		}
	}
	row := func(label string, names []string) []string {
		if len(names) == 0 {
			return []string{label, pg.dim("nobody")}
		}
		return []string{label, esp.IGBText(strings.Join(names, ", "))}
	}
	ourPanel := esp.Panel{Title: "We count as", Width: []string{"90", ""}}
	theirPanel := esp.Panel{Title: "They count us as", Width: []string{"90", ""}}
	for _, t := range []struct{ tier, label string }{
		{"devoted", "Allies"}, {"liked", "Friends"}, {"disliked", "Rivals"}, {"despised", "Enemies"},
	} {
		ourPanel.Rows = append(ourPanel.Rows, row(t.label, ours[t.tier]))
		theirPanel.Rows = append(theirPanel.Rows, row(t.label, theirs[t.tier]))
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{ourPanel, theirPanel}})

	// Pacts and wars.
	p := esp.Panel{Title: "Pacts and wars", Width: []string{"", "120"}, Empty: "No pact stands and no war runs."}
	now := clock.Unix()
	if rows, err := m.db.QueryContext(ctx, `
		SELECT corpA, corpB, kind, endAt FROM rolebot_corp_pacts
		 WHERE (corpA = ? OR corpB = ?) AND status = 'standing' AND endAt > ? ORDER BY endAt`, corpID, corpID, now); err == nil {
		for rows.Next() {
			var a, bb, until int64
			var kind string
			if rows.Scan(&a, &bb, &kind, &until) != nil {
				continue
			}
			other := a
			if other == corpID {
				other = bb
			}
			p.Rows = append(p.Rows, []string{
				esp.IGBText(letterKindWords(kind) + " with " + corpNameOf(ctx, m.db, other)),
				pg.dim("until " + ycDate(time.Unix(until, 0))),
			})
		}
		rows.Close()
	}
	for _, e := range relWarEnemiesOf(corpID) {
		p.Rows = append(p.Rows, []string{esp.IGBText("at war with " + corpNameOf(ctx, m.db, e)), pg.tint(pg.pal.Red, "shooting")})
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{p}})
}

// writeRelationsDesk is the letters on the corporation's desk, with the
// answer to give for an officer; the letters it has out are listed too.
func (m *Mod) writeRelationsDesk(ctx context.Context, b *strings.Builder, pg igbPage, charID, corpID int64, officer bool, q url.Values) {
	host := esp.IGBHost()
	in, _ := m.lettersWhere(ctx, `toCorpID = ? AND status = ?`, corpID, letterDelivered)
	out, _ := m.lettersWhere(ctx, `fromCorpID = ? AND status IN (?, ?, ?, ?)`, corpID, letterDrafted, letterSent, letterDelivered, letterAccepted)
	esp.Section(b, pg.pal, "The desk")
	if len(in) == 0 && len(out) == 0 {
		b.WriteString(pg.dim3("No letter awaits an answer and none is out.") + "<br><br>\n")
		return
	}
	for _, l := range in {
		from := corpNameOf(ctx, m.db, l.FromCorpID)
		b.WriteString(igbF3("<b>"+esp.IGBText(from)+"</b> proposes "+esp.IGBText(letterKindWords(l.Kind))) + "<br>")
		b.WriteString(pg.dim3(esp.IGBText(l.Terms)+" "+esp.IGBText(letterOfferWords(l))) + "<br>")
		if officer {
			b.WriteString(igbF3(fmt.Sprintf(`<a href="%s%s?form=answer&letter=%d&do=accept">Accept</a> &middot; `+
				`<a href="%s%s?form=answer&letter=%d&do=reject">Decline</a>`, host, relationsPath, l.ID, host, relationsPath, l.ID)))
		} else {
			b.WriteString(pg.dim3("The chair — the CEO and the Directors — answers it."))
		}
		b.WriteString("<br><br>\n")
	}
	for _, l := range out {
		to := corpNameOf(ctx, m.db, l.ToCorpID)
		status := l.Status
		if l.Status == letterSent && l.EnvoyCharID != 0 {
			status = "with " + m.charNameOr(ctx, l.EnvoyCharID) + " on the road"
		}
		b.WriteString(igbF3("Our letter to <b>"+esp.IGBText(to)+"</b> proposing "+esp.IGBText(letterKindWords(l.Kind))) +
			" " + pg.dim3(esp.IGBText(status)) + "<br>")
	}
	b.WriteString("<br>\n")
}

// relationsAnswerSubmit is the one write: preview, then on confirm the answer through
// answerLetter, the function the bot pass uses.
func (m *Mod) relationsAnswerSubmit(ctx context.Context, pg igbPage, charID, corpID int64, officer bool, q url.Values) string {
	if !officer {
		return pg.ordersBanner("Letters are answered by the chair — the CEO and the Directors.", true)
	}
	id, _ := strconv.ParseInt(q.Get("letter"), 10, 64)
	l, _ := m.letterByID(ctx, id)
	if l == nil || l.ToCorpID != corpID || l.Status != letterDelivered {
		return pg.ordersBanner("That letter is not on this corporation's desk.", true)
	}
	accept := q.Get("do") == "accept"
	verb := "decline"
	if accept {
		verb = "accept"
	}
	from := corpNameOf(ctx, m.db, l.FromCorpID)
	if q.Get("confirm") != "1" {
		var b strings.Builder
		esp.Section(&b, pg.pal, "Preview")
		b.WriteString(igbF3(fmt.Sprintf("You will <b>%s</b> %s's proposal of %s.", verb, esp.IGBText(from), letterKindWords(l.Kind))) + "<br>")
		if accept && (l.Kind == letterNAP || l.Kind == letterAlliance || l.Kind == letterPeace) {
			b.WriteString(pg.dim3(fmt.Sprintf("The pact stands %d days and holds both corporations' standing toward each other; a hostile act breaks it.", l.TermDays)) + "<br>")
		}
		if accept && l.Kind == letterMutual {
			b.WriteString(pg.dim3("The war turns mutual at once: CONCORD bills neither side, and it runs until one of you "+
				"withdraws in the Wars tab or retracts. They may withdraw too, if the fight goes cold or turns on them.") + "<br>")
		}
		b.WriteString(`<form action="` + esp.IGBHost() + relationsPath + `" method="get">`)
		for k, v := range map[string]string{"form": "answer", "letter": strconv.FormatInt(l.ID, 10), "do": q.Get("do"), "confirm": "1"} {
			b.WriteString(`<input type="hidden" name="` + k + `" value="` + attrEsc(v) + `">`)
		}
		b.WriteString(keyField(q))
		b.WriteString(`<input type="submit" value="` + strings.ToUpper(verb[:1]) + verb[1:] + ` the proposal"></form><br>`)
		return b.String()
	}
	if err := m.verifyPortalKey(ctx, charID, corpID, strings.TrimSpace(q.Get("key"))); err != nil {
		return pg.ordersBanner("That portal key is not yours, or has lapsed. "+
			`<a href="`+esp.IGBHost()+ordersPath+`?key=request">Request a key</a> on the Orders page.`, true)
	}
	why := "answered on the Corporate Portal by " + m.charNameOr(ctx, charID)
	if err := m.answerLetter(ctx, l, charID, accept, why); err != nil {
		if errors.Is(err, errMutualLapsed) {
			msg := err.Error()
			return pg.ordersBanner(esp.IGBText(strings.ToUpper(msg[:1])+msg[1:]+"."), true)
		}
		rlogf("rolebots: portal answer letter %d: %v", l.ID, err)
		return pg.ordersBanner("The answer could not be filed. Try again shortly.", true)
	}
	if accept && l.Kind == letterMutual {
		return pg.ordersBanner("Accepted. The war is mutual: CONCORD bills neither side while it stays so.", false)
	}
	if accept {
		return pg.ordersBanner("Accepted. The pact is recorded and both corporations' standing holds to it.", false)
	}
	return pg.ordersBanner("Declined. The letter is answered.", false)
}

// writeRelationsRecord is the last few things that happened between this
// corporation and another, as sentences without numbers.
func (m *Mod) writeRelationsRecord(ctx context.Context, b *strings.Builder, pg igbPage, corpID int64) {
	p := esp.Panel{Title: "Recent incidents", Width: []string{"", "90"}, Empty: "Nothing of note lately."}
	rows, err := m.db.QueryContext(ctx, `
		SELECT toID, kind, side, systemID, at FROM rolebot_corp_incidents
		 WHERE corporationID = ? AND kind <> ? ORDER BY at DESC, incidentID DESC LIMIT 20`, corpID, incCrowd)
	if err == nil {
		for rows.Next() {
			var to, sys, at int64
			var kind, side string
			if rows.Scan(&to, &kind, &side, &sys, &at) != nil {
				continue
			}
			p.Rows = append(p.Rows, []string{
				esp.IGBText(incidentSentence(kind, side, corpNameOf(ctx, m.db, to), m.nameOfSystem(ctx, sys))),
				pg.dim(fmtDuration(clock.Unix()-at) + " ago"),
			})
		}
		rows.Close()
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{p}})
}

// ycDate is a day in New Eden's calendar (2026 is YC 128 unless the calendar settings say
// otherwise), dashes rather than dots because the browser's size-2 font drops the dot.
func ycDate(t time.Time) string { return esp.YC(t) }

// incidentSentence is one incident as a member reads it. Pure.
func incidentSentence(kind, side, other, system string) string {
	where := ""
	if system != "" && system != "?" {
		where = " in " + system
	}
	victim := side == relSideVictim
	switch kind {
	case incKill:
		if victim {
			return other + " destroyed one of our ships" + where
		}
		return "one of ours destroyed a ship of " + other + "'s" + where
	case incGank:
		if victim {
			return other + " suicide-ganked one of our ships under CONCORD's nose" + where
		}
		return "one of ours was lost to CONCORD taking a ship of " + other + "'s" + where
	case incPod:
		if victim {
			return other + " podded one of ours" + where
		}
		return "one of ours podded a pilot of " + other + "'s" + where
	case incAggress:
		if victim {
			return other + " fired on us" + where
		}
		return "we fired on " + other + where
	case incRobbery:
		if victim {
			return other + " held one of ours to ransom" + where
		}
		return "we held one of " + other + "'s to ransom" + where
	case incBounty:
		return other + " put a price on one of our heads"
	case incSiege:
		if victim {
			return other + " put our tower into reinforcement" + where
		}
		return "we put " + other + "'s tower into reinforcement" + where
	case incTowerKill:
		if victim {
			return other + " destroyed our tower" + where
		}
		return "we destroyed " + other + "'s tower" + where
	case incWarDec:
		if victim {
			return other + " declared war on us"
		}
		return "we declared war on " + other
	case incWarRetract:
		return other + " retracted its war on us"
	case incWarEnd:
		return "the war with " + other + " ended"
	case incEnvoyKill:
		return other + " killed our envoy on the road" + where
	case incContest:
		return "raced " + other + " for a deadspace site" + where
	case incPact:
		return "a pact was agreed with " + other
	case incReparation:
		if victim {
			return other + " paid us reparations"
		}
		return "we paid " + other + " reparations"
	case incPeace:
		return "peace was made with " + other
	case incPactBreak:
		if victim {
			return other + " broke its pact with us"
		}
		return "we broke our pact with " + other
	}
	return kind + " with " + other
}
