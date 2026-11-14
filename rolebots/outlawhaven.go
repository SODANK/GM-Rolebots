package rolebots

import (
	"context"
	"sync"

	"gurimoon/seclaw"
)

// An outlaw's record and rack.
//
// Under crimewatch 1.0 an outlaw (security status at or below -5.0) is a free
// target and raises no flag (core spaceentry.go), so no system is closed to
// one: its route prices the danger instead (router.go OutlawDanger). This file
// holds the plain rack it rehabilitates in and a cheap read of the record.

// ── the outlaw's catalogue ──────────────────────────────────────────────────
//
// An outlaw rehabilitates by killing rats, and for that job its rack is the
// plain one: a named variant is dropped wherever a plain module of the same
// kind exists (dropNamedWhenPlain), because the only thing a named part adds is
// a price the pilot cannot pay.

type catalogueLimitsKey struct{}

// catalogueLimits narrows the fit generator's catalogue for one pilot.
type catalogueLimits struct {
	// Plain drops named variants that have a plain sibling.
	Plain bool
}

func catalogueLimitsFrom(ctx context.Context) catalogueLimits {
	l, _ := ctx.Value(catalogueLimitsKey{}).(catalogueLimits)
	return l
}

// outlawCatalogueCtx marks ctx for an outlaw's combat fit; any other pilot or
// job gets ctx back untouched.
func (m *Mod) outlawCatalogueCtx(ctx context.Context, charID int64, job string) context.Context {
	if job != JobCombat {
		return ctx
	}
	sec, ok := m.pilotSecStatus(ctx, charID)
	if !ok || sec > seclaw.OutlawStatus {
		return ctx
	}
	return context.WithValue(ctx, catalogueLimitsKey{}, catalogueLimits{Plain: true})
}

// namedModuleBand reports whether a module is a named or premium variant: any
// invmetatypes band other than tech two. Bare tech one has no invmetatypes row.
func namedModuleBand(metaGroup int) bool {
	switch metaGroup {
	case 1, fitMetaStoryline, fitMetaFaction, fitMetaOfficer, fitMetaDeadspace:
		return true
	}
	return false
}

// dropNamedWhenPlain removes named variants from a catalogue wherever a plain
// module of the same kind is also on it. "Same kind" is group, slot band, hull
// size tag, charge size and weapon type. A named module with no plain sibling
// stays: an empty slot is worse than a dear one.
func dropNamedWhenPlain(cat []*marketModule) []*marketModule {
	type kind struct {
		group, slot      int64
		size             modSize
		charge           int
		turret, launcher bool
	}
	k := func(mm *marketModule) kind {
		return kind{mm.GroupID, mm.Slot, mm.Size, mm.ChargeSize, mm.Turret, mm.Launcher}
	}
	plain := map[kind]bool{}
	for _, mm := range cat {
		if !namedModuleBand(mm.MetaGroup) {
			plain[k(mm)] = true
		}
	}
	out := make([]*marketModule, 0, len(cat))
	for _, mm := range cat {
		if namedModuleBand(mm.MetaGroup) && plain[k(mm)] {
			continue
		}
		out = append(out, mm)
	}
	return out
}

// rehabModules applies the plain-rack rule when ctx asks for it.
func rehabModules(ctx context.Context, cat []*marketModule) []*marketModule {
	if !catalogueLimitsFrom(ctx).Plain {
		return cat
	}
	return dropNamedWhenPlain(cat)
}

// ── a pilot's record, cheaply ───────────────────────────────────────────────

type secEntry struct {
	sec float64
	at  int64
}

var outlawSecCache sync.Map // charID -> secEntry

const outlawSecTTLSec = 60

func init() {
	registerCharForget("outlawSecCache", func(id int64) { outlawSecCache.Delete(id) })
}

// pilotSecStatus reads a pilot's security record, held for a minute. The fit
// generator asks for every pilot on every plan pass and a record moves slowly.
func (m *Mod) pilotSecStatus(ctx context.Context, charID int64) (float64, bool) {
	now := clock.Unix()
	if v, ok := outlawSecCache.Load(charID); ok {
		if e := v.(secEntry); now-e.at < outlawSecTTLSec {
			return e.sec, true
		}
	}
	var sec float64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(securityRating, 0) FROM character_ WHERE characterID = ?`, charID).Scan(&sec); err != nil {
		return 0, false
	}
	outlawSecCache.Store(charID, secEntry{sec: sec, at: now})
	return sec, true
}
