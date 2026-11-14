package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
)

// The module's side of the in-game browser kit (core/esp/igbkit_mod.go exports the renderer;
// the Corporate Portal sections build their pages from this file). The helpers favour reading
// at a glance: portraits for names, hull icons for types, tinted rows for words, bars for numbers.
//
// The renderer's rules hold on every page: flat tables only, no <form> inside a <td>,
// width=100% in source, named entities through esp.IGBText, YC dates, capsuleer wording.

// Row tints beside the palette's own alternation: a fleet mustering, one
// live, one standing down, and a loss. The registry's kill/loss greens and
// reds, dimmed to sit under text.
const (
	igbShadeLive   = "#0f2216"
	igbShadeMuster = "#2a2410"
	igbShadeDown   = "#1c1d20"
	igbShadeLoss   = "#251013"
	igbShadeMine   = "#16294a" // the row that is about the viewer
)

// igbPage is one request's palette, resolved once.
type igbPage struct {
	pal esp.IGBPalette
}

func (m *Mod) igbPageFor(r *http.Request) igbPage { return igbPage{pal: esp.CorpPortalPalette(r)} }

func igbF3(s string) string { return "<font size=3>" + s + "</font>" }

func (p igbPage) dim(s string) string {
	return "<font size=2 color=" + p.pal.Dim + ">" + s + "</font>"
}

func (p igbPage) dim3(s string) string {
	return "<font size=3 color=" + p.pal.Dim + ">" + s + "</font>"
}

func (p igbPage) accent(s string) string {
	return "<font size=3 color=" + p.pal.Accent + "><b>" + s + "</b></font>"
}

func (p igbPage) big(s string) string {
	return "<font size=6 color=" + p.pal.Title + "><b>" + s + "</b></font>"
}

func (p igbPage) tint(color, s string) string {
	return "<font size=3 color=" + color + "><b>" + s + "</b></font>"
}

// igbStat is one number on a status strip.
type igbStat struct {
	Label string
	Value string
	Note  string
}

// statStrip draws the numbers that answer "what's up" before anything else
// on the page: one band, one panel per figure, the figure at size 6.
func (p igbPage) statStrip(b *strings.Builder, stats []igbStat) {
	if len(stats) == 0 {
		return
	}
	panels := make([]esp.Panel, 0, len(stats))
	for _, s := range stats {
		cell := p.big(s.Value)
		if s.Note != "" {
			cell += " " + p.dim(s.Note)
		}
		panels = append(panels, esp.Panel{Title: s.Label, Align: []string{""}, Rows: [][]string{{cell}}})
	}
	esp.PanelBand(b, p.pal, panels)
}

// ratioBar draws two quantities as one bar split between two colours (killed against lost,
// spent against earned) with no number on it. The portal shows how the books lean, never what
// is in them (corpportal.go keeps nothing private). Nothing on either side draws an empty bar.
func (p igbPage) ratioBar(a, b float64, width int, ca, cb string) string {
	a, b = max(a, 0), max(b, 0)
	if a+b <= 0 {
		return esp.Bar(0, width, ca)
	}
	na := int(float64(width)*a/(a+b) + 0.5)
	return ratioBarCells(na, width, ca, cb)
}

// ratioBarCells is ratioBar's drawing: na marks in ca, the rest in cb.
func ratioBarCells(na, width int, ca, cb string) string {
	na = min(max(na, 0), width)
	return "<font color=" + ca + "><b>" + strings.Repeat("|", na) + "</b></font>" +
		"<font color=" + cb + "><b>" + strings.Repeat("|", width-na) + "</b></font>"
}

// relBar is v against the largest value of its set, for a strip of bars that
// compare to each other and not to any total.
func relBar(v, most float64, width int, color string) string {
	if most <= 0 {
		return esp.Bar(0, width, color)
	}
	return esp.Bar(v/most, width, color)
}

// igbWhen / igbAgo render a unix timestamp as a YC date with the clock, and
// as an age. Both at size 3: the size-2 font loses '.' and ':'.
func igbWhen(unix int64) string {
	if unix <= 0 {
		return "&mdash;"
	}
	return esp.YCTime(time.Unix(unix, 0))
}

func igbAgo(unix int64) string {
	if unix <= 0 {
		return "&mdash;"
	}
	return esp.RelTime(time.Unix(unix, 0))
}

// igbIn renders "in ~Nh Nm" for a deadline, or "now" once it has passed.
func igbIn(unix int64) string {
	d := unix - clock.Unix()
	switch {
	case unix <= 0:
		return "&mdash;"
	case d <= 0:
		return "now"
	case d < 3600:
		return fmt.Sprintf("in %dm", d/60)
	default:
		return fmt.Sprintf("in %dh %02dm", d/3600, (d%3600)/60)
	}
}

// pilotCells is a portrait beside a dossier link, the way a pilot appears in every panel.
// The icon column is fixed and the name takes the slack.
func (m *Mod) pilotCells(ctx context.Context, charID int64, size int) (icon, name string) {
	return esp.Portrait(charID, size), esp.PilotLink(charID, esp.IGBText(m.charNameOr(ctx, charID)))
}

// shipTypeOf is the hull a pilot is sitting in (0, "capsule" when none).
func (m *Mod) shipTypeOf(ctx context.Context, charID int64) (int64, string) {
	var typeID int64
	var name string
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(t.typeID, 0), COALESCE(t.typeName, '')
		  FROM entity ce
		  JOIN entity ship ON ship.itemID = ce.locationID AND ship.ownerID = ce.itemID
		  JOIN invTypes t ON t.typeID = ship.typeID
		 WHERE ce.itemID = ?`, charID).Scan(&typeID, &name)
	if typeID == 0 {
		return 0, "capsule"
	}
	return typeID, name
}

// corpPresence is who of a corporation is present right now, keyed by character: real
// clients and synthetic sessions alike, as the registry's by-corp lookup answers.
func (m *Mod) corpPresence(corpID int64) map[int64]esp.Session {
	out := map[int64]esp.Session{}
	if m.sessions == nil || corpID == 0 {
		return out
	}
	for _, s := range m.sessions.FindByCorpID(corpID) {
		if s != nil && s.CharID != 0 {
			out[s.CharID] = *s
		}
	}
	return out
}

// memberWhere words a member's place: present and docked, present and in
// space, or (from the character row) where they were last.
func (m *Mod) memberWhere(ctx context.Context, charID int64, present map[int64]esp.Session) (where string, inSpace bool, sysID int64) {
	if s, ok := present[charID]; ok {
		if s.StationID != 0 {
			return "docked &middot; " + esp.IGBText(m.stationLabel(ctx, s.StationID)), false, s.SolarSystemID
		}
		if s.SolarSystemID != 0 {
			return "in space &middot; " + esp.IGBText(m.systemLabel(ctx, s.SolarSystemID)), true, s.SolarSystemID
		}
	}
	var station, system int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(stationID, 0), COALESCE(solarSystemID, 0) FROM character_ WHERE characterID = ?`,
		charID).Scan(&station, &system)
	switch {
	case station != 0:
		return "last seen docked &middot; " + esp.IGBText(m.stationLabel(ctx, station)), false, system
	case system != 0:
		return "last seen &middot; " + esp.IGBText(m.systemLabel(ctx, system)), false, system
	}
	return "whereabouts unknown", false, 0
}

// systemCell is a system's name tinted by its security, the way the overview
// colours it.
func (m *Mod) systemCell(ctx context.Context, sysID int64) string {
	if sysID == 0 {
		return "&mdash;"
	}
	sec := m.systemSecurity(ctx, sysID)
	return "<font size=3 color=" + esp.SecColor(sec) + "><b>" + esp.IGBText(m.systemLabel(ctx, sysID)) +
		"</b></font> <font size=2 color=" + esp.SecColor(sec) + ">" + strconv.FormatFloat(sec, 'f', 1, 64) + "</font>"
}

// corpCEOID is the corporation's CEO from the game table, for any corporation and not
// only one with a bot in the chair.
func (m *Mod) corpCEOID(ctx context.Context, corpID int64) int64 {
	var ceo int64
	_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(ceoID, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&ceo)
	return ceo
}

// portalViewer resolves the pilot behind the identity header and their corporation, through
// the database. The header is a hint: it chooses which corporation's page to show and nothing more.
func (m *Mod) portalViewer(ctx context.Context, r *http.Request) (charID, corpID int64) {
	charID = esp.IGBCharID(r)
	if charID != 0 {
		corpID, _ = evedb.GetCharCorpID(ctx, m.db, charID)
	}
	return charID, corpID
}

// roleIcon is a representative hull icon per fleet role: what the seat flies,
// read faster than the word.
func roleIcon(role string) string {
	var typeID int64
	switch role {
	case RoleMiner:
		typeID = 17480 // Retriever
	case RoleHauler:
		typeID = 648 // Badger
	case RoleCombat, RoleEscort:
		typeID = 622 // Stabber
	case RoleHunter:
		typeID = 11186 // Malediction
	case RoleTackler:
		typeID = 587 // Rifter
	case RoleLogi:
		typeID = 624 // Osprey
	case RoleEwar:
		typeID = 609 // Griffin
	case RoleTrader:
		typeID = 649 // Tayra
	}
	return esp.ShipIcon(typeID, 24)
}

// fleetShade is the row tint for an op's lifecycle stage.
func fleetShade(g *Gang) string {
	switch g.State.OpPhase {
	case "muster":
		return igbShadeMuster
	case "winddown", "settled":
		return igbShadeDown
	}
	if !g.Enabled || g.Activity == "" {
		return igbShadeDown
	}
	return igbShadeLive
}

// igbSoftText is the tone for secondary text that still has to be read (what a member is
// doing, a row's second line). The palette's Dim is for labels and is hard to read at size 2
// on charcoal.
const igbSoftText = "#aab2bd"

// soft is secondary text at a readable size.
func (p igbPage) soft(s string) string {
	return "<font size=3 color=" + igbSoftText + ">" + s + "</font>"
}

// colored is size-3 text in a colour, unbolded.
func colored(color, s string) string {
	return "<font size=3 color=" + color + ">" + s + "</font>"
}

// armColor is each department's colour on the portal, readable on charcoal.
func armColor(a corpArm) string {
	switch a {
	case armSecurity:
		return "#e07a6a"
	case armMining:
		return "#e3bc5a"
	case armIndustry:
		return "#78aee0"
	case armResearch:
		return "#b39ce0"
	case armSales:
		return "#78d39a"
	case armInfrastructure:
		return "#e0a070"
	case armHR:
		return "#d8a6c8"
	case armFinance:
		return "#d2d2a0"
	}
	return igbSoftText
}

// armIconID is a department's icon, borrowed from the client's own interface
// so it reads as the thing the department does: a station service button, a
// corp window tab, a neocom entry. "" for the unassigned pool.
func armIconID(a corpArm) string {
	switch a {
	case armResearch:
		return "33_02" // Research Facility (cmd/clientpatch)
	case armMining:
		return "17_01" // Reprocessing Plant
	case armSecurity:
		return "07_07" // corp window Wars
	case armSales:
		return "18_01" // Market
	case armIndustry:
		return "18_04" // Repair Shop
	case armHR:
		return "12_01" // EVEmail
	case armFinance:
		return "07_12" // Wallet
	case armInfrastructure:
		return "07_13" // Assets
	}
	return ""
}

// armIcon is armIconID drawn, or the corporation's logo for the unassigned.
func armIcon(a corpArm, corpID int64, size int) string {
	if id := armIconID(a); id != "" {
		return esp.UIIcon(id, size, armLabel(a))
	}
	return esp.CorpLogo(corpID, size)
}

// armName is a department's name in its colour, bold.
func armName(a corpArm) string {
	label := armLabel(a)
	if a == "" {
		label = "Unassigned"
	}
	return "<font size=3 color=" + armColor(a) + "><b>" + esp.IGBText(label) + "</b></font>"
}

// armRoleShort is what an arm's bundle opens, in the client's own role
// names, short enough for a card.
func armRoleShort(mask uint64) string {
	var out []string
	if mask&corp.RoleFactoryManager != 0 {
		out = append(out, "Factory Manager")
	}
	if mask&(corp.RoleHangarCanQuery1|corp.RoleHangarCanQuery2|corp.RoleHangarCanQuery3) != 0 {
		out = append(out, "view Materials, Library and Production")
	}
	if mask&corp.RoleSecurityOfficer != 0 {
		out = append(out, "Security Officer")
	}
	if mask&corp.RoleEquipmentConfig != 0 {
		out = append(out, "Config Equipment")
	}
	if mask&corp.RoleStationManager != 0 {
		out = append(out, "Station Manager")
	}
	if mask&corp.RoleCanRentOffice != 0 {
		out = append(out, "Rent Office")
	}
	if mask&corp.RoleChatManager != 0 {
		out = append(out, "Communications Officer")
	}
	if mask&corp.RoleAuditor != 0 {
		out = append(out, "Auditor")
	}
	if mask&corp.RoleJuniorAccountant != 0 {
		out = append(out, "Junior Accountant")
	}
	return strings.Join(out, ", ")
}
