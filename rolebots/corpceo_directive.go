package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"gurimoon/esp"

	evedb "gurimoon/db"
)

// Two powers that describe the corporation rather than spend anything: identity
// (its tags, and the member limit its CEO's skills allow) and the directive (how
// much of its effort each kind of work should get). Plus the member's side of the
// directive: the multiplier it puts on each activity in scoreActivities.

const (
	// ceoDirectiveFloorSec: a CEO does not change its directive more often than
	// this. The charter only moves when traits do, so it bites an operator dragging
	// sliders; a flickering directive would be read by every member on every pass.
	ceoDirectiveFloorSec = 12 * 3600
	// ceoDirectiveEpsilon: two directives within this on every share are the
	// same directive.
	ceoDirectiveEpsilon = 0.05

	// directiveGain scales how far a directive bends a member's appetite
	// (directiveTerm). For a neutral CEO and member, the headline activity of a
	// 50/30/20 directive over six activities is lifted about 1.5x, others damped.
	directiveGain = 1.5
	directiveMin  = 0.5
	directiveMax  = 2.5

	// directiveMemoSec is how long a corporation's directive is held in
	// memory before it is read again.
	directiveMemoSec = 600
)

// ── identity ────────────────────────────────────────────────────────────────

// sameTags compares two tag lists in order. The order is meaning: the charter
// stores tags best-appeal first and the SCC sector is read off the first one
// (corpSectorFromTags). An order-blind comparison would leave a stale lead tag in
// place when only the ranking changed.
func sameTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ceoIdentity writes the corporation's tags from the charter and refreshes the
// member limit from the CEO's own skills (the corp window's refresh abilities
// button), which a player CEO forgets and a bot never should.
func (m *Mod) ceoIdentity(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter) error {
	if len(ch.Tags) > 0 && !sameTags(facts.Policy.Tags, ch.Tags) && !m.humanHoldOn(ctx, c.CorpID, cmdKindTags) {
		p := facts.Policy
		p.CorpID = c.CorpID
		old := p.TagCSV()
		p.Tags = append([]string(nil), ch.Tags...)
		if err := saveCorpPolicy(ctx, m.db, &p); err != nil {
			return err
		}
		// facts is the think's cached row (thinkctx.go): the roles pass later in this same think
		// reads its tags for the departments and fleet seats, so it must see the new ones.
		facts.Policy.Tags = append([]string(nil), p.Tags...)
		if old == "" {
			old = "nothing"
		}
		m.ceoLog(ctx, c, powerIdentity, "tags", c.CorpID, 0,
			"declared %s a %s outfit (was %s)", facts.Name, strings.Join(ch.Tags, "/"), old)
	}

	oldLimit, newLimit, oldRaces, newRaces, err := evedb.RecomputeCorpAbilities(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	m.noteAbilityChange(ctx, c, facts.Name, oldLimit, newLimit, oldRaces, newRaces)
	return nil
}

// noteAbilityChange logs a member limit or race change RecomputeCorpAbilities made and
// tells the members' clients. Shared by the CEO's think and the openings sweep
// (corpopenings.go), which refreshes the limit while the CEO is logged off.
func (m *Mod) noteAbilityChange(ctx context.Context, c *corpCEO, name string, oldLimit, newLimit, oldRaces, newRaces int64) {
	if oldLimit == newLimit && oldRaces == newRaces {
		return
	}
	var changes []esp.CorpFieldChange
	if oldLimit != newLimit {
		changes = append(changes, esp.CorpFieldChange{Column: "memberLimit", Old: oldLimit, New: newLimit})
		verb := "raised"
		if newLimit < oldLimit {
			// A successor with less Corporation Management lowers the limit; members
			// already over it keep their places.
			verb = "lowered"
		}
		m.ceoLog(ctx, c, powerIdentity, "limit", c.CorpID, float64(newLimit),
			"%s the member limit of %s from %d to %d", verb, name, oldLimit, newLimit)
	}
	if oldRaces != newRaces {
		changes = append(changes, esp.CorpFieldChange{Column: "allowedMemberRaceIDs", Old: oldRaces, New: newRaces})
		m.ceoLog(ctx, c, powerIdentity, "races", c.CorpID, float64(newRaces),
			"opened %s to %s", name, playerCorpRaceNames(newRaces))
	}
	pushCorpChanged(c.CorpID, name, changes...)
}

// pushCorpChanged sends an OnCorporationChanged for columns a CEO changed,
// through the hook the ESP corporation editor uses. The name rides every push
// unchanged: a change dict with all-nil entries on one side reads as an add or
// remove (GetAddRemoveFromChange), and the name pins it to the update branch.
func pushCorpChanged(corpID int64, name string, changes ...esp.CorpFieldChange) {
	if esp.CorpChangedHook == nil || len(changes) == 0 {
		return
	}
	out := append([]esp.CorpFieldChange{{Column: "corporationName", Old: name, New: name}}, changes...)
	esp.CorpChangedHook(corpID, out)
}

// ── directive ───────────────────────────────────────────────────────────────

// loadCorpDirective reads a corporation's stored directive and when it was
// last set.
func (m *Mod) loadCorpDirective(ctx context.Context, corpID int64) (map[string]float64, int64, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT activity, weight, setAt FROM rolebot_corp_directive WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil, 0, fmt.Errorf("loadCorpDirective: %w", err)
	}
	defer rows.Close()
	out := map[string]float64{}
	var setAt int64
	for rows.Next() {
		var a string
		var w float64
		var at int64
		if err := rows.Scan(&a, &w, &at); err != nil {
			return nil, 0, err
		}
		out[a] = w
		if at > setAt {
			setAt = at
		}
	}
	return out, setAt, rows.Err()
}

func sameDirective(a, b map[string]float64) bool {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for k := range keys {
		if math.Abs(a[k]-b[k]) > ceoDirectiveEpsilon {
			return false
		}
	}
	return true
}

// directiveText renders a directive the way a person would say it, biggest
// share first.
func directiveText(d map[string]float64) string {
	type kv struct {
		k string
		v float64
	}
	var list []kv
	for k, v := range d {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
	parts := make([]string, 0, len(list))
	for _, e := range list {
		label := e.k
		if def, ok := activityDefFor(e.k); ok {
			label = def.Label
		}
		parts = append(parts, fmt.Sprintf("%s %.0f%%", label, 100*e.v))
	}
	if len(parts) == 0 {
		return "no directive"
	}
	return strings.Join(parts, ", ")
}

// ceoDirective publishes the charter's directive to the members.
func (m *Mod) ceoDirective(ctx context.Context, c *corpCEO, ch Charter) error {
	// The chair's own directive outranks the CEO's leaning while it is held
	// (corpcommanddesk.go): the table is theirs until the hold runs out.
	if _, hold := m.humanDirectiveHold(ctx, c.CorpID); hold > 0 {
		return nil
	}
	cur, setAt, err := m.loadCorpDirective(ctx, c.CorpID)
	if err != nil {
		return err
	}
	// A war puts the directive on a footing (corpceo_war.go): a fighting outfit
	// turns out to fight, anyone else hunkers down. The floor does not hold it back.
	want, reason := ch.Directive, "the CEO's own leaning"
	footing, fight, footWhy := m.ceoWarFooting(ctx, c, ch)
	if footing > 0 {
		want = warFooting(ch.Directive, footing, fight)
		reason = "war footing: " + footWhy
	}
	wasWar := strings.HasPrefix(m.corpDirectiveReason(ctx, c.CorpID), "war footing")
	if sameDirective(cur, want) {
		return nil
	}
	if len(cur) > 0 && clock.Unix()-setAt < ceoDirectiveFloorSec && wasWar == (footing > 0) {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM rolebot_corp_directive WHERE corporationID = ?`, c.CorpID); err != nil {
		return fmt.Errorf("ceoDirective clear: %w", err)
	}
	now := clock.Unix()
	for act, w := range want {
		if w <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rolebot_corp_directive (corporationID, activity, weight, reason, setAt)
			VALUES (?, ?, ?, ?, ?)`,
			c.CorpID, act, w, truncate(reason, 255), now); err != nil {
			return fmt.Errorf("ceoDirective write: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	forgetCorpDirective(c.CorpID)
	if footing > 0 {
		m.ceoLog(ctx, c, powerDirective, "directive", c.CorpID, footing,
			"put the corporation on a war footing (%s): %s", footWhy, directiveText(want))
	} else {
		m.ceoLog(ctx, c, powerDirective, "directive", c.CorpID, 0,
			"set the corporation's directive: %s", directiveText(want))
	}
	return nil
}

// corpDirectiveReason is why the stored directive was set, "" when there is
// none.
func (m *Mod) corpDirectiveReason(ctx context.Context, corpID int64) string {
	var r string
	_ = m.db.QueryRowContext(ctx,
		`SELECT reason FROM rolebot_corp_directive WHERE corporationID = ? LIMIT 1`, corpID).Scan(&r)
	return r
}

// ── the member's side ───────────────────────────────────────────────────────

type directiveMemo struct {
	weights  map[string]float64
	strength float64
	at       int64
}

var (
	directiveMu    sync.Mutex
	directiveCache = map[int64]directiveMemo{}
)

func forgetCorpDirective(corpID int64) {
	directiveMu.Lock()
	delete(directiveCache, corpID)
	directiveMu.Unlock()
}

// corpDirectiveFor is a member's view of its corporation's directive: the
// shares and how hard the chair pushes them. Memoised per corporation, since
// every member reads it on every planning pass. ok is false for a corporation
// with no directive (a bot CEO's is its charter's, else the chair's, corpcommanddesk.go).
func (m *Mod) corpDirectiveFor(ctx context.Context, corpID int64) (map[string]float64, float64, bool) {
	if corpID == 0 || m.db == nil {
		return nil, 0, false
	}
	ceoID, npc, chaired := chairOf(corpID)
	now := clock.Unix()
	directiveMu.Lock()
	memo, hit := directiveCache[corpID]
	directiveMu.Unlock()
	if hit && now-memo.at < directiveMemoSec {
		return memo.weights, memo.strength, len(memo.weights) > 0
	}
	weights, _, err := m.loadCorpDirective(ctx, corpID)
	if err != nil {
		return nil, 0, false
	}
	strength := 1.0
	if chaired {
		if traits, err := getTraits(ctx, m.db, ceoID); err == nil {
			strength = resolveTuning(traits).CommandDrive
		}
		// An NPC chair pushes less than a capsuleer CEO (npcchair.go).
		if npc {
			strength *= npcChairPush
		}
	}
	// The chair's push, when the chair set it.
	if hs, hold := m.humanDirectiveHold(ctx, corpID); hold > 0 && hs > 0 {
		strength = hs
	}
	directiveMu.Lock()
	directiveCache[corpID] = directiveMemo{weights: weights, strength: strength, at: now}
	directiveMu.Unlock()
	return weights, strength, len(weights) > 0
}

// directiveMultiplier is the arithmetic of the directive's pull on one activity:
// 1 plus how far its share sits above or below an even split across the
// catalogue, scaled by the CEO's push and the member's obedience, clamped so no
// directive makes work impossible or irresistible.
func directiveMultiplier(weights map[string]float64, slug string, strength, obedience float64) float64 {
	if len(weights) == 0 {
		return 1
	}
	mean := 1.0 / float64(len(activityCatalog))
	v := 1 + directiveGain*strength*obedience*(weights[slug]-mean)
	if v < directiveMin {
		return directiveMin
	}
	if v > directiveMax {
		return directiveMax
	}
	return v
}

// directiveTerm is the multiplier a member's corporation directive puts on one
// activity (appetite.go scoreActivities). Exactly 1 for a pilot whose corporation
// has no managed CEO. Obedience is Tuning.Obedience. The CEO is not bound by its
// own directive: it wrote it from its leanings, so its appetites already agree.
func (m *Mod) directiveTerm(ctx context.Context, b *botRun, slug string) float64 {
	if b == nil || b.CorpID == 0 {
		return 1
	}
	if ceoID, ok := managedCEO(b.CorpID); ok && ceoID == b.CharID {
		return 1
	}
	weights, strength, ok := m.corpDirectiveFor(ctx, b.CorpID)
	if !ok {
		return 1
	}
	return directiveMultiplier(weights, slug, strength, b.Tune.Obedience)
}

func init() { registerCorpForget("directiveCache", forgetCorpDirective) }
