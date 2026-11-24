package rolebots

import (
	"context"
	"sync"

	"gurimoon/corp"
	evedb "gurimoon/db"
)

// Who places a corporation's market orders. A corporation order counts against the order slots of the character
// that placed it (the core's countOpenOrders keys on charID), and a CEO with Trade untrained has five, two of
// which the business pass never takes, so listings, bids, output and the company store compete for one slot.
// An order goes out under the first of these with room: the CEO, then the Industry Director, then its deputy.
// Bots only (a player's slots are its own business), and only one holding a trading role, because the core's
// ResolveMarketAccount asks for Trader or Director. The placing pilot is recorded (rolebot_corp_orders.placedBy)
// so a reprice is made as the pilot who owns the order, and a replaced director's orders are cancelled back
// into the office (ceoIndustryStaff). When nobody has room the shortfall is recorded as order pressure, and the
// skill planner trains the CEO's Trade and Retail against it (corpceo_skills.go).

var (
	orderPressureMu sync.Mutex
	orderPressure   = map[int64]int{}
)

// noteCorpOrderPressure records how many corporation orders went unplaced
// for want of a slot this think (0 clears it).
func noteCorpOrderPressure(corpID int64, n int) {
	orderPressureMu.Lock()
	defer orderPressureMu.Unlock()
	if n <= 0 {
		delete(orderPressure, corpID)
		return
	}
	orderPressure[corpID] = n
}

// addCorpOrderPressure adds to this think's count.
func addCorpOrderPressure(corpID int64, n int) {
	orderPressureMu.Lock()
	defer orderPressureMu.Unlock()
	orderPressure[corpID] += n
}

// corpOrderPressure is the last recorded count.
func corpOrderPressure(corpID int64) int {
	orderPressureMu.Lock()
	defer orderPressureMu.Unlock()
	return orderPressure[corpID]
}

// corpOrderPlacer is who places the corporation's next order, 0 for nobody. The Director of Commerce comes first
// when there is one (corpstaff_dept.go): trading is its department, and it holds the Director role, which the
// core's market account accepts in place of Trader.
func (m *Mod) corpOrderPlacer(ctx context.Context, corpID, ceoID int64) int64 {
	if _, human := humanChaired(corpID); human {
		return m.chairOrderPlacer(ctx, corpID, ceoID)
	}
	if head := m.commerceHead(ctx, corpID); head != 0 && head != ceoID &&
		m.freeOrderSlots(ctx, head) > corpOrderSlotKeep {
		if info, err := evedb.GetCorporationMemberInfo(ctx, m.db, head); err == nil && info != nil &&
			info.Corprole&(corp.RoleTrader|corp.RoleDirector) != 0 {
			return head
		}
	}
	if m.freeOrderSlots(ctx, ceoID) > corpOrderSlotKeep {
		return ceoID
	}
	p, err := m.loadCorpPolicy(ctx, corpID)
	if err != nil {
		return 0
	}
	for _, id := range []int64{p.DirectorID, p.DeputyID} {
		if id == 0 || id == ceoID || !m.isRostered(ctx, id) {
			continue
		}
		info, err := evedb.GetCorporationMemberInfo(ctx, m.db, id)
		if err != nil || info == nil || info.Corprole&(corp.RoleTrader|corp.RoleDirector) == 0 {
			continue
		}
		if m.freeOrderSlots(ctx, id) > corpOrderSlotKeep {
			return id
		}
	}
	return 0
}

// isRostered reports whether a character is one of the module's pilots.
// It is isSynthetic (mailsuppress.go), the one cached bot test.
func (m *Mod) isRostered(_ context.Context, charID int64) bool {
	return m.isSynthetic(charID)
}

// corpOrderOwner is who placed a tracked corporation order (the CEO for a
// row older than the placedBy column).
func (m *Mod) corpOrderOwner(ctx context.Context, orderID, fallback int64) int64 {
	var by int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT placedBy FROM rolebot_corp_orders WHERE orderID = ?`, orderID).Scan(&by)
	if by == 0 {
		return fallback
	}
	return by
}

func init() { registerCorpForget("orderPressure", func(id int64) { noteCorpOrderPressure(id, 0) }) }
