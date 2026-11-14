package rolebots

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync/atomic"

	evedb "gurimoon/db"
)

// Drones are spent, not owned.
// A player's drones die in fights: rats switch onto them and a warp-off leaves the slow ones
// behind. That loss is the market for the drones the industrialists build, so the abstract
// tier (drones.go) loses them too.
// The model is per drone and per fight, off two inputs: how hard the pilot was hit and
// whether it ran. Embodied pilots lose drones for real in space (drones_embodied.go); this is
// the abstract half.

// settingDroneAttrition scales abstract drone losses: 1 is the model below,
// 0 turns abstract losses off, 2 doubles them.
const settingDroneAttrition = "rolebots.droneAttrition"

const (
	// droneLossBase is the chance any launched drone is lost in a fight that barely touched the
	// ship.
	droneLossBase = 0.04
	// droneLossPerDamage is added per unit of the ship's EHP lost.
	droneLossPerDamage = 0.25
	// droneLossEscaped is added when the pilot warped out of the fight.
	droneLossEscaped = 0.30
	droneLossMax     = 0.95
)

var droneAttritionBits atomic.Uint64

func init() { droneAttritionBits.Store(math.Float64bits(1.0)) }

// DroneAttrition is the operator's scale on abstract drone losses.
func DroneAttrition() float64 { return math.Float64frombits(droneAttritionBits.Load()) }

func setDroneAttrition(v float64) {
	if v < 0 || math.IsNaN(v) {
		v = 0
	}
	if v > 5 {
		v = 5
	}
	droneAttritionBits.Store(math.Float64bits(v))
}

// loadDroneAttrition reads the persisted setting at Init.
func (m *Mod) loadDroneAttrition(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingDroneAttrition); err == nil && ok {
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
			setDroneAttrition(f)
		}
	}
}

// droneLossChance is the chance one launched drone does not come home from a fight.
// lostFrac is the share of the ship's EHP the fight took (0..1).
func droneLossChance(lostFrac float64, escaped bool, dial float64) float64 {
	if dial <= 0 {
		return 0
	}
	p := droneLossBase + droneLossPerDamage*math.Max(0, math.Min(1, lostFrac))
	if escaped {
		p += droneLossEscaped
	}
	p *= dial
	return math.Min(p, droneLossMax)
}

// rollDroneLosses rolls each launched drone once. roll returns [0,1).
func rollDroneLosses(flight []droneLaunchPick, p float64, roll func() float64) []droneLaunchPick {
	if p <= 0 {
		return nil
	}
	var lost []droneLaunchPick
	for _, f := range flight {
		var n int64
		for i := int64(0); i < f.N; i++ {
			if roll() < p {
				n++
			}
		}
		if n > 0 {
			lost = append(lost, droneLaunchPick{TypeID: f.TypeID, N: n})
		}
	}
	return lost
}

// abstractDroneAttrition costs a surviving pilot the drones its fight
// destroyed. A ship that died already lost its whole bay with the hull.
func (m *Mod) abstractDroneAttrition(ctx context.Context, charID, shipID int64,
	flight []droneLaunchPick, lostFrac float64, escaped bool) {

	if len(flight) == 0 || shipID == 0 {
		return
	}
	lost := rollDroneLosses(flight, droneLossChance(lostFrac, escaped, DroneAttrition()), rand.Float64)
	var total int64
	for _, l := range lost {
		n, err := m.loseBayDrones(ctx, shipID, l.TypeID, l.N)
		if err != nil {
			rlogf("rolebots: drone attrition ship=%d type=%d: %v", shipID, l.TypeID, err)
			continue
		}
		total += n
	}
	if total > 0 {
		how := "shot out of the sky"
		if escaped {
			how = "left behind on the way out"
		}
		m.logBot(charID, logInfo, "combat", "lost %d drone(s) in the fight — %s", total, how)
	}
}

// loseBayDrones destroys up to n drones of one type from a ship's drone bay,
// shrinking stacks and deleting emptied ones. Reports how many went.
func (m *Mod) loseBayDrones(ctx context.Context, shipID, typeID, n int64) (int64, error) {
	if n <= 0 {
		return 0, nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE locationID = ? AND flag = 87 AND typeID = ?
		ORDER BY quantity ASC, itemID ASC
		FOR UPDATE`, shipID, typeID)
	if err != nil {
		return 0, err
	}
	type stack struct{ id, qty int64 }
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.id, &s.qty); err != nil {
			rows.Close()
			return 0, err
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var gone int64
	for _, s := range stacks {
		if gone >= n {
			break
		}
		take := n - gone
		if take >= s.qty {
			// Attributes first, as DeleteItemWithAttrs does: a drone that
			// took damage in an earlier fight carries entity_attributes rows.
			if _, err := tx.ExecContext(ctx, `DELETE FROM entity_attributes WHERE itemID = ?`, s.id); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, s.id); err != nil {
				return 0, err
			}
			gone += s.qty
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, take, s.id); err != nil {
			return 0, err
		}
		gone += take
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return gone, nil
}

// pvpDroneAttrition runs the same loss roll for every survivor of one side of
// a bot-versus-bot fight. The dead lost their bays with their hulls.
func (m *Mod) pvpDroneAttrition(ctx context.Context, side []*pvpCombatant, escaped bool) {
	for _, c := range side {
		if c == nil || c.abstractCombatant == nil || c.EHP <= 0 || c.MaxEHP <= 0 {
			continue
		}
		m.abstractDroneAttrition(ctx, c.CharID, c.ShipID, c.Drones, 1-c.EHP/c.MaxEHP, escaped)
	}
}
