package main

import (
	"context"
	"log"
	"math/rand"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/drone"
	"gurimoon/modapi"
)

// init installs the drone seam (modapi.DroneHooks). Module drones are spawned by
// spawnDroneEntity and use the callbacks drones.go installs; this file adds only
// the launch and removals that have no client connection.
func init() {
	modapi.InstallDrones(modapi.DroneHooks{
		Launch: modLaunchDrones,
		Engage: func(ids []int64, shipID int64, targetID uint32) {
			if globalDroneMgr != nil {
				globalDroneMgr.Engage(ids, shipID, targetID)
			}
		},
		Mine: func(ids []int64, shipID int64, asteroidID uint32, repeat bool) {
			if globalDroneMgr != nil {
				globalDroneMgr.Mine(ids, shipID, asteroidID, repeat)
			}
		},
		ReturnBay: func(ids []int64, shipID int64) {
			if globalDroneMgr != nil {
				globalDroneMgr.ReturnBay(ids, shipID)
			}
		},
		InSpace: func(shipID int64) []int64 {
			if globalDroneMgr == nil {
				return nil
			}
			var ids []int64
			for _, rec := range globalDroneMgr.ControlledBy(shipID) {
				ids = append(ids, rec.ItemID)
			}
			return ids
		},
		Recall: modRecallDrones,
		Lose:   modLoseDrones,
	})
}

// modLaunchDrones is launchDronesForPlayer without a connection: the bay is walked
// by type and no OnItemChange is sent. spawnDroneEntity gets a stand-in destinyConn
// with the five fields it reads; the master has no Conn, so the ball goes to
// everyone else on the grid.
func modLaunchDrones(ss *destiny.SolarSystem, master *destiny.Entity,
	charID, corpID, shipID, sysID int64, want map[int64]int64) []int64 {

	if ss == nil || master == nil || globalDB == nil || globalDroneMgr == nil || len(want) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	maxActive := droneMaxActive(ctx, globalDB, charID)
	inSpace := globalDroneMgr.CountControlled(shipID)
	if inSpace >= maxActive {
		return nil
	}
	bay, err := evedb.GetDroneBayItems(ctx, globalDB, shipID)
	if err != nil {
		log.Printf("[drone] module launch: bay of ship %d: %v", shipID, err)
		return nil
	}
	shim := &destinyConn{solarSystem: ss, entity: master, shipID: shipID, sysID: sysID, corpID: corpID}

	var launched []int64
	left := make(map[int64]int64, len(want))
	for t, n := range want {
		left[t] = n
	}
	for i := range bay {
		stack := &bay[i]
		for left[stack.TypeID] > 0 && stack.Quantity > 0 && inSpace < maxActive {
			shipPos := master.Pos()
			pos := destiny.Vec3{
				X: shipPos.X + 300 + rand.Float64()*400,
				Y: shipPos.Y + (rand.Float64()-0.5)*200,
				Z: shipPos.Z + 300 + rand.Float64()*400,
			}
			newID, err := evedb.LaunchDroneToSpace(ctx, globalDB, stack.ItemID, charID, shipID, sysID, pos.X, pos.Y, pos.Z)
			if err != nil {
				log.Printf("[drone] module launch item %d: %v", stack.ItemID, err)
				break
			}
			stack.Quantity--
			left[stack.TypeID]--
			if rec := spawnDroneEntity(shim, charID, newID, stack, pos, globalDB); rec != nil {
				launched = append(launched, newID)
				inSpace++
			}
		}
	}
	return launched
}

// modRecallDrones scoops every drone shipID controls into its bay at any range:
// scoopDronesForPlayer without the range test and the client notification.
func modRecallDrones(shipID int64) int {
	if globalDB == nil || globalDroneMgr == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n := 0
	for _, rec := range globalDroneMgr.ControlledBy(shipID) {
		if err := evedb.ScoopDroneToBay(ctx, globalDB, rec.ItemID, shipID); err != nil {
			log.Printf("[drone] module recall %d: %v", rec.ItemID, err)
			continue
		}
		takeDroneOffGrid(rec)
		n++
	}
	return n
}

// modLoseDrones destroys every drone shipID controls: the ball leaves every grid
// and the item is deleted, like the manager's Killed path without the explosion.
func modLoseDrones(shipID int64) int {
	if globalDroneMgr == nil {
		return 0
	}
	recs := globalDroneMgr.ControlledBy(shipID)
	for _, rec := range recs {
		takeDroneOffGrid(rec)
	}
	if globalDB != nil && len(recs) > 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, rec := range recs {
				if err := evedb.DeleteDroneItem(ctx, globalDB, rec.ItemID); err != nil {
					log.Printf("[drone] module lose %d: %v", rec.ItemID, err)
				}
			}
		}()
	}
	return len(recs)
}

// takeDroneOffGrid removes a drone's running fire FX (a gun that stops without
// StopFire keeps drawing on every client), registry entry, ball and drone window
// row. removeDroneBall already lifts an EWAR drone's hold.
func takeDroneOffGrid(rec *drone.Record) {
	if tgt := rec.Target(); tgt != 0 && rec.Attrs.EwarKind == "" {
		stopNPCFireFX(rec.Entity, rec.System, tgt)
	}
	globalDroneMgr.Remove(rec.ItemID)
	removeDroneBall(rec)
	broadcastDroneState(rec, true)
}
