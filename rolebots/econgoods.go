package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
)

// The NPC market by goods: what the "Sold to an NPC bid" faucet and the "Bought from an NPC
// ask" sink are made of.
// A refType-2 journal row has no typeID, and the category split (rolebot_mkt_flow) cannot
// separate ore from ice, minerals from moon goo, or trade goods from tags. So the market pass
// keeps a ledger keyed by group (rolebot_mkt_group) and this page folds groups into goods
// classes in Go, so a class can be re-cut without re-reading the tape.
// It reads market_transactions, pruned by age only (db/walletretention.go), so its 30-day
// bootstrap is complete where the journal's is not. Corporation treasury orders are not on it.

// goodsClass is one bucket of the breakdown.
type goodsClass struct {
	Key, Label string
}

// goodsClasses is every class in display order; goodsClassOf only returns
// keys that are here.
var goodsClasses = []goodsClass{
	{"ore", "Ore"},
	{"ice", "Ice"},
	{"minerals", "Minerals"},
	{"ice_products", "Ice products"},
	{"moon", "Moon materials & reactions"},
	{"trade", "Trade goods"},
	{"loot_comm", "Tags & loot commodities"},
	{"commodity", "Other commodities"},
	{"modules", "Modules"},
	{"charges", "Ammo & charges"},
	{"drones", "Drones"},
	{"ships", "Ships"},
	{"blueprints", "Blueprints"},
	{"skills", "Skillbooks"},
	{"other", "Everything else"},
}

// goodsTradeGroups mirrors db.tradeGoodGroupList: the groups the NPC dynamics treat as trade
// goods.
var goodsTradeGroups = map[int64]bool{280: true, 281: true, 282: true, 283: true, 284: true, 313: true, 314: true}

// goodsLootGroups are the commodities that come out of a wreck: ship logs,
// criminal tags, insignias, overseer effects, artifacts.
var goodsLootGroups = map[int64]bool{369: true, 370: true, 409: true, 493: true, 528: true}

// goodsClassOf classifies an item group. Pure.
func goodsClassOf(groupID, categoryID int64) string {
	switch {
	case groupID == 465:
		return "ice"
	case categoryID == 25:
		return "ore"
	case groupID == 18:
		return "minerals"
	case groupID == 423:
		return "ice_products"
	case groupID == 427 || groupID == 428 || groupID == 429 || categoryID == 24:
		return "moon"
	case goodsTradeGroups[groupID]:
		return "trade"
	case goodsLootGroups[groupID]:
		return "loot_comm"
	case categoryID == 17 || categoryID == 4:
		return "commodity"
	case categoryID == 7:
		return "modules"
	case categoryID == 20:
		return "implants"
	case categoryID == 8:
		return "charges"
	case categoryID == 18:
		return "drones"
	case categoryID == 6:
		return "ships"
	case categoryID == 9:
		return "blueprints"
	case categoryID == 16:
		return "skills"
	}
	return "other"
}

// goodsCell is one (group, side) sum over the window, NPC counterparty only.
type goodsCell struct {
	GroupID, CategoryID int64
	GroupName           string
	Side                string // "sell" = into an NPC bid, "buy" = off an NPC ask
	ISK                 float64
	Units, N            int64
}

// goodsRow is one class on one side of the NPC market, dressed for the page.
type goodsRow struct {
	Key, Label string
	ISK        string
	ISKV       float64
	Units, N   int64
	Pct        string
	Bar        int
	// Groups names the biggest item groups inside the class, so a broad class says what it holds.
	Groups string
}

// econGoods is both sides of the breakdown.
type econGoods struct {
	Sold, Bought           []goodsRow
	SoldTotal, BoughtTotal string
	// Has is whether the group ledger holds anything for the window at all.
	Has bool
}

func (m *Mod) econGoodsCells(ctx context.Context, sinceHour int64) ([]goodsCell, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT f.groupID, COALESCE(g.categoryID, 0), COALESCE(g.groupName, CONCAT('group ', f.groupID)),
		       f.side, SUM(f.isk), SUM(f.units), SUM(f.n)
		  FROM rolebot_mkt_group f
		  LEFT JOIN invGroups g ON g.groupID = f.groupID
		 WHERE f.hour >= ? AND f.counter = 'npc'
		 GROUP BY f.groupID, g.categoryID, g.groupName, f.side`, sinceHour)
	if err != nil {
		return nil, fmt.Errorf("npc goods: %w", err)
	}
	defer rows.Close()
	var out []goodsCell
	for rows.Next() {
		var c goodsCell
		if err := rows.Scan(&c.GroupID, &c.CategoryID, &c.GroupName, &c.Side, &c.ISK, &c.Units, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// foldGoods turns the group cells into the two class tables. Pure.
func foldGoods(cells []goodsCell) econGoods {
	var g econGoods
	g.Sold, g.SoldTotal = foldGoodsSide(cells, "sell")
	g.Bought, g.BoughtTotal = foldGoodsSide(cells, "buy")
	g.Has = len(cells) > 0
	return g
}

func foldGoodsSide(cells []goodsCell, side string) ([]goodsRow, string) {
	type grp struct {
		name string
		isk  float64
	}
	type acc struct {
		isk      float64
		units, n int64
		groups   map[int64]*grp
	}
	by := map[string]*acc{}
	total := 0.0
	for _, c := range cells {
		if c.Side != side || c.ISK <= 0 {
			continue
		}
		k := goodsClassOf(c.GroupID, c.CategoryID)
		a := by[k]
		if a == nil {
			a = &acc{groups: map[int64]*grp{}}
			by[k] = a
		}
		a.isk += c.ISK
		a.units += c.Units
		a.n += c.N
		gr := a.groups[c.GroupID]
		if gr == nil {
			gr = &grp{name: c.GroupName}
			a.groups[c.GroupID] = gr
		}
		gr.isk += c.ISK
		total += c.ISK
	}
	out := make([]goodsRow, 0, len(by))
	for _, cl := range goodsClasses {
		a := by[cl.Key]
		if a == nil {
			continue
		}
		gs := make([]*grp, 0, len(a.groups))
		for _, gr := range a.groups {
			gs = append(gs, gr)
		}
		sort.Slice(gs, func(i, j int) bool {
			if gs[i].isk != gs[j].isk {
				return gs[i].isk > gs[j].isk
			}
			return gs[i].name < gs[j].name
		})
		var parts []string
		for i, gr := range gs {
			if i == 4 {
				parts = append(parts, fmt.Sprintf("+%d more", len(gs)-4))
				break
			}
			parts = append(parts, gr.name+" "+iskShort(gr.isk))
		}
		out = append(out, goodsRow{Key: cl.Key, Label: cl.Label, ISKV: a.isk, ISK: iskShort(a.isk),
			Units: a.units, N: a.n, Groups: strings.Join(parts, " · ")})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ISKV > out[j].ISKV })
	for i := range out {
		if total > 0 {
			out[i].Pct = fmt.Sprintf("%.1f%%", 100*out[i].ISKV/total)
		}
		if out[0].ISKV > 0 {
			out[i].Bar = int(math.Round(100 * out[i].ISKV / out[0].ISKV))
		}
	}
	return out, iskShort(total)
}
