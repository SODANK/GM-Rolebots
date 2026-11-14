package rolebots

import (
	"context"
	"math"
	"sync"

	evedb "gurimoon/db"
)

// Choosing the round a gun is loaded with. Damage is the sort key and price only a tie-break:
// db.GetWeaponDamageProfile (db/destinydb.go) reads Em/Explosive/Kinetic/Thermal off the charge,
// so those four attributes are the damage the server applies. Price is never a multiplier in a
// choice: a fit is rebuilt every plan pass and diffed against the owned ship by exact (typeID,
// flag), so a price term would change the round and send the bot on a needless refit errand.
// Range (attribute 120) is not modelled: nothing on the server applies it. Resist matching is
// not done: a fit is generated before the bot knows what it will shoot, and ComputeFitStats sums
// the four damage types flat.

// chargeChoice is one loadable round with what the ranking needs. chargeFor builds it from cached
// data and pickCharge ranks it without touching a database.
type chargeChoice struct {
	typeID int64
	name   string
	// ask is the cheapest live sell order anywhere; 0 means nobody sells it.
	ask float64
	// basePrice of 0 means unknown in this dataset, never free. Faction ammunition carries it.
	basePrice float64
	// metaGroup is invMetaTypes.metaGroupID; 0 is plain T1, which has no row
	// there at all. 4 and up is faction and better.
	metaGroup int64
	// damage is the sum of attrs 114/116/117/118, exactly what GetWeaponDamageProfile hands the
	// damage model.
	damage float64
	// reachableAsk is the cheapest live ask inside the pilot's shopping reach: equal to ask when
	// reach is unbounded, zero when somebody sells the round but nobody the buy cap lets the pilot
	// shop from.
	reachableAsk float64
	// groupID decides how many are bought, never which one wins: a crystal is not spent by firing,
	// so it is bought in twos and ammunition in magazines (chargeQtyFor). It is absent from
	// pickCharge, whose ranking is damage then price.
	groupID int64
}

// chargeStickiness is how much of the best round's damage the loaded round must be worth to keep
// its slot, so a bot does not re-ammo every time the market moves.
const chargeStickiness = 0.85

// durableChargeGroups are the charge groups a firing module does not spend: crystals and mining
// crystals. Everything else in category 8 is consumed a round per cycle.
// See docs/comment-notes-b09.md#durable-charges.
var durableChargeGroups = map[int64]bool{
	86:  true, // Frequency Crystal
	374: true, // Advanced Beam Laser Crystal
	375: true, // Advanced Pulse Laser Crystal
	482: true, // Mining Crystal
	663: true, // Mercoxit Mining Crystal
}

// durableChargeQty is how many of a durable charge a fit carries: the one in the module plus one
// spare against a T2 crystal burning out mid-trip. The fit is diffed against the owned ship by
// exact (typeID, flag), so one breakage with a single crystal would read as a missing line.
const durableChargeQty = 2

// chargeQtyFor is how many rounds of one charge a generated fit buys.
func chargeQtyFor(groupID int64) int64 {
	if durableChargeGroups[groupID] {
		return durableChargeQty
	}
	return fitGenChargeQty
}

// chargeTier separates rounds by whether they can be bought, which outranks damage. 3: sold where
// the pilot's buy cap reaches. 2: sold somewhere beyond it. 1: T1 or T2 with a real basePrice
// (faction ammunition has basePrice 0, so price alone separates it even if invMetaTypes cannot be
// read). 0: faction and up, or unpriceable, a last resort.
func chargeTier(c chargeChoice) int {
	switch {
	case c.reachableAsk > 0:
		return 3 // sold where this pilot's buys can actually go
	case c.ask > 0:
		return 2 // somebody, somewhere, sells it, beyond its shopping cap
	case c.metaGroup <= 2 && c.basePrice > 0:
		return 1 // T1 or T2: buildable, priceable, reachable
	default:
		return 0 // faction and up, or unpriceable: a last resort
	}
}

// priceOrUnknown is the price to compare two rounds by. A zero reads as unknown, not free, so
// it ranks last.
func priceOrUnknown(c chargeChoice) float64 {
	if c.reachableAsk > 0 {
		return c.reachableAsk // the price this pilot would actually pay
	}
	if c.ask > 0 {
		return c.ask
	}
	if c.basePrice > 0 {
		return c.basePrice
	}
	return math.Inf(1)
}

// chargeRanksBefore reports whether a should be loaded in preference to b.
func chargeRanksBefore(a, b chargeChoice) bool {
	if ta, tb := chargeTier(a), chargeTier(b); ta != tb {
		return ta > tb
	}
	if a.damage != b.damage {
		return a.damage > b.damage
	}
	if pa, pb := priceOrUnknown(a), priceOrUnknown(b); pa != pb {
		return pa < pb
	}
	// Nothing separates them but identity: pick the same one every time, or the fit changes between
	// plan passes.
	return a.typeID < b.typeID
}

// pickCharge returns the index of the round to load, or -1 for nothing loadable. An incumbent
// keeps its slot while it is in the best tier and worth chargeStickiness of the best damage.
// Pure: the decision is arithmetic over chargeChoice fields, so it tests without a database.
func pickCharge(cands []chargeChoice, incumbent int64) int {
	if len(cands) == 0 {
		return -1
	}
	best := 0
	for i := 1; i < len(cands); i++ {
		if chargeRanksBefore(cands[i], cands[best]) {
			best = i
		}
	}
	if incumbent == 0 {
		return best
	}
	for i, c := range cands {
		if c.typeID != incumbent {
			continue
		}
		if chargeTier(c) == chargeTier(cands[best]) &&
			c.damage >= chargeStickiness*cands[best].damage {
			return i
		}
		break
	}
	return best
}

// chargeMetaByID is invMetaTypes in one map. Static seed data the server never writes, loaded
// once per process like hullProfileByID.
var (
	chargeMetaMu   sync.Mutex
	chargeMetaByID map[int64]int64
)

// chargeMetaGroups answers metaGroupID per type; a type with no row is
// absent, which reads as 0 and means plain T1.
func (m *Mod) chargeMetaGroups(ctx context.Context) map[int64]int64 {
	chargeMetaMu.Lock()
	defer chargeMetaMu.Unlock()
	if chargeMetaByID != nil {
		return chargeMetaByID
	}
	byID := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx, `SELECT typeID, metaGroupID FROM invMetaTypes`)
	if err != nil {
		// Not cached, so the next fit retries. Answering empty is safe only because chargeTier also
		// demands a real basePrice.
		return byID
	}
	defer rows.Close()
	for rows.Next() {
		var id, mg int64
		if rows.Scan(&id, &mg) == nil {
			byID[id] = mg
		}
	}
	if err := rows.Err(); err != nil {
		return byID
	}
	chargeMetaByID = byID
	return byID
}

// metaGroupOf is a type's invMetaTypes.metaGroupID, 0 for a base tech-one
// item (which has no row). Not charge-specific despite the cache's name: it
// is the one reader of that table, and the build book asks it the same
// question about modules and hulls.
func (m *Mod) metaGroupOf(ctx context.Context, typeID int64) int64 {
	return m.chargeMetaGroups(ctx)[typeID]
}

// chargeFor loads the best round this pilot can use into a weapon. chargeOptions is already
// filtered by evedb.ValidateChargeForModule, so size needs no thought here (a Medium Pulse Laser I
// is chargeSize 1). An empty gun is shed first by effectiveFit, so skipping this zeroes the fit.
// reach is the region set the pilot's shopping covers (chargeShopScope); nil means unbounded.
func (m *Mod) chargeFor(ctx context.Context, mod *FitModule, skills map[int64]int, incumbent int64, reach map[int64]bool) {
	opts, err := m.chargeOptions(ctx, mod.TypeID)
	if err != nil || len(opts) == 0 {
		return
	}
	usable := opts[:0:0]
	ids := make([]int64, 0, len(opts))
	for _, o := range opts {
		if ok, err := m.pilotCanUse(ctx, o.ID, skills); err == nil && ok {
			usable = append(usable, o)
			ids = append(ids, o.ID)
		}
	}
	if len(usable) == 0 {
		return
	}

	// Process-wide cache behind all three: no query here after warmup.
	attrs, err := evedb.TypeAttrsMany(ctx, m.db, ids)
	if err != nil {
		attrs = nil // every damage reads 0, and the ranking falls back to price
	}
	asks := m.chargeAsks(ctx)
	meta := m.chargeMetaGroups(ctx)
	var regionAsks map[int64]map[int64]float64
	if reach != nil {
		regionAsks = m.chargeRegionAsks(ctx)
	}

	cands := make([]chargeChoice, 0, len(usable))
	for _, o := range usable {
		a := attrs[o.ID]
		c := chargeChoice{
			typeID: o.ID, name: o.Name, groupID: o.GroupID,
			ask: asks[o.ID], basePrice: o.BasePrice, metaGroup: meta[o.ID],
			// Charges with no damage (mining crystals, cap booster charges) tie at zero and fall to price.
			damage: a[attrEmDamage] + a[attrExplosiveDamage] +
				a[attrKineticDamage] + a[attrThermalDamage],
		}
		if reach == nil {
			c.reachableAsk = c.ask
		} else if o.ID == incumbent {
			// The loaded round is in hand, so it counts as reachable. Scored by where it is sold, it would
			// be stripped and re-bought whenever the pilot moved away from the sellers.
			c.reachableAsk = c.ask
			if c.reachableAsk <= 0 {
				c.reachableAsk = c.basePrice
			}
			if c.reachableAsk <= 0 {
				c.reachableAsk = 0.01
			}
		} else {
			for region, price := range regionAsks[o.ID] {
				if !reach[region] {
					continue
				}
				if c.reachableAsk == 0 || price < c.reachableAsk {
					c.reachableAsk = price
				}
			}
		}
		cands = append(cands, c)
	}
	i := pickCharge(cands, incumbent)
	if i < 0 {
		return
	}
	mod.ChargeTypeID, mod.ChargeName = cands[i].typeID, cands[i].name
	mod.ChargeQty = chargeQtyFor(cands[i].groupID)
}
