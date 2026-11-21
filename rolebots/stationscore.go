package rolebots

// Station scores: the contest inside a system. Once a pilot has picked a system to trade in
// (distance and liquidity, liquidity.go), a per-station score picks the dock to land at. Five terms:
//   - amenities: exclusive services only (medical bay, factory floor); universal ones score nothing.
//   - liquidity: decayed ISK changing hands at this dock, seeder trades excluded (liquidity.go).
//     Homes and factories are counted for the ESP map only.
//   - guests: decayed dock arrivals, measured like gate arrivals in the traffic term.
//   - book: player and bot orders listed here and their worth, a small term. A real player's order
//     counts humanOrderWeight bot orders (fortune.go).
//   - fortune: this world's luck for the station (fortune.go), so the same amenities do not pick the
//     same dock on every server.
// The hub sweep stores the score; stocking goods and fencing loot read it. Distance never appears,
// since both ends are in one system.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// The exclusive amenities and what each is worth. Bits are staServices.serviceID (via
// staStations.operationID and staOperationServices). Services absent here (Market, Fitting, Repair,
// Docking, Storage, Insurance, News, agents) score zero: ubiquitous services separate no stations.
var stationAmenities = []struct {
	Bit  int64
	Pts  float64
	Name string
}{
	{512, 30, "Cloning"},      // a medical bay makes a station a place people LIVE
	{8192, 30, "Factory"},     // goods are made here, week after week
	{16384, 18, "Laboratory"}, // research parks a blueprint (and its owner) for months
	{8388608, 15, "Jump Clone Facility"},
	{32, 12, "Refinery"}, // where the ore in this system's belts becomes minerals
	{16, 8, "Reprocessing Plant"},
	{1024, 5, "Surgery"},
	{2048, 5, "DNA Therapy"},
}

const (
	// Liquidity: one e-fold of decayed millions changing hands here.
	stationWeightLiquidity = 20.0

	// Footfall. One e-fold of decayed dock arrivals, against the same
	// half-life the system traffic term uses (decayTraffic ages both).
	stationWeightGuests = 22.0

	// The book terms are kept small.
	stationWeightOrders = 10.0
	stationWeightBook   = 6.0
)

// StationScore is one station's standing in its own system's contest.
type StationScore struct {
	StationID int64
	SystemID  int64
	Name      string
	Services  int64 // serviceMask, kept for display

	Homes        int     // ESP map only, not scored
	Factories    int     // ESP map only, not scored
	Liquidity    float64 // decayed ISK traded here, seeders excluded
	Guests       float64 // decayed dock arrivals
	PlayerOrders int64
	PlayerISK    float64
	// The real players' share of PlayerOrders/PlayerISK, not bots.
	HumanOrders int64
	HumanISK    float64
	// Luck is this world's draw for the station in [-1, 1) (stationFortune), set before stationRaw.
	Luck float64

	AmenityTerm   float64
	LiquidityTerm float64
	GuestTerm     float64
	BookTerm      float64
	FortuneTerm   float64

	Raw   float64 // the sum, used by every comparison
	Score float64 // 0..100 against the best dock in the universe, for display
}

// stationRaw is the arithmetic, separate so the harness can assert it without a database. Amenities
// count per fact; liquidity, guests and the book are measurements and use the log treatment.
func stationRaw(s *StationScore) {
	s.AmenityTerm = 0
	for _, a := range stationAmenities {
		if s.Services&a.Bit != 0 {
			s.AmenityTerm += a.Pts
		}
	}
	s.LiquidityTerm = stationWeightLiquidity * liquidityValue(s.Liquidity)
	s.GuestTerm = stationWeightGuests * math.Log1p(s.Guests)
	s.BookTerm = stationWeightOrders*math.Log1p(humanWeighted(float64(s.PlayerOrders), float64(s.HumanOrders))) +
		stationWeightBook*math.Log1p(humanWeighted(s.PlayerISK, s.HumanISK)/1e6)
	s.FortuneTerm = fortuneDockPts * s.Luck
	s.Raw = s.AmenityTerm + s.LiquidityTerm + s.GuestTerm + s.BookTerm + s.FortuneTerm
}

// recordStationDock books one arrival at a dock, the same way recordJump
// books a gate transit. Best-effort; the ledger is a measurement.
func (m *Mod) recordStationDock(_ context.Context, stationID int64) {
	if stationID == 0 {
		return
	}
	bufDock(stationID, clock.Unix()) // trafficbuf.go
}

// computeStationScores builds the whole per-station table in memory, the
// same shape as computeHubScores: one aggregate per fact over the whole
// database, applied to the candidate set in Go.
func (m *Mod) computeStationScores(ctx context.Context) ([]*StationScore, error) {
	byID := map[int64]*StationScore{}

	// Every station, with its service mask: a SUM over the operation's service rows, the same join
	// GetStationItemBits serves the client, so score and station window agree.
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, s.solarSystemID, s.stationName,
		       COALESCE(CAST(SUM(os.serviceID) AS SIGNED), 0)
		  FROM staStations s
		  LEFT JOIN staOperationServices os ON os.operationID = s.operationID
		 GROUP BY s.stationID, s.solarSystemID, s.stationName`)
	if err != nil {
		return nil, fmt.Errorf("station candidates: %w", err)
	}
	for rows.Next() {
		s := &StationScore{}
		if err := rows.Scan(&s.StationID, &s.SystemID, &s.Name, &s.Services); err != nil {
			rows.Close()
			return nil, err
		}
		byID[s.StationID] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(byID) == 0 {
		return nil, nil
	}

	scan := func(query string, apply func(s *StationScore, a, b float64)) error {
		r, err := m.db.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("station scan: %w", err)
		}
		defer r.Close()
		for r.Next() {
			var st int64
			var a, b float64
			if err := r.Scan(&st, &a, &b); err != nil {
				return err
			}
			if s, ok := byID[st]; ok {
				apply(s, a, b)
			}
		}
		return r.Err()
	}

	if err := scan(`
		SELECT stationID, docks, 0 FROM rolebot_traffic_stations`,
		func(s *StationScore, docks, _ float64) { s.Guests = docks }); err != nil {
		return nil, err
	}
	// Where the roster lives and builds, for the ESP map only. Home is the medical clone station: one
	// pilot, one home.
	if err := scan(`
		SELECT c.homeStationID, COUNT(*), 0
		  FROM character_ c
		  JOIN rolebot_characters b ON b.characterID = c.characterID
		 WHERE c.homeStationID > 0
		 GROUP BY c.homeStationID`,
		func(s *StationScore, n, _ float64) { s.Homes = int(n) }); err != nil {
		return nil, err
	}
	if err := scan(`
		SELECT p.industryStationID, COUNT(*), 0
		  FROM rolebot_profiles p
		  JOIN rolebot_characters b ON b.characterID = p.characterID
		 WHERE p.industryStationID > 0
		 GROUP BY p.industryStationID`,
		func(s *StationScore, n, _ float64) { s.Factories = int(n) }); err != nil {
		return nil, err
	}
	if err := scan(`
		SELECT stationID, isk, 0 FROM rolebot_station_liquidity`,
		func(s *StationScore, isk, _ float64) { s.Liquidity = isk }); err != nil {
		return nil, err
	}
	// Player/bot orders only, exactly as the hub market term counts them:
	// the seeders' furniture (all three, liquidityExcluded) is on every station and separates none.
	if err := scan(`
		SELECT stationID, COUNT(*), COALESCE(SUM(price * volRemaining), 0)
		  FROM market_orders
		 WHERE orderState = 1 AND volRemaining > 0 AND charID NOT IN (`+liquidityExcluded+`)
		 GROUP BY stationID`,
		func(s *StationScore, n, isk float64) { s.PlayerOrders, s.PlayerISK = int64(n), isk }); err != nil {
		return nil, err
	}
	// The real players' share of that book (fortune.go).
	if err := scan(`
		SELECT o.stationID, COUNT(*), COALESCE(SUM(o.price * o.volRemaining), 0)
		  FROM market_orders o
		  LEFT JOIN rolebot_characters b ON b.characterID = o.charID
		 WHERE `+humanOrdersFilter+`
		 GROUP BY o.stationID`,
		func(s *StationScore, n, isk float64) { s.HumanOrders, s.HumanISK = int64(n), isk }); err != nil {
		return nil, err
	}

	luck := m.stationFortune(ctx)
	out := make([]*StationScore, 0, len(byID))
	best := 0.0
	for _, s := range byID {
		s.Luck = luck(s.StationID)
		stationRaw(s)
		if s.Raw > best {
			best = s.Raw
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StationID < out[j].StationID })
	for _, s := range out {
		if best > 0 {
			s.Score = 100 * s.Raw / best
		}
	}
	return out, nil
}

// stationStoreBatch is how many stations go in one INSERT.
const stationStoreBatch = 250

// storeStationScores replaces the stored table, batched.
func (m *Mod) storeStationScores(ctx context.Context, scores []*StationScore) error {
	now := clock.Unix()
	const cols = 19
	row := "(" + strings.TrimSuffix(strings.Repeat("?,", cols), ",") + ")"
	for start := 0; start < len(scores); start += stationStoreBatch {
		end := start + stationStoreBatch
		if end > len(scores) {
			end = len(scores)
		}
		chunk := scores[start:end]
		vals := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*cols)
		for _, s := range chunk {
			vals = append(vals, row)
			// anchorTerm is a retired column, written zero.
			args = append(args,
				s.StationID, s.SystemID, s.Services,
				s.AmenityTerm, 0.0, s.LiquidityTerm, s.Liquidity, s.GuestTerm, s.BookTerm,
				s.Homes, s.Factories, s.Guests,
				s.PlayerOrders, s.PlayerISK, s.Raw, s.Score, now,
				s.FortuneTerm, s.HumanOrders)
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_station_scores
				(stationID, systemID, services, amenityTerm, anchorTerm,
				 liquidityTerm, liquidity, guestTerm,
				 bookTerm, homes, factories, guests,
				 playerOrders, playerISK, raw, score, updatedAt,
				 fortuneTerm, humanOrders)
			VALUES `+strings.Join(vals, ",")+`
			ON DUPLICATE KEY UPDATE
				systemID = VALUES(systemID), services = VALUES(services),
				amenityTerm = VALUES(amenityTerm), anchorTerm = VALUES(anchorTerm),
				liquidityTerm = VALUES(liquidityTerm), liquidity = VALUES(liquidity),
				guestTerm = VALUES(guestTerm), bookTerm = VALUES(bookTerm),
				homes = VALUES(homes),
				factories = VALUES(factories), guests = VALUES(guests),
				playerOrders = VALUES(playerOrders), playerISK = VALUES(playerISK),
				raw = VALUES(raw), score = VALUES(score), updatedAt = VALUES(updatedAt),
				fortuneTerm = VALUES(fortuneTerm), humanOrders = VALUES(humanOrders)`,
			args...); err != nil {
			return fmt.Errorf("storeStationScores: %w", err)
		}
	}
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_station_scores WHERE updatedAt < ?`, now); err != nil {
		return fmt.Errorf("storeStationScores prune: %w", err)
	}
	return nil
}

// stationScores is the planners' read: station to score, cached for a minute. An empty map (nothing
// swept yet) is a legitimate answer: every caller falls back to ranking by the order book.
func (m *Mod) stationScores(ctx context.Context) map[int64]*StationScore {
	stationCacheMu.Lock()
	if stationCache != nil && clock.Now().Sub(stationCacheAt) < liquidityTTL {
		out := stationCache
		stationCacheMu.Unlock()
		return out
	}
	stationCacheMu.Unlock()

	rows, err := m.db.QueryContext(ctx, `
		SELECT stationID, systemID, services, homes, factories, liquidity,
		       guests, playerOrders, playerISK, raw, score
		  FROM rolebot_station_scores`)
	if err != nil {
		rlogf("rolebots: station scores: %v", err)
		return map[int64]*StationScore{}
	}
	defer rows.Close()
	out := map[int64]*StationScore{}
	for rows.Next() {
		s := &StationScore{}
		if err := rows.Scan(&s.StationID, &s.SystemID, &s.Services,
			&s.Homes, &s.Factories, &s.Liquidity,
			&s.Guests, &s.PlayerOrders, &s.PlayerISK, &s.Raw, &s.Score); err != nil {
			return out
		}
		out[s.StationID] = s
	}
	stationCacheMu.Lock()
	stationCache, stationCacheAt = out, clock.Now()
	stationCacheMu.Unlock()
	return out
}

var (
	stationCacheMu sync.Mutex
	stationCache   map[int64]*StationScore
	stationCacheAt time.Time
)

func indexStations(scores []*StationScore) map[int64]*StationScore {
	out := make(map[int64]*StationScore, len(scores))
	for _, s := range scores {
		out[s.StationID] = s
	}
	return out
}

// bestDockPerSystem runs the contest: given candidate stations (already filtered by the caller's
// manners, tradeStationClause), it answers each system's winning dock. The order book breaks ties
// and carries a fresh install where the sweep has not run yet.
func bestDockPerSystem(scores map[int64]*StationScore, stations []dockCandidate) map[int64]int64 {
	type held struct {
		station int64
		raw     float64
		book    int64
	}
	best := map[int64]held{}
	for _, c := range stations {
		raw := 0.0
		if s := scores[c.StationID]; s != nil {
			raw = s.Raw
		}
		cur, seen := best[c.SystemID]
		better := !seen || raw > cur.raw ||
			(raw == cur.raw && c.Book > cur.book) ||
			(raw == cur.raw && c.Book == cur.book && c.StationID < cur.station)
		if better {
			best[c.SystemID] = held{c.StationID, raw, c.Book}
		}
	}
	out := make(map[int64]int64, len(best))
	for sys, h := range best {
		out[sys] = h.station
	}
	return out
}

// dockCandidate is one allowed station in one candidate system, with its
// live book count riding along as the tiebreak.
type dockCandidate struct {
	StationID int64
	SystemID  int64
	Book      int64
}
