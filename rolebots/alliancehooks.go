package rolebots

// The module's side of the alliance bundle in modapi.CorpHooks (the core overlay installs it from
// core/main/rolebotcorp_mod.go). Every wrapper nil-checks the hook at call time, since a test binary has
// none unless it swaps one in, and falls back to the ESP hook where the panel carries the same push.

import (
	"context"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/modapi"
)

// allianceChanged pushes one alliance edit's notification. kind is one of
// details, member, member-removed, rel-set, rel-del, app, app-removed.
func (m *Mod) allianceChanged(kind string, allianceID, subjectID int64) {
	if h := modapi.Corp().AllianceChanged; h != nil {
		h(kind, allianceID, subjectID)
		return
	}
	if esp.AllianceEventHook != nil {
		esp.AllianceEventHook(kind, allianceID, subjectID)
	}
}

// allianceMembership pushes the allianceid session change to a corporation's
// online pilots and recomputes its sovereignty weight.
func (m *Mod) allianceMembership(corpID, oldAllianceID, newAllianceID int64) {
	if oldAllianceID == newAllianceID {
		return
	}
	if h := modapi.Corp().AllianceMembership; h != nil {
		h(corpID, oldAllianceID, newAllianceID)
		return
	}
	if esp.AllianceMembershipHook != nil {
		esp.AllianceMembershipHook(corpID, oldAllianceID, newAllianceID)
	}
}

// allianceRemoveCorp takes a corporation out of its alliance with every push
// the in-game Kick makes. Without the bundle it falls back to the bare
// database write plus the session change, which is what a client sees on
// relog anyway. left is 0 when the corporation was in no alliance.
// The corporation's pending tithes are voided once it is out (titheCloseOut).
func (m *Mod) allianceRemoveCorp(ctx context.Context, allianceID, corpID int64) (left, executor int64, err error) {
	if h := modapi.Corp().RemoveCorpFromAlliance; h != nil {
		left, executor, err = h(allianceID, corpID)
	} else {
		left, executor, err = evedb.LeaveAlliance(ctx, m.db, corpID)
		if err == nil && left != 0 {
			m.allianceChanged("member-removed", left, corpID)
			m.allianceMembership(corpID, left, 0)
		}
	}
	if err == nil && left != 0 {
		m.titheCloseOut(ctx, corpID, 0, "left the alliance")
	}
	return left, executor, err
}

// allianceAnnounceAdoptions pushes the wars AdoptCorpWars moved onto an
// alliance. No fallback: without the bundle the clients learn on relog.
func (m *Mod) allianceAnnounceAdoptions(adopted []evedb.WarAdoption) {
	if len(adopted) == 0 {
		return
	}
	if h := modapi.Corp().AnnounceWarAdoptions; h != nil {
		h(adopted)
	}
}

// allianceInvalidateNames drops the owner-name and ticker bulks after a
// founding, so a pilot logging in afterwards sees the new alliance's name.
func (m *Mod) allianceInvalidateNames() {
	if h := modapi.Corp().InvalidateOwnerNames; h != nil {
		h()
		return
	}
	if esp.OwnerNamesInvalidator != nil {
		esp.OwnerNamesInvalidator()
	}
}

// allianceInvalidateWarCache drops the core's war pair cache.
func (m *Mod) allianceInvalidateWarCache() {
	if h := modapi.Corp().InvalidateWarCache; h != nil {
		h()
	}
}

// allianceMailPush rings the Alliance tab of every online pilot in the
// alliance for a message already in its mailbox.
func (m *Mod) allianceMailPush(allianceID, msgID, senderID int64, subject string, created int64) {
	if msgID == 0 {
		return
	}
	if h := modapi.Corp().AllianceMailPush; h != nil {
		h(allianceID, msgID, senderID, subject, created)
	}
}

// allianceBulletin posts to the alliance's mailbox from a pilot (the executor
// CEO, normally) and rings every member. Returns the messageID, 0 when the
// mailbox is suppressed or the write failed.
func (m *Mod) allianceBulletin(ctx context.Context, allianceID, from int64, subject, body string) int64 {
	subject = mailSubject(subject)
	created := win32From(clock.Now())
	msgID, err := evedb.SendEveMail(ctx, m.db, allianceID, from, subject, body, created)
	if err != nil {
		rlogf("rolebots: alliance bulletin to %d (%q): %v", allianceID, subject, err)
		return 0
	}
	if msgID != 0 {
		m.allianceMailPush(allianceID, msgID, from, subject, created)
	}
	return msgID
}
