package modapi

import "gurimoon/destiny"

// DroneHooks is the drone seam, installed by package main from init(). IDs are
// inventory itemIDs; for a drone in space that is also its ball ID. Every field
// may be nil.
type DroneHooks struct {
	// Launch puts drones from shipID's drone bay into space around master. want maps
	// typeID to count; the Drones skill and drones already out cap the total. Returns
	// the itemIDs now in space.
	Launch func(ss *destiny.SolarSystem, master *destiny.Entity,
		charID, corpID, shipID, sysID int64, want map[int64]int64) []int64

	// Engage, Mine and ReturnBay are the drone window's commands for the drones
	// shipID controls. Mine with repeat works the rock until it is gone or the hold
	// is full. ReturnBay flies them home and scoops them.
	Engage    func(droneIDs []int64, shipID int64, targetID uint32)
	Mine      func(droneIDs []int64, shipID int64, asteroidID uint32, repeat bool)
	ReturnBay func(droneIDs []int64, shipID int64)

	// InSpace lists the drones shipID controls, as itemIDs.
	InSpace func(shipID int64) []int64

	// Recall puts every drone shipID controls straight back into its bay, wherever
	// they are. Returns how many came home.
	Recall func(shipID int64) int

	// Lose destroys every drone shipID controls with no wreck and no explosion: the
	// ball goes and the item is deleted. Returns how many were lost.
	Lose func(shipID int64) int
}

var droneHooks DroneHooks

// InstallDrones publishes the core's drone entry points. Package main calls it
// once from init(); a later call overwrites.
func InstallDrones(h DroneHooks) { droneHooks = h }

// Drones returns the installed hooks. Every field may be nil.
func Drones() DroneHooks { return droneHooks }
