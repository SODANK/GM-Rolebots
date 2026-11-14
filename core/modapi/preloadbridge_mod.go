package modapi

import (
	"errors"

	"gurimoon/destiny"
)

// SpaceHooks is the space-loading seam. GetOrCreate on the SystemManager mints an
// empty system that skips the world build, so a module loads space only through
// these hooks. Both fields may be nil; CanLoadSystems reports whether they are set.
type SpaceHooks struct {
	// EnsureLoaded brings a system up to the state a player entering it would find and
	// returns it. Idempotent: a live system is returned as is.
	EnsureLoaded func(sysID int64) (*destiny.SolarSystem, error)

	// Release is the core teardown: it flushes in-memory belt state, runs the unloading
	// hook and releases NPCs. Returns false when the system was not torn down, for
	// example while a player entity (a bot projection counts) is still in it.
	Release func(sysID int64) bool
}

var spaceHooks SpaceHooks

// ErrNoSystemLoader is what EnsureSystemLoaded returns on a core without the seam.
// It is an error so a caller can tell no loader from a system that failed to load.
var ErrNoSystemLoader = errors.New("modapi: no system loader installed")

// InstallSpace publishes the core's system-loading entry points. Package main
// calls it once from init(); a later call overwrites.
func InstallSpace(h SpaceHooks) { spaceHooks = h }

// CanLoadSystems reports whether this core carries the seam. Ask before offering a
// preload setting, which would otherwise do nothing.
func CanLoadSystems() bool {
	return spaceHooks.EnsureLoaded != nil && spaceHooks.Release != nil
}

// EnsureSystemLoaded builds sysID if it is not already live and returns it.
func EnsureSystemLoaded(sysID int64) (*destiny.SolarSystem, error) {
	if spaceHooks.EnsureLoaded == nil {
		return nil, ErrNoSystemLoader
	}
	return spaceHooks.EnsureLoaded(sysID)
}

// ReleaseSystem tears sysID down through the core path. False means it was not
// torn down, usually because something is still in it.
func ReleaseSystem(sysID int64) bool {
	if spaceHooks.Release == nil {
		return false
	}
	return spaceHooks.Release(sysID)
}
