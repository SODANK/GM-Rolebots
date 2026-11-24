package rolebots

import (
	"context"
	"sync"
	"sync/atomic"
)

// The human chair: a player corporation whose CEO is a person and which has bots in it.
//
// Such a corporation has no rolebot_ceos row, so nothing a bot CEO decides is decided for it. What
// it has instead is the Management page (corpmanage_igb.go): the chair and the division heads post
// the orders a bot CEO would, and a bot head runs its own division (corpchair_desks.go). Everything
// that carries an order out asks corpDeskRun, not managedCEO, so a board the chair posted to is
// worked exactly as a bot CEO's.
//
// The shared powers take a *corpCEO. deskCEO answers one for either kind of corporation; for a human
// chair it is built in memory and never saved. Its CharID is the acting BOT (chairHand), never the
// person: the powers place orders in that pilot's market slots, send it on buy trips and put its
// name to courier contracts. The CEO's think, the treasury, shares, roles, estate, diplomacy and war
// never see it (humanChairPowersOff).

// ceoOriginHuman marks the in-memory chair of a human CEO's corporation.
const ceoOriginHuman = "human"

// humanChairPowersOff is fixed: what stays on is what the Management page can order.
var humanChairPowersOff = map[string]bool{
	powerIdentity: true, powerDirective: true, powerDiplomacy: true, powerAlliance: true, powerWar: true,
	powerHR: true, powerRoles: true, powerTreasury: true, powerEstate: true, powerShares: true,
}

// humanChairIndex maps a human-chaired corporation to its CEO. Every planning pass reads it, so it
// is memory only, rebuilt with the CEO index (ceoThinkPass) and after an appointment.
var humanChairIndex atomic.Pointer[map[int64]int64]

// humanChaired is the human CEO of a corporation with bots in it.
func humanChaired(corpID int64) (ceoID int64, ok bool) {
	p := humanChairIndex.Load()
	if p == nil || corpID == 0 {
		return 0, false
	}
	id, ok := (*p)[corpID]
	return id, ok
}

// corpDeskRun reports whether a corporation's boards are worked: it has a bot CEO, or a human one
// commanding bots. A map read.
func corpDeskRun(corpID int64) bool {
	if _, ok := managedCEO(corpID); ok {
		return true
	}
	_, ok := humanChaired(corpID)
	return ok
}

// humanChairCorpIDs is every human-chaired corporation, in no order.
func humanChairCorpIDs() []int64 {
	p := humanChairIndex.Load()
	if p == nil {
		return nil
	}
	out := make([]int64, 0, len(*p))
	for id := range *p {
		out = append(out, id)
	}
	return out
}

// refreshHumanChairIndex rebuilds the index: player corporations whose CEO is a person and which
// have an active bot on the books.
func (m *Mod) refreshHumanChairIndex(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT co.corporationID, co.ceoID
		  FROM corporation co
		  JOIN character_ cc ON cc.characterID = co.ceoID AND cc.accountID > 0
		  LEFT JOIN rolebot_characters rb ON rb.characterID = co.ceoID
		  LEFT JOIN crpNPCCorporations n ON n.corporationID = co.corporationID
		 WHERE n.corporationID IS NULL AND rb.characterID IS NULL AND COALESCE(co.deleted, 0) = 0
		   AND EXISTS (SELECT 1 FROM character_ ch
		                 JOIN rolebot_characters b ON b.characterID = ch.characterID AND b.enabled = 1
		                WHERE ch.corporationID = co.corporationID)`)
	if err != nil {
		rlogf("rolebots: human chair index: %v", err)
		return
	}
	idx := map[int64]int64{}
	for rows.Next() {
		var corpID, ceoID int64
		if rows.Scan(&corpID, &ceoID) == nil {
			idx[corpID] = ceoID
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}
	for corpID, ceoID := range idx {
		// A generated pilot off the roster is still a bot, and a bot CEO's
		// corporation is managed, not chaired.
		if _, managed := managedCEO(corpID); managed || m.isSynthetic(ceoID) {
			delete(idx, corpID)
		}
	}
	humanChairIndex.Store(&idx)
}

// humanChairPilot is the neutral identity a human chair's charter is resolved from: a person has no
// traits, and the numbers that matter are the Budget order's (chairCharter).
func humanChairPilot() pilotIdentity {
	return pilotIdentity{Traits: map[string]int{}, Tune: defaultTuning()}
}

// humanChairCharter is a human chair's charter: the neutral one with the chair's budget on top.
func (m *Mod) humanChairCharter(ctx context.Context, corpID int64) Charter {
	return m.chairCharter(ctx, corpID, resolveCharter(humanChairPilot()))
}

// deskCEO is the chair the shared powers act for: the bot CEO, or a human chair's in-memory one with
// a bot as its hand. Nil when nobody runs the corporation's desks, or a human chair has no bot that
// could act.
func (m *Mod) deskCEO(ctx context.Context, corpID int64) *corpCEO {
	return m.deskCEOFor(ctx, corpID, "")
}

// deskCEOFor is deskCEO with the arm's own bot head preferred as the hand.
func (m *Mod) deskCEOFor(ctx context.Context, corpID int64, arm corpArm) *corpCEO {
	if c, err := getCEO(ctx, m.db, corpID); err == nil && c != nil {
		return c
	}
	if _, ok := humanChaired(corpID); !ok {
		return nil
	}
	hand := m.chairHand(ctx, corpID, arm)
	if hand == 0 {
		return nil
	}
	return humanChairCEO(corpID, hand)
}

// humanChairCEO is the in-memory chair. Never saved. Pure.
func humanChairCEO(corpID, hand int64) *corpCEO {
	return &corpCEO{CorpID: corpID, CharID: hand, Origin: ceoOriginHuman, PowersOff: humanChairPowersOff}
}

// withChairStaff carries a human chair's officers on ctx, read with the person as CEO: staffFor
// would otherwise read them with the acting bot left out. A bot CEO's context is unchanged.
func (m *Mod) withChairStaff(ctx context.Context, c *corpCEO) context.Context {
	if c == nil || c.Origin != ceoOriginHuman {
		return ctx
	}
	s, err := m.loadCorpStaff(ctx, c.CorpID, m.corpCEOID(ctx, c.CorpID))
	if err != nil {
		return ctx
	}
	return withCorpStaff(ctx, s)
}

// ── the hand ────────────────────────────────────────────────────────────────

// chairHandMemoSec is how long a corporation's acting bot is remembered.
const chairHandMemoSec = 60

type chairHandKey struct {
	corpID int64
	arm    corpArm
}

type chairHandRow struct {
	id, at int64
}

var chairHandMemo sync.Map // chairHandKey to chairHandRow

func forgetChairHand(corpID int64) {
	chairHandMemo.Range(func(k, _ any) bool {
		if k.(chairHandKey).corpID == corpID {
			chairHandMemo.Delete(k)
		}
		return true
	})
}

func init() { registerCorpForget("chairHandMemo", forgetChairHand) }

// chairOrderPlacer is the bot that places a human chair's next market order, 0 for none: the Sales
// head, else the bot asking, else any member, whoever holds Trader or Director and has a slot free. The
// core books the order to the placer's corporation, so it must be a member's.
func (m *Mod) chairOrderPlacer(ctx context.Context, corpID, hand int64) int64 {
	members, err := m.corpMembers(ctx, corpID, m.corpCEOID(ctx, corpID))
	if err != nil {
		return 0
	}
	sales := m.corpHeads(ctx, corpID)[armSales].CharID
	var first, second, rest []int64
	for _, cm := range members {
		if cm.Human() || !cm.Enabled || !(holdsDirector(cm.Roles) || holdsTrader(cm.Roles)) {
			continue
		}
		switch cm.CharID {
		case sales:
			first = append(first, cm.CharID)
		case hand:
			second = append(second, cm.CharID)
		default:
			rest = append(rest, cm.CharID)
		}
	}
	for _, id := range append(append(first, second...), rest...) {
		if m.freeOrderSlots(ctx, id) > corpOrderSlotKeep {
			return id
		}
	}
	return 0
}

// handCand is one bot that could act for a human chair.
type handCand struct {
	CharID int64
	Head   bool // heads the arm the work is for
	Rank   int  // 2 Director, 1 Trader, 0 neither
	On     bool
}

// pickChairHand is the bot that acts: the arm's own head, else whoever is logged in and holds most
// authority, lowest character ID on a tie so the answer is steady. Pure.
func pickChairHand(cands []handCand) int64 {
	best, bestScore := int64(0), -1
	for _, c := range cands {
		s := c.Rank
		if c.On {
			s += 10
		}
		if c.Head {
			s += 100
		}
		if s > bestScore || (s == bestScore && c.CharID < best) {
			best, bestScore = c.CharID, s
		}
	}
	return best
}

// chairHand is the bot that acts for a human chair, 0 when the corporation has none.
func (m *Mod) chairHand(ctx context.Context, corpID int64, arm corpArm) int64 {
	key := chairHandKey{corpID, arm}
	now := clock.Unix()
	if v, ok := chairHandMemo.Load(key); ok && now-v.(chairHandRow).at < chairHandMemoSec {
		return v.(chairHandRow).id
	}
	members, err := m.corpMembers(ctx, corpID, m.corpCEOID(ctx, corpID))
	if err != nil {
		return 0
	}
	var head int64
	if arm != "" {
		head = m.corpHeads(ctx, corpID)[arm].CharID
	}
	var cands []handCand
	for _, cm := range members {
		if cm.Human() || !cm.Enabled {
			continue
		}
		hc := handCand{CharID: cm.CharID, Head: cm.CharID == head, On: !popIsOffline(cm.CharID)}
		switch {
		case holdsDirector(cm.Roles):
			hc.Rank = 2
		case holdsTrader(cm.Roles):
			hc.Rank = 1
		}
		cands = append(cands, hc)
	}
	id := pickChairHand(cands)
	chairHandMemo.Store(key, chairHandRow{id, now})
	return id
}
