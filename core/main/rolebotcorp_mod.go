package main

import (
	"context"
	"fmt"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	"gurimoon/modapi"
	"gurimoon/server"
	"gurimoon/server/infocache"
)

// init installs the corporation push seam (modapi.CorpHooks). Each entry point
// builds the payload the client's own corp handlers send (corpwrites.go) and uses
// the same helpers. Overlay build only.
func init() {
	modapi.InstallCorp(modapi.CorpHooks{
		ApplicationChanged: modCorpApplicationChanged,
		TitleChanged:       modCorpTitleChanged,
		OfficeRentalChanged: func(corpID, stationID, officeID, folderID int64) {
			pushOfficeRentalChanged(globalSessReg, corpID, stationID,
				marshal.PyInt{Value: uint64(officeID)}, marshal.PyInt{Value: uint64(folderID)})
		},
		ReleaseOffice: func(corpID, stationID int64) (int64, int64, error) {
			if globalDB == nil {
				return 0, 0, fmt.Errorf("the server is not ready")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			officeID, _, contents, err := evedb.ReleaseOffice(ctx, globalDB, corpID, stationID)
			if err != nil || officeID == 0 {
				return officeID, contents, err
			}
			pushOfficeRentalChanged(globalSessReg, corpID, stationID, marshal.PyNone{}, marshal.PyNone{})
			return officeID, contents, nil
		},
		MemberRolesChanged: func(charID int64, old *evedb.CorpMemberInfo) {
			if globalDB == nil {
				return
			}
			pushMemberRoleSession(context.Background(), globalDB, charID, old, false)
		},
		SharesChanged: func(corpID int64, deltas []evedb.ShareDelta, oldTotal, newTotal int64) {
			if globalDB == nil || globalSessReg == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			announceShareChange(ctx, globalDB, globalSessReg, corpID, deltas, oldTotal, newTotal)
		},
		AccountChanged: func(charID int64, balance float64) {
			if globalSessReg == nil {
				return
			}
			server.NotifyCharacter(globalSessReg, charID, "OnAccountChange", "clientID", marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "cash"},
				marshal.PyInt{Value: uint64(charID)},
				marshal.PyReal{Value: balance},
			}})
		},
		DeclareWar: func(declaredBy, feeCorp, against int64) (int64, error) {
			if globalDB == nil {
				return 0, fmt.Errorf("the server is not ready")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			w, err := declareWar(ctx, globalDB, globalSessReg, declaredBy, feeCorp, against, false)
			invalidateWarCache()
			if err != nil {
				return 0, err
			}
			return w.WarID, nil
		},
		RetractWar: func(ownerID, warID int64) error {
			if globalDB == nil {
				return fmt.Errorf("the server is not ready")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err := retractWarFor(ctx, globalDB, globalSessReg, ownerID, warID)
			invalidateWarCache()
			return err
		},
		SetWarMutual: func(ownerID, warID int64, mutual bool) error {
			if globalDB == nil {
				return fmt.Errorf("the server is not ready")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return setWarMutualFor(ctx, globalDB, globalSessReg, ownerID, warID, mutual)
		},
		PayBill: func(billID, payerCharID int64) error {
			if globalDB == nil {
				return fmt.Errorf("the server is not ready")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			bill, err := evedb.PayBill(ctx, globalDB, billID, payerCharID)
			if err != nil {
				return err
			}
			announceBillPaid(ctx, globalDB, globalSessReg, bill)
			invalidateWarCache()
			return nil
		},
		CorpMailPush: modCorpMailPush,
		LockedItemChanged: func(li evedb.LockedItem, added bool) {
			if globalSessReg == nil {
				return
			}
			pushLockedItemChange(globalSessReg, li, added)
		},
		ItemChanged: modItemChanged,

		// The alliance bundle: each body matches an ESP hook in main.go's ESP block, but
		// works without the panel. The globals are nil until main() sets them.
		AllianceChanged: func(kind string, allianceID, subjectID int64) {
			if globalDB == nil {
				return
			}
			espAllianceRefresh(globalDB, globalSessReg, kind, allianceID, subjectID)
		},
		AllianceMembership: func(corpID, oldAllianceID, newAllianceID int64) {
			if globalDB == nil {
				return
			}
			pushAllianceSession(globalSessReg, corpID, oldAllianceID, newAllianceID)
		},
		RemoveCorpFromAlliance: func(allianceID, corpID int64) (int64, int64, error) {
			if globalDB == nil {
				return 0, 0, fmt.Errorf("the server is not ready")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			left, executor, err := removeCorpFromAlliance(ctx, globalDB, globalSessReg, allianceID, corpID)
			invalidateWarCache()
			return left, executor, err
		},
		AnnounceWarAdoptions: func(adopted []evedb.WarAdoption) {
			if globalDB == nil || len(adopted) == 0 {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			announceWarAdoptions(ctx, globalDB, globalSessReg, adopted)
			invalidateWarCache()
		},
		InvalidateWarCache: invalidateWarCache,
		AllianceMailPush:   modAllianceMailPush,
		RunAllianceSweep: func() {
			if globalDB == nil {
				return
			}
			sweepAllianceApplications(globalDB, globalSessReg)
		},
		InvalidateOwnerNames: func() {
			if globalCacheStore != nil {
				globalCacheStore.InvalidateOwnerNames()
			}
			infocache.Shared.Clear()
		},
	})
}

// modAllianceMailPush mirrors LSC.Page's OnMessage push for an alliance mailbox.
// OnlineRecipients matches a corpID, so the member corporations are resolved first;
// the inbox files the mail under the Alliance tab.
func modAllianceMailPush(allianceID, msgID, senderID int64, subject string, created int64) {
	if globalLSCReg == nil || globalDB == nil || msgID == 0 || allianceID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	members, err := evedb.ListAllianceMembers(ctx, globalDB, allianceID)
	if err != nil || len(members) == 0 {
		return
	}
	corpIDs := make([]int64, 0, len(members))
	for _, mb := range members {
		corpIDs = append(corpIDs, mb.CorporationID)
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(allianceID)}}},
		marshal.PyInt{Value: uint64(msgID)},
		marshal.PyInt{Value: uint64(senderID)},
		marshal.PyString{Value: subject},
		marshal.PyInt{Value: uint64(created)},
	}}
	for _, m := range globalLSCReg.OnlineRecipients(corpIDs...) {
		_ = server.SendNotification(m.Conn, m.AccountID, "OnMessage", "charid", args)
	}
}

// modCorpMailPush mirrors LSC.Page's OnMessage push for a corporation mailbox to
// every online member; the inbox files it under the Corp tab.
func modCorpMailPush(corpID, msgID, senderID int64, subject string, created int64) {
	if globalLSCReg == nil || msgID == 0 || corpID == 0 {
		return
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(corpID)}}},
		marshal.PyInt{Value: uint64(msgID)},
		marshal.PyInt{Value: uint64(senderID)},
		marshal.PyString{Value: subject},
		marshal.PyInt{Value: uint64(created)},
	}}
	for _, m := range globalLSCReg.OnlineRecipients(corpID) {
		_ = server.SendNotification(m.Conn, m.AccountID, "OnMessage", "charid", args)
	}
}

// modCorpApplicationChanged mirrors UpdateApplicationOffer's two push shapes.
// Nil-safe before main assigns the session registry.
func modCorpApplicationChanged(corpID, applicantID, oldStatus, newStatus, updaterID int64) {
	if globalSessReg == nil {
		return
	}
	if evedb.IsTerminalAppStatus(newStatus) {
		rem := appRemoveNotification(corpID, applicantID, oldStatus)
		notifyCorp(globalSessReg, corpID, "OnCorporationApplicationChanged", rem)
		server.NotifyCharacter(globalSessReg, applicantID, "OnCorporationApplicationChanged", "charid", rem)
		return
	}
	appChange := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(applicantID)},
		marshal.PyInt{Value: uint64(corpID)},
		marshal.PyDict{Items: []marshal.DictEntry{
			chg("status", marshal.PyInt{Value: uint64(oldStatus)}, marshal.PyInt{Value: uint64(newStatus)}),
			chg("lastCorpUpdaterID", marshal.PyNone{}, marshal.PyInt{Value: uint64(updaterID)}),
		}},
	}}
	notifyCorp(globalSessReg, corpID, "OnCorporationApplicationChanged", appChange)
	server.NotifyCharacter(globalSessReg, applicantID, "OnCorporationApplicationChanged", "charid", appChange)
}

// modCorpTitleChanged mirrors UpdateTitles' OnTitleChanged for a name change. The
// dict carries the name and eight unchanged zero role masks the client reads.
func modCorpTitleChanged(corpID, titleID int64, oldName, newName string) {
	if globalSessReg == nil {
		return
	}
	cols := []string{"roles", "grantableRoles", "rolesAtHQ", "grantableRolesAtHQ",
		"rolesAtBase", "grantableRolesAtBase", "rolesAtOther", "grantableRolesAtOther"}
	entries := []marshal.DictEntry{
		chg("titleName", marshal.PyString{Value: oldName}, marshal.PyString{Value: newName}),
	}
	for _, c := range cols {
		entries = append(entries, chg(c, marshal.PyInt{Value: 0}, marshal.PyInt{Value: 0}))
	}
	notifyCorp(globalSessReg, corpID, "OnTitleChanged", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(corpID)},
		marshal.PyInt{Value: uint64(titleID)},
		marshal.PyDict{Items: entries},
	}})
}

// modItemChanged is CorpHooks.ItemChanged: OnItemChange for one moved row, sent to
// the pilot whose windows show it.
func modItemChanged(charID, itemID, oldLocationID, oldFlag, oldOwnerID int64) {
	if globalDB == nil || globalSessReg == nil || charID == 0 || itemID == 0 {
		return
	}
	s := globalSessReg.FindByCharID(charID)
	if s == nil || s.Conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	er, err := evedb.GetEntityRow(ctx, globalDB, itemID)
	if err != nil || er == nil {
		return
	}
	change := marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocationID)}},
		{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
		{Key: marshal.PyInt{Value: 2}, Value: marshal.PyInt{Value: uint64(oldOwnerID)}},
	}}
	_ = server.SendNotification(s.Conn, s.AccountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		evedb.BuildEntityRow(er), change,
	}})
}
