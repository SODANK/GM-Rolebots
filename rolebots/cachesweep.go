package rolebots

// cacheSweepEveryTicks spaces the cache sweep. The sweep frees cache entries nobody
// reads again, and it runs even while the master switch is off.
const cacheSweepEveryTicks = 300 // 5 minutes; tick.Manager runs at 1s

// startCacheSweep registers the housekeeping pass.
func (m *Mod) startCacheSweep() {
	m.registerPeriodic("rolebot-cache-sweep", cacheSweepEveryTicks, func(uint64) bool {
		// The sweep walks bounded in-memory maps with no I/O, so it runs inline and cannot overlap
		// itself. Each pass registers itself with registerSweep (forgetreg.go).
		runSweeps()
		m.sweepLogEchoes()
		return true
	})
}
