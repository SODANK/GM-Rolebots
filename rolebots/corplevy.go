package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// The harvest levy: a managed corporation's cut of what its bot members dig out of the ground and sell. Bounty
// tax was the only income a corporation earned without spending first, so a mining or research outfit's tax
// line stayed at zero. The corporation takes its tax rate off a bot member's ore sales, as the core takes it
// off a bounty: the rate the CEO already steers (corpceo_treasury.go), capped at harvestLevyMaxRate because a
// greedy charter's 20% is a bounty number.
// Only what the member harvested (the asteroid category) and only sales to someone outside the corporation
// count: ore sold into its own buyback is already its business, and a trader's flips are gross revenue.
// Humans are never levied: the core does not tax a player's market sales and the module does not reach into a
// player's wallet. It is a journal sweep, not a hook on every sell path: market_transactions records every
// fill, and one server-setting cursor over transactionID is read in batches.

const (
	settingLevyCursor = "rolebots.levy.transactionCursor"

	// harvestLevyMaxRate caps the corporation's tax rate as a levy on ore.
	harvestLevyMaxRate = 0.10
	// harvestLevyMinISK: a pass's levy on one member below this is dropped.
	harvestLevyMinISK = 100.0

	levySweepTicks = 60
	levyBatch      = 5000
	levyMaxBatches = 20 // per pass; the rest waits for the next one

	// mtSell is market_transactions.transactionType for the seller's row
	// (db/marketorders.go recordTransaction: 1 = buy, 0 = sell).
	mtSell = 0
	// catAsteroid is invCategories' Asteroid: ore and ice, as mined.
	catAsteroid = 25
)

// harvestLevyRate is the levy a corporation with this tax rate takes. Pure.
func harvestLevyRate(taxRate float64) float64 {
	if taxRate <= 0 || taxRate >= 1 {
		return 0
	}
	return math.Min(taxRate, harvestLevyMaxRate)
}

// levySale is one sale the levy may apply to.
type levySale struct {
	CharID, CorpID, ClientCorpID, CategoryID int64
	TxType                                   int64
	ISK                                      float64
}

// levyable reports whether a sale is the corporation's to levy, given that
// the seller is a bot in a managed corporation. Pure.
func levyable(s levySale) bool {
	return s.TxType == mtSell && s.CategoryID == catAsteroid && s.ISK > 0 &&
		s.CorpID != 0 && s.ClientCorpID != s.CorpID
}

var levySweepBusy atomic.Bool

// startLevySweep registers the pass that reads new market fills.
func (m *Mod) startLevySweep() {
	m.registerPeriodic("rolebot-corp-levy", levySweepTicks, func(uint64) bool {
		if !Master() || !levySweepBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("corp levy sweep", func() {
			defer levySweepBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := m.levySweep(ctx); err != nil {
				rlogf("rolebots: corp levy sweep: %v", err)
			}
			// The trade levy (corptradelevy.go) rides the same pass.
			if err := m.tradeLevySweep(ctx); err != nil {
				rlogf("rolebots: corp trade levy sweep: %v", err)
			}
		})
		return true
	})
}

// levySweep reads the fills past its cursor and levies what is due.
func (m *Mod) levySweep(ctx context.Context) error {
	humanLevy := m.humanLevyCorps(ctx)
	if len(managedCorpIDs()) == 0 && len(humanLevy) == 0 {
		return nil
	}
	var cursor int64
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingLevyCursor); err != nil {
		return err
	} else if ok {
		cursor, _ = strconv.ParseInt(v, 10, 64)
	} else {
		// First run: start at the end. Sales made before the levy existed
		// were the members' own, and are not taxed after the fact.
		_ = m.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(transactionID), 0) FROM market_transactions`).Scan(&cursor)
		return evedb.SetServerSetting(ctx, m.db, settingLevyCursor, strconv.FormatInt(cursor, 10))
	}

	type key struct{ charID, corpID int64 }
	due := map[key]float64{}
	for range levyMaxBatches {
		sales, last, err := m.levyBatch(ctx, cursor)
		if err != nil {
			return err
		}
		for _, s := range sales {
			if _, ok := managedCEO(s.CorpID); (!ok && !humanLevy[s.CorpID]) || !levyable(s) || !m.isSynthetic(s.CharID) {
				continue
			}
			due[key{s.CharID, s.CorpID}] += s.ISK
		}
		if last == cursor {
			break
		}
		cursor = last
		if len(sales) < levyBatch {
			break
		}
	}
	// The cursor moves before the money does: a crash between the two loses
	// one pass of levy, where the other order would charge it twice.
	if err := evedb.SetServerSetting(ctx, m.db, settingLevyCursor, strconv.FormatInt(cursor, 10)); err != nil {
		return err
	}
	rates := map[int64]float64{}
	for k, gross := range due {
		rate, ok := rates[k.corpID]
		if !ok {
			rate = harvestLevyRate(evedb.GetCorpTaxRate(ctx, m.db, k.corpID))
			rates[k.corpID] = rate
		}
		m.chargeLevy(ctx, k.charID, k.corpID, gross, rate)
	}
	return nil
}

// levyBatch reads one batch of fills past the cursor. last is the highest
// transactionID read, whatever it was.
func (m *Mod) levyBatch(ctx context.Context, cursor int64) ([]levySale, int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.transactionID, t.transactionType, t.characterID, t.price * t.quantity,
		       COALESCE(ch.corporationID, 0), COALESCE(cl.corporationID, 0), COALESCE(g.categoryID, 0)
		  FROM market_transactions t
		  LEFT JOIN character_ ch ON ch.characterID = t.characterID
		  LEFT JOIN character_ cl ON cl.characterID = t.clientID
		  LEFT JOIN invTypes ty ON ty.typeID = t.typeID
		  LEFT JOIN invGroups g ON g.groupID = ty.groupID
		 WHERE t.transactionID > ?
		 ORDER BY t.transactionID LIMIT ?`, cursor, levyBatch)
	if err != nil {
		return nil, cursor, fmt.Errorf("levy batch: %w", err)
	}
	defer rows.Close()
	last := cursor
	var out []levySale
	for rows.Next() {
		var id int64
		var s levySale
		if err := rows.Scan(&id, &s.TxType, &s.CharID, &s.ISK, &s.CorpID, &s.ClientCorpID, &s.CategoryID); err != nil {
			return nil, cursor, fmt.Errorf("levy batch scan: %w", err)
		}
		last = id
		out = append(out, s)
	}
	return out, last, rows.Err()
}

// chargeLevy moves one member's levy for a pass into its corporation.
func (m *Mod) chargeLevy(ctx context.Context, charID, corpID int64, gross, rate float64) {
	levy := math.Floor(gross * rate)
	if levy < harvestLevyMinISK {
		return
	}
	// What the member has spent since the sale is spent: the levy takes what
	// is left, and never more than it holds.
	if bal, err := evedb.GetCharBalance(ctx, m.db, charID); err != nil {
		return
	} else if levy > math.Floor(bal) {
		levy = math.Floor(bal)
	}
	if levy < harvestLevyMinISK {
		return
	}
	if _, _, err := evedb.TransferISK(ctx, m.db, charID, corpID, levy, refPlayerDonation,
		"Corporation harvest levy"); err != nil {
		if !errors.Is(err, evedb.ErrInsufficientFunds) {
			rlogf("rolebots: corp levy char=%d corp=%d: %v", charID, corpID, err)
		}
		return
	}
	m.corpBook(ctx, corpID, lineLevy, levy, charID,
		fmt.Sprintf("%.0f%% of %s of ore sold", rate*100, iskString(gross)))
}
