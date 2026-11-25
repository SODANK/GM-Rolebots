package rolebots

// What one CEO think, or one staff-desk pass, reads once. The roster-wide facts it needs (corpMembers, a bot
// member's traits, the corporation's facts row, MAX(at) over the action ledger per verb) do not change under the
// lock the think holds, except the roles it writes itself, which setMemberRoles folds back in. So the first read is
// carried on the ctx and the rest are map lookups. Outside a think the ctx carries nothing and helpers read directly.
//
// The cache is per corporation: a helper asked about another corporation goes to the database.

import (
	"context"
	"sync"
)

type thinkCache struct {
	corpID int64
	mu     sync.Mutex

	membersOK   bool
	membersExcl int64
	members     []corpMember

	traits map[int64]map[string]int

	facts *CorpFacts

	actionsOK bool
	actions   map[string]int64 // verb to last at

	rentOK bool
	rent   float64
}

type thinkCacheKey struct{}

// withThinkCache starts a fresh per-think cache for one corporation.
func withThinkCache(ctx context.Context, corpID int64) context.Context {
	return context.WithValue(ctx, thinkCacheKey{}, &thinkCache{corpID: corpID})
}

// thinkCacheOf is the ctx's cache when it is for this corporation.
func thinkCacheOf(ctx context.Context, corpID int64) *thinkCache {
	tc, _ := ctx.Value(thinkCacheKey{}).(*thinkCache)
	if tc == nil || tc.corpID != corpID {
		return nil
	}
	return tc
}

// thinkCacheAny is the ctx's cache whatever corporation it is for.
func thinkCacheAny(ctx context.Context) *thinkCache {
	tc, _ := ctx.Value(thinkCacheKey{}).(*thinkCache)
	return tc
}

// cachedMembers is the carried member list, a copy, when it was read with
// the same exclusion.
func (tc *thinkCache) cachedMembers(exclude int64) ([]corpMember, bool) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if !tc.membersOK || tc.membersExcl != exclude {
		return nil, false
	}
	return append([]corpMember(nil), tc.members...), true
}

func (tc *thinkCache) keepMembers(exclude int64, members []corpMember) {
	tc.mu.Lock()
	tc.membersOK, tc.membersExcl = true, exclude
	tc.members = append([]corpMember(nil), members...)
	tc.mu.Unlock()
}

// noteRoles folds a role write back into the carried members, so a later
// power in the same think reads what was just written.
func (tc *thinkCache) noteRoles(charID int64, roles uint64) {
	tc.mu.Lock()
	for i := range tc.members {
		if tc.members[i].CharID == charID {
			tc.members[i].Roles = roles
		}
	}
	tc.mu.Unlock()
}

// memberCorp reports the cache's corporation when charID is a carried member.
func (tc *thinkCache) memberCorp(charID int64) (int64, bool) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for i := range tc.members {
		if tc.members[i].CharID == charID {
			return tc.corpID, true
		}
	}
	return 0, false
}

// memberRoles is a carried member's role mask as the think has it.
func (tc *thinkCache) memberRoles(charID int64) (uint64, bool) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for i := range tc.members {
		if tc.members[i].CharID == charID {
			return tc.members[i].Roles, true
		}
	}
	return 0, false
}

// traitsOf is a carried pilot's traits, a copy.
func (tc *thinkCache) traitsOf(charID int64) (map[string]int, bool) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	t, ok := tc.traits[charID]
	if !ok {
		return nil, false
	}
	out := make(map[string]int, len(t))
	for k, v := range t {
		out[k] = v
	}
	return out, true
}

func (tc *thinkCache) keepTraits(all map[int64]map[string]int) {
	tc.mu.Lock()
	tc.traits = all
	tc.mu.Unlock()
}

// preloadThink fills the member list and every bot member's traits in two reads. Called once the think holds the
// lock; best-effort: a failed read leaves the helpers on their own queries.
func (m *Mod) preloadThink(ctx context.Context, corpID, ceoID int64, facts *CorpFacts) {
	tc := thinkCacheOf(ctx, corpID)
	if tc == nil {
		return
	}
	if facts != nil {
		tc.mu.Lock()
		tc.facts = facts
		tc.mu.Unlock()
	}
	members, err := m.corpMembers(ctx, corpID, ceoID)
	if err != nil {
		return
	}
	ids := make([]int64, 0, len(members)+1)
	ids = append(ids, ceoID)
	for _, cm := range members {
		if cm.OnRoster {
			ids = append(ids, cm.CharID)
		}
	}
	if traits, err := loadTraitsBulk(ctx, m.db, ids); err == nil {
		tc.keepTraits(traits)
	}
}
