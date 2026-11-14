package rolebots

import (
	"context"
	"sync/atomic"
	"time"
)

// Live reload for the traffic dials: the undock wave, the gate wave and the warp budget and interlock. A jammed hub
// needs a number changed without a restart. Runs once a minute off the tick goroutine; a missing or unparseable row
// keeps its default, and bucket and window state is not reset.
const trafficSettingsEveryTicks = 60

// trafficSettingsBusy keeps one reload in flight at a time: a slow database
// must not stack a goroutine a minute.
var trafficSettingsBusy atomic.Bool

func (m *Mod) startTrafficSettingsReload() {
	m.registerPeriodic("rolebot-traffic-settings", trafficSettingsEveryTicks, func(uint64) bool {
		if !trafficSettingsBusy.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer trafficSettingsBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			m.loadUndockSettings(ctx)
			m.loadGateSettings(ctx)
			m.loadWarpBudgetSettings(ctx)
		}()
		return true
	})
}
