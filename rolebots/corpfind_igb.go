package rolebots

import (
	"strconv"
	"strings"

	"gurimoon/esp"
)

// The look of a Management form, and finding things to put in it (corpfind.go). One field to a line:
// what it is in plain words, the box, and what to put in it. A field that names something (an item,
// a system) has a Find button, which lists what the letters could mean; a click puts the name in.

// line is one labelled field: label, box, hint. find is the field's kind ("item", "system",
// "location", "blueprint") when it has a Find button, "" for none.
func (p *mgPage) line(label, name, value string, size int, hint, find string) string {
	s := `<font size=3><b>` + esp.IGBText(label) + `</b></font><br>` +
		`<input type="text" name="` + name + `" size="` + strconv.Itoa(size) + `" value="` + attrEsc(value) + `"> `
	if find != "" {
		s += `<input type="submit" name="find_` + name + `" value="Find ` + find + `"> `
	}
	if hint != "" {
		s += p.pg.dim3(esp.IGBText(hint))
	}
	return s + "<br><br>\n"
}

// lineHTML is a labelled field the caller drew (a select), with its hint.
func (p *mgPage) lineHTML(label, field, hint string) string {
	s := `<font size=3><b>` + esp.IGBText(label) + `</b></font><br>` + field + ` `
	if hint != "" {
		s += p.pg.dim3(esp.IGBText(hint))
	}
	return s + "<br><br>\n"
}

const (
	hintItem   = "Type part of the name. Spelling need not be exact; Find lists what it could be."
	hintSystem = "Type part of the name, or press Find."
)

// offerHits sets the page's "did you mean" list from ranked hits: a picture, the name to click, and
// what it is.
func (p *mgPage) offerHits(field string, hits []findHit, pic func(id int64) string) {
	p.suggest = p.suggest[:0]
	for _, h := range hits {
		s := ""
		if pic != nil {
			s = pic(h.ID) + " "
		}
		s += p.again(h.Name, field, h.Name)
		if h.Group != "" {
			s += " " + p.pg.dim(esp.IGBText(h.Group))
		}
		p.suggest = append(p.suggest, s)
	}
}

// findField is the field whose Find button was pressed, "" for none.
func (p *mgPage) findField() string {
	for _, f := range []string{"item", "system", "place"} {
		if p.q.Has("find_" + f) {
			return f
		}
	}
	return ""
}

// findPage answers a Find button: what the letters in the field could mean, to click. It reports
// false when the letters already mean one thing (or the key pressed was Enter in another field, which
// a browser sends as the first button): the form is then taken as it stands.
func (p *mgPage) findPage(field, form string) (string, bool) {
	words := strings.TrimSpace(p.q.Get(field))
	if words == "" {
		return p.pg.ordersBanner("Type a few letters first, then press Find.", true), true
	}
	var hits []findHit
	var pic func(int64) string
	what := "items"
	switch {
	case field == "place":
		_, clear, places := resolvePlace(words, p.m.placeEntries(p.ctx), 2*mgSuggestMax)
		if clear {
			return "", false
		}
		hits, what = places, "places"
	case field == "system":
		hits, what = p.m.findSystems(p.ctx, words, 2*mgSuggestMax), "systems"
	case form == "printbuy":
		hits, what = p.m.findBlueprints(p.ctx, words, 2*mgSuggestMax), "blueprints"
		pic = func(id int64) string { return esp.ShipIcon(id, 16) }
	default:
		hits = p.m.findMarketTypes(p.ctx, words, 2*mgSuggestMax)
		pic = func(id int64) string { return esp.ShipIcon(id, 16) }
	}
	if len(hits) == 0 {
		return p.pg.ordersBanner("Nothing looks like \""+words+"\". Try fewer letters, or another part of the name.", true), true
	}
	if field == "place" {
		p.offerPlaces(hits)
	} else {
		if findClear(hits) {
			return "", false
		}
		p.offerHits(field, hits, pic)
	}
	var b strings.Builder
	b.WriteString(igbF3("These "+what+" could be <b>"+esp.IGBText(words)+"</b>. Click the one you mean:") + "<br>\n")
	b.WriteString(strings.Join(p.suggest, "<br>\n") + "<br><br>\n")
	p.suggest = nil
	return b.String(), true
}
