package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Bot-to-bot trades: /rolebots/peers.
//
// The trade feed is one row per leg from one pilot's view. This page answers whether any commerce
// is between the bots themselves. rolebot_peer_trades (tradebook.go) holds pairs of a buyer and a
// seller on the roster plus the station; the page shows the ledger and three summaries: which
// pairs trade, where they meet and what changes hands. purpose is what the taker was doing:
// blueprint (buying the means of production), fitting (outfitting from the roster's production),
// haul (a spread between two bot-held orders).

// peerRow is one bot-to-bot trade, dressed for the page.
type peerRow struct {
	ID       int64  `json:"id"`
	When     string `json:"when"`
	Full     string `json:"full"`
	BuyerID  int64  `json:"buyerID"`
	Buyer    string `json:"buyer"`
	SellerID int64  `json:"sellerID"`
	Seller   string `json:"seller"`
	// Taker says which of the two walked the book. The other one's order
	// was already resting, which is the whole difference between a pilot
	// that went shopping and a pilot that was shopped at.
	TakerID  int64  `json:"takerID"`
	TypeID   int64  `json:"typeID"`
	TypeName string `json:"typeName"`
	Qty      int64  `json:"qty"`
	Price    string `json:"price"`
	Value    string `json:"value"`
	Station  string `json:"station"`
	System   string `json:"system"`
	Region   string `json:"region"`
	Purpose  string `json:"purpose"`
}

// TakerSide names who moved, for the row's badge.
func (p peerRow) TakerSide() string {
	if p.TakerID == p.BuyerID {
		return "buyer took an ask"
	}
	return "seller hit a bid"
}

type peerFilter struct {
	CharID   int64
	Who      string // the pilot name typed, resolved into CharID
	TypeID   int64
	Text     string
	Purpose  string
	SystemID int64
	// Paging on the row ID, as the feed's.
	After  int64
	Before int64
	Newer  bool
}

// peerPageSize is one page of the ledger.
const peerPageSize = 100

// peerPurposes is what the taker was doing, as the call sites write it (notePeerFills). It is a fixed list
// because a DISTINCT over the ledger would scan ninety days.
var peerPurposes = []string{
	"blueprint", "manufactured", "fitting", "hull", "materials", "minerals",
	"ore", "haul", "loot", "bid", "listing",
}

func (m *Mod) peerFilterFrom(ctx context.Context, r *http.Request) peerFilter {
	q := r.URL.Query()
	f := peerFilter{
		Text:    strings.TrimSpace(q.Get("q")),
		Purpose: strings.TrimSpace(q.Get("purpose")),
		Who:     strings.TrimSpace(q.Get("who")),
	}
	f.CharID, _ = strconv.ParseInt(q.Get("char"), 10, 64)
	if f.CharID == 0 && f.Who != "" {
		f.CharID = m.resolveCharName(ctx, f.Who)
		if f.CharID == 0 {
			f.CharID = -1 // a name nobody has: match nothing rather than everything
		}
	}
	f.TypeID, _ = strconv.ParseInt(q.Get("type"), 10, 64)
	f.SystemID, _ = strconv.ParseInt(q.Get("system"), 10, 64)
	f.After, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	f.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	f.Newer = q.Get("page") == "newer"
	return f
}

func (f peerFilter) query() string {
	v := make([]string, 0, 5)
	if f.Who != "" {
		v = append(v, "who="+url.QueryEscape(f.Who))
	} else if f.CharID > 0 {
		v = append(v, fmt.Sprintf("char=%d", f.CharID))
	}
	if f.TypeID != 0 {
		v = append(v, fmt.Sprintf("type=%d", f.TypeID))
	}
	if f.Text != "" {
		v = append(v, "q="+url.QueryEscape(f.Text))
	}
	if f.Purpose != "" {
		v = append(v, "purpose="+url.QueryEscape(f.Purpose))
	}
	if f.SystemID != 0 {
		v = append(v, fmt.Sprintf("system=%d", f.SystemID))
	}
	return strings.Join(v, "&")
}

// peerRows reads the ledger. The pilot filter matches either side, since a bot's trades with
// other bots are one question.
func (m *Mod) peerRows(ctx context.Context, f peerFilter, limit int) ([]peerRow, error) {
	where := []string{"1=1"}
	var args []any
	if f.CharID != 0 {
		where = append(where, "(p.buyerID = ? OR p.sellerID = ?)")
		args = append(args, f.CharID, f.CharID)
	}
	if f.TypeID != 0 {
		where = append(where, "p.typeID = ?")
		args = append(args, f.TypeID)
	}
	if f.Purpose != "" {
		where = append(where, "p.purpose = ?")
		args = append(args, f.Purpose)
	}
	if f.Text != "" {
		where = append(where, "(p.typeName LIKE ? OR sta.stationName LIKE ?)")
		args = append(args, "%"+f.Text+"%", "%"+f.Text+"%")
	}
	if f.SystemID != 0 {
		where = append(where, "p.systemID = ?")
		args = append(args, f.SystemID)
	}
	if f.After > 0 {
		where = append(where, "p.peerID > ?")
		args = append(args, f.After)
	}
	if f.Before > 0 {
		where = append(where, "p.peerID < ?")
		args = append(args, f.Before)
	}
	args = append(args, limit)
	order := "DESC"
	if f.Newer {
		order = "ASC"
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT p.peerID, p.ts, p.buyerID, COALESCE(bc.characterName, ''),
		       p.sellerID, COALESCE(sc.characterName, ''), p.takerID,
		       p.typeID, p.typeName, p.quantity, p.price, p.value,
		       COALESCE(sta.stationName, CONCAT('station ', p.stationID)),
		       COALESCE(ms.solarSystemName, ''), COALESCE(rg.regionName, ''), p.purpose
		  FROM rolebot_peer_trades p
		  LEFT JOIN character_ bc ON bc.characterID = p.buyerID
		  LEFT JOIN character_ sc ON sc.characterID = p.sellerID
		  LEFT JOIN staStations sta ON sta.stationID = p.stationID
		  LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = sta.solarSystemID
		  LEFT JOIN mapRegions rg ON rg.regionID = ms.regionID
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY p.peerID `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("peer ledger: %w", err)
	}
	defer rows.Close()
	out := []peerRow{}
	for rows.Next() {
		var p peerRow
		var ts int64
		var price, value float64
		if err := rows.Scan(&p.ID, &ts, &p.BuyerID, &p.Buyer, &p.SellerID, &p.Seller,
			&p.TakerID, &p.TypeID, &p.TypeName, &p.Qty, &price, &value,
			&p.Station, &p.System, &p.Region, &p.Purpose); err != nil {
			return nil, err
		}
		t := time.Unix(ts, 0).UTC()
		p.When, p.Full = t.Format("15:04:05"), t.Format("2006-01-02 15:04:05 UTC")
		p.Price, p.Value = iskShort(price), iskShort(value)
		out = append(out, p)
	}
	if f.Newer {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, rows.Err()
}

// peerTotals is the headline arithmetic, over the last week only, since all-time counts would scan the whole
// ninety-day ledger on every load and poll.
type peerTotals struct {
	Day      int64  `json:"day"`
	DayISK   string `json:"dayISK"`
	Week     int64  `json:"week"`
	WeekISK  string `json:"weekISK"`
	Pairs    int64  `json:"pairs"`    // distinct (buyer, seller) in the week
	Stations int64  `json:"stations"` // distinct stations in the week
}

func (m *Mod) peerTotals(ctx context.Context) (peerTotals, error) {
	var t peerTotals
	now := clock.Unix()
	var dayISK, weekISK sql.NullFloat64
	err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(ts > ?), 0), COALESCE(SUM(CASE WHEN ts > ? THEN value END), 0),
		       COUNT(*), COALESCE(SUM(value), 0),
		       COUNT(DISTINCT CONCAT(buyerID, ':', sellerID)),
		       COUNT(DISTINCT stationID)
		  FROM rolebot_peer_trades WHERE ts > ?`,
		now-86400, now-86400, now-7*86400).
		Scan(&t.Day, &dayISK, &t.Week, &weekISK, &t.Pairs, &t.Stations)
	if err != nil {
		return t, fmt.Errorf("peer totals: %w", err)
	}
	t.DayISK, t.WeekISK = iskShort(nullISK(dayISK)), iskShort(nullISK(weekISK))
	return t, nil
}

// peerSummary is everything above the ledger (the tiles and three top-ten tables), memoised a
// minute. None of it depends on the filter.
type peerSummary struct {
	Totals  peerTotals
	Pairs   []peerPair
	Places  []peerPlace
	Goods   []peerGood
	BuiltAt int64
}

func (m *Mod) peerSummary(ctx context.Context, fresh bool) (*peerSummary, error) {
	v, at, err := econMemoGet("peers", fresh, func() (*peerSummary, error) {
		var out peerSummary
		var err error
		if out.Totals, err = m.peerTotals(ctx); err != nil {
			return nil, err
		}
		if out.Pairs, err = m.peerPairs(ctx, 10); err != nil {
			return nil, err
		}
		if out.Places, err = m.peerPlaces(ctx, 10); err != nil {
			return nil, err
		}
		if out.Goods, err = m.peerGoods(ctx, 10); err != nil {
			return nil, err
		}
		return &out, nil
	})
	if v != nil {
		v.BuiltAt = at
	}
	return v, err
}

// peerPair is one trading relationship.
type peerPair struct {
	SellerID, BuyerID int64
	Seller, Buyer     string
	Trades            int64
	Value             string
}

// peerPlace is one station the roster does business with itself at.
type peerPlace struct {
	Station, System, Region string
	Trades                  int64
	Value                   string
}

// peerGood is one commodity moving inside the roster.
type peerGood struct {
	TypeID   int64
	TypeName string
	Trades   int64
	Units    int64
	Value    string
}

func (m *Mod) peerPairs(ctx context.Context, limit int) ([]peerPair, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT p.sellerID, COALESCE(sc.characterName, ''),
		       p.buyerID, COALESCE(bc.characterName, ''),
		       COUNT(*) n, COALESCE(SUM(p.value), 0)
		  FROM rolebot_peer_trades p
		  LEFT JOIN character_ sc ON sc.characterID = p.sellerID
		  LEFT JOIN character_ bc ON bc.characterID = p.buyerID
		 WHERE p.ts > ?
		 GROUP BY p.sellerID, p.buyerID, sc.characterName, bc.characterName
		 ORDER BY n DESC LIMIT ?`, clock.Unix()-7*86400, limit)
	if err != nil {
		return nil, fmt.Errorf("peer pairs: %w", err)
	}
	defer rows.Close()
	out := []peerPair{}
	for rows.Next() {
		var p peerPair
		var v float64
		if err := rows.Scan(&p.SellerID, &p.Seller, &p.BuyerID, &p.Buyer, &p.Trades, &v); err != nil {
			return nil, err
		}
		p.Value = iskShort(v)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (m *Mod) peerPlaces(ctx context.Context, limit int) ([]peerPlace, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT COALESCE(sta.stationName, CONCAT('station ', p.stationID)),
		       COALESCE(ms.solarSystemName, ''), COALESCE(rg.regionName, ''),
		       COUNT(*) n, COALESCE(SUM(p.value), 0)
		  FROM rolebot_peer_trades p
		  LEFT JOIN staStations sta ON sta.stationID = p.stationID
		  LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = sta.solarSystemID
		  LEFT JOIN mapRegions rg ON rg.regionID = ms.regionID
		 WHERE p.ts > ?
		 GROUP BY p.stationID, sta.stationName, ms.solarSystemName, rg.regionName
		 ORDER BY n DESC LIMIT ?`, clock.Unix()-7*86400, limit)
	if err != nil {
		return nil, fmt.Errorf("peer places: %w", err)
	}
	defer rows.Close()
	out := []peerPlace{}
	for rows.Next() {
		var p peerPlace
		var v float64
		if err := rows.Scan(&p.Station, &p.System, &p.Region, &p.Trades, &v); err != nil {
			return nil, err
		}
		p.Value = iskShort(v)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (m *Mod) peerGoods(ctx context.Context, limit int) ([]peerGood, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT p.typeID, p.typeName, COUNT(*) n,
		       COALESCE(SUM(p.quantity), 0), COALESCE(SUM(p.value), 0)
		  FROM rolebot_peer_trades p
		 WHERE p.ts > ?
		 GROUP BY p.typeID, p.typeName
		 ORDER BY n DESC LIMIT ?`, clock.Unix()-7*86400, limit)
	if err != nil {
		return nil, fmt.Errorf("peer goods: %w", err)
	}
	defer rows.Close()
	out := []peerGood{}
	for rows.Next() {
		var g peerGood
		var v float64
		if err := rows.Scan(&g.TypeID, &g.TypeName, &g.Trades, &g.Units, &v); err != nil {
			return nil, err
		}
		g.Value = iskShort(v)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (m *Mod) handlePeers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	f := m.peerFilterFrom(ctx, r)
	rows, err := m.peerRows(ctx, f, peerPageSize)
	if err != nil {
		espError(w, "rolebot-peers", err.Error())
		return
	}
	sum, err := m.peerSummary(ctx, r.URL.Query().Get("refresh") == "1")
	if err != nil && sum == nil {
		espError(w, "rolebot-peers", err.Error())
		return
	}
	var firstID, lastID int64
	if len(rows) > 0 {
		firstID, lastID = rows[0].ID, rows[len(rows)-1].ID
	}
	paged := f.Before > 0 || f.After > 0
	older, newer := pagerLinks("/rolebots/peers", f.query(), firstID, lastID, len(rows), peerPageSize, paged)

	espRender(w, "rolebots_peers.html", map[string]any{
		"Page":       "rolebot-economy",
		"Tab":        "peers",
		"Rows":       rows,
		"Summary":    sum,
		"Purposes":   peerPurposes,
		"Who":        f.Who,
		"PurposeSel": f.Purpose,
		"Text":       f.Text,
		"TypeSel":    f.TypeID,
		"Query":      f.query(),
		"Older":      older,
		"Newer":      newer,
		"Paged":      paged,
		"Retention":  peerTradeRetentionDays,
	})
}

func (m *Mod) handlePeersAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	// Rows only; the summary is memoised and drawn at page load.
	f := m.peerFilterFrom(ctx, r)
	f.Newer = false
	rows, err := m.peerRows(ctx, f, peerPageSize)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"rows": rows,
		"now":  time.Now().UTC().Format("15:04:05 UTC"),
	})
}
