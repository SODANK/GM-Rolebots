package rolebots

import (
	"context"
	"database/sql"
	"fmt"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Corporation titles: the name of the position a member holds.
//
// A title is derived from a position something else already decided (the chair, an
// officer's role bit, a fleet seat, a department, an arm's rung or a merit rank), so
// whatever grants or removes the position re-syncs the title. What it is called is the
// corporation's vocabulary (corptitlevocab.go). Storage is the game's own: eight title
// slots per corporation (crpTitles), a member's titleMask, and character_.title.
// A title never carries role masks. The module writes only slots and text it owns.
// See docs/comment-notes-b06.md#corp-titles.

// titleSlot is one fixed position to slot mapping, in priority order: a member holding
// several positions is shown by the first one it holds.
type titleSlot struct {
	ID   int64
	Kind string
}

const (
	titleKindCEO       = "ceo"
	titleKindDirector  = "director"
	titleKindPersonnel = "personnel"
	titleKindTreasurer = "treasurer"
	titleKindMining    = "mining"
	titleKindFactory   = "factory"
	titleKindCombat    = "combat"
)

// Slot 1<<7 is reserved and unclaimed; migrateTradeConvoys resets a leftover Convoy
// Master title to the default name.
var titleSlots = []titleSlot{
	{1 << 0, titleKindCEO},
	{1 << 1, titleKindDirector},
	{1 << 2, titleKindPersonnel},
	{1 << 3, titleKindTreasurer},
	{1 << 4, titleKindMining},
	{1 << 5, titleKindFactory},
	{1 << 6, titleKindCombat},
}

// titleSlotConvoy is the retired slot, named here only for the migration.
const titleSlotConvoy = int64(1 << 7)

// defaultTitleName is the name EnsureDefaultTitles seeds a slot with.
func defaultTitleName(id int64) string {
	for i := 0; i < 8; i++ {
		if int64(1)<<i == id {
			return fmt.Sprintf("Title %d", i+1)
		}
	}
	return ""
}

// seatTitleKind maps a fleet seat's activity to its title.
func seatTitleKind(activity string) string {
	switch activity {
	case ActMining, ActIndMining:
		return titleKindMining
	case ActCombat, ActSecurity, ActPiracy:
		return titleKindCombat
	}
	return ""
}

// industryTagged reports whether a corporation's tags make it a factory outfit, the
// ones that get a Factory Overseer.
func industryTagged(tags []string) bool {
	for _, t := range tags {
		switch t {
		case "industry", "shipbuilding", "research", "cartelindustry":
			return true
		}
	}
	return false
}

// claimable is the ownership rule for one slot.
func claimable(cur string, id int64, ownedName string, owned bool) bool {
	if cur == "" || cur == defaultTitleName(id) {
		return true
	}
	return owned && cur == ownedName
}

// titlesManaged reports whether the module keeps this corporation's titles:
// every corporation with a bot CEO, plus any the operator opted in.
func (m *Mod) titlesManaged(ctx context.Context, corpID int64) bool {
	if _, ok := managedCEO(corpID); ok {
		return true
	}
	var on int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(manageTitles, 0) FROM rolebot_corps WHERE corporationID = ?`, corpID).Scan(&on)
	return on != 0
}

// syncCorpTitles brings a corporation's titles in line with its positions.
// Best effort throughout: a title is presentation, and a failure here must
// never fail the pass that called it.
func (m *Mod) syncCorpTitles(ctx context.Context, corpID int64) {
	if !m.titlesManaged(ctx, corpID) {
		return
	}
	if _, err := m.syncCorpTitlesErr(ctx, corpID); err != nil {
		rlogf("rolebots: title sync corp %d: %v", corpID, err)
	}
}

// ── the plan ────────────────────────────────────────────────────────────────

// titleSlotClaim is one slot: what it reads, whether the module may name it,
// and what it should read.
type titleSlotClaim struct {
	Slot     titleSlot
	Row      *evedb.TitleRow
	Cur      string
	PrevName string // the name the module last wrote there
	Owned    bool   // a rolebot_corp_titles row exists
	Claim    bool   // the module may name it
	Want     string
}

// titleSlotClaims reads every slot. Read-only.
func (m *Mod) titleSlotClaims(ctx context.Context, corpID int64, set *corpTitleSet) ([]titleSlotClaim, error) {
	owned := map[int64]string{}
	if rows, err := m.db.QueryContext(ctx,
		`SELECT titleID, name FROM rolebot_corp_titles WHERE corporationID = ?`, corpID); err == nil {
		for rows.Next() {
			var id int64
			var name string
			if rows.Scan(&id, &name) == nil {
				owned[id] = name
			}
		}
		rows.Close()
	}
	out := make([]titleSlotClaim, 0, len(titleSlots))
	for _, slot := range titleSlots {
		row, err := evedb.GetTitleRow(ctx, m.db, corpID, slot.ID)
		if err != nil {
			return nil, err
		}
		c := titleSlotClaim{Slot: slot, Row: row, Want: set.Name(slot.Kind)}
		if row != nil {
			c.Cur = row.TitleName
		}
		c.PrevName, c.Owned = owned[slot.ID]
		c.Claim = claimable(c.Cur, slot.ID, c.PrevName, c.Owned)
		out = append(out, c)
	}
	return out, nil
}

// memberTitle is one member's title, as it is and as it should be.
type memberTitle struct {
	CharID int64
	Name   string
	Bot    bool

	Cur     string // character_.title now
	CurMask uint64

	Key      string // the position it holds ("" for none)
	Want     string // what that position is called here
	WantMask uint64

	// Writable: the text is the module's to replace (empty, what the module
	// last wrote, or an earlier build's name). False for a title somebody
	// typed.
	Writable bool
	// PrevKey is the position its current text stands for, when the module
	// knows: the recorded key, or the legacy name's.
	PrevKey string
	// Recorded is the rolebot_title_text row, if any.
	RecText, RecKey string
	Recorded        bool
}

// Stale reports whether a sync would change this member's text.
func (t memberTitle) Stale() bool { return t.Writable && t.Cur != t.Want }

// Typed reports whether the member wears a title somebody typed in place of
// the module's.
func (t memberTitle) Typed() bool { return !t.Writable && t.Cur != t.Want }

// corpTitlePlan decides every member's position and title. Read-only: the
// sync writes from it and the corporation page reads it, so the two cannot
// disagree.
func (m *Mod) corpTitlePlan(ctx context.Context, corpID int64, facts *CorpFacts, set *corpTitleSet,
	claims []titleSlotClaim) ([]memberTitle, uint64, error) {
	var ownedMask uint64
	slotName := map[string]bool{}
	for _, c := range claims {
		if c.Claim {
			ownedMask |= uint64(c.Slot.ID)
			slotName[c.Slot.Kind] = true
		}
	}

	holders, err := m.titleHolders(ctx, corpID, facts)
	if err != nil {
		return nil, 0, err
	}

	// A bot CEO leads its human members too (corpmembers.go), so they are titled like
	// anyone else; in an opted-in corporation only the roster is.
	humans := 0
	if leadsHumans(corpID) {
		humans = 1
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID, ch.characterName, COALESCE(ch.title, ''), COALESCE(r.titleMask, 0),
		       CASE WHEN b.characterID IS NULL THEN 0 ELSE 1 END,
		       t.characterID IS NOT NULL, COALESCE(t.text, ''), COALESCE(t.titleKey, '')
		  FROM character_ ch
		  LEFT JOIN rolebot_characters b ON b.characterID = ch.characterID
		  LEFT JOIN chrCorporationRoles r ON r.characterID = ch.characterID
		  LEFT JOIN rolebot_title_text t ON t.characterID = ch.characterID
		 WHERE ch.corporationID = ? AND ch.accountID > 0
		   AND (b.characterID IS NOT NULL OR ? = 1)
		 ORDER BY ch.characterName`, corpID, humans)
	if err != nil {
		return nil, 0, fmt.Errorf("title members: %w", err)
	}
	var out []memberTitle
	for rows.Next() {
		var mt memberTitle
		var bot int
		if rows.Scan(&mt.CharID, &mt.Name, &mt.Cur, &mt.CurMask, &bot,
			&mt.Recorded, &mt.RecText, &mt.RecKey) == nil {
			mt.Bot = bot != 0
			out = append(out, mt)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("title members: %w", err)
	}

	staffRows := m.loadStaffRows(ctx, corpID)
	posSeats := m.posSeatsOf(ctx, corpID)
	indSeats := m.indSeatsOf(ctx, corpID)
	// A member with no position wears its merit rank (corpmerit.go), in a
	// corporation a bot CEO runs, with the ladder on.
	var ranks map[int64]int
	if leadsHumans(corpID) {
		ranks = m.meritRanksOf(ctx, corpID)
	}

	for i := range out {
		mt := &out[i]
		for _, slot := range titleSlots {
			if ownedMask&uint64(slot.ID) == 0 || !holders[slot.Kind][mt.CharID] {
				continue
			}
			mt.WantMask |= uint64(slot.ID)
			if mt.Key == "" {
				mt.Key = slot.Kind
			}
		}
		// A department head wears its department; the slot it holds is
		// still the plain Director.
		if mt.Key == titleKindDirector {
			if k := headKeyOfPortfolio(staffRows[mt.CharID].Portfolio); k != "" {
				mt.Key = k
			}
		}
		// The starbase and Industry seats wear their seat when they hold no
		// slot of their own.
		if mt.Key == "" {
			mt.Key = seatTitleKey(posSeats[mt.CharID])
		}
		if mt.Key == "" {
			mt.Key = seatTitleKey(indSeats[mt.CharID].Seat)
		}
		if mt.Key == "" {
			if s, ok := armOf(mt.CharID, corpID); ok {
				// Every arm member wears a rung, the bottom one with the
				// merit ladder off: "every bot is flagged".
				if _, has := titleLadderPools[rankLadderKey(s.Arm)]; has {
					mt.Key = rankTitleKey(s.Arm, ranks[mt.CharID])
				}
			} else if ranks != nil {
				if r, ok := ranks[mt.CharID]; ok {
					mt.Key = meritTitleKey(r)
				}
			}
		}
		if mt.Key != "" {
			mt.Want = set.Name(mt.Key)
		}

		legacyKey, legacy := legacyTitleKeys[mt.Cur]
		switch {
		case mt.Cur == "":
			mt.Writable = true
		case mt.Recorded && mt.Cur == mt.RecText:
			mt.Writable, mt.PrevKey = true, mt.RecKey
		case legacy:
			mt.Writable, mt.PrevKey = true, legacyKey
		case mt.Cur == mt.Want:
			mt.Writable, mt.PrevKey = true, mt.Key
		}
	}
	return out, ownedMask, nil
}

// titleSyncStats is what one sync changed.
type titleSyncStats struct {
	Slots   int // slot names written
	Members int // members whose title text changed
}

func (m *Mod) syncCorpTitlesErr(ctx context.Context, corpID int64) (titleSyncStats, error) {
	var st titleSyncStats
	facts, err := getAnyCorpFacts(ctx, m.db, corpID)
	if err != nil {
		return st, err
	}
	if !facts.Player {
		return st, nil
	}
	if err := evedb.EnsureDefaultTitles(ctx, m.db, corpID); err != nil {
		return st, err
	}
	set := m.titleSetFor(ctx, facts)
	titleSetMu.Lock()
	titleSetCache[corpID] = titleSetEntry{set, clock.Unix()}
	titleSetMu.Unlock()

	claims, err := m.titleSlotClaims(ctx, corpID, set)
	if err != nil {
		return st, err
	}

	// Claim and name the slots.
	for _, c := range claims {
		if !c.Claim {
			if c.Owned {
				_, _ = m.db.ExecContext(ctx,
					`DELETE FROM rolebot_corp_titles WHERE corporationID = ? AND titleID = ?`, corpID, c.Slot.ID)
			}
			continue
		}
		if c.Cur != c.Want {
			t := evedb.TitleRow{TitleID: c.Slot.ID, TitleName: c.Want}
			if c.Row != nil {
				t.Masks = c.Row.Masks // only the name is the module's
			}
			if err := evedb.UpsertTitle(ctx, m.db, corpID, t); err != nil {
				return st, err
			}
			st.Slots++
			if h := modapi.Corp().TitleChanged; h != nil {
				h(corpID, c.Slot.ID, c.Cur, c.Want)
			}
		}
		if c.PrevName != c.Want || !c.Owned {
			if _, err := m.db.ExecContext(ctx, `
				INSERT INTO rolebot_corp_titles (corporationID, titleID, kind, name) VALUES (?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE kind = VALUES(kind), name = VALUES(name)`,
				corpID, c.Slot.ID, c.Slot.Kind, c.Want); err != nil {
				return st, err
			}
		}
	}

	plan, ownedMask, err := m.corpTitlePlan(ctx, corpID, facts, set, claims)
	if err != nil {
		return st, err
	}
	if ownedMask == 0 && len(plan) == 0 {
		return st, nil
	}
	ceo, _ := getCEO(ctx, m.db, corpID)
	now := clock.Unix()
	for _, mt := range plan {
		upd := &evedb.CorpMemberUpdate{}
		changed := false
		newMask := (mt.CurMask &^ ownedMask) | mt.WantMask
		if newMask != mt.CurMask {
			v := int64(newMask)
			upd.TitleMask = &v
			changed = true
		}
		writeText := mt.Writable && mt.Cur != mt.Want
		if writeText {
			want := mt.Want
			upd.Title = &want
			changed = true
		}
		if changed {
			if err := evedb.UpdateCorpMemberRow(ctx, m.db, mt.CharID, upd); err != nil {
				return st, fmt.Errorf("title write %d: %w", mt.CharID, err)
			}
		}

		// The record: what the module wrote, and for which position.
		switch {
		case !mt.Writable:
		case mt.Want == "":
			if mt.Recorded {
				_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_title_text WHERE characterID = ?`, mt.CharID)
			}
		case writeText || !mt.Recorded || mt.RecText != mt.Want || mt.RecKey != mt.Key:
			if _, err := m.db.ExecContext(ctx, `
				INSERT INTO rolebot_title_text (characterID, corporationID, titleKey, text, at) VALUES (?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), titleKey = VALUES(titleKey),
				                        text = VALUES(text), at = VALUES(at)`,
				mt.CharID, corpID, mt.Key, mt.Want, now); err != nil {
				return st, fmt.Errorf("title record %d: %w", mt.CharID, err)
			}
		}
		if !writeText {
			continue
		}
		st.Members++

		// Only a change of POSITION is news. A rank is the merit pass's news,
		// and a new name for the same position is nobody's.
		if mt.Key == mt.PrevKey || (!isPositionKey(mt.Key) && !isPositionKey(mt.PrevKey)) {
			continue
		}
		switch {
		case ceo != nil && !isPositionKey(mt.Key):
			m.ceoLog(ctx, ceo, powerRoles, "title", mt.CharID, 0, "%s no longer holds the title %s", mt.Name, mt.Cur)
		case ceo != nil:
			m.ceoLog(ctx, ceo, powerRoles, "title", mt.CharID, 0, "gave %s the title %s", mt.Name, mt.Want)
		case isPositionKey(mt.Key):
			m.logBot(mt.CharID, logInfo, "corp", "now holds the title %s in %s", mt.Want, facts.Name)
		}
	}
	return st, nil
}

// reconcileCorpTitlesAtBoot re-syncs every corporation whose titles the module keeps,
// once, on its own goroutine, so changes made while the server was down land before
// anybody looks. Idempotent; a corporation already in line writes nothing.
func (m *Mod) reconcileCorpTitlesAtBoot(ctx context.Context) {
	goSafe("title-reconcile", func() {
		rows, err := m.db.QueryContext(ctx, `
			SELECT corporationID FROM rolebot_ceos
			UNION SELECT corporationID FROM rolebot_corps WHERE manageTitles = 1`)
		if err != nil {
			rlogf("rolebots: title reconcile: %v", err)
			return
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		var corps, slots, members, failed int
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			st, err := m.syncCorpTitlesErr(ctx, id)
			if err != nil {
				failed++
				rlogf("rolebots: title reconcile corp %d: %v", id, err)
				continue
			}
			corps++
			slots += st.Slots
			members += st.Members
		}
		rlogf("rolebots: title reconcile: %d corporation(s) checked, %d slot name(s) and %d member title(s) brought in line, %d failed",
			corps, slots, members, failed)
	})
}

// titleHolders answers, per title kind, the set of members holding that
// position right now.
func (m *Mod) titleHolders(ctx context.Context, corpID int64, facts *CorpFacts) (map[string]map[int64]bool, error) {
	out := map[string]map[int64]bool{}
	add := func(kind string, id int64) {
		if out[kind] == nil {
			out[kind] = map[int64]bool{}
		}
		out[kind][id] = true
	}
	if facts.CEOID != 0 {
		add(titleKindCEO, facts.CEOID)
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID, COALESCE(r.corprole, 0)
		  FROM character_ ch
		  JOIN chrCorporationRoles r ON r.characterID = ch.characterID
		 WHERE ch.corporationID = ? AND ch.accountID > 0`, corpID)
	if err != nil {
		return nil, fmt.Errorf("title roles: %w", err)
	}
	for rows.Next() {
		var id int64
		var roles uint64
		if rows.Scan(&id, &roles) != nil || id == facts.CEOID {
			continue // the CEO holds RoleAll; its title is the chair's
		}
		switch {
		case roles&corp.RoleDirector != 0:
			add(titleKindDirector, id)
		case roles&corp.RolePersonnelManager != 0:
			add(titleKindPersonnel, id)
		case roles&corp.RoleAccountant != 0:
			add(titleKindTreasurer, id)
		}
	}
	rows.Close()

	ranks, err := m.listCorpRanks(ctx, corpID)
	if err != nil {
		return nil, err
	}
	for _, r := range ranks {
		if k := seatTitleKind(r.Activity); k != "" {
			add(k, r.CharID)
		}
	}

	// A corporation a bot CEO runs has a real Industry Director (corpdirector.go); an
	// opted-in one gets the derived overseer, a title and nothing more.
	if leadsHumans(corpID) {
		if id := m.corpDirectorOf(ctx, corpID); id != 0 {
			add(titleKindFactory, id)
		}
	} else if industryTagged(facts.Policy.Tags) {
		if id := m.factoryOverseer(ctx, corpID, facts.CEOID); id != 0 {
			add(titleKindFactory, id)
		}
	}
	return out, nil
}

// factoryOverseer is the corporation's senior industrialist: the rostered member who
// most wants to build, weighted by length of service. The CEO is excluded, having a title.
func (m *Mod) factoryOverseer(ctx context.Context, corpID, ceoID int64) int64 {
	rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID, COALESCE(ch.corporationDateTime, 0)
		  FROM character_ ch
		  JOIN rolebot_characters b ON b.characterID = ch.characterID
		 WHERE ch.corporationID = ? AND ch.characterID <> ? AND b.enabled = 1`, corpID, ceoID)
	if err != nil {
		return 0
	}
	type cand struct{ id, since int64 }
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.since) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()
	best, bestScore := int64(0), 0.0
	now := win32Now()
	for _, c := range cands {
		traits, err := getTraits(ctx, m.db, c.id)
		if err != nil {
			continue
		}
		tn := resolveTuning(traits)
		drive := tn.SupplyDrive * tn.IndustryAppetite
		// A consumer does not run the corporation's factories, however long
		// it has served: only a pilot at least as keen as an even temperament.
		if drive < supplyDriveBase {
			continue
		}
		days := 1.0
		if c.since > 0 && now > c.since {
			days += float64(now-c.since) / 864000000000.0
		}
		if s := drive * days; s > bestScore {
			best, bestScore = c.id, s
		}
	}
	return best
}

// setManageTitles is the operator's opt-in for a corporation with no CEO.
func setManageTitles(ctx context.Context, db *sql.DB, corpID int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	res, err := db.ExecContext(ctx,
		`UPDATE rolebot_corps SET manageTitles = ?, updatedAt = ? WHERE corporationID = ?`,
		v, clock.Unix(), corpID)
	if err != nil {
		return fmt.Errorf("setManageTitles: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO rolebot_corps (corporationID, manageTitles, seeded, updatedAt) VALUES (?, ?, 0, ?)
			ON DUPLICATE KEY UPDATE manageTitles = VALUES(manageTitles)`, corpID, v, clock.Unix()); err != nil {
			return fmt.Errorf("setManageTitles: %w", err)
		}
	}
	return nil
}
