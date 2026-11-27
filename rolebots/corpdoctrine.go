package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// Doctrine is what an employer's combat wing may do, from its tags (corps.go).
// Factions share one table; rivalry is standingToPilot and roe.go.
// See docs/comment-notes-b04.md#corp-combat-doctrine.
type Doctrine uint8

const (
	// DoctrineNone has no guns worth speaking of (a school, a ministry, a newsroom).
	DoctrineNone Doctrine = iota
	// DoctrineDefence is an industrial employer's own security: it clears its
	// miners' belts, covers its haulers and runs off rivals on its ground. It does
	// not go looking or leave home.
	DoctrineDefence
	// DoctrineSecurity is a navy, customs office or police force: it patrols, holds
	// checkpoints, runs down pilots with a price and fights rivals it meets where the
	// shot is no crime (a flag, an outlaw, a war, or nullsec). It never raids, never
	// shoots neutrals and never commits a crime, lowsec included (roeAllows).
	DoctrineSecurity
	// DoctrineMercenary is guns for hire: everything Security may do, plus roaming
	// into other space to find work and an appetite for worthwhile neutrals.
	DoctrineMercenary
	// DoctrinePredation is a cartel. It keeps ActPiracy (ransom, fencing, criminal
	// timers, the CONCORD stopwatch), the only lane that commits crimes in policed space.
	DoctrinePredation
)

func (d Doctrine) String() string {
	switch d {
	case DoctrineDefence:
		return "defence"
	case DoctrineSecurity:
		return "security"
	case DoctrineMercenary:
		return "mercenary"
	case DoctrinePredation:
		return "predation"
	}
	return "none"
}

// Label is the panel's prose.
func (d Doctrine) Label() string {
	switch d {
	case DoctrineDefence:
		return "Defence"
	case DoctrineSecurity:
		return "Security"
	case DoctrineMercenary:
		return "Mercenary"
	case DoctrinePredation:
		return "Predation"
	}
	return "None"
}

// Blurb is the panel's one-line description of what this wing does.
func (d Doctrine) Blurb() string {
	switch d {
	case DoctrineDefence:
		return "Clears its own belts and covers its own haulers. Never leaves home ground."
	case DoctrineSecurity:
		return "Patrols, holds checkpoints, runs down marks. Fights rivals only where it is no crime; does not raid and does not shoot neutrals."
	case DoctrineMercenary:
		return "Goes looking. Roams rival space and will take on a neutral worth the trouble."
	case DoctrinePredation:
		return "A cartel. Keeps the piracy lane whole — robbery, ransom, criminal timers."
	}
	return "No combat wing."
}

// Raids reports whether this doctrine mounts operations into somebody else's
// home region rather than fighting what it meets on its own beat. It is the
// only thing separating Security from Mercenary.
func (d Doctrine) Raids() bool {
	return d == DoctrineMercenary || d == DoctrinePredation
}

// Lawful reports whether this doctrine keeps within the law wherever there is one: a navy,
// police force or an employer's own guard never opens fire where the shot would be a crime
// (roeAllows). Mercenaries and cartels accept the record for the work.
func (d Doctrine) Lawful() bool {
	return d == DoctrineSecurity || d == DoctrineDefence
}

// NeutralMul scales the neutral-engagement nerve roll (roe.go). Zero means a
// neutral is never a target: a navy opening on unaligned lowsec traffic would be
// a terrorist organisation, and a defence wing only shoots what stands on its
// ground.
func (d Doctrine) NeutralMul() float64 {
	switch d {
	case DoctrineMercenary:
		return 1.0
	case DoctrinePredation:
		return 1.5
	}
	return 0
}

// baseWingFrac is the fraction of a corporation's roster that may be on a
// combat activity at once, before the industrial cap below.
func (d Doctrine) baseWingFrac() float64 {
	switch d {
	case DoctrineDefence:
		return 0.15
	case DoctrineSecurity:
		return 0.40
	case DoctrineMercenary:
		return 0.60
	case DoctrinePredation:
		return 0.80
	}
	return 0
}

// industrialWingMul is what a factory's guns are worth against a dedicated
// outfit's. It applies only to the three doctrines that could field a serious
// wing; Defence's 0.15 is already the industrial answer.
const industrialWingMul = 0.30

// The three tags that decide what a wing does.
const (
	tagOutlaw    = "outlaw"
	tagCombat    = "combat"
	tagSecurity  = "security"
	tagMercenary = "mercenary"
)

// doctrineFieldTags is the work that puts a hull somewhere it can be shot at,
// and so earns a defensive wing. Research, biotech, education, civil service,
// finance and media are absent: they have no ships in space to defend.
var doctrineFieldTags = map[string]bool{
	"mining": true, "industry": true, "shipbuilding": true,
	"construction": true, "chemical": true, "trade": true,
	"retail": true, "hauling": true, "food": true,
	"cartelindustry": true,
}

// doctrineIndustrialTags is what makes a corporation a factory for capping its
// wing. It is narrower than doctrineFieldTags: hauling, retail and trade move
// goods rather than make them, so their security is not docked.
var doctrineIndustrialTags = map[string]bool{
	"mining": true, "industry": true, "shipbuilding": true,
	"construction": true, "chemical": true, "cartelindustry": true,
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func hasAnyTag(tags []string, set map[string]bool) bool {
	for _, t := range tags {
		if set[t] {
			return true
		}
	}
	return false
}

// doctrineFromTags is the whole classification. A corporation with several
// violence tags is read at its most committed (outlaw beats bare combat beats
// security); whether it can afford to act on that is the wing cap's question.
// Resolution order matters.
func doctrineFromTags(tags []string) Doctrine {
	switch {
	case hasTag(tags, tagOutlaw):
		return DoctrinePredation
	case (hasTag(tags, tagCombat) || hasTag(tags, tagMercenary)) && !hasTag(tags, tagSecurity):
		// Guns for hire with no uniform, and the explicit mercenary tag. Security still
		// wins: a contractor that also polices keeps a navy's restraint.
		return DoctrineMercenary
	case hasTag(tags, tagSecurity):
		return DoctrineSecurity
	case hasAnyTag(tags, doctrineFieldTags):
		return DoctrineDefence
	}
	return DoctrineNone
}

// wingFracFromTags is the fraction of a corporation's roster that may be on a
// combat activity simultaneously.
func wingFracFromTags(tags []string) float64 {
	d := doctrineFromTags(tags)
	frac := d.baseWingFrac()
	if frac == 0 {
		return 0
	}
	if d != DoctrineDefence && hasAnyTag(tags, doctrineIndustrialTags) {
		frac *= industrialWingMul
	}
	return frac
}

// ── the lookup ──────────────────────────────────────────────────────────────

type corpDoctrineEntry struct {
	doctrine Doctrine
	wingFrac float64
	known    bool
}

var (
	corpDoctrineMu    sync.RWMutex
	corpDoctrineCache = map[int64]corpDoctrineEntry{}
)

// invalidateCorpDoctrine drops one corporation's memoised classification, or
// every one for corpID 0. The tags are the doctrine, so an edited tag list must
// not keep its old answer.
func invalidateCorpDoctrine(corpID int64) {
	corpDoctrineMu.Lock()
	defer corpDoctrineMu.Unlock()
	if corpID == 0 {
		corpDoctrineCache = map[int64]corpDoctrineEntry{}
		return
	}
	delete(corpDoctrineCache, corpID)
}

// corpDoctrineOf answers a corporation's doctrine, its wing fraction, and
// whether it is classified at all. An unclassified player corporation (no
// rolebot_corps row) has its wing cap skipped; DoctrineNone is a real answer
// (a school has no wing).
func (m *Mod) corpDoctrineOf(ctx context.Context, corpID int64) (Doctrine, float64, bool) {
	if corpID == 0 {
		return DoctrineNone, 0, false
	}
	corpDoctrineMu.RLock()
	e, ok := corpDoctrineCache[corpID]
	corpDoctrineMu.RUnlock()
	if ok {
		return e.doctrine, e.wingFrac, e.known
	}

	var raw string
	err := m.db.QueryRowContext(ctx,
		"SELECT COALESCE(tags, '') FROM rolebot_corps WHERE corporationID = ?",
		corpID).Scan(&raw)
	if err != nil {
		// A transient database error is not cached. A corporation with no row is cached
		// as unclassified; saveCorpPolicy, which writes the row, invalidates it.
		if errors.Is(err, sql.ErrNoRows) {
			corpDoctrineMu.Lock()
			corpDoctrineCache[corpID] = corpDoctrineEntry{doctrine: DoctrineNone, known: false}
			corpDoctrineMu.Unlock()
		}
		return DoctrineNone, 0, false
	}
	tags := parseTagList(raw)
	e = corpDoctrineEntry{
		doctrine: doctrineFromTags(tags),
		wingFrac: wingFracFromTags(tags),
		known:    true,
	}
	corpDoctrineMu.Lock()
	corpDoctrineCache[corpID] = e
	corpDoctrineMu.Unlock()
	return e.doctrine, e.wingFrac, true
}

// corpDoctrineFor is the doctrine alone, for callers that do not price a wing.
// An unclassified employer answers DoctrineDefence: it spares neutrals and does
// not raid, but fights declared enemies.
// See docs/comment-notes-b04.md#doctrine-default.
func (m *Mod) corpDoctrineFor(ctx context.Context, corpID int64) Doctrine {
	d, _, known := m.corpDoctrineOf(ctx, corpID)
	if !known {
		return DoctrineDefence
	}
	return d
}

func init() { registerCorpForget("corpDoctrineCache", invalidateCorpDoctrine) }
