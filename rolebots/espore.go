package rolebots

// The Ore page: what the roster digs, what it is worth, and where it goes.
//
// The Mineral Supply page asks whether the shelf is stocked; this one asks what became of the
// rock behind it. rolebot_ore_flow (oreflow.go) measures it. The headline is the sink share:
// the fraction mined that left through an NPC bid instead of reaching a refiner. The economics
// table gives, per ore and from the live book, the break-even yield at which refining equals
// the ore's own bid, against the constants resolveMiningDisposal uses (oreIntrinsicSpread,
// refineWorthEdge). The volume pair shows why ore is local and minerals travel (Arkonor
// refines 400:1). Only the flow counter is stored; prices, yields and volumes are read live.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// oreEconRows is how much of the ore catalogue the economics table shows. All
// 60 reprocessable types fit comfortably; the cap is here so a database that
// grew a hundred more does not turn one page into a scroll.
const oreEconRows = 60

// oreRefinerRows is how many pilots the refiner panel names. A shortlist: the
// point is what the best of the roster can do and how far the rest are from
// it, which two dozen rows show and two thousand hide.
const oreRefinerRows = 25

// oreFlowWindowDays is the flow window. A day is what an operator watching a
// change wants; the seven-day column beside it is what says whether the day
// was ordinary.
const (
	oreFlowWindowDays = 1
	oreFlowWeekDays   = 7
)

// oreEconRow is one ore type against the live book.
type oreEconRow struct {
	TypeID    int64
	Name      string
	Group     string
	Volume    float64 // m³ per unit
	Portion   int64   // units per reprocessing batch
	NPCBid    float64 // best NPC bid, ISK/unit
	BotBid    float64 // best player bid, ISK/unit: a refiner buying rock
	Ask       float64 // best player ask, ISK/unit
	Intrinsic float64 // refined value of one unit at 100% yield, ISK
	BreakEven float64 // net yield at which refining matches the best bid
	RawM3     float64 // m³ of one batch as rock
	RefM3     float64 // m³ of what that batch refines into
	Mined     int64   // units out of belts in the window
}

// Compression is how many times smaller the batch gets when refined. The
// number that decides the geography of the whole ore economy: a 400× ore
// cannot be hauled to a hub as rock, so it must be bought where it is mined.
func (r oreEconRow) Compression() float64 {
	if r.RefM3 <= 0 {
		return 0
	}
	return r.RawM3 / r.RefM3
}

// BestBid is the side paying most for the rock, which is what a miner's dumpToBestBid takes
// and so the price every comparison on this page uses.
func (r oreEconRow) BestBid() float64 {
	if r.BotBid > r.NPCBid {
		return r.BotBid
	}
	return r.NPCBid
}

// ISKPerM3Raw and ISKPerM3Refined are the haulability pair. Raw is what a
// hold full of this rock is worth; refined is what the same hold is worth
// once it has been through a reprocessing plant. The gap is the freight
// argument, and for the deep ores it is three orders of magnitude.
func (r oreEconRow) ISKPerM3Raw() float64 {
	if r.Volume <= 0 {
		return 0
	}
	return r.BestBid() / r.Volume
}

func (r oreEconRow) ISKPerM3Refined() float64 {
	if r.RefM3 <= 0 || r.Portion <= 0 {
		return 0
	}
	return r.Intrinsic * float64(r.Portion) / r.RefM3
}

// PaysToRefine is whether a pilot at the roster's reference yield would come
// out ahead reprocessing this rock rather than selling it. Drawn as a mark on
// the row, because "which of these should anybody be refining" is the whole
// question the column exists to answer.
func (r oreEconRow) PaysToRefine() bool {
	return r.BreakEven > 0 && r.BreakEven <= oreReferenceYield()
}

// Fmt helpers. The page is parsed with no funcMap (TestESPPagesParse parses
// module pages with none at all), so every formatted number is either a
// preformatted string or a method on the row.
func (r oreEconRow) NPCBidStr() string    { return oreOrNone(r.NPCBid) }
func (r oreEconRow) BotBidStr() string    { return oreOrNone(r.BotBid) }
func (r oreEconRow) AskStr() string       { return oreOrNone(r.Ask) }
func (r oreEconRow) IntrinsicStr() string { return iskShort(r.Intrinsic) }
func (r oreEconRow) BreakEvenStr() string {
	if r.BreakEven <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", r.BreakEven*100)
}
func (r oreEconRow) CompressionStr() string {
	if c := r.Compression(); c > 0 {
		return fmt.Sprintf("%.0f×", c)
	}
	return "—"
}
func (r oreEconRow) RawM3Str() string  { return fmt.Sprintf("%.0f", r.RawM3) }
func (r oreEconRow) RefM3Str() string  { return fmt.Sprintf("%.1f", r.RefM3) }
func (r oreEconRow) RawIPMStr() string { return iskShort(r.ISKPerM3Raw()) }
func (r oreEconRow) RefIPMStr() string { return iskShort(r.ISKPerM3Refined()) }
func (r oreEconRow) MinedStr() string  { return iskShort(float64(r.Mined)) }

// oreOrNone prints a price, or names its absence. "none" rather than a dash
// or a zero because an ore with no ask is the page's most important fact and
// a blank cell reads as a query that did not finish.
func oreOrNone(v float64) string {
	if v <= 0 {
		return "none"
	}
	return iskShort(v)
}

// oreReferenceYield is the net after-tax yield the economics table judges an ore against: a
// station at refineStationEffDefault, a pilot with Refining IV and Refinery Efficiency IV, less
// the standard tax at zero standing (npcRefineTax). It models a pilot who has trained, not a
// roster average. A func, not a var, so it reads the reprocessing dials after the settings cache
// has loaded.
func oreReferenceYield() float64 {
	return refineEffYield(refineStationEffDefault, 4, 4, 0) * refineNetFactor()
}

// refineNetFactor is what a pilot keeps after the standard NPC station tax at zero standing.
func refineNetFactor() float64 { return 1 - npcRefineTax(0) }

// oreRefinerRow is one pilot's refining arithmetic.
type oreRefinerRow struct {
	CharID    int64
	Name      string
	Refining  int
	Efficient int
	Yield     float64 // net of the standard tax at a default station
	NextGain  float64 // yield a pilot would add by taking the cheaper of the
	// two skills up one level; 0 when both are maxed
	NextSkill string
	Pays      bool // clears the roster's own refine-worth bar
}

func (r oreRefinerRow) YieldStr() string { return fmt.Sprintf("%.1f%%", r.Yield*100) }
func (r oreRefinerRow) NextStr() string {
	if r.NextGain <= 0 {
		return "—"
	}
	return fmt.Sprintf("+%.1f%% (%s)", r.NextGain*100, r.NextSkill)
}

// oreRefinerBand is one (Refining, Refinery Efficiency) pair and how many pilots hold it.
// See oreRefiners for why the panel shows a distribution and not a league table.
type oreRefinerBand struct {
	Refining  int
	Efficient int
	Pilots    int
	Share     float64 // of the whole enabled roster
	Yield     float64
	Pays      bool
	OneShort  bool // one more level of Refining clears the bar
}

func (b oreRefinerBand) YieldStr() string { return fmt.Sprintf("%.1f%%", b.Yield*100) }
func (b oreRefinerBand) ShareStr() string { return fmt.Sprintf("%.1f%%", b.Share*100) }
func (b oreRefinerBand) PilotsStr() string {
	return iskShort(float64(b.Pilots))
}

// Verdict is the band's one-word standing against the disposal bar. Three
// states rather than two, because "one level short" is the only one an
// operator can do anything about and folding it into "no" hides the whole
// point of the panel.
func (b oreRefinerBand) Verdict() string {
	switch {
	case b.Pays:
		return "refines"
	case b.OneShort:
		return "one level short"
	}
	return "sells the rock"
}

// oreRefinerCensus is the whole-roster count behind the headline tiles. Total is every
// enabled pilot; Trained is those with any refining skill. Both cover the same roster.
type oreRefinerCensus struct {
	Total    int
	Trained  int
	Pays     int
	OneShort int
}

func (c oreRefinerCensus) PaysShareStr() string {
	if c.Total == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", float64(c.Pays)/float64(c.Total)*100)
}

// oreBeltRow is one region's remaining rock.
type oreBeltRow struct {
	RegionID   int64
	Name       string
	Systems    int
	Fullness   float64 // 0..1 over the systems with a recorded field
	M3Left     float64
	M3Total    float64
	FullnessPc int
}

// handleOreBoard renders /rolebots/ore.
func (m *Mod) handleOreBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method == http.MethodPost {
		m.handleOreBoardPost(w, r, ctx)
		return
	}

	day, errDay := m.oreFlowTotals(ctx, oreFlowWindowDays)
	week, errWeek := m.oreFlowTotals(ctx, oreFlowWeekDays)
	if errDay != nil || errWeek != nil {
		err := errDay
		if err == nil {
			err = errWeek
		}
		espError(w, "rolebot-ore", err.Error())
		return
	}

	econ, err := m.oreEconomics(ctx, oreFlowWeekDays)
	if err != nil {
		espError(w, "rolebot-ore", err.Error())
		return
	}
	refiners, refinerBands, refinerCensus, err := m.oreRefiners(ctx, oreRefinerRows)
	if err != nil {
		espError(w, "rolebot-ore", err.Error())
		return
	}
	// The belt panel is context for the rest and may fail without the page being worth less;
	// a roster that has never recorded a field has none yet.
	belts, beltErr := m.oreBeltRegions(ctx)
	beltNote := ""
	if beltErr != nil {
		beltNote = beltErr.Error()
	}

	espRender(w, "rolebots_ore.html", map[string]any{
		"Page":           "rolebot-industry",
		"Tab":            "ore",
		"Day":            day,
		"Week":           week,
		"SinkShareDay":   orePct(oreSinkShare(day)),
		"SinkShareWeek":  orePct(oreSinkShare(week)),
		"RefinedShare":   orePct(oreRefinedShare(week)),
		"ListedShareDay": orePct(oreListedShare(day)),
		"BoughtDay":      iskShort(float64(oreStageUnits(day, oreStageBought))),
		"RealisedYield":  orePct(oreRealisedYield(week)),
		"Econ":           econ,
		"Refiners":       refiners,
		"RefinerBands":   refinerBands,
		"RefinerTotal":   refinerCensus.Total,
		"RefinerTrained": refinerCensus.Trained,
		"RefinerPays":    refinerCensus.Pays,
		"RefinerShort":   refinerCensus.OneShort,
		"RefinerShare":   refinerCensus.PaysShareStr(),
		"RefinerNamed":   len(refiners),
		"ReferenceYield": fmt.Sprintf("%.0f%%", oreReferenceYield()*100),
		"RefineBar":      fmt.Sprintf("%.0f%%", oreIntrinsicSpread*refineWorthEdge*100),
		"Belts":          belts,
		"BeltNote":       beltNote,
		"AskEdge":        fmt.Sprintf("%.2f", oreAskRefinerEdge()),
		"SubEdge":        fmt.Sprintf("%.2f", oreSubEdge()),
		"KeepDays":       oreFlowKeepDays,
		"Msg":            r.URL.Query().Get("msg"),
		"Err":            r.URL.Query().Get("err"),
	})
}

// handleOreBoardPost handles the two edge dials. Each redirects with a sentence saying what
// changed. The ore economy has no on/off gates (oresettings.go).
func (m *Mod) handleOreBoardPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	const self = "/rolebots/ore"
	switch r.FormValue("action") {
	case "askEdge", "subEdge":
		key := settingOreAskEdge
		if r.FormValue("action") == "subEdge" {
			key = settingOreSubEdge
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("value")), 64)
		if err != nil || v <= 0 {
			redirect(w, r, self, "", "that has to be a multiple, like 1.10")
			return
		}
		if err := m.setOreEdge(ctx, key, v); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		redirect(w, r, self, fmt.Sprintf("%s is now %.2f", key, v), "")
	default:
		http.Redirect(w, r, self, http.StatusSeeOther)
	}
}

// oreStageUnits pulls one stage out of a totals slice.
func oreStageUnits(rows []oreFlowTotal, stage string) int64 {
	for _, t := range rows {
		if t.Stage == stage {
			return t.Units
		}
	}
	return 0
}

// oreSinkShare is the fraction of mined units that went to an NPC bid. The denominator is
// mined, not disposed: a unit still in a hangar is not lost. Zero mined answers 0.
func oreSinkShare(rows []oreFlowTotal) float64 {
	mined := oreStageUnits(rows, oreStageMined)
	if mined <= 0 {
		return 0
	}
	return float64(oreStageUnits(rows, oreStageSoldNPC)) / float64(mined)
}

// oreListedShare is the fraction of mined units rested on the market as an ask (oreask.go),
// the supply side of the ore market. It is not a disposal, since the units are still the
// miner's, so it is outside the sink/refine pair.
func oreListedShare(rows []oreFlowTotal) float64 {
	mined := oreStageUnits(rows, oreStageMined)
	if mined <= 0 {
		return 0
	}
	return float64(oreStageUnits(rows, oreStageListed)) / float64(mined)
}

// oreRefinedShare is the fraction of mined units that reached a reprocessing plant, the
// sink share's opposite number.
func oreRefinedShare(rows []oreFlowTotal) float64 {
	mined := oreStageUnits(rows, oreStageMined)
	if mined <= 0 {
		return 0
	}
	return float64(oreStageUnits(rows, oreStageRefinedIn)) / float64(mined)
}

// oreRealisedYield is minerals out over ore in, measured. It is a ratio read against itself
// over time, not a percentage (a batch of ore yields many times its unit count in minerals):
// a fall means refining is moving to less skilled pilots or worse stations.
func oreRealisedYield(rows []oreFlowTotal) float64 {
	in := oreStageUnits(rows, oreStageRefinedIn)
	if in <= 0 {
		return 0
	}
	return float64(oreStageUnits(rows, oreStageRefinedOut)) / float64(in)
}

func orePct(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

// oreEconomics builds the economics table: every reprocessable ore against the live order
// book, valued by the best bid on each mineral it yields. The arithmetic is oreValues'
// (orerefine.go), shared with every pilot decision. Scope is the whole game (nil regions), so
// intrinsics can differ from a pilot's; the break-even ratio comes from one book and cannot.
func (m *Mod) oreEconomics(ctx context.Context, flowDays int) ([]oreEconRow, error) {
	vals, err := m.oreValues(ctx, nil, oreValueBid)
	if err != nil {
		return nil, err
	}
	out := make([]oreEconRow, 0, len(vals))
	for _, v := range vals {
		out = append(out, oreEconRow{
			TypeID: v.TypeID, Name: v.Name, Group: v.Group,
			Volume: v.Volume, Portion: v.Portion,
			Intrinsic: v.Intrinsic, RawM3: v.RawM3(), RefM3: v.RefM3,
		})
	}
	byID := map[int64]*oreEconRow{}
	for i := range out {
		byID[out[i].TypeID] = &out[i]
	}

	// The book, split by who is bidding. accountID = 0 is the repo's NPC
	// separator, the same one every count on the station pages uses.
	br, err := m.db.QueryContext(ctx, `
		SELECT o.typeID, o.bid, MAX(c.accountID > 0), MAX(o.price), MIN(o.price)
		  FROM market_orders o
		  JOIN character_ c ON c.characterID = o.charID
		  JOIN invTypes t  ON t.typeID = o.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE g.categoryID = 25 AND o.orderState = 1 AND o.volRemaining > 0
		 GROUP BY o.typeID, o.bid, (c.accountID > 0)`)
	if err != nil {
		return nil, fmt.Errorf("ore book: %w", err)
	}
	for br.Next() {
		var typeID, bid, isPlayer int64
		var maxP, minP float64
		if err := br.Scan(&typeID, &bid, &isPlayer, &maxP, &minP); err != nil {
			br.Close()
			return nil, fmt.Errorf("ore book scan: %w", err)
		}
		e := byID[typeID]
		if e == nil {
			continue
		}
		switch {
		case bid == 1 && isPlayer == 1:
			if maxP > e.BotBid {
				e.BotBid = maxP
			}
		case bid == 1:
			if maxP > e.NPCBid {
				e.NPCBid = maxP
			}
		default:
			// Asks from either side land in one column.
			if e.Ask == 0 || minP < e.Ask {
				e.Ask = minP
			}
		}
	}
	br.Close()
	if err := br.Err(); err != nil {
		return nil, fmt.Errorf("ore book: %w", err)
	}

	// Break-even: the net yield at which refining a unit is worth what the best bid pays. Above
	// it refining gains; below it the bid is better (miningRefineWorth, per ore, on the live book).
	for i := range out {
		out[i].BreakEven = oreBreakEvenYield(vals[out[i].TypeID], out[i].BestBid())
	}

	// What the roster has actually been digging, so a table of sixty rocks
	// leads with the handful anybody works.
	if mined, err := m.oreFlowByType(ctx, flowDays, oreEconRows); err == nil {
		for _, f := range mined {
			if e := byID[f.TypeID]; e != nil {
				e.Mined = f.Mined
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Mined != out[j].Mined {
			return out[i].Mined > out[j].Mined
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > oreEconRows {
		out = out[:oreEconRows]
	}
	return out, nil
}

// mineralPrices (orerefine.go) takes a region scope and a side: a buyer choosing between rock and metal needs
// the ask it would pay, everyone else the bid a refiner can realise.

// oreRefiners is the roster's refining capability: the distribution over the two skills, how
// many clear the disposal rule's bar, and how many are one level short of it. It reads skills,
// not the flow counter, because the question is who could refine. One query over literal
// skill typeIDs: a bind parameter inside a grouped CASE collapses on MariaDB (see skillKindCase).
func (m *Mod) oreRefiners(ctx context.Context, limit int) ([]oreRefinerRow, []oreRefinerBand, oreRefinerCensus, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID, COALESCE(ch.characterName, ''),
		       COALESCE(MAX(CASE WHEN e.typeID = 3385 THEN a.valueInt END), 0),
		       COALESCE(MAX(CASE WHEN e.typeID = 3389 THEN a.valueInt END), 0)
		  FROM rolebot_characters c
		  LEFT JOIN character_ ch ON ch.characterID = c.characterID
		  LEFT JOIN entity e ON e.ownerID = c.characterID AND e.flag IN (7,61)
		                    AND e.typeID IN (3385, 3389)
		  LEFT JOIN entity_attributes a ON a.itemID = e.itemID AND a.attributeID = 280
		 WHERE c.enabled = 1
		 GROUP BY c.characterID, ch.characterName`)
	if err != nil {
		return nil, nil, oreRefinerCensus{}, fmt.Errorf("refiner roster: %w", err)
	}
	defer rows.Close()

	// The bar the module applies (mining.go), stated once so the panel and resolveMiningDisposal agree.
	bar := oreIntrinsicSpread * refineWorthEdge

	var census oreRefinerCensus
	bands := map[[2]int]*oreRefinerBand{}
	var top []oreRefinerRow

	for rows.Next() {
		var charID int64
		var name string
		var ref, eff int
		if err := rows.Scan(&charID, &name, &ref, &eff); err != nil {
			return nil, nil, oreRefinerCensus{}, fmt.Errorf("refiner roster scan: %w", err)
		}
		census.Total++
		row := oreRefinerRow{CharID: charID, Name: name, Refining: ref, Efficient: eff}
		net := refineNetFactor()
		row.Yield = refineEffYield(refineStationEffDefault, ref, eff, 0) * net
		row.Pays = row.Yield >= bar
		// One more level of Refining specifically: that is what the floor dial buys. It is also the
		// cheaper road, since Refinery Efficiency is rank 3 and needs Refining V first.
		if ref < 5 {
			row.NextGain = refineEffYield(refineStationEffDefault, ref+1, eff, 0)*net - row.Yield
			row.NextSkill = "Refining"
		} else if eff < 5 {
			row.NextGain = refineEffYield(refineStationEffDefault, ref, eff+1, 0)*net - row.Yield
			row.NextSkill = "Refinery Efficiency"
		}
		oneShort := !row.Pays && ref < 5 &&
			refineEffYield(refineStationEffDefault, ref+1, eff, 0)*net >= bar

		if row.Pays {
			census.Pays++
		} else if oneShort {
			census.OneShort++
		}
		if ref > 0 || eff > 0 {
			census.Trained++
			top = append(top, row)
		}

		key := [2]int{ref, eff}
		b := bands[key]
		if b == nil {
			b = &oreRefinerBand{
				Refining: ref, Efficient: eff, Yield: row.Yield,
				Pays: row.Pays, OneShort: oneShort,
			}
			bands[key] = b
		}
		b.Pilots++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, oreRefinerCensus{}, fmt.Errorf("refiner roster: %w", err)
	}

	byYield := func(a, b oreRefinerBand) bool {
		if a.Yield != b.Yield {
			return a.Yield > b.Yield
		}
		return a.Refining > b.Refining
	}
	out := make([]oreRefinerBand, 0, len(bands))
	for _, b := range bands {
		if census.Total > 0 {
			b.Share = float64(b.Pilots) / float64(census.Total)
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return byYield(out[i], out[j]) })

	sort.Slice(top, func(i, j int) bool {
		if top[i].Yield != top[j].Yield {
			return top[i].Yield > top[j].Yield
		}
		return top[i].Name < top[j].Name
	})
	if len(top) > limit {
		top = top[:limit]
	}
	return top, out, census, nil
}

// oreBeltRegions is how much rock each region has left, from the fullness rows a pilot writes
// when it works a field (beltbudget.go). It joins asteroidFields by seed, as the belt search
// does, so a reseed drops the row and the system reads as untouched.
func (m *Mod) oreBeltRegions(ctx context.Context) ([]oreBeltRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.regionID, COUNT(*), SUM(f.m3Left), SUM(f.m3Total)
		  FROM rolebot_belt_field f
		  JOIN asteroidFields a ON a.solarSystemID = f.solarSystemID AND a.seed = f.seed
		  JOIN mapSolarSystems s ON s.solarSystemID = f.solarSystemID
		 WHERE f.m3Total > 0
		 GROUP BY s.regionID`)
	if err != nil {
		return nil, fmt.Errorf("belt fullness: %w", err)
	}
	defer rows.Close()

	var out []oreBeltRow
	for rows.Next() {
		var b oreBeltRow
		if err := rows.Scan(&b.RegionID, &b.Systems, &b.M3Left, &b.M3Total); err != nil {
			return nil, fmt.Errorf("belt fullness scan: %w", err)
		}
		if b.M3Total > 0 {
			b.Fullness = b.M3Left / b.M3Total
		}
		b.FullnessPc = int(b.Fullness * 100)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("belt fullness: %w", err)
	}
	names, err := m.regionNames(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if n := names[out[i].RegionID]; n != "" {
			out[i].Name = n
		} else {
			out[i].Name = "region " + strconv.FormatInt(out[i].RegionID, 10)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Fullness != out[j].Fullness {
			return out[i].Fullness < out[j].Fullness
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
