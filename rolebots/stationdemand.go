package rolebots

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// Shopper demand ledger: where buyers actually come. At every shopping stop the trip books what it
// wanted at this counter and what it got, per (station, type), decayed on the liquidity half-life.
// Rows are written only where the shopper stood: a stop is a statement about a market, a home is
// not (traffic.go header). The gap, asked less filled, is the plainest "list here" a seller can read.
// Readers: industryHaulToOutlet (industry.go) carries durables to the counter with the most pull;
// ceoOutletHauls (corpstore.go) moves a corporation's durables to the office whose counter pulls
// hardest; findSpecRun (tradeorders.go) adds the biggest gaps as destinations.
// rolebots.outlet.enabled = 0 turns every reader off; the ledger still records.

const (
	// stationDemandHalfLifeSec is the liquidity horizon: roughly the last
	// week, this week counting most.
	stationDemandHalfLifeSec = 3.5 * 86400.0
	// shopperShareFrac is how much of what the incumbents already fill a new
	// seller can expect to take. The gap counts in full: it is unmet.
	shopperShareFrac = 0.25

	settingOutletEnabled = "rolebots.outlet.enabled"
	outletSettingsTTL    = 60
)

// shopLine is one line of one shopping stop: units wanted at this counter
// and units the counter gave.
type shopLine struct {
	TypeID        int64
	Asked, Filled int64
}

// noteShopStop books one shopping stop at a station. Best-effort: the
// ledger is a measurement, and a failed write costs a shopper nothing.
func (m *Mod) noteShopStop(ctx context.Context, stationID int64, lines []shopLine) {
	if stationID == 0 || len(lines) == 0 || m == nil || m.db == nil {
		return
	}
	var kept []shopLine
	for _, l := range lines {
		if l.TypeID != 0 && l.Asked > 0 {
			kept = append(kept, shopLine{l.TypeID, l.Asked, min(max(l.Filled, 0), l.Asked)})
		}
	}
	if len(kept) == 0 {
		return
	}
	sys, reg := m.stationSystemID(ctx, stationID), m.stationRegionID(ctx, stationID)
	now := clock.Unix()
	// One stop is one visit, spread over its lines so a station's visit total
	// stays a count of stops.
	visit := 1 / float64(len(kept))
	var sb strings.Builder
	args := make([]any, 0, len(kept)*9)
	for i, l := range kept {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, stationID, l.TypeID, sys, reg, l.Asked, l.Filled, visit, now, now)
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_station_demand
			(stationID, typeID, systemID, regionID, asked, filled, visits, decayedAt, lastAt)
		VALUES `+sb.String()+`
		ON DUPLICATE KEY UPDATE
			asked = asked + VALUES(asked), filled = filled + VALUES(filled),
			visits = visits + VALUES(visits), lastAt = VALUES(lastAt)`, args...); err != nil {
		rlogf("rolebots: station demand %d: %v", stationID, err)
	}
}

// decayStationDemand ages the whole ledger on the hub sweep, the same way
// refreshLiquidity ages its own (liquidity.go), and drops what has faded.
func (m *Mod) decayStationDemand(ctx context.Context) error {
	now := clock.Unix()
	factor := fmt.Sprintf("POWER(2, -GREATEST(0, %d - decayedAt) / %f)", now, stationDemandHalfLifeSec)
	for _, q := range []string{
		`UPDATE rolebot_station_demand
		    SET asked = asked * ` + factor + `, filled = filled * ` + factor + `,
		        visits = visits * ` + factor + `, decayedAt = ` + fmt.Sprint(now),
		`DELETE FROM rolebot_station_demand WHERE asked < 0.01`,
	} {
		if _, err := m.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("station demand decay: %w", err)
		}
	}
	clearStationDemandCache()
	return nil
}

// counterDemand is one counter's decayed shopper demand for one type.
type counterDemand struct {
	StationID, SystemID, RegionID int64
	Asked, Gap                    float64
}

// shopperPull is how many units a new seller at a counter can expect to
// move: the whole gap, plus its share of what the incumbents already fill.
// Pure.
func shopperPull(asked, gap float64) float64 {
	gap = max(gap, 0)
	return gap + shopperShareFrac*max(asked-gap, 0)
}

// Pull is shopperPull for this row.
func (r counterDemand) Pull() float64 { return shopperPull(r.Asked, r.Gap) }

// stationDemandMap is the readers' copy: every counter's rows by type, and
// the gap by (type, station) for a direct lookup.
type stationDemandMap struct {
	ByType map[int64][]counterDemand
	Gap    map[[2]int64]float64
}

func (d *stationDemandMap) rows(typeID int64) []counterDemand {
	if d == nil {
		return nil
	}
	return d.ByType[typeID]
}

func (d *stationDemandMap) row(typeID, stationID int64) (counterDemand, bool) {
	for _, r := range d.rows(typeID) {
		if r.StationID == stationID {
			return r, true
		}
	}
	return counterDemand{}, false
}

var (
	stationDemandMu    sync.Mutex
	stationDemandCache *stationDemandMap
	stationDemandAt    time.Time
)

// clearStationDemandCache drops the readers' copy. Called by the sweep and
// by the economy purge (econpurge.go).
func clearStationDemandCache() {
	stationDemandMu.Lock()
	stationDemandCache, stationDemandAt = nil, time.Time{}
	stationDemandMu.Unlock()
}

// stationDemand is the readers' entry point, cached for liquidityTTL. An
// empty map (never written, or a read error) is a legitimate answer: every
// reader then ranks as it did before the ledger existed.
func (m *Mod) stationDemand(ctx context.Context) *stationDemandMap {
	stationDemandMu.Lock()
	if stationDemandCache != nil && clock.Now().Sub(stationDemandAt) < liquidityTTL {
		out := stationDemandCache
		stationDemandMu.Unlock()
		return out
	}
	stationDemandMu.Unlock()

	out := &stationDemandMap{ByType: map[int64][]counterDemand{}, Gap: map[[2]int64]float64{}}
	rows, err := m.db.QueryContext(ctx, `
		SELECT stationID, typeID, systemID, regionID, asked, GREATEST(asked - filled, 0)
		  FROM rolebot_station_demand`)
	if err != nil {
		rlogf("rolebots: station demand: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r counterDemand
		var typeID int64
		if err := rows.Scan(&r.StationID, &typeID, &r.SystemID, &r.RegionID, &r.Asked, &r.Gap); err != nil {
			rlogf("rolebots: station demand scan: %v", err)
			return out
		}
		out.ByType[typeID] = append(out.ByType[typeID], r)
		if r.Gap > 0 {
			out.Gap[[2]int64{typeID, r.StationID}] = r.Gap
		}
	}
	if err := rows.Err(); err != nil {
		rlogf("rolebots: station demand rows: %v", err)
		return out
	}
	stationDemandMu.Lock()
	stationDemandCache, stationDemandAt = out, clock.Now()
	stationDemandMu.Unlock()
	return out
}

var (
	outletOnV  atomic.Bool
	outletOnAt atomic.Int64
)

// outletEnabled is the rolebots.outlet.enabled switch (default on), re-read
// at most once a minute so an operator change lands without a restart.
func (m *Mod) outletEnabled(ctx context.Context) bool {
	now := clock.Unix()
	if at := outletOnAt.Load(); at != 0 && now-at < outletSettingsTTL {
		return outletOnV.Load()
	}
	on := true
	if m != nil && m.db != nil {
		if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingOutletEnabled); err == nil && ok {
			on = strings.TrimSpace(v) != "0"
		}
	}
	outletOnV.Store(on)
	outletOnAt.Store(now)
	return on
}
