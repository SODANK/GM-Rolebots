package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// The player side of the industry desk. /corp/industry (also /corp/work and /ig/corpwork) is the Industry
// department's page of the core's Corporate Portal, for the corporation of whoever opens it: the build board,
// the factory floor, the materials against the board, what the corporation is buying and the factory bases.
// Research and copies have their own page (corprnd_igb.go). The eve.charID header is the client's hint, not
// authorization, so the page shows nothing a member could not learn in game.
// The office-deposit hook: a player who puts an original blueprint into the corporation's hangar has given it
// capital and is paid in shares, as a bot donating one is (corplibrary.go, corpshares.go).

// handleIGBCorpIndustry serves /corp/industry and its old addresses
// /corp/work and /ig/corpwork (letters carry them).
func (m *Mod) handleIGBCorpIndustry(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	charID := esp.IGBCharID(r)
	var corpID int64
	if charID != 0 {
		corpID, _ = evedb.GetCharCorpID(ctx, m.db, charID)
	}
	c, _ := getCEO(ctx, m.db, corpID)
	if corpID == 0 || !m.corpHasWorkDesk(ctx, corpID) {
		esp.WriteCorpPortalPage(w, r, "industry", "<font size=3>Your corporation posts no build orders. "+
			"Corporations with an industry desk post paid work here; the chair posts it on the Orders page.</font>")
		return
	}
	if c != nil {
		facts, err := getAnyCorpFacts(ctx, m.db, corpID)
		if err != nil || !hasIndustryDesk(facts.Policy.Tags) {
			if work, _ := m.openWorkFor(ctx, corpID); len(work) == 0 {
				esp.WriteCorpPortalPage(w, r, "industry", "<font size=3>Your corporation does not build &mdash; "+
					"it posts no build orders.</font>")
				return
			}
		}
	}
	esp.WriteCorpPortalPage(w, r, "industry", m.industryBody(ctx, r, c, corpID, charID))
}

// industryBody is the Industry department's page (corpwork_igb.go draws its parts): how a member takes part,
// the figures, the build board, the factory floor, the materials against the board beside the buyback, the
// factory bases, the originals at the factory, then the desk and the department. Research and copies are the
// R&D page's.
func (m *Mod) industryBody(ctx context.Context, r *http.Request, c *corpCEO, corpID, viewer int64) string {
	pg := m.igbPageFor(r)
	work, _ := m.openWorkFor(ctx, corpID)
	factoryID, officeID := m.corpFactoryOffice(ctx, corpID)
	if factoryID == 0 {
		factoryID = m.corpFactoryStation(ctx, corpID)
	}
	jobs := m.corpRunningJobs(ctx, corpID)
	var library int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_library WHERE corporationID = ?`, corpID).Scan(&library)
	build, research := splitBoard(work)
	host := esp.IGBHost()

	var b strings.Builder

	// How to take part.
	m.writeTakePart(ctx, &b, pg, corpID, viewer, factoryID, jobs)
	b.WriteString("<br>\n")

	// The figures.
	runsLeft, wages := 0, 0.0
	for _, w := range build {
		runsLeft += w.RunsLeft()
		wages += float64(w.RunsLeft()) * w.WagePerRun
	}
	mfg := 0
	for _, j := range jobs {
		if j.Activity == 1 {
			mfg++
		}
	}
	pg.statStrip(&b, []igbStat{
		{Label: "Build orders", Value: strconv.Itoa(len(build))},
		{Label: "Runs wanted", Value: strconv.Itoa(runsLeft)},
		{Label: "Wages on offer", Value: esp.ISKShort(wages), Note: "ISK"},
		{Label: "Building now", Value: strconv.Itoa(mfg), Note: "jobs"},
		{Label: "Library", Value: strconv.Itoa(library), Note: "prints"},
	})
	b.WriteString("<br>\n")

	// The board, one panel per Industry base: work is taken at one's own.
	bases := m.industryBaseStations(ctx, corpID)
	esp.Section(&b, pg.pal, "The build board")
	if factoryID != 0 {
		where := "Paid work at " + esp.IGBText(m.stationLabel(ctx, factoryID))
		if len(bases) > 1 {
			where = fmt.Sprintf("Paid work at %d factory bases, each order at one; take it at the base you are posted to", len(bases))
		}
		b.WriteString(pg.soft(where+". The wage is per run, paid on delivery. "+fmt.Sprintf(`%d research and copy order(s) are on the `+
			`<a href="%s/corp/research">Research &amp; Development</a> page.`, len(research), host)) + "<br>\n")
	}
	if len(bases) > 1 {
		var grid [][]esp.Panel
		for _, st := range bases {
			var here []corpWork
			for _, w := range build {
				if orderStation(w, factoryID) == st {
					here = append(here, w)
				}
			}
			grid = append(grid, []esp.Panel{m.boardPanel(ctx, pg, "Building at "+esp.IGBText(stationNameOf(ctx, m, st)),
				"manufacture and refine", here, "Nothing posted at this base right now.")})
		}
		esp.PanelGrid(&b, pg.pal, grid)
	} else {
		esp.PanelGrid(&b, pg.pal, [][]esp.Panel{{
			m.boardPanel(ctx, pg, "Building", "manufacture and refine", build, "Nothing to build right now."),
		}})
	}
	b.WriteString("<br>\n")

	// In the factory now.
	esp.Section(&b, pg.pal, "On the factory floor")
	m.writeBuildJobs(ctx, &b, pg, jobs, viewer)
	b.WriteString("<br>\n")

	// Materials beside the buyback.
	esp.Section(&b, pg.pal, "Materials")
	m.writeMaterialsBand(ctx, &b, pg, corpID, factoryID, officeID, bases)
	b.WriteString("<br>\n")

	// The factory bases.
	esp.Section(&b, pg.pal, "Factory bases")
	m.writeFactoryBases(ctx, &b, pg, corpID, viewer)
	b.WriteString("<br>\n")

	// The library at the factory.
	esp.Section(&b, pg.pal, "Originals at the factory")
	m.writeLibraryPanels(ctx, &b, pg, corpID, factoryID, jobs)
	b.WriteString("<br>\n")

	// The desk, and the department.
	var ch Charter
	if c != nil {
		ch = ceoCharterOf(ctx, m, c)
	}
	// Both panels draw icon | what | label, so they share one grid and the
	// department's rows can carry the viewer's shade.
	desk := esp.Panel{Title: "The desk", Align: []string{"", "", ""}, Width: []string{"36", "", "150"}}
	if factoryID != 0 {
		desk.Rows = append(desk.Rows, []string{"", igbF3(esp.IGBText(m.stationLabel(ctx, factoryID))), pg.soft("factory")})
	}
	if ch.SRPCap > 0 {
		desk.Rows = append(desk.Rows, []string{"",
			igbF3("hulls up to " + esp.ISKShort(ch.SRPCap) + " ISK lost flying for the corporation"), pg.soft("ship replacement")})
	}
	desk.Rows = append(desk.Rows, []string{"", igbF3(fmt.Sprintf("%d print(s)", library)), pg.soft("library")})
	if p, err := m.loadCorpPolicy(ctx, corpID); err == nil && p.DirectorID != 0 {
		icon, name := m.pilotCells(ctx, p.DirectorID, 32)
		desk.Rows = append(desk.Rows, []string{icon, igbF3(name), pg.soft(esp.IGBText(corpIndustryDirector))})
	}
	staff := esp.Panel{Title: "The department", Align: []string{"", "", ""}, Width: []string{"36", "", "150"},
		Empty: "Nobody holds a seat on the desk."}
	for _, d := range m.industryDepartment(ctx, corpID) {
		icon, name := m.pilotCells(ctx, d.CharID, 32)
		shade := ""
		if d.CharID == viewer {
			shade = igbShadeMine
		}
		staff.Rows = append(staff.Rows, []string{icon, igbF3(name), pg.soft(esp.IGBText(d.Title))})
		staff.Shades = append(staff.Shades, shade)
	}
	esp.PanelGrid(&b, pg.pal, [][]esp.Panel{{desk, staff}})
	m.writeArmRequests(ctx, &b, pg, corpID, armIndustry)
	return b.String()
}

// ceoCharterOf is the CEO's current charter, resolved from its traits.
func ceoCharterOf(ctx context.Context, m *Mod, c *corpCEO) Charter {
	// Five minutes per corporation: the share and purchase-order paths ask
	// this per PILOT (two reads each), and a charter moves on a think.
	now := clock.Unix()
	if v, ok := ceoCharterMemo.Load(c.CorpID); ok {
		if e := v.(ceoCharterEntry); now-e.at < ceoCharterMemoSec {
			return e.ch
		}
	}
	traits, err := getTraits(ctx, m.db, c.CharID)
	if err != nil {
		traits = map[string]int{}
	}
	ch := m.chairCharter(ctx, c.CorpID, resolveCharter(pilotIdentity{CharID: c.CharID, Traits: traits, Tune: resolveTuning(traits)}))
	ceoCharterMemo.Store(c.CorpID, ceoCharterEntry{ch: ch, at: now})
	return ch
}

const ceoCharterMemoSec = 300

type ceoCharterEntry struct {
	ch Charter
	at int64
}

var ceoCharterMemo sync.Map // corpID → ceoCharterEntry

func init() {
	registerCorpForget("ceoCharterMemo", func(id int64) { ceoCharterMemo.Delete(id) })
	registerSweep("ceoCharterMemoSweep", func() {
		now := clock.Unix()
		ceoCharterMemo.Range(func(k, v any) bool {
			if now-v.(ceoCharterEntry).at >= 2*ceoCharterMemoSec {
				ceoCharterMemo.Delete(k)
			}
			return true
		})
	})
}

// onOfficeDeposit is esp.OfficeDepositHook: runs off the RPC goroutine.
func (m *Mod) onOfficeDeposit(corpID, charID, itemID, typeID, flag int64) {
	if _, ok := managedCEO(corpID); !ok {
		return
	}
	goSafe("office deposit", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// A print carried between bases is a delivery, not a donation
		// (corpprintmove.go).
		if !m.noteMoveDeposit(ctx, corpID, charID, itemID) {
			m.creditPrintDeposit(ctx, corpID, charID, itemID, typeID)
		}
		// An original in the corporation's keeping is locked at once, paid for or not (corpvault.go). Copies stay
		// free to circulate.
		m.lockArrivedPrint(ctx, corpID, itemID)
		m.noteAssignmentDeposit(ctx, corpID, charID, itemID, typeID, flag)
	})
}

// creditPrintDeposit pays a member in shares for an original blueprint it put
// into its corporation's hangar, and records it in the library as a donation.
func (m *Mod) creditPrintDeposit(ctx context.Context, corpID, charID, itemID, typeID int64) {
	facts, err := getAnyCorpFacts(ctx, m.db, corpID)
	if err != nil || !hasIndustryDesk(facts.Policy.Tags) {
		return
	}
	var isBP, isCopy int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM invBlueprintTypes WHERE blueprintTypeID = ?`, typeID).Scan(&isBP)
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(copy), 0) FROM invBlueprints WHERE blueprintID = ?`, itemID).Scan(&isCopy)
	if isBP == 0 || isCopy != 0 {
		return
	}
	var already int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_library WHERE itemID = ? AND donorID <> 0`, itemID).Scan(&already)
	if already > 0 {
		return // credited once; a print moved back and forth earns nothing more
	}
	value := evedb.TypeValueOf(ctx, m.db, typeID).Value
	var stationID int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT o.stationID FROM crpoffices o JOIN entity e ON e.locationID = o.itemID
		 WHERE e.itemID = ?`, itemID).Scan(&stationID)
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_library
			(itemID, corporationID, bpTypeID, stationID, source, cost, donorID, acquiredAt, lastSeenAt)
		VALUES (?, ?, ?, ?, 'donation', ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE source = 'donation', cost = VALUES(cost), donorID = VALUES(donorID)`,
		itemID, corpID, typeID, stationID, value, charID, now, now); err != nil {
		rlogf("rolebots: print deposit %d: %v", itemID, err)
		return
	}
	// Paid for in stock at the ask, as far as the treasury has any to sell (corpshares.go): the terms a bot
	// giving one gets.
	m.creditSharesInKind(ctx, corpID, charID, value, "a "+m.typeNameOr(ctx, typeID))
	if c, _ := getCEO(ctx, m.db, corpID); c != nil {
		m.ceoLog(ctx, c, powerProduction, "donateprint", charID, value,
			"%s gave the library a %s", m.charNameOr(ctx, charID), m.typeNameOr(ctx, typeID))
	}
}
