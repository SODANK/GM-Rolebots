package rolebots

import (
	"context"
	"fmt"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// War-chest sign-outs.
//
// A capsuleer in the department that owns a war-chest division (Security the Armory, Mining the
// Fleet Yard, Sales the Spoils: humanGuardedTake, corpdivisions.go) takes from it directly. The
// war chest is there to be used, so nothing is locked; what a capsuleer takes is signed out to it
// instead, by type and quantity, so a stack merged or a module repackaged still counts. Only
// ships and modules worth signoutMinValue or more are signed out: ammunition, drones and the
// like are spent in use.
//
// The sign-out closes when the pilot puts the type back in any of the corporation's divisions
// (returned) or loses it on a killmail (lost). What is neither held by the pilot, returned nor
// lost (sold, contracted, traded away, reprocessed) earns a letter, and once the Armory's theft
// grace has passed it is stolen; leaving the corporation with it is stolen at once. Stolen goods
// get the Armory's own answer (punishTheft): barred, hostile, a bounty, and the take on the war
// chest gone.

const (
	signoutMinValue = 250_000
	signoutEverySec = 3600

	signoutOpen     = "open"
	signoutReturned = "returned"
	signoutLost     = "lost"
	signoutStolen   = "stolen"
)

// signoutAt is when each corporation's sign-outs were last worked.
var signoutAt sync.Map

// onOfficeTake is esp.OfficeTakeHook: runs off the RPC goroutine.
func (m *Mod) onOfficeTake(corpID, charID, stationID, typeID, qty, flag int64) {
	if qty <= 0 || charID == 0 || m.isSynthetic(charID) {
		return
	}
	if _, ok := managedCEO(corpID); !ok {
		return
	}
	goSafe("office take", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		div := flagDiv(flag)
		if div == 0 || !isGuardedDiv(m.corpDivs(ctx, corpID), div) {
			return
		}
		if !signoutCategory(m.typeCategoryOf(ctx, typeID)) {
			return
		}
		unit := evedb.TypeValueOf(ctx, m.db, typeID).Value
		if unit*float64(qty) < signoutMinValue {
			return
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_signout
			  (corporationID, characterID, stationID, division, typeID, taken, qty, unitValue, takenAt, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			corpID, charID, stationID, div, typeID, qty, qty, unit, clock.Unix(), signoutOpen); err != nil {
			rlogf("rolebots: sign-out corp=%d char=%d type=%d: %v", corpID, charID, typeID, err)
		}
	})
}

// isGuardedDiv reports whether a division holds a war-chest purpose. Pure.
func isGuardedDiv(p *divProfile, div int) bool {
	for _, d := range p.guardedDivs() {
		if d == div {
			return true
		}
	}
	return false
}

// signoutCategory reports whether a type's category is signed out: ships (6) and modules (7).
func signoutCategory(cat int64) bool { return cat == 6 || cat == 7 }

// signoutReturned signs back in what a pilot put into one of its corporation's divisions,
// oldest sign-out first: the item and, for a ship, what is fitted to it or in its holds.
func (m *Mod) signoutReturned(ctx context.Context, corpID, charID, itemID int64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, SUM(GREATEST(quantity, 1)) FROM entity
		 WHERE itemID = ? OR locationID = ?
		 GROUP BY typeID`, itemID, itemID)
	if err != nil {
		return
	}
	got := map[int64]int64{}
	for rows.Next() {
		var t, n int64
		if rows.Scan(&t, &n) == nil && n > 0 {
			got[t] = n
		}
	}
	rows.Close()
	for t, n := range got {
		m.closeSignouts(ctx, corpID, charID, t, n, signoutReturned)
	}
}

// closeSignouts takes n off a pilot's open sign-outs of a type, oldest first, closing each one
// emptied with status. Answers how many it took off.
func (m *Mod) closeSignouts(ctx context.Context, corpID, charID, typeID, n int64, status string) int64 {
	rows, err := m.db.QueryContext(ctx, `
		SELECT signoutID, qty FROM rolebot_corp_signout
		 WHERE corporationID = ? AND characterID = ? AND typeID = ? AND status = ?
		 ORDER BY takenAt, signoutID`, corpID, charID, typeID, signoutOpen)
	if err != nil {
		return 0
	}
	type open struct{ id, qty int64 }
	var list []open
	for rows.Next() {
		var o open
		if rows.Scan(&o.id, &o.qty) == nil {
			list = append(list, o)
		}
	}
	rows.Close()
	done := int64(0)
	for _, o := range list {
		if n <= 0 {
			break
		}
		take := min(n, o.qty)
		n -= take
		done += take
		if take == o.qty {
			_, _ = m.db.ExecContext(ctx,
				`UPDATE rolebot_corp_signout SET qty = 0, status = ?, closedAt = ? WHERE signoutID = ?`,
				status, clock.Unix(), o.id)
		} else {
			_, _ = m.db.ExecContext(ctx,
				`UPDATE rolebot_corp_signout SET qty = qty - ? WHERE signoutID = ?`, take, o.id)
		}
	}
	return done
}

// signoutGroup is one pilot's open sign-outs of one type.
type signoutGroup struct {
	CharID, TypeID int64
	Owed           int64
	Unit           float64
	FirstAt        int64
	WarnedAt       int64
	LastKillID     int64
}

// signoutVerdict is what the watch does with a group. Pure.
func signoutVerdict(missing int64, left bool, warnedAt, now, graceSec int64) string {
	switch {
	case missing <= 0:
		return watchHold
	case left:
		return watchStolen
	case warnedAt == 0:
		return watchWarn
	case now-warnedAt > graceSec:
		return watchStolen
	}
	return watchHold
}

// signoutChore works a corporation's open sign-outs, hourly.
func (m *Mod) signoutChore(ctx context.Context, c *corpCEO) error {
	if v, ok := signoutAt.Load(c.CorpID); ok && clock.Unix()-v.(int64) < signoutEverySec {
		return nil
	}
	signoutAt.Store(c.CorpID, clock.Unix())
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, typeID, SUM(qty), MAX(unitValue), MIN(takenAt), MAX(warnedAt), MAX(lastKillID)
		  FROM rolebot_corp_signout
		 WHERE corporationID = ? AND status = ?
		 GROUP BY characterID, typeID`, c.CorpID, signoutOpen)
	if err != nil {
		return err
	}
	var groups []signoutGroup
	for rows.Next() {
		var g signoutGroup
		if rows.Scan(&g.CharID, &g.TypeID, &g.Owed, &g.Unit, &g.FirstAt, &g.WarnedAt, &g.LastKillID) == nil {
			groups = append(groups, g)
		}
	}
	rows.Close()
	now := clock.Unix()
	grace := armoryTheftGraceHV.Load() * 3600
	for _, g := range groups {
		m.workSignoutGroup(ctx, c, g, now, grace)
	}
	return nil
}

func (m *Mod) workSignoutGroup(ctx context.Context, c *corpCEO, g signoutGroup, now, grace int64) {
	// Losses first: every killmail of the pilot since the first sign-out, not yet counted, that
	// carried the type (as its hull or among its items).
	var lost, maxKill int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM((k.shipTypeID = ?) +
		       COALESCE((SELECT SUM(i.quantity) FROM killmail_items i WHERE i.killID = k.killID AND i.typeID = ?), 0)), 0),
		       COALESCE(MAX(k.killID), 0)
		  FROM killmails k
		 WHERE k.victimCharID = ? AND k.killTime >= ? AND k.killID > ?`,
		g.TypeID, g.TypeID, g.CharID, g.FirstAt, g.LastKillID).Scan(&lost, &maxKill)
	if maxKill > 0 {
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_signout SET lastKillID = ?
			 WHERE corporationID = ? AND characterID = ? AND typeID = ? AND status = ?`,
			maxKill, c.CorpID, g.CharID, g.TypeID, signoutOpen)
	}
	if lost > 0 {
		g.Owed -= m.closeSignouts(ctx, c.CorpID, g.CharID, g.TypeID, lost, signoutLost)
	}
	if g.Owed <= 0 {
		return
	}
	var held int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(GREATEST(quantity, 1)), 0) FROM entity WHERE ownerID = ? AND typeID = ?`,
		g.CharID, g.TypeID).Scan(&held)
	left := m.charCorpID(ctx, g.CharID) != c.CorpID
	missing := g.Owed - held
	if left {
		missing = g.Owed
	}
	typeName := m.typeNameOr(ctx, g.TypeID)
	switch signoutVerdict(missing, left, g.WarnedAt, now, grace) {
	case watchHold:
		if missing <= 0 && g.WarnedAt != 0 {
			_, _ = m.db.ExecContext(ctx, `
				UPDATE rolebot_corp_signout SET warnedAt = 0
				 WHERE corporationID = ? AND characterID = ? AND typeID = ? AND status = ?`,
				c.CorpID, g.CharID, g.TypeID, signoutOpen)
		}
	case watchWarn:
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_signout SET warnedAt = ?
			 WHERE corporationID = ? AND characterID = ? AND typeID = ? AND status = ?`,
			now, c.CorpID, g.CharID, g.TypeID, signoutOpen)
		from, _ := m.letterFrom(ctx, c, mailFromSecurity)
		m.mailPilot(ctx, g.CharID, from, "Quartermaster: signed-out gear missing",
			fmt.Sprintf("You signed out %d %s from the corporation's war chest and no longer hold it.<br><br>"+
				"Put it back in any of the corporation's hangars, or fly it, within %d hours. "+
				"After that it is counted stolen.", missing, typeName, grace/3600))
	case watchStolen:
		n := m.closeSignouts(ctx, c.CorpID, g.CharID, g.TypeID, missing, signoutStolen)
		if n <= 0 {
			return
		}
		value := g.Unit * float64(n)
		what := fmt.Sprintf("%d %s", n, typeName)
		letter := fmt.Sprintf("The %s you signed out of the corporation's war chest is gone: "+
			"not held, not returned and not lost in battle. The corporation counts it stolen.", what)
		if left {
			letter = fmt.Sprintf("You left the corporation holding the %s you signed out of its war chest. "+
				"The corporation counts it stolen.", what)
		}
		m.punishTheft(ctx, c, theftCase{
			CharID: g.CharID, Value: value,
			Deed:    "stole " + what + " from the war chest",
			Subject: "Quartermaster: theft",
			Letter:  letter,
			Log:     fmt.Sprintf("counts %s signed out to %s stolen", what, m.charNameOr(ctx, g.CharID)),
		})
	}
}
