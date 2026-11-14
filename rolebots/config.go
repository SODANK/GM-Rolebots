package rolebots

import (
	"encoding/json"
	"log"
	"sync/atomic"
)

// Settings keys in server_settings (module-owned, prefixed).
const (
	settingMaster = "rolebots.master"
	// Which generation of defaultCorpPolicy the catalogue was last seeded against (corps.go,
	// applyCorpDefaultGeneration). Bump the generation below when the seeded answer changes.
	settingCorpDefaults = "rolebots.corpDefaults"
	// Whether the lifetime trade counters have been seeded from the 30-day trade feed
	// (statcount.go, seedTradeCounters). It stores the newest ts the seed consumed.
	settingStatsSeed = "rolebots.statsSeed"
)

// corpDefaultsGeneration is the current shape of the seeded catalogue; the generations are
// listed in docs/comment-notes-b03.md#corp-defaults-generations.
const corpDefaultsGeneration = "4"

// Config is the module's block from config.json "modules": {"rolebots": {...}}. Everything here
// is a default; live state (master switch, per-bot profiles) is DB-backed and edited from the
// ESP Rolebot section.
type Config struct {
	// Enabled is the master-switch default before the first ESP toggle.
	Enabled bool `json:"enabled"`
	// OffloadMinutes is the combat loot offload cadence (default 10). The removed "conjure" key is
	// ignored.
	OffloadMinutes int `json:"offloadMinutes"`
	// Console opens the module's own console window at boot (Windows) and keeps every rolebot
	// line out of the server's console. Default on; false gives one stream in one window.
	// See console.go.
	Console bool `json:"console"`
	// ConsoleMirror also echoes each line to the server console, for a headless run or while
	// diagnosing the window itself.
	ConsoleMirror bool `json:"consoleMirror"`
	// SimWorkers is how many rolebots may take their sim turn at once (simpool.go). Default 8; 1
	// runs the pass serially. Clamped to simMaxWorkers. The residency and homeLoop keys are ignored.
	SimWorkers int `json:"simWorkers"`
}

// defaultConfig is the shape a missing or unreadable config block falls back to. It is defined
// once so parseConfig does not keep a second copy.
func defaultConfig() Config {
	return Config{OffloadMinutes: 10, Console: true, SimWorkers: simDefaultWorkers}
}

func parseConfig(raw json.RawMessage) Config {
	cfg := defaultConfig()
	if raw == nil {
		return cfg
	}
	// Decoded onto the defaults, not a zero value: json.Unmarshal leaves an absent field
	// untouched, so "console" keeps its default unless somebody wrote an explicit false.
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Printf("rolebots: bad config block (using defaults): %v", err)
		return defaultConfig()
	}
	if cfg.OffloadMinutes <= 0 {
		cfg.OffloadMinutes = 10
	}
	return cfg
}

// masterEnabled is the process-wide RoleBot master switch.
var masterEnabled atomic.Bool

func setMaster(on bool) { masterEnabled.Store(on) }

// Master reports whether the RoleBot system is switched on.
func Master() bool { return masterEnabled.Load() }
