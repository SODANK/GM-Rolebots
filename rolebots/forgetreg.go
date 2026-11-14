package rolebots

import "sort"

// Registries for the module's volatile, ID-keyed memory. Many decisions are remembered in
// package-level maps keyed by character or corporation ID (when a pilot last looked at a
// warehouse, a corporation's faction). A restart re-measures them, but a purge ends a
// pilot or corporation without a restart, so each map's owner registers, from its own
// init(), the function that frees its entries for one ID, and forgetCharacter and
// forgetCorp walk the registry. TestEveryCharKeyedCacheRegistersForget (forget_test.go)
// catches a map added without registering.
//
// The sweep registry does the same for housekeeping (startCacheSweep, cachesweep.go). A
// duplicate name panics at init, and the walk is in name order, not file init order.

type namedForget struct {
	name string
	fn   func(id int64)
}

type namedSweep struct {
	name string
	fn   func()
}

var (
	charForgets []namedForget
	corpForgets []namedForget
	sweeps      []namedSweep
)

// registerCharForget registers fn to free everything the owner remembers about one
// character. name is the map (or helper) it covers; the lint test matches it against the
// declaration.
func registerCharForget(name string, fn func(charID int64)) {
	charForgets = addForget(charForgets, "character", name, fn)
}

// registerCorpForget registers fn to free everything the owner remembers
// about one corporation.
func registerCorpForget(name string, fn func(corpID int64)) {
	corpForgets = addForget(corpForgets, "corporation", name, fn)
}

// registerSweep registers a periodic housekeeping pass (cachesweep.go).
func registerSweep(name string, fn func()) {
	if fn == nil {
		panic("rolebots: nil sweep registered for " + name)
	}
	for _, s := range sweeps {
		if s.name == name {
			panic("rolebots: duplicate sweep registration: " + name)
		}
	}
	sweeps = append(sweeps, namedSweep{name, fn})
	sort.Slice(sweeps, func(i, j int) bool { return sweeps[i].name < sweeps[j].name })
}

func addForget(list []namedForget, kind, name string, fn func(int64)) []namedForget {
	if fn == nil {
		panic("rolebots: nil " + kind + " forget registered for " + name)
	}
	for _, f := range list {
		if f.name == name {
			panic("rolebots: duplicate " + kind + " forget registration: " + name)
		}
	}
	list = append(list, namedForget{name, fn})
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	return list
}

// forgetCharacter frees every volatile entry the module holds about charID. It runs from
// the purge (botpurge.go), the migration to a player account (migrate.go) and the roster
// removal (roster.go), whatever else failed. The stats counter (statcount.go) must be
// cleared too: its pending map would re-create the row on the next flush.
func forgetCharacter(charID int64) {
	for _, f := range charForgets {
		f.fn(charID)
	}
}

// forgetCorp frees every volatile entry the module holds about corpID. It runs when a
// managed corporation ends (removeCEO), when the purge closes a corporation whose CEO had
// no heir (releaseFromPlayerCorp), and from the core's corp-deleted listener
// (evedb.CorpDeletedListeners).
func forgetCorp(corpID int64) {
	for _, f := range corpForgets {
		f.fn(corpID)
	}
}

// runSweeps runs every registered housekeeping pass, in name order.
func runSweeps() {
	for _, s := range sweeps {
		s.fn()
	}
}

// forgetNames lists what is registered, for the lint test.
func forgetNames(list []namedForget) []string {
	out := make([]string, 0, len(list))
	for _, f := range list {
		out = append(out, f.name)
	}
	return out
}
