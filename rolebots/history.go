package rolebots

import (
	"context"
	"fmt"
	mrand "math/rand"
	"time"

	evedb "gurimoon/db"
)

// A past for the Capsuleers that are not green.
//
// A trained Capsuleer gets a career that started years ago, employers it worked
// for in order, and a home at the last of them. All of it lives in columns the
// client already reads (character_.startDateTime, chrEmployment, homeStationID),
// so Show Info reads as a real history. Green is untouched: it is new, and the
// module's skill trainer grows it.

// Every date column written here is win32 FILETIME (chrEmployment.startDate,
// character_.startDateTime and createDateTime, corporationDateTime): a unix value
// renders as the year 1601 in the client. win32From (piracy_prey.go) converts.

const (
	// historySchoolActivity is crpNPCCorporations.mainActivityID 18. Excluded from
	// the employer pool, since a history that loops back through the academy is not
	// a history.
	historySchoolActivity = 18

	// criminalPirateOdds is how often a criminal's employer is drawn from the lawless
	// end of its own empire's corporations rather than the whole pool; most criminals
	// hold an ordinary job.
	criminalPirateOdds = 40
)

// historyEmployer is one NPC corporation a Capsuleer can have worked for.
type historyEmployer struct {
	CorpID    int64
	Name      string
	StationID int64
	MinSec    float64
	// Sansha is a Sansha's Nation corporation, which nobody leaves (corpIsSansha,
	// corpjoin.go), so it can only be the last employer in a history.
	Sansha bool
}

// employerPool lists the NPC corporations of one empire that could have employed
// a pilot: they have a headquarters station (the staStations join, since the last
// employer is where the pilot ends up living), they are not schools, and the
// operator has opened them (the rolebot_corps join; a missing row means shut).
func (m *Mod) employerPool(ctx context.Context, raceID int64) ([]historyEmployer, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT n.corporationID, c.corporationName, c.stationID,
		       COALESCE(n.minSecurity, 0), COALESCE(f.factionName, '')
		FROM crpNPCCorporations n
		JOIN corporation c ON c.corporationID = n.corporationID
		JOIN staStations sta ON sta.stationID = c.stationID
		LEFT JOIN chrFactions f ON f.factionID = n.factionID
		LEFT JOIN rolebot_corps p ON p.corporationID = n.corporationID
		WHERE c.raceID = ? AND COALESCE(n.mainActivityID, 0) <> ?
		  AND COALESCE(p.joinable, 0) = 1`,
		raceID, historySchoolActivity)
	if err != nil {
		return nil, fmt.Errorf("employerPool: %w", err)
	}
	defer rows.Close()
	var out []historyEmployer
	for rows.Next() {
		var e historyEmployer
		var faction string
		if err := rows.Scan(&e.CorpID, &e.Name, &e.StationID, &e.MinSec, &faction); err != nil {
			return nil, fmt.Errorf("employerPool scan: %w", err)
		}
		e.Sansha = normFactionName(faction) == "Sansha's Nation"
		out = append(out, e)
	}
	return out, rows.Err()
}

// pickEmployers draws n distinct employers in the order they were worked for. A
// criminal sometimes leans toward corporations with a low minSecurity, which take
// a pilot with a record.
func pickEmployers(pool []historyEmployer, n int, criminal bool, rng *mrand.Rand) []historyEmployer {
	if len(pool) == 0 || n <= 0 {
		return nil
	}
	var shady []historyEmployer
	for _, e := range pool {
		if e.MinSec <= 0 {
			shady = append(shady, e)
		}
	}
	used := map[int64]bool{}
	var out []historyEmployer
	for len(out) < n {
		src := pool
		if criminal && len(shady) > 0 && rng.Intn(100) < criminalPirateOdds {
			src = shady
		}
		// Bounded: a pool with fewer distinct entries than n stops the run
		// short rather than looping forever on collisions.
		picked := false
		for try := 0; try < 40; try++ {
			e := src[rng.Intn(len(src))]
			if used[e.CorpID] {
				continue
			}
			used[e.CorpID] = true
			out = append(out, e)
			picked = true
			break
		}
		// A career that reaches the Nation ends there: nobody leaves it.
		if !picked || out[len(out)-1].Sansha {
			break
		}
	}
	return out
}

// careerShape decides how long a Capsuleer has been flying and how many
// employers it got through. Seasoned is a few months to a year at one place;
// a veteran is three to five years across two or three.
func careerShape(mt maturity, rng *mrand.Rand) (span time.Duration, jobs int) {
	const day = 24 * time.Hour
	switch mt.Key {
	case "veteran":
		return time.Duration(3*365+rng.Intn(2*365)) * day, 2 + rng.Intn(2)
	case "seasoned":
		return time.Duration(90+rng.Intn(275)) * day, 1
	}
	return 0, 0
}

// applyEmploymentHistory backdates a trained Capsuleer's career, walks it through
// its employers, and moves it in with the last of them. createBot calls it in the
// soft-error block: a pilot with no backstory is bland, not broken.
func (m *Mod) applyEmploymentHistory(ctx context.Context, charID, raceID int64, mt maturity, criminal bool, rng *mrand.Rand) error {
	span, jobs := careerShape(mt, rng)
	if span <= 0 || jobs <= 0 {
		return nil // green: it really is new
	}

	pool, err := m.employerPool(ctx, raceID)
	if err != nil {
		return err
	}
	employers := pickEmployers(pool, jobs, criminal, rng)
	if len(employers) == 0 {
		return fmt.Errorf("no OPEN NPC employer with a headquarters for race %d — "+
			"every corporation of that race is shut to bots on the corporations page", raceID)
	}

	now := time.Now()
	careerStart := now.Add(-span)

	// The career begins at the school, where CreateCharacter already put it: that
	// row is kept and backdated. Then the employers, each for a share of the rest.
	schoolStint := time.Duration(14+rng.Intn(60)) * 24 * time.Hour
	if mt.Key == "veteran" {
		schoolStint = time.Duration(30+rng.Intn(90)) * 24 * time.Hour
	}
	if schoolStint > span/2 {
		schoolStint = span / 2
	}

	// Stint boundaries: the moment each employer was joined, ascending.
	starts := make([]time.Time, 0, len(employers))
	remaining := span - schoolStint
	cursor := careerStart.Add(schoolStint)
	for i := range employers {
		starts = append(starts, cursor)
		if i == len(employers)-1 {
			break
		}
		// Divide what is left roughly evenly, jittered, never under a third of the even
		// share: a two-week job in a four-year career reads as a data error.
		even := remaining / time.Duration(len(employers)-i)
		stint := even/3 + time.Duration(rng.Int63n(int64(even)))
		cursor = cursor.Add(stint)
		remaining -= stint
	}

	// ── writes ──────────────────────────────────────────────────────────────

	// Born when the career began: Show Info reads startDateTime.
	start32 := win32From(careerStart)
	if _, err := m.db.ExecContext(ctx, `
		UPDATE character_ SET createDateTime = ?, startDateTime = ?
		WHERE characterID = ?`, start32, start32, charID); err != nil {
		return fmt.Errorf("backdate character: %w", err)
	}
	// The chargen employment row, moved back to the same moment.
	if _, err := m.db.ExecContext(ctx, `
		UPDATE chrEmployment SET startDate = ? WHERE characterID = ?`,
		start32, charID); err != nil {
		return fmt.Errorf("backdate school employment: %w", err)
	}

	// Every employer but the last is history only (a record row, no membership):
	// JoinCorporation would churn memberCount and roles for each.
	for i := 0; i < len(employers)-1; i++ {
		if _, err := m.db.ExecContext(ctx, `
			INSERT IGNORE INTO chrEmployment (characterID, corporationID, startDate, deleted)
			VALUES (?, ?, ?, 0)`,
			charID, employers[i].CorpID, win32From(starts[i])); err != nil {
			return fmt.Errorf("employment history: %w", err)
		}
	}

	// The last employer is a real join (JoinCorporation sets memberCount, roles and
	// corporationID); it stamps now, so both dates it wrote are moved back.
	last := employers[len(employers)-1]
	lastStart := win32From(starts[len(starts)-1])
	var oldCorp int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&oldCorp)
	if _, err := evedb.JoinCorporation(ctx, m.db, charID, last.CorpID, oldCorp); err != nil {
		return fmt.Errorf("join %s: %w", last.Name, err)
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE chrEmployment SET startDate = ?
		WHERE characterID = ? AND corporationID = ?
		ORDER BY startDate DESC LIMIT 1`, lastStart, charID, last.CorpID); err != nil {
		return fmt.Errorf("backdate current employment: %w", err)
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE character_ SET corporationDateTime = ? WHERE characterID = ?`,
		lastStart, charID); err != nil {
		return fmt.Errorf("backdate corp date: %w", err)
	}

	// It lives where it works, so it does not wake up docked at the academy it left,
	// whose region would scope its economic decisions (residency.go).
	if err := m.relocateToStation(ctx, charID, last.StationID); err != nil {
		return fmt.Errorf("relocate to %s: %w", last.Name, err)
	}

	// Both residency caches are keyed on the old home.
	forgetHomeRegion(charID)
	forgetCorpHome(charID)

	// Commit it: without a rolebot_corp_reviews row (heldCorpID set, lastMoveAt
	// backdated) the next plan pass would ship the pilot back onto the open market.
	traits, err := getTraits(ctx, m.db, charID)
	if err != nil {
		return fmt.Errorf("commit employment: %w", err)
	}
	fromCorp := oldCorp
	if len(employers) > 1 {
		fromCorp = employers[len(employers)-2].CorpID
	}
	window := corpReviewEvery(traits)
	if window < 60 {
		window = 60
	}
	rev := &corpReviewRow{
		CharID:       charID,
		LastReviewAt: 0,
		NextReviewAt: now.Unix() + charID%window,
		LastMoveAt:   starts[len(starts)-1].Unix(),
		FromCorpID:   fromCorp,
		SchoolCorpID: oldCorp,
		HeldCorpID:   last.CorpID,
		Moves:        len(employers) - 1,
	}
	if err := saveCorpReview(ctx, m.db, rev); err != nil {
		return fmt.Errorf("commit employment: %w", err)
	}
	return nil
}

// relocateToStation docks a character at a station and makes it home; the
// location columns and homeStationID move together. The docking half is
// dockCharacterAt (migrate.go), because a character's own entity row has
// locationID = the ship it sits in, so writing character_ alone strands the capsule.
func (m *Mod) relocateToStation(ctx context.Context, charID, stationID int64) error {
	if stationID == 0 {
		return nil
	}
	if err := m.dockCharacterAt(ctx, charID, stationID); err != nil {
		return err
	}
	return evedb.SetHomeStationID(ctx, m.db, charID, stationID)
}
