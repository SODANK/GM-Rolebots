package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
)

// Per-corporation division profiles: the war chest. The desks' own divisions are
// fixed (corpfactory.go, postables.go): Materials 1, Library 2, Production 3,
// Company Store 4, Trade 5, Starbase Kit 6, Fuel 7, read as constants by about
// 160 call sites. A profile adds the war chest: the Armory (doctrine kits), the
// Fleet Yard (barges and industrials lent for a mining op) and Spoils (loot
// handed in), each borrowed per corporation from a desk division it does not use
// and stored in rolebot_corp_divisions. Nothing is borrowed from 6 and 7. A human
// holds take on a war-chest division only when its own department owns it
// (humanGuardedTake); what it takes is signed out to it (corpsignout.go).
// See docs/comment-notes-b04.md#war-chest-divisions.

type divPurpose string

const (
	purArmory divPurpose = "armory"
	purYard   divPurpose = "yard"
	purSpoils divPurpose = "spoils"
)

// warChestOrder is the allocation order when there is not room for all three:
// the Armory carries the fleet, the Yard is the mining op's hauler, Spoils folds
// most happily.
var warChestOrder = []divPurpose{purArmory, purYard, purSpoils}

var warChestLabels = map[divPurpose]string{
	purArmory: "Armory",
	purYard:   "Fleet Yard",
	purSpoils: "Spoils",
}

// borrowableDivs are the desk divisions a war chest may borrow, in order of
// preference: the ones furthest from a combat outfit's own business first.
var borrowableDivs = []int{2, 3, 4, 5}

const (
	settingDivisionProfiles = "rolebots.corp.divisionProfiles"
	settingFleetArmory      = "rolebots.fleet.armory"
	settingFleetYard        = "rolebots.fleet.yard"
	settingLootSpoils       = "rolebots.loot.spoils"

	divProfileTTL = 10 * time.Minute
)

var (
	divisionProfilesOn atomic.Bool
	fleetArmoryOn      atomic.Bool
	fleetYardOn        atomic.Bool
	lootSpoilsOn       atomic.Bool
)

func init() {
	divisionProfilesOn.Store(true)
	fleetArmoryOn.Store(true)
	fleetYardOn.Store(true)
	lootSpoilsOn.Store(true)
	registerCorpForget("divisions", forgetDivProfile)
}

func (m *Mod) loadDivisionSettings(ctx context.Context) {
	for _, s := range []struct {
		key string
		v   *atomic.Bool
	}{
		{settingDivisionProfiles, &divisionProfilesOn},
		{settingFleetArmory, &fleetArmoryOn},
		{settingFleetYard, &fleetYardOn},
		{settingLootSpoils, &lootSpoilsOn},
	} {
		if v, ok, err := evedb.GetServerSetting(ctx, m.db, s.key); err == nil && ok {
			s.v.Store(truthySetting(v))
		}
	}
}

// divFlag is a corporation hangar division's entity flag: division 1 is flag
// 4, divisions 2-7 are flags 116-121.
func divFlag(div int) int64 {
	if div <= 1 {
		return 4
	}
	return int64(114 + div)
}

// flagDiv is divFlag's inverse; 0 for a flag that is no corporation division.
func flagDiv(flag int64) int {
	switch {
	case flag == 4:
		return 1
	case flag >= 116 && flag <= 121:
		return int(flag - 114)
	}
	return 0
}

// divTakeBit / divQueryBit are a division's hangar role bits.
func divTakeBit(div int) uint64  { return corp.RoleHangarCanTake1 << uint(div-1) }
func divQueryBit(div int) uint64 { return corp.RoleHangarCanQuery1 << uint(div-1) }

// divProfile is one corporation's war chest: which division each borrowed
// purpose lives in. A purpose missing from div has no division; the feature is
// off for it.
type divProfile struct {
	CorpID int64
	div    map[divPurpose]int
}

// Flag is the purpose's entity flag, false when the corporation has none.
func (p *divProfile) Flag(pur divPurpose) (int64, bool) {
	if p == nil {
		return 0, false
	}
	d, ok := p.div[pur]
	if !ok || d == 0 {
		return 0, false
	}
	return divFlag(d), true
}

// Div is the purpose's division number, 0 for none.
func (p *divProfile) Div(pur divPurpose) int {
	if p == nil {
		return 0
	}
	return p.div[pur]
}

// guardedDivs are the divisions holding any war-chest purpose, ascending.
func (p *divProfile) guardedDivs() []int {
	if p == nil {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, d := range p.div {
		if d > 0 && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Ints(out)
	return out
}

// guardedTakeBits is the take bits a human member must never hold in this
// corporation.
func (p *divProfile) guardedTakeBits() uint64 {
	var bits uint64
	for _, d := range p.guardedDivs() {
		bits |= divTakeBit(d)
	}
	return bits
}

// Label is the name a war-chest division carries: one purpose's label, or
// the folded ones joined ("Armory & Spoils"). Empty for a division that holds
// no war-chest purpose.
func (p *divProfile) Label(div int) string {
	if p == nil {
		return ""
	}
	var parts []string
	for _, pur := range warChestOrder {
		if p.div[pur] == div && div > 0 {
			parts = append(parts, warChestLabels[pur])
		}
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " & " + parts[len(parts)-1]
}

// divNeeds is what the allocator is told about one corporation.
type divNeeds struct {
	// Need is which war-chest purposes the corporation wants now.
	Need map[divPurpose]bool
	// Have is the stored borrow, purpose to division.
	Have map[divPurpose]int
	// DeskClaimed is a desk division the corporation uses: its desk needs it
	// (tags) or it holds stock that is not a war chest's. Never borrowed;
	// a war chest standing in one is evicted.
	DeskClaimed map[int]bool
	// HumanNamed is a division a human named: it is theirs, never borrowed.
	HumanNamed map[int]bool
	// Occupied is a division holding anything at all in any office.
	Occupied map[int]bool
}

// divMove is a war chest that must leave its division: everything in From
// moves to To (0 = Materials, when there is nowhere better).
type divMove struct {
	Purpose divPurpose
	From    int
	To      int
}

// assignDivisions decides the war chest. Pure. A borrow stands until a desk or a
// human claims its division; a needed purpose takes the first free borrowable
// division, else folds into another war-chest division, else has none. Pushed-out
// borrows are returned as moves. See docs/comment-notes-b04.md#war-chest-divisions.
func assignDivisions(in divNeeds) (map[divPurpose]int, []divMove) {
	claimed := func(d int) bool { return in.DeskClaimed[d] || in.HumanNamed[d] }
	out := map[divPurpose]int{}
	var moves []divMove
	evicted := map[divPurpose]int{}

	for _, pur := range warChestOrder {
		d := in.Have[pur]
		if d == 0 {
			continue
		}
		if claimed(d) {
			evicted[pur] = d
			continue
		}
		if !in.Need[pur] {
			if in.Occupied[d] {
				out[pur] = d
			}
			continue
		}
		out[pur] = d
	}

	used := func(d int) bool {
		for _, v := range out {
			if v == d {
				return true
			}
		}
		return false
	}
	place := func(pur divPurpose) int {
		for _, d := range borrowableDivs {
			if claimed(d) || used(d) || in.Occupied[d] {
				continue
			}
			return d
		}
		for _, host := range warChestOrder {
			if d := out[host]; d > 0 && host != pur {
				return d
			}
		}
		return 0
	}

	for _, pur := range warChestOrder {
		if _, ok := out[pur]; ok {
			continue
		}
		from, wasEvicted := evicted[pur]
		if !in.Need[pur] && !wasEvicted {
			continue
		}
		to := place(pur)
		if to > 0 && (in.Need[pur] || wasEvicted) {
			out[pur] = to
		}
		if wasEvicted {
			moves = append(moves, divMove{Purpose: pur, From: from, To: to})
		}
	}
	return out, moves
}

// legacyDeskNeeds is which desk divisions a corporation's tags put to use.
// Starbase 6 and 7 are never borrowed, so they need no entry. Pure.
func legacyDeskNeeds(tags []string) map[int]bool {
	out := map[int]bool{1: true}
	if hasIndustryDesk(tags) {
		out[2], out[3], out[4] = true, true, true
	}
	if tradeDeskTagged(tags) {
		out[5] = true
	}
	return out
}

// warChestNeeds is which war-chest purposes a corporation wants. Pure. Armory
// and Spoils follow the combat wing (every corporation fields one); the Yard
// follows the mining op, which only a player corporation runs.
func warChestNeeds(p CorpPolicy) map[divPurpose]bool {
	out := map[divPurpose]bool{}
	if doctrineFromTags(p.Tags) != DoctrineNone {
		if fleetArmoryOn.Load() {
			out[purArmory] = true
		}
		if lootSpoilsOn.Load() {
			out[purSpoils] = true
		}
	}
	if fleetYardOn.Load() {
		for _, a := range corpFleetActivities(p) {
			if a == ActMining {
				out[purYard] = true
			}
		}
	}
	return out
}

// ---- cache ----

type divProfileEntry struct {
	p  *divProfile
	at time.Time
}

var divProfiles sync.Map // corpID to divProfileEntry

func forgetDivProfile(corpID int64) { divProfiles.Delete(corpID) }

// corpDivs is a corporation's war chest. An empty profile (no borrow, every
// Flag false) when profiles are switched off or nothing is stored.
func (m *Mod) corpDivs(ctx context.Context, corpID int64) *divProfile {
	empty := &divProfile{CorpID: corpID, div: map[divPurpose]int{}}
	if corpID == 0 || m.db == nil || !divisionProfilesOn.Load() {
		return empty
	}
	if v, ok := divProfiles.Load(corpID); ok {
		e := v.(divProfileEntry)
		if clock.Now().Sub(e.at) < divProfileTTL {
			return e.p
		}
	}
	p, err := m.loadDivProfile(ctx, corpID)
	if err != nil {
		return empty
	}
	divProfiles.Store(corpID, divProfileEntry{p: p, at: clock.Now()})
	return p
}

// corpDivFlag is one purpose's flag for a corporation (false for none).
func (m *Mod) corpDivFlag(ctx context.Context, corpID int64, pur divPurpose) (int64, bool) {
	return m.corpDivs(ctx, corpID).Flag(pur)
}

func (m *Mod) loadDivProfile(ctx context.Context, corpID int64) (*divProfile, error) {
	p := &divProfile{CorpID: corpID, div: map[divPurpose]int{}}
	rows, err := m.db.QueryContext(ctx,
		`SELECT purpose, division FROM rolebot_corp_divisions WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pur string
		var d int
		if err := rows.Scan(&pur, &d); err != nil {
			return nil, err
		}
		if d >= 1 && d <= 7 {
			p.div[divPurpose(pur)] = d
		}
	}
	return p, rows.Err()
}

// ---- the CEO's pass ----

// divisionOccupancy is which divisions hold anything in any of the
// corporation's offices.
func (m *Mod) divisionOccupancy(ctx context.Context, corpID int64) (map[int]bool, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT e.flag
		  FROM entity e
		  JOIN crpoffices o ON o.itemID = e.locationID
		 WHERE o.corporationID = ? AND (e.flag = 4 OR e.flag BETWEEN 116 AND 121)`, corpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var f int64
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		if d := flagDiv(f); d > 0 {
			out[d] = true
		}
	}
	return out, rows.Err()
}

// divisionNames is the corporation's division1..7 as stored.
func (m *Mod) divisionNames(ctx context.Context, corpID int64) (map[int]string, error) {
	var n [7]sql.NullString
	err := m.db.QueryRowContext(ctx, `
		SELECT division1, division2, division3, division4, division5, division6, division7
		  FROM corporation WHERE corporationID = ?`, corpID).
		Scan(&n[0], &n[1], &n[2], &n[3], &n[4], &n[5], &n[6])
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	for i, v := range n {
		if v.Valid && v.String != "" {
			out[i+1] = v.String
		}
	}
	return out, nil
}

// moduleDivisionName reports whether a stored division name is one the module
// writes (any desk or war-chest label, folded or not): such a division is the
// module's to rename. Anything else a human chose.
func moduleDivisionName(name string) bool {
	if name == "" {
		return true
	}
	// The owner's tag is the module's too (corpdeptdivs.go): "[SEC] Armory".
	name = untagDivName(name)
	for _, n := range corpDivisionNames {
		if n == name {
			return true
		}
	}
	for _, n := range starbaseDivisionNames {
		if n == name {
			return true
		}
	}
	for _, pur := range warChestOrder {
		if strings.Contains(name, warChestLabels[pur]) {
			return true
		}
	}
	return false
}

// ensureCorpDivisions settles one corporation's war chest: places what it
// needs, carries an evicted borrow's stock to its new division, retires an
// empty one, and names the result. Runs at the top of the CEO's think, before
// any desk writes into a division.
func (m *Mod) ensureCorpDivisions(ctx context.Context, c *corpCEO, facts *CorpFacts) error {
	if !divisionProfilesOn.Load() || facts == nil {
		return nil
	}
	have, err := m.loadDivProfile(ctx, c.CorpID)
	if err != nil {
		return fmt.Errorf("divisions load: %w", err)
	}
	occ, err := m.divisionOccupancy(ctx, c.CorpID)
	if err != nil {
		return fmt.Errorf("divisions occupancy: %w", err)
	}
	names, err := m.divisionNames(ctx, c.CorpID)
	if err != nil {
		return fmt.Errorf("divisions names: %w", err)
	}

	borrowed := map[int]bool{}
	for _, d := range have.div {
		borrowed[d] = true
	}
	claimed := legacyDeskNeeds(facts.Policy.Tags)
	for d := range occ {
		if !borrowed[d] {
			claimed[d] = true // a desk's stock (or anyone's): not ours to borrow
		}
	}
	human := map[int]bool{}
	for d, n := range names {
		if !moduleDivisionName(n) {
			human[d] = true
		}
	}

	next, moves := assignDivisions(divNeeds{
		Need:        warChestNeeds(facts.Policy),
		Have:        have.div,
		DeskClaimed: claimed,
		HumanNamed:  human,
		Occupied:    occ,
	})

	for _, mv := range moves {
		to := mv.To
		if to == 0 {
			to = 1 // nowhere better: Materials, where donations always landed
		}
		n, err := m.moveDivisionStock(ctx, c.CorpID, divFlag(mv.From), divFlag(to))
		if err != nil {
			return fmt.Errorf("divisions move %s: %w", mv.Purpose, err)
		}
		m.ceoLog(ctx, c, powerRoles, "div.move", int64(to), float64(n),
			"moved the %s out of division %d into division %d (%d items): its desk needs the slot",
			warChestLabels[mv.Purpose], mv.From, to, n)
	}

	changed := len(moves) > 0
	for pur, d := range next {
		if have.div[pur] != d {
			changed = true
			if _, err := m.db.ExecContext(ctx, `
				INSERT INTO rolebot_corp_divisions (corporationID, purpose, division, assignedAt)
				VALUES (?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE division = VALUES(division), assignedAt = VALUES(assignedAt)`,
				c.CorpID, string(pur), d, clock.Unix()); err != nil {
				return fmt.Errorf("divisions save: %w", err)
			}
			m.ceoLog(ctx, c, powerRoles, "div.assign", int64(d), 0,
				"made division %d the %s", d, warChestLabels[pur])
		}
	}
	for pur := range have.div {
		if _, ok := next[pur]; !ok {
			changed = true
			if _, err := m.db.ExecContext(ctx,
				`DELETE FROM rolebot_corp_divisions WHERE corporationID = ? AND purpose = ?`,
				c.CorpID, string(pur)); err != nil {
				return fmt.Errorf("divisions retire: %w", err)
			}
		}
	}
	if changed {
		forgetDivProfile(c.CorpID)
	}
	p := &divProfile{CorpID: c.CorpID, div: next}
	m.nameWarChestDivisions(ctx, c.CorpID, facts.Name, p, names, human)
	return nil
}

// moveDivisionStock moves everything in one division to another across every
// office of the corporation. Ownership is unchanged; locks are by itemID and
// stay valid within the station.
func (m *Mod) moveDivisionStock(ctx context.Context, corpID, fromFlag, toFlag int64) (int64, error) {
	res, err := m.db.ExecContext(ctx, `
		UPDATE entity e
		  JOIN crpoffices o ON o.itemID = e.locationID
		   SET e.flag = ?
		 WHERE o.corporationID = ? AND e.flag = ?`, toFlag, corpID, fromFlag)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// nameWarChestDivisions names each war-chest division, and gives a division
// the war chest left back to its desk's name (or empty). A human's name is
// never touched.
func (m *Mod) nameWarChestDivisions(ctx context.Context, corpID int64, corpName string,
	p *divProfile, names map[int]string, human map[int]bool) {
	for _, d := range borrowableDivs {
		if human[d] {
			continue
		}
		want := taggedDivName(divisionArm(p, d), p.Label(d))
		cur := names[d]
		if want == "" {
			// Not a war chest (any more): a war-chest label left behind goes;
			// the desk's own naming (nameCorpDivisions) fills it back in.
			isWarChest := false
			for _, pur := range warChestOrder {
				if strings.Contains(cur, warChestLabels[pur]) {
					isWarChest = true
				}
			}
			if !isWarChest {
				continue
			}
		}
		if cur == want {
			continue
		}
		col := fmt.Sprintf("division%d", d)
		var newVal any = want
		if want == "" {
			newVal = nil
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE corporation SET `+col+` = ? WHERE corporationID = ?`, newVal, corpID); err != nil {
			continue
		}
		var old any
		if cur != "" {
			old = cur
		}
		pushCorpChanged(corpID, corpName, esp.CorpFieldChange{Column: col, Old: old, New: newVal})
	}
}

// stripGuardedTake removes the war-chest take bits a human member may not hold.
// Bots keep theirs: the quartermaster is a bot. Every module role write passes
// through here (setMemberRoles, applyBaseRoles).
func (m *Mod) stripGuardedTake(ctx context.Context, corpID, charID int64, roles uint64) uint64 {
	if corpID == 0 || m.isSynthetic(charID) {
		return roles
	}
	return roles &^ m.humanGuardedTake(ctx, corpID, charID)
}

// humanGuardedTake is the war-chest take bits a human member of corpID may not
// hold: every war-chest division except those its own department owns
// (Security the Armory, Mining the Fleet Yard, Sales the Spoils). A member the
// corporation barred for theft loses that exemption. What a member takes from
// the war chest is signed out to them (corpsignout.go).
func (m *Mod) humanGuardedTake(ctx context.Context, corpID, charID int64) uint64 {
	p := m.corpDivs(ctx, corpID)
	guarded := p.guardedTakeBits()
	if guarded == 0 {
		return 0
	}
	seat, ok := armOf(charID, corpID)
	if !ok {
		return guarded
	}
	own := ownedTakeBits(p, seat.Arm) & guarded
	if own == 0 {
		return guarded
	}
	if barred, why := m.kitBarred(ctx, corpID, charID); barred && why == "theft" {
		return guarded
	}
	return guarded &^ own
}

// ownedTakeBits is the take bits of every division an arm owns. Pure.
func ownedTakeBits(p *divProfile, a corpArm) uint64 {
	var bits uint64
	for _, d := range armDivisions(p, a) {
		bits |= divTakeBit(d)
	}
	return bits
}

// corpRoleFilter is the core's role filter (db.CorpRoleFilter): every
// effective-role read strips a human member's take on the war chest of a
// bot-run corporation outside its own department, so a role a human Director
// granted themselves, or a title carrying one, does not open the Armory.
func (m *Mod) corpRoleFilter(corpID, charID int64, roles uint64) uint64 {
	if corpID == 0 || charID == 0 || !divisionProfilesOn.Load() {
		return roles
	}
	if _, managed := managedCEO(corpID); !managed {
		return roles
	}
	if m.isSynthetic(charID) {
		return roles
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return roles &^ m.humanGuardedTake(ctx, corpID, charID)
}
