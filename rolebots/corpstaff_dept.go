package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"gurimoon/destiny"
)

// The department desks: what a bot CEO's directors and recruitment officers do on the
// staff desk's cadence (corpstaff.go). None of them moves ISK. A department head turns
// the corporation's needs into two levers the fleets read: demand, which weights the
// fleet-seat deal (promoteCorp, demandRotation) and asks the roles pass for an early
// seat pass (requestSeatPass); and a fleet order (rolebot_corp_fleet_orders), which the
// gang pickers prefer while it is fresh (gangMiningPlan, gangSite). An order is advice
// with a deadline: a fleet whose band, security or guns rule the site out picks as before.
// Operations orders the ores that cover the factory's shortfall, Security the nearest
// war-enemy system or distressed belt, Commerce runs the business power in its own name
// (corpOrderPlacer), and Recruitment works the application desk and headhunts.

const (
	// fleetOrderTTLSec: an order a desk stops refreshing lapses on its own.
	fleetOrderTTLSec = 3 * 3600
	// fleetOrderMaxJumps bounds how far from headquarters a distress order
	// may send the security wing (a war order reaches warExpeditionMaxJumps).
	fleetOrderMaxJumps = 10
	// seatRequestEverySec holds early seat passes to one per this long.
	seatRequestEverySec = 2 * 3600
	// deptDemandShift is how far a department's demand must move before it
	// asks for an early seat pass.
	deptDemandShift = 0.34
	// warSurgeFrac: on a war footing the security department gets this
	// share of the seat budget again, on top of it.
	warSurgeFrac = 0.5
	// commerce cadence by the head's patience (HIGH = hurried).
	commerceLagSlow, commerceLagMid, commerceLagFast = 2 * 3600, 3600, 1800
	// headhunting: how often a desk looks, and how many invitations a
	// corporation keeps out at once.
	headhuntEverySec  = 6 * 3600
	headhuntMaxOpen   = 3
	headhuntInviteTTL = 7 * 86400
	// hrInviteMul is what an invitation is worth in the invitee's own
	// review of the corporation (corpjoin.go).
	hrInviteMul = 1.5
)

// ── demand ──────────────────────────────────────────────────────────────────

// deptDemand is each department's demand in [0, 1], per corporation, as its
// head last measured it. In memory: the desks run every 15 minutes, so a
// restart costs one pass.
var (
	deptDemandMu   sync.Mutex
	deptDemand     = map[int64]map[string]float64{}
	deptWarFooting = map[int64]bool{}
	seatWantedAt   = map[int64]int64{}
)

// noteDeptDemand records a department's demand and asks for an early seat
// pass when it moved far enough to change the deal.
func noteDeptDemand(corpID int64, port string, v float64) {
	v = math.Max(0, math.Min(1, v))
	deptDemandMu.Lock()
	defer deptDemandMu.Unlock()
	if deptDemand[corpID] == nil {
		deptDemand[corpID] = map[string]float64{}
	}
	old, had := deptDemand[corpID][port]
	deptDemand[corpID][port] = v
	if had && math.Abs(v-old) >= deptDemandShift {
		seatWantedAt[corpID] = clock.Unix()
	}
}

func noteWarFooting(corpID int64, on bool) {
	deptDemandMu.Lock()
	defer deptDemandMu.Unlock()
	if deptWarFooting[corpID] != on {
		seatWantedAt[corpID] = clock.Unix()
	}
	deptWarFooting[corpID] = on
}

// requestSeatPass asks for an early roles-and-seat pass for a corporation: a new
// member, a promotion, anything that changes who should hold what.
func requestSeatPass(corpID int64) {
	deptDemandMu.Lock()
	seatWantedAt[corpID] = clock.Unix()
	deptDemandMu.Unlock()
}

// seatPassWanted reports whether a department asked for an early seat pass
// since the last one, and the last is at least seatRequestEverySec old.
func seatPassWanted(corpID, lastPass int64) bool {
	deptDemandMu.Lock()
	defer deptDemandMu.Unlock()
	at := seatWantedAt[corpID]
	return at > lastPass && clock.Unix()-lastPass >= seatRequestEverySec
}

// corpSeatDemand is the demand and war footing the seat deal reads, empty
// for a corporation no desk has measured.
func corpSeatDemand(corpID int64) (map[string]float64, bool) {
	deptDemandMu.Lock()
	defer deptDemandMu.Unlock()
	out := map[string]float64{}
	for k, v := range deptDemand[corpID] {
		out[k] = v
	}
	return out, deptWarFooting[corpID]
}

// deptOfActivity is the department a fleet activity belongs to.
func deptOfActivity(act string) string {
	switch act {
	case ActMining, ActIndMining:
		return portOperations
	case ActCombat, ActSecurity, ActPiracy:
		return portSecurity
	}
	return ""
}

// demandRotation is the seat deal's wheel with demand in it. Pure. With no demand it
// is fleetSeatRotation; each department's activities are entered once more per third
// of demand, so full demand deals three turns per lap against one. Order is kept, so
// ties favour the corporation's own trade (corpFleetActivities).
func demandRotation(activities []string, demand map[string]float64) []string {
	base := fleetSeatRotation(activities)
	if len(demand) == 0 {
		return base
	}
	out := append([]string(nil), base...)
	for _, act := range activities {
		extra := int(math.Round(2 * demand[deptOfActivity(act)]))
		for i := 0; i < extra; i++ {
			out = append(out, act)
		}
	}
	return out
}

// warSurgeSeats is how many seats a war footing adds, on top of the budget.
func warSurgeSeats(seats int, war bool, activities []string) int {
	if !war || seats <= 0 {
		return 0
	}
	for _, act := range activities {
		if deptOfActivity(act) == portSecurity {
			return max(1, int(math.Round(float64(seats)*warSurgeFrac)))
		}
	}
	return 0
}

// ── fleet orders ────────────────────────────────────────────────────────────

// corpFleetOrder is a department's live standing order to its fleets.
type corpFleetOrder struct {
	CorpID    int64
	Dept      string
	SystemID  int64
	OreNames  []string
	Reason    string
	PostedBy  int64
	PostedAt  int64
	ExpiresAt int64
	// Source is who posted it: a department desk (fleetOrderDesk, the default) or the
	// chair through the Corporate Portal (fleetOrderHuman). A human's order outranks the
	// desk's while it stands: the desk neither overwrites nor clears it (corpcommanddesk.go).
	Source string
}

const (
	fleetOrderDesk  = "desk"
	fleetOrderHuman = "human"
)

func (m *Mod) postFleetOrder(ctx context.Context, o corpFleetOrder) error {
	now := clock.Unix()
	if o.Source == "" {
		o.Source = fleetOrderDesk
	}
	// What was standing before, so corp chat hears only a new order (chatter_corp.go):
	// the desks re-post the same one each pass.
	prev := m.corpFleetOrderFor(ctx, o.CorpID, o.Dept)
	expires := now + fleetOrderTTLSec
	if o.ExpiresAt > now {
		expires = o.ExpiresAt
	}
	// The ON DUPLICATE arms keep a standing human order over a desk's: a column is taken
	// from the new row only when the old is not a live human order or the new one is the chair's.
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_fleet_orders
			(corporationID, activity, systemID, oreNames, reason, postedBy, postedAt, expiresAt, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			postedAt = IF(source = 'human' AND expiresAt > ? AND VALUES(source) <> 'human', postedAt,
			           IF(systemID <=> VALUES(systemID) AND oreNames <=> VALUES(oreNames), postedAt, VALUES(postedAt))),
			systemID  = IF(source = 'human' AND expiresAt > ? AND VALUES(source) <> 'human', systemID, VALUES(systemID)),
			oreNames  = IF(source = 'human' AND expiresAt > ? AND VALUES(source) <> 'human', oreNames, VALUES(oreNames)),
			reason    = IF(source = 'human' AND expiresAt > ? AND VALUES(source) <> 'human', reason, VALUES(reason)),
			postedBy  = IF(source = 'human' AND expiresAt > ? AND VALUES(source) <> 'human', postedBy, VALUES(postedBy)),
			expiresAt = IF(source = 'human' AND expiresAt > ? AND VALUES(source) <> 'human', expiresAt, VALUES(expiresAt)),
			source    = IF(source = 'human' AND expiresAt > ? AND VALUES(source) <> 'human', source, VALUES(source))`,
		o.CorpID, o.Dept, o.SystemID, truncate(strings.Join(o.OreNames, ","), 255),
		truncate(o.Reason, 255), o.PostedBy, now, expires, o.Source,
		now, now, now, now, now, now, now)
	if err == nil && fleetOrderIsNew(prev, o) {
		goSafe("chatter corp order", func() {
			cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			m.chatterCorpOrder(cctx, o)
		})
	}
	return err
}

// fleetOrderIsNew reports whether an order changes what the fleets are told:
// a new system or a new ore list. A standing human order the desk could not
// overwrite is not new either.
func fleetOrderIsNew(prev *corpFleetOrder, o corpFleetOrder) bool {
	if prev == nil {
		return true
	}
	if prev.Source == fleetOrderHuman && o.Source != fleetOrderHuman && prev.ExpiresAt > clock.Unix() {
		return false
	}
	return prev.SystemID != o.SystemID || strings.Join(prev.OreNames, ",") != strings.Join(o.OreNames, ",")
}

// clearFleetOrder withdraws a desk's order. A standing human order is left
// where it is.
func (m *Mod) clearFleetOrder(ctx context.Context, corpID int64, dept string) {
	_, _ = m.db.ExecContext(ctx, `
		DELETE FROM rolebot_corp_fleet_orders
		 WHERE corporationID = ? AND activity = ? AND NOT (source = 'human' AND expiresAt > ?)`,
		corpID, dept, clock.Unix())
}

// clearFleetOrderAny withdraws an order whoever posted it (the chair
// cancelling its own).
func (m *Mod) clearFleetOrderAny(ctx context.Context, corpID int64, dept string) {
	_, _ = m.db.ExecContext(ctx,
		`DELETE FROM rolebot_corp_fleet_orders WHERE corporationID = ? AND activity = ?`, corpID, dept)
}

// corpFleetOrderFor is the live order a department has out, nil for none.
func (m *Mod) corpFleetOrderFor(ctx context.Context, corpID int64, dept string) *corpFleetOrder {
	if corpID == 0 {
		return nil
	}
	var o corpFleetOrder
	var ores string
	err := m.db.QueryRowContext(ctx, `
		SELECT systemID, oreNames, reason, postedBy, postedAt, expiresAt, source
		  FROM rolebot_corp_fleet_orders
		 WHERE corporationID = ? AND activity = ? AND expiresAt > ?`, corpID, dept, clock.Unix()).
		Scan(&o.SystemID, &ores, &o.Reason, &o.PostedBy, &o.PostedAt, &o.ExpiresAt, &o.Source)
	if err != nil {
		return nil
	}
	o.CorpID, o.Dept = corpID, dept
	for _, n := range strings.Split(ores, ",") {
		if n = strings.TrimSpace(n); n != "" {
			o.OreNames = append(o.OreNames, n)
		}
	}
	return &o
}

// listFleetOrders is every live order a corporation has out, for the panel.
func (m *Mod) listFleetOrders(ctx context.Context, corpID int64) []corpFleetOrder {
	rows, err := m.db.QueryContext(ctx, `
		SELECT activity, systemID, oreNames, reason, postedBy, postedAt, expiresAt, source
		  FROM rolebot_corp_fleet_orders WHERE corporationID = ? AND expiresAt > ?
		 ORDER BY activity`, corpID, clock.Unix())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []corpFleetOrder
	for rows.Next() {
		var o corpFleetOrder
		var ores string
		if rows.Scan(&o.Dept, &o.SystemID, &ores, &o.Reason, &o.PostedBy, &o.PostedAt, &o.ExpiresAt, &o.Source) == nil {
			o.CorpID = corpID
			if ores != "" {
				o.OreNames = strings.Split(ores, ",")
			}
			out = append(out, o)
		}
	}
	return out
}

// ── Operations ──────────────────────────────────────────────────────────────

// corpShortfallUnits is the factory's mineral shortfall in units, 0 for a
// corporation with no factory or nothing short.
func (m *Mod) corpShortfallUnits(ctx context.Context, corpID int64) float64 {
	stationID, officeID := m.corpFactoryOffice(ctx, corpID)
	if officeID == 0 {
		return 0
	}
	short, err := m.corpMaterialShortfall(ctx, corpID, stationID, officeID)
	if err != nil {
		return 0
	}
	total := 0.0
	for _, n := range short {
		total += float64(n)
	}
	return total
}

// operationsDesk measures the factory's hunger and tells the mining fleets
// what to dig for it.
func (m *Mod) operationsDesk(ctx context.Context, c *corpCEO, ch Charter, d *staffMember, facts *CorpFacts) error {
	stationID, officeID := m.corpFactoryOffice(ctx, c.CorpID)
	if officeID == 0 {
		noteDeptDemand(c.CorpID, portOperations, 0)
		m.clearFleetOrder(ctx, c.CorpID, portOperations)
		return nil
	}
	short, err := m.corpMaterialShortfall(ctx, c.CorpID, stationID, officeID)
	if err != nil {
		return err
	}
	// A resting bid for a deep metal still counts as hunger (corpdeep.go): the shortfall
	// subtracts what bids are buying, so the order would otherwise lapse. Deep ores lead.
	bidding := m.corpDeepOutstanding(ctx, c.CorpID)
	if short == nil && len(bidding) > 0 {
		short = map[int64]int64{}
	}
	for t, n := range bidding {
		short[t] += n
	}
	deepWant := map[int64]int64{}
	for t, n := range short {
		if isDeepMineral(t) && n > 0 {
			deepWant[t] = n
		}
	}
	deep := len(deepWant) > 0
	m.noteDeepMineralWants(ctx, c, stationID, deepWant)
	if len(short) == 0 {
		noteDeptDemand(c.CorpID, portOperations, 0)
		m.clearFleetOrder(ctx, c.CorpID, portOperations)
		return nil
	}
	// Demand: how many mining-op loads the shortfall is, saturating at about a dozen,
	// counted in m³ of the ores that carry it.
	p, _ := m.loadCorpPolicy(ctx, c.CorpID)
	yield := m.corpRefineYield(ctx, c.CorpID, p)
	if yield <= 0 {
		yield = 0.5
	}
	picks := m.oreBidPicks(ctx, stationID, short, yield)
	if len(picks) == 0 && !deep {
		noteDeptDemand(c.CorpID, portOperations, 0.3)
		return nil
	}
	var names []string
	seen := map[string]bool{}
	units := int64(0)
	if deep {
		for _, n := range deepOreNames {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, b := range picks {
		units += b.units
		if n := oreFamilyBases[destiny.BaseOreType(b.typeID)]; n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	noteDeptDemand(c.CorpID, portOperations, math.Tanh(float64(units)/200000))
	if len(names) == 0 {
		return nil
	}
	prev := m.corpFleetOrderFor(ctx, c.CorpID, portOperations)
	reason := fmt.Sprintf("the factory at %s is short of %d mineral(s)", m.stationNameOr(ctx, stationID), len(short))
	if err := m.postFleetOrder(ctx, corpFleetOrder{CorpID: c.CorpID, Dept: portOperations,
		OreNames: names, Reason: reason, PostedBy: d.CharID}); err != nil {
		return err
	}
	if prev == nil || strings.Join(prev.OreNames, ",") != strings.Join(names, ",") {
		m.ceoLog(ctx, c, powerRoles, "order", 0, float64(units),
			"ordered the mining fleets onto %s — %s", strings.Join(names, ", "), reason)
	}
	return nil
}

// ── Security ────────────────────────────────────────────────────────────────

// securityDesk reads the war and the belts and points the combat wing.
func (m *Mod) securityDesk(ctx context.Context, c *corpCEO, ch Charter, d *staffMember, facts *CorpFacts) error {
	// The fleet doctrines (fleetdoctrine_db.go): what the wing flies, re-read
	// from the roster, the market and the treasury, written only on a change.
	if fleetDoctrineOn() && deskDue(c.CorpID, "doctrine", doctrineDeskEverySec) {
		m.deriveCorpDoctrines(ctx, c, ch, facts)
	}
	// A siege (possiege.go) outranks every order, then the alliance's defend call
	// (gangcalls.go): calls to arms point every fleet of the wing there (formCallsToArms).
	armsSys, armsWhy := m.siegeOrderFor(ctx, c.CorpID)
	if armsSys != 0 && m.siegeLedByMate(ctx, c.CorpID, armsSys) {
		// A mate leads the attack: its one alliance fleet takes this wing (siegefleet.go). No order
		// and no call of this corporation's own, and no patrol either, so the wing is free to join.
		noteDeptDemand(c.CorpID, portSecurity, 1)
		return nil
	}
	if armsSys == 0 {
		armsSys, armsWhy = m.allianceDefendOrderFor(ctx, c.CorpID)
	}
	if armsSys != 0 {
		noteDeptDemand(c.CorpID, portSecurity, 1)
		prev := m.corpFleetOrderFor(ctx, c.CorpID, portSecurity)
		if err := m.postFleetOrder(ctx, corpFleetOrder{CorpID: c.CorpID, Dept: portSecurity,
			SystemID: armsSys, Reason: armsWhy, PostedBy: d.CharID}); err != nil {
			return err
		}
		m.postHelpCall(ctx, 0, c.CorpID, armsSys, helpCallToArms, 0, armsWhy)
		if prev == nil || prev.SystemID != armsSys {
			m.ceoLog(ctx, c, powerRoles, "order", armsSys, 0, "ordered the combat wing to %s — %s", m.nameOfSystem(ctx, armsSys), armsWhy)
		}
		return nil
	}
	footing, fight, footWhy := m.ceoWarFooting(ctx, c, ch)
	war := footing > 0 && fight
	noteWarFooting(c.CorpID, war)
	distress := m.distressedBeltSystems(ctx, c.CorpID)

	demand := 0.0
	if footing > 0 {
		demand = footing
	}
	demand = math.Max(demand, math.Min(1, float64(len(distress))/5))
	noteDeptDemand(c.CorpID, portSecurity, demand)

	hq := m.stationSystemID(ctx, m.corpHQStation(ctx, c.CorpID))
	u, err := m.universe(ctx)
	if err != nil || hq == 0 {
		return err
	}
	near := hopsWithin(u, hq, fleetOrderMaxJumps)

	var enemySys []int64
	if war {
		enemySys = relWarEnemySystemsOf(c.CorpID)
	}
	var distressSys []int64
	for id := range distress {
		distressSys = append(distressSys, id)
	}
	// The war target is searched across the map, not the home radius: a war is declared
	// on an owner wherever it works. Distress stays a home-ground order.
	var pick int64
	var why string
	if war {
		reach := hopsWithin(u, hq, warExpeditionMaxJumps)
		// Where the enemy IS (warhunt.go), kept while it is still worth
		// holding, before the nearest-address fallback: that one moves every
		// time an enemy pilot does.
		var standing int64
		if o := m.corpFleetOrderFor(ctx, c.CorpID, portSecurity); o != nil {
			standing = o.SystemID
		}
		pick, why = pickWarTargetSystem(m.warEnemyCensus(ctx, c.CorpID), reach, standing)
		if pick == 0 {
			pick, why = pickSecurityTarget(reach, enemySys, nil, true)
		}
	}
	if pick == 0 {
		pick, why = pickSecurityTarget(near, nil, distressSys, false)
	}
	if pick == 0 {
		m.clearFleetOrder(ctx, c.CorpID, portSecurity)
		return nil
	}
	if war {
		why += " (" + footWhy + ")"
	}
	prev := m.corpFleetOrderFor(ctx, c.CorpID, portSecurity)
	if err := m.postFleetOrder(ctx, corpFleetOrder{CorpID: c.CorpID, Dept: portSecurity,
		SystemID: pick, Reason: why, PostedBy: d.CharID}); err != nil {
		return err
	}
	if prev == nil || prev.SystemID != pick {
		m.ceoLog(ctx, c, powerRoles, "order", pick, 0,
			"ordered the combat wing to %s — %s", m.nameOfSystem(ctx, pick), why)
	}
	return nil
}

// pickSecurityTarget chooses where the wing is sent. Pure. At war it goes to the
// nearest system a war enemy works or keeps an address in; failing that (and always
// when not at war) the nearest system whose belts its miners are chased off. Only
// systems within `near` hops of headquarters are candidates; ties break on system ID.
func pickSecurityTarget(near map[int64]int, enemy, distress []int64, war bool) (int64, string) {
	best := func(ids []int64) int64 {
		var pick int64
		bestHops := math.MaxInt
		for _, id := range ids {
			h, ok := near[id]
			if !ok {
				continue
			}
			if h < bestHops || (h == bestHops && id < pick) {
				pick, bestHops = id, h
			}
		}
		return pick
	}
	if war {
		if p := best(enemy); p != 0 {
			return p, "the enemy works there"
		}
	}
	if p := best(distress); p != 0 {
		return p, "our miners are being run off its belts"
	}
	return 0, ""
}

// hopsWithin is every system within maxJumps of from, with its hop count.
func hopsWithin(u *universe, from int64, maxJumps int) map[int64]int {
	out := map[int64]int{from: 0}
	frontier := []int64{from}
	for depth := 1; depth <= maxJumps && len(frontier) > 0; depth++ {
		var next []int64
		for _, s := range frontier {
			for _, n := range u.adj[s] {
				if _, seen := out[n]; seen {
					continue
				}
				out[n] = depth
				next = append(next, n)
			}
		}
		frontier = next
	}
	return out
}

// securityOddsMul is the Director of Security's say in fight-or-hire: a
// violent head takes on up to a third worse odds, a pacifist a third less.
// 1 with no head.
func (m *Mod) securityOddsMul(ctx context.Context, c *corpCEO) float64 {
	h := m.staffFor(ctx, c).head(portSecurity)
	if h == nil {
		return 1
	}
	return 1 + h.Ident.trait(TraitAggression)/3
}

// ── Commerce ────────────────────────────────────────────────────────────────

// commerceDesk runs the business power in the Director of Commerce's name,
// at its own pace.
func (m *Mod) commerceDesk(ctx context.Context, c *corpCEO, ch Charter, d *staffMember) error {
	lag := int64(bend(d.Ident.trait(TraitPatience), commerceLagSlow, commerceLagMid, commerceLagFast))
	if !deskDue(c.CorpID, "commerce", lag) {
		return nil
	}
	return m.ceoBusiness(ctx, c, ch)
}

// ── Diplomacy ───────────────────────────────────────────────────────────────

// Diplomacy desk cadence by patience: LOW patience is unhurried.
const (
	diplomacyLagSlow = 24 * 3600
	diplomacyLagMid  = 12 * 3600
	diplomacyLagFast = 6 * 3600
)

// diplomacyDesk runs the diplomacy pass in the Director of Diplomacy's name,
// every several hours by its own patience, with its own bend on the rates.
// A part-time office: the holder's day job goes on between passes.
func (m *Mod) diplomacyDesk(ctx context.Context, c *corpCEO, ch Charter, d *staffMember) error {
	lag := int64(bend(d.Ident.trait(TraitPatience), diplomacyLagSlow, diplomacyLagMid, diplomacyLagFast))
	if !deskDue(c.CorpID, "diplomacy", lag) {
		return nil
	}
	return m.diplomacyPass(ctx, c, ch, diploBendOf(d.Ident))
}

// diplomacyPortfolio is whether the corporation has relations worth a
// Director's chair: the layer on, the corporation big enough, and either an
// opinion on the ledger or a war on its hands. Appended to the departments
// the arms justify (ceoRoles).
func (m *Mod) diplomacyPortfolio(ctx context.Context, corpID int64, members int) []string {
	if !relationsEnabled() || !ceoDiplomacyOn() || !relLedgerOn() || members < diplomacyFromMembers {
		return nil
	}
	if relCorpAtWar(corpID) {
		return []string{portDiplomacy}
	}
	var n int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_relations WHERE corporationID = ? AND yielded = 0`, corpID).Scan(&n)
	if n == 0 {
		return nil
	}
	return []string{portDiplomacy}
}

// commerceHead is the Director of Commerce's charID when it is a pilot the module
// flies, 0 otherwise: corpOrderPlacer's first choice.
func (m *Mod) commerceHead(ctx context.Context, corpID int64) int64 {
	var id int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT s.characterID FROM rolebot_corp_posts s
		  JOIN rolebot_characters b ON b.characterID = s.characterID AND b.enabled = 1
		  JOIN character_ ch ON ch.characterID = s.characterID AND ch.corporationID = s.corporationID
		 WHERE s.corporationID = ? AND s.post = ? AND s.portfolio = ?
		 LIMIT 1`, corpID, postDirector, portCommerce).Scan(&id)
	return id
}

// ── Recruitment ─────────────────────────────────────────────────────────────

// personnelDesk is the Recruitment Officers' pass: the application desk,
// then headhunting.
func (m *Mod) personnelDesk(ctx context.Context, c *corpCEO, ch Charter, s *corpStaff, facts *CorpFacts) error {
	var bots []*staffMember
	for _, p := range s.Personnel {
		if !p.Human && p.Enabled {
			bots = append(bots, p)
		}
	}
	if len(bots) == 0 {
		// A human Recruitment Officer answers in the corp window; the CEO still covers a
		// player waiting at the door at this desk's cadence, and every applicant while it
		// is logged off (its think, which works the desk, does not run then).
		if popIsOffline(c.CharID) || m.humanApplicantsPending(ctx, c.CorpID) {
			return m.workHRDesk(ctx, c, ch,
				hrReader{CharID: c.CharID, Accepts: ceoHRPerThink, DelaySec: ch.HRDelaySec})
		}
		return nil
	}
	who := bots[int(clock.Unix()/staffDeskEveryTicks)%len(bots)]
	pctx := withStaffActor(ctx, s.executor(who, "Recruitment Officer"))
	rd := hrReader{
		CharID:   who.CharID,
		Accepts:  hrOfficerPerPass * len(bots),
		DelaySec: min(ch.HRDelaySec, hrOfficerDelaySec),
	}
	if err := m.workHRDesk(pctx, c, ch, rd); err != nil {
		return err
	}
	if deskDue(c.CorpID, "headhunt", headhuntEverySec) {
		return m.headhunt(pctx, c, ch, who, facts)
	}
	return nil
}

// corpInvitees is who a corporation has invited, unexpired.
func (m *Mod) corpInvitees(ctx context.Context, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	ids, err := int64Column(ctx, m, `
		SELECT characterID FROM rolebot_corp_invites WHERE corporationID = ? AND expiresAt > ?`,
		corpID, clock.Unix())
	if err != nil {
		return out
	}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// pilotInvites is which corporations have invited a pilot, unexpired, read by the
// pilot's own corporation review (corpjoin.go).
func (m *Mod) pilotInvites(ctx context.Context, charID int64) map[int64]bool {
	out := map[int64]bool{}
	ids, err := int64Column(ctx, m, `
		SELECT corporationID FROM rolebot_corp_invites WHERE characterID = ? AND expiresAt > ?`,
		charID, clock.Unix())
	if err != nil {
		return out
	}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// headhunt invites pilots a department is short of: fleet seats with nobody able to
// hold them. A seat needs Leadership (promoteCorp), so a corporation whose members
// have none fields no fleets however its seats are dealt.
func (m *Mod) headhunt(ctx context.Context, c *corpCEO, ch Charter, who *staffMember, facts *CorpFacts) error {
	if len(corpFleetActivities(facts.Policy)) == 0 {
		return nil
	}
	cands, err := m.rankCorpFCs(ctx, c.CorpID)
	if err != nil {
		return err
	}
	seats := len(cands) / FCSeatDivisor()
	able := 0
	for _, cd := range cands {
		if cd.Leadership >= 1 {
			able++
		}
	}
	short := seats - able
	if seats == 0 && len(cands) >= ceoHierarchyFrom {
		short = 1 - able // a corporation big enough to want its first commander
	}
	if short <= 0 {
		return nil
	}
	var open int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_invites WHERE corporationID = ? AND expiresAt > ?`,
		c.CorpID, clock.Unix()).Scan(&open)
	room := min(short, headhuntMaxOpen-open)
	if room <= 0 {
		return nil
	}
	free, unlimited, err := m.corpFreeOpenings(ctx, c.CorpID)
	if err != nil {
		return err
	}
	if !unlimited && free <= 0 {
		return nil // nowhere to put them; the waiting list is the desk's
	}

	// Candidates: enabled rostered pilots in NPC corporations (whose review looks at player
	// corporations) with Leadership, in the headquarters' region, no application or invite out.
	hqSys := m.stationSystemID(ctx, m.corpHQStation(ctx, c.CorpID))
	if hqSys == 0 {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID, ch.characterName, ch.corporationID
		  FROM character_ ch
		  JOIN rolebot_characters b ON b.characterID = ch.characterID AND b.enabled = 1
		  JOIN crpNPCCorporations n ON n.corporationID = ch.corporationID
		  JOIN mapSolarSystems ms ON ms.solarSystemID = ch.solarSystemID
		  JOIN mapSolarSystems hq ON hq.solarSystemID = ?
		  JOIN entity e ON e.ownerID = ch.characterID AND e.typeID = ? AND e.flag IN (7, 61)
		  JOIN entity_attributes a ON a.itemID = e.itemID AND a.attributeID = 280 AND COALESCE(a.valueInt, 0) >= 1
		  LEFT JOIN rolebot_corp_applications ap ON ap.characterID = ch.characterID AND ap.state = 'pending'
		  LEFT JOIN rolebot_corp_invites iv ON iv.characterID = ch.characterID AND iv.corporationID = ?
		 WHERE ms.regionID = hq.regionID AND ap.characterID IS NULL AND iv.characterID IS NULL
		 ORDER BY ch.characterID
		 LIMIT 40`, hqSys, skillLeadership, c.CorpID)
	if err != nil {
		if isMissingTable(err) {
			return nil
		}
		return fmt.Errorf("headhunt: %w", err)
	}
	type cand struct {
		id     int64
		name   string
		corpID int64
		fit    float64
	}
	var list []cand
	for rows.Next() {
		var x cand
		if rows.Scan(&x.id, &x.name, &x.corpID) == nil {
			list = append(list, x)
		}
	}
	rows.Close()

	blocked := map[int64]bool{}
	var scored []cand
	for _, x := range list {
		// Never poach from a corporation this one counts a friend.
		if relCorpStanding(c.CorpID, x.corpID) > 0 || blocked[x.id] {
			continue
		}
		ids, err := m.loadPilotIdentities(ctx, x.id, true)
		if err != nil || len(ids) == 0 {
			continue
		}
		p := ids[0]
		if v := corpEligibility(p, *facts, nil); !v.OK {
			continue
		}
		x.fit = corpTagFit(p, facts.Policy.Tags)
		if x.fit < hrEffectiveBar(ch.HRBar, facts.Members, facts.MemberLimit) {
			continue
		}
		scored = append(scored, x)
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].fit > scored[j].fit })
	now := clock.Unix()
	for _, x := range scored {
		if room <= 0 {
			break
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT IGNORE INTO rolebot_corp_invites
				(corporationID, characterID, reason, invitedBy, invitedAt, expiresAt)
			VALUES (?, ?, ?, ?, ?, ?)`,
			c.CorpID, x.id, "the fleets are short of commanders", who.CharID, now, now+headhuntInviteTTL); err != nil {
			return err
		}
		// The invitee's corporation review is pulled forward to now, so it
		// answers this week rather than whenever its clock next comes round.
		_, _ = m.db.ExecContext(ctx,
			`UPDATE rolebot_corp_reviews SET nextReviewAt = LEAST(nextReviewAt, ?) WHERE characterID = ?`,
			now, x.id)
		m.ceoLog(ctx, c, powerHR, "invite", x.id, x.fit,
			"invited %s to join — it holds Leadership and the fleets are short of commanders (fit %.2f)",
			x.name, x.fit)
		room--
	}
	return nil
}

// pruneCorpInvites drops expired invitations and those whose pilot joined.
func (m *Mod) pruneCorpInvites(ctx context.Context) {
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_invites WHERE expiresAt <= ?`, clock.Unix())
	_, _ = m.db.ExecContext(ctx, `
		DELETE iv FROM rolebot_corp_invites iv
		  JOIN character_ ch ON ch.characterID = iv.characterID AND ch.corporationID = iv.corporationID`)
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_fleet_orders WHERE expiresAt <= ?`, clock.Unix())
}

func init() {
	registerCorpForget("deptDemand/deptWarFooting/seatWantedAt", func(id int64) {
		deptDemandMu.Lock()
		delete(deptDemand, id)
		delete(deptWarFooting, id)
		delete(seatWantedAt, id)
		deptDemandMu.Unlock()
	})
}
