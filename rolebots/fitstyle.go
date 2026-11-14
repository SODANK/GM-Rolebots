package rolebots

import (
	"context"
	"math"
)

// Pilot intent: how a pilot wants to fight, decided before the guns go on. Cautious
// pilots lean Kite and reckless ones Brawl, expressed through module tags (modtags.go).
// A pilot inside styleDeadband gets engageAuto, which returns wishList's own answer.
// Intent works by role order only (a styled role first, the plain role behind it in the
// same wish), never by a score multiplier, so a style never empties a slot.

// engageStyle is a pilot's intended engagement range.
type engageStyle uint8

const (
	engageAuto engageStyle = iota
	engageBrawl
	engageKite
	// engageHold is a fleet seat's intent, never a pilot's (styleFor does not draw it):
	// long guns held at optimal on a hull that cannot kite. See fleetdoctrine_seat.go
	// seatIntent and combatdoctrine.go stanceHold.
	engageHold
)

func (s engageStyle) String() string {
	switch s {
	case engageBrawl:
		return "brawl"
	case engageKite:
		return "kite"
	case engageHold:
		return "hold"
	}
	return "auto"
}

// lean is the combatLean this intent stands for.
func (s engageStyle) lean() combatLean {
	switch s {
	case engageBrawl:
		return leanBrawler
	case engageKite, engageHold:
		return leanKiter
	}
	return leanUnknown
}

// styleDeadband is how far apart the two leans must be before a pilot has a style.
// Inside it the pilot is engageAuto.
const styleDeadband = 0.15

// styleBrawlProb is the chance a pilot with lean difference s (BrawlLean minus
// KiteLean, -1..+1) is a brawler this epoch. It is clamped to 0.05..0.95 so every
// pilot is occasionally on the other side.
func styleBrawlProb(s float64) float64 {
	return math.Max(0.05, math.Min(0.95, 0.5+0.45*s))
}

// styleFor is a pilot's style for this epoch. Pure over the tuning and the taste seed,
// so it is stable within an epoch (a style that moved each pass would send the pilot
// shopping each pass). A zero seed takes the dominant side.
func styleFor(tn Tuning, seed int64) engageStyle {
	s := tn.BrawlLean - tn.KiteLean
	if math.Abs(s) < styleDeadband {
		return engageAuto
	}
	p := styleBrawlProb(s)
	if seed == 0 {
		if p >= 0.5 {
			return engageBrawl
		}
		return engageKite
	}
	u := (splitmixUnit(uint64(seed)^styleSeedSalt) + 1) / 2
	if u < p {
		return engageBrawl
	}
	return engageKite
}

// styleSeedSalt keeps the style draw independent of the per-type taste draws made
// off the same seed.
const styleSeedSalt = 0x5717E5EED

// styleShares is how much of a pilot's combat appetite backs each style's skills
// (skillscore.go). Both are zero inside the deadband.
func styleShares(tn Tuning) (brawl, kite float64) {
	s := tn.BrawlLean - tn.KiteLean
	if math.Abs(s) < styleDeadband {
		return 0, 0
	}
	p := styleBrawlProb(s)
	return p, 1 - p
}

// ── styled roles ────────────────────────────────────────────────────────────

// The styled roles. Each is a plain role with a tag filter in front of it (see
// styleAdmits) and is followed by its plain role in the same wish.
const (
	roleWeaponKite  modRole = "weapon.kite"
	roleWeaponBrawl modRole = "weapon.brawl"
	roleSpeedMWD    modRole = "speed.mwd"
	roleSpeedAB     modRole = "speed.ab"
	roleTackleLong  modRole = "tackle.long" // the disruptor
	roleEwarKite    modRole = "ewar.kite"   // damps, disruptors, painters
	// roleEwarSpec is the EWAR family the hull's bonus line names. It is not a pilot
	// style: the specialty racks (fitgen.go ewarSpecialtyWishes) ask for the bonused
	// module before they settle for tank.
	roleEwarSpec modRole = "ewar.spec"
	roleTankLight   modRole = "tank.light"  // armour tank that is not a plate
	// roleSpeedMod is the kiter's low slot: overdrive, nanofiber, inertia stabiliser.
	// It is its own role because nothing else fits group 78 for speed.
	roleSpeedMod modRole = "speedmod"
	// roleTracking is a tracking computer (mid) or enhancer (low): turret reach for a
	// kiter. Its own role, like roleSpeedMod.
	roleTracking modRole = "tracking"
)

// baseRole is the plain role a styled role filters. Affinity, scoring, redundancy and
// fitState all work on the plain role, so a styled pick counts like a plain one.
func baseRole(r modRole) modRole {
	switch r {
	case roleWeaponKite, roleWeaponBrawl:
		return roleWeapon
	case roleSpeedMWD, roleSpeedAB:
		return roleSpeed
	case roleTackleLong:
		return roleTackle
	case roleEwarKite, roleEwarSpec:
		return roleEwar
	case roleTankLight:
		return roleTank
	}
	return r
}

// styleAdmits is the styled role's filter, applied on top of what the plain role
// requires. It is true for every plain role.
func styleAdmits(mm *marketModule, role modRole, h *HullProfile) bool {
	switch role {
	case roleWeaponKite, roleWeaponBrawl:
		want := styleBrawl
		if role == roleWeaponKite {
			want = styleKite
		}
		if mm.Style != want {
			return false
		}
		// A style chooses only between the hull's own guns. It must not put another empire's
		// turret, or an undersized one, ahead of a home gun: that is doctrine.
		if doctrineSizeFactor(mm, roleWeapon, h) < 1 {
			return false
		}
		switch h.Weapon {
		case WeaponEnergy, WeaponHybrid, WeaponProjectile, WeaponMissile:
			return weaponAffinity(h, mm.GroupID) >= doctrineHome
		}
		return true
	case roleSpeedMWD, roleSpeedAB:
		// The engine must fit the hull's own size, or the plain role decides: a 1MN
		// microwarpdrive on a cruiser is a worse engine than its own 10MN afterburner.
		if isMicrowarpdrive(mm) != (role == roleSpeedMWD) {
			return false
		}
		want := hullPropClass(h.Class)
		return want == 0 || propMassClass(mm) == want
	case roleTackleLong, roleEwarKite:
		return mm.Style == styleKite
	case roleEwarSpec:
		return h != nil && h.HasSpecialty(ewarSpecialtyByGroup[mm.GroupID])
	case roleTankLight:
		return mm.Style != styleBrawl
	case roleSpeedMod, roleTracking:
		return mm.Style == styleKite
	}
	return true
}

// speedModScore is the kiter's low-slot score: top-speed percent, with agility counted
// at a quarter because it buys the align, not the standoff.
func speedModScore(mm *marketModule) float64 {
	v := mm.Attrs[attrSpeedBonusPct]
	a := -mm.Attrs[attrAgilityPct]
	if v <= 0 && a <= 0 {
		return 0
	}
	return math.Max(v, 0) + math.Max(a, 0)*0.25
}

// ── the intent, applied to a wish list ──────────────────────────────────────

// wishListFor is wishList with the pilot's intent folded in. engageAuto returns
// wishList's own answer untouched (pinned in fitstyle_test.go).
func wishListFor(h *HullProfile, job string, lean combatLean, intent engageStyle) []slotWish {
	wishes := wishList(h, job, lean)
	if intent == engageAuto || !styledJob(job) {
		return wishes
	}
	return applyIntent(wishes, h, job, intent)
}

// styledJob reports the jobs a style shapes: the leanedJob set plus the EWAR seat.
func styledJob(job string) bool {
	return leanedJob(job) || job == JobEwar
}

// applyIntent rewrites a wish list for an intent. Pure. Each rewrite puts a styled role
// in front of the plain one in the same wish, so a market with nothing of the style
// still fills the slot.
func applyIntent(in []slotWish, h *HullProfile, job string, intent engageStyle) []slotWish {
	out := make([]slotWish, 0, len(in)+1)
	for _, w := range in {
		w.Roles = styledRoles(w, h, job, intent)
		out = append(out, w)
	}
	if intent == engageKite && (job == JobCombat || job == JobPiracy || job == JobEwar) {
		out = withSpeedMods(out, h)
	}
	return out
}

// styledRoles is one wish's role list under an intent.
func styledRoles(w slotWish, h *HullProfile, job string, intent engageStyle) []modRole {
	roles := make([]modRole, 0, len(w.Roles)+2)
	for _, r := range w.Roles {
		switch {
		case r == roleWeapon && (intent == engageKite || intent == engageHold):
			roles = append(roles, roleWeaponKite)
		case r == roleWeapon && intent == engageBrawl:
			roles = append(roles, roleWeaponBrawl)
		case r == roleSpeed && intent == engageKite:
			roles = append(roles, roleSpeedMWD)
		case r == roleSpeed && intent == engageBrawl:
			roles = append(roles, roleSpeedAB)
		case r == roleTackle && intent == engageKite:
			roles = append(roles, roleTackleLong)
		case r == roleEwar && intent == engageKite && job == JobEwar:
			// The EWAR seat: kite electronics first, then any EWAR, and a tracking computer
			// only when the market has none.
			roles = append(roles, roleEwarKite, r, roleTracking)
			continue
		case r == roleEwar && intent == engageKite:
			// A kiting gunboat: kite electronics first, then the module that extends turret
			// reach, then any EWAR.
			roles = append(roles, roleEwarKite, roleTracking)
		case r == roleEwar && intent == engageHold && job != JobEwar:
			// A holder's spare mid is a tracking computer before a jammer, to land long shots.
			roles = append(roles, roleTracking)
		case r == roleTank && intent == engageKite && h.Tank == TankArmor && w.Band == flagLoFirst:
			roles = append(roles, roleTankLight)
		}
		roles = append(roles, r)
	}
	return roles
}

// speedModMax is the most lows a kiter spends on legs.
const speedModMax = 2

// withSpeedMods carves the kiter's speed lows out of the first low wish after the
// damage control: at most speedModMax, and at most half of what is left of the rack.
func withSpeedMods(in []slotWish, h *HullProfile) []slotWish {
	for i, w := range in {
		if w.Band != flagLoFirst || w.Count < 2 || containsRole(w.Roles, roleDamageControl) ||
			containsRole(w.Roles, roleHarden) {
			continue
		}
		n := min(speedModMax, w.Count/2)
		if n < 1 {
			return in
		}
		out := make([]slotWish, 0, len(in)+1)
		out = append(out, in[:i]...)
		out = append(out, slotWish{Band: flagLoFirst, Roles: []modRole{roleSpeedMod}, Count: n})
		w.Count -= n
		out = append(out, w)
		return append(out, in[i+1:]...)
	}
	return in
}

func containsRole(roles []modRole, r modRole) bool {
	for _, x := range roles {
		if x == r {
			return true
		}
	}
	return false
}

// styleMatchBonus is what judgeFit adds to a finished fit whose guns match its pilot's
// intent. It is small next to a doctrine step (0.55), so it only breaks near-ties.
const styleMatchBonus = 1.10

// styleMatches reports whether a finished fit's lean is its pilot's intent.
func styleMatches(intent engageStyle, lean combatLean) bool {
	return intent != engageAuto && intent.lean() == lean
}

// pilotStyle is one pilot's style this epoch, for a caller holding only the character:
// its traits, through styleFor.
func (m *Mod) pilotStyle(ctx context.Context, charID int64, seed int64) engageStyle {
	traits, err := getTraits(ctx, m.db, charID)
	if err != nil || len(traits) == 0 {
		return engageAuto
	}
	return styleFor(resolveTuning(traits), seed)
}

// fitStyleLabel is the ESP readout of a fit's style: the pilot's intent where it has
// one, and what the guns came out as. It is empty for a fit with neither (a hauler).
func fitStyleLabel(intent engageStyle, lean combatLean) string {
	switch {
	case intent == engageAuto && lean == leanUnknown:
		return ""
	case intent == engageAuto:
		return lean.String()
	case lean == leanUnknown || intent.lean() == lean:
		return intent.String() + " (intent)"
	}
	return intent.String() + " intent, " + lean.String() + " guns"
}
