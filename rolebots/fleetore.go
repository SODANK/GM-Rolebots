package rolebots

// Co-op mining fleet ore: each miner's rock, handed back. The fleet buys nothing. Miners
// fill their own cans, the hauler runs them to the staging station, and handBackFleetOre
// hands each miner's units into that miner's own hangar, less a cut the hauler keeps.
//
// rolebot_ore_hold records what the fleet delivered to each pilot, so disposal can tell
// the op's haul from a stockpile the pilot keeps (strays.go, industrySellSurplus). It is
// keyed by character, not gang, so a fleet killed before settling still leaves a note.
// disposeFleetOre runs for any docked pilot holding a note (orestrays.go, homeshop.go,
// warehouse.go): refine where the pilot's own yield beats the ore bid, sell otherwise.

import (
	"context"
	"fmt"
	"sort"

	evedb "gurimoon/db"
)

// coopHaulCutFrac is the share of every miner's units the hauler keeps for carrying. It
// is small because the hauler's hold is the fleet's, and a miner that would do better
// hauling its own rock stops flying with one.
const coopHaulCutFrac = 0.08

// coopClaim is one miner's attributed units of one ore type on one load.
type coopClaim struct {
	CharID int64
	Units  int64
}

// splitCoopLoad divides the units of one ore type that arrived among the miners whose
// cans they came out of. Claims scale down pro rata when less arrived (the largest
// remainders take the rounding); any excess is the hauler's. The hauler keeps cutFrac of
// every other miner's share. Returns units per pilot, summing to arrived. Pure.
func splitCoopLoad(arrived int64, claims []coopClaim, haulerID int64, cutFrac float64) map[int64]int64 {
	out := map[int64]int64{}
	if arrived <= 0 {
		return out
	}
	total := int64(0)
	for _, c := range claims {
		if c.Units > 0 {
			total += c.Units
		}
	}
	if total <= 0 {
		out[haulerID] = arrived
		return out
	}
	scaled := make([]int64, len(claims))
	given := int64(0)
	if total <= arrived {
		for i, c := range claims {
			if c.Units > 0 {
				scaled[i] = c.Units
				given += c.Units
			}
		}
	} else {
		type rem struct {
			i    int
			frac int64
		}
		var rems []rem
		for i, c := range claims {
			if c.Units <= 0 {
				continue
			}
			num := c.Units * arrived
			scaled[i] = num / total
			given += scaled[i]
			rems = append(rems, rem{i, num % total})
		}
		sort.SliceStable(rems, func(a, b int) bool { return rems[a].frac > rems[b].frac })
		for k := 0; given < arrived && k < len(rems); k++ {
			scaled[rems[k].i]++
			given++
		}
	}
	for i, c := range claims {
		if scaled[i] <= 0 {
			continue
		}
		units := scaled[i]
		if c.CharID != haulerID {
			cut := int64(float64(units) * cutFrac)
			units -= cut
		}
		out[c.CharID] += units
	}
	kept := arrived
	for _, u := range out {
		kept -= u
	}
	if kept > 0 {
		out[haulerID] += kept
	}
	return out
}

// handBackFleetOre distributes a docked co-op hauler's load: each miner's share goes to
// that miner's own hangar at this station and is noted in rolebot_ore_hold; the hauler's
// cut and anything unattributed land in its own hangar the same way. The load's claims
// are cleared. Returns how many other pilots received rock.
func (m *Mod) handBackFleetOre(ctx context.Context, b *botRun, g *Gang, lots []oreHoldRow) (int, error) {
	handed := map[int64]bool{}
	for _, lot := range lots {
		if lot.Units <= 0 {
			continue
		}
		// This hauler's claims only (the ledger is keyed by haulerCharID): a sister hauler's
		// claims are still in her hold, and reading them would dilute this split.
		rows, err := m.db.QueryContext(ctx, `
			SELECT characterID, units FROM rolebot_gang_ore
			 WHERE gangID = ? AND haulerCharID = ? AND typeID = ? AND units > 0
			 ORDER BY characterID`, g.GangID, b.CharID, lot.TypeID)
		if err != nil {
			return len(handed), fmt.Errorf("co-op claims: %w", err)
		}
		var claims []coopClaim
		for rows.Next() {
			var c coopClaim
			if rows.Scan(&c.CharID, &c.Units) == nil {
				claims = append(claims, c)
			}
		}
		rows.Close()

		shares := splitCoopLoad(lot.Units, claims, b.CharID, coopHaulCutFrac)
		ids := make([]int64, 0, len(shares))
		for id := range shares {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			moved, err := m.moveCargoUnitsTo(ctx, b, lot.TypeID, shares[id], id)
			if err != nil {
				return len(handed), err
			}
			if moved <= 0 {
				continue
			}
			m.noteFleetOreDelivered(ctx, id, b.StationID, lot.TypeID, lot.TypeName, moved)
			m.stackHangar(ctx, id, b.StationID)
			m.noteOreFlow(ctx, lot.TypeID, oreStageHandBack, moved, 0)
			if id != b.CharID {
				handed[id] = true
				m.logBot(id, logInfo, "gang",
					"the %q hauler ran in %d units of its %s to %s — its own rock, waiting in its hangar",
					g.Name, moved, lot.TypeName, m.stationLabel(ctx, b.StationID))
			} else {
				m.logBot(b.CharID, logInfo, "gang",
					"kept %d units of %s as its carrying cut of the load", moved, lot.TypeName)
			}
		}
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM rolebot_gang_ore WHERE gangID = ? AND haulerCharID = ? AND typeID = ?`,
			g.GangID, b.CharID, lot.TypeID); err != nil {
			return len(handed), fmt.Errorf("clear co-op claims: %w", err)
		}
	}
	return len(handed), nil
}

// moveCargoUnitsTo moves up to want units of one type out of the pilot's hold
// into ownerID's hangar at this station, splitting a stack when it must, and
// hands ownership over with it. Returns what moved.
func (m *Mod) moveCargoUnitsTo(ctx context.Context, b *botRun, typeID, want, ownerID int64) (int64, error) {
	return m.moveCargoUnitsInto(ctx, b, typeID, want, ownerID, b.StationID)
}

// moveCargoUnitsInto is moveCargoUnitsTo with the destination spelled out:
// the station itself for a personal hangar, or a corporation office's itemID
// for its hangar. Flag 4 either way.
func (m *Mod) moveCargoUnitsInto(ctx context.Context, b *botRun, typeID, want, ownerID, locationID int64) (int64, error) {
	return m.moveCargoUnitsIntoFlag(ctx, b, typeID, want, ownerID, locationID, flagHangar)
}

// moveCargoUnitsIntoFlag is moveCargoUnitsInto into one flag, such as a corporation
// office's division (lootdonate.go: Spoils).
func (m *Mod) moveCargoUnitsIntoFlag(ctx context.Context, b *botRun, typeID, want, ownerID, locationID, flag int64) (int64, error) {
	if b.StationID == 0 || locationID == 0 {
		return 0, fmt.Errorf("not docked")
	}
	if b.ShipID == 0 || want <= 0 {
		return 0, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE locationID = ? AND ownerID = ? AND flag = ? AND typeID = ?
		ORDER BY quantity DESC`, b.ShipID, b.CharID, flagCargo, typeID)
	if err != nil {
		return 0, fmt.Errorf("cargo stacks: %w", err)
	}
	type stack struct{ itemID, qty int64 }
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.itemID, &s.qty); err != nil {
			rows.Close()
			return 0, err
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	moved := int64(0)
	for _, s := range stacks {
		if moved >= want {
			break
		}
		left := want - moved
		if s.qty <= left {
			if _, err := m.db.ExecContext(ctx,
				`UPDATE entity SET locationID = ?, flag = ?, ownerID = ? WHERE itemID = ?`,
				locationID, flag, ownerID, s.itemID); err != nil {
				return moved, fmt.Errorf("hand over stack: %w", err)
			}
			moved += s.qty
			continue
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, left, s.itemID); err != nil {
			return moved, fmt.Errorf("split stack: %w", err)
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO entity
				(itemName, typeID, ownerID, locationID, flag, contraband, singleton,
				 quantity, x, y, z, customInfo)
			SELECT typeName, typeID, ?, ?, ?, 0, 0, ?, 0, 0, 0, NULL
			FROM invTypes WHERE typeID = ?`,
			ownerID, locationID, flag, left, typeID); err != nil {
			return moved, fmt.Errorf("land split stack: %w", err)
		}
		moved += left
	}
	return moved, nil
}

// oreHoldRow is one type of undisposed fleet ore in a commander's hangar.
type oreHoldRow struct {
	TypeID   int64
	TypeName string
	Units    int64
}

// noteFleetOreDelivered records units the hauler has just put into the
// commander's hangar and that the commander has not yet dealt with.
func (m *Mod) noteFleetOreDelivered(ctx context.Context, leaderID, stationID, typeID int64, typeName string, units int64) {
	if leaderID == 0 || stationID == 0 || units <= 0 {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_ore_hold (characterID, stationID, typeID, typeName, units, takenAt)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			units = units + VALUES(units),
			typeName = VALUES(typeName),
			takenAt = VALUES(takenAt)`,
		leaderID, stationID, typeID, typeName, units, clock.Unix()); err != nil {
		rlogf("rolebots: note fleet ore char=%d station=%d type=%d: %v", leaderID, stationID, typeID, err)
	}
}

// fleetOreHold is the undisposed fleet ore sitting in one hangar.
func (m *Mod) fleetOreHold(ctx context.Context, charID, stationID int64) ([]oreHoldRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, typeName, units FROM rolebot_ore_hold
		WHERE characterID = ? AND stationID = ? AND units > 0
		ORDER BY takenAt`, charID, stationID)
	if err != nil {
		return nil, fmt.Errorf("fleet ore hold: %w", err)
	}
	defer rows.Close()
	var out []oreHoldRow
	for rows.Next() {
		var r oreHoldRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.Units); err != nil {
			return nil, fmt.Errorf("fleet ore hold scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// clearFleetOreHold forgets one hangar's delivered ore once it is disposed of.
func (m *Mod) clearFleetOreHold(ctx context.Context, charID, stationID int64) {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_ore_hold WHERE characterID = ? AND stationID = ?`,
		charID, stationID); err != nil {
		rlogf("rolebots: clear fleet ore char=%d station=%d: %v", charID, stationID, err)
	}
}

// hasFleetOre is the cheap gate the home loop runs on every docked pilot: one
// indexed read, and almost always zero.
func (m *Mod) hasFleetOre(ctx context.Context, charID, stationID int64) bool {
	var n int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_ore_hold
		WHERE characterID = ? AND stationID = ? AND units > 0`,
		charID, stationID).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// disposeFleetOre is the commander's end of an ordinary mining op: the pooled haul, dealt
// with in one pass. It refines when the commander's own yield beats the ore bid
// (miningRefineWorth), otherwise it lists or sells, capped to the ledger stack by stack so
// a stockpile the commander mined for itself is untouched. Reports whether it did anything.
func (m *Mod) disposeFleetOre(ctx context.Context, b *botRun) bool {
	if b.StationID == 0 {
		return false
	}
	held, err := m.fleetOreHold(ctx, b.CharID, b.StationID)
	if err != nil || len(held) == 0 {
		return false
	}

	// refineHangarOre takes no cap, so refine only at a dock that is not one of this
	// commander's factory bases, where listing the minerals would sell its bill of materials.
	factoryHere := (b.Profile != nil && b.Profile.IndustryStationID == b.StationID) ||
		b.State.Data.StandbyFactoryID == b.StationID ||
		b.State.Data.IndFactoryID == b.StationID
	worth, _, _ := m.miningRefineWorth(ctx, b, refineWorthEdge)
	if !factoryHere && worth {
		unitsIn, got, rerr := m.refineHangarOre(ctx, b)
		if rerr == nil && unitsIn > 0 && len(got) > 0 {
			listed, _, kept, lerr := m.listRefinedMinerals(ctx, b, got, nil, mineralHoldMaxSec)
			if lerr != nil {
				rlogf("rolebots: fleet ore list char=%d: %v", b.CharID, lerr)
			}
			m.logBot(b.CharID, logInfo, "gang",
				"refined the fleet's whole haul at %s — %d units in, %d mineral lot(s) listed, %d held back; its refining yield beats the ore bid",
				m.stationLabel(ctx, b.StationID), unitsIn, listed, kept)
			// Booked as a fleet haul and, separately, by refineHangarOre as refining throughput;
			// readers use single stages, never a sum (espore.go).
			m.noteFleetHaulRefined(ctx, held, unitsIn)
			m.clearFleetOreHold(ctx, b.CharID, b.StationID)
			return true
		}
		// Nothing made a whole portion, or the refine refused: fall through and sell the rock.
	}

	// Per handed-back row, capped to the ledger: list an ask here, else dump into a
	// standing bid, else carry it home and re-key the note to the home station. What none
	// could move stays.
	var listed, sold, carried, stayed int64
	gross := 0.0
	home := m.homeStationFor(ctx, b.CharID)
	for _, r := range held {
		stackID, units := m.fleetOreStack(ctx, b, r.TypeID, r.Units)
		if units <= 0 {
			continue // already gone: refined, moved, or sold by another pass
		}
		if n, isk, err := m.listOreStackFrom(ctx, b, r.TypeID, stackID, units); err != nil {
			rlogf("rolebots: list fleet ore char=%d type=%d: %v", b.CharID, r.TypeID, err)
		} else if n > 0 {
			listed += n
			gross += isk
			units -= n
		}
		if units > 0 {
			took, isk := m.dumpFleetOre(ctx, b, r.TypeID, stackID, r.TypeName, units)
			sold += took
			gross += isk
			units -= took
		}
		if units > 0 && home != 0 && home != b.StationID && b.ShipID != 0 {
			// The stack may have been split or replaced by the two sales
			// above; find what is left of the share before moving it.
			stackID, units = m.fleetOreStack(ctx, b, r.TypeID, units)
			if units > 0 {
				free, ferr := m.holdFreeM3(ctx, b.CharID, b.ShipID)
				if ferr == nil {
					moved, merr := m.moveHangarUnitsToCargo(ctx, b, stackID, units, free)
					if merr != nil {
						rlogf("rolebots: carry fleet ore home char=%d type=%d: %v", b.CharID, r.TypeID, merr)
					} else if moved > 0 {
						m.noteFleetOreDelivered(ctx, b.CharID, home, r.TypeID, r.TypeName, moved)
						carried += moved
						units -= moved
					}
				}
			}
		}
		stayed += units
	}
	switch {
	case listed+sold+carried > 0:
		m.logBot(b.CharID, logInfo, "gang",
			"disposed of its fleet share at %s — %d units listed, %d sold (%.0f ISK), %d carried home, %d left as stock",
			m.stationLabel(ctx, b.StationID), listed, sold, gross, carried, stayed)
	default:
		m.logBot(b.CharID, logInfo, "gang",
			"nobody at %s bids on the fleet's haul and it could not be listed or carried — it stays in the hangar as stock",
			m.stationLabel(ctx, b.StationID))
	}
	// Cleared either way: what was carried has its own note at home, and what stayed is a
	// stockpile that a surviving row would have the next pass sell again.
	m.clearFleetOreHold(ctx, b.CharID, b.StationID)
	return true
}

// fleetOreStack finds the pilot's biggest hangar stack of an ore type here and splits
// the ledger's share off it, so every move acts on exactly the handed-back units.
// Returns the stack holding the share and its units (0 when none is left).
func (m *Mod) fleetOreStack(ctx context.Context, b *botRun, typeID, want int64) (int64, int64) {
	var itemID, qty int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0
		ORDER BY quantity DESC LIMIT 1`,
		b.CharID, b.StationID, typeID).Scan(&itemID, &qty); err != nil || qty <= 0 {
		return 0, 0
	}
	units := want
	if qty < units {
		units = qty
	}
	if units <= 0 {
		return 0, 0
	}
	stackID, err := m.splitHangarStack(ctx, b.CharID, b.StationID, itemID, units)
	if err != nil {
		rlogf("rolebots: split fleet ore char=%d type=%d: %v", b.CharID, typeID, err)
		return 0, 0
	}
	return stackID, units
}

// dumpFleetOre sells a share into the best local bid and books it under the
// fleet stage. (0, 0) when nobody bids.
func (m *Mod) dumpFleetOre(ctx context.Context, b *botRun, typeID, itemID int64, typeName string, units int64) (int64, float64) {
	took, gross, err := m.dumpToBestBid(ctx, b, typeID, itemID, typeName, units,
		bidDump{Note: "mining fleet share sold by its miner", Tag: "gang haul"})
	if err != nil {
		rlogf("rolebots: sell fleet ore char=%d type=%d: %v", b.CharID, typeID, err)
		return 0, 0
	}
	// Booked as its own stage (oreflow.go). dumpToBestBid does not say which side took the
	// fill, and the pooled haul's units were already counted as mined.
	m.noteOreFlow(ctx, typeID, oreStageFleet, took, gross)
	return took, gross
}

// splitHangarStack makes a hangar stack of exactly `units` out of a bigger
// one, same owner, same station: the stack itself when it is no bigger, else
// the original is shrunk and a new row minted. Returns the stack to act on.
func (m *Mod) splitHangarStack(ctx context.Context, ownerID, stationID, itemID, units int64) (int64, error) {
	var typeID, qty int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeID, quantity FROM entity WHERE itemID = ?`, itemID).Scan(&typeID, &qty); err != nil {
		return 0, fmt.Errorf("split stack %d: %w", itemID, err)
	}
	if units <= 0 || qty <= units {
		return itemID, nil
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, units, itemID); err != nil {
		return 0, fmt.Errorf("shrink stack %d: %w", itemID, err)
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO entity
			(itemName, typeID, ownerID, locationID, flag, contraband, singleton,
			 quantity, x, y, z, customInfo)
		SELECT typeName, typeID, ?, ?, ?, 0, 0, ?, 0, 0, 0, NULL
		FROM invTypes WHERE typeID = ?`,
		ownerID, stationID, flagHangar, units, typeID)
	if err != nil {
		return 0, fmt.Errorf("mint split stack: %w", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("mint split stack id: %w", err)
	}
	return newID, nil
}

// moveHangarUnitsToCargo is the mirror of moveCargoUnitsTo: up to `units` of one hangar
// stack into the pilot's own ship, capped by the hold's free m3 (freeM3, measured by the
// caller with holdFreeM3 so this can be tested against a bare entity table). Returns what
// moved.
func (m *Mod) moveHangarUnitsToCargo(ctx context.Context, b *botRun, itemID, units int64, freeM3 float64) (int64, error) {
	if b.ShipID == 0 || units <= 0 {
		return 0, nil
	}
	var typeID, qty int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeID, quantity FROM entity WHERE itemID = ?`, itemID).Scan(&typeID, &qty); err != nil {
		return 0, fmt.Errorf("hangar stack %d: %w", itemID, err)
	}
	vol, err := evedb.GetTypeVolume(ctx, m.db, typeID)
	if err != nil || vol <= 0 {
		vol = 0.1
	}
	fit := int64(freeM3 / vol)
	if fit > units {
		fit = units
	}
	if fit > qty {
		fit = qty
	}
	if fit <= 0 {
		return 0, nil
	}
	if fit == qty {
		if _, err := m.db.ExecContext(ctx,
			`UPDATE entity SET locationID = ?, flag = ? WHERE itemID = ?`,
			b.ShipID, flagCargo, itemID); err != nil {
			return 0, fmt.Errorf("board stack %d: %w", itemID, err)
		}
	} else {
		if _, err := m.db.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, fit, itemID); err != nil {
			return 0, fmt.Errorf("shrink stack %d: %w", itemID, err)
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO entity
				(itemName, typeID, ownerID, locationID, flag, contraband, singleton,
				 quantity, x, y, z, customInfo)
			SELECT typeName, typeID, ?, ?, ?, 0, 0, ?, 0, 0, 0, NULL
			FROM invTypes WHERE typeID = ?`,
			b.CharID, b.ShipID, flagCargo, fit, typeID); err != nil {
			return 0, fmt.Errorf("board split stack: %w", err)
		}
	}
	m.stackCargo(ctx, b.CharID, b.ShipID)
	return fit, nil
}

// noteFleetHaulRefined books a refined haul against the fleet stage. refineHangarOre
// reports one total for the whole hangar, so the split across the held types is
// proportional, not exact. No ISK is booked here: listRefinedMinerals books the minerals.
func (m *Mod) noteFleetHaulRefined(ctx context.Context, held []oreHoldRow, unitsIn int64) {
	if unitsIn <= 0 || len(held) == 0 {
		return
	}
	total := int64(0)
	for _, r := range held {
		total += r.Units
	}
	if total <= 0 {
		return
	}
	booked := int64(0)
	for i, r := range held {
		share := r.Units * unitsIn / total
		if i == len(held)-1 {
			// The last type takes the rounding, so the stage total matches what the refine consumed.
			share = unitsIn - booked
		}
		if share <= 0 {
			continue
		}
		booked += share
		m.noteOreFlow(ctx, r.TypeID, oreStageFleet, share, 0)
	}
}

// clearGangOreClaimsFor drops the co-op ore claims riding in one hauler's hold when the
// load went with the ship (the pod path). A standing claim would be settled against rock
// that no longer exists, or logged as never delivered at the disband.
func (m *Mod) clearGangOreClaimsFor(ctx context.Context, charID int64) {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_gang_ore WHERE haulerCharID = ?`, charID); err != nil {
		rlogf("rolebots: clear gang ore claims hauler=%d: %v", charID, err)
	}
}
