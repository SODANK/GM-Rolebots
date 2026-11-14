package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// Self-founding: a leader starting a corporation of its own.
// Who: a pilot whose Leadership puts FoundPropensity above zero (tuning.go).
// When: at its employment review (corpjoin.go corpReview), docked, out of
// school, in an NPC corporation with no application outstanding; a roll of
// FoundPropensity x corpFoundChance, the switch (settingCorpFound, default off),
// the cap (settingCorpFoundMax) and one global pace. Where: the station it is
// docked in becomes the HQ, and its region must be one a name may carry
// (corpbrand.go). What: the client's Create Corporation without the wire
// (handleBoundAddCorporation): fee charged first and refunded on failure, the
// corporation, the move into it, then installCEO (origin founded).

const (
	settingCorpFound    = "rolebots.corpFound"
	settingCorpFoundMax = "rolebots.corpFoundMax"

	defCorpFoundMax = 100
	maxCorpFoundMax = 1000

	// corpFoundPaceSec is the least time between two foundings, server-wide.
	corpFoundPaceSec = 600
	// corpFoundChance scales FoundPropensity into a per-review chance.
	corpFoundChance = 0.25
	// corpFoundReserveMul: a founder keeps as much again as the fee, so its
	// new corporation does not open with a broke CEO.
	corpFoundReserveMul = 2.0
	corpFoundRerolls    = 64
	corpFoundTaxRate    = 0.10

	brandLoreTTLSec = 3600
)

var (
	corpFoundEnabled atomic.Bool
	corpFoundMaxV    atomic.Int64
	corpFoundLastAt  atomic.Int64
	// corpFoundMu serialises foundings: two pilots passing the cap check together
	// would both found.
	corpFoundMu sync.Mutex
)

func init() { corpFoundMaxV.Store(defCorpFoundMax) }

// CorpFound reports whether rolebots may found corporations.
func CorpFound() bool { return corpFoundEnabled.Load() }

func corpFoundMax() int { return int(corpFoundMaxV.Load()) }

// loadCorpFoundSettings reads the switch, the cap and the last founding.
func (m *Mod) loadCorpFoundSettings(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingCorpFound); err != nil {
		rlogf("rolebots: read founding setting: %v (leaving it off)", err)
	} else if ok {
		corpFoundEnabled.Store(v == "1")
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingCorpFoundMax); err == nil && ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			corpFoundMaxV.Store(int64(n))
		}
	}
	var last int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(foundedAt), 0) FROM rolebot_founded_corps`).Scan(&last); err == nil {
		corpFoundLastAt.Store(last)
	}
}

// ── the record ──────────────────────────────────────────────────────────────

type foundedCorp struct {
	CorpID, FounderID int64
	FoundedAt         int64
	RegionName        string
	Template, Org     string
	Adj               string
	Name, Ticker      string
	FounderName       string
	// Tags are the corporation's charter tags; Sector is the founder's sector at
	// founding, read while the identity power has not tagged it yet.
	Tags, Sector string
}

// liveFoundedCorps lists the bot-founded corporations still open, the ones that
// count against the cap. A closed corporation frees its slot.
func (m *Mod) liveFoundedCorps(ctx context.Context) ([]foundedCorp, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT f.corporationID, f.founderID, f.foundedAt, f.regionName,
		       f.template, f.org, f.adj, c.corporationName, c.tickerName,
		       COALESCE(ch.characterName, ''), COALESCE(rc.tags, ''), f.sector
		  FROM rolebot_founded_corps f
		  JOIN corporation c ON c.corporationID = f.corporationID
		  LEFT JOIN character_ ch ON ch.characterID = f.founderID
		  LEFT JOIN rolebot_corps rc ON rc.corporationID = f.corporationID
		 WHERE COALESCE(c.deleted, 0) = 0
		 ORDER BY f.foundedAt DESC`)
	if err != nil {
		return nil, fmt.Errorf("liveFoundedCorps: %w", err)
	}
	defer rows.Close()
	var out []foundedCorp
	for rows.Next() {
		var f foundedCorp
		if err := rows.Scan(&f.CorpID, &f.FounderID, &f.FoundedAt, &f.RegionName,
			&f.Template, &f.Org, &f.Adj, &f.Name, &f.Ticker, &f.FounderName, &f.Tags, &f.Sector); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// When is the founding time as the panel prints it.
func (f foundedCorp) When() string {
	return time.Unix(f.FoundedAt, 0).UTC().Format("2006-01-02 15:04")
}

func diversityOf(live []foundedCorp) *brandDiversity {
	d := newBrandDiversity()
	for _, f := range live {
		d.add(corpBrand{Template: f.Template, Org: f.Org, Adj: f.Adj}, f.RegionName)
	}
	return d
}

// ── lore ────────────────────────────────────────────────────────────────────

var brandLoreCache struct {
	sync.Mutex
	lore *brandLore
	at   int64
}

// brandLoreNow is the lore set with the database's NPC corporation and faction
// names in it, rebuilt hourly. A failed read falls back to the static lists.
func (m *Mod) brandLoreNow(ctx context.Context) *brandLore {
	brandLoreCache.Lock()
	defer brandLoreCache.Unlock()
	now := clock.Unix()
	if brandLoreCache.lore != nil && now-brandLoreCache.at < brandLoreTTLSec {
		return brandLoreCache.lore
	}
	read := func(q string) []string {
		rows, err := m.db.QueryContext(ctx, q)
		if err != nil {
			rlogf("rolebots: brand lore: %v", err)
			return nil
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil {
				out = append(out, s)
			}
		}
		return out
	}
	npc := read(`SELECT c.corporationName FROM corporation c
		JOIN crpNPCCorporations n ON n.corporationID = c.corporationID`)
	npc = append(npc, read(`SELECT factionName FROM chrFactions`)...)
	brandLoreCache.lore = newBrandLore(npc)
	brandLoreCache.at = now
	return brandLoreCache.lore
}

// ── the decision ────────────────────────────────────────────────────────────

// errFoundSkip is a refusal that is nobody's fault: no money, no uncrowded
// station, nothing left to call it. The review simply moves on.
var errFoundSkip = errors.New("not founding")

const (
	// corpHQPerSystemMax is the most player-corporation headquarters one system
	// takes before a new founder looks elsewhere.
	corpHQPerSystemMax = 3
	// corpFoundSpreadRadius is how far (raw jumps) a founder looks for an
	// uncrowded headquarters.
	corpFoundSpreadRadius = 12
)

// founderFacts is everything the founding decision reads about one pilot.
type founderFacts struct {
	CharID, CorpID      int64
	StationID, SystemID int64
	Name                string
	RaceID              int64
	Balance, SecStatus  float64
	Traits              map[string]int
	Propensity          float64
	Tags                []string
	Kind                string
	Profile             *Profile
}

// loadFounderFacts reads one pilot for the founding decision.
func (m *Mod) loadFounderFacts(ctx context.Context, charID int64) (*founderFacts, error) {
	f := &founderFacts{CharID: charID}
	if err := m.db.QueryRowContext(ctx, `
		SELECT c.characterName, c.corporationID, COALESCE(c.stationID, 0),
		       COALESCE(c.solarSystemID, 0), c.balance, COALESCE(c.securityRating, 0),
		       COALESCE(b.raceID, 0)
		  FROM character_ c
		  LEFT JOIN chrBloodlines b ON b.bloodlineID = c.bloodlineID
		 WHERE c.characterID = ?`, charID).Scan(&f.Name, &f.CorpID, &f.StationID,
		&f.SystemID, &f.Balance, &f.SecStatus, &f.RaceID); err != nil {
		return nil, fmt.Errorf("founder %d: %w", charID, err)
	}
	traits, err := getTraits(ctx, m.db, charID)
	if err != nil {
		return nil, err
	}
	f.Traits = traits
	f.Propensity = resolveTuning(traits).FoundPropensity
	f.Tags = charterTags(charID, traits)
	f.Kind = titleKindOf(f.Tags)
	if p, err := m.getProfile(ctx, charID); err == nil {
		f.Profile = p
	}
	return f, nil
}

// botRunOf is the minimal botRun the travel rules read.
func (f *founderFacts) botRunOf() *botRun {
	return &botRun{CharID: f.CharID, Name: f.Name, CorpID: f.CorpID, SystemID: f.SystemID,
		StationID: f.StationID, Profile: f.Profile, Tune: resolveTuning(f.Traits), SecStatus: f.SecStatus}
}

// foundRefusal says why this pilot cannot found a corporation right now, ""
// when it can. Shared by the review and the panel's probe. The roll, the
// pace, the switch and the cap are not part of it: those belong to the
// review (and the cap to whoever founds).
func (m *Mod) foundRefusal(ctx context.Context, f *founderFacts) string {
	switch {
	case f.Propensity <= 0:
		return fmt.Sprintf("not a leader (Leadership %d)", f.Traits[TraitLeadership])
	case f.StationID == 0:
		return "in space"
	}
	if other, ok := managedCEOOf(f.CharID); ok {
		return fmt.Sprintf("already runs corporation %d", other)
	}
	if npc, err := isNPCCorp(ctx, m.db, f.CorpID); err != nil || !npc {
		return "already in a player corporation"
	}
	if corpIsSchool(ctx, m.db, f.CorpID) {
		return "still at school"
	}
	if corpIsConcord(ctx, m.db, f.CorpID) {
		return "holds a CONCORD seat"
	}
	if corpIsSansha(ctx, m.db, f.CorpID) {
		return "belongs to Sansha's Nation"
	}
	// Applications outstanding do not stop a founding: foundCorp withdraws them once the
	// corporation exists.
	if need := corpFoundReserveMul * evedb.CorporationStartupCost; f.Balance < need {
		return fmt.Sprintf("wallet %.1fM short of %.1fM", f.Balance/1e6, need/1e6)
	}
	return ""
}

// considerFounding is the review's founding look. On a founding it updates
// rev to the new corporation (the caller saves it) and reports true, also when
// a later step failed after the corporation was created (the error says what).
func (m *Mod) considerFounding(ctx context.Context, b *botRun, traits map[string]int,
	rev *corpReviewRow) (bool, error) {
	if !CorpFound() || b.StationID == 0 {
		return false, nil
	}
	prop := resolveTuning(traits).FoundPropensity
	if prop <= 0 {
		return false, nil
	}
	if clock.Unix()-corpFoundLastAt.Load() < corpFoundPaceSec {
		return false, nil
	}
	// A founder whose sector is full would be refused after the roll: it keeps
	// its roll for a review where the balance has room. A scarce sector's
	// founder rolls harder (foundSectorBoost).
	fits, boost := m.founderSectorFits(ctx, b.CharID, traits)
	if !fits {
		return false, nil
	}
	if rand.Float64() >= prop*corpFoundChance*boost {
		return false, nil
	}
	corpID, err := m.foundNow(ctx, b.CharID, "self", false, true)
	if errors.Is(err, errFoundSkip) {
		return false, nil
	}
	if corpID == 0 {
		return false, err
	}
	rev.FromCorpID = b.CorpID
	rev.HeldCorpID = corpID
	rev.LastMoveAt = clock.Unix()
	rev.Moves++
	return true, err
}

// foundNow is the whole founding for one pilot: refusals, cap, sector balance,
// HQ, brand, the corporation. by is who asked ("self", or the panel). ignorePace
// lets the panel's test button through the global pace; the cap always holds.
// balanced applies the sector balance (corpfound_sector.go); an operator naming
// one pilot skips it.
func (m *Mod) foundNow(ctx context.Context, charID int64, by string, ignorePace, balanced bool) (int64, error) {
	if !corpFoundMu.TryLock() {
		return 0, fmt.Errorf("%w: another founding is in progress", errFoundSkip)
	}
	defer corpFoundMu.Unlock()
	if !ignorePace && clock.Unix()-corpFoundLastAt.Load() < corpFoundPaceSec {
		return 0, fmt.Errorf("%w: a corporation was founded less than %d minutes ago", errFoundSkip, corpFoundPaceSec/60)
	}
	live, err := m.liveFoundedCorps(ctx)
	if err != nil {
		return 0, err
	}
	if len(live) >= corpFoundMax() {
		return 0, fmt.Errorf("%w: the cap of %d live bot-founded corporations is reached", errFoundSkip, corpFoundMax())
	}
	f, err := m.loadFounderFacts(ctx, charID)
	if err != nil {
		return 0, err
	}
	if why := m.foundRefusal(ctx, f); why != "" {
		return 0, fmt.Errorf("%w: %s", errFoundSkip, why)
	}
	orig := *f
	var nudged map[string]int
	var ownSector error
	if balanced {
		if nudged, ownSector = m.balanceFounder(ctx, f, live); nudged == nil && ownSector != nil {
			return 0, ownSector
		}
	}
	hq, err := m.planFoundingHQ(ctx, f)
	if err != nil && nudged != nil {
		// No laboratory in reach: found the factory it would have founded, if its sector has room.
		if ownSector != nil {
			return 0, ownSector
		}
		*f, nudged = orig, nil
		hq, err = m.planFoundingHQ(ctx, f)
	}
	if err != nil {
		return 0, err
	}
	corpID, br, err := m.foundCorp(ctx, f, hq, live, by)
	if corpID != 0 && nudged != nil {
		m.saveFounderTraits(ctx, charID, orig.Traits, nudged, br.Name)
	}
	return corpID, err
}

// ── where the headquarters goes ─────────────────────────────────────────────

// foundHQ is where a new corporation will be headquartered.
type foundHQ struct {
	StationID, SystemID, RegionID int64
	RegionName                    string
	// Spread is set when the founder's own base was crowded (or unusable)
	// and this is somewhere else: the founder moves its home here.
	Spread bool
	// Why says how the station was chosen, for the logs and the probe.
	Why string
}

// founderHQIsFactory: an industrial or research outfit is headquartered at
// its founder's factory base; everyone else (combat, trade, mining, a plain
// house) at its clone station.
func founderHQIsFactory(kind string) bool {
	return kind == tvWorks || kind == tvYard || kind == tvLab || kind == tvCartel
}

// hqStationFits: a station can be this kind's headquarters.
func (m *Mod) hqStationFits(ctx context.Context, kind string, stationID int64) bool {
	switch {
	case stationID == 0:
		return false
	case kind == tvLab:
		return m.stationHasLab(ctx, stationID)
	case founderHQIsFactory(kind):
		return m.stationHasLine(ctx, stationID)
	}
	return m.stationHasCloning(ctx, stationID)
}

// playerHQsBySystem counts open player corporations' headquarters per system.
func (m *Mod) playerHQsBySystem(ctx context.Context) (map[int64]int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.solarSystemID, COUNT(*)
		  FROM corporation c
		  JOIN staStations s ON s.stationID = c.stationID
		  LEFT JOIN crpNPCCorporations n ON n.corporationID = c.corporationID
		 WHERE n.corporationID IS NULL AND COALESCE(c.deleted, 0) = 0
		 GROUP BY s.solarSystemID`)
	if err != nil {
		return nil, fmt.Errorf("player HQs: %w", err)
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var sys int64
		var n int
		if rows.Scan(&sys, &n) == nil {
			out[sys] = n
		}
	}
	return out, rows.Err()
}

func (m *Mod) stationRegion(ctx context.Context, stationID int64) (sysID, regionID int64, region string, err error) {
	err = m.db.QueryRowContext(ctx, `
		SELECT s.solarSystemID, r.regionID, r.regionName
		  FROM staStations s JOIN mapRegions r ON r.regionID = s.regionID
		 WHERE s.stationID = ?`, stationID).Scan(&sysID, &regionID, &region)
	return
}

// planFoundingHQ picks the headquarters: the founder's own base for its kind
// (factory base for industry and research, clone station otherwise), else the
// nearest fitting station within corpFoundSpreadRadius jumps and the founder's
// travel rules, in a system below corpHQPerSystemMax.
func (m *Mod) planFoundingHQ(ctx context.Context, f *founderFacts) (foundHQ, error) {
	hqs, err := m.playerHQsBySystem(ctx)
	if err != nil {
		return foundHQ{}, err
	}
	base, baseWhat := int64(0), "clone station"
	if founderHQIsFactory(f.Kind) {
		baseWhat = "factory base"
		if f.Profile != nil {
			base = f.Profile.IndustryStationID
		}
	} else {
		base = m.homeStationFor(ctx, f.CharID)
	}
	from := f.SystemID
	why := fmt.Sprintf("no usable %s", baseWhat)
	if m.hqStationFits(ctx, f.Kind, base) {
		sys, regID, region, err := m.stationRegion(ctx, base)
		if err == nil {
			from = sys
			switch {
			case hqs[sys] >= corpHQPerSystemMax:
				why = fmt.Sprintf("its %s's system already holds %d headquarters", baseWhat, hqs[sys])
			case !regionNamable(region):
				why = fmt.Sprintf("its %s is in %s, which is never named", baseWhat, region)
			default:
				return foundHQ{StationID: base, SystemID: sys, RegionID: regID, RegionName: region,
					Why: "its " + baseWhat}, nil
			}
		}
	}

	// Spread out.
	rules := rulesForBot(f.botRunOf())
	u, err := m.universe(ctx)
	if err != nil {
		return foundHQ{}, err
	}
	near, err := m.corpJumpMap(ctx, from)
	if err != nil {
		return foundHQ{}, err
	}
	var cands []medStation
	if founderHQIsFactory(f.Kind) {
		if cands, err = m.factoryStationsNear(ctx, near, corpFoundSpreadRadius); err != nil {
			return foundHQ{}, err
		}
	} else {
		bays, err := m.medicalStations(ctx)
		if err != nil {
			return foundHQ{}, err
		}
		for sys, list := range bays {
			if d, ok := near[sys]; ok && d <= corpFoundSpreadRadius {
				cands = append(cands, list...)
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		di, dj := near[cands[i].SystemID], near[cands[j].SystemID]
		if di != dj {
			return di < dj
		}
		return cands[i].StationID < cands[j].StationID
	})
	for _, c := range cands {
		if hqs[c.SystemID] >= corpHQPerSystemMax {
			continue
		}
		if c.SystemID != from && !u.allowed(c.SystemID, rules) {
			continue
		}
		if f.Kind == tvLab && !m.stationHasLab(ctx, c.StationID) {
			continue
		}
		if f.Profile != nil && m.despisesStationOwner(ctx, f.Profile, c.CorpID) {
			continue
		}
		sys, regID, region, err := m.stationRegion(ctx, c.StationID)
		if err != nil || !regionNamable(region) {
			continue
		}
		return foundHQ{StationID: c.StationID, SystemID: sys, RegionID: regID, RegionName: region,
			Spread: true, Why: fmt.Sprintf("spread out %d jump(s) — %s", near[c.SystemID], why)}, nil
	}
	return foundHQ{}, fmt.Errorf("%w: %s, and no uncrowded station within %d jumps", errFoundSkip, why, corpFoundSpreadRadius)
}

// ── the founding ────────────────────────────────────────────────────────────

// foundCorp founds a corporation for f at hq and seats it as CEO. live is the
// current bot-founded set, for the diversity caps.
func (m *Mod) foundCorp(ctx context.Context, f *founderFacts, hq foundHQ,
	live []foundedCorp, by string) (int64, corpBrand, error) {
	seed := brandSeed{FounderID: f.CharID, FounderName: f.Name, RaceID: f.RaceID,
		Tags: f.Tags, RegionName: hq.RegionName}
	div := diversityOf(live)
	br, ok := pickCorpBrand(seed, m.brandLoreNow(ctx), corpFoundRerolls, func(c corpBrand) bool {
		return div.allows(c, hq.RegionName) && m.corpNameFree(ctx, c.Name) && m.corpLogoFree(ctx, c)
	})
	if !ok {
		return 0, corpBrand{}, fmt.Errorf("%w: no free name in %d rolls", errFoundSkip, corpFoundRerolls)
	}
	ticker := m.freeTicker(ctx, br.Name)
	if ticker == "" {
		return 0, corpBrand{}, fmt.Errorf("%w: no free ticker for %q", errFoundSkip, br.Name)
	}

	charged, _, err := evedb.ChargeCorporationCreation(ctx, m.db, f.CharID)
	if err != nil {
		return 0, br, fmt.Errorf("foundCorp fee: %w", err)
	}
	if !charged {
		return 0, br, fmt.Errorf("%w: the fee was refused", errFoundSkip)
	}
	sh, co := logoArgs(br)
	corpID, err := evedb.AddCorporation(ctx, m.db, f.CharID, br.Name, ticker, br.Description, "",
		corpFoundTaxRate, sh[0], sh[1], sh[2], co[0], co[1], co[2])
	if err != nil {
		if corpID == 0 {
			if _, rerr := evedb.RefundCorporationCreation(ctx, m.db, f.CharID); rerr != nil {
				rlogf("rolebots: founding refund for %d: %v", f.CharID, rerr)
			}
			return 0, br, fmt.Errorf("foundCorp create: %w", err)
		}
		// The row exists and a later step failed: it is a real corporation, so the
		// founder moves in anyway, as the client path keeps the fee.
		rlogf("rolebots: founding %q (%d): %v — continuing", br.Name, corpID, err)
	}
	// From here the corporation exists: it is recorded and paces the next founding
	// even when a later step fails, so it counts against the cap.
	now := clock.Unix()
	recorded := false
	record := func() {
		if recorded {
			return
		}
		recorded = true
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_founded_corps
			  (corporationID, founderID, foundedAt, regionID, regionName, template, org, adj, sector)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			corpID, f.CharID, now, hq.RegionID, hq.RegionName, br.Template, br.Org, br.Adj,
			corpSectorFromTags(f.Tags)); err != nil {
			rlogf("rolebots: founded-corp record %d: %v", corpID, err)
		}
		corpFoundLastAt.Store(now)
		forgetFoundSectors()
	}
	defer record()
	// AddCorporation registers the corporation where the founder is docked. The HQ
	// is written directly, not through evedb.SetCorpHQ, whose rule is about moving.
	if hq.StationID != 0 && hq.StationID != f.StationID {
		if _, err := m.db.ExecContext(ctx,
			`UPDATE corporation SET stationID = ? WHERE corporationID = ?`, hq.StationID, corpID); err != nil {
			rlogf("rolebots: founding %q HQ: %v", br.Name, err)
		}
	}
	if _, err := evedb.JoinCorporation(ctx, m.db, f.CharID, corpID, f.CorpID); err != nil {
		return corpID, br, fmt.Errorf("foundCorp join: %w", err)
	}
	// The founder's applications elsewhere close now, so no offer moves it out of
	// the corporation it just founded.
	m.withdrawOtherApplications(ctx, f.CharID, corpID)
	if err := m.installCEO(ctx, f.CharID, corpID, ceoOriginFounded, by); err != nil {
		return corpID, br, fmt.Errorf("foundCorp install: %w", err)
	}
	// The side it flies for (allegiance.go): once, from the founder's race, charter
	// and Loyalty. The charter tags are passed in because the policy row the CEO
	// install wrote is still untagged; the identity power fills it later.
	if fid, kind, err := m.deriveCorpAllegiance(ctx, corpID, f.RaceID, f.Tags, f.Traits); err != nil {
		rlogf("rolebots: founding %q allegiance: %v", br.Name, err)
	} else if fid != 0 {
		if err := m.setCorpAllegiance(ctx, corpID, fid); err != nil {
			rlogf("rolebots: founding %q allegiance: %v", br.Name, err)
		} else {
			m.notePledge(ctx, corpID, f.CharID, br.Name, fid, kind)
		}
	} else {
		m.logBot(f.CharID, logInfo, "corp", "founded %s unaligned (%s)", br.Name, kind)
	}
	// A founder who spread out moves in: its factory (industrial outfit) and clone
	// go to the new HQ or the nearest station that can host them. Not pinned.
	if hq.Spread {
		if clone, factory, err := m.corpHomeTargets(ctx, hq.StationID); err == nil {
			if founderHQIsFactory(f.Kind) && factory != 0 {
				if err := setIndustryBaseByID(ctx, m.db, f.CharID, factory); err != nil {
					rlogf("rolebots: founder %d factory move: %v", f.CharID, err)
				}
			}
			if clone != 0 {
				m.setHomeStationByID(ctx, f.CharID, clone)
			}
			forgetCloneCheck(f.CharID)
			forgetMoveOut(f.CharID)
			forgetCorpHome(f.CharID)
		}
	}
	record()
	if esp.OwnerNamesInvalidator != nil {
		esp.OwnerNamesInvalidator()
	}
	where := m.stationLabel(ctx, hq.StationID)
	m.logBot(f.CharID, logInfo, "corp", "founded %s [%s], headquartered at %s (%s) — a %s %s",
		br.Name, ticker, where, hq.Why, br.Race, titleKindLabels[br.Kind])
	if c, err := getCEO(ctx, m.db, corpID); err == nil && c != nil {
		m.ceoLog(ctx, c, powerIdentity, "found", f.CharID, evedb.CorporationStartupCost,
			"founded %s [%s], headquartered at %s (%s)", br.Name, ticker, where, hq.Why)
	}
	return corpID, br, nil
}

// ── the panel's probe ───────────────────────────────────────────────────────

// foundProbeRow is one leader on the probe table.
type foundProbeRow struct {
	CharID     int64
	Name       string
	Leadership int
	Propensity float64
	CorpName   string
	Wallet     float64
	Kind       string
	Sector     string // the line of business it would found in
	Refusal    string // "" = ready
	HQ         string // where it would headquarter, when ready
}

// WalletText is the wallet in millions.
func (r foundProbeRow) WalletText() string { return fmt.Sprintf("%.1fM", r.Wallet/1e6) }

// Ready reports whether the row may be founded from the panel.
func (r foundProbeRow) Ready() bool { return r.Refusal == "" }

const foundProbeHQLimit = 25

// probeFounders lists every enabled rostered leader (Leadership past the
// founding threshold) with why it can or cannot found, the ready ones first and
// the scarcest sector's ahead of the rest (foundScarcityOrder). The jump-map HQ
// plan runs only for the first foundProbeHQLimit ready pilots.
func (m *Mod) probeFounders(ctx context.Context) ([]foundProbeRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.characterID
		  FROM rolebot_traits t
		  JOIN rolebot_characters r ON r.characterID = t.characterID AND r.enabled = 1
		 WHERE t.trait = ? AND t.value > ?
		 ORDER BY t.value DESC, t.characterID`, TraitLeadership, int(foundPropensityFrom*100))
	if err != nil {
		return nil, fmt.Errorf("probe founders: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	var out []foundProbeRow
	facts := map[int64]*founderFacts{}
	for _, id := range ids {
		f, err := m.loadFounderFacts(ctx, id)
		if err != nil {
			continue
		}
		row := foundProbeRow{CharID: id, Name: f.Name, Leadership: f.Traits[TraitLeadership],
			Propensity: f.Propensity, CorpName: corpNameOf(ctx, m.db, f.CorpID), Wallet: f.Balance,
			Kind: titleKindLabels[f.Kind], Sector: corpSectorFromTags(f.Tags)}
		row.Refusal = m.foundRefusal(ctx, f)
		if row.Refusal == "" {
			if fits, _ := m.founderSectorFits(ctx, id, f.Traits); !fits {
				row.Refusal = fmt.Sprintf("%s holds its share of the bot-founded corporations", row.Sector)
			}
		}
		facts[id] = f
		out = append(out, row)
	}
	counts, _, err := m.cachedFoundSectors(ctx)
	if err != nil {
		counts = nil
	}
	foundScarcityOrder(out, counts)

	planned := 0
	for i := range out {
		row := &out[i]
		if !row.Ready() {
			continue
		}
		if planned >= foundProbeHQLimit {
			row.HQ = "(not planned — past the first ready pilots)"
			continue
		}
		planned++
		if hq, err := m.planFoundingHQ(ctx, facts[row.CharID]); err != nil {
			row.Refusal = strings.TrimPrefix(err.Error(), errFoundSkip.Error()+": ")
		} else {
			row.HQ = m.stationLabel(ctx, hq.StationID) + " (" + hq.Why + ")"
		}
	}
	// A pilot the HQ plan refused is no longer ready: move it behind the ready ones.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ready() && !out[j].Ready() })
	return out, nil
}

// foundAnyNow founds for the readiest leader that can found, trying each in
// turn until one succeeds. Returns the corporation and the founder's name.
func (m *Mod) foundAnyNow(ctx context.Context, by string) (int64, string, error) {
	probe, err := m.probeFounders(ctx)
	if err != nil {
		return 0, "", err
	}
	var last error
	for _, r := range probe {
		if !r.Ready() || r.HQ == "" || strings.HasPrefix(r.HQ, "(") {
			continue
		}
		corpID, err := m.foundNow(ctx, r.CharID, by, true, true)
		if err == nil {
			return corpID, r.Name, nil
		}
		last = err
		if !errors.Is(err, errFoundSkip) {
			return 0, r.Name, err
		}
	}
	if last == nil {
		last = fmt.Errorf("%w: no rostered leader can found a corporation right now", errFoundSkip)
	}
	return 0, "", last
}

// ── the panel's purge ───────────────────────────────────────────────────────
// botFoundedCorp is an open corporation whose founder is a generated rolebot,
// a character on a RoleBotN account.
type botFoundedCorp struct {
	CorpID      int64
	Name        string
	FounderName string
}

// listBotFoundedCorps finds every open corporation whose creator is on a
// generated account. Reads corporation.creatorID, not rolebot_founded_corps:
// a corporation an operator founded for a bot from the panel counts too.
func (m *Mod) listBotFoundedCorps(ctx context.Context) ([]botFoundedCorp, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.corporationID, c.corporationName, ch.characterName, a.accountName
		  FROM corporation c
		  JOIN character_ ch ON ch.characterID = c.creatorID
		  JOIN account a ON a.accountID = ch.accountID
		 WHERE COALESCE(c.deleted, 0) = 0 AND a.accountName LIKE ?
		 ORDER BY c.corporationID`, botAccountPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("bot-founded corporations: %w", err)
	}
	defer rows.Close()
	var out []botFoundedCorp
	for rows.Next() {
		var c botFoundedCorp
		var account string
		if err := rows.Scan(&c.CorpID, &c.Name, &c.FounderName, &account); err != nil {
			return nil, err
		}
		// Strict on the number, like the purge: RoleBotStaging is not a generated account.
		if _, ok := botSeqOf(account); ok {
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// deleteBotFoundedCorps closes every corporation a rolebot founded through the
// core's own close (evedb.ESPDeleteCorp). A corporation that still owns a
// starbase is refused by the core and reported, not forced.
func (m *Mod) deleteBotFoundedCorps(ctx context.Context) (deleted int, failed []string, err error) {
	list, err := m.listBotFoundedCorps(ctx)
	if err != nil {
		return 0, nil, err
	}
	for _, c := range list {
		res, err := evedb.ESPDeleteCorp(ctx, m.db, c.CorpID)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", c.Name, err))
			continue
		}
		deleted++
		if esp.CorpDeletedHook != nil {
			esp.CorpDeletedHook(c.CorpID, res.OldAllianceID, res.Kicked)
		}
		if err := m.removeCEO(ctx, c.CorpID, "the corporation was closed from the panel"); err != nil {
			rlogf("rolebots: closing %s: %v", c.Name, err)
		}
		m.db.ExecContext(ctx, `DELETE FROM rolebot_founded_corps WHERE corporationID = ?`, c.CorpID)
		// Hold each rostered pilot where the close landed it: a stale record of the
		// closed corporation reads as another mover and sends it back to school.
		for _, k := range res.Kicked {
			if rev, err := getCorpReview(ctx, m.db, k.CharID); err == nil && rev.HeldCorpID == c.CorpID {
				rev.HeldCorpID = k.NPCCorpID
				rev.LastMoveAt = clock.Unix()
				if err := saveCorpReview(ctx, m.db, rev); err != nil {
					rlogf("rolebots: closing %s: review %d: %v", c.Name, k.CharID, err)
				}
			}
			forgetCorpHome(k.CharID)
		}
		rlogf("rolebots: closed %s (founded by %s): %d pilot(s) returned to NPC corporations",
			c.Name, c.FounderName, len(res.Kicked))
	}
	if deleted > 0 {
		m.syncPresence()
		if esp.OwnerNamesInvalidator != nil {
			esp.OwnerNamesInvalidator()
		}
	}
	return deleted, failed, nil
}

func (m *Mod) corpNameFree(ctx context.Context, name string) bool {
	var n int
	err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM corporation WHERE corporationName = ?`, name).Scan(&n)
	return err == nil && n == 0
}

// corpLogoFree: no corporation already flies this exact logo.
func (m *Mod) corpLogoFree(ctx context.Context, b corpBrand) bool {
	sh, co := logoArgs(b)
	var n int
	err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM corporation
		 WHERE shape1 <=> ? AND shape2 <=> ? AND shape3 <=> ?
		   AND color1 <=> ? AND color2 <=> ? AND color3 <=> ?`,
		sh[0], sh[1], sh[2], co[0], co[1], co[2]).Scan(&n)
	return err == nil && n == 0
}

// freeTicker is the client's own suggestion (evedb.SuggestedTickers), or the
// initials with a digit when every suggestion is taken.
func (m *Mod) freeTicker(ctx context.Context, name string) string {
	if s := evedb.SuggestedTickers(ctx, m.db, name); len(s) > 0 {
		return s[0]
	}
	var ini strings.Builder
	for _, w := range strings.Fields(strings.ToUpper(name)) {
		if r := w[0]; r >= 'A' && r <= 'Z' {
			ini.WriteByte(r)
		}
	}
	base := ini.String()
	if len(base) > 4 {
		base = base[:4]
	}
	for d := 1; d <= 9 && base != ""; d++ {
		t := fmt.Sprintf("%s%d", base, d)
		var n int
		if err := m.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM corporation WHERE tickerName = ?`, t).Scan(&n); err == nil && n == 0 {
			return t
		}
	}
	return ""
}

func logoArgs(b corpBrand) (sh, co [3]sql.NullInt64) {
	for i := 0; i < 3; i++ {
		if b.Shapes[i] != 0 {
			sh[i] = sql.NullInt64{Int64: b.Shapes[i], Valid: true}
			co[i] = sql.NullInt64{Int64: b.Colours[i], Valid: true}
		}
	}
	return sh, co
}

// ── the panel's preview ─────────────────────────────────────────────────────

type brandLogoLayer struct {
	Style template.CSS
}

type brandPreviewRow struct {
	corpBrand
	Founder, Region, KindLabel, Ticker string
	// Layers bottom first, so the stacking order in the page is the game's.
	Layers []brandLogoLayer
}

// brandPreviewTags gives one synthetic tag set per kind, in rotation.
var brandPreviewTags = [][]string{
	{"mining"}, {"trade"}, {"industry"}, {"shipbuilding"}, {"research"},
	{"combat", tagMercenary}, {tagSecurity}, {tagCombat}, {tagOutlaw, tagCombat},
	{"cartelindustry"}, {"hauling"}, {"mining", "trade"},
}

// brandPreview generates n sample brands from rostered pilots' real names and
// races, spread over kinds and namable regions, under the same diversity caps
// founding applies. offset reshuffles.
func (m *Mod) brandPreview(ctx context.Context, n, offset int) []brandPreviewRow {
	type pilot struct {
		id     int64
		name   string
		raceID int64
	}
	var pilots []pilot
	if rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName, COALESCE(b.raceID, 0)
		  FROM rolebot_characters r
		  JOIN character_ c ON c.characterID = r.characterID
		  LEFT JOIN chrBloodlines b ON b.bloodlineID = c.bloodlineID
		 ORDER BY c.characterID LIMIT 500`); err == nil {
		for rows.Next() {
			var p pilot
			if rows.Scan(&p.id, &p.name, &p.raceID) == nil {
				pilots = append(pilots, p)
			}
		}
		rows.Close()
	}
	if len(pilots) == 0 {
		for i, r := range []int64{raceAmarr, raceCaldari, raceGallente, raceMinmatar} {
			pilots = append(pilots, pilot{int64(9000 + i), "Example Founder", r})
		}
	}
	var regions []string
	for name, r := range brandRegions {
		if r.Class != regionNever {
			regions = append(regions, name)
		}
	}
	sort.Strings(regions)

	lore := m.brandLoreNow(ctx)
	div := newBrandDiversity()
	var out []brandPreviewRow
	for i := 0; i < n; i++ {
		p := pilots[(i*37+offset*101)%len(pilots)]
		region := regions[(i*13+offset*7)%len(regions)]
		seed := brandSeed{FounderID: p.id + int64(offset)*7919, FounderName: p.name,
			RaceID: p.raceID, Tags: brandPreviewTags[(i+offset)%len(brandPreviewTags)],
			RegionName: region}
		br, ok := pickCorpBrand(seed, lore, corpFoundRerolls, func(c corpBrand) bool {
			return div.allows(c, region)
		})
		if !ok {
			continue
		}
		div.add(br, region)
		row := brandPreviewRow{corpBrand: br, Founder: p.name, Region: region,
			KindLabel: titleKindLabels[br.Kind]}
		if t := evedb.SuggestedTickers(ctx, m.db, br.Name); len(t) > 0 {
			row.Ticker = t[0]
		}
		for _, i := range []int{2, 1, 0} {
			if br.Shapes[i] == 0 {
				continue
			}
			row.Layers = append(row.Layers, brandLogoLayer{Style: template.CSS(fmt.Sprintf(
				"background:%s;-webkit-mask-image:url(/logoshape/%d.png);mask-image:url(/logoshape/%d.png)",
				logoColourHex[br.Colours[i]], br.Shapes[i], br.Shapes[i]))})
		}
		out = append(out, row)
	}
	return out
}
