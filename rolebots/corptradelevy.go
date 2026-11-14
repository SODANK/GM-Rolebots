package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	evedb "gurimoon/db"
)

// The trade levy is a managed corporation's cut of what its bot members make trading.
// It is a share of realised profit read from the trade book (tradebook.go): a 'sell' row
// carries the run's margin and a 'fill' row the net profit of a listing someone took.
// 'haul' rows are the same sale booked twice and are not levied. Losses are not netted.
// Ore is not levied: the harvest levy (corplevy.go) takes it. Humans and the Trade desk's
// own runs are never levied. The sweep moves its cursor over rolebot_trades.tradeID before
// the money moves, and adds each pass to rolebot_levy_due; a member is charged at
// tradeLevyChargeISK or after a day (docs/CORPS.md, "The trade levy").

const (
	settingTradeLevyCursor = "rolebots.levy.tradeCursor"

	// tradeLevyMaxRate caps the corporation's tax rate as a levy on profit.
	// It is separate from harvestLevyMaxRate so the two can differ.
	tradeLevyMaxRate = 0.10
	// tradeLevyMinISK is the smallest levy one charge takes from one member.
	tradeLevyMinISK = 100.0
	// tradeLevyChargeISK and tradeLevyChargeSec: a member's levy is charged once
	// it reaches this much, or once its oldest unpaid profit is this old.
	tradeLevyChargeISK = 100_000.0
	tradeLevyChargeSec = 24 * 3600

	lineTradeLevy = "tradelevy"
)

// tradeLevyRate returns the levy rate a corporation with this tax rate takes. Pure.
func tradeLevyRate(taxRate float64) float64 {
	if taxRate <= 0 || taxRate >= 1 {
		return 0
	}
	return math.Min(taxRate, tradeLevyMaxRate)
}

// tradeLevyable reports whether one trade-book row carries profit the corporation may
// levy: a sale or a fill at a profit, of anything but ore. Pure.
func tradeLevyable(kind string, profit float64, categoryID int64) bool {
	return profit > 0 && (kind == tradeKindSell || kind == tradeKindFill) && categoryID != catAsteroid
}

// tradeLevyDue reports whether a member's accumulated levy is charged now. Pure.
func tradeLevyDue(levy float64, sinceSec int64) bool {
	return levy >= tradeLevyChargeISK || sinceSec >= tradeLevyChargeSec
}

// tradeLevyRow is one rolebot_trades row the sweep reads.
type tradeLevyRow struct {
	CharID, CorpID int64
	Kind           string
	Profit         float64
	CategoryID     int64
}

// tradeLevySweep reads the trade book past its cursor and levies what is due.
// It is called from the harvest levy pass (corplevy.go).
func (m *Mod) tradeLevySweep(ctx context.Context) error {
	if len(managedCorpIDs()) == 0 {
		return nil
	}
	var cursor int64
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingTradeLevyCursor); err != nil {
		return err
	} else if ok {
		cursor, _ = strconv.ParseInt(v, 10, 64)
	} else {
		// First run: start at the end, so profit made before the levy existed is not taxed.
		_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(tradeID), 0) FROM rolebot_trades`).Scan(&cursor)
		return evedb.SetServerSetting(ctx, m.db, settingTradeLevyCursor, strconv.FormatInt(cursor, 10))
	}

	type key struct{ charID, corpID int64 }
	due := map[key]float64{}
	for range levyMaxBatches {
		rows, last, err := m.tradeLevyBatch(ctx, cursor)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if _, ok := managedCEO(r.CorpID); !ok || !tradeLevyable(r.Kind, r.Profit, r.CategoryID) || !m.isSynthetic(r.CharID) {
				continue
			}
			due[key{r.CharID, r.CorpID}] += r.Profit
		}
		if last == cursor {
			break
		}
		cursor = last
		if len(rows) < levyBatch {
			break
		}
	}
	// The cursor moves before the money does. A crash between the two loses one pass of
	// levy; the other order would charge it twice.
	if err := evedb.SetServerSetting(ctx, m.db, settingTradeLevyCursor, strconv.FormatInt(cursor, 10)); err != nil {
		return err
	}
	now := clock.Unix()
	for k, profit := range due {
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_levy_due (characterID, corporationID, profit, since)
			VALUES (?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE profit = profit + VALUES(profit)`,
			k.charID, k.corpID, profit, now); err != nil {
			return fmt.Errorf("trade levy due: %w", err)
		}
	}
	return m.chargeDueTradeLevies(ctx, now)
}

// chargeDueTradeLevies charges every member whose accumulated levy is due. A row
// is deleted before its money moves, as the cursor is: a crash loses a charge
// rather than taking it twice. A member who changed corporation is charged by
// the corporation it earned the profit in only while that one is still managed.
func (m *Mod) chargeDueTradeLevies(ctx context.Context, now int64) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT d.characterID, d.corporationID, d.profit, d.since, COALESCE(co.taxRate, 0)
		  FROM rolebot_levy_due d
		  LEFT JOIN corporation co ON co.corporationID = d.corporationID`)
	if err != nil {
		return fmt.Errorf("trade levy due read: %w", err)
	}
	type dueRow struct {
		charID, corpID int64
		profit, rate   float64
	}
	var charge []dueRow
	for rows.Next() {
		var r dueRow
		var since int64
		var tax float64
		if err := rows.Scan(&r.charID, &r.corpID, &r.profit, &since, &tax); err != nil {
			rows.Close()
			return fmt.Errorf("trade levy due scan: %w", err)
		}
		r.rate = tradeLevyRate(tax)
		if tradeLevyDue(r.profit*r.rate, now-since) {
			charge = append(charge, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range charge {
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM rolebot_levy_due WHERE characterID = ? AND corporationID = ?`, r.charID, r.corpID); err != nil {
			return fmt.Errorf("trade levy due clear: %w", err)
		}
		if _, ok := managedCEO(r.corpID); !ok {
			continue
		}
		m.chargeTradeLevy(ctx, r.charID, r.corpID, r.profit, r.rate)
	}
	return nil
}

// tradeLevyBatch reads one batch of trade-book rows past the cursor. last is the highest
// tradeID read. The corporation is the member's current one, as in the harvest levy.
func (m *Mod) tradeLevyBatch(ctx context.Context, cursor int64) ([]tradeLevyRow, int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.tradeID, t.characterID, t.kind, t.profit, COALESCE(ch.corporationID, 0),
		       COALESCE(g.categoryID, 0)
		  FROM rolebot_trades t
		  LEFT JOIN character_ ch ON ch.characterID = t.characterID
		  LEFT JOIN invTypes it ON it.typeID = t.typeID
		  LEFT JOIN invGroups g ON g.groupID = it.groupID
		 WHERE t.tradeID > ?
		 ORDER BY t.tradeID LIMIT ?`, cursor, levyBatch)
	if err != nil {
		return nil, cursor, fmt.Errorf("trade levy batch: %w", err)
	}
	defer rows.Close()
	last := cursor
	var out []tradeLevyRow
	for rows.Next() {
		var id int64
		var r tradeLevyRow
		if err := rows.Scan(&id, &r.CharID, &r.Kind, &r.Profit, &r.CorpID, &r.CategoryID); err != nil {
			return nil, cursor, fmt.Errorf("trade levy batch scan: %w", err)
		}
		last = id
		out = append(out, r)
	}
	return out, last, rows.Err()
}

// chargeTradeLevy moves one member's accumulated levy into its corporation.
func (m *Mod) chargeTradeLevy(ctx context.Context, charID, corpID int64, profit, rate float64) {
	levy := math.Floor(profit * rate)
	if levy < tradeLevyMinISK {
		return
	}
	// The levy never exceeds what the member holds.
	if bal, err := evedb.GetCharBalance(ctx, m.db, charID); err != nil {
		return
	} else if levy > math.Floor(bal) {
		levy = math.Floor(bal)
	}
	if levy < tradeLevyMinISK {
		return
	}
	if _, _, err := evedb.TransferISK(ctx, m.db, charID, corpID, levy, refPlayerDonation,
		"Corporation trade levy"); err != nil {
		if !errors.Is(err, evedb.ErrInsufficientFunds) {
			rlogf("rolebots: corp trade levy char=%d corp=%d: %v", charID, corpID, err)
		}
		return
	}
	m.corpBook(ctx, corpID, lineTradeLevy, levy, charID,
		fmt.Sprintf("%.0f%% of %s of trading profit", rate*100, iskString(profit)))
}
