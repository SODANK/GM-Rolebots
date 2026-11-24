package rolebots

import (
	"context"
	"database/sql"
	"errors"

	"gurimoon/corp"
)

// Who may order what on the Management page. The CEO commands every division; a division head
// commands its own and sees no other; a Director nobody has given a division commands nothing. A
// head is whoever the human CEO appointed (corpheads.go), or in a bot-run corporation the human the
// bot CEO put in charge of a department (armPositions).
//
// The same rule is asked three times: when the page is drawn, when an order is filed, and when the
// desk takes it up (commandTake), because a head may have lost the division in between.

// commandScope is what one pilot may order in their corporation.
type commandScope struct {
	CEO  bool
	Arms map[corpArm]bool
}

// Officer reports whether the scope commands anything at all.
func (s commandScope) Officer() bool { return s.CEO || len(s.Arms) > 0 }

// Commands reports whether the scope covers a division. The corporation's own
// settings ("") are the CEO's alone.
func (s commandScope) Commands(a corpArm) bool {
	return s.CEO || (a != "" && s.Arms[a])
}

// Divisions is the divisions commanded, in portal order.
func (s commandScope) Divisions() []corpArm {
	var out []corpArm
	for _, a := range portalArmOrder {
		if a != "" && s.Commands(a) {
			out = append(out, a)
		}
	}
	return out
}

func holdsDirector(roles uint64) bool { return roles&corp.RoleDirector != 0 }
func holdsTrader(roles uint64) bool   { return roles&corp.RoleTrader != 0 }

// memberRank is a pilot's standing in a corporation, read from the database in one go.
type memberRank struct {
	Member, CEO, Director bool
}

// memberRankIn is memberRankErr for whoever may take "cannot say" as "no".
func (m *Mod) memberRankIn(ctx context.Context, corpID, charID int64) memberRank {
	r, _ := m.memberRankErr(ctx, corpID, charID)
	return r
}

func (m *Mod) memberRankErr(ctx context.Context, corpID, charID int64) (memberRank, error) {
	var r memberRank
	if corpID == 0 || charID == 0 {
		return r, nil
	}
	var cur, ceo int64
	// corprole is BIGINT UNSIGNED and a CEO holds every bit (18446744073709551615), which overflows
	// an int64 and failed the whole scan, leaving the CEO looking like a stranger.
	var roles sql.Null[uint64]
	err := m.db.QueryRowContext(ctx, `
		SELECT ch.corporationID, COALESCE(c.ceoID, 0), cr.corprole
		  FROM character_ ch
		  JOIN corporation c ON c.corporationID = ch.corporationID
		  LEFT JOIN chrCorporationRoles cr ON cr.characterID = ch.characterID
		 WHERE ch.characterID = ?`, charID).Scan(&cur, &ceo, &roles)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil || cur != corpID {
		return r, err
	}
	r.Member = true
	r.CEO = ceo == charID
	r.Director = r.CEO || (roles.Valid && holdsDirector(roles.V))
	return r, nil
}

// commandScopeOf is what a pilot may order. A bot commands nothing here: its desk does.
func (m *Mod) commandScopeOf(ctx context.Context, corpID, charID int64) commandScope {
	s, err := m.commandScopeErr(ctx, corpID, charID)
	if err != nil {
		return commandScope{}
	}
	return s
}

func (m *Mod) commandScopeErr(ctx context.Context, corpID, charID int64) (commandScope, error) {
	if !m.seatedByPerson(ctx, charID) {
		return commandScope{}, nil
	}
	r, err := m.memberRankErr(ctx, corpID, charID)
	switch {
	case err != nil:
		return commandScope{}, err
	case r.CEO:
		return commandScope{CEO: true}, nil
	case !r.Director:
		return commandScope{}, nil
	}
	s := commandScope{Arms: map[corpArm]bool{}}
	if _, bot := managedCEO(corpID); bot {
		if p, ok := m.armPositions(ctx, corpID)[charID]; ok && p.Tier == armTierHead {
			s.Arms[p.Arm] = true
		}
		return s, nil
	}
	heads, err := m.corpHeadsErr(ctx, corpID)
	if err != nil {
		return commandScope{}, err
	}
	for a, h := range heads {
		if h.CharID == charID {
			s.Arms[a] = true
		}
	}
	return s, nil
}

// managementVisible is the Management tab's own test (esp.CorpPortalSection.Visible): the CEO and
// the Directors, people only.
func (m *Mod) managementVisible(ctx context.Context, corpID, charID int64) bool {
	if !m.seatedByPerson(ctx, charID) {
		return false
	}
	return m.memberRankIn(ctx, corpID, charID).Director
}

// seatedByPerson reports whether a person sits at this character: not a rolebot, or a roster
// character the operator switched off (enabled = 0) and took over to play (personSeat). isSynthetic counts
// the latter as a bot, which would lock its human out of their own corporation's Management.
func (m *Mod) seatedByPerson(_ context.Context, charID int64) bool {
	if charID == 0 {
		return false
	}
	return !m.isSynthetic(charID) || m.personSeat(charID)
}

// mayCancelCommand reports whether a pilot may withdraw an order: the CEO any of them, a head its
// own division's unless the CEO gave it. Pure.
func mayCancelCommand(s commandScope, c corpCommand) bool {
	if s.CEO {
		return true
	}
	return !c.PosterCEO && s.Commands(corpArm(c.Arm))
}

// commandOutranks reports whether an order in force stops a new one on the same target: the CEO's
// stops a head's, a head's stops nobody's. Pure.
func commandOutranks(inForce corpCommand, s commandScope) bool {
	return inForce.PosterCEO && !s.CEO
}
