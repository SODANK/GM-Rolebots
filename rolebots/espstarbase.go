package rolebots

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// The starbase pages: a card on each bot CEO's page (towers, fuel, seats,
// requisitions, the haul board, surveys, sieges) and the global
// /rolebots/starbases board. Both draw their towers through the shared
// rb_towertable template: one line per tower, a detail row that opens on click,
// and a filter bar.

// towerRow is one tower as the panel shows it.
type towerRow struct {
	PlanID   int64
	TowerID  int64
	CorpID   int64
	CorpName string
	Purpose  string
	// PurposeBase is the purpose label without the reactions shape, the filter's value.
	PurposeBase string
	Size        string
	Race        string
	Moon        string
	// MoonShort is the moon's name with the system's name taken off the front ("VII - Moon 3").
	MoonShort string
	System    string
	Region    string
	Security  string
	SecVal    float64
	// SecBand is "hi", "lo" or "null", the class that colours the security figure.
	SecBand    string
	Status     string
	State      string
	FuelDays   string
	FuelLow    bool
	FuelSort   int64 // batches held, -1 with no tower to fuel
	Strontium  string
	SrSort     int64 // hours held, -1 with none read
	Fueler     string
	Structures string
	Built      float64 // standing / planned, the Built column's sort key
	// Grid is the tower's real fitting: draw against its own grid plus its
	// boosters. The count above is progress (standing / planned), not a limit.
	Grid    string
	PnL     string
	PnLNeg  bool
	PnLISK  float64
	Staging string
	Note    string
	Siege   string
	// Age is how long the tower has stood in its current phase, "online 12d".
	Age string
	// Infrastructure's book (posinfra.go): the last threat reading, the
	// structures lost in the window, what it holds offline, and the battery
	// slots by compass point.
	Threat  int
	Lost    int
	Offline int
	Slots   string
	// A reactions tower's production line (posindustry.go): the reaction its
	// reactor runs and whether the reactor holds it, every silo's fill, and
	// the chain legs in and out of it.
	Reaction string
	Reactor  string
	Silos    []siloView
	Chain    []string
	// Flags are the row's warnings; Attention is any of them.
	Flags     []towerFlag
	Attention bool
	// Search is the lower-cased text the filter box matches.
	Search string
}

// towerFlag is one warning pill on a tower's line. Bad draws red, the rest amber.
type towerFlag struct {
	Text string
	Bad  bool
}

// siloView is one silo's gauge on the panel.
type siloView struct {
	Material string
	Used     int64
	Cap      int64
	Pct      int
	Role     string // "in", "out" or "mine"
}

type reqRow struct {
	What, Where, Requester, Status, Budget, Deadline string
}

type legViewRow struct {
	Kind, Dest, Carrier, Status, M3, Wage string
}

type siegeViewRow struct {
	System, Region, Attacker, Defender, Phase, Exit, Result string
}

// ceoStarbaseView is the CEO page's card.
type ceoStarbaseView struct {
	CorpID   int64
	Board    towerBoard
	Fuelers  []string
	Procurer string
	Reqs     []reqRow
	Hauls    []legViewRow
	Surveys  int
	Intel    int
	BestMoon string
	Sieges   []siegeViewRow
	Saving   string
	// Infra is the Infrastructure department: who heads it and its size.
	Infra string
}

// towerBoard is what rb_towertable draws: the rows in region, system, moon
// order and the filter bar's choices.
type towerBoard struct {
	ID       string // the table's DOM id; one page may hold more than one board
	ShowCorp bool   // the global board names each tower's corporation, a CEO card does not
	Bar      bool   // the filter bar; a CEO card with a handful of towers goes without
	Rows     []towerRow
	Regions  []nameCount
	Purposes []string
	Statuses []string
}

// nameCount is one choice in a filter list with its row count.
type nameCount struct {
	Name string
	N    int
}

// towerBoardBarFrom is the row count at which a CEO card shows the filter bar.
const towerBoardBarFrom = 8

// newTowerBoard sorts the rows by region, system and moon and gathers the
// filter choices. Pure.
func newTowerBoard(id string, showCorp bool, rows []towerRow) towerBoard {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Region != b.Region {
			return lessFold(a.Region, b.Region)
		}
		if a.System != b.System {
			return lessFold(a.System, b.System)
		}
		return lessFold(a.Moon, b.Moon)
	})
	b := towerBoard{ID: id, ShowCorp: showCorp, Bar: showCorp || len(rows) >= towerBoardBarFrom, Rows: rows}
	regions := map[string]int{}
	purposes, statuses := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		regions[r.Region]++
		purposes[r.PurposeBase] = true
		statuses[r.Status] = true
	}
	for name, n := range regions {
		b.Regions = append(b.Regions, nameCount{name, n})
	}
	sort.Slice(b.Regions, func(i, j int) bool { return lessFold(b.Regions[i].Name, b.Regions[j].Name) })
	for p := range purposes {
		b.Purposes = append(b.Purposes, p)
	}
	sort.Strings(b.Purposes)
	// Plan states in the order a tower walks them.
	for _, s := range []string{towerPlanned, towerStaging, towerAnchored, towerRigging, towerRunning, towerUnanchor} {
		if statuses[s] {
			b.Statuses = append(b.Statuses, s)
		}
	}
	return b
}

// towerStats is the global board's headline strip. Pure given the rows.
type towerStats struct {
	Total, Running, Building, Attention, Sieged, Regions, Corps int
	PnL                                                         string
	PnLNeg                                                      bool
}

func towerStatsOf(rows []towerRow) towerStats {
	var s towerStats
	regions, corps := map[string]bool{}, map[int64]bool{}
	var pnl float64
	for _, r := range rows {
		s.Total++
		switch r.Status {
		case towerRunning:
			s.Running++
		case towerPlanned, towerStaging, towerAnchored, towerRigging:
			s.Building++
		}
		if r.Attention {
			s.Attention++
		}
		if r.Siege != "" {
			s.Sieged++
		}
		regions[r.Region], corps[r.CorpID] = true, true
		pnl += r.PnLISK
	}
	s.Regions, s.Corps = len(regions), len(corps)
	s.PnL, s.PnLNeg = iskString(pnl), pnl < 0
	return s
}

func (m *Mod) towerRowFor(ctx context.Context, p *towerPlan) towerRow {
	r := towerRow{PlanID: p.PlanID, TowerID: p.TowerID, CorpID: p.CorpID, CorpName: corpNameOf(ctx, m.db, p.CorpID),
		Purpose: purposeLabel[p.Purpose], Size: sizeLabel(p.Size), Moon: moonNameOf(ctx, m, p.MoonID),
		System: systemNameOf(ctx, m, p.SystemID), Status: p.Status, Note: p.Note, FuelSort: -1, SrSort: -1}
	r.PurposeBase = r.Purpose
	r.MoonShort = strings.TrimPrefix(r.Moon, r.System+" ")
	if reg := m.systemRegion(ctx, p.SystemID); reg != 0 {
		r.Region = regionNameOf(ctx, m, reg)
	}
	// The last reconsider verdict (posreconsider.go): whether the tower still
	// serves the corporation, was refitted, or is coming down.
	if v := p.Layout.ReconsiderNote; v != "" {
		if r.Note != "" {
			r.Note += " · "
		}
		r.Note += "reconsidered " + v
	}
	if race, _ := towerRaceOf(p.TowerTypeID); race != 0 {
		r.Race = raceLabel(race)
	}
	if sec, ok, _ := evedb.GetSystemSecurity(ctx, m.db, p.SystemID); ok {
		r.SecVal = dispSec(sec)
		r.Security = fmt.Sprintf("%.1f", r.SecVal)
		r.SecBand = secClass(r.SecVal)
	}
	if p.FuelerID != 0 {
		r.Fueler = m.charNameOr(ctx, p.FuelerID)
	}
	if p.StagingStationID != 0 {
		r.Staging = m.stationLabel(ctx, p.StagingStationID)
	}
	up := 0
	for _, pc := range p.Layout.Pieces {
		if pc.ItemID != 0 {
			up++
		}
	}
	r.Structures = fmt.Sprintf("%d / %d", up, len(p.Layout.Pieces))
	if len(p.Layout.Pieces) > 0 {
		r.Built = float64(up) / float64(len(p.Layout.Pieces))
	}
	r.Grid = gridLine(p)
	r.Threat, r.Lost = p.Layout.Infra.Threat, p.Layout.Infra.recentLosses(clock.Unix())
	for _, pc := range p.Layout.Pieces {
		if pc.Off {
			r.Offline++
		}
	}
	r.Slots = slotSummary(p.Layout.Pieces)
	r.Age = towerAge(p, clock.Unix())
	if p.Purpose == purposeReactions {
		m.fillReactionRow(ctx, p, &r)
	}
	reinforced := false
	if p.TowerID != 0 {
		if rec, ok, _ := evedb.GetStarbase(ctx, m.db, p.TowerID); ok {
			r.State = rec.State
			if rec.State == evedb.StarbaseStateReinforced {
				reinforced = true
				r.State += " until " + fmtDuration(max(0, evedb.Win32ToUnix(int64(rec.ReinforceUntil))-clock.Unix()))
			}
		}
		if batch, err := m.towerFuelBatch(ctx, p.TowerID, p.TowerTypeID, p.SystemID, p.CorpID, 0); err == nil && len(batch) > 0 {
			held := batchesHeld(batch, m.bayHeld(ctx, p.TowerID))
			r.FuelDays = fmt.Sprintf("%d d", held)
			r.FuelSort = held
			r.FuelLow = held <= fuelUrgentBatches
		}
		if hrs, ok, _ := evedb.StarbaseReinforceHoursHeld(ctx, m.db, evedb.StarbaseFuelRow{EntityID: p.TowerID,
			TypeID: p.TowerTypeID, SolarSystemID: p.SystemID, OwnerCorpID: p.CorpID}); ok {
			r.Strontium = fmt.Sprintf("%d h", hrs)
			r.SrSort = int64(hrs)
		}
	}
	net, _ := m.towerPnL(ctx, p.PlanID, clock.Unix()-7*86400)
	r.PnL, r.PnLNeg, r.PnLISK = iskString(net), net < 0, net
	// A muster has no ships at the tower yet: listed in the siege tables, not flagged on the tower.
	for _, s := range m.openSieges(ctx, `towerID = ? AND phase <> 'muster'`, p.TowerID) {
		r.Siege = s.Stage
		if s.ExitAt > 0 {
			r.Siege += ", exit in " + fmtDuration(max(0, s.ExitAt-clock.Unix()))
		}
	}
	r.Flags = towerFlags(r, reinforced)
	r.Attention = len(r.Flags) > 0
	r.Search = strings.ToLower(strings.Join([]string{r.CorpName, r.Region, r.System, r.Moon, r.Purpose,
		r.Size, r.Race, r.Fueler, r.Reaction}, " "))
	return r
}

// towerFlags is a row's warnings, worst first. Pure.
func towerFlags(r towerRow, reinforced bool) []towerFlag {
	var out []towerFlag
	if r.Siege != "" {
		out = append(out, towerFlag{"siege", true})
	}
	if reinforced {
		out = append(out, towerFlag{"reinforced", true})
	}
	if r.FuelLow {
		out = append(out, towerFlag{"fuel low", true})
	}
	if r.Lost > 0 {
		out = append(out, towerFlag{fmt.Sprintf("%d lost", r.Lost), true})
	}
	if r.Offline > 0 {
		out = append(out, towerFlag{fmt.Sprintf("%d offline", r.Offline), false})
	}
	if r.Reactor != "" && r.Reactor != "loaded" {
		out = append(out, towerFlag{"reactor", false})
	}
	return out
}

// secClass classes a displayed security figure: high above 0.45, low above 0, null at or below. Pure.
func secClass(sec float64) string {
	switch {
	case sec >= 0.45:
		return "hi"
	case sec > 0:
		return "lo"
	}
	return "null"
}

// towerAge is how long a plan has stood in its phase, "online 12d" or "planned 3h". Pure given now.
func towerAge(p *towerPlan, now int64) string {
	at, verb := p.PlannedAt, "planned"
	switch p.Status {
	case towerRunning, towerRigging:
		at, verb = p.OnlineAt, "online"
	case towerAnchored:
		at, verb = p.AnchoredAt, "anchored"
	case towerUnanchor:
		return "tearing down"
	}
	if at <= 0 {
		return ""
	}
	return verb + " " + shortAge(now-at)
}

// shortAge is a span in its largest unit, "12d", "5h" or "40m". Pure.
func shortAge(sec int64) string {
	switch {
	case sec >= 86400:
		return fmt.Sprintf("%dd", sec/86400)
	case sec >= 3600:
		return fmt.Sprintf("%dh", sec/3600)
	}
	return fmt.Sprintf("%dm", max(0, sec)/60)
}

// fillReactionRow reads a reactions tower's production line for the panel:
// the shape (miner+reactor or hub), the reaction and the reactor's state,
// each standing silo's fill, and the chain legs through it.
func (m *Mod) fillReactionRow(ctx context.Context, p *towerPlan, r *towerRow) {
	if planHarvests(p) {
		r.Purpose += " (miner+reactor)"
	} else {
		r.Purpose += " (hub)"
	}
	var reaction int64
	ins, outs := map[int64]bool{}, map[int64]bool{}
	for _, pc := range p.Layout.Pieces {
		if !isReactorType(pc.TypeID) || pc.Provision == 0 {
			continue
		}
		reaction = pc.Provision
		r.Reaction = m.typeNameOr(ctx, reaction)
		if in, out, err := evedb.ReactionRecipe(ctx, m.db, reaction); err == nil {
			for _, l := range in {
				ins[l.TypeID] = true
			}
			for _, l := range out {
				outs[l.TypeID] = true
			}
		}
		switch {
		case pc.ItemID == 0:
			r.Reactor = "not yet anchored"
		default:
			_, held, ok, err := evedb.ReactorReaction(ctx, m.db, pc.ItemID)
			switch {
			case err != nil:
				r.Reactor = "unreadable"
			case !ok:
				r.Reactor = "empty — reaction on order"
			case held != reaction:
				r.Reactor = "holds " + m.typeNameOr(ctx, held) + " instead"
			default:
				r.Reactor = "loaded"
			}
			if accepts, err := evedb.ReactorAccepts(ctx, m.db, pc.TypeID, reaction); err == nil && !accepts {
				r.Reactor += " (this reactor cannot run it)"
			}
		}
	}
	harvested := map[int64]bool{}
	for _, pc := range p.Layout.Pieces {
		if pc.TypeID == typeHarvester && pc.Provision != 0 {
			harvested[pc.Provision] = true
		}
	}
	for _, pc := range p.Layout.Pieces {
		if pc.TypeID != typeSilo || pc.ItemID == 0 || pc.Provision == 0 {
			continue
		}
		v := siloView{Material: m.typeNameOr(ctx, pc.Provision)}
		switch {
		case outs[pc.Provision]:
			v.Role = "out"
		case harvested[pc.Provision]:
			v.Role = "mine"
		case ins[pc.Provision]:
			v.Role = "in"
		}
		if used, capacity, err := evedb.SiloUnits(ctx, m.db, pc.ItemID, typeSilo, pc.Provision); err == nil && capacity > 0 {
			v.Used, v.Cap, v.Pct = used, capacity, int(100*used/capacity)
		}
		r.Silos = append(r.Silos, v)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT fromPlanID, toPlanID, typeID FROM rolebot_corp_chain WHERE fromPlanID = ? OR toPlanID = ?`, p.PlanID, p.PlanID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var from, to, typeID int64
		if rows.Scan(&from, &to, &typeID) != nil {
			continue
		}
		other, arrow := to, "→ %s to %s"
		if to == p.PlanID {
			other, arrow = from, "← %s from %s"
		}
		where := fmt.Sprintf("plan %d", other)
		if op, _ := m.loadTowerPlan(ctx, other); op != nil {
			where = moonNameOf(ctx, m, op.MoonID)
		}
		r.Chain = append(r.Chain, fmt.Sprintf(arrow, m.typeNameOr(ctx, typeID), where))
	}
}

// gridLine is a plan's draw against its grid, "power 1.2M / 2.8M MW · cpu
// 3,100 / 7,500 tf", with the number of grid boosters in it. Pure given the plan.
func gridLine(p *towerPlan) string {
	race, _ := towerRaceOf(p.TowerTypeID)
	g, u := boostedGrid(race, p.Size, p.Layout.Pieces), gridUse(p.Layout.Pieces)
	boosters := 0
	for _, pc := range p.Layout.Pieces {
		if isBoosterType(pc.TypeID) && !pc.Off && !pc.Strip {
			boosters++
		}
	}
	line := fmt.Sprintf("power %s / %s MW · cpu %.0f / %.0f tf", megaString(u.Power), megaString(g.Power), u.CPU, g.CPU)
	if boosters > 0 {
		line += fmt.Sprintf(" · %d booster(s)", boosters)
	}
	return line
}

// megaString prints a power figure in millions ("1.25M"), or whole below that.
func megaString(v float64) string {
	if v >= 1e6 {
		return fmt.Sprintf("%.2fM", v/1e6)
	}
	return fmt.Sprintf("%.0f", v)
}

// slotSummary is a tower's batteries by compass point, "N 2 · E 1 · S 1 ·
// W 1", with any battery placed before slots counted apart. Pure.
func slotSummary(pieces []layoutPiece) string {
	var n [4]int
	legacy := 0
	for _, pc := range pieces {
		if !isDefenseType(pc.TypeID) {
			continue
		}
		if pc.Slot < 1 {
			legacy++
			continue
		}
		n[(pc.Slot-1)%4]++
	}
	var parts []string
	for i, c := range n {
		if c > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", compassNames[i], c))
		}
	}
	if legacy > 0 {
		parts = append(parts, fmt.Sprintf("%d on the old ring", legacy))
	}
	return strings.Join(parts, " · ")
}

func (m *Mod) ceoStarbaseView(ctx context.Context, corpID int64) *ceoStarbaseView {
	plans, err := m.corpTowerPlans(ctx, corpID, true)
	if err != nil {
		return nil
	}
	v := &ceoStarbaseView{CorpID: corpID}
	var rows []towerRow
	for _, p := range plans {
		rows = append(rows, m.towerRowFor(ctx, p))
	}
	v.Board = newTowerBoard("sbx-corp", false, rows)
	for _, id := range m.seatHolders(ctx, corpID, seatFueler) {
		v.Fuelers = append(v.Fuelers, m.charNameOr(ctx, id))
	}
	if ps := m.seatHolders(ctx, corpID, seatProcurer); len(ps) > 0 {
		v.Procurer = m.charNameOr(ctx, ps[0])
	}
	v.Infra = "not formed; the CEO runs the towers"
	if c, _ := getCEO(ctx, m.db, corpID); c != nil {
		if h := m.staffFor(ctx, c).head(portInfrastructure); h != nil {
			v.Infra = fmt.Sprintf("%s heads it, %d member(s)", h.Name, armCount(corpID, armInfrastructure))
		} else if n := armCount(corpID, armInfrastructure); n > 0 {
			v.Infra = fmt.Sprintf("%d member(s), no Director yet; the CEO runs the towers", n)
		}
	}
	for _, r := range m.openReqSummary(ctx, corpID) {
		if r.Requester == reqTrade {
			continue // the Trade desk's card shows its own runs (espceo_trade.go)
		}
		v.Reqs = append(v.Reqs, reqRow{
			What:      fmt.Sprintf("%d× %s", r.left(), m.typeNameOr(ctx, r.TypeID)),
			Where:     m.stationLabel(ctx, r.DestStationID),
			Requester: r.Requester, Status: r.Status, Budget: iskString(r.Budget - r.Spent),
			Deadline: fmtDuration(max(0, r.Deadline-clock.Unix())),
		})
	}
	if legs, err := m.queryHauls(ctx, `WHERE corporationID = ? AND status IN ('open','claimed') ORDER BY deadline LIMIT 25`, corpID); err == nil {
		for _, l := range legs {
			h := legViewRow{Kind: l.Kind, Dest: m.haulDestLabel(ctx, l), Status: l.Status,
				M3: fmt.Sprintf("%.0f", l.M3), Wage: iskString(l.Wage)}
			if l.CarrierID != 0 {
				h.Carrier = m.charNameOr(ctx, l.CarrierID)
			}
			v.Hauls = append(v.Hauls, h)
		}
	}
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_corp_survey WHERE corporationID = ? AND status IN ('open','claimed')`,
		corpID).Scan(&v.Surveys)
	if intel, err := m.corpMoonIntel(ctx, corpID, moonR4); err == nil {
		v.Intel = len(intel)
		if len(intel) > 0 {
			v.BestMoon = fmt.Sprintf("%s (%s, %s/h)", moonNameOf(ctx, m, intel[0].MoonID),
				evedb.MoonClassNames[max(0, intel[0].BestClass)], iskString(intel[0].Value))
		}
	}
	for _, s := range m.openSieges(ctx, `(attackerCorpID = ? OR defenderCorpID = ?)`, corpID, corpID) {
		v.Sieges = append(v.Sieges, m.siegeViewRowOf(ctx, s))
	}
	var saving float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount - spent), 0) FROM rolebot_corp_commit
		 WHERE corporationID = ? AND refKind = ? AND closedAt = 0`, corpID, towerSaveCommit).Scan(&saving)
	if saving > 0 {
		v.Saving = iskString(saving)
	}
	return v
}

func (m *Mod) siegeViewRowOf(ctx context.Context, s *siegeRow) siegeViewRow {
	row := siegeViewRow{System: systemNameOf(ctx, m, s.SystemID), Phase: s.Stage, Result: s.Result,
		Defender: corpNameOf(ctx, m.db, s.DefenderCorpID)}
	if s.isStation() {
		row.System += " — " + stationNameOf(ctx, m, s.TowerID)
	}
	if reg := m.systemRegion(ctx, s.SystemID); reg != 0 {
		row.Region = regionNameOf(ctx, m, reg)
	}
	if s.AttackerCorpID != 0 {
		row.Attacker = corpNameOf(ctx, m.db, s.AttackerCorpID)
	} else {
		row.Attacker = "unknown"
	}
	if s.ExitAt > 0 {
		row.Exit = fmtDuration(max(0, s.ExitAt-clock.Unix()))
	}
	return row
}

const tmplCEOStarbase = `
{{with .Starbase}}
<div class="card">
  <h3>Starbases</h3>
  <p style="font-size:12px">Fuel Technicians: {{if .Fuelers}}{{range $i, $f := .Fuelers}}{{if $i}}, {{end}}{{$f}}{{end}}{{else}}none{{end}}
   &middot; Procurement Officer: {{if .Procurer}}{{.Procurer}}{{else}}the CEO{{end}}
   &middot; Infrastructure: {{.Infra}}
   &middot; moon intel: {{.Intel}} worth a harvester{{if .BestMoon}} (best {{.BestMoon}}){{end}}
   &middot; open surveys: {{.Surveys}}{{if .Saving}} &middot; saving <b>{{.Saving}}</b> for the next tower{{end}}</p>
  <form method="POST" action="/rolebots/starbases" style="margin:4px 0 8px">
    <input type="hidden" name="action" value="consider"><input type="hidden" name="corp" value="{{.CorpID}}">
    <input type="hidden" name="back" value="/rolebots/corp/{{.CorpID}}">
    <button type="submit">Consider a tower now</button>
    <span style="font-size:11px;color:#6b7280">runs the CEO's tower decision now</span>
  </form>
  {{if .Board.Rows}}{{template "rb_towertable" .Board}}{{else}}<p>No towers planned or standing.</p>{{end}}
  {{if .Sieges}}<h4>Sieges</h4>
  <table class="tbl" style="font-size:12px"><tr><th>Region</th><th>System</th><th>Attacker</th><th>Defender</th><th>Phase</th><th>Exit</th></tr>
    {{range .Sieges}}<tr><td>{{.Region}}</td><td>{{.System}}</td><td>{{.Attacker}}</td><td>{{.Defender}}</td><td>{{.Phase}}</td><td>{{.Exit}}</td></tr>{{end}}
  </table>{{end}}
  {{if .Reqs}}<h4>Requisitions</h4>
  <table class="tbl" style="font-size:12px"><tr><th>What</th><th>To</th><th>For</th><th>Status</th><th>Budget left</th><th>Due in</th></tr>
    {{range .Reqs}}<tr><td>{{.What}}</td><td>{{.Where}}</td><td>{{.Requester}}</td><td>{{.Status}}</td><td>{{.Budget}}</td><td>{{.Deadline}}</td></tr>{{end}}
  </table>{{end}}
  {{if .Hauls}}<h4>Haul board</h4>
  <table class="tbl" style="font-size:12px"><tr><th>Leg</th><th>To</th><th>m³</th><th>Pays</th><th>Carrier</th><th>Status</th></tr>
    {{range .Hauls}}<tr><td>{{.Kind}}</td><td>{{.Dest}}</td><td>{{.M3}}</td><td>{{.Wage}}</td><td>{{.Carrier}}</td><td>{{.Status}}</td></tr>{{end}}
  </table>{{end}}
</div>
{{end}}
`

// tmplTowerTable is the shared tower list, appended at top level to every page
// that draws one (registerESP). Each tower is its own <tbody>: the line and its
// detail row move together when a header sorts the table. Filtering and sorting
// run in the page; the server sends every row in region, system, moon order.
const tmplTowerTable = `
{{define "rb_towertable"}}
<style>
.sbx-bar { display:flex; flex-wrap:wrap; gap:8px; align-items:center; margin:4px 0 8px; font-size:12px; }
.sbx-bar input[type="text"] { padding:5px 8px; font-size:12px; width:260px; }
.sbx-bar select { padding:4px 6px; font-size:12px; }
.sbx-bar .sbx-count { color:#6b7280; margin-left:auto; }
table.sbx th[data-col] { cursor:pointer; user-select:none; }
table.sbx th[data-col]:hover { color:#e0e4ec; }
table.sbx th.asc::after { content:' \25B2'; color:var(--accent); font-size:9px; }
table.sbx th.desc::after { content:' \25BC'; color:var(--accent); font-size:9px; }
table.sbx th.num, table.sbx td.num { text-align:right; }
table.sbx tr.sbx-main { cursor:pointer; }
table.sbx tr.sbx-main td:first-child { width:12px; color:#6b7280; }
table.sbx tbody.open tr.sbx-main td:first-child { color:var(--accent); }
table.sbx tr.sbx-detail { display:none; }
table.sbx tbody.open tr.sbx-detail { display:table-row; }
table.sbx tr.sbx-detail td { white-space:normal; background:var(--win-deep); padding:8px 14px 10px 30px; }
table.sbx .sec-hi { color:#6ee7b7; } table.sbx .sec-lo { color:#eab308; } table.sbx .sec-null { color:#ef4444; }
table.sbx .bad { color:#ef4444; }
.sbx-pill { display:inline-block; padding:0 6px; border-radius:8px; font-size:10px; line-height:16px; border:1px solid var(--line); color:#9ca3af; }
.sbx-pill.st-running { color:#6ee7b7; border-color:#1a4731; }
.sbx-pill.st-rigging, .sbx-pill.st-anchored { color:#93c5fd; border-color:#1e3a5f; }
.sbx-pill.st-unanchor { color:#eab308; border-color:#4a3b0f; }
.sbx-pill.warn { color:#eab308; border-color:#4a3b0f; }
.sbx-pill.bad { color:#fca5a5; border-color:#471a1a; background:#2a0f0f; }
.sbx-dl { display:grid; grid-template-columns:max-content 1fr; gap:3px 14px; font-size:12px; }
.sbx-dl dt { color:#6b7280; }
.sbx-dl dd { margin:0; }
.sbx-meter { display:inline-block; width:60px; height:6px; background:var(--line-soft); border-radius:3px; vertical-align:middle; margin:0 4px; }
.sbx-meter > i { display:block; height:100%; background:var(--accent); border-radius:3px; }
</style>
{{if .Bar}}<div class="sbx-bar" data-for="{{.ID}}">
  <input type="text" data-f="q" placeholder="filter: corporation, region, system, moon, fueler" autocomplete="off">
  <select data-f="region"><option value="">every region</option>{{range .Regions}}<option value="{{.Name}}">{{.Name}} ({{.N}})</option>{{end}}</select>
  <select data-f="purpose"><option value="">every purpose</option>{{range .Purposes}}<option>{{.}}</option>{{end}}</select>
  <select data-f="status"><option value="">every plan state</option>{{range .Statuses}}<option>{{.}}</option>{{end}}</select>
  <label><input type="checkbox" data-f="att"> needs attention</label>
  <button type="button" data-f="expand" style="padding:3px 8px;font-size:11px">expand all</button>
  <span class="sbx-count"></span>
</div>{{end}}
<table class="rows sbx" id="{{.ID}}">
<thead><tr><th></th>
  {{if .ShowCorp}}<th data-col="corp">Corporation</th>{{end}}
  <th data-col="region">Region</th><th data-col="system">System</th><th data-col="sec" class="num">Sec</th>
  <th data-col="moon">Moon</th><th data-col="tower">Tower</th><th data-col="plan">Plan</th><th data-col="state">State</th>
  <th data-col="fuel" class="num">Fuel</th><th data-col="sr" class="num">Sr</th>
  <th data-col="built" class="num" title="structures standing / planned">Built</th>
  <th data-col="pnl" class="num">P&amp;L 7d</th><th>Flags</th></tr></thead>
{{range .Rows}}<tbody data-q="{{.Search}}" data-region="{{.Region}}" data-purpose="{{.PurposeBase}}" data-status="{{.Status}}"{{if .Attention}} data-att="1"{{end}}
  data-k-corp="{{.CorpName}}" data-k-region="{{.Region}}" data-k-system="{{.System}}" data-k-sec="{{.SecVal}}" data-k-moon="{{.Moon}}"
  data-k-tower="{{.Purpose}} {{.Size}}" data-k-plan="{{.Status}}" data-k-state="{{.State}}" data-k-fuel="{{if ge .FuelSort 0}}{{.FuelSort}}{{end}}"
  data-k-sr="{{if ge .SrSort 0}}{{.SrSort}}{{end}}" data-k-built="{{.Built}}" data-k-pnl="{{.PnLISK}}">
<tr class="sbx-main"><td>&#9656;</td>
  {{if $.ShowCorp}}<td><a href="/rolebots/corp/{{.CorpID}}"><span class="clip">{{.CorpName}}</span></a></td>{{end}}
  <td>{{.Region}}</td><td>{{.System}}</td>
  <td class="num">{{if .Security}}<span class="sec-{{.SecBand}}">{{.Security}}</span>{{end}}</td>
  <td><span class="clip" title="{{.Moon}}">{{.MoonShort}}</span></td>
  <td>{{.Purpose}} <span class="sub">{{.Size}} {{.Race}}</span></td>
  <td><span class="sbx-pill st-{{.Status}}">{{.Status}}</span></td>
  <td{{if .Siege}} class="bad"{{end}}>{{.State}}</td>
  <td class="num{{if .FuelLow}} bad{{end}}">{{if .FuelDays}}{{.FuelDays}}{{else}}<span class="sub">&mdash;</span>{{end}}</td>
  <td class="num">{{if .Strontium}}{{.Strontium}}{{else}}<span class="sub">&mdash;</span>{{end}}</td>
  <td class="num">{{.Structures}}</td>
  <td class="num{{if .PnLNeg}} bad{{end}}">{{.PnL}}</td>
  <td>{{range .Flags}}<span class="sbx-pill {{if .Bad}}bad{{else}}warn{{end}}">{{.Text}}</span> {{end}}</td></tr>
<tr class="sbx-detail"><td colspan="{{if $.ShowCorp}}14{{else}}13{{end}}"><dl class="sbx-dl">
  <dt>Moon</dt><dd>{{.Moon}}{{if .Age}} &middot; {{.Age}}{{end}} &middot; plan {{.PlanID}}{{if .TowerID}}, tower {{.TowerID}}{{end}}</dd>
  {{if .Siege}}<dt>Siege</dt><dd class="bad">{{.Siege}}</dd>{{end}}
  <dt>Fitting</dt><dd>{{.Structures}} structures standing{{if .Grid}} &middot; {{.Grid}}{{end}}</dd>
  {{if .Slots}}<dt>Guns</dt><dd>{{.Slots}}</dd>{{end}}
  {{if or .Threat .Lost .Offline}}<dt>Defence</dt><dd>threat {{.Threat}}{{if .Lost}} &middot; <span class="bad">{{.Lost}} lost</span>{{end}}{{if .Offline}} &middot; {{.Offline}} offline{{end}}</dd>{{end}}
  <dt>Logistics</dt><dd>fueler {{if .Fueler}}{{.Fueler}}{{else}}none{{end}}{{if .Staging}} &middot; staged at {{.Staging}}{{end}}</dd>
  {{if .Reaction}}<dt>Reaction</dt><dd><b>{{.Reaction}}</b> &middot; reactor {{.Reactor}}</dd>{{end}}
  {{if .Silos}}<dt>Silos</dt><dd>{{range .Silos}}<span style="white-space:nowrap;margin-right:12px">{{.Material}} <span class="sbx-meter"><i style="width:{{.Pct}}%"></i></span>{{.Used}}/{{.Cap}} <span class="sub">({{.Role}})</span></span>{{end}}</dd>{{end}}
  {{if .Chain}}<dt>Chain</dt><dd>{{range $i, $c := .Chain}}{{if $i}} &middot; {{end}}{{$c}}{{end}}</dd>{{end}}
  {{if .Note}}<dt>Note</dt><dd>{{.Note}}</dd>{{end}}
</dl></td></tr>
</tbody>{{end}}
</table>
<script>
(function (id) {
  var table = document.getElementById(id);
  if (!table) return;
  var bar = document.querySelector('.sbx-bar[data-for="' + id + '"]');
  var bodies = Array.prototype.slice.call(table.tBodies);
  function f(name) { return bar ? bar.querySelector('[data-f="' + name + '"]') : null; }
  function apply() {
    if (!bar) return;
    var q = f('q').value.trim().toLowerCase(), reg = f('region').value, pur = f('purpose').value,
        st = f('status').value, att = f('att').checked, shown = 0;
    bodies.forEach(function (b) {
      var ok = (!q || b.dataset.q.indexOf(q) >= 0) && (!reg || b.dataset.region === reg) &&
        (!pur || b.dataset.purpose === pur) && (!st || b.dataset.status === st) && (!att || b.dataset.att);
      b.style.display = ok ? '' : 'none';
      if (ok) shown++;
    });
    bar.querySelector('.sbx-count').textContent = shown + ' of ' + bodies.length + ' tower(s)';
  }
  if (bar) {
    ['q', 'region', 'purpose', 'status', 'att'].forEach(function (n) {
      f(n).addEventListener(n === 'q' ? 'input' : 'change', apply);
    });
    f('expand').addEventListener('click', function () {
      var open = this.textContent === 'expand all';
      bodies.forEach(function (b) { b.classList.toggle('open', open); });
      this.textContent = open ? 'collapse all' : 'expand all';
    });
    apply();
  }
  table.addEventListener('click', function (e) {
    if (e.target.closest('a')) return;
    var tr = e.target.closest('tr.sbx-main');
    if (tr) tr.parentNode.classList.toggle('open');
  });
  var heads = table.tHead.querySelectorAll('th[data-col]');
  Array.prototype.forEach.call(heads, function (th) {
    th.addEventListener('click', function () {
      var key = 'k' + th.dataset.col.charAt(0).toUpperCase() + th.dataset.col.slice(1);
      var num = th.classList.contains('num');
      var dir = th.classList.contains('asc') ? -1 : 1;
      if (!th.classList.contains('asc') && !th.classList.contains('desc') && num) dir = -1;
      Array.prototype.forEach.call(heads, function (h) { h.classList.remove('asc', 'desc'); });
      th.classList.add(dir === 1 ? 'asc' : 'desc');
      bodies.sort(function (a, b) {
        var x = a.dataset[key] || '', y = b.dataset[key] || '';
        if (x === '' || y === '') return (x === '') - (y === ''); // no reading sorts last either way
        var c = num ? (parseFloat(x) || 0) - (parseFloat(y) || 0) : x.localeCompare(y, undefined, {sensitivity: 'base'});
        return c * dir;
      });
      bodies.forEach(function (b) { table.appendChild(b); });
    });
  });
})({{.ID}});
</script>
{{end}}
`

// ── the global board ────────────────────────────────────────────────────────

// starbaseBoardLimit caps the towers the global board reads in one page load.
const starbaseBoardLimit = 1000

func (m *Mod) handleStarbaseBoard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	self := "/rolebots/starbases"
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		switch r.FormValue("action") {
		case "power":
			if err := m.saveStarbasePower(ctx, r.FormValue("on") == "1"); err != nil {
				redirect(w, r, self, "", err.Error())
				return
			}
			redirect(w, r, self, "saved", "")
			return
		case "consider":
			// Back to the page the button sat on: this board, or the corporation's.
			back := self
			if b := r.FormValue("back"); strings.HasPrefix(b, "/rolebots/corp/") {
				back = b
			}
			corpID, _ := strconv.ParseInt(r.FormValue("corp"), 10, 64)
			if corpID == 0 {
				redirect(w, r, back, "", "pick a corporation")
				return
			}
			out, err := m.considerTowerNow(ctx, corpID)
			name := corpNameOf(ctx, m.db, corpID)
			if err != nil {
				redirect(w, r, back, "", name+": "+err.Error())
				return
			}
			redirect(w, r, back, name+": "+out, "")
			return
		}
		redirect(w, r, self, "", "unknown action")
		return
	}
	var towers []towerRow
	truncated := false
	rows, err := m.db.QueryContext(ctx, `SELECT `+towerCols+` FROM rolebot_corp_towers WHERE status <> 'gone' ORDER BY corporationID, planID LIMIT ?`,
		starbaseBoardLimit+1)
	if err == nil {
		var plans []*towerPlan
		for rows.Next() {
			if p, err := scanTowerPlan(rows); err == nil {
				plans = append(plans, p)
			}
		}
		rows.Close()
		if len(plans) > starbaseBoardLimit {
			plans, truncated = plans[:starbaseBoardLimit], true
		}
		for _, p := range plans {
			towers = append(towers, m.towerRowFor(ctx, p))
		}
	}
	var sieges []siegeViewRow
	for _, s := range m.openSieges(ctx, `1 = 1 ORDER BY openedAt DESC LIMIT 50`) {
		sieges = append(sieges, m.siegeViewRowOf(ctx, s))
	}
	var lost, gone int
	_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(losses), 0), COUNT(*) FROM rolebot_corp_towers WHERE status = 'gone'`).Scan(&lost, &gone)
	// The consider-now picker: every bot-run corporation, richest in free capital first. Free is the
	// wallet less open commitments; the charter's rent reserve is left out, so the figure reads a
	// touch high. The decision itself uses the exact one.
	var picks []considerPick
	if rows, err := m.db.QueryContext(ctx, `
		SELECT c.corporationID, c.corporationName, c.balance,
		       COALESCE((SELECT SUM(GREATEST(k.amount - k.spent, 0)) FROM rolebot_corp_commit k
		                  WHERE k.corporationID = c.corporationID AND k.closedAt = 0), 0),
		       (SELECT COUNT(*) FROM rolebot_corp_towers t WHERE t.corporationID = c.corporationID AND t.status <> 'gone')
		  FROM rolebot_ceos e JOIN corporation c ON c.corporationID = e.corporationID
		 WHERE e.paused = 0`); err == nil {
		for rows.Next() {
			var p considerPick
			var bal, owed float64
			if rows.Scan(&p.CorpID, &p.Name, &bal, &owed, &p.Towers) == nil {
				p.FreeISK = math.Max(0, bal-owed)
				p.Free = iskString(p.FreeISK)
				p.Ready = towerCapitalReady(p.FreeISK)
				picks = append(picks, p)
			}
		}
		rows.Close()
	}
	sort.Slice(picks, func(i, j int) bool { return picks[i].FreeISK > picks[j].FreeISK })
	espRender(w, "rolebots_starbases.html", map[string]any{
		"Page":      "rolebot-starbases",
		"Msg":       r.URL.Query().Get("msg"),
		"Err":       r.URL.Query().Get("err"),
		"Power":     StarbasePower(),
		"IceDemand": fmt.Sprintf("%.2f", iceDemandIndex()),
		"Stats":     towerStatsOf(towers),
		"Board":     newTowerBoard("sbx-all", true, towers),
		"Truncated": truncated,
		"Limit":     starbaseBoardLimit,
		"SiegeRows": sieges,
		"Lost":      lost,
		"Gone":      gone,
		"Picks":     picks,
		"StartISK":  iskString(towerStartISK),
	})
}

// considerPick is one corporation in the "consider now" picker.
type considerPick struct {
	CorpID  int64
	Name    string
	Free    string
	FreeISK float64
	Ready   bool // over the free-capital line where tower planning starts
	Towers  int
}

const tmplStarbases = `{{define "content"}}
<h2>Starbases</h2>
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}
<div class="stats">
  <div class="stat"><div class="label">Towers</div><div class="value">{{.Stats.Total}}</div></div>
  <div class="stat"><div class="label">Running</div><div class="value">{{.Stats.Running}}</div></div>
  <div class="stat"><div class="label">Being built</div><div class="value">{{.Stats.Building}}</div></div>
  <div class="stat"><div class="label">Need attention</div><div class="value"{{if .Stats.Attention}} style="color:#ef4444"{{end}}>{{.Stats.Attention}}</div></div>
  <div class="stat"><div class="label">Under siege</div><div class="value"{{if .Stats.Sieged}} style="color:#ef4444"{{end}}>{{.Stats.Sieged}}</div></div>
  <div class="stat"><div class="label">Regions / corps</div><div class="value">{{.Stats.Regions}} / {{.Stats.Corps}}</div></div>
  <div class="stat"><div class="label">P&amp;L 7d</div><div class="value"{{if .Stats.PnLNeg}} style="color:#ef4444"{{end}}>{{.Stats.PnL}}</div></div>
</div>
<div class="card" style="font-size:12px">
  <div style="display:flex;flex-wrap:wrap;gap:6px 18px;align-items:center">
    <form method="POST" style="display:inline"><input type="hidden" name="action" value="power">
      <input type="hidden" name="on" value="{{if .Power}}0{{else}}1{{end}}">
      Bot CEOs plant and run towers: {{if .Power}}<b>on</b>{{else}}<b style="color:#eab308">off</b>{{end}}
      <button type="submit" style="padding:3px 8px;font-size:11px">{{if .Power}}switch off{{else}}switch on{{end}}</button></form>
    <span style="color:#6b7280">Ice demand index <b>{{.IceDemand}}</b> (1 = shelf value) &middot; towers lost {{.Lost}} &middot; plans closed {{.Gone}}</span>
  </div>
  {{if .Picks}}
  <form method="POST" style="margin-top:8px">
    <input type="hidden" name="action" value="consider">
    Consider a tower now:
    <select name="corp" style="padding:3px 6px;font-size:12px">
      {{range .Picks}}<option value="{{.CorpID}}">{{.Name}} — ~{{.Free}} free{{if .Towers}}, {{.Towers}} tower plan(s){{end}}{{if not .Ready}} (under the line){{end}}</option>{{end}}
    </select>
    <button type="submit" style="padding:3px 8px;font-size:11px">Consider now</button>
    <span style="color:#6b7280" title="Runs the CEO's tower decision for this corporation now. Normal requirements apply, starting with {{.StartISK}} free capital.">
      runs the CEO's tower decision now ({{.StartISK}} free capital needed)</span>
  </form>
  {{else}}<p style="margin-top:8px">No bot CEO is running a corporation.</p>{{end}}
</div>
<div class="card">
  <h3>Every bot tower</h3>
  {{if .Truncated}}<p class="alert alert-error">Showing the first {{.Limit}} towers by corporation; the rest are not on this page.</p>{{end}}
  {{if .Board.Rows}}{{template "rb_towertable" .Board}}
  <p style="font-size:11px;color:#6b7280;margin-top:6px">Click a tower for details. Click a header to sort.</p>
  {{else}}<p>No bot corporation has planned a tower yet.</p>{{end}}
</div>
<div class="card">
  <h3>Sieges</h3>
  {{if .SiegeRows}}
  <table class="tbl" style="font-size:12px"><tr><th>Region</th><th>System</th><th>Attacker</th><th>Defender</th><th>Phase</th><th>Exit</th></tr>
    {{range .SiegeRows}}<tr><td>{{.Region}}</td><td>{{.System}}</td><td>{{.Attacker}}</td><td>{{.Defender}}</td><td>{{.Phase}}</td><td>{{.Exit}}</td></tr>{{end}}
  </table>
  {{else}}<p>No siege under way.</p>{{end}}
</div>
{{end}}`
