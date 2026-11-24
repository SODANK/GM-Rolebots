package rolebots

import (
	"context"
	"sync"
)

// The Sales arm brokers the library's copies. A Sales member docked at one of its corporation's offices
// lists the copies the CEO marked for sale there (corpPrintSaleChoreN), the way a builder does at its
// base: the corporation pays the fee and books the proceeds (corpprintsale.go). Traders dock at the
// trade desk's offices all day, which corpSaleStations adds to the stations a copy may be sold from.

const (
	// corpPrintChoreSalesMax is how many prints a Sales member lists in one visit.
	corpPrintChoreSalesMax = 30
	// salesPrintChoreSec is how often one Sales member looks at the counter.
	salesPrintChoreSec = 15 * 60
)

var salesPrintChoreAt sync.Map // charID -> unix of the last look

func init() {
	registerCharForget("salesPrintChoreAt", func(id int64) { salesPrintChoreAt.Delete(id) })
}

// salesPrintChoreDue reports whether a member's last look at the counter is old enough, and stamps it.
func salesPrintChoreDue(charID, now int64) bool {
	if v, ok := salesPrintChoreAt.Load(charID); ok && now-v.(int64) < salesPrintChoreSec {
		return false
	}
	salesPrintChoreAt.Store(charID, now)
	return true
}

// salesPrintChore lists the corporation's marked copies when the pilot is a Sales member docked at an
// office. Cheap for everyone else: a map read before any query.
func (m *Mod) salesPrintChore(ctx context.Context, b *botRun) {
	if b.StationID == 0 {
		return
	}
	if s, ok := armOf(b.CharID, b.CorpID); !ok || s.Arm != armSales {
		return
	}
	if !corpDeskRun(b.CorpID) || !salesPrintChoreDue(b.CharID, clock.Unix()) {
		return
	}
	officeID := m.officeAt(ctx, b.CorpID, b.StationID)
	if officeID == 0 {
		return
	}
	m.corpPrintSaleChoreN(ctx, b, b.StationID, officeID, corpPrintChoreSalesMax)
}
