package rolebots

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gurimoon/corp"
)

// The Management page through its handler, against a database that answers from a table of canned
// rows. MANAGE_DUMP=dir writes each page drawn there, to be looked at.

type fakeRow []driver.Value

type fakeAnswer struct {
	match string
	rows  func(args []driver.NamedValue) []fakeRow
}

// fakeDB answers the first entry whose text a query holds; anything else has no rows.
type fakeDB struct {
	mu      sync.Mutex
	answers []fakeAnswer
	writes  []string
	missed  map[string]int
}

func (f *fakeDB) Connect(context.Context) (driver.Conn, error) { return &fakeConn{f}, nil }
func (f *fakeDB) Driver() driver.Driver                        { return f }
func (f *fakeDB) Open(string) (driver.Conn, error)             { return &fakeConn{f}, nil }

type fakeConn struct{ f *fakeDB }

func (c *fakeConn) Prepare(q string) (driver.Stmt, error) { return nil, fmt.Errorf("prepare: %s", q) }
func (c *fakeConn) Close() error                          { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)             { return nil, fmt.Errorf("no transactions") }

var spaceRE = regexp.MustCompile(`\s+`)

func (c *fakeConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	q = strings.TrimSpace(spaceRE.ReplaceAllString(q, " "))
	for _, a := range c.f.answers {
		if strings.Contains(q, a.match) {
			return &fakeRows{rows: a.rows(args)}, nil
		}
	}
	c.f.mu.Lock()
	c.f.missed[q]++
	c.f.mu.Unlock()
	return &fakeRows{}, nil
}

func (c *fakeConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	q = strings.TrimSpace(spaceRE.ReplaceAllString(q, " "))
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	for _, a := range args {
		q += fmt.Sprintf("|%v", a.Value)
	}
	c.f.writes = append(c.f.writes, q+"|")
	return fakeResult{}, nil
}

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 77, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

type fakeRows struct {
	rows []fakeRow
	i    int
}

func (r *fakeRows) Columns() []string {
	if len(r.rows) == 0 {
		return []string{"none"}
	}
	out := make([]string, len(r.rows[0]))
	for i := range out {
		out[i] = fmt.Sprintf("c%d", i)
	}
	return out
}
func (r *fakeRows) Close() error { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

// posted is the commands filed, as their INSERT.
func (f *fakeDB) posted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, w := range f.writes {
		if strings.HasPrefix(w, "INSERT INTO rolebot_corp_commands") {
			out = append(out, w)
		}
	}
	return out
}

const (
	tCorp     = int64(98000001)
	tCEO      = int64(90000001)
	tMiner    = int64(90000002) // a person, Director, heads Mining
	tSpare    = int64(90000003) // a person, Director, no division
	tMember   = int64(90000004) // a person
	tBotDir   = int64(90000010) // a bot, Director, heads Industry
	tBotSec   = int64(90000011) // a bot, Director
	tBotPlain = int64(90000012)
	tBotHaul  = int64(90000013)
	tBotLead  = int64(90000014) // a bot of Security, out with the one fleet
	tStation  = int64(60000004)
	tStation2 = int64(60000007)
	tOffice   = int64(140000001)
	tOffice2  = int64(140000002)
	tKeyAt    = int64(1_700_000_000)
)

type tPilot struct {
	name  string
	roles uint64
	bot   bool
	title string
	arm   corpArm
}

var tPilots = map[int64]tPilot{
	tCEO:      {"Vela Korrin", 0, false, "Chief Executive", ""},
	tMiner:    {"Dane Aubreau", corp.RoleDirector, false, "Director of Mining", armMining},
	tSpare:    {"Ilse Marn", corp.RoleDirector, false, "Director", ""},
	tMember:   {"Pell Ostarik", 0, false, "", armMining},
	tBotDir:   {"Aurelie Tachikov", corp.RoleDirector, true, "Director of Industry", armIndustry},
	tBotSec:   {"Hadar Okuumi", corp.RoleDirector, true, "Director", armSecurity},
	tBotPlain: {"Renn Salvaje", corp.RoleFactoryManager, true, "[IND] Technician", armIndustry},
	tBotHaul:  {"Moira Keltan", corp.RoleTrader, true, "[SAL] Clerk", armSales},
	tBotLead:  {"Yann Dessarin", 0, true, "[SEC] Ensign", armSecurity},
}

// tPlaces is the map the location field is found in: "Amevync" is a constellation's name and a
// system's both.
var tPlaces = []findEntry{newFindEntry(10000032, "Sinq Laison", ""), newFindEntry(10000064, "Essence", ""),
	newFindEntry(20000300, "Amevync", "Sinq Laison"), newFindEntry(30002900, "Amevync", "Sinq Laison"),
	newFindEntry(30005304, "Alentene", "Sinq Laison"), newFindEntry(30005305, "Alenia", "Essence")}

// tMap is three systems in a row: two of Sinq Laison, one of them of Amevync, and one of Essence with
// no belts.
func tMap() *universe {
	u := &universe{adj: map[int64][]int64{30005304: {30002900}, 30002900: {30005304, 30005305}, 30005305: {30002900}},
		meta: map[int64]sysMeta{
			30005304: {Security: 0.9, RegionID: 10000032, ConstellationID: 20000301},
			30002900: {Security: 0.5, RegionID: 10000032, ConstellationID: 20000300},
			30005305: {Security: 0.4, RegionID: 10000064, ConstellationID: 20000400}},
		belts: map[int64]bool{30005304: true, 30002900: true}}
	u.beltOnce.Do(func() {})
	return u
}

var tOrder = []int64{tCEO, tMiner, tSpare, tMember, tBotDir, tBotSec, tBotPlain, tBotHaul, tBotLead}

var tTypes = map[int64]string{
	34: "Tritanium", 35: "Pyerite", 36: "Mexallon", 1230: "Veldspar", 1228: "Scordite", 1224: "Pyroxeres",
	587: "Rifter", 691: "Rifter Blueprint", 11186: "Malediction", 966: "Cargohold Expander I Blueprint", 2046: "Damage Control I",
	2047: "Damage Control I Blueprint",
}

func tNow() int64 { return time.Now().Unix() }

func argInt(args []driver.NamedValue, i int) int64 {
	if i < len(args) {
		if v, ok := args[i].Value.(int64); ok {
			return v
		}
	}
	return 0
}

func tBit(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func one1(v ...driver.Value) []fakeRow { return []fakeRow{v} }

// newManageDB is a small corporation: a person as CEO, a person heading Mining, a bot heading
// Industry, a factory, a laboratory, a few prints and orders.
func newManageDB() *fakeDB {
	f := &fakeDB{missed: map[string]int{}}
	add := func(match string, rows func(args []driver.NamedValue) []fakeRow) {
		f.answers = append(f.answers, fakeAnswer{match, rows})
	}
	none := func([]driver.NamedValue) []fakeRow { return nil }

	add("FROM rolebot_ceos", none)
	add("SELECT ch.corporationID, COALESCE(c.ceoID, 0), cr.corprole", func(a []driver.NamedValue) []fakeRow {
		p, ok := tPilots[argInt(a, 0)]
		if !ok {
			return nil
		}
		return one1(tCorp, tCEO, int64(p.roles))
	})
	add("SELECT corporationID FROM character_ WHERE characterID", func(a []driver.NamedValue) []fakeRow {
		if _, ok := tPilots[argInt(a, 0)]; ok {
			return one1(tCorp)
		}
		return nil
	})
	add("SELECT COALESCE(ceoID, 0) FROM corporation", func([]driver.NamedValue) []fakeRow { return one1(tCEO) })
	add("SELECT issuedAt FROM rolebot_portal_keys", func([]driver.NamedValue) []fakeRow { return one1(tKeyAt) })
	add("SELECT keyHash, corporationID", func([]driver.NamedValue) []fakeRow {
		return one1(hashPortalKey("CORP-ABCD-EFGH-JKLM"), tCorp, int64(0), int64(0))
	})
	add("FROM rolebot_corp_heads", func([]driver.NamedValue) []fakeRow {
		return []fakeRow{{tCorp, "mining", tMiner, int64(0), tCEO, tNow() - 86400, int64(0)},
			{tCorp, "industry", tBotDir, tBotPlain, tCEO, tNow() - 5*86400, int64(0)}}
	})
	add("SELECT ch.characterID, ch.characterName, COALESCE(r.corprole, 0), COALESCE(r.titleMask, 0)", func(a []driver.NamedValue) []fakeRow {
		var out []fakeRow
		for _, id := range tOrder {
			if id == argInt(a, 1) {
				continue
			}
			p := tPilots[id]
			out = append(out, fakeRow{id, p.name, int64(p.roles), int64(0), p.title, int64(0), 1e6, p.bot, tBit(p.bot)})
		}
		return out
	})
	add("SELECT ch.characterID, ch.characterName FROM character_ ch JOIN rolebot_characters", func([]driver.NamedValue) []fakeRow {
		var out []fakeRow
		for _, id := range tOrder {
			if tPilots[id].bot {
				out = append(out, fakeRow{id, tPilots[id].name})
			}
		}
		return out
	})
	add("SELECT ch.characterID, COALESCE(r.corprole, 0) FROM character_", func([]driver.NamedValue) []fakeRow {
		var out []fakeRow
		for _, id := range tOrder {
			out = append(out, fakeRow{id, int64(tPilots[id].roles)})
		}
		return out
	})
	add("FROM rolebot_corp_arm_members WHERE corporationID", func([]driver.NamedValue) []fakeRow {
		var out []fakeRow
		for _, id := range tOrder {
			if p := tPilots[id]; p.arm != "" {
				by := tCEO
				if id == tMember {
					by = tMiner
				}
				out = append(out, fakeRow{id, tCorp, string(p.arm), int64(0), int64(0), by, int64(0), "human"})
			}
		}
		return out
	})
	add("SELECT r.characterID, COALESCE(r.baseID, 0)", func([]driver.NamedValue) []fakeRow {
		return []fakeRow{{tBotDir, tStation}, {tBotPlain, tStation}, {tMember, tStation2}}
	})
	add("characterName FROM character_ WHERE characterID", func(a []driver.NamedValue) []fakeRow {
		if p, ok := tPilots[argInt(a, 0)]; ok {
			return one1(p.name)
		}
		return nil
	})
	add("SELECT typeID, typeName FROM invTypes WHERE typeName = ?", func(a []driver.NamedValue) []fakeRow {
		for id, n := range tTypes {
			if strings.EqualFold(n, fmt.Sprint(a[0].Value)) {
				return one1(id, n)
			}
		}
		return nil
	})
	add("SELECT typeID, typeName FROM invTypes WHERE typeName LIKE", func(a []driver.NamedValue) []fakeRow {
		want := strings.ToLower(strings.Trim(fmt.Sprint(a[0].Value), "%"))
		var out []fakeRow
		for id, n := range tTypes {
			if strings.Contains(strings.ToLower(n), want) {
				out = append(out, fakeRow{id, n})
			}
		}
		return out
	})
	add("SELECT t.typeID, t.typeName, COALESCE(g.groupName", func([]driver.NamedValue) []fakeRow {
		var out []fakeRow
		for id, n := range tTypes {
			out = append(out, fakeRow{id, n, "Mineral"})
		}
		return out
	})
	add("SELECT solarSystemID FROM mapSolarSystems WHERE LOWER(solarSystemName)", func([]driver.NamedValue) []fakeRow { return one1(int64(30002809)) })
	for match, kind := range map[string]string{"SELECT regionID, regionName, ''": placeRegion,
		"SELECT c.constellationID, c.constellationName": placeConstellation, "SELECT s.solarSystemID, s.solarSystemName": placeSystem} {
		add(match, func([]driver.NamedValue) []fakeRow {
			var out []fakeRow
			for _, e := range tPlaces {
				if placeKindOf(e.ID) == kind {
					out = append(out, fakeRow{e.ID, e.Name, e.Group})
				}
			}
			return out
		})
	}
	add("SELECT regionName FROM mapRegions", func([]driver.NamedValue) []fakeRow { return one1("Sinq Laison") })
	add("SELECT constellationName FROM mapConstellations", func([]driver.NamedValue) []fakeRow { return one1("Amevync") })
	add("SELECT o.stationID, o.price FROM market_orders o", func([]driver.NamedValue) []fakeRow { return one1(tStation, 5.0) })
	add("SELECT COALESCE(typeName, ''), COALESCE(groupID, 0)", func(a []driver.NamedValue) []fakeRow {
		if n, ok := tTypes[argInt(a, 0)]; ok {
			return one1(n, int64(18), 1.0, 0.01, 2.0)
		}
		return nil
	})
	add("FROM staStations WHERE stationID", func(a []driver.NamedValue) []fakeRow {
		n := "Alentene VI - Moon 16 - Duvolle Laboratories Factory"
		if argInt(a, 0) == tStation2 {
			n = "Cistuvaert V - Moon 12 - Center for Advanced Studies"
		}
		return one1(n)
	})
	add("SELECT stationName", func(a []driver.NamedValue) []fakeRow {
		n := "Alentene VI - Moon 16 - Duvolle Laboratories Factory"
		if argInt(a, 0) == tStation2 {
			n = "Cistuvaert V - Moon 12 - Center for Advanced Studies"
		}
		return one1(n)
	})
	add("SELECT solarSystemID FROM mapSolarSystems WHERE LOWER(solarSystemName)", func(a []driver.NamedValue) []fakeRow {
		if strings.EqualFold(fmt.Sprint(a[0].Value), "Alentene") {
			return one1(int64(30005304))
		}
		return nil
	})
	add("SELECT solarSystemName FROM mapSolarSystems WHERE solarSystemName LIKE", func([]driver.NamedValue) []fakeRow {
		return []fakeRow{{"Alentene"}, {"Alenia"}}
	})
	add("SELECT r.stationID, s.solarSystemID, r.state = 'dropping'", func([]driver.NamedValue) []fakeRow {
		return []fakeRow{{tStation, int64(30005304), false}, {tStation2, int64(30005305), false}}
	})
	add("SELECT o.stationID FROM crpoffices o JOIN corporation c", func([]driver.NamedValue) []fakeRow { return one1(tStation) })
	add("SELECT itemID FROM crpoffices", func(a []driver.NamedValue) []fakeRow { return one1(tOffice) })
	add("FROM crpoffices WHERE", func(a []driver.NamedValue) []fakeRow { return one1(tOffice, tCorp) })
	add("SELECT stationID, purposes FROM rolebot_corp_offices", func([]driver.NamedValue) []fakeRow {
		return []fakeRow{{tStation, int64(officeHQ | officeFactory | officeLab)}, {tStation2, int64(officeLab)}}
	})
	add("FROM ramassemblylines", func([]driver.NamedValue) []fakeRow { return one1(int64(1)) })
	add("AND COALESCE(b.copy, 0) = 0 ORDER BY e.typeID", func([]driver.NamedValue) []fakeRow {
		return []fakeRow{{int64(1001), int64(691), tStation, int64(116), int64(12), int64(4)},
			{int64(1002), int64(966), tStation, int64(116), int64(0), int64(0)},
			{int64(1003), int64(2047), tStation2, int64(116), int64(20), int64(10)}}
	})
	add("AND b.licensedProductionRunsRemaining > 0", func([]driver.NamedValue) []fakeRow {
		return []fakeRow{{int64(2001), int64(691), tStation, int64(116), int64(12), int64(4), int64(40)}}
	})
	add("FROM rolebot_corp_work WHERE corporationID = ? AND status = 'open' AND bpItemID", none)
	add("FROM rolebot_corp_work WHERE corporationID = ? AND status = 'open'", func([]driver.NamedValue) []fakeRow {
		return []fakeRow{{int64(501), tCorp, "build", int64(1), int64(691), int64(587), tStation, int64(40), int64(16), 25000.0, "human",
			int64(100), "open", tNow() - 7200, "ordered by Vela Korrin", tCEO, "human", int64(0), "industry", int64(0), int64(0), int64(0)},
			{int64(502), tCorp, "me", int64(4), int64(2047), int64(2047), tStation2, int64(5), int64(0), 80000.0, "human",
				int64(60), "open", tNow() - 7200, "ordered by Vela Korrin", tCEO, "human", int64(1003), "industry", int64(0), int64(0), int64(0)}}
	})
	add("SELECT runsDone, runsWanted FROM rolebot_corp_work", func([]driver.NamedValue) []fakeRow { return one1(int64(16), int64(40)) })
	add("SELECT qtyDone, qty FROM rolebot_corp_requisition", func([]driver.NamedValue) []fakeRow { return one1(int64(300000), int64(1200000)) })
	cmdRow := func(id, poster int64, hat, kind string, pay any, status string, refID int64, result, arm string, ceo bool) fakeRow {
		return fakeRow{id, tCorp, poster, hat, kind, encodeCommandPayload(pay), "", tNow() - 3600, tNow() + 86400, status, int64(0), tNow() - 3000,
			int64(0), "", refID, result, int64(0), arm, ceo}
	}
	cmds := []fakeRow{
		cmdRow(1, tCEO, "CEO", cmdKindWork, cmdWorkPayload{Kind: workKindBuild, BPTypeID: 691, ProductTypeID: 587, Runs: 40, WagePerRun: 25000}, cmdLive, 501, "16 of 40 delivered", "industry", true),
		cmdRow(2, tMiner, "Director", cmdKindStanding, cmdStandingPayload{Dept: portOperations, Ores: "Veldspar,Scordite", Hours: 24}, cmdLive, tCorp, "the fleets have it", "mining", false),
		cmdRow(3, tCEO, "CEO", cmdKindReq, cmdBoardPayload{TypeID: 34, Qty: 1200000, StationID: tStation, Days: 4}, cmdClaimed, 601, "a bid is on the market", "mining", true),
		cmdRow(4, tCEO, "CEO", cmdKindHead, cmdHeadPayload{Arm: "mining", CharID: tMiner}, cmdPending, 0, "", "", true),
	}
	add("FROM rolebot_corp_commands WHERE commandID = ?", func(a []driver.NamedValue) []fakeRow {
		for _, c := range cmds {
			if c[0].(int64) == argInt(a, 0) {
				return []fakeRow{c}
			}
		}
		return nil
	})
	add("FROM rolebot_corp_commands WHERE corporationID = ? AND kind = ?", none)
	add("FROM rolebot_corp_commands WHERE corporationID = ?", func([]driver.NamedValue) []fakeRow { return cmds })
	// One fleet of Security's is out, raised by a desk, and led by the spare bot.
	add("SELECT gangID FROM rolebot_gangs WHERE corpID", func([]driver.NamedValue) []fakeRow { return one1(int64(9001)) })
	add("SELECT gangID, name, enabled, activity", func([]driver.NamedValue) []fakeRow {
		return one1(int64(9001), "Test Corp Patrol", int64(1), ActSecurity, "",
			`{"opPhase":"live","orderAreaID":10000032,"huntSystemID":30005304,"huntSystemName":"Alentene"}`,
			tBotLead, tCorp, int64(1), tNow()+3600, tNow()-600, "")
	})
	add("SELECT gm.gangID, gm.characterID, gm.role", func([]driver.NamedValue) []fakeRow {
		return []fakeRow{{int64(9001), tBotLead, RoleCombat, int64(0), int64(1), int64(0), int64(0), "", int64(0)},
			{int64(9001), tBotHaul, RoleCombat, int64(0), int64(1), int64(0), int64(0), "", int64(0)}}
	})
	add("SELECT e.typeID, MAX(COALESCE(ea.valueInt", func(a []driver.NamedValue) []fakeRow {
		if argInt(a, 0) == tBotSec {
			return one1(int64(skillLeadership), int64(4))
		}
		return nil
	})
	add("SELECT balance FROM corporation", func([]driver.NamedValue) []fakeRow { return one1(412_000_000.0) })
	add("FROM rolebot_corp_commit", func([]driver.NamedValue) []fakeRow { return one1(12_000_000.0) })
	add("SELECT productTypeID FROM invBlueprintTypes", func([]driver.NamedValue) []fakeRow { return one1(int64(587)) })
	add("SELECT systemID FROM rolebot_belt_distress", func([]driver.NamedValue) []fakeRow { return one1(int64(30005304)) })
	add("FROM mapSolarSystems WHERE solarSystemID", func([]driver.NamedValue) []fakeRow { return one1("Alentene") })
	add("FROM rolebot_corp_policy", func([]driver.NamedValue) []fakeRow {
		return one1(0.5, 0.5, int64(0), "", tBotDir, int64(0), tBotPlain, int64(0), "", int64(0))
	})
	return f
}

// managePage draws one request as a pilot.
func managePage(t *testing.T, f *fakeDB, who int64, signedIn bool, q url.Values) string {
	t.Helper()
	db := sql.OpenDB(f)
	defer db.Close()
	m := &Mod{db: db}
	savedIdx, savedSecret := humanChairIndex.Load(), deskSecret
	savedSet, savedAt, savedOK := botMailboxSet, botMailboxAt, botMailboxEverOK
	defer func() {
		humanChairIndex.Store(savedIdx)
		deskSecret, botMailboxSet, botMailboxAt, botMailboxEverOK = savedSecret, savedSet, savedAt, savedOK
	}()
	humanChairIndex.Store(&map[int64]int64{tCorp: tCEO})
	deskSecret = []byte("0123456789abcdef0123456789abcdef")
	// Nobody's hours are up, whatever the clock on the wall says.
	armsMu.Lock()
	heldSaved := armsHolds
	armsHolds = map[int64]int64{}
	for id := range tPilots {
		armsHolds[id] = clock.Unix() + 3600
	}
	armsMu.Unlock()
	defer func() {
		armsMu.Lock()
		armsHolds = heldSaved
		armsMu.Unlock()
	}()
	uniSaved := uniOverride
	uniOverride = tMap()
	defer func() { uniOverride = uniSaved }()
	deskModSaved := deskMod
	deskMod = m
	defer func() { deskMod = deskModSaved }()
	botMailboxSet = map[int64]struct{}{}
	for id, p := range tPilots {
		if p.bot {
			botMailboxSet[id] = struct{}{}
		}
	}
	botMailboxAt, botMailboxEverOK = clock.Now(), true
	corpOrdersOn.Store(true)
	humanFleetMaxOpen.Store(2)
	directiveHoldMax.Store(30)
	orderTTLHours.Store(24)
	portalKeyLockSec.Store(900)
	assignmentsV.Store(true)
	printMovesV.Store(true)
	if signedIn {
		q.Set("s", manageToken(deskKeyAny(), who, tCorp, tKeyAt, clock.Unix()+manageTokenSec))
	}
	r := httptest.NewRequest("GET", managePath+"?"+q.Encode(), nil)
	r.Header.Set("eve.charID", fmt.Sprint(who))
	w := httptest.NewRecorder()
	m.handleIGBCorpManagement(w, r)
	body := w.Body.String()
	if dir := os.Getenv("MANAGE_DUMP"); dir != "" {
		name := regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(t.Name(), "_")
		_ = os.WriteFile(filepath.Join(dir, name+".html"), []byte(body), 0o644)
		var b strings.Builder
		for q, n := range f.missed {
			fmt.Fprintf(&b, "%d\t%s\n", n, q)
		}
		_ = os.WriteFile(filepath.Join(dir, name+".missed.txt"), []byte(b.String()), 0o644)
	}
	if n := strings.Count(body, "<form"); n != strings.Count(body, "</form>") {
		t.Errorf("%d forms opened, %d closed", n, strings.Count(body, "</form>"))
	}
	checkNoFormInTable(t, body)
	return body
}

// checkNoFormInTable holds a page to what the in-game browser draws: no form inside a table and no
// table inside another.
func checkNoFormInTable(t *testing.T, page string) {
	t.Helper()
	depth := 0
	for _, tag := range regexp.MustCompile(`(?i)</?(table|form)\b`).FindAllString(page, -1) {
		switch strings.ToLower(tag) {
		case "<table":
			if depth++; depth > 1 {
				t.Error("a table inside a table")
				return
			}
		case "</table":
			depth--
		case "<form":
			if depth > 0 {
				t.Error("a form inside a table")
				return
			}
		}
	}
}

func TestManagementPageIsForTheChair(t *testing.T) {
	for _, c := range []struct {
		name     string
		who      int64
		signedIn bool
		has, not []string
	}{
		{"a member", tMember, true, []string{"for the CEO and the Directors"}, []string{"<form", "Orders in hand"}},
		{"a bot Director", tBotSec, true, []string{"from your own terminal"}, []string{"<form"}},
		{"a Director with no division", tSpare, true, []string{"no division"}, []string{"<form", "Orders in hand"}},
		{"the CEO before the key", tCEO, false, []string{"Portal key", "Chief Executive Officer"}, []string{"Orders in hand", "Characters"}},
		{"the CEO", tCEO, true, []string{"Your desk", "Divisions", "Orders in hand", "Aurelie Tachikov", "Heads", "Characters"}, nil},
		{"a head", tMiner, true, []string{"Mining", "You head it", "Give an order", "Pell Ostarik"},
			[]string{"Give orders &raquo;", "Who heads", "Budget", "Renn Salvaje"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := managePage(t, newManageDB(), c.who, c.signedIn, url.Values{})
			for _, s := range c.has {
				if !strings.Contains(got, s) {
					t.Errorf("the page lacks %q", s)
				}
			}
			for _, s := range c.not {
				if strings.Contains(got, s) {
					t.Errorf("the page shows %q", s)
				}
			}
		})
	}
}

func TestManagementPages(t *testing.T) {
	for _, div := range []string{"", "industry", "research", "mining", "security", "sales", "infrastructure", "hr", "finance", mgCorp, mgHeads, mgPeople} {
		t.Run("page "+div, func(t *testing.T) {
			managePage(t, newManageDB(), tCEO, true, url.Values{"div": {div}})
		})
	}
	// Every form opens on every page it belongs to.
	for _, f := range manageForms {
		arms := f.Arms
		if len(arms) == 0 {
			arms = []corpArm{""}
		}
		for _, a := range arms {
			t.Run("form "+f.Form+" on "+string(a), func(t *testing.T) {
				div := string(a)
				if a == "" {
					div = mgCorp
				}
				if got := managePage(t, newManageDB(), tCEO, true, url.Values{"div": {div}, "o": {f.Form}}); !strings.Contains(got, ">close</a>") {
					t.Error("the form did not open")
				}
			})
		}
	}
	t.Run("appoint", func(t *testing.T) {
		got := managePage(t, newManageDB(), tCEO, true, url.Values{"div": {mgHeads}, "o": {"head"}, "pre": {"head"}, "pre_arm": {"security"}})
		if !strings.Contains(got, "Who heads Security?") || !strings.Contains(got, "Hadar Okuumi") {
			t.Error("the candidates are not shown")
		}
	})
	t.Run("appoint says where each Director stands", func(t *testing.T) {
		got := managePage(t, newManageDB(), tCEO, true, url.Values{"div": {mgHeads}, "o": {"head"}, "pre": {"head"}, "pre_arm": {"security"}})
		// A Director placed in a division by the Characters page or the deal is not "no division".
		for _, want := range []string{"works in", "heads ", "no division"} {
			if !strings.Contains(got, want) {
				t.Errorf("the list does not say %q", want)
			}
		}
	})
	t.Run("assign", func(t *testing.T) {
		got := managePage(t, newManageDB(), tCEO, true, url.Values{"div": {mgPeople}, "o": {"people"}, "pre": {"people"}, "pre_member": {fmt.Sprint(tBotPlain)}})
		if !strings.Contains(got, `name="seat"`) || !strings.Contains(got, `name="station"`) {
			t.Error("the assignment is not shown")
		}
	})
}

// Security's page lists the fleets out and offers their leaders by Leadership.
func TestManagementSecurity(t *testing.T) {
	f := newManageDB()
	page := managePage(t, f, tCEO, true, url.Values{"div": {"security"}, "o": {"fleet"}})
	for _, want := range []string{"Fleets out", "Test Corp Patrol", "working ", "sent to the Sinq Laison region", "Yann Dessarin",
		"Hadar Okuumi (Leadership 4)</option>", "Yann Dessarin (Leadership 0) - out with Test Corp Patrol", `name="find_place"`, "whoever is best placed"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(page, `name="system"`) {
		t.Error("Security is still asked for a system")
	}
	// The other divisions' pages have no such list.
	if page := managePage(t, newManageDB(), tCEO, true, url.Values{"div": {"mining"}, "o": {"fleet"}}); strings.Contains(page, "Fleets out") ||
		strings.Contains(page, `name="leader"`) || !strings.Contains(page, `name="find_system"`) {
		t.Error("the mining page is not as it was")
	}
}

func TestManagementOrders(t *testing.T) {
	build := url.Values{"div": {"industry"}, "form": {"build"}, "print": {fmt.Sprintf("691.%d", tStation)}, "runs": {"10"}, "wage": {"25,000"}, "prio": {"100"}}
	with := func(q url.Values, kv ...string) url.Values {
		out := url.Values{}
		for k, v := range q {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	secFleet := func(place, leader string) url.Values {
		return url.Values{"div": {"security"}, "form": {"fleet"}, "activity": {ActSecurity}, "place": {place}, "leader": {leader}, "size": {"5"},
			"duration": {"120"}, "confirm": {"1"}, "n": {"0123456789abcd02"}}
	}
	research := url.Values{"div": {"research"}, "form": {"research"}, "what": {"me"}, "print": {"1001"}, "runs": {"5"}}
	for _, c := range []struct {
		name     string
		who      int64
		signedIn bool
		q        url.Values
		posts    string // a piece of the command filed, "" for none
		has      string
	}{
		{"a preview files nothing", tCEO, true, build, "", "Is this right?"},
		{"a confirmation files it", tCEO, true, with(build, "confirm", "1", "n", "0123456789abcdef"), "|work|", "Order given"},
		{"not without a nonce", tCEO, true, with(build, "confirm", "1"), "", "already been given"},
		{"not signed out", tCEO, false, with(build, "confirm", "1", "n", "0123456789abcde0"), "", "Portal key"},
		{"not another division's head", tMiner, true, with(build, "confirm", "1", "n", "0123456789abcde1"), "", "not an order you can give"},
		{"not a head on a forged page", tMiner, true, with(build, "div", "industry", "confirm", "1", "n", "0123456789abcde2"), "", "not an order you can give"},
		{"not a member", tMember, true, with(build, "confirm", "1", "n", "0123456789abcde3"), "", "for the CEO and the Directors"},
		{"not a Director with no division", tSpare, true, with(build, "confirm", "1", "n", "0123456789abcde4"), "", "no division"},
		{"not past the reserve", tCEO, true, with(build, "wage", "999b", "confirm", "1", "n", "0123456789abcde5"), "", "free above its reserve"},
		{"not a print that is not there", tCEO, true, with(build, "print", "1.2", "confirm", "1", "n", "0123456789abcde6"), "", "Pick a print"},
		{"research", tCEO, true, with(research, "confirm", "1", "n", "0123456789abcde7"), `"bpItemID":1001`, "Order given"},
		{"copies", tCEO, true, with(research, "what", "copy", "confirm", "1", "n", "0123456789abcde8"), `"kind":"copy"`, "Order given"},
		{"a requisition by a head", tMiner, true, url.Values{"form": {"req"}, "item": {"tritanium"}, "qty": {"1000"}, "station": {fmt.Sprint(tStation)},
			"flag": {"1"}, "days": {"4"}, "confirm": {"1"}, "n": {"0123456789abcde9"}}, "|mining|false|", "Order given"},
		{"an item half named", tCEO, true, url.Values{"div": {"mining"}, "form": {"req"}, "item": {"rifter"}, "qty": {"1"}, "station": {fmt.Sprint(tStation)},
			"flag": {"1"}, "days": {"4"}}, "", "Is this right?"},
		{"an item misnamed", tCEO, true, url.Values{"div": {"mining"}, "form": {"req"}, "item": {"control"}, "qty": {"1"}, "station": {fmt.Sprint(tStation)},
			"flag": {"1"}, "days": {"4"}}, "", "Did you mean"},
		{"a fleet", tMiner, true, url.Values{"form": {"fleet"}, "activity": {ActMining}, "system": {"alentene"}, "size": {"5"}, "duration": {"120"},
			"confirm": {"1"}, "n": {"0123456789abcdea"}}, "|fleet|", "Order given"},
		{"a head raising another division's fleet", tMiner, true, url.Values{"form": {"fleet"}, "activity": {ActSecurity}, "system": {"alentene"}, "size": {"5"},
			"duration": {"120"}, "confirm": {"1"}, "n": {"0123456789abcdeb"}}, "", "Pick the kind of fleet"},
		{"a fleet to a system", tCEO, true, secFleet("alentene", ""), `"systemID":30005304,`, "Order given"},
		{"a fleet to a region", tCEO, true, secFleet("sinq laison", ""), `"areaID":10000032,"systemName":"Sinq Laison region"`, "Order given"},
		{"a fleet to a constellation, its kind said", tCEO, true, secFleet("Amevync (constellation)", ""), `"areaID":20000300`, "Order given"},
		{"a name two places share", tCEO, true, secFleet("Amevync", ""), "", "could be more than one place"},
		{"a region with nothing to hunt", tCEO, true, secFleet("essence region", ""), "", "no ground for a fleet"},
		{"nowhere", tCEO, true, secFleet("zzzzqq", ""), "", "No system, constellation or region"},
		{"a Find button lists places", tCEO, true, with(secFleet("ale", ""), "find_place", "Find location"), "", "Click the one you mean"},
		{"a fleet's leader", tCEO, true, secFleet("alentene", fmt.Sprint(tBotSec)), fmt.Sprintf(`"leaderID":%d`, tBotSec), "Order given"},
		{"a leader's Leadership in the preview", tCEO, true, with(secFleet("alentene", fmt.Sprint(tBotSec)), "confirm", ""), "", "</b> (Leadership 4)"},
		{"a leader out with a fleet", tCEO, true, secFleet("alentene", fmt.Sprint(tBotLead)), "", "is out with Test Corp Patrol"},
		{"a leader of another division", tCEO, true, secFleet("alentene", fmt.Sprint(tBotPlain)), "", "Pick a member of Security"},
		{"a standing patrol of a region", tCEO, true, url.Values{"div": {"security"}, "form": {"standing"}, "place": {"Sinq Laison"}, "hours": {"24"},
			"confirm": {"1"}, "n": {"0123456789abcd03"}}, `"areaID":10000032`, "Order given"},
		{"a patrol job in a constellation", tCEO, true, url.Values{"div": {"security"}, "form": {"assign"}, "what": {assignPatrol}, "qty": {"5"}, "reward": {"1000"},
			"place": {"Amevync constellation"}, "confirm": {"1"}, "n": {"0123456789abcd04"}}, `"systemID":20000300`, "Order given"},
		{"an appointment", tCEO, true, url.Values{"div": {mgHeads}, "form": {"head"}, "arm": {"security"}, "member": {fmt.Sprint(tBotSec)}, "confirm": {"1"},
			"n": {"0123456789abcdec"}}, "|head|", "Order given"},
		{"an appointment by a head", tMiner, true, url.Values{"div": {mgHeads}, "form": {"head"}, "arm": {"security"}, "member": {fmt.Sprint(tBotSec)},
			"confirm": {"1"}, "n": {"0123456789abcded"}}, "", "not an order you can give"},
		{"an appointment without the role", tCEO, true, url.Values{"div": {mgHeads}, "form": {"head"}, "arm": {"security"}, "member": {fmt.Sprint(tBotPlain)},
			"confirm": {"1"}, "n": {"0123456789abcdee"}}, "", "does not hold the Director role"},
		{"assigning a person", tCEO, true, url.Values{"div": {mgPeople}, "form": {"people"}, "member": {fmt.Sprint(tMember)}, "arm": {"research"},
			"station": {fmt.Sprint(tStation)}, "seat": {"keep"}, "confirm": {"1"}, "n": {"0123456789abcdef"}}, "|arm|", "Order given"},
		{"a head assigning another division's bot", tMiner, true, url.Values{"form": {"people"}, "member": {fmt.Sprint(tBotPlain)}, "arm": {"mining"},
			"confirm": {"1"}, "n": {"0123456789abcdf0"}}, "", "Pick a character"},
		{"a head's setting", tMiner, true, url.Values{"div": {mgCorp}, "form": {"budget"}, "reserve": {"0"}, "confirm": {"1"}, "n": {"0123456789abcdf1"}}, "",
			"not an order you can give"},
		{"the CEO's hangars", tCEO, true, url.Values{"div": {mgCorp}, "form": {"hangars"}, "armory": {"4"}, "yard": {"none"}, "spoils": {""},
			"confirm": {"1"}, "n": {"0123456789abcdf4"}}, "|hangars|", "Order given"},
		{"a head's hangars", tMiner, true, url.Values{"div": {mgCorp}, "form": {"hangars"}, "armory": {"4"}, "confirm": {"1"},
			"n": {"0123456789abcdf5"}}, "", "not an order you can give"},
		{"a hangar no war chest can stand in", tCEO, true, url.Values{"div": {mgCorp}, "form": {"hangars"}, "armory": {"1"}, "confirm": {"1"},
			"n": {"0123456789abcdf6"}}, "", "hangar 2, 3, 4 or 5 only"},
		{"hangars as they stand", tCEO, true, url.Values{"div": {mgCorp}, "form": {"hangars"}, "confirm": {"1"},
			"n": {"0123456789abcdf7"}}, "", "already stand"},
		{"a trade run at the going rate", tCEO, true, url.Values{"div": {"sales"}, "form": {"trade"}, "item": {"Tritanium"}, "qty": {"100"},
			"system": {"Alentene"}, "price": {"6"}, "station": {fmt.Sprint(tStation2)}, "wage": {""}, "confirm": {"1"}, "n": {"0123456789abcdf8"}},
			"|trade|", "Order given"},
		{"a trade run with a wage of nonsense", tCEO, true, url.Values{"div": {"sales"}, "form": {"trade"}, "item": {"Tritanium"}, "qty": {"100"},
			"system": {"Alentene"}, "price": {"6"}, "station": {fmt.Sprint(tStation2)}, "wage": {"lots"}, "confirm": {"1"}, "n": {"0123456789abcdf9"}},
			"", "going rate"},
		{"a Find button lists what the letters could be", tCEO, true, url.Values{"div": {"sales"}, "form": {"trade"}, "item": {"damge contrl"},
			"find_item": {"Find item"}}, "", "Click the one you mean"},
		{"a Find button on a plain name previews instead", tCEO, true, url.Values{"div": {"sales"}, "form": {"trade"}, "item": {"Tritanium"},
			"qty": {"100"}, "system": {"Alentene"}, "price": {"6"}, "station": {fmt.Sprint(tStation2)}, "find_item": {"Find item"}}, "", "Is this right?"},
		{"a misspelt item is found", tCEO, true, url.Values{"div": {"sales"}, "form": {"trade"}, "item": {"tritanum"}, "qty": {"100"},
			"system": {"Alentene"}, "price": {"6"}, "station": {fmt.Sprint(tStation2)}}, "", "Is this right?"},
		{"the CEO's budget", tCEO, true, url.Values{"div": {mgCorp}, "form": {"budget"}, "reserve": {"2"}, "confirm": {"1"}, "n": {"0123456789abcdf2"}}, "|budget|",
			"Order given"},
		{"a budget of nonsense", tCEO, true, url.Values{"div": {mgCorp}, "form": {"budget"}, "reserve": {"NaN"}, "confirm": {"1"}, "n": {"0123456789abcdf3"}}, "",
			"percentages"},
	} {
		t.Run(c.name, func(t *testing.T) {
			manageNonces.Range(func(k, _ any) bool { manageNonces.Delete(k); return true })
			f := newManageDB()
			got := managePage(t, f, c.who, c.signedIn, c.q)
			posted := strings.Join(f.posted(), "\n")
			if c.posts == "" && posted != "" {
				t.Errorf("it filed %s", posted)
			}
			if c.posts != "" && !strings.Contains(posted, c.posts) {
				t.Errorf("it filed %q, want %q", posted, c.posts)
			}
			if !strings.Contains(got, c.has) {
				t.Errorf("the page does not say %q", c.has)
			}
		})
	}
}

func TestManagementWithdraw(t *testing.T) {
	for _, c := range []struct {
		name string
		who  int64
		id   string
		has  string
		done bool
	}{
		{"the CEO withdraws a head's", tCEO, "2", "Order withdrawn", true},
		{"a head withdraws its own", tMiner, "2", "Order withdrawn", true},
		{"a head withdraws the CEO's", tMiner, "3", "No such order of yours", false},
		{"a head withdraws another division's", tMiner, "1", "No such order of yours", false},
		{"nobody withdraws what is not there", tCEO, "99", "No such order of yours", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			manageNonces.Range(func(k, _ any) bool { manageNonces.Delete(k); return true })
			f := newManageDB()
			got := managePage(t, f, c.who, true, url.Values{"form": {"cancel"}, "id": {c.id}, "confirm": {"1"}, "n": {"fedcba9876543210"}})
			if !strings.Contains(got, c.has) {
				t.Errorf("the page does not say %q", c.has)
			}
			done := false
			for _, w := range f.writes {
				done = done || (strings.HasPrefix(w, "UPDATE rolebot_corp_commands SET status") && strings.Contains(w, "|cancelled|"))
			}
			if done != c.done {
				t.Errorf("withdrawn = %v, want %v", done, c.done)
			}
		})
	}
}

// A human chair's members are placed by hand, so their roles are brought in line with their
// division's bundle by refreshArmRoles, not by a CEO's turn.
func TestRefreshArmRolesWrites(t *testing.T) {
	f := newManageDB()
	db := sql.OpenDB(f)
	defer db.Close()
	m := &Mod{db: db}
	saved := humanChairIndex.Load()
	humanChairIndex.Store(&map[int64]int64{tCorp: tCEO})
	defer humanChairIndex.Store(saved)
	n, err := m.refreshArmRoles(context.Background(), tCorp)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatalf("no member was brought in line; writes: %v", f.writes)
	}
	var roleWrite bool
	for _, w := range f.writes {
		roleWrite = roleWrite || strings.Contains(w, "chrCorporationRoles")
	}
	if !roleWrite {
		t.Errorf("no role was written: %v", f.writes)
	}
}

// A human chair's wages are owed, and the Treasury's payroll pays them.
func TestHumanChairWageIsOwed(t *testing.T) {
	f := newManageDB()
	db := sql.OpenDB(f)
	defer db.Close()
	m := &Mod{db: db}
	saved := humanChairIndex.Load()
	humanChairIndex.Store(&map[int64]int64{tCorp: tCEO})
	defer humanChairIndex.Store(saved)
	if !m.payCorpWage(context.Background(), tCorp, tBotDir, 5000, "trade run") {
		t.Fatal("the wage was not accounted for")
	}
	var owed, paid bool
	for _, w := range f.writes {
		owed = owed || strings.Contains(w, "INSERT INTO rolebot_corp_owed")
		paid = paid || strings.Contains(w, "Corporation pay:")
	}
	if !owed || paid {
		t.Errorf("owed=%v paid=%v, writes %v", owed, paid, f.writes)
	}
}

// A CEO whose roster row is switched off is played by its person: the corporation is human-chaired. A
// generated pilot switched off, and a bot CEO's corporation, are not.
func TestHumanChairIndexTakesAPlayedRosterPilot(t *testing.T) {
	const played, person, generated = int64(90000021), int64(90000022), int64(90000023)
	f := &fakeDB{missed: map[string]int{}, answers: []fakeAnswer{
		{"COALESCE(rb.enabled, 0) = 0", func([]driver.NamedValue) []fakeRow {
			return []fakeRow{{tCorp, played}, {tCorp + 1, person}, {tCorp + 2, generated}, {tCorp + 3, played}}
		}},
		{"WHERE r.enabled = 0", func([]driver.NamedValue) []fakeRow { return one1(played) }},
	}}
	db := sql.OpenDB(f)
	defer db.Close()
	m := &Mod{db: db}
	savedChairs, savedCEOs := humanChairIndex.Load(), ceoIndex.Load()
	savedSet, savedAt, savedOK := botMailboxSet, botMailboxAt, botMailboxEverOK
	savedSeats, savedSeatsAt := personSeatSet, personSeatAt
	defer func() {
		humanChairIndex.Store(savedChairs)
		ceoIndex.Store(savedCEOs)
		botMailboxSet, botMailboxAt, botMailboxEverOK = savedSet, savedAt, savedOK
		personSeatSet, personSeatAt = savedSeats, savedSeatsAt
	}()
	botMailboxSet = map[int64]struct{}{played: {}, generated: {}}
	botMailboxAt, botMailboxEverOK = clock.Now(), true
	personSeatAt = time.Time{}
	ceoIndex.Store(&map[int64]int64{tCorp + 3: tBotDir})
	m.refreshHumanChairIndex(context.Background())
	for corpID, want := range map[int64]int64{tCorp: played, tCorp + 1: person, tCorp + 2: 0, tCorp + 3: 0} {
		if id, _ := humanChaired(corpID); id != want {
			t.Errorf("corp %d is chaired by %d, want %d", corpID, id, want)
		}
	}
}

// A switched-off roster pilot its person plays heads a division as a capsuleer: it can be appointed, its
// mail is written, and no bot desk runs beside it. Switched off and played by nobody, it is neither.
func TestPlayedRosterPilotHeadsAsAPerson(t *testing.T) {
	savedChairs := humanChairIndex.Load()
	savedSet, savedAt, savedOK := botMailboxSet, botMailboxAt, botMailboxEverOK
	savedSeats, savedSeatsAt := personSeatSet, personSeatAt
	defer func() {
		humanChairIndex.Store(savedChairs)
		botMailboxSet, botMailboxAt, botMailboxEverOK = savedSet, savedAt, savedOK
		personSeatSet, personSeatAt = savedSeats, savedSeatsAt
	}()
	for _, played := range []bool{true, false} {
		t.Run(fmt.Sprintf("played %v", played), func(t *testing.T) {
			f := newManageDB()
			f.answers = append([]fakeAnswer{
				{"SELECT ch.characterID, ch.characterName, COALESCE(r.corprole, 0), COALESCE(r.titleMask, 0)", func([]driver.NamedValue) []fakeRow {
					p := tPilots[tBotSec]
					return []fakeRow{{tBotSec, p.name, int64(p.roles), int64(0), p.title, int64(0), 1e6, true, int64(0)}}
				}},
				{"WHERE r.enabled = 0", func([]driver.NamedValue) []fakeRow {
					if played {
						return one1(tBotSec)
					}
					return nil
				}},
			}, f.answers...)
			personSeatAt = time.Time{}
			page := managePage(t, f, tCEO, true, url.Values{"div": {mgHeads}, "o": {"head"}, "pre": {"head"}, "pre_arm": {"security"}})
			if got := strings.Contains(page, "Hadar Okuumi (capsuleer)"); got != played {
				t.Errorf("offered as a capsuleer = %v", got)
			}
			if strings.Contains(page, "Hadar Okuumi (bot)") {
				t.Error("a pilot switched off is offered as a bot")
			}
			preview := managePage(t, f, tCEO, true, url.Values{"div": {mgHeads}, "form": {"head"}, "arm": {"security"}, "member": {fmt.Sprint(tBotSec)}})
			if got := strings.Contains(preview, "give its orders themselves"); got != played {
				t.Errorf("previewed as a person = %v", got)
			}

			f.answers = append([]fakeAnswer{{"FROM rolebot_corp_heads", func([]driver.NamedValue) []fakeRow {
				return []fakeRow{{tCorp, "security", tBotSec, int64(0), tCEO, tNow() - 86400, int64(0)}}
			}}}, f.answers...)
			db := sql.OpenDB(f)
			defer db.Close()
			m := &Mod{db: db}
			botMailboxSet, botMailboxAt, botMailboxEverOK = map[int64]struct{}{tBotSec: {}}, clock.Now(), true
			humanChairIndex.Store(&map[int64]int64{tCorp: tCEO})
			if got := len(m.chairSeatActivities(context.Background(), tCorp)) == 0; got != played {
				t.Errorf("no bot desk = %v", got)
			}
			if m.botFlown(tBotSec) == played {
				t.Errorf("botFlown = %v", !played)
			}
			if m.mailSuppressed(tBotSec) == played {
				t.Errorf("mailSuppressed = %v", !played)
			}
		})
	}
}
