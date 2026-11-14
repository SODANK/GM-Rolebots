package rolebots

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/modapi"
)

// The estate power: where the corporation lives. Its address is what it rents
// (crpoffices) plus its headquarters (corporation.stationID), which the rest of
// the module reads (corpAnchorStations, fleetOreStationID). The CEO rents the
// office that makes an address real, puts the HQ in it and points the fleet ore
// at it. With no office it picks the station most rostered members live at that
// it would live in itself (Charter.HQSecFloor), preferring assembly lines for a
// builder, if the wallet covers rent plus the reserve. An HQ not at an office
// moves only with the operator's leave (corpCEO.HQMovable). Rent is billed
// monthly by the core's bill sweep; an unpaid rental bill evicts (ceoPayRent).

// ceoEstateRetrySec: a CEO that could not afford an office does not try again
// every think.
const ceoEstateRetrySec = 24 * 3600

// estateHuntEverySec: between weekly reviews the estate hunts (facility,
// expansion, purpose offices, branch) run once a day.
const estateHuntEverySec = 24 * 3600

// hqHeldLogSec: a barred headquarters move is logged once a month per target.
const hqHeldLogSec = 30 * 24 * 3600

// estateHuntAt is when each corporation last ran its estate hunts. Memory only: a
// restart costs at most one extra hunt.
var estateHuntAt sync.Map // corpID to unix

// estateHuntDue reports whether a corporation's daily estate hunt is due.
func estateHuntDue(corpID int64) bool {
	v, ok := estateHuntAt.Load(corpID)
	return !ok || clock.Unix()-v.(int64) >= estateHuntEverySec
}

// markEstateHunt stamps a hunt.
func markEstateHunt(corpID int64) { estateHuntAt.Store(corpID, clock.Unix()) }

func init() { registerCorpForget("estateHuntAt", func(id int64) { estateHuntAt.Delete(id) }) }

// estateCandidate is one station the corporation could call home.
type estateCandidate struct {
	StationID int64
	Name      string
	RegionID  int64
	Security  float64
	Members   int
	HasLine   bool
	HasLab    bool // research lines (corpfacility.go stationHasLab)
	HasOffice bool
	Full      bool // no free office slot for a renter (officefull.go)
}

// rankEstate orders candidates for a first or second office. Pure. The security
// floor is a hard door and members living there is the pull; an assembly line
// is worth half a member more for a factory outfit, a fifth more again with
// research lines.
func rankEstate(cands []estateCandidate, secFloor float64, wantsLines bool, excludeRegion int64) []estateCandidate {
	var out []estateCandidate
	for _, c := range cands {
		if c.Security < secFloor || c.HasOffice || c.Full || (excludeRegion != 0 && c.RegionID == excludeRegion) {
			continue
		}
		out = append(out, c)
	}
	score := func(c estateCandidate) float64 {
		s := float64(c.Members)
		if wantsLines && c.HasLine {
			s *= 1.5
			if c.HasLab {
				s *= 1.2
			}
		}
		return s
	}
	sort.SliceStable(out, func(i, j int) bool {
		if score(out[i]) != score(out[j]) {
			return score(out[i]) > score(out[j])
		}
		return out[i].StationID < out[j].StationID
	})
	return out
}

// estateCandidates is every station at least one rostered member calls home
// (its home station, or where it is docked when it has none), plus the
// current headquarters.
func (m *Mod) estateCandidates(ctx context.Context, corpID, hq int64) ([]estateCandidate, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, s.stationName, COALESCE(ss.regionID, 0), COALESCE(ss.security, 0),
		       COUNT(*) AS n,
		       MAX(CASE WHEN o.itemID IS NULL THEN 0 ELSE 1 END)
		  FROM character_ ch
		  JOIN rolebot_characters b ON b.characterID = ch.characterID
		  JOIN staStations s ON s.stationID = COALESCE(NULLIF(ch.homeStationID, 0), NULLIF(ch.stationID, 0))
		  LEFT JOIN mapSolarSystems ss ON ss.solarSystemID = s.solarSystemID
		  LEFT JOIN crpoffices o ON o.stationID = s.stationID AND o.corporationID = ch.corporationID
		 WHERE ch.corporationID = ?
		 GROUP BY s.stationID, s.stationName, ss.regionID, ss.security`, corpID)
	if err != nil {
		return nil, fmt.Errorf("estateCandidates: %w", err)
	}
	var out []estateCandidate
	seenHQ := false
	for rows.Next() {
		var c estateCandidate
		var office int
		if err := rows.Scan(&c.StationID, &c.Name, &c.RegionID, &c.Security, &c.Members, &office); err != nil {
			rows.Close()
			return nil, err
		}
		c.HasOffice = office != 0
		if c.StationID == hq {
			seenHQ = true
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if hq != 0 && !seenHQ {
		c := estateCandidate{StationID: hq}
		var office int
		if err := m.db.QueryRowContext(ctx, `
			SELECT s.stationName, COALESCE(ss.regionID, 0), COALESCE(ss.security, 0),
			       (SELECT COUNT(*) FROM crpoffices o WHERE o.stationID = s.stationID AND o.corporationID = ?)
			  FROM staStations s
			  LEFT JOIN mapSolarSystems ss ON ss.solarSystemID = s.solarSystemID
			 WHERE s.stationID = ?`, corpID, hq).Scan(&c.Name, &c.RegionID, &c.Security, &office); err == nil {
			c.HasOffice = office != 0
			out = append(out, c)
		}
	}
	for i := range out {
		out[i].HasLine = m.stationHasLine(ctx, out[i].StationID)
		out[i].HasLab = m.stationHasLab(ctx, out[i].StationID)
	}
	m.markFullStations(ctx, out)
	return out, nil
}

// ceoEstate is the estate power. Every think sees to the essentials (first
// office, HQ at an office, fleet ore); the hunts (factory, department base,
// nomad's second address, trading post, branch) are the Corporate Review's.
func (m *Mod) ceoEstate(ctx context.Context, c *corpCEO, ch Charter, review bool) error {
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	var hq int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(stationID, 0) FROM corporation WHERE corporationID = ?`, c.CorpID).Scan(&hq)
	// A headquarters at a station the corporation owns gets its free office
	// before anything counts offices: without it the HQ would move to one it rents.
	if hq != 0 && evedb.StationOwnerCorp(ctx, m.db, hq) == c.CorpID {
		if officeID, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, c.CorpID, hq); officeID == hq {
			if _, err := m.ceoOpenOwnerOffice(ctx, c, hq, "its headquarters"); err != nil {
				return err
			}
		}
	}
	// The front or the capital, every think: sovereignty and station ownership change between
	// reviews (corpfront.go).
	if hq != 0 {
		if hq, err = m.ceoFront(ctx, c, ch, facts, hq); err != nil {
			return err
		}
	}
	// An office the corporation rents where no rostered member lives is not
	// in the candidate list; count it from the table.
	var officeCount int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM crpoffices WHERE corporationID = ?`, c.CorpID).Scan(&officeCount)
	// The hunts (facility, expansion, purpose offices, branch) run at the weekly
	// review and once a day between reviews, each behind its own cooldown.
	hunt := review || estateHuntDue(c.CorpID)
	if !hunt && officeCount > 0 && hq != 0 && m.corpFleetOreStation(ctx, c.CorpID) != 0 {
		// The essentials stand: the candidate scan (a GROUP BY plus two
		// line probes per station) is the hunt's. A CEO barred from moving its
		// headquarters skips it too: the move it would find is refused anyway.
		if officeID, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, c.CorpID, hq); officeID != 0 || !c.HQMovable {
			return nil
		}
	}
	cands, err := m.estateCandidates(ctx, c.CorpID, hq)
	if err != nil {
		return err
	}
	var offices []estateCandidate
	for _, cd := range cands {
		if cd.HasOffice {
			offices = append(offices, cd)
		}
	}

	wantsLines := industryTagged(ch.Tags)
	switch {
	case officeCount == 0:
		ranked := rankEstate(cands, ch.HQSecFloor, wantsLines, 0)
		if len(ranked) == 0 {
			// No member lives anywhere the CEO will fly, and an office-less corporation has
			// no hangar, factory or buyback: take the best station its members live at.
			ranked = rankEstate(cands, -1, wantsLines, 0)
		}
		if len(ranked) == 0 {
			return nil
		}
		return m.ceoRentOffice(ctx, c, facts, ch, ranked[0], true)

	default:
		// The headquarters belongs at an office.
		if len(offices) > 0 {
			atOffice := false
			for _, o := range offices {
				if o.StationID == hq {
					atOffice = true
				}
			}
			if !atOffice {
				sort.SliceStable(offices, func(i, j int) bool { return offices[i].Members > offices[j].Members })
				moved, err := m.ceoMoveHQ(ctx, c, facts, hq, offices[0])
				if err != nil {
					return err
				}
				if moved {
					hq = offices[0].StationID
				}
			}
		}
		// Point the fleet ore at the HQ office once there is one. A CEO banned from
		// moving its HQ may hold one with no office, so fleets use the office most members live at.
		if m.corpFleetOreStation(ctx, c.CorpID) == 0 {
			oreAt := int64(0)
			if hq != 0 {
				if officeID, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, c.CorpID, hq); officeID != 0 {
					oreAt = hq
				}
			}
			if oreAt == 0 && len(offices) > 0 {
				sort.SliceStable(offices, func(i, j int) bool { return offices[i].Members > offices[j].Members })
				oreAt = offices[0].StationID
			}
			if oreAt != 0 {
				if err := m.setCorpFleetOreStation(ctx, c.CorpID, oreAt); err != nil {
					return err
				}
				m.ceoLog(ctx, c, powerEstate, "hangar", oreAt, 0,
					"told the mining fleets to deliver their ore to the corp hangar at %s", stationNameOf(ctx, m, oreAt))
			}
		}
		if !hunt {
			return nil
		}
		markEstateHunt(c.CorpID)
		// An industry desk needs a factory with the lines its work runs on
		// (corpfacility.go); an office only at a logistics station would build nothing.
		if acted, err := m.ceoSecureFacilities(ctx, c, facts, ch, cands, hq); err != nil || acted {
			return err
		}
		// A department whose base is out of lines or overloaded grows into another
		// office (corpexpand.go). It runs on its own cooldown; the office hunts
		// below wait for the next day when it acted.
		expanded, err := m.ceoExpandBases(ctx, c, facts, ch, hq)
		if err != nil {
			return err
		}
		if !expanded {
			// An arm with members and nowhere of its own (miners, the security arm, R&D)
			// gets an office for its job (corpofficehunt.go).
			if acted, err := m.ceoPurposeOffices(ctx, c, facts, ch, hq); err != nil || acted {
				return err
			}
			// A nomad's second office.
			if officeCount == 1 && len(offices) == 1 && ch.nomadic() {
				ranked := rankEstate(cands, ch.HQSecFloor, wantsLines, offices[0].RegionID)
				if len(ranked) > 0 && ranked[0].Members > 0 {
					return m.ceoRentOffice(ctx, c, facts, ch, ranked[0], false)
				}
			}
			// A trade corporation's shelves where the market is (corptradepost.go).
			if acted, err := m.ceoTradingPost(ctx, c, facts, ch, hq); err != nil || acted {
				return err
			}
		}
		// A branch where the pilots are, to recruit from (corpbranch.go), on its own
		// cooldown whatever else this hunt did; it re-reads the wallet before renting.
		return m.ceoBranchOffice(ctx, c, facts, ch, hq, m.officeCount(ctx, c.CorpID))
	}
	return nil
}

// ceoRentOffice rents one office if the wallet covers the rent and the reserve
// after it, and makes it the headquarters when asked.
func (m *Mod) ceoRentOffice(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, at estateCandidate, makeHQ bool) error {
	return m.ceoRentOfficeAs(ctx, c, facts, ch, at, makeHQ, makeHQ)
}

// ceoRentOfficeAs is ceoRentOffice with the affordability rule chosen apart
// from the HQ move: an essential office (the first, or an industry desk's
// factory) needs rent plus the reserve; a merely useful one (a nomad's second
// address) needs three times the reserve.
func (m *Mod) ceoRentOfficeAs(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, at estateCandidate, makeHQ, essential bool) error {
	_, err := m.ceoTryRentOffice(ctx, c, facts, ch, at, makeHQ, essential, "")
	if errors.Is(err, evedb.ErrNoFreeOffices) {
		// Marked full now: the next pass ranks the station out.
		return nil
	}
	return err
}

// ceoTryRentOffice is ceoRentOfficeAs reporting whether an office was
// actually rented, with why the corporation wants it for the log ("" = the
// members who live there).
func (m *Mod) ceoTryRentOffice(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, at estateCandidate, makeHQ, essential bool, why string) (bool, error) {
	// The office cap (corpofficehunt.go) holds every office but the headquarters
	// and an essential one (the first office, the factory).
	if !makeHQ && !essential && facts != nil && m.officeCount(ctx, c.CorpID) >= officeCap(facts.Members) {
		return false, nil
	}
	if evedb.StationOwnerCorp(ctx, m.db, at.StationID) == c.CorpID {
		opened, err := m.ceoOpenOwnerOffice(ctx, c, at.StationID, why)
		if err != nil || !opened || !makeHQ {
			return opened, err
		}
		return true, m.ceoHQToOffice(ctx, c, facts, at)
	}
	// A full station is refused before the wallet is read. The hunts that walk a list
	// match the error and try their next station.
	if m.stationOfficesFull(ctx, at.StationID) {
		return false, fmt.Errorf("rent at %s: %w", at.Name, evedb.ErrNoFreeOffices)
	}
	cost := evedb.OfficeRentQuote(ctx, m.db, at.StationID)
	balance, err := evedb.GetCorpBalance(ctx, m.db, c.CorpID)
	if err != nil {
		return false, err
	}
	need := cost + ch.ReserveMonths*(m.corpMonthlyRent(ctx, c.CorpID)+cost)
	if !essential {
		need = cost + 3*ch.ReserveMonths*(m.corpMonthlyRent(ctx, c.CorpID)+cost)
	}
	if balance < need {
		if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "wait") >= ceoEstateRetrySec {
			m.ceoLog(ctx, c, powerEstate, "wait", at.StationID, need,
				"wants an office at %s but the corporation holds %s against the %s it would take with its reserve",
				at.Name, iskString(balance), iskString(need))
		}
		return false, nil
	}
	officeID, folderID, err := evedb.RentOffice(ctx, m.db, c.CorpID, at.StationID, cost)
	if errors.Is(err, evedb.ErrOfficeExists) {
		return false, nil
	}
	if errors.Is(err, evedb.ErrNoFreeOffices) {
		noteOfficesFull(at.StationID)
	}
	if err != nil {
		return false, fmt.Errorf("rent at %s: %w", at.Name, err)
	}
	forgetOfficesFull(at.StationID)
	if h := modapi.Corp().OfficeRentalChanged; h != nil {
		h(c.CorpID, at.StationID, officeID, folderID)
	}
	forgetThinkRent(ctx, c.CorpID)
	forgetCorpFactory(c.CorpID)
	forgetCorpAnchors(c.CorpID)
	m.corpBook(ctx, c.CorpID, lineRent, -cost, at.StationID, "office at "+at.Name)
	if why == "" {
		why = fmt.Sprintf("%d rostered member(s) live there", at.Members)
	}
	m.ceoLog(ctx, c, powerEstate, "rent", at.StationID, cost,
		"rented an office at %s for %s a month (%s)", at.Name, iskString(cost), why)
	if makeHQ {
		return true, m.ceoHQToOffice(ctx, c, facts, at)
	}
	return true, nil
}

// ceoHQToOffice moves the headquarters to an office just opened, unless it is
// there already.
func (m *Mod) ceoHQToOffice(ctx context.Context, c *corpCEO, facts *CorpFacts, at estateCandidate) error {
	hq := m.corpHQStation(ctx, c.CorpID)
	if hq == at.StationID {
		return nil
	}
	_, err := m.ceoMoveHQ(ctx, c, facts, hq, at)
	return err
}

// ceoOpenOwnerOffice opens the office of a corporation at a station it owns
// (an outpost): no rent, no bill, no reserve (evedb.OpenOwnerOffice). Reports
// whether an office was opened.
func (m *Mod) ceoOpenOwnerOffice(ctx context.Context, c *corpCEO, stationID int64, why string) (bool, error) {
	officeID, folderID, err := evedb.OpenOwnerOffice(ctx, m.db, c.CorpID, stationID)
	if errors.Is(err, evedb.ErrOfficeExists) || errors.Is(err, evedb.ErrNoFreeOffices) ||
		errors.Is(err, evedb.ErrNotStationOwner) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("owner office at %d: %w", stationID, err)
	}
	if h := modapi.Corp().OfficeRentalChanged; h != nil {
		h(c.CorpID, stationID, officeID, folderID)
	}
	forgetThinkRent(ctx, c.CorpID)
	forgetCorpFactory(c.CorpID)
	forgetCorpAnchors(c.CorpID)
	if why == "" {
		why = "its own station"
	}
	m.ceoLog(ctx, c, powerEstate, "office.own", stationID, 0,
		"opened an office at %s, which it owns: no rent (%s)", stationNameOf(ctx, m, stationID), why)
	return true, nil
}

// ceoMoveHQ moves the headquarters and pushes what the core's own move pushes:
// OnCorporationChanged and the hqID session change that decides where members'
// HQ roles apply. Every CEO HQ move goes through it, so the operator's ban lives
// here: without c.HQMovable it moves nothing and reports false.
func (m *Mod) ceoMoveHQ(ctx context.Context, c *corpCEO, facts *CorpFacts, oldHQ int64, to estateCandidate) (bool, error) {
	if !c.HQMovable {
		// Once per target a month, not once a day: the refusal does not change.
		if clock.Unix()-m.lastCEOActionFor(ctx, c.CorpID, "hqheld", to.StationID) >= hqHeldLogSec {
			m.ceoLog(ctx, c, powerEstate, "hqheld", to.StationID, 0,
				"would move the headquarters to %s but is barred from moving it", to.Name)
		}
		return false, nil
	}
	forgetCorpAnchors(c.CorpID)
	forgetCorpFactory(c.CorpID)
	if _, err := evedb.SetCorpHQ(ctx, m.db, c.CorpID, to.StationID); err != nil {
		return false, fmt.Errorf("move HQ to %s: %w", to.Name, err)
	}
	pushCorpChanged(c.CorpID, facts.Name, esp.CorpFieldChange{Column: "stationID", Old: oldHQ, New: to.StationID})
	if esp.CorpHQChangedHook != nil {
		esp.CorpHQChangedHook(c.CorpID, oldHQ, to.StationID)
	}
	// Members' addresses follow the headquarters on their next look.
	if ids, err := int64Column(ctx, m, `
		SELECT c.characterID FROM character_ c
		JOIN rolebot_characters b ON b.characterID = c.characterID
		WHERE c.corporationID = ?`, c.CorpID); err == nil {
		for _, id := range ids {
			forgetCorpHome(id)
		}
	}
	m.ceoLog(ctx, c, powerEstate, "hq", to.StationID, 0,
		"moved the headquarters of %s to %s", facts.Name, to.Name)
	return true, nil
}

// nomadic reports whether the charter's CEO lives on the move; those keep a
// second address.
func (ch Charter) nomadic() bool { return ch.Nomad }

// stationNameOf is a best-effort station name.
func stationNameOf(ctx context.Context, m *Mod, stationID int64) string {
	return m.stationNameOr(ctx, stationID)
}
