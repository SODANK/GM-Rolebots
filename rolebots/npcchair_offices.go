package rolebots

// Owner offices for NPC chairs. An NPC corporation owns its stations, so it
// never rents: the chair opens a real office (evedb.OpenOwnerOffice, no rent,
// no bill) at its headquarters and at the owned stations most of its members
// live at, up to the npcChairOffices dial, and closes an empty one its members
// have left. Nothing is deposited into these hangars: readCorpDepositTarget
// and charterToCorp skip NPC corporations, because no NPC desk sells or uses
// stock. See docs/CORPS.md §9a.

import (
	"context"
	"errors"
	"sort"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

const (
	// npcOfficeReviewSec is how often the chair re-reads where its members live.
	npcOfficeReviewSec = 7 * 86400
	// npcOfficeMinMembers is how many members must live at an owned station
	// before it is worth an office. The headquarters needs none.
	npcOfficeMinMembers = 5
	// npcOfficeSlotReserve is how many office slots a station keeps free for
	// renters: an NPC chair never takes the last of them.
	npcOfficeSlotReserve = 4
)

// npcOfficeCand is one station the corporation owns, with how many members
// live there.
type npcOfficeCand struct {
	StationID int64
	Members   int
	HQ        bool
}

// pickNPCOffices is where the chair keeps its offices: the headquarters
// first, then the owned stations with at least minMembers living there, most
// first, n in all. Pure.
func pickNPCOffices(cands []npcOfficeCand, n, minMembers int) []int64 {
	list := append([]npcOfficeCand(nil), cands...)
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].HQ != list[j].HQ {
			return list[i].HQ
		}
		if list[i].Members != list[j].Members {
			return list[i].Members > list[j].Members
		}
		return list[i].StationID < list[j].StationID
	})
	var out []int64
	for _, c := range list {
		if len(out) >= n {
			break
		}
		if c.HQ || c.Members >= minMembers {
			out = append(out, c.StationID)
		}
	}
	return out
}

// npcOfficeCands reads the stations the corporation owns where a member lives,
// and its headquarters.
func (m *Mod) npcOfficeCands(ctx context.Context, corpID int64) ([]npcOfficeCand, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, COUNT(ch.characterID) AS members, s.stationID = COALESCE(h.stationID, 0) AS hq
		  FROM staStations s
		  JOIN corporation h ON h.corporationID = s.corporationID
		  LEFT JOIN character_ ch ON ch.homeStationID = s.stationID AND ch.corporationID = s.corporationID
		 WHERE s.corporationID = ?
		 GROUP BY s.stationID, h.stationID
		HAVING members > 0 OR hq = 1`, corpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []npcOfficeCand
	for rows.Next() {
		var c npcOfficeCand
		if err := rows.Scan(&c.StationID, &c.Members, &c.HQ); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// npcOwnerOffices is the corporation's offices at stations it owns, by
// station.
func (m *Mod) npcOwnerOffices(ctx context.Context, corpID int64) (map[int64]int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, o.itemID FROM crpoffices o
		  JOIN staStations s ON s.stationID = o.stationID AND s.corporationID = o.corporationID
		 WHERE o.corporationID = ?`, corpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var st, office int64
		if err := rows.Scan(&st, &office); err != nil {
			return nil, err
		}
		out[st] = office
	}
	return out, rows.Err()
}

// npcOfficeSlotsFree reports whether a station keeps npcOfficeSlotReserve
// office slots free after one more office.
func (m *Mod) npcOfficeSlotsFree(ctx context.Context, stationID int64) bool {
	var used int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM crpoffices WHERE stationID = ?`, stationID).Scan(&used); err != nil {
		return false
	}
	return used+1+npcOfficeSlotReserve <= evedb.FlagOfficeSlotLast-evedb.FlagOfficeSlotFirst+1
}

// officeEmpty reports whether nothing sits in an office's hangars.
func (m *Mod) officeEmpty(ctx context.Context, officeID int64) bool {
	var one int
	err := m.db.QueryRowContext(ctx, `SELECT 1 FROM entity WHERE locationID = ? LIMIT 1`, officeID).Scan(&one)
	return err != nil
}

// npcChairOfficePass opens the owner offices the chair wants and closes the
// empty ones it no longer wants. An office with something in it is kept.
func (m *Mod) npcChairOfficePass(ctx context.Context, c *corpCEO) {
	cands, err := m.npcOfficeCands(ctx, c.CorpID)
	if err != nil {
		rlogf("rolebots: NPC chair %d offices: %v", c.CorpID, err)
		return
	}
	have, err := m.npcOwnerOffices(ctx, c.CorpID)
	if err != nil {
		rlogf("rolebots: NPC chair %d offices: %v", c.CorpID, err)
		return
	}
	members := map[int64]int{}
	for _, cd := range cands {
		members[cd.StationID] = cd.Members
	}
	want := map[int64]bool{}
	for _, st := range pickNPCOffices(cands, int(npcChairOffices()), npcOfficeMinMembers) {
		want[st] = true
		if have[st] != 0 || !m.npcOfficeSlotsFree(ctx, st) {
			continue
		}
		officeID, folderID, err := evedb.OpenOwnerOffice(ctx, m.db, c.CorpID, st)
		if errors.Is(err, evedb.ErrOfficeExists) || errors.Is(err, evedb.ErrNoFreeOffices) {
			continue
		}
		if err != nil {
			rlogf("rolebots: NPC chair %d open office at %d: %v", c.CorpID, st, err)
			continue
		}
		if h := modapi.Corp().OfficeRentalChanged; h != nil {
			h(c.CorpID, st, officeID, folderID)
		}
		m.ceoLog(ctx, c, powerRoles, "office", st, 0,
			"opened the corporation's office at %s, where %d member(s) live", stationNameOf(ctx, m, st), members[st])
	}
	for st, office := range have {
		if want[st] || !m.officeEmpty(ctx, office) {
			continue
		}
		if m.releaseNPCOffice(ctx, c.CorpID, st) {
			m.ceoLog(ctx, c, powerRoles, "office.close", st, 0,
				"closed the corporation's office at %s: its members have moved on", stationNameOf(ctx, m, st))
		}
	}
}

// releaseNPCOffice gives one owner office up through the core seam, so online
// members' clients drop it. Reports whether it went.
func (m *Mod) releaseNPCOffice(ctx context.Context, corpID, stationID int64) bool {
	release := modapi.Corp().ReleaseOffice
	if release == nil {
		return false
	}
	officeID, _, err := release(corpID, stationID)
	if err != nil {
		rlogf("rolebots: NPC corp %d release office at %d: %v", corpID, stationID, err)
		return false
	}
	return officeID != 0
}

// releaseEmptyNPCOffices closes every empty owner office of a corporation
// whose chair is going.
func (m *Mod) releaseEmptyNPCOffices(ctx context.Context, corpID int64) {
	have, err := m.npcOwnerOffices(ctx, corpID)
	if err != nil {
		return
	}
	for st, office := range have {
		if m.officeEmpty(ctx, office) {
			m.releaseNPCOffice(ctx, corpID, st)
		}
	}
}

// npcOfficeStations lists the stations an NPC corporation holds an owner
// office at, for the Chair page.
func (m *Mod) npcOfficeStations(ctx context.Context, corpID int64) []int64 {
	have, err := m.npcOwnerOffices(ctx, corpID)
	if err != nil {
		return nil
	}
	out := make([]int64, 0, len(have))
	for st := range have {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
