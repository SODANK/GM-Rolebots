package rolebots

import (
	"context"
	"fmt"
	"math"
)

// Who a bot CEO leads: every member, not only the roster. A human who joined a bot-run corporation is a member
// like any other, eligible for every office, seat, title and wage, so leadership must not read candidates
// through a JOIN on rolebot_characters. corpMembers is the one reader those passes share: it returns every
// pilot wearing the badge (accountID > 0 separates pilots from NPC agents), marks which are on the roster, and
// leaves a disabled bot out of every appointment.
// A human has no traits. memberIdentity gives it a revealed profile instead (memberprofile.go): the appetites
// its last thirty days of work show, never below an even temperament.
// Two things stay bot-only: the CEO's chair (ceoSuccessor: a bot CEO's successor must be a bot or the
// corporation stops being bot-run) and anything the module has to fly (idleFleetCommanders only forms fleets
// around a pilot it can drive; a human's fleet seat is a title and an authority).

// corpMember is one pilot of a corporation as leadership sees it.
type corpMember struct {
	CharID    int64
	Name      string
	Roles     uint64
	TitleMask uint64
	Title     string
	Since     int64 // corporationDateTime, win32; 0 when unknown
	Balance   float64
	OnRoster  bool
	Enabled   bool
}

// Human reports whether the member is a player rather than a rostered bot.
func (cm corpMember) Human() bool { return !cm.OnRoster }

// Eligible reports whether leadership may appoint or pay the member: every
// human, and every ENABLED bot.
func (cm corpMember) Eligible() bool { return !cm.OnRoster || cm.Enabled }

// tenureDays is how long a member has worn the badge, in days, at least 0.
func tenureDays(sinceWin32, nowWin32 int64) float64 {
	if sinceWin32 <= 0 || nowWin32 <= sinceWin32 {
		return 0
	}
	return float64(nowWin32-sinceWin32) / 864000000000.0
}

// tenureTrust is the service half of every trust score: the square root of
// days served, from 1 so a member of a day is not worth nothing.
func tenureTrust(sinceWin32, nowWin32 int64) float64 {
	return math.Sqrt(1 + tenureDays(sinceWin32, nowWin32))
}

// corpMembers reads every pilot of a corporation except the one excluded
// (usually the CEO), eligible or not; callers filter on Eligible.
func (m *Mod) corpMembers(ctx context.Context, corpID, exclude int64) ([]corpMember, error) {
	tc := thinkCacheOf(ctx, corpID)
	if tc != nil {
		if out, ok := tc.cachedMembers(exclude); ok {
			return out, nil
		}
	}
	out, err := m.readCorpMembers(ctx, corpID, exclude)
	if err == nil && tc != nil {
		tc.keepMembers(exclude, out)
	}
	return out, err
}

func (m *Mod) readCorpMembers(ctx context.Context, corpID, exclude int64) ([]corpMember, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID, ch.characterName, COALESCE(r.corprole, 0), COALESCE(r.titleMask, 0),
		       COALESCE(ch.title, ''), COALESCE(ch.corporationDateTime, 0), COALESCE(ch.balance, 0),
		       b.characterID IS NOT NULL, COALESCE(b.enabled, 0)
		  FROM character_ ch
		  LEFT JOIN rolebot_characters b ON b.characterID = ch.characterID
		  LEFT JOIN chrCorporationRoles r ON r.characterID = ch.characterID
		 WHERE ch.corporationID = ? AND ch.accountID > 0 AND ch.characterID <> ?
		 ORDER BY ch.characterID`, corpID, exclude)
	if err != nil {
		return nil, fmt.Errorf("corpMembers: %w", err)
	}
	defer rows.Close()
	var out []corpMember
	for rows.Next() {
		var cm corpMember
		var enabled int64
		if err := rows.Scan(&cm.CharID, &cm.Name, &cm.Roles, &cm.TitleMask, &cm.Title,
			&cm.Since, &cm.Balance, &cm.OnRoster, &enabled); err != nil {
			return nil, fmt.Errorf("corpMembers scan: %w", err)
		}
		cm.Enabled = enabled != 0
		// An orphan generated pilot (no roster row, RoleBot account) is still a bot (isSynthetic). It stays ineligible
		// because nothing drives it.
		if !cm.OnRoster && m.isSynthetic(cm.CharID) {
			cm.OnRoster = true
		}
		out = append(out, cm)
	}
	return out, rows.Err()
}

// memberIdentity is the pilot identity every leadership score runs on: a
// bot's own traits, or a human's revealed profile.
func (m *Mod) memberIdentity(ctx context.Context, cm corpMember) pilotIdentity {
	if cm.OnRoster {
		traits, err := getTraits(ctx, m.db, cm.CharID)
		if err == nil {
			p := pilotIdentity{CharID: cm.CharID, Name: cm.Name, Enabled: cm.Enabled, Traits: traits, Tune: resolveTuning(traits)}
			// The loyalist bloc (grace_ledger.go): regard for its own bot CEO, 0..1.
			if ceo, ok := managedCEO(m.charCorpID(ctx, cm.CharID)); ok && ceo != cm.CharID {
				p.Favour = math.Max(0, math.Min(1, graceOpinionOf(cm.CharID, ceo)/5))
			}
			return p
		}
	}
	return pilotIdentity{CharID: cm.CharID, Name: cm.Name, Enabled: true, Traits: map[string]int{}, Tune: m.revealedTuning(ctx, cm.CharID)}
}

// leadsHumans reports whether leadership passes over a corporation include
// its human members: only a corporation a bot CEO runs. An NPC corporation's
// seats, and a player corporation run by a human, stay the roster's.
func leadsHumans(corpID int64) bool {
	_, ok := managedCEO(corpID)
	return ok
}
