package rolebots

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// The Ship Replacement Program: the corporation replaces what its members lose flying for
// it, out of what it builds. A member's hull loss (a killmail with the corporation as the
// victim's, not a pod) is judged once and owed when: the victim is still a member with
// srpMinTenureDays served; it died in a region of one of the corporation's offices or its
// headquarters; the hull was worth no more than the charter's SRPCap; and it is the member's
// first claim in srpClaimGapSec. It is paid in kind: the next hull of that type the factory
// finishes goes to the member's hangar, and until then the loss is top-priority work on the
// board. A claim nobody could build within srpOwedSec lapses. Humans and bots are judged
// alike; a human is mailed when its hull is waiting.

const (
	srpWindowSec     = 7 * 86400
	srpMinTenureDays = 7
	srpClaimGapSec   = 3 * 86400
	srpOwedSec       = 14 * 86400
	srpPriority      = 100
)

// srpRegions is where a corporation works: its offices' regions and its
// headquarters'.
func (m *Mod) srpRegions(ctx context.Context, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	ids, err := int64Column(ctx, m, `
		SELECT DISTINCT s.regionID FROM staStations s
		 WHERE s.stationID IN (SELECT stationID FROM crpoffices WHERE corporationID = ?)
		    OR s.stationID = (SELECT COALESCE(stationID, 0) FROM corporation WHERE corporationID = ?)`,
		corpID, corpID)
	if err == nil {
		for _, id := range ids {
			out[id] = true
		}
	}
	return out
}

// srpLoss is one loss as the judge sees it.
type srpLoss struct {
	KillID, CharID, ShipTypeID, RegionID int64
	Value                                float64
	LostAt                               int64 // unix
	StillMember                          bool
	JoinedWin32                          int64
	LastClaimAt                          int64 // unix, 0 for none
	// Kit marks a corporation kit lent for the op (corparmory_issue.go): the
	// corporation owned the hull, so there is nothing to replace.
	Kit bool
}

// srpEligible is the pure judgement. why is empty for an owed claim.
func srpEligible(l srpLoss, regions map[int64]bool, cap float64) string {
	switch {
	case l.Kit:
		return "a corporation kit, not the pilot's ship"
	case !l.StillMember:
		return "no longer a member"
	case l.JoinedWin32 > 0 && l.JoinedWin32 > evedb.UnixToWin32(l.LostAt-srpMinTenureDays*86400):
		return "had not served a week"
	case !regions[l.RegionID]:
		return "lost outside the corporation's space"
	case cap <= 0 || l.Value > cap:
		return "worth more than the corporation replaces"
	case l.LastClaimAt > 0 && l.LostAt-l.LastClaimAt < srpClaimGapSec:
		return "a second claim inside three days"
	}
	return ""
}

// ceoSRP judges new losses, pays what it can, and lapses what it cannot.
func (m *Mod) ceoSRP(ctx context.Context, c *corpCEO, ch Charter, stationID, officeID int64) error {
	now := clock.Unix()
	rows, err := m.db.QueryContext(ctx, `
		SELECT k.killID, k.victimCharID, k.shipTypeID, k.regionID, k.shipValue, k.killTime,
		       (COALESCE(ch.corporationID, 0) = ?), COALESCE(ch.corporationDateTime, 0),
		       COALESCE((SELECT MAX(s.lostAt) FROM rolebot_corp_srp s
		                  WHERE s.characterID = k.victimCharID AND s.corporationID = ?
		                    AND s.status IN ('owed', 'granted')), 0),
		       EXISTS (SELECT 1 FROM rolebot_corp_issue i
		                WHERE i.characterID = k.victimCharID AND i.hullTypeID = k.shipTypeID
		                  AND i.status = 'lost' AND ABS(i.closedAt - k.killTime) <= 3600)
		  FROM killmails k
		  LEFT JOIN character_ ch ON ch.characterID = k.victimCharID
		  LEFT JOIN rolebot_corp_srp done ON done.killID = k.killID
		 WHERE k.victimCorpID = ? AND k.isPod = 0 AND k.killTime >= ? AND done.killID IS NULL
		   AND k.victimCharID > 0
		 ORDER BY k.killTime
		 LIMIT 50`, c.CorpID, c.CorpID, c.CorpID, now-srpWindowSec)
	if err != nil {
		return fmt.Errorf("srp losses: %w", err)
	}
	var losses []srpLoss
	for rows.Next() {
		var l srpLoss
		if rows.Scan(&l.KillID, &l.CharID, &l.ShipTypeID, &l.RegionID, &l.Value, &l.LostAt,
			&l.StillMember, &l.JoinedWin32, &l.LastClaimAt, &l.Kit) == nil {
			losses = append(losses, l)
		}
	}
	rows.Close()
	regions := m.srpRegions(ctx, c.CorpID)
	lastByChar := map[int64]int64{}
	for _, l := range losses {
		if at, ok := lastByChar[l.CharID]; ok && at > l.LastClaimAt {
			l.LastClaimAt = at
		}
		status := "owed"
		// Rank raises the cap a member's hull may be worth (corpmerit.go).
		why := srpEligible(l, regions, ch.SRPCap*m.meritPayFor(ctx, c.CorpID, l.CharID))
		if why != "" {
			status = "void"
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT IGNORE INTO rolebot_corp_srp (killID, corporationID, characterID, shipTypeID, value, status, lostAt)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, l.KillID, c.CorpID, l.CharID, l.ShipTypeID, l.Value, status, l.LostAt); err != nil {
			return err
		}
		if status == "owed" {
			lastByChar[l.CharID] = l.LostAt
			m.ceoLog(ctx, c, powerProduction, "srp", l.CharID, l.Value,
				"owes %s a %s — lost flying for the corporation", m.charNameOr(ctx, l.CharID), m.typeNameOr(ctx, l.ShipTypeID))
		}
	}

	// Lapse what nobody could build in time.
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_srp SET status = 'lapsed' WHERE corporationID = ? AND status = 'owed' AND lostAt < ?`,
		c.CorpID, now-srpOwedSec)

	// Pay in kind from Production or the store.
	owed, err := m.db.QueryContext(ctx, `
		SELECT s.killID, s.characterID, s.shipTypeID, s.value
		  FROM rolebot_corp_srp s
		  JOIN character_ ch ON ch.characterID = s.characterID AND ch.corporationID = s.corporationID
		 WHERE s.corporationID = ? AND s.status = 'owed'
		 ORDER BY s.lostAt`, c.CorpID)
	if err != nil {
		return err
	}
	type claim struct {
		killID, charID, typeID int64
		value                  float64
	}
	var claims []claim
	for owed.Next() {
		var cl claim
		if owed.Scan(&cl.killID, &cl.charID, &cl.typeID, &cl.value) == nil {
			claims = append(claims, cl)
		}
	}
	owed.Close()
	for _, cl := range claims {
		itemID, err := m.handOverHull(ctx, c.CorpID, officeID, stationID, cl.charID, cl.typeID)
		if err != nil || itemID == 0 {
			continue
		}
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_srp SET status = 'granted', itemID = ?, grantedAt = ? WHERE killID = ?`,
			itemID, now, cl.killID)
		value := evedb.TypeValueOf(ctx, m.db, cl.typeID).Value
		m.corpBook(ctx, c.CorpID, lineSRP, -value, cl.charID,
			fmt.Sprintf("%s replaced for %s", m.typeNameOr(ctx, cl.typeID), m.charNameOr(ctx, cl.charID)))
		m.ceoLog(ctx, c, powerProduction, "srpgrant", cl.charID, value,
			"replaced %s's %s from the corporation's own production", m.charNameOr(ctx, cl.charID),
			m.typeNameOr(ctx, cl.typeID))
		m.mailSRPGrant(ctx, c, cl.charID, cl.typeID, stationID)
	}
	return nil
}

// handOverHull moves one hull of a type from the factory's Production or
// Company Store division to a member's own hangar at the station.
func (m *Mod) handOverHull(ctx context.Context, corpID, officeID, stationID, charID, typeID int64) (int64, error) {
	var itemID, qty int64
	err := m.db.QueryRowContext(ctx, `
		SELECT itemID, quantity FROM entity
		 WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag IN (?, ?) AND quantity > 0
		 ORDER BY (flag = ?) DESC, quantity
		 LIMIT 1`, corpID, officeID, typeID, corpDivProduct, corpDivStore, corpDivProduct).Scan(&itemID, &qty)
	if err != nil {
		return 0, nil // none on hand
	}
	if qty <= 1 {
		res, err := m.db.ExecContext(ctx, `
			UPDATE entity SET ownerID = ?, locationID = ?, flag = 4 WHERE itemID = ? AND ownerID = ?`,
			charID, stationID, itemID, corpID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, nil
		}
		return itemID, nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := evedb.ReduceEntityQuantity(ctx, tx, itemID, 1); err != nil {
		return 0, err
	}
	newID, err := evedb.CreateItem(ctx, tx, typeID, charID, stationID, 1)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newID, nil
}

// mailSRPGrant tells the member where its hull is waiting.
func (m *Mod) mailSRPGrant(ctx context.Context, c *corpCEO, charID, typeID, stationID int64) {
	subject := mailSubject("Your " + m.typeNameOr(ctx, typeID) + " has been replaced")
	body := fmt.Sprintf("The corporation has replaced the %s you lost flying for it.<br><br>"+
		"It is waiting in your hangar at <b>%s</b>, from the corporation's own production.",
		m.typeNameOr(ctx, typeID), m.stationLabel(ctx, stationID))
	from, _ := m.letterFrom(ctx, c, mailFromSecurity)
	m.mailPilot(ctx, charID, from, subject, body)
}

// srpDemand is what the corporation owes and does not hold, as internal
// demand for the work board.
func (m *Mod) srpDemand(ctx context.Context, corpID int64) []buildPlanRow {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.shipTypeID, bt.blueprintTypeID, COUNT(*)
		  FROM rolebot_corp_srp s
		  JOIN invBlueprintTypes bt ON bt.productTypeID = s.shipTypeID
		 WHERE s.corporationID = ? AND s.status = 'owed'
		 GROUP BY s.shipTypeID, bt.blueprintTypeID`, corpID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []buildPlanRow
	for rows.Next() {
		var r buildPlanRow
		if rows.Scan(&r.ProductTypeID, &r.BPTypeID, &r.Wanted) == nil && r.Wanted > 0 {
			r.Priority, r.Purpose = srpPriority, "srp"
			out = append(out, r)
		}
	}
	return out
}
