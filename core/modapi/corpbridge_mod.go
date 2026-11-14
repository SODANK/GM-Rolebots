package modapi

import evedb "gurimoon/db"

// CorpHooks is the corporation seam. The corp window only hears about a change
// through pushes sent by package-main handlers, so main installs them from init()
// through InstallCorp. Every field may be nil; check the one you need.
type CorpHooks struct {
	// ApplicationChanged pushes OnCorporationApplicationChanged to the online members
	// and the applicant. A terminal status goes as a REMOVE (the caller soft-deletes
	// the row); any other as a status UPDATE carrying the updater.
	ApplicationChanged func(corpID, applicantID, oldStatus, newStatus, updaterID int64)

	// TitleChanged pushes OnTitleChanged for a title whose name changed, so an open
	// Titles tab updates in place.
	TitleChanged func(corpID, titleID int64, oldName, newName string)

	// OfficeRentalChanged pushes OnOfficeRentalChanged to the online members for a
	// newly rented office (officewrites.go pushOfficeRentalChanged).
	OfficeRentalChanged func(corpID, stationID, officeID, folderID int64)

	// ReleaseOffice gives an office up (evedb.ReleaseOffice dumps its contents to the
	// station floor) and pushes the removal form of OnOfficeRentalChanged. officeID 0
	// means the corporation held no office there.
	ReleaseOffice func(corpID, stationID int64) (officeID, contents int64, err error)

	// MemberRolesChanged pushes the session change carrying a member's role mask
	// (corpwrites.go pushMemberRoleSession). old is the role row read before the
	// write, or nil. A pilot with no connection is a no-op.
	MemberRolesChanged func(charID int64, old *evedb.CorpMemberInfo)

	// SharesChanged pushes OnShareChange to every holder in deltas and the new issued
	// total when it moved, then re-weighs open votes (corpshares.go
	// announceShareChange). Installed whether or not the ESP panel runs.
	SharesChanged func(corpID int64, deltas []evedb.ShareDelta, oldTotal, newTotal int64)

	// AccountChanged pushes OnAccountChange (cash) to an online pilot whose wallet the
	// module moved, since evedb.TransferISK sends no push. No-op if offline.
	AccountChanged func(charID int64, balance float64)

	// DeclareWar runs the core declaration (warwrites.go declareWar): validity, the
	// CONCORD fee from feeCorp, the wars row and the pushes. declaredBy and against are
	// owner IDs. The lone-corp voteWar path is skipped; the bot CEO's decision is the vote.
	DeclareWar func(declaredBy, feeCorp, against int64) (warID int64, err error)

	// RetractWar retracts a war for one of its parties (retractWarFor) and pushes
	// OnWarChanged. The war cools off for 24h, then the war sweep finishes it.
	RetractWar func(ownerID, warID int64) error

	// SetWarMutual flips a war's mutual flag for one of its parties and pushes
	// OnWarChanged. The core flag is one bit either side may flip; the module keeps
	// both sides' consent itself (corpceo_warmutual.go).
	SetWarMutual func(ownerID, warID int64, mutual bool) error

	// PayBill settles one bill (evedb.PayBill) and pushes OnBillReceived to both
	// parties (announceBillPaid).
	PayBill func(billID, payerCharID int64) error

	// CorpMailPush rings the Corp mail tab of every online member for a message
	// already written to the corporation's mailbox. Gangs().PushMail cannot do it,
	// because it filters recipients to one character.
	CorpMailPush func(corpID, msgID, senderID int64, subject string, created int64)

	// LockedItemChanged pushes OnLockedItemChange (corpwrites.go pushLockedItemChange)
	// to the online members after a lock was added or removed. bco_itemLocking caches
	// the lock list for the session.
	LockedItemChanged func(li evedb.LockedItem, added bool)

	// ItemChanged pushes OnItemChange to charID's session for an item the module moved
	// or re-owned. Old IDs must be real: an old locationID of 0 reads to the client as
	// a new item. The item must still exist.
	ItemChanged func(charID, itemID, oldLocationID, oldFlag, oldOwnerID int64)

	// ── the alliance bundle ─────────────────────────────────────────────
	//
	// The esp.*Alliance* hooks cover some of these only while the ESP panel runs, and
	// are set after modapi.InitAll, so a module capturing them in Init would hold nil.
	// These are installed from init().

	// AllianceChanged pushes the OnAlliance*Changed notification for one edit. kind is
	// details, member, member-removed, rel-set, rel-del, app or app-removed; subjectID
	// is the corpID or toID concerned (0 for details).
	AllianceChanged func(kind string, allianceID, subjectID int64)

	// AllianceMembership pushes the allianceid session change to the online members of
	// a corporation that joined or left an alliance and recomputes its sovereignty
	// weight. Founding needs it: CreateAlliance pushes nothing.
	AllianceMembership func(corpID, oldAllianceID, newAllianceID int64)

	// RemoveCorpFromAlliance removes a corporation from its alliance with the pushes
	// and session change (removeCorpFromAlliance). left is 0 if it was in no alliance.
	RemoveCorpFromAlliance func(allianceID, corpID int64) (left, executor int64, err error)

	// AnnounceWarAdoptions pushes the REMOVE and ADD pair for each war moved by
	// evedb.AdoptCorpWars and drops the war pair cache.
	AnnounceWarAdoptions func(adopted []evedb.WarAdoption)

	// InvalidateWarCache drops the war-pair cache WarShotIsLegal reads, for an alliance
	// change that moved no war row.
	InvalidateWarCache func()

	// AllianceMailPush rings the Alliance mail tab of every online pilot in the
	// alliance for a message already in its mailbox. CorpMailPush cannot do it,
	// because it matches a corpID.
	AllianceMailPush func(allianceID, msgID, senderID int64, subject string, created int64)

	// RunAllianceSweep completes accepted applications whose waiting period is up
	// (sweepAllianceApplications). The core also runs it every 300 s.
	RunAllianceSweep func()

	// InvalidateOwnerNames drops the owner-name and alliance-ticker bulk cachables
	// after an alliance is founded or renamed. Installed whether or not the ESP panel
	// runs.
	InvalidateOwnerNames func()
}

var corpHooks CorpHooks

// InstallCorp publishes the core's corporation push entry points. Package main
// calls it once from init(); a later call overwrites.
func InstallCorp(h CorpHooks) { corpHooks = h }

// Corp returns the installed hooks. Every field may be nil.
func Corp() CorpHooks { return corpHooks }
