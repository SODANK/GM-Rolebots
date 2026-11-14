package main

import (
	"database/sql"
	"strconv"

	"gurimoon/destiny"
	"gurimoon/modapi"
)

// The system-loading seam. The build is core's ensureSystemLoaded
// (cmd/gurimoon/systemload.go), the function registerUndock calls; this file
// publishes it and the matching teardown through modapi.SpaceHooks.

// releaseSystemByID is releaseSystem reached by ID, for callers outside package
// main that hold no *SolarSystem. The belt-state flush it runs is mandatory.
func releaseSystemByID(sysMgr *destiny.SystemManager, sysID int64, sqldb *sql.DB) bool {
	if sysMgr == nil {
		return false
	}
	ss := sysMgr.Get(sysID)
	if ss == nil {
		return false
	}
	releaseSystem(sysMgr, ss, sqldb)
	// releaseSystem returns nothing, so success is the system leaving the map.
	// RemoveIfEmpty refuses while a player entity (a bot projection counts) is in it.
	return sysMgr.Get(sysID) == nil
}

// init installs the seam. Each closure reads its globals at call time, because
// init() runs before main() assigns them.
func init() {
	modapi.InstallSpace(modapi.SpaceHooks{
		EnsureLoaded: func(sysID int64) (*destiny.SolarSystem, error) {
			return ensureSystemLoaded(globalSysMgr, sysID, globalDB,
				"preload:"+strconv.FormatInt(sysID, 10), nil, nil)
		},
		Release: func(sysID int64) bool {
			return releaseSystemByID(globalSysMgr, sysID, globalDB)
		},
	})
}
