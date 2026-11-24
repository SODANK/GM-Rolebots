package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	evedb "gurimoon/db"
)

// The tower laboratory: originals out for a high-priority research rotation. Originals are the
// corporation's research and copy engine and are locked in the office (corpvault.go). A tower
// laboratory is where one goes when research or copying is urgent: a starved type's copies, the
// chair's own orders, anything on the board at towerLabPriority or above.
// The original goes out in a Director's (or the CEO's) hold, never anybody else's (posHaulDue,
// legCarriesOriginal), and stays at the array while it has urgent work. When no urgent order names
// it any more it is carried home to the library, where the vault keeper locks it again. Every copy
// a tower job makes comes home to the factory office's library division at once.
// A tower lab never takes ordinary research (corpResearchPriority, 5): an original out of the vault
// is a risk worth taking only for work that cannot wait.

// towerLabPriority is the board priority from which research and copy
// orders go out to a tower laboratory: starved copies (60) and the chair's
// orders (100).
const towerLabPriority = corpCopyStarvedPriority

// labActivity reports whether a work activity runs in a laboratory
// (research or copying). Pure.
func labActivity(act int) bool {
	return act == ramActivityPEResearch || act == ramActivityMEResearch || act == ramActivityCopying
}

// towerLabTakes is whether an order goes to a tower laboratory: a lab
// activity, urgent, naming its original, with runs left. Pure.
func towerLabTakes(w corpWork) bool {
	return labActivity(workActivity(w.Kind)) && w.Priority >= towerLabPriority && w.BPItemID != 0 && w.RunsLeft() > 0
}

// legCarriesOriginal reports whether a haul leg moves a blueprint original.
// Pure.
func legCarriesOriginal(l *haulLeg) bool {
	for _, it := range l.Items {
		if it.Original {
			return true
		}
	}
	return false
}

// towerOriginalWanted is whether an urgent order still names this original.
func (m *Mod) towerOriginalWanted(ctx context.Context, corpID, itemID int64) bool {
	work, err := m.openWorkFor(ctx, corpID)
	if err != nil {
		return false
	}
	for _, w := range work {
		if w.BPItemID == itemID && towerLabTakes(w) {
			return true
		}
	}
	return false
}

// feedTowerLab gives an idle laboratory array its next urgent job: an
// original already resident is put straight back to work; otherwise the
// highest-priority urgent order whose original sits in the factory office is
// sent out, in a Director's hold. True when it gave the array something.
func (m *Mod) feedTowerLab(ctx context.Context, c *corpCEO, p *towerPlan, arr evedb.POSInstallation, stationID, officeID int64) bool {
	work, err := m.openWorkFor(ctx, c.CorpID)
	if err != nil {
		return false
	}
	resident := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID FROM entity e JOIN invBlueprints b ON b.blueprintID = e.itemID AND b.copy = 0
		 WHERE e.locationID = ? AND e.ownerID = ? AND e.flag <> 111`, arr.ArrayID, c.CorpID)
	if err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				resident[id] = true
			}
		}
		rows.Close()
	}
	// A resident original first: the rotation.
	for _, home := range []bool{false, true} {
		for _, w := range work {
			if !towerLabTakes(w) || resident[w.BPItemID] == home {
				continue
			}
			var loc, flag int64
			if m.db.QueryRowContext(ctx, `SELECT locationID, flag FROM entity WHERE itemID = ? AND ownerID = ?`,
				w.BPItemID, c.CorpID).Scan(&loc, &flag) != nil || flag == 111 {
				continue // gone, or in a job
			}
			if home && loc != officeID {
				continue // only out of the factory office
			}
			act := workActivity(w.Kind)
			// Every level or copy the order asks for, in one job.
			runs := w.RunsLeft()
			got, err := m.claimWorkRuns(ctx, w, arr.ArrayID, runs)
			if err != nil || got <= 0 {
				continue
			}
			if home {
				if _, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindFeed, PlanID: p.PlanID, FromKind: haulFromOffice,
					FromID: officeID, FromStationID: stationID, FromFlag: flag, ToKind: haulToTower, ToID: arr.ArrayID,
					ToSystemID: p.SystemID, ToFlag: evedb.FlagCorpHangarDiv1,
					Items:    []haulItem{{TypeID: w.BPTypeID, Qty: 1, Flag: flag, ItemID: w.BPItemID, Original: true}},
					PostedBy: "industry", Note: "an original out to the tower lab for urgent work"}); err != nil {
					m.releaseWorkClaim(ctx, w.WorkID, arr.ArrayID)
					continue
				}
			}
			_, _ = m.db.ExecContext(ctx, `
				INSERT INTO rolebot_corp_towerjobs (arrayID, corporationID, planID, workID, bpTypeID, activityID, runs, status, postedAt)
				VALUES (?, ?, ?, ?, ?, ?, ?, 'feeding', ?)`,
				arr.ArrayID, c.CorpID, p.PlanID, w.WorkID, w.BPTypeID, act, got, clock.Unix())
			verb := "kept its original of %s at the tower lab on %s for the next urgent job"
			if home {
				verb = "sent its original of %s out to the tower lab on %s — urgent research, in a Director's hold"
			}
			m.ceoLog(ctx, c, powerStarbase, "towerlab", w.BPTypeID, 0, verb,
				m.typeNameOr(ctx, w.BPTypeID), moonNameOf(ctx, m, p.MoonID))
			return true
		}
	}
	return false
}

// returnArrayOutput hauls an array's contents home to the factory office: products to Production;
// every copy to the library at once; an original to the library only once no urgent order names
// it, until then it stays for the rotation (feedTowerLab). Prints go by itemID, since an original
// and its copies share a type.
func (m *Mod) returnArrayOutput(ctx context.Context, c *corpCEO, p *towerPlan, arr evedb.POSInstallation) {
	stationID, officeID := m.corpFactoryOffice(ctx, c.CorpID)
	if officeID == 0 {
		return
	}
	type print struct {
		itemID, typeID int64
		copy           bool
	}
	var prints []print
	isPrint := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, COALESCE(b.copy, 0) FROM entity e
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		 WHERE e.locationID = ? AND e.ownerID = ? AND e.flag <> 111`, arr.ArrayID, c.CorpID)
	if err == nil {
		for rows.Next() {
			var pr print
			if rows.Scan(&pr.itemID, &pr.typeID, &pr.copy) == nil {
				prints = append(prints, pr)
				isPrint[pr.typeID] = true
			}
		}
		rows.Close()
	}
	var goods, copies, originals []haulItem
	for t, q := range m.bayHeldAll(ctx, arr.ArrayID) {
		if q > 0 && !isPrint[t] {
			goods = append(goods, haulItem{TypeID: t, Qty: q, Flag: -1})
		}
	}
	for _, pr := range prints {
		switch {
		case pr.copy:
			copies = append(copies, haulItem{TypeID: pr.typeID, Qty: 1, Flag: -1, ItemID: pr.itemID})
		case !m.towerOriginalWanted(ctx, c.CorpID, pr.itemID):
			originals = append(originals, haulItem{TypeID: pr.typeID, Qty: 1, Flag: -1, ItemID: pr.itemID, Original: true})
		}
	}
	post := func(items []haulItem, flag int64, note string) {
		if len(items) == 0 {
			return
		}
		_, _ = m.postHaul(ctx, c, haulLeg{Kind: haulKindBackhaul, PlanID: p.PlanID, FromKind: haulFromStructure,
			FromID: arr.ArrayID, FromSystemID: p.SystemID, ToKind: haulToOffice, ToStationID: stationID,
			ToFlag: flag, Items: items, PostedBy: "industry", Note: note})
	}
	post(goods, corpDivProduct, "tower job output home")
	post(copies, corpDivLibrary, "blueprint copies home to the library")
	post(originals, corpDivLibrary, "an original home from the tower lab: its urgent work is done")
}

// takeOriginalFromOffice moves one named original out of a corporation
// office into a hold, unlocking it first (corpvault.go): a tower cannot hold
// a lock, and the keeper locks it again once it is home.
func (m *Mod) takeOriginalFromOffice(ctx context.Context, corpID, officeID, itemID, shipID int64) (int64, error) {
	if itemID == 0 || m.lockedByVote(ctx, corpID, itemID) {
		return 0, nil
	}
	if _, err := m.unlockPrint(ctx, corpID, itemID); err != nil {
		return 0, fmt.Errorf("unlock original %d: %w", itemID, err)
	}
	res, err := m.db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = ? WHERE itemID = ? AND ownerID = ? AND locationID = ? AND flag <> 111`,
		shipID, flagCargo, itemID, corpID, officeID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// takeItemFromStructure moves one named item out of a structure the
// corporation owns into a hold.
func (m *Mod) takeItemFromStructure(ctx context.Context, corpID, structureID, itemID, shipID int64) (int64, error) {
	var owner int64
	err := m.db.QueryRowContext(ctx, `SELECT ownerID FROM entity WHERE itemID = ? AND locationID = ? AND flag <> 111`,
		itemID, structureID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil || owner != corpID {
		return 0, err
	}
	res, err := m.db.ExecContext(ctx, `UPDATE entity SET locationID = ?, flag = ? WHERE itemID = ? AND locationID = ?`,
		shipID, flagCargo, itemID, structureID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
