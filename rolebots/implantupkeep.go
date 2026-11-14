package rolebots

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// Implant upkeep: plugging, buying, keeping, losing. Four doors into a pilot's head, all
// through plugImplant so the core's retime (db.WithTrainingRetimed) runs every time:
//   - implantUpkeep (ladder row): docked, the pilot compares what is listed here with what it
//     wants and buys the best deal it can afford. No pilot travels for an implant.
//   - implantKeepOrSell (loot ledger): a looted implant that helps goes in; the rest is listed.
//   - applyShopItem "implant": a fit line for a doctrine that asks for one.
//   - loseImplantsOnPod: every implant dies with the capsule, on the kill report.
// Settings: rolebots.implants.enabled (on), .budgetPct (15, the share of spendable money one
// implant may cost, before the pod-death divisor) and .maxPerPass (1).

// skillCybernetics is what every implant requires, at level I for the cheap ones and up to
// V for a set piece.
const skillCybernetics int64 = 3411

const (
	settingImplantsOn      = "rolebots.implants.enabled"
	settingImplantsBudget  = "rolebots.implants.budgetPct"
	settingImplantsPerPass = "rolebots.implants.maxPerPass"

	implantCheckInterval = 30 * time.Minute
	implantPurpose       = "implant"
)

var (
	implantsOn         atomic.Bool
	implantBudgetPct   atomic.Int64
	implantMaxPerPass  atomic.Int64
	implantCheckedMu   sync.Mutex
	implantCheckedAt   = map[int64]time.Time{}
	implantNothingHere = map[int64]int64{} // charID to stationID where nothing was worth buying
)

func init() {
	implantsOn.Store(true)
	implantBudgetPct.Store(15)
	implantMaxPerPass.Store(1)
	registerCharForget("implantCheckedAt/implantNothingHere", forgetImplantCheck)
}

func (m *Mod) loadImplantSettings(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingImplantsOn); err == nil && ok {
		implantsOn.Store(truthySetting(v))
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingImplantsBudget); err == nil && ok {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n >= 0 && n <= 100 {
			implantBudgetPct.Store(n)
		}
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingImplantsPerPass); err == nil && ok {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n >= 0 {
			implantMaxPerPass.Store(n)
		}
	}
}

// forgetImplantCheck drops the pilot's "looked lately" stamp, after a pod (its head is
// empty) and after a loot plug (its wants moved).
func forgetImplantCheck(charID int64) {
	implantCheckedMu.Lock()
	delete(implantCheckedAt, charID)
	delete(implantNothingHere, charID)
	implantCheckedMu.Unlock()
}

// plugImplant puts a hangar implant into the pilot's head through the
// core's own rule (slot conflict destroys the old one, skills checked) and
// its retime. The pilot must be docked where the item is.
func (m *Mod) plugImplant(ctx context.Context, b *botRun, itemID int64, why string) error {
	if b.StationID == 0 {
		return fmt.Errorf("plug implant: not docked")
	}
	var destroyed *evedb.EntityRow
	_, _, err := evedb.WithTrainingRetimed(ctx, m.db, b.CharID, func() error {
		var perr error
		destroyed, _, perr = evedb.PlugInImplant(ctx, m.db, b.CharID, itemID, b.StationID)
		return perr
	})
	if err != nil {
		return err
	}
	name := ""
	if er, gerr := evedb.GetEntityRow(ctx, m.db, itemID); gerr == nil && er != nil {
		name, _ = evedb.GetTypeName(ctx, m.db, er.TypeID)
	}
	if destroyed != nil {
		old, _ := evedb.GetTypeName(ctx, m.db, destroyed.TypeID)
		m.logBot(b.CharID, logInfo, "implants", "plugged in %s — %s; the %s it replaced is gone", name, why, old)
	} else {
		m.logBot(b.CharID, logInfo, "implants", "plugged in %s — %s", name, why)
	}
	forgetImplantCheck(b.CharID)
	return nil
}

// implantNeed is one Cybernetics level an implant the pilot holds or wants
// would need, with the gain that implant promises (implantwants.go units).
type implantNeed struct {
	Level int
	Gain  float64
	Why   string
}

// implantNeedsFrom turns the implants a pilot wants (positive gain) into the Cybernetics
// levels to train: one need per implant whose requirement is above the trained level, and
// level I when nothing is trained, so the next looted implant is pluggable. Pure.
func implantNeedsFrom(wanted []implantScore, reqLevel func(typeID int64) int, have int) []implantNeed {
	var out []implantNeed
	for _, w := range wanted {
		if w.Gain <= 0 || w.Type == nil {
			continue
		}
		need := reqLevel(w.Type.TypeID)
		if need <= have {
			continue
		}
		out = append(out, implantNeed{Level: need, Gain: w.Gain, Why: "lets it plug in " + w.Type.Name})
	}
	if have == 0 {
		out = append(out, implantNeed{Level: 1, Gain: 1, Why: "lets it plug in an implant at all"})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level < out[j].Level
		}
		return out[i].Gain > out[j].Gain
	})
	return out
}

// implantCyberneticsRequired is the Cybernetics level an implant type asks
// for (0 when the data names none).
func (m *Mod) implantCyberneticsRequired(ctx context.Context, typeID int64) int {
	reqs, err := evedb.GetRequiredSkills(ctx, m.db, typeID)
	if err != nil {
		return 0
	}
	for _, r := range reqs {
		if r.SkillID == skillCybernetics {
			return r.Level
		}
	}
	return 0
}

// implantSkillGap reports the Cybernetics level an implant needs and the
// level the pilot has; need > have is a gap.
func (m *Mod) implantSkillGap(ctx context.Context, charID, typeID int64) (need, have int) {
	need = m.implantCyberneticsRequired(ctx, typeID)
	if skills, err := m.charSkillsFor(ctx, charID); err == nil {
		have = skills[skillCybernetics]
	}
	return need, have
}

// hangarImplants lists the implants the pilot owns in station hangars (flag 4): looted or
// bought and not plugged. stationID 0 means every station.
func (m *Mod) hangarImplants(ctx context.Context, charID, stationID int64) ([]pluggedImplant, error) {
	byID, _, err := m.implantCatalogue(ctx)
	if err != nil {
		return nil, err
	}
	q := `SELECT e.itemID, e.typeID FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.ownerID = ? AND e.flag = 4 AND t.groupID = ?`
	args := []any{charID, groupCyberimplant}
	if stationID != 0 {
		q += ` AND e.locationID = ?`
		args = append(args, stationID)
	}
	rows, err := m.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pluggedImplant
	for rows.Next() {
		var itemID, typeID int64
		if err := rows.Scan(&itemID, &typeID); err != nil {
			return nil, err
		}
		if t := byID[typeID]; t != nil {
			out = append(out, pluggedImplant{ItemID: itemID, Type: t})
		}
	}
	return out, rows.Err()
}

// implantSkillNeeds is the skill planner's read: the Cybernetics levels the
// implants this pilot holds in a hangar and values would need, plus level I
// when it has none at all.
func (m *Mod) implantSkillNeeds(ctx context.Context, charID int64, tn Tuning) []implantNeed {
	if !implantsOn.Load() {
		return nil
	}
	p, err := m.implantPilotFor(ctx, charID, tn)
	if err != nil {
		return nil
	}
	held, err := m.hangarImplants(ctx, charID, 0)
	if err != nil {
		return nil
	}
	var wanted []implantScore
	for _, h := range held {
		if s := implantValue(h.Type, p); s.Gain > 0 {
			wanted = append(wanted, s)
		}
	}
	have := 0
	if skills, err := m.charSkillsFor(ctx, charID); err == nil {
		have = skills[skillCybernetics]
	}
	return implantNeedsFrom(wanted, func(typeID int64) int { return m.implantCyberneticsRequired(ctx, typeID) }, have)
}

// plugHeldImplants plugs whatever the pilot keeps in this station's hangar that it values
// and can now plug, such as a looted set piece held until Cybernetics caught up.
func (m *Mod) plugHeldImplants(ctx context.Context, b *botRun, p *implantPilot) int {
	held, err := m.hangarImplants(ctx, b.CharID, b.StationID)
	if err != nil || len(held) == 0 {
		return 0
	}
	plugged := 0
	for _, h := range held {
		s := implantValue(h.Type, *p)
		if s.Gain <= 0 {
			continue
		}
		if need, have := m.implantSkillGap(ctx, b.CharID, h.Type.TypeID); need > have {
			continue
		}
		if err := m.plugImplant(ctx, b, h.ItemID, "from its hangar; "+s.Why); err != nil {
			continue
		}
		plugged++
		if np, err := m.implantPilotFor(ctx, b.CharID, b.Tune); err == nil {
			*p = np
		}
	}
	return plugged
}

// implantKeepOrSell is the loot ledger's question about one looted implant in the pilot's
// hangar: plug it (true) or let the sale go on (false). It uses the shop's score. One the
// pilot values but cannot plug yet stays (true) while the skill planner trains Cybernetics.
func (m *Mod) implantKeepOrSell(ctx context.Context, b *botRun, typeID, itemID int64) bool {
	if !implantsOn.Load() || b.StationID == 0 {
		return false
	}
	byID, _, err := m.implantCatalogue(ctx)
	if err != nil {
		return false
	}
	t := byID[typeID]
	if t == nil {
		return false
	}
	p, err := m.implantPilotFor(ctx, b.CharID, b.Tune)
	if err != nil {
		return false
	}
	s := implantValue(t, p)
	if s.Gain <= 0 {
		m.logBot(b.CharID, logInfo, "implants", "selling looted %s — %s", t.Name, s.Why)
		return false
	}
	if need, have := m.implantSkillGap(ctx, b.CharID, typeID); need > have {
		m.logBot(b.CharID, logInfo, "implants",
			"keeping looted %s in the hangar until Cybernetics %d (has %d) — %s", t.Name, need, have, s.Why)
		forgetImplantCheck(b.CharID)
		return true
	}
	if err := m.plugImplant(ctx, b, itemID, "looted, and "+s.Why); err != nil {
		m.logBot(b.CharID, logWarn, "implants", "wanted the looted %s but could not plug it: %v", t.Name, err)
		return false
	}
	return true
}

// loseImplantsOnPod destroys every implant in the pilot's head, the way
// the player death path does, and returns the rows for the kill report.
func (m *Mod) loseImplantsOnPod(ctx context.Context, charID int64) []evedb.EntityRow {
	var lost []evedb.EntityRow
	_, _, err := evedb.WithTrainingRetimed(ctx, m.db, charID, func() error {
		var derr error
		lost, derr = evedb.DestroyCharacterImplants(ctx, m.db, charID)
		return derr
	})
	if err != nil {
		rlogf("rolebots: implants on pod %d: %v", charID, err)
	}
	forgetImplantCheck(charID)
	if len(lost) > 0 {
		value := 0.0
		names := make([]string, 0, len(lost))
		if byID, _, cerr := m.implantCatalogue(ctx); cerr == nil {
			for _, r := range lost {
				if t := byID[r.TypeID]; t != nil {
					value += t.BasePrice
					names = append(names, t.Name)
				}
			}
		}
		m.logBot(charID, logError, "implants", "lost %d implant(s) with the pod (~%s): %v",
			len(lost), iskString(value), names)
	}
	return lost
}

// implantUpkeep is the ladder row: docked, not looked lately, buy what is
// listed here and wanted, within budget. Never pre-empts.
func (m *Mod) implantUpkeep(ctx context.Context, b *botRun) (bool, error) {
	if !implantsOn.Load() || b.StationID == 0 || implantMaxPerPass.Load() <= 0 {
		return false, nil
	}
	implantCheckedMu.Lock()
	last := implantCheckedAt[b.CharID]
	nothingAt := implantNothingHere[b.CharID]
	implantCheckedMu.Unlock()
	if clock.Now().Sub(last) < implantCheckInterval && nothingAt == b.StationID {
		return false, nil
	}
	stamp := func(nothing bool) {
		implantCheckedMu.Lock()
		implantCheckedAt[b.CharID] = clock.Now()
		if nothing {
			implantNothingHere[b.CharID] = b.StationID
		} else {
			delete(implantNothingHere, b.CharID)
		}
		implantCheckedMu.Unlock()
	}
	byID, cat, err := m.implantCatalogue(ctx)
	if err != nil {
		return false, nil
	}
	// What is for sale here, before any scoring: most docks list nothing.
	ids := make([]int64, 0, len(cat))
	for _, t := range cat {
		ids = append(ids, t.TypeID)
	}
	asks, err := m.implantAsksAt(ctx, b, ids)
	if err != nil {
		return false, nil
	}
	if len(asks) == 0 {
		// Nothing for sale: still plug what it holds here, then stand down.
		if p, perr := m.implantPilotFor(ctx, b.CharID, b.Tune); perr == nil {
			m.plugHeldImplants(ctx, b, &p)
		}
		stamp(true)
		return false, nil
	}
	p, err := m.implantPilotFor(ctx, b.CharID, b.Tune)
	if err != nil {
		return false, nil
	}
	// What it already holds here and can plug now comes first: free.
	m.plugHeldImplants(ctx, b, &p)
	balance, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return false, nil
	}
	deaths := m.implantPodDeaths(ctx, b.CharID)
	// The next clone comes first: a pod death spends it and the implants together.
	budget := implantBudget(afterCloneReserve(balance, m.cloneReserve(ctx, b.CharID)), implantBudgetPct.Load(), deaths)
	type deal struct {
		s   implantScore
		ask float64
	}
	var deals []deal
	for typeID, ask := range asks {
		t := byID[typeID]
		if t == nil || t.Slot() == 0 || t.Slot() > 10 {
			continue
		}
		s := implantValue(t, p)
		if s.Gain <= 0 {
			continue
		}
		// Not bought until it can go in: a pilot short of Cybernetics trains toward what it holds,
		// and buying ahead of the skill would park ISK in the hangar.
		if need, have := m.implantSkillGap(ctx, b.CharID, t.TypeID); need > have {
			continue
		}
		deals = append(deals, deal{s: s, ask: ask})
	}
	if len(deals) == 0 {
		stamp(true)
		return false, nil
	}
	sort.SliceStable(deals, func(i, j int) bool { return deals[i].s.Gain > deals[j].s.Gain })
	bought := int64(0)
	for _, d := range deals {
		if bought >= implantMaxPerPass.Load() {
			break
		}
		cap := implantAskCap(budget, d.s.Type.BasePrice)
		if d.ask > cap {
			m.logBot(b.CharID, logInfo, "implants",
				"%s is listed here at %s — wants it (%s) but its cap is %s (budget %s after %d pod death(s) in 14 days)",
				d.s.Type.Name, iskString(d.ask), d.s.Why, iskString(cap), iskString(budget), deaths)
			continue
		}
		itemID, filled, spent, err := m.buyAtStationCapped(ctx, b, b.StationID, d.s.Type.TypeID, 1, cap, implantPurpose)
		if err != nil || filled <= 0 {
			continue
		}
		m.bookTrade(ctx, tradeEntry{
			CharID: b.CharID, Kind: tradeKindBuy, TypeID: d.s.Type.TypeID,
			TypeName: d.s.Type.Name, Qty: filled, Price: spent,
			Station: b.StationID, System: b.SystemID,
			Note: "implant — " + d.s.Why,
		})
		budget -= spent
		if err := m.plugImplant(ctx, b, itemID, fmt.Sprintf("bought for %s; %s", iskString(spent), d.s.Why)); err != nil {
			m.logBot(b.CharID, logWarn, "implants", "bought %s and could not plug it: %v", d.s.Type.Name, err)
			continue
		}
		bought++
		// The head changed; score the rest against it.
		if p, err = m.implantPilotFor(ctx, b.CharID, b.Tune); err != nil {
			break
		}
	}
	stamp(bought == 0)
	return false, nil
}

// implantAsksAt is the cheapest live ask per implant type at this station,
// other pilots' orders only.
func (m *Mod) implantAsksAt(ctx context.Context, b *botRun, typeIDs []int64) (map[int64]float64, error) {
	out := map[int64]float64{}
	if len(typeIDs) == 0 {
		return out, nil
	}
	args := append([]any{b.StationID, b.CharID}, int64Args(typeIDs)...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, MIN(price) FROM market_orders
		WHERE stationID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0 AND charID <> ?
		  AND typeID IN (`+placeholders(len(typeIDs))+`)
		GROUP BY typeID`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t int64
		var p float64
		if err := rows.Scan(&t, &p); err == nil && p > 0 {
			out[t] = p
		}
	}
	return out, rows.Err()
}

// ── the panel ───────────────────────────────────────────────────────────────

// implantRow is one line of the per-pilot Implants card.
type implantRow struct {
	Slot  int64
	Name  string
	What  string
	Price string
	Why   string
	Set   string
}

// implantView is the per-pilot Implants card.
type implantView struct {
	Plugged []implantRow
	Sets    []string
	Wants   []implantRow
	Budget  string
	Deaths  int
	Note    string
	On      bool
}

// implantProfileView builds the card for /rolebot/<id>.
func (m *Mod) implantProfileView(ctx context.Context, charID int64, tn Tuning) implantView {
	v := implantView{On: implantsOn.Load()}
	held, err := m.pluggedImplants(ctx, charID)
	if err != nil {
		v.Note = err.Error()
		return v
	}
	slots := make([]int64, 0, len(held))
	for s := range held {
		slots = append(slots, s)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i] < slots[j] })
	for _, s := range slots {
		t := held[s].Type
		v.Plugged = append(v.Plugged, implantRow{Slot: s, Name: t.Name, What: t.Label(), Price: iskString(t.BasePrice)})
	}
	p, err := m.implantPilotFor(ctx, charID, tn)
	if err != nil {
		v.Note = err.Error()
		return v
	}
	seen := map[string]bool{}
	for _, h := range p.Held {
		if h.Tag.Set == 0 {
			continue
		}
		key := fmt.Sprintf("%s|%s", h.Tag.Set, h.Tag.Grade)
		if seen[key] {
			continue
		}
		seen[key] = true
		same, _ := p.setStanding(h.Tag.Set, h.Tag.Grade)
		v.Sets = append(v.Sets, fmt.Sprintf("%s (%s grade) %d/%d", h.Tag.Set, h.Tag.Grade, same, 6))
	}
	_, cat, err := m.implantCatalogue(ctx)
	if err != nil {
		v.Note = err.Error()
		return v
	}
	ranked := rankImplants(cat, p)
	for i, s := range ranked {
		if i >= 8 {
			break
		}
		v.Wants = append(v.Wants, implantRow{Slot: s.Type.Slot(), Name: s.Type.Name, What: s.Type.Label(),
			Price: iskString(s.Type.BasePrice), Why: fmt.Sprintf("gain %.1f — %s", s.Gain, s.Why)})
	}
	if balance, err := evedb.GetCharBalance(ctx, m.db, charID); err == nil {
		v.Deaths = m.implantPodDeaths(ctx, charID)
		v.Budget = iskString(implantBudget(afterCloneReserve(balance, m.cloneReserve(ctx, charID)), implantBudgetPct.Load(), v.Deaths))
	}
	return v
}
