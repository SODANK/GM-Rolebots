package rolebots

// Corp ore: a bot-run corporation's mining op delivers to the corporation. The
// co-op hand-back (fleetore.go) gives each miner its own rock at the staging
// station; for a bot-CEO corporation the rock is the corporation's work, so the
// hauler runs the load to the corporation's office (corpDepositTarget) and the
// members' shares land in one corp-owned stack there. Nobody is paid at the dock:
// each member's share (miners and the hauler's cut) is credited as labour
// (rolebot_corp_labour, corppayroll.go) at oreLabourFrac of refined value and the
// weekly merit review turns it into pay, so cash moves only for what sold. A
// non-member's share goes back to them. A human-chair corporation
// (payrollManaged), one with no office, and ActIndMining keep the co-op hand-back.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

const (
	// settingCorpOreDelivery switches corp delivery on for bot-run
	// corporations' mining ops. Default on; "0" restores the co-op hand-back
	// for everyone.
	settingCorpOreDelivery = "rolebots.mining.corpDelivery"
	// settingOreLabourFrac is the share of a delivered unit's refined value
	// credited as labour. Default 0.5.
	settingOreLabourFrac = "rolebots.mining.oreLabourFrac"

	oreLabourFracDefault = 0.5
	oreLabourFracMax     = 2.0

	// haulCorpOre is the work-log kind for rock a pilot's share put into the
	// corporation's hangar, with the labour it was credited as its ISK.
	haulCorpOre = "corpore"
)

var (
	corpOreOffV      atomic.Bool  // true = switched off
	oreLabourFracPct atomic.Int64 // hundredths; 0 = the shipped default
)

// corpOreDeliveryOn reports whether bot-run corporations take their mining
// ops' ore into the office hangar.
func corpOreDeliveryOn() bool { return !corpOreOffV.Load() }

// oreLabourFrac is the labour credited per ISK of delivered ore value.
func oreLabourFrac() float64 {
	if v := oreLabourFracPct.Load(); v > 0 {
		return float64(v) / 100
	}
	return oreLabourFracDefault
}

// loadCorpOreSettings reads both dials at boot. Never fatal: an unreadable
// dial is the shipped default.
func (m *Mod) loadCorpOreSettings(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingCorpOreDelivery); err != nil {
		rlogf("rolebots: read %s: %v (using the default)", settingCorpOreDelivery, err)
	} else if ok {
		corpOreOffV.Store(strings.TrimSpace(v) == "0")
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingOreLabourFrac); err != nil {
		rlogf("rolebots: read %s: %v (using the default)", settingOreLabourFrac, err)
	} else if ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			oreLabourFracPct.Store(int64(clampRange(f, 0.01, oreLabourFracMax) * 100))
		}
	}
}

// corpOreRun answers whether this gang's load goes to its corporation, and
// where: the corporation, the station and the office itemID. ok=false for
// anything that keeps the co-op hand-back.
func (m *Mod) corpOreRun(ctx context.Context, g *Gang) (corpID, stationID, officeID int64, ok bool) {
	if g == nil || !corpOreDeliveryOn() || g.Activity != ActMining || g.CorpID == 0 || !payrollManaged(g.CorpID) {
		return 0, 0, 0, false
	}
	corpID, stationID, officeID = m.corpDepositTarget(ctx, g.CorpID)
	if officeID == 0 {
		return 0, 0, 0, false
	}
	return corpID, stationID, officeID, true
}

// corpOreRunDispatch sends a corp run's loaded hauler to the office station.
// handled=false when the gang is not a corp run (or the station cannot be
// located) and the ordinary dispatch should carry on.
func (m *Mod) corpOreRunDispatch(ctx context.Context, b *botRun, g *Gang) (bool, error) {
	_, stationID, _, ok := m.corpOreRun(ctx, g)
	if !ok {
		return false, nil
	}
	loc, err := evedb.GetStationLocation(ctx, m.db, stationID)
	if err != nil || loc.SolarSystemID == 0 {
		return false, nil
	}
	// Never a refinery drop: that refines each share under its miner's name.
	b.State.Data.PosRefineArray = 0
	return true, m.beginTravel(ctx, b, loc.SolarSystemID, stationID, "gang.haul.drop")
}

// oreLabourLine is one pilot's labour for one ore type on one load.
type oreLabourLine struct {
	CharID int64
	Kind   string // "mining" or "haul"
	Units  int64
	Credit float64
}

// corpOreLabour sizes the labour a corp delivery credits: every member's share
// at units x unitValue x frac, the hauler's (its cut and anything unattributed)
// as haul, everyone else's as mining. Non-members are absent. Sorted by pilot.
// Pure.
func corpOreLabour(shares map[int64]int64, member func(int64) bool, haulerID int64, unitValue, frac float64) []oreLabourLine {
	out := make([]oreLabourLine, 0, len(shares))
	for id, units := range shares {
		if units <= 0 || !member(id) {
			continue
		}
		kind := "mining"
		if id == haulerID {
			kind = "haul"
		}
		out = append(out, oreLabourLine{CharID: id, Kind: kind, Units: units,
			Credit: float64(units) * unitValue * frac})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CharID < out[j].CharID })
	return out
}

// corpOreUnitValues prices ore per unit for a delivery at this station: the
// fleet ore valuation (refined value at ideal yield, ask side) over the
// station's region and its neighbours.
func (m *Mod) corpOreUnitValues(ctx context.Context, stationID int64) map[int64]float64 {
	region := m.stationRegionID(ctx, stationID)
	if region == 0 {
		return nil
	}
	regions := regionIDs(m.scopeRegions(ctx, region, bandAdjacent))
	if len(regions) == 0 {
		regions = []int64{region}
	}
	return m.fleetOreUnitValue(ctx, regions)
}

// gangCoopClaims is THIS hauler's claims on one ore type (see the note in
// handBackFleetOre on why a sister hauler's claims are never read).
// gangCoopClaimsAll is every claim on one hauler's load, by ore type.
func (m *Mod) gangCoopClaimsAll(ctx context.Context, gangID, haulerID int64) (map[int64][]coopClaim, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, characterID, units FROM rolebot_gang_ore
		 WHERE gangID = ? AND haulerCharID = ? AND units > 0
		 ORDER BY typeID, characterID`, gangID, haulerID)
	if err != nil {
		return nil, fmt.Errorf("co-op claims: %w", err)
	}
	defer rows.Close()
	out := map[int64][]coopClaim{}
	for rows.Next() {
		var typeID int64
		var c coopClaim
		if rows.Scan(&typeID, &c.CharID, &c.Units) == nil {
			out[typeID] = append(out[typeID], c)
		}
	}
	return out, rows.Err()
}

func (m *Mod) gangCoopClaims(ctx context.Context, gangID, haulerID, typeID int64) ([]coopClaim, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, units FROM rolebot_gang_ore
		 WHERE gangID = ? AND haulerCharID = ? AND typeID = ? AND units > 0
		 ORDER BY characterID`, gangID, haulerID, typeID)
	if err != nil {
		return nil, fmt.Errorf("co-op claims: %w", err)
	}
	defer rows.Close()
	var claims []coopClaim
	for rows.Next() {
		var c coopClaim
		if rows.Scan(&c.CharID, &c.Units) == nil {
			claims = append(claims, c)
		}
	}
	return claims, rows.Err()
}

// corpMembersAmong reports which of these pilots belong to corpID right now.
func (m *Mod) corpMembersAmong(ctx context.Context, corpID int64, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, corpID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID FROM character_ WHERE corporationID = ? AND characterID IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("corp members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// corpOreDrop delivers a docked corp run's load: per ore type it splits the
// arrival over this hauler's claims, stacks every member's share in the office
// hangar as one corp-owned stack credited as labour, and hands non-members'
// shares back. No ISK moves. Returns units taken and non-members paid in rock.
func (m *Mod) corpOreDrop(ctx context.Context, b *botRun, g *Gang, corpID, officeID int64,
	lots []oreHoldRow, unitValue map[int64]float64) (toCorp int64, handed int, err error) {

	frac := oreLabourFrac()
	gaveBack := map[int64]bool{}
	// One read of every claim on this hauler's load and one membership read for
	// everybody named, not two per ore type.
	claimsByType, err := m.gangCoopClaimsAll(ctx, g.GangID, b.CharID)
	if err != nil {
		return toCorp, 0, err
	}
	allIDs := map[int64]bool{b.CharID: true}
	for _, cs := range claimsByType {
		for _, c := range cs {
			allIDs[c.CharID] = true
		}
	}
	idList := make([]int64, 0, len(allIDs))
	for id := range allIDs {
		idList = append(idList, id)
	}
	members, err := m.corpMembersAmong(ctx, corpID, idList)
	if err != nil {
		return toCorp, 0, err
	}
	var labour []labourRow
	var hauls []gangHaulRow
	defer func() {
		m.creditLabourBatch(ctx, corpID, labour)
		m.noteGangHaulBatch(ctx, g.GangID, hauls)
	}()
	for _, lot := range lots {
		if lot.Units <= 0 {
			continue
		}
		claims := claimsByType[lot.TypeID]
		shares := splitCoopLoad(lot.Units, claims, b.CharID, coopHaulCutFrac)
		ids := make([]int64, 0, len(shares))
		for id := range shares {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

		// Members first, as one move: one corp stack per type per drop.
		var memberUnits int64
		for _, id := range ids {
			if members[id] {
				memberUnits += shares[id]
			}
		}
		if memberUnits > 0 {
			moved, err := m.moveCargoUnitsInto(ctx, b, lot.TypeID, memberUnits, corpID, officeID)
			if err != nil {
				return toCorp, len(gaveBack), err
			}
			m.stackContainer(ctx, corpID, officeID, flagHangar)
			if moved > 0 {
				toCorp += moved
				m.noteOreFlow(ctx, lot.TypeID, oreStageStockpiled, moved, 0)
				// A short move (the hold held less than it said) scales everybody's credit
				// alike instead of shorting whoever sorts last.
				scale := float64(moved) / float64(memberUnits)
				vol := 0.0
				if v, verr := evedb.GetTypeVolume(ctx, m.db, lot.TypeID); verr == nil && v > 0 {
					vol = v
				}
				for _, l := range corpOreLabour(shares, func(id int64) bool { return members[id] },
					b.CharID, unitValue[lot.TypeID], frac) {
					units := int64(float64(l.Units) * scale)
					credit := l.Credit * scale
					labour = append(labour, labourRow{CharID: l.CharID, Kind: l.Kind, Credit: credit,
						Detail: fmt.Sprintf("%s %d %s for %q", map[string]string{"mining": "mined", "haul": "hauled"}[l.Kind],
							units, lot.TypeName, g.Name)})
					hauls = append(hauls, gangHaulRow{CharID: l.CharID, Kind: haulCorpOre, TypeID: lot.TypeID,
						TypeName: lot.TypeName, Units: units, M3: vol * float64(units), ISK: credit})
				}
			}
		}

		// Non-members: their own rock, handed back exactly as the co-op does.
		for _, id := range ids {
			if members[id] || shares[id] <= 0 {
				continue
			}
			moved, err := m.moveCargoUnitsTo(ctx, b, lot.TypeID, shares[id], id)
			if err != nil {
				return toCorp, len(gaveBack), err
			}
			if moved <= 0 {
				continue
			}
			m.noteFleetOreDelivered(ctx, id, b.StationID, lot.TypeID, lot.TypeName, moved)
			m.stackHangar(ctx, id, b.StationID)
			m.noteOreFlow(ctx, lot.TypeID, oreStageHandBack, moved, 0)
			if id != b.CharID {
				gaveBack[id] = true
				m.logBot(id, logInfo, "gang",
					"the %q hauler ran in %d units of its %s to %s — not a member of the corporation, so it is its own rock, waiting in its hangar",
					g.Name, moved, lot.TypeName, m.stationLabel(ctx, b.StationID))
			}
		}

		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM rolebot_gang_ore WHERE gangID = ? AND haulerCharID = ? AND typeID = ?`,
			g.GangID, b.CharID, lot.TypeID); err != nil {
			return toCorp, len(gaveBack), fmt.Errorf("clear co-op claims: %w", err)
		}
	}
	return toCorp, len(gaveBack), nil
}
