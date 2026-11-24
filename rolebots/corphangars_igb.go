package rolebots

import (
	"fmt"
	"strconv"
	"strings"

	"gurimoon/esp"
)

// The Hangars form of the Management page (corphangars.go): which hangar the Armory, the Fleet Yard
// and Spoils stand in. The chair's own settings, so it sits on the Corporation page with the
// others. What a division's members may do in a hangar follows from who owns it.

// hangarState is what the form and its preview read: the hangars as they stand.
type hangarState struct {
	prof        *divProfile
	pins        map[divPurpose]int
	names       map[int]string
	occ         map[int]bool
	deskClaimed map[int]bool
	named       map[int]bool
}

func (p *mgPage) hangarState() hangarState {
	st := hangarState{prof: p.m.corpDivs(p.ctx, p.corpID), pins: map[divPurpose]int{}, deskClaimed: map[int]bool{1: true}}
	if pins, err := p.m.divPins(p.ctx, p.corpID); err == nil {
		st.pins = pins
	}
	st.names, _ = p.m.divisionNames(p.ctx, p.corpID)
	st.occ, _ = p.m.divisionOccupancy(p.ctx, p.corpID)
	if facts, err := getAnyCorpFacts(p.ctx, p.m.db, p.corpID); err == nil && facts != nil {
		st.deskClaimed = legacyDeskNeeds(facts.Policy.Tags)
	}
	st.named = humanNamedDivs(st.names)
	return st
}

// holdsOther reports whether a hangar holds stock that is not the war chest's.
func (st hangarState) holdsOther(d int) bool {
	if !st.occ[d] {
		return false
	}
	for _, x := range hangarPurposes {
		if st.prof.Div(x) == d {
			return false
		}
	}
	return true
}

// pinField is a pin as the form's field holds it.
func pinField(v int) string {
	switch {
	case v == 0:
		return ""
	case v == pinNone:
		return "none"
	}
	return strconv.Itoa(v)
}

// hangarArm is the division a purpose belongs to.
func hangarArm(pur divPurpose) corpArm { return warChestArm[pur] }

func writeHangarsForm(p *mgPage, b *strings.Builder, _ corpArm) {
	st := p.hangarState()
	t := esp.Panel{Title: "The seven hangars", Align: []string{"right", "", "", "right"}, Width: []string{"30", "", "130", "110"}}
	for d := 1; d <= 7; d++ {
		held := "empty"
		if st.occ[d] {
			held = "holds stock"
		}
		t.Rows = append(t.Rows, []string{strconv.Itoa(d), igbF3(esp.IGBText(divDisplayName(st.prof, d, st.names[d]))),
			p.pg.soft(armLabel(divisionArm(st.prof, d))), p.pg.soft(held)})
	}
	esp.PanelGrid(b, p.pg.pal, [][]esp.Panel{{t}})
	b.WriteString("<br>" + p.open("hangars"))
	for _, pur := range hangarPurposes {
		field := strings.ToLower(string(pur))
		cur := p.cur("hangars", field, pinField(st.pins[pur]))
		auto := "automatic: none needed"
		if d := st.prof.Div(pur); d > 0 && st.pins[pur] == 0 {
			auto = "automatic: " + pinWord(d)
		}
		b.WriteString(`<font size=3>` + esp.IGBText(warChestLabels[pur]) + ` (` + esp.IGBText(armLabel(hangarArm(pur))) + `): </font>` +
			`<select name="` + field + `">` + selOpt("", auto, cur == "") + selOpt("none", "none", cur == "none"))
		for _, d := range borrowableDivs {
			if hangarChoiceRefusal(pur, d, st.deskClaimed, st.named) != "" {
				continue
			}
			label := pinWord(d)
			if st.holdsOther(d) {
				label += ", holds stock"
			}
			b.WriteString(selOpt(strconv.Itoa(d), label, cur == strconv.Itoa(d)))
		}
		b.WriteString(`</select><br>`)
	}
	b.WriteString(p.submit("Preview hangars"))
}

func parseHangarsForm(p *mgPage, _ corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	st := p.hangarState()
	var pay cmdHangarsPayload
	var lines []string
	changed := false
	for _, pur := range hangarPurposes {
		field := strings.ToLower(string(pur))
		raw := p.q.Get(field)
		hidden[field] = raw
		v, ok := parseHangarChoice(raw)
		if !ok {
			return nil, "", "Pick a hangar for the " + warChestLabels[pur] + "."
		}
		if why := hangarChoiceRefusal(pur, v, st.deskClaimed, st.named); why != "" {
			return nil, "", why
		}
		switch pur {
		case purArmory:
			pay.Armory = v
		case purYard:
			pay.Yard = v
		default:
			pay.Spoils = v
		}
		if v == st.pins[pur] {
			continue
		}
		changed = true
		label := esp.IGBText(warChestLabels[pur])
		switch {
		case v == 0:
			lines = append(lines, fmt.Sprintf("The <b>%s</b> goes back to the corporation's own rules.", label))
		case v == pinNone:
			lines = append(lines, fmt.Sprintf("The <b>%s</b> has no hangar. Stock already in its hangar stays where it is.", label))
		default:
			line := fmt.Sprintf("The <b>%s</b> stands in <b>%s</b>.", label, esp.IGBText(pinWord(v)))
			if st.holdsOther(v) {
				line += " Whatever it holds moves to Materials."
			}
			if from := st.prof.Div(pur); from > 0 && from != v {
				line += fmt.Sprintf(" The %s's stock moves with it from %s.", label, esp.IGBText(pinWord(from)))
			}
			lines = append(lines, line)
		}
	}
	if !changed {
		return nil, "", "That is how the hangars already stand."
	}
	preview := igbF3(strings.Join(lines, "<br>")) + "<br>" +
		p.pg.dim3("Each division's members hold the roles for the hangars it owns, and are brought in line when this is done. "+
			"A capsuleer never holds take on a war-chest hangar unless their own division owns it.") + "<br><br>"
	return []corpCommand{{Kind: cmdKindHangars, Payload: encodeCommandPayload(pay)}}, preview, ""
}
