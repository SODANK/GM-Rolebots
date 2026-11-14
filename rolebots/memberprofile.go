package rolebots

import (
	"context"
	"math"
	"sync"

	evedb "gurimoon/db"
)

// The revealed profile: what leadership knows about a human member.
//
// A human has no traits, so the neutral fallback (defaultTuning) would rank every
// player level with an even-tempered bot. Appetites are read off the last thirty
// days of record instead: industry jobs, ore sold, kills and market business.
// Each can only lift the neutral value, up to revealedLiftMax, because a missing
// record is not a bad candidate. Leadership reads this for scoring only (trust,
// factory, seat); it never drives a human's behaviour.

const (
	revealedWindowSec = 30 * 86400
	revealedLiftMax   = 0.6
	// The amount of each kind of work that counts as a full-time record.
	revealedFullJobs   = 20.0
	revealedFullOreM3  = 150000.0
	revealedFullKills  = 30.0
	revealedFullTrades = 60.0
	revealedFullHauls  = 20.0
	revealedCacheSec   = 1800
)

// memberEvidence is thirty days of one pilot's work.
type memberEvidence struct {
	Jobs   int     // industry jobs installed
	OreM3  float64 // ore sold on the market, m³
	Kills  int     // killmails it appears on as an attacker
	Trades int     // market transactions that were not ore
	Hauls  int     // courier contracts it delivered
}

// revealedLift maps an amount of work onto a multiplier in [1, 1+max].
func revealedLift(x, full float64) float64 {
	if x <= 0 || full <= 0 {
		return 1
	}
	return 1 + revealedLiftMax*math.Min(1, x/full)
}

// revealedTuningFrom is the pure half: evidence to appetites.
func revealedTuningFrom(ev memberEvidence) Tuning {
	tn := defaultTuning()
	ind := revealedLift(float64(ev.Jobs), revealedFullJobs)
	tn.IndustryAppetite *= ind
	tn.SupplyDrive *= ind
	tn.MiningAppetite *= revealedLift(ev.OreM3, revealedFullOreM3)
	tn.ViolenceAppetite *= revealedLift(float64(ev.Kills), revealedFullKills)
	tn.TradeAppetite *= revealedLift(float64(ev.Trades), revealedFullTrades)
	tn.HaulAppetite *= revealedLift(float64(ev.Hauls), revealedFullHauls)
	return tn
}

type revealedEntry struct {
	at int64
	tn Tuning
}

var revealedCache sync.Map // charID to revealedEntry

// revealedTuning reads a pilot's evidence (cached half an hour: every CEO
// pass that ranks members asks, and the record does not move that fast).
func (m *Mod) revealedTuning(ctx context.Context, charID int64) Tuning {
	now := clock.Unix()
	if v, ok := revealedCache.Load(charID); ok {
		if e := v.(revealedEntry); now-e.at < revealedCacheSec {
			return e.tn
		}
	}
	tn := revealedTuningFrom(m.memberEvidence(ctx, charID))
	revealedCache.Store(charID, revealedEntry{at: now, tn: tn})
	return tn
}

// memberEvidence reads the record. Best effort: a failed read is no evidence.
func (m *Mod) memberEvidence(ctx context.Context, charID int64) memberEvidence {
	var ev memberEvidence
	cutUnix := clock.Unix() - revealedWindowSec
	cutWin := evedb.UnixToWin32(cutUnix)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ramJobs WHERE installerID = ? AND installTime >= ?`,
		charID, cutWin).Scan(&ev.Jobs)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN g.categoryID = 25 AND tx.transactionType = 0
		                         THEN tx.quantity * t.volume ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN g.categoryID = 25 THEN 0 ELSE 1 END), 0)
		  FROM market_transactions tx
		  JOIN invTypes t ON t.typeID = tx.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE tx.characterID = ? AND tx.transactionDateTime >= ?`,
		charID, cutWin).Scan(&ev.OreM3, &ev.Trades)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT a.killID)
		  FROM killmail_attackers a
		  JOIN killmails k ON k.killID = a.killID
		 WHERE a.charID = ? AND k.killTime >= ?`,
		charID, cutUnix).Scan(&ev.Kills)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM couriermissions
		 WHERE acceptorID = ? AND state = ? AND dateIssued >= ?`,
		charID, evedb.CourierStateDelivered, cutUnix).Scan(&ev.Hauls)
	return ev
}

func init() { registerCharForget("revealedCache", func(id int64) { revealedCache.Delete(id) }) }
