package rolebots

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Hangars for a human chair. A bot CEO settles its war chest (the Armory, the Fleet Yard and
// Spoils: corpdivisions.go) in its own turn. A human chair has no such turn, so its war chest was
// never placed, and the members of Security and Mining held no hangar roles.
//
// Two things close that. The human chair's pass settles the war chest itself, the way the CEO's
// turn does (chairHangars), so a corporation that fields a combat wing or a mining op has its
// hangars without anyone asking. And the Hangars form on the Management page (corphangars_igb.go)
// lets the chair choose a hangar for a purpose, or none: a pin, rolebot_corp_div_pins, which the
// allocator keeps until a desk or a human claims that hangar. Either way the members' roles follow
// the hangars (refreshArmRoles), because a role bundle is computed from the hangars their division
// owns.

// hangarPurposes is the war chest in the order the form shows it.
var hangarPurposes = []divPurpose{purArmory, purYard, purSpoils}

const (
	chairHangarsEverySec = 600
	chairRolesEverySec   = 1800
)

// pinWord is a pin as a phrase. Pure.
func pinWord(v int) string {
	switch {
	case v == 0:
		return "automatic"
	case v == pinNone:
		return "none"
	}
	if n, ok := corpDivisionNames[v]; ok {
		return fmt.Sprintf("hangar %d (%s)", v, n)
	}
	return fmt.Sprintf("hangar %d", v)
}

// hangarsWords words an order. Pure.
func hangarsWords(p cmdHangarsPayload) string {
	var parts []string
	for _, pur := range hangarPurposes {
		parts = append(parts, warChestLabels[pur]+": "+pinWord(p.pin(pur)))
	}
	return strings.Join(parts, ", ")
}

// parseHangarChoice reads a form field: "" is the allocator's, "none" no hangar, else a hangar
// number. Pure.
func parseHangarChoice(s string) (int, bool) {
	switch s = strings.TrimSpace(s); s {
	case "", "auto":
		return 0, true
	case "none":
		return pinNone, true
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 1 && n <= 7
}

// hangarChoiceRefusal is why a purpose may not stand in a hangar, "" when it may. deskClaimed is
// what the corporation's desks use (legacyDeskNeeds); named is what a human named. Pure.
func hangarChoiceRefusal(pur divPurpose, v int, deskClaimed, named map[int]bool) string {
	label := warChestLabels[pur]
	switch {
	case v == 0 || v == pinNone:
		return ""
	case !containsDiv(borrowableDivs, v):
		return "The " + label + " can stand in hangar 2, 3, 4 or 5 only."
	case deskClaimed[v]:
		return fmt.Sprintf("Hangar %d is in use by one of the corporation's desks; the %s cannot have it.", v, label)
	case named[v]:
		return fmt.Sprintf("Hangar %d carries a name you gave it; the %s cannot have it.", v, label)
	}
	return ""
}

func containsDiv(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// humanNamedDivs is the divisions a human named: the module never borrows them.
func humanNamedDivs(names map[int]string) map[int]bool {
	out := map[int]bool{}
	for d, n := range names {
		if !moduleDivisionName(n) {
			out[d] = true
		}
	}
	return out
}

// divProfilesEqual reports whether two war chests stand in the same divisions. Pure.
func divProfilesEqual(a, b *divProfile) bool {
	for _, pur := range hangarPurposes {
		if a.Div(pur) != b.Div(pur) {
			return false
		}
	}
	return true
}

// applyHangars carries out the chair's order: it records the pins, clears what is in the way of a
// pinned hangar, lets the allocator settle the war chest, and brings the members' roles in line.
// It answers the order's result, or why not.
func (m *Mod) applyHangars(ctx context.Context, c corpCommand, p cmdHangarsPayload) (string, error) {
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil || facts == nil {
		return "", fmt.Errorf("the corporation's policy could not be read")
	}
	names, err := m.divisionNames(ctx, c.CorpID)
	if err != nil {
		return "", fmt.Errorf("the hangar names could not be read")
	}
	deskClaimed, named := legacyDeskNeeds(facts.Policy.Tags), humanNamedDivs(names)
	for _, pur := range hangarPurposes {
		if why := hangarChoiceRefusal(pur, p.pin(pur), deskClaimed, named); why != "" {
			return "", fmt.Errorf("%s", strings.TrimSuffix(strings.ToLower(why[:1])+why[1:], "."))
		}
	}
	have, err := m.loadDivProfile(ctx, c.CorpID)
	if err != nil {
		return "", fmt.Errorf("the war chest could not be read")
	}
	occ, err := m.divisionOccupancy(ctx, c.CorpID)
	if err != nil {
		return "", fmt.Errorf("the hangars could not be read")
	}
	held := map[int]bool{}
	for _, d := range have.div {
		held[d] = true
	}
	for _, pur := range hangarPurposes {
		v := p.pin(pur)
		if v == 0 {
			if _, err := m.db.ExecContext(ctx,
				`DELETE FROM rolebot_corp_div_pins WHERE corporationID = ? AND purpose = ?`, c.CorpID, string(pur)); err != nil {
				return "", fmt.Errorf("the choice could not be written")
			}
			continue
		}
		// Whatever else stands in a hangar the war chest is moving into goes to Materials, where
		// donations land; a hangar the war chest already holds keeps its stock.
		if v > 0 && occ[v] && !held[v] {
			if _, err := m.moveDivisionStock(ctx, c.CorpID, divFlag(v), divFlag(1)); err != nil {
				return "", fmt.Errorf("the hangar could not be cleared")
			}
			held[v] = true
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_div_pins (corporationID, purpose, division, setBy, since) VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE division = VALUES(division), setBy = VALUES(setBy), since = VALUES(since)`,
			c.CorpID, string(pur), v, c.PosterID, clock.Unix()); err != nil {
			return "", fmt.Errorf("the choice could not be written")
		}
	}
	forgetDivProfile(c.CorpID)
	ceo := m.deskCEO(ctx, c.CorpID)
	if ceo == nil {
		return "", fmt.Errorf("no bot is free to carry it out")
	}
	if err := m.ensureCorpDivisions(ctx, ceo, facts); err != nil {
		return "", err
	}
	forgetDivProfile(c.CorpID)
	if _, err := m.refreshArmRoles(ctx, c.CorpID); err != nil {
		rlogf("rolebots: hangar roles of corp %d: %v", c.CorpID, err)
	}
	return m.hangarsNow(ctx, c.CorpID), nil
}

// hangarsNow is where the war chest stands, in a line.
func (m *Mod) hangarsNow(ctx context.Context, corpID int64) string {
	p := m.corpDivs(ctx, corpID)
	var parts []string
	for _, pur := range hangarPurposes {
		w := "none"
		if d := p.Div(pur); d > 0 {
			w = pinWord(d)
		}
		parts = append(parts, warChestLabels[pur]+": "+w)
	}
	return strings.Join(parts, ", ")
}

// chairHangars is the human chair's turn at the war chest: what the CEO's own turn does for a bot
// CEO. The members' roles follow when it moved, and now and then regardless, so a role bundle never
// stays out of step with the hangars.
func (m *Mod) chairHangars(ctx context.Context, c *corpCEO) {
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil || facts == nil {
		return
	}
	moved := false
	if deskDue(c.CorpID, "chair.hangars", chairHangarsEverySec) {
		before, err := m.loadDivProfile(ctx, c.CorpID)
		if err != nil {
			return
		}
		if err := m.ensureCorpDivisions(ctx, c, facts); err != nil {
			rlogf("rolebots: hangars of corp %d: %v", c.CorpID, err)
			return
		}
		forgetDivProfile(c.CorpID)
		if after, err := m.loadDivProfile(ctx, c.CorpID); err == nil {
			moved = !divProfilesEqual(before, after)
		}
	}
	if moved || deskDue(c.CorpID, "chair.roles", chairRolesEverySec) {
		if _, err := m.refreshArmRoles(ctx, c.CorpID); err != nil {
			rlogf("rolebots: arm roles of corp %d: %v", c.CorpID, err)
		}
	}
}

// refreshArmRoles brings every placed member's roles in line with their division's bundle, as it
// stands now: the hangars the division owns, the tier. A bot CEO's turn does this for its
// corporation (ceoArms); a human chair's corporation is placed by hand and would otherwise keep the
// roles of the day it was placed. It touches only the bits an arm grants (armRoleDelta) and answers
// how many members it changed.
func (m *Mod) refreshArmRoles(ctx context.Context, corpID int64) (int, error) {
	members, err := m.corpMembers(ctx, corpID, 0)
	if err != nil {
		return 0, err
	}
	rows, err := m.loadArmRows(ctx, corpID)
	if err != nil {
		return 0, err
	}
	byID := map[int64]corpMember{}
	for _, cm := range members {
		byID[cm.CharID] = cm
	}
	pos := m.armPositions(ctx, corpID)
	benched := m.benchedBuilders(ctx, corpID)
	changed := 0
	for id, r := range rows {
		cm, ok := byID[id]
		if !ok || !cm.Eligible() || !validArm(r.Arm) {
			continue
		}
		tier := r.Tier
		if p, ok := pos[id]; ok && p.Arm == r.Arm && p.Tier > tier {
			tier = p.Tier
		}
		grant, err := m.applyArmRoles(ctx, cm, r.GrantMask, armPlacement{r.Arm, tier}, benched[id])
		if err != nil {
			return changed, err
		}
		if grant != r.GrantMask || tier != r.Tier {
			r.Tier, r.GrantMask = tier, grant
			if err := m.saveArmRow(ctx, r); err != nil {
				return changed, err
			}
			changed++
		}
	}
	if changed > 0 {
		m.refreshArmIndex(ctx)
	}
	return changed, nil
}
