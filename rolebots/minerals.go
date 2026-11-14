package rolebots

// The mineral shortage ledger, and the prospector that reads it.
//
// A row is one mineral at one station, so a trader reading it knows which way to
// fly. No NPC corporation lists a mineral or ore on this server, so the supply
// chain is three jobs:
//
//	prospector  digs the ore that yields what factories lack, refines it where it
//	            stands and lists the minerals there; it does not haul
//	trader      moves minerals to the industry hubs (tradeorders.go)
//	industrial  buys minerals off the shelf (industryRefine's shopping branch)

import (
	"context"

	evedb "gurimoon/db"
)

const (
	// mineralDemandTTLSec ages a shortage out: a factory that stopped asking got its
	// minerals or gave up on the project.
	mineralDemandTTLSec = 3 * 86400
	// mineralWantMinUnits ignores trivial rows. A bill short two units of
	// Tritanium is not a market signal, it is rounding.
	mineralWantMinUnits = 100
)

// mineralWantMin is the least of one mineral a shortfall must be for the ledger
// to record it: mineralWantMinUnits, or the mineral's own lot floor when that is
// smaller, so 40 Megacyte (a battleship's whole bill) still counts.
func mineralWantMin(typeID int64) int64 {
	if f, ok := mineralListFloors[typeID]; ok && f < mineralWantMinUnits {
		return f
	}
	return mineralWantMinUnits
}

// noteMaterialShortfall is a factory saying what its whole bill still lacks.
// Minerals go to rolebot_mineral_demand, whose readers (prospectTarget,
// mineralAsk, refinelist) assume every row is a mineral. Other lines go to
// rolebot_market_demand, only those nothing in reach sells, since it is cumulative (procure.go).
func (m *Mod) noteMaterialShortfall(ctx context.Context, b *botRun, stationID int64,
	short []matNeed, reach map[int64]int) {

	var minerals []matNeed
	var others []matNeed
	for _, s := range short {
		if s.Needed <= 0 {
			continue
		}
		if m.isMineral(ctx, s.TypeID) {
			minerals = append(minerals, s)
			continue
		}
		others = append(others, s)
	}
	m.noteMineralShortfall(ctx, b, stationID, minerals)
	if len(others) == 0 {
		return
	}
	ids := make([]int64, 0, len(others))
	for _, s := range others {
		ids = append(ids, s.TypeID)
	}
	listed, err := m.askedForInReach(ctx, ids, reach)
	if err != nil {
		return // best-effort: a bot mid-project never fails over bookkeeping
	}
	var unmet []ShopItem
	for _, s := range others {
		if listed[s.TypeID] {
			continue
		}
		unmet = append(unmet, ShopItem{
			TypeID: s.TypeID, Name: m.typeNameOr(ctx, s.TypeID), Qty: s.Needed,
		})
	}
	if len(unmet) > 0 {
		m.noteMarketDemand(ctx, b, unmet)
	}
}

// noteMineralShortfall records what a project could not cover, per station, from
// industryRefine's exact outstanding bill. It stores the last known shortfall and
// does not accumulate, so one pilot asking repeatedly cannot pin a mineral at the
// top of every prospector's list. Best effort.
func (m *Mod) noteMineralShortfall(ctx context.Context, b *botRun, stationID int64, short []matNeed) {
	if stationID == 0 || len(short) == 0 {
		return
	}
	loc, err := evedb.GetStationLocation(ctx, m.db, stationID)
	systemID, regionID := int64(0), int64(0)
	if err == nil {
		systemID, regionID = loc.SolarSystemID, loc.RegionID
	}
	now := clock.Unix()
	for _, s := range short {
		if s.Needed < mineralWantMin(s.TypeID) || !m.isMineral(ctx, s.TypeID) {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_mineral_demand
				(typeID, stationID, systemID, regionID, typeName, wanted, lastCharID, lastAt)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				systemID   = VALUES(systemID),
				regionID   = VALUES(regionID),
				typeName   = VALUES(typeName),
				wanted     = VALUES(wanted),
				lastCharID = VALUES(lastCharID),
				lastAt     = VALUES(lastAt)`,
			s.TypeID, stationID, systemID, regionID, m.typeNameOr(ctx, s.TypeID),
			s.Needed, b.CharID, now); err != nil {
			m.logBot(b.CharID, logError, "industry", "mineral demand ledger: %v", err)
			return
		}
	}
	// Cheap on the lastAt index; keeps the table from holding every project that
	// ever stalled.
	_, _ = m.db.ExecContext(ctx,
		`DELETE FROM rolebot_mineral_demand WHERE lastAt < ?`, now-mineralDemandTTLSec)
}

// isMineral keeps ore, ice products and anything else off the ledger: this
// is the refined-mineral market specifically, which is what a prospector can
// actually produce out of a belt.
func (m *Mod) isMineral(ctx context.Context, typeID int64) bool {
	return m.typeGroupOf(ctx, typeID) == groupMineral
}

// mineralShortageUnits is the whole roster's outstanding want of one mineral, for
// pricing a listing (mineralAsk). It is summed across stations because scarcity
// belongs to the mineral, not to the counter it is sold over.
func (m *Mod) mineralShortageUnits(ctx context.Context, typeID int64) int64 {
	var n int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(wanted), 0) FROM rolebot_mineral_demand
		 WHERE typeID = ? AND lastAt >= ?`,
		typeID, clock.Unix()-mineralDemandTTLSec).Scan(&n)
	return n
}
