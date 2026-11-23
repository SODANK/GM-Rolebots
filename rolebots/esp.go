package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// The Rolebot ESP category. Pages render through the panel's layout and template set
// (esp.ModulePages, esp.ModuleRender) and the sidebar gains a Rolebot section
// (esp.ModuleNav). All of it comes and goes with the module.

// registerESP mounts routes, sidebar section and page templates. It must run before
// esp.New; modapi Init is called before the ESP server starts.
func (m *Mod) registerESP() {
	esp.ModuleHandlers["/rolebots"] = m.handleCast
	esp.ModuleHandlers["/rolebots/"] = m.handleRolebotsSub
	esp.ModuleHandlers["/rolebot/"] = m.handleProfile

	esp.ModuleNav = append(esp.ModuleNav, esp.ModuleNavSection{
		Title: "Rolebot",
		Links: []esp.ModuleNavLink{
			// One entry, four tabs: the Main Cast, the generator, the generated population and
			// what it has learned are views of one subject, linked by a sub-tab strip (esptabs.go).
			{Label: "Bot Population", Path: "/rolebots", Page: "rolebots"},
			// Player fleets are a card at the bottom of Gangs: both answer who is flying with whom.
			{Label: "Gangs", Path: "/rolebots/gangs", Page: "rolebot-gangs"},
			// One entry, two tabs (rb_chattabs, espchatter_tabs.go): bots talking in corp chat
			// and Local where a person is reading (espchatter.go), and the bot drama they post
			// on the Intergalactic Summit forum (espsummit.go).
			{Label: "Chatter", Path: "/rolebots/chatter", Page: "rolebot-chatter"},
			// One entry, tabs in rb_corptabs (espceo.go): corporations, CEOs, NPC corps, alliances,
			// diplomacy (esprelations.go, the only page that can write a corporation's opinion of
			// another), departments (corparm_esp.go) and morale.
			{Label: "Corps / Alliances", Path: "/rolebots/corps", Page: "rolebot-corps"},
			// Industry is a section of its own: it crosses the roster, the market and the map.
			{Label: "Industry", Path: "/rolebots/industry", Page: "rolebot-industry"},
			// Bot corporations' towers, sieges, and the starbase switches
			// (espstarbase.go).
			{Label: "Starbases", Path: "/rolebots/starbases", Page: "rolebot-starbases"},
			// One entry, five tabs (rb_tradetabs, esptrade_tmpl.go): overview, hub map, trade feed,
			// bot-to-bot ledger and wants list share one sidebar entry and one strip.
			{Label: "Economy", Path: "/rolebots/economy", Page: "rolebot-economy"},
			{Label: "Activity Log", Path: "/rolebots/log", Page: "rolebot-log"},
			// Stats crosses every page above, so it is a sidebar entry and not a sub-tab.
			{Label: "Stats", Path: "/rolebots/stats", Page: "rolebot-stats"},
		},
	})

	esp.ModulePages["rolebots_cast.html"] = tmplCast
	esp.ModulePages["rolebots_seed.html"] = tmplSeed
	esp.ModulePages["rolebots_generated.html"] = tmplGenerated
	esp.ModulePages["rolebots_profile.html"] = tmplProfile
	esp.ModulePages["rolebots_skills.html"] = tmplSkills
	esp.ModulePages["rolebots_skillboard.html"] = tmplSkillBoard
	esp.ModulePages["rolebots_timezones.html"] = tmplTimezones
	esp.ModulePages["rolebots_factory.html"] = tmplFactory
	esp.ModulePages["rolebots_log.html"] = tmplLog
	esp.ModulePages["rolebots_trademap.html"] = tmplTradeMap
	esp.ModulePages["rolebots_trademap_region.html"] = tmplTradeMapRegion
	esp.ModulePages["rolebots_trades.html"] = tmplTrades
	esp.ModulePages["rolebots_wants.html"] = tmplWants
	esp.ModulePages["rolebots_minerals.html"] = tmplMinerals
	esp.ModulePages["rolebots_want.html"] = tmplWant
	esp.ModulePages["rolebots_peers.html"] = tmplPeers
	esp.ModulePages["rolebots_economy.html"] = tmplEconomy
	esp.ModulePages["rolebots_corps.html"] = tmplCorps
	esp.ModulePages["rolebots_npccorps.html"] = tmplCorpsNPC
	esp.ModulePages["rolebots_corp.html"] = tmplCorpPage + tmplTowerTable
	esp.ModulePages["rolebots_corp_npc.html"] = tmplCorpNPC
	esp.ModulePages["rolebots_ceos.html"] = tmplCEOs
	esp.ModulePages["rolebots_alliances.html"] = tmplAlliances
	esp.ModulePages["rolebots_morale.html"] = tmplMorale
	// The core's /corporation/<id> page links back to the policy page for every
	// corporation, and to the CEO page for a bot-run one.
	esp.CorpPageLinks = m.corpPageLinks
	// deskMod is the module handle the industry desk (corpigb.go) and the member desk
	// (corpdesk.go) use for their pages and signed links.
	deskMod = m
	// The desk pages are sections of the core's Corporate Portal (/corp/). The /ig/
	// addresses stay registered with the same handlers because sent letters carry them.
	esp.CorpPortalSections = append(esp.CorpPortalSections,
		esp.CorpPortalSection{Key: "bulletins", Label: "Bulletins", Path: "/corp/bulletins", Order: 2, Handler: m.handleIGBCorpNews},
		// The job board (corpassign_igb.go): work the departments post for a pilot to take.
		esp.CorpPortalSection{Key: "assignments", Label: "Assignments", Path: assignBoardPortalPath, Order: 2, Handler: m.handleIGBCorpAssignments},
		// One page per department (corpigb.go, corprnd_igb.go, corpdept_igb.go).
		esp.CorpPortalSection{Key: "industry", Label: "Industry", Path: "/corp/industry", Order: 3, Handler: m.handleIGBCorpIndustry},
		esp.CorpPortalSection{Key: "research", Label: "R&amp;D", Path: "/corp/research", Order: 4, Handler: m.handleIGBCorpResearch},
		esp.CorpPortalSection{Key: "mining", Label: "Mining", Path: "/corp/mining", Order: 5, Handler: m.handleIGBCorpMining},
		esp.CorpPortalSection{Key: "infra", Label: "Infrastructure", Path: "/corp/infra", Order: 6, Handler: m.handleIGBCorpInfra},
		// corpsecurity_igb.go, corptrade_igb.go.
		esp.CorpPortalSection{Key: "security", Label: "Security", Path: "/corp/security", Order: 7, Handler: m.handleIGBCorpSecurity},
		esp.CorpPortalSection{Key: "trade", Label: "Trade", Path: "/corp/trade", Order: 8, Handler: m.handleIGBCorpTrade},
		esp.CorpPortalSection{Key: "shares", Label: "Shares", Path: "/corp/shares", Order: 9, Handler: m.handleIGBCorpShares},
		esp.CorpPortalSection{Key: "ops", Label: "Operations", Path: "/corp/ops", Order: 10, Handler: m.handleIGBCorpOps},
		esp.CorpPortalSection{Key: "orders", Label: "Orders", Path: "/corp/orders", Order: 11, Handler: m.handleIGBCorpOrders},
		esp.CorpPortalSection{Key: "relations", Label: "Relations", Path: relationsPath, Order: 12, Handler: m.handleIGBCorpRelations},
		esp.CorpPortalSection{Key: "alliance", Label: "Alliance", Path: alliancePortalPath, Order: 13, Handler: m.handleIGBCorpAlliance},
		// Every department's requests of the others, and the hangar each goes
		// into (corprequest_igb.go). Beside the department pages.
		esp.CorpPortalSection{Key: "requests", Label: "Requests", Path: requestsPath, Order: 9, Handler: m.handleIGBCorpRequests},
		// An NPC corporation's own members see how their chair runs it
		// (npcchair_igb.go); nobody else is shown the section.
		esp.CorpPortalSection{Key: "chair", Label: "Chair", Path: npcChairPortalPath, Order: 2, Handler: m.handleIGBCorpChair, Chartered: true},
	)
	esp.CorpPortalChartered = npcChaired
	esp.IGBModuleHandlers["/ig/corprelations"] = m.handleIGBCorpRelations
	esp.IGBModuleHandlers["/ig/corpops"] = m.handleIGBCorpOps
	esp.IGBModuleHandlers["/ig/corporders"] = m.handleIGBCorpOrders
	// Work-order addresses in sent letters serve the Industry page directly.
	esp.IGBModuleHandlers["/ig/corpwork"] = m.handleIGBCorpIndustry
	esp.IGBModuleHandlers["/corp/work"] = m.handleIGBCorpIndustry
	esp.IGBModuleHandlers["/ig/corpshares"] = m.handleIGBCorpShares
	esp.IGBModuleHandlers["/ig/corpnews"] = m.handleIGBCorpNews
	esp.IGBModuleHandlers["/ig/corpdesk"] = m.handleIGBCorpDesk
	// The Overview's departments and the mailed Join links behind them
	// (corpoverview_igb.go, corpjoin_igb.go).
	esp.IGBModuleHandlers["/corp/join"] = m.handleIGBCorpJoin
	esp.CorpOverviewSections = append(esp.CorpOverviewSections,
		esp.CorpOverviewSection{Order: 10, Render: m.renderCorpOverview})
	esp.OfficeDepositHook = m.onOfficeDeposit
	esp.OfficeTakeHook = m.onOfficeTake
	// A Base a human set in the corp window is final for the posting pass
	// (corpposting.go).
	esp.MemberBaseChangedHook = m.onMemberBaseChanged
	// A pilot giving shares back to the corporation that issued them is
	// selling them to its buyback (corpshares.go).
	esp.ShareMovedHook = m.onShareMoved
	// A corporation closed by the core (ESP delete, or a sole CEO resigning in game)
	// drops its volatile memory here.
	evedb.SubscribeCorpDeleted(forgetCorp)
	esp.ModulePages["rolebots_relations.html"] = tmplRelations
	esp.ModulePages["rolebots_gangs.html"] = tmplGangs
	esp.ModulePages["rolebots_gang_view.html"] = tmplGangView
	esp.ModulePages["rolebots_gang_boosts.html"] = tmplGangBoosts
	esp.ModulePages["rolebots_industry.html"] = tmplIndustry
	esp.ModulePages["rolebots_ore.html"] = tmplOre
	esp.ModulePages["rolebots_stats.html"] = tmplStats
	esp.ModulePages["rolebots_starbases.html"] = tmplStarbases + tmplTowerTable
	esp.ModulePages["rolebots_chatter.html"] = tmplChatter
	esp.ModulePages["rolebots_summit.html"] = tmplSummit
	esp.ModulePages["rolebots_arms.html"] = tmplArms
	esp.ModulePages["rolebots_arm_chart.html"] = tmplArmChart
	// The Mind Clash tab exists only when the MindClash module is compiled in
	// (build tag mindclash): espclash.go or clash_off.go.
	m.clashESP()
	esp.ModulePages["rolebots_error.html"] = tmplError
}

// tmplError is the module's error page. It reads only the error text and the section,
// so a handler can render it when the page's own data could not be built.
const tmplError = `{{define "content"}}
<h2>{{.Section}}</h2>
<div class="alert alert-error">{{.Err}}</div>
<div class="card">
  <p><a href="{{.BackPath}}">&larr; Back to {{.BackLabel}}</a></p>
</div>
{{end}}`

// espSection is one page's error context: section title, back link, and the nav entry
// and sub-tab it belongs to. The panel layout matches Nav to Page exactly; an empty Nav
// means the key is the nav key.
type espSection struct {
	Title, Path, Label string
	Nav, Tab           string
}

// espSections maps a page key to that context. Unknown keys fall back to the
// Main Cast.
var espSections = map[string]espSection{
	"rolebots":          {Title: "Main Cast", Path: "/rolebots", Label: "the Main Cast", Nav: "rolebots", Tab: "cast"},
	"rolebot-seed":      {Title: "RoleBot Generation", Path: "/rolebots/seed", Label: "the generation page", Nav: "rolebots", Tab: "gen"},
	"rolebot-gen":       {Title: "Generated Characters", Path: "/rolebots/generated", Label: "the population list", Nav: "rolebots", Tab: "pop"},
	"rolebot-skills":    {Title: "Roster Skills", Path: "/rolebots/skills", Label: "the roster's skills", Nav: "rolebots", Tab: "skills"},
	"rolebot-timezones": {Title: "Timezones", Path: "/rolebots/timezones", Label: "the roster's play hours", Nav: "rolebots", Tab: "tz"},
	"rolebot-gangs":     {Title: "Rolebot Gangs", Path: "/rolebots/gangs", Label: "the gang list"},
	"rolebot-chatter":   {Title: "Chatter", Path: "/rolebots/chatter", Label: "the chatter board", Nav: "rolebot-chatter", Tab: "chatter"},
	"rolebot-summit":    {Title: "Summit Desk", Path: "/rolebots/summit", Label: "the Summit desk", Nav: "rolebot-chatter", Tab: "summit"},
	"rolebot-corps":     {Title: "Corporations", Path: "/rolebots/corps", Label: "the corporation dashboard", Nav: "rolebot-corps", Tab: "corps"},
	"rolebot-ceos":      {Title: "Bot CEOs", Path: "/rolebots/ceos", Label: "the CEO list", Nav: "rolebot-corps", Tab: "ceos"},
	"rolebot-alliances": {Title: "Alliances", Path: "/rolebots/corps/alliances", Label: "the alliances board", Nav: "rolebot-corps", Tab: "alliances"},
	"rolebot-morale":    {Title: "Corporate Morale", Path: "/rolebots/corps/morale", Label: "the morale board", Nav: "rolebot-corps", Tab: "morale"},
	"rolebot-relations": {Title: "Diplomacy", Path: "/rolebots/corps/diplomacy", Label: "the diplomacy board", Nav: "rolebot-corps", Tab: "diplomacy"},
	"rolebot-arms":      {Title: "Departments", Path: "/rolebots/corps/departments", Label: "the departments board", Nav: "rolebot-corps", Tab: "departments"},
	"rolebot-log":       {Title: "Rolebot Activity Log", Path: "/rolebots/log", Label: "the activity log"},
	"rolebot-stats":     {Title: "Rolebot Stats", Path: "/rolebots/stats", Label: "the stats board"},
	// The three industry views share Nav rolebot-industry (espindustry_tabs.go) and differ by Tab.
	"rolebot-industry": {Title: "Rolebot Industry", Path: "/rolebots/industry", Label: "the industry board", Nav: "rolebot-industry", Tab: "pipeline"},
	"rolebot-ore":      {Title: "Ore Supply", Path: "/rolebots/ore", Label: "the ore board", Nav: "rolebot-industry", Tab: "ore"},
	"rolebot-minerals": {Title: "Mineral Supply", Path: "/rolebots/minerals", Label: "the mineral supply page", Nav: "rolebot-industry", Tab: "minerals"},

	// The five economy views share Nav rolebot-economy (rb_tradetabs) and differ by Tab.
	// The page keys are the ones every espError call site names.
	"rolebot-economy":  {Title: "Economy", Path: "/rolebots/economy", Label: "the economy overview", Nav: "rolebot-economy", Tab: "overview"},
	"rolebot-trademap": {Title: "Trade Hubs", Path: "/rolebots/trademap", Label: "the hub map", Nav: "rolebot-economy", Tab: "hubs"},
	"rolebot-trades":   {Title: "Trade Feed", Path: "/rolebots/trades", Label: "the trade feed", Nav: "rolebot-economy", Tab: "feed"},
	"rolebot-wants":    {Title: "Wants List", Path: "/rolebots/wants", Label: "the wants list", Nav: "rolebot-economy", Tab: "wants"},
	"rolebot-peers":    {Title: "Bot-to-Bot Trades", Path: "/rolebots/peers", Label: "the bot-to-bot ledger", Nav: "rolebot-economy", Tab: "peers"},
}

// espRender renders a module page through the panel layout.
func espRender(w http.ResponseWriter, page string, data map[string]any) {
	if esp.ModuleRender == nil {
		http.Error(w, "ESP module renderer not initialised", http.StatusInternalServerError)
		return
	}
	esp.ModuleRender(w, page, data)
}

// espError renders the module's error page and keeps the sidebar on the section
// the failure happened in.
func espError(w http.ResponseWriter, page, msg string) {
	sec, ok := espSections[page]
	if !ok {
		page = "rolebots"
		sec = espSections[page]
	}
	nav := sec.Nav
	if nav == "" {
		nav = page
	}
	espRender(w, "rolebots_error.html", map[string]any{
		"Page":      nav,
		"Tab":       sec.Tab,
		"Section":   sec.Title,
		"BackPath":  sec.Path,
		"BackLabel": sec.Label,
		"Err":       msg,
	})
}

func redirect(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	// The path may already carry a query string, so the separator is & in that case.
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	if msg != "" {
		path += sep + "msg=" + url.QueryEscape(msg)
	} else if errMsg != "" {
		path += sep + "err=" + url.QueryEscape(errMsg)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// handleRolebotsSub routes the /rolebots/ subtree to the page handlers.
func (m *Mod) handleRolebotsSub(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		http.Redirect(w, r, "/rolebots", http.StatusSeeOther)
		return
	}
	switch parts[1] {
	case "seed":
		m.handleSeed(w, r)
	case "generated":
		m.handleGenRoster(w, r)
	case "skills":
		m.handleSkillBoard(w, r)
	case "timezones":
		m.handleTimezones(w, r)
	case "gangs":
		if len(parts) >= 3 && parts[2] == "boosts" {
			m.handleGangBoosts(w, r)
			return
		}
		m.handleGangs(w, r)
	case "corps":
		if len(parts) >= 3 && parts[2] == "npc" {
			m.handleCorpsNPC(w, r)
			return
		}
		if len(parts) >= 3 && parts[2] == "alliances" {
			m.handleCorpsAlliances(w, r)
			return
		}
		if len(parts) >= 3 && parts[2] == "morale" {
			m.handleCorpsMorale(w, r)
			return
		}
		if len(parts) >= 3 && parts[2] == "diplomacy" {
			m.handleRelations(w, r)
			return
		}
		// /rolebots/corps/departments is the board, /rolebots/corps/departments/<id> one chart.
		if len(parts) >= 3 && parts[2] == "departments" {
			m.handleArmsBoard(w, r)
			return
		}
		m.handleCorps(w, r)
	case "relations":
		// The page was renamed Diplomacy and moved under Corps / Alliances.
		http.Redirect(w, r, "/rolebots/corps/diplomacy", http.StatusMovedPermanently)
	case "clash":
		m.handleClashTab(w, r)
	case "industry":
		m.handleIndustryBoard(w, r)
	case "starbases":
		m.handleStarbaseBoard(w, r)
	case "sites":
		// The board is gone: sites are tracked by the core's dungeon spawner pages.
		http.Redirect(w, r, "/rolebots", http.StatusSeeOther)
	case "chatter":
		m.handleChatterBoard(w, r)
	case "summit":
		m.handleSummitBoard(w, r)
	case "arms":
		// The page was renamed Departments and moved under Corps / Alliances.
		dest := "/rolebots/corps/departments" + strings.TrimPrefix(strings.TrimRight(r.URL.Path, "/"), "/rolebots/arms")
		http.Redirect(w, r, dest, http.StatusMovedPermanently)
	case "ceos":
		m.handleCEOs(w, r)
	case "corp", "ceo":
		// /rolebots/corp/<corporationID> and /rolebots/ceo/<corporationID> are the same page.
		// handleCorpPage picks the player page or the NPC one.
		if len(parts) >= 3 {
			if corpID, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				m.handleCorpPage(w, r, corpID)
				return
			}
		}
		http.NotFound(w, r)
	case "gang":
		if len(parts) >= 3 {
			if gangID, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				m.handleGangView(w, r, gangID)
				return
			}
		}
		http.NotFound(w, r)
	case "fits", "fit":
		// The fit editor no longer exists (fitgen.go); old bookmarks land on the roster.
		http.Redirect(w, r, "/rolebots", http.StatusSeeOther)
	case "economy":
		m.handleEconomy(w, r)
	case "trademap":
		if len(parts) >= 3 && parts[2] == "api" {
			m.handleTradeMapAPI(w, r)
			return
		}
		// /rolebots/trademap/region/<id> draws one region on its own.
		if len(parts) >= 4 && parts[2] == "region" {
			if regionID, err := strconv.ParseInt(parts[3], 10, 64); err == nil {
				m.handleTradeMapRegion(w, r, regionID)
				return
			}
			http.NotFound(w, r)
			return
		}
		m.handleTradeMap(w, r)
	case "trades":
		if len(parts) >= 3 && parts[2] == "api" {
			m.handleTradeFeedAPI(w, r)
			return
		}
		m.handleTradeFeed(w, r)
	case "conscripts":
		// The player-fleet card is on the Gangs page; the old path redirects there.
		http.Redirect(w, r, "/rolebots/gangs", http.StatusFound)
	case "stats":
		m.handleStats(w, r)
	case "minerals":
		m.handleMineralSupply(w, r)
	case "ore":
		m.handleOreBoard(w, r)
	case "wants":
		m.handleWants(w, r)
	case "want":
		// /rolebots/want/<typeID> shows one line of the capability census and what the
		// roster has done about it.
		if len(parts) >= 3 {
			if typeID, err := strconv.ParseInt(parts[2], 10, 64); err == nil {
				m.handleWantPage(w, r, typeID)
				return
			}
		}
		http.NotFound(w, r)
	case "peers":
		if len(parts) >= 3 && parts[2] == "api" {
			m.handlePeersAPI(w, r)
			return
		}
		m.handlePeers(w, r)
	case "log":
		if len(parts) >= 3 && parts[2] == "api" {
			m.handleLogAPI(w, r)
			return
		}
		m.handleLog(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ── /rolebot/<charID>: profile ─────────────────────────────────────────────

const tmplProfile = `{{define "content"}}
<style>
.rb-head { display:flex; align-items:center; gap:20px; margin-bottom:14px; }
.rb-head img.pf { width:192px; height:192px; border-radius:10px; background:#170a0a; object-fit:cover; box-shadow:0 0 0 1px #382020; }
.rb-head .sub { color:#9ca3af; font-size:13px; margin-top:2px; }
.rb-badge { display:inline-block; padding:2px 10px; border-radius:10px; font-size:11px; background:#382020; color:#9ca3af; vertical-align:middle; }
.rb-badge.on { background:#0f2a1f; color:#6ee7b7; }
.rb-grid { display:grid; grid-template-columns:1fr 1fr; gap:16px; }
@media (max-width: 1100px){ .rb-grid { grid-template-columns:1fr; } }
.rb-pick { max-height:260px; overflow-y:auto; border:1px solid #382020; border-radius:4px; padding:4px 8px; background:#170a0a; }
.rb-pick .row { display:flex; align-items:center; justify-content:space-between; padding:2px 0; border-bottom:1px solid #241313; font-size:13px; }
.rb-pick .row:last-child { border-bottom:none; }
.rb-pick select { font-size:12px; padding:2px; width:110px; }
.rb-lbl { color:#6b7280; font-size:12px; margin:10px 0 3px; }
.rb-scale { color:#6b7280; font-size:11px; margin:0 0 4px; }
.rb-loyal { font-size:12px; color:#9ca3af; margin:0 0 8px; }
.rb-scale b { color:#9ca3af; font-weight:600; }
.rb-tr-grid { display:grid; grid-template-columns:1fr 1fr; gap:10px 14px; }
@media (max-width:1100px){ .rb-tr-grid { grid-template-columns:1fr; } }
.rb-tr { background:#170a0a; border:1px solid #382020; border-radius:8px; padding:10px 14px 9px; }
.rb-tr-head { display:flex; align-items:baseline; gap:10px; margin-bottom:7px; }
.rb-tr-head strong { font-size:14px; }
.rb-tr-what { flex:1; font-size:11.5px; color:#9ca3af; }
.rb-tr-val { font-variant-numeric:tabular-nums; font-size:12px; color:#6b7280; min-width:36px; text-align:right; }
.rb-tr-val.set { color:#fbbf24; font-weight:600; }
.rb-tr input[type=range] { width:100%; margin:0 0 3px; -webkit-appearance:none; appearance:none; height:8px; border-radius:4px; cursor:pointer;
  background:var(--rb-fill, linear-gradient(to right, rgba(34,197,94,.20) 50%, rgba(239,68,68,.20) 50%)); }
.rb-tr input[type=range]::-webkit-slider-runnable-track { height:8px; border-radius:4px; background:transparent; }
.rb-tr input[type=range]::-moz-range-track { height:8px; border-radius:4px; background:transparent; }
.rb-tr input[type=range]::-webkit-slider-thumb { -webkit-appearance:none; width:16px; height:16px; margin-top:-4px; border-radius:50%; background:#e5e7eb; border:2px solid #170a0a; }
.rb-tr input[type=range]::-moz-range-thumb { width:12px; height:12px; border-radius:50%; background:#e5e7eb; border:2px solid #170a0a; }
.rb-tr-poles { display:flex; justify-content:space-between; font-size:11.5px; margin-bottom:5px; }
.rb-tr-poles .lo { color:#86efac; }
.rb-tr-poles .mid { color:#4b5563; font-size:10.5px; }
.rb-tr-poles .hi { color:#fca5a5; }
.rb-tr-desc { display:flex; justify-content:space-between; gap:16px; font-size:11px; color:#8b949e; line-height:1.5; }
.rb-tr-desc span { flex:1; max-width:48%; }
.rb-tr-desc span:last-child { text-align:right; }
.rb-drop { position:relative; }
.rb-drop .list { position:absolute; z-index:30; left:0; right:0; top:100%; background:#271111; border:1px solid #382020; border-radius:4px; max-height:220px; overflow-y:auto; display:none; }
.rb-drop .list div { padding:5px 8px; cursor:pointer; font-size:13px; }
.rb-drop .list div:hover { background:#382020; }
.rb-check { display:block; margin:4px 0; font-size:13px; }
.rb-incl td { padding:5px 8px; vertical-align:middle; }
.rb-incl .bar { display:inline-block; width:120px; height:8px; border-radius:4px; background:#241313; vertical-align:middle; overflow:hidden; }
.rb-incl .bar i { display:block; height:100%; background:#7f1d1d; }
.rb-incl .bar i.hot { background:#dc2626; }
.rb-incl .muted { color:#6b7280; font-size:12px; }
</style>

<div class="rb-head">
  <img class="pf" src="/portrait/{{.Bot.CharacterID}}?size=256" onerror="this.style.display='none'">
  <div>
    <h2 style="margin:0">{{.Status.CharacterName}}
      {{if .Bot.Enabled}}<span class="rb-badge on">possessed</span>{{else}}<span class="rb-badge">released</span>{{end}}
    </h2>
    <div class="sub">
      rolebot {{.Bot.CharacterID}} &middot; {{.Status.CorpName}} &middot;
      {{if .Status.Docked}}Docked — {{.Status.StationName}}{{else}}In space — {{.Status.SystemName}}{{end}}
      {{if .Status.ShipTypeName}}&middot; flying {{.Status.ShipTypeName}} <img src="/typeicon/{{.Status.ShipTypeID}}.png" width="22" height="22" style="vertical-align:middle;border-radius:3px;background:#170a0a" onerror="this.style.display='none'">{{end}}
    </div>
    {{if .SimPhase}}<div class="sub">Sim: <strong>{{if .SimActivity}}{{.SimActivity}}{{else}}idle{{end}}</strong> — phase <code>{{.SimPhase}}</code>{{if .SimUpdated}}, updated {{.SimUpdated}}{{end}}</div>{{end}}
  </div>
</div>

{{if .Msg}}<div class="alert alert-success">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}
{{if .SimHalted}}<div class="alert alert-error">
  <strong>HALTED</strong> — this bot hit a dead end and is doing nothing: {{.SimError}}
  <form method="POST" style="display:inline;margin-left:8px">
    <input type="hidden" name="action" value="reboot">
    <button type="submit">Reboot</button>
  </form>
</div>{{end}}

<div class="card">
  <h3>Possession</h3>
  <p style="font-size:13px">
    {{if .Bot.Enabled}}<strong style="color:#22c55e">ENABLED</strong> — RoleBot owns this character; player login is refused.{{else}}<strong style="color:#ef4444">DISABLED</strong> — players may log in on this character.{{end}}
  </p>
  <form method="POST" style="display:inline">
    <input type="hidden" name="action" value="enabled">
    <input type="hidden" name="state" value="{{if .Bot.Enabled}}off{{else}}on{{end}}">
    <button type="submit">{{if .Bot.Enabled}}Disable — release the character{{else}}Enable — RoleBot takes possession{{end}}</button>
  </form>
  <form method="POST" style="display:inline;margin-left:6px">
    <input type="hidden" name="action" value="reboot">
    <button type="submit">Reboot the sim</button>
  </form>
  <form method="POST" style="display:inline;margin-left:6px" onsubmit="return confirm('Wipe this bot\'s memory? It forgets the belts it has worked, how tired it is of each kind of work, and what it has been hauling.');">
    <input type="hidden" name="action" value="wipe">
    <button type="submit">Wipe memory</button>
  </form>
  <p style="margin-top:8px;font-size:12px;color:#6b7280">Reboot clears the phase and any halt. The world is untouched and learned memory is kept. Wipe memory also clears that memory.</p>
</div>

<div class="card">
  <h3>Wallet</h3>
  <p style="font-size:13px">Balance: <strong>{{.Wallet}}</strong></p>
  <form method="POST">
    <input type="hidden" name="action" value="wallet">
    <p class="rb-lbl">ISK to inject — a negative amount takes it back out</p>
    <input type="number" name="amount" step="0.01" placeholder="1000000" style="max-width:200px" required>
    <input type="text" name="reason" maxlength="100" placeholder="reason (optional — it goes in the journal)" style="max-width:340px">
    <button type="submit">Apply</button>
  </form>
  <p style="margin-top:8px;font-size:12px;color:#6b7280">This is real ISK, logged in the wallet journal as a GM donation. A withdrawal that would go below zero is refused.</p>
</div>

<div class="card">
  <h3>Grievances &amp; respect</h3>
  <p class="rb-scale">Grace: <b>{{.GraceTier}}</b>. What this pilot holds against, or owes, named pilots.</p>
  {{if .Grace}}
  <table class="rb-table">
    <tr><th>Pilot</th><th>Grudge</th><th>Regard</th><th>Band</th><th>Last</th><th>When</th></tr>
    {{range .Grace}}
    <tr><td>{{.Name}}</td><td>{{.Grudge}}</td><td>{{.Regard}}</td><td>{{.Band}}</td><td>{{.Kind}}</td><td>{{.Age}}</td></tr>
    {{end}}
  </table>
  {{else}}<p class="rb-scale" style="color:#6b7280">Nothing on the ledger.</p>{{end}}
</div>

<div class="card">
  <h3>Global Flags</h3>
  {{if .Loyalties}}<p class="rb-loyal">Through its employer, this pilot is {{.Loyalties}}.</p>{{end}}
  <form method="POST">
    <input type="hidden" name="action" value="profile">
    <div class="rb-grid">
      <div>
        <p class="rb-lbl">Home — medical clone station</p>
        <div class="rb-drop" style="max-width:420px">
          <input type="text" id="homeStationSearch" placeholder="Search stations by name..." autocomplete="off"
                 value="{{if .HomeStationName}}{{.HomeStationName}}{{end}}">
          <div class="list" id="homeStationList"></div>
        </div>
        <input type="hidden" name="homeStationID" id="homeStationID" value="{{.HomeStationID}}">
        <p class="rb-scale">Where it wakes up and parks spare hulls. Setting it moves the clone.</p>

        <p class="rb-lbl" style="margin-top:12px">Industry base — where a supplier manufactures</p>
        <div class="rb-drop" style="max-width:420px">
          <input type="text" id="industryBaseSearch" placeholder="Factory station (blank = pilot's own choice)..." autocomplete="off"
                 value="{{.IndustryStationName}}">
          <div class="list" id="industryBaseList"></div>
        </div>
        <input type="hidden" name="industryStationID" id="industryStationID" value="{{.Profile.IndustryStationID}}">
        <label class="rb-check"><input type="checkbox" onchange="if(this.checked){document.getElementById('industryStationID').value='0';document.getElementById('industryBaseSearch').value='';}"> clear — the pilot chooses</label>
        <p class="rb-scale">Blank = nearest usable factory.</p>
      </div>
      <div>
        <p class="rb-lbl">Where it will fly</p>
        <p class="rb-scale">Anywhere the gates go. Personality only decides how it weighs rough space. See <b>Space it will fly in</b> below.</p>

        <p class="rb-lbl" style="margin-top:12px">Full mining hold</p>
        <p class="rb-scale">Not a setting. The activity decides: stockpile, refine, or sell to the best bid.</p>

        {{if .CEOOf}}<p class="rb-scale" style="color:#eab308"><b>CEO</b> of this corporation —
        <a href="/rolebots/corp/{{.CEOOf}}">see what it has decided &rarr;</a></p>{{end}}
        <p class="rb-lbl" style="margin-top:12px">Factions — its employer's, not its own</p>
        {{if .EmployerFaction}}
        <p class="rb-scale">Flies for <a href="/rolebots/corp/{{.EmployerCorpID}}">{{.EmployerCorpName}}</a>,
        which answers to <b>{{.EmployerFaction}}</b>.</p>
        {{else}}
        <p class="rb-scale">Flies for <a href="/rolebots/corp/{{.EmployerCorpID}}">{{.EmployerCorpName}}</a>,
        a player corporation. No faction standings unless the corp or its alliance sets some.</p>
        {{end}}
        <p class="rb-scale">Follows the corporation's standings. See <a href="/rolebots/corps/diplomacy">Diplomacy</a>.</p>
      </div>
    </div>

    <p style="margin-top:10px"><button type="submit">Save global flags</button></p>
  </form>
</div>

<div class="card">
  <h3 style="margin-top:0">Play hours</h3>
  <p style="margin:0;font-size:13px">{{.TZHours}} server time &middot; {{.TZToday}}
    &middot; <a href="/rolebots/timezones">Timezones</a></p>
</div>

<div class="card">
  <h3>Personality</h3>
  <p style="font-size:12px;color:#9ca3af;max-width:860px">
    Centre is stock behaviour. Peaceful traits sit <span style="color:#86efac">left</span>, hostile ones <span style="color:#fca5a5">right</span>.
  </p>
  <form method="POST">
    <input type="hidden" name="action" value="traits">
    <div class="rb-tr-grid">
      {{range .Traits}}
      <div class="rb-tr">
        <div class="rb-tr-head">
          <strong>{{.Label}}</strong>
          <span class="rb-tr-what">{{.What}}</span>
          <output class="rb-tr-val{{if .Value}} set{{end}}">{{.Value}}</output>
        </div>
        <input type="range" name="trait_{{.Key}}" min="-100" max="100" step="5"
               {{if .Flip}}style="direction:rtl"{{end}}
               value="{{.Value}}" list="rb-tr-ticks" oninput="rbTraitEcho(this)">
        <div class="rb-tr-poles">
          <span class="lo">&#9666; {{.Low}}</span>
          <span class="mid">0 = stock</span>
          <span class="hi">{{.High}} &#9656;</span>
        </div>
        <div class="rb-tr-desc">
          <span>{{.LowDesc}}</span>
          <span>{{.HighDesc}}</span>
        </div>
      </div>
      {{end}}
    </div>
    <datalist id="rb-tr-ticks"><option value="0"></option></datalist>
    <p style="margin-top:10px"><button type="submit">Save personality</button></p>
  </form>
</div>

<div class="card">
  <h3>Ships &amp; Fits</h3>
  <p><span style="font-size:12px;color:#6b7280">What this pilot would fly today. It builds its own fits.</span></p>
  <p><a href="/rolebot/{{.Bot.CharacterID}}/skills">Skill training</a> <span style="font-size:12px;color:#6b7280">— {{if .Profile.AutoSkills}}it plans its own training queue.{{else}}auto-training is off for this pilot.{{end}}</span></p>
  <p><a href="/rolebot/{{.Bot.CharacterID}}/factory">Factory &amp; shelf</a> <span style="font-size:12px;color:#6b7280">— where it manufactures, its stock lines and build book.</span></p>

  <table>
    <tr><th></th><th>Role</th><th>Would fly</th><th>Score</th><th>Skills into it</th><th>Est. cost</th></tr>
    {{range .RoleChoices}}
    <tr>
      <td style="width:36px">{{if .Green}}<img src="/typeicon/{{.HullTypeID}}.png" width="30" height="30" style="vertical-align:middle;border-radius:3px;background:#170a0a" onerror="this.style.display='none'">{{end}}</td>
      <td>{{.Label}}</td>
      <td>{{if .Green}}{{.FitName}}{{if .SameRace}} <span style="font-size:11px;color:#22c55e" title="own race's hull">◆</span>{{end}}{{if .Dropped}} <span style="font-size:11px;color:#f59e0b">− {{.Dropped}} line(s) it cannot run</span>{{end}}{{if .Style}} <span style="font-size:11px;color:#60a5fa" title="combat style: the pilot's intent, then what the guns came out as">{{.Style}}</span>{{end}}{{else}}<span style="color:#6b7280">nothing it can fly</span>{{end}}</td>
      <td>{{if .Green}}{{printf "%.0f" .Score}}{{else}}—{{end}}</td>
      <td>{{if .Green}}{{printf "%.0f" .AffinityPct}}%{{else}}—{{end}}</td>
      <td>{{if .Cost}}{{.Cost}}{{else}}—{{end}}</td>
    </tr>
    {{end}}
  </table>
  <p style="font-size:12px;color:#6b7280">Best fit per role for this pilot's skills. Lines it cannot run are left off.</p>

  {{if .HeldShips}}
  <table>
    <tr><th></th><th>Hull it owns</th><th>Good at (all-V)</th><th>Est. cost</th><th>Ship</th></tr>
    {{range .HeldShips}}
    <tr>
      <td style="width:36px"><img src="/typeicon/{{.HullTypeID}}.png" width="30" height="30" style="vertical-align:middle;border-radius:3px;background:#170a0a" onerror="this.style.display='none'"></td>
      <td>{{.Name}}</td>
      <td>{{range .GoodAt}}<span style="display:inline-block;margin-right:8px;{{if not .Able}}color:#4b5563{{end}}">{{.Label}} {{if .Able}}<strong>{{printf "%.0f" .Power}}</strong>{{else}}—{{end}}</span>{{end}}</td>
      <td>{{.Cost}}</td>
      <td>{{.ShipItemID}}</td></tr>
    {{end}}
  </table>
  {{else}}<p style="font-size:12px;color:#6b7280">Owns no rolebot-built hulls yet.</p>{{end}}
</div>

<!-- ── Implants ─────────────────────────────────────────────────────────── -->
<div class="card">
  <h3>Implants</h3>
  <p><span style="font-size:12px;color:#6b7280">Implants this pilot has plugged in, and what it would buy or keep.{{if not .Implants.On}} <strong>Switched off</strong> (rolebots.implants.enabled).{{end}}</span></p>
  {{if .Implants.Note}}<p style="color:#f59e0b">{{.Implants.Note}}</p>{{end}}
  {{if .Implants.Plugged}}
  <table>
    <tr><th>Slot</th><th>Implant</th><th>Does</th><th>Value</th></tr>
    {{range .Implants.Plugged}}<tr><td>{{.Slot}}</td><td>{{.Name}}</td><td>{{.What}}</td><td>{{.Price}}</td></tr>{{end}}
  </table>
  {{if .Implants.Sets}}<p style="font-size:12px">Sets: {{range .Implants.Sets}}<span style="margin-right:12px">{{.}}</span>{{end}}</p>{{end}}
  {{else}}<p style="font-size:12px;color:#6b7280">Nothing plugged in.</p>{{end}}
  <p class="rb-lbl" style="margin-top:10px">Would buy or keep</p>
  {{if .Implants.Wants}}
  <table>
    <tr><th>Slot</th><th>Implant</th><th>Does</th><th>Catalogue</th><th>Why</th></tr>
    {{range .Implants.Wants}}<tr><td>{{.Slot}}</td><td>{{.Name}}</td><td>{{.What}}</td><td>{{.Price}}</td><td style="font-size:12px;color:#9ca3af">{{.Why}}</td></tr>{{end}}
  </table>
  {{else}}<p style="font-size:12px;color:#6b7280">Nothing in the catalogue would gain it anything.</p>{{end}}
  <p style="font-size:12px;color:#6b7280">Budget for one implant: {{.Implants.Budget}}{{if .Implants.Deaths}} — divided by {{.Implants.Deaths}} pod death(s) in the last 14 days{{end}}.</p>
</div>

<!-- ── Inclinations — derived, read-only ────────────────────────────────── -->
<div class="card">
  <h3>Inclinations</h3>
  <p style="font-size:12px;color:#6b7280">
    What this pilot is drawn to, derived from Personality, employer and the ships it can fly. Read-only.
  </p>
  {{if .InclNarrative}}<p class="rb-loyal">{{.InclNarrative}}</p>{{end}}
  <table class="rb-incl" style="max-width:780px">
    <tr><th>Work</th><th colspan="2">Pull</th><th>Would fly</th><th>Lately</th></tr>
    {{range .Inclinations}}
    <tr>
      <td style="width:110px">{{if .Sub}}<span class="muted" style="padding-left:16px">&#9492; {{.Label}}</span>{{else}}<strong>{{.Label}}</strong>{{end}}</td>
      <td style="width:130px"><span class="bar"><i {{if .Hot}}class="hot"{{end}} style="width:{{.BarPct}}%"></i></span></td>
      <td class="muted" style="width:130px">{{.DriveWord}}</td>
      <td>{{if .ShipName}}{{.ShipName}}{{else}}<span class="muted">{{.Note}}</span>{{end}}</td>
      <td class="muted">{{.Freshness}}</td>
    </tr>
    {{end}}
  </table>
  <p style="font-size:11px;color:#6b7280;margin-top:6px">Pull is raw personality. To change it, move the Personality dials or the employer.</p>

  <p class="rb-lbl" style="margin-top:14px">Space it will fly in</p>
  <table class="rb-incl" style="max-width:780px">
    <tr><th>Space</th><th colspan="2">Willing to enter</th></tr>
    {{range .SpaceBands}}
    <tr>
      <td style="width:110px"><strong>{{.Label}}</strong></td>
      <td style="width:130px"><span class="bar"><i style="width:{{.BarPct}}%"></i></span></td>
      <td class="muted">{{.Note}}</td>
    </tr>
    {{end}}
  </table>
  <p style="font-size:11px;color:#6b7280;margin-top:6px">{{.SpaceNarrative}}</p>

  <p class="rb-lbl" style="margin-top:14px">Where its economy looks</p>
  {{if .ScopeBand}}
  <p style="font-size:12px">
    Scoped to its <strong>{{.ScopeBand}}</strong>: {{.ScopeRegions}}.
  </p>
  <p style="font-size:11px;color:#6b7280;margin-top:6px">
    Where it sells, mines, builds and prices competition. Follows the home station. Not a travel limit.
  </p>
  {{else}}
  <p style="font-size:12px;color:#6b7280">
    Unscoped. No home region yet. It gets one once it has a medical clone station.
  </p>
  {{end}}
</div>

<script>
// ── station typeahead (home + industry base) ──────────────────────────────
function wireStationPicker(inputID, listID, hiddenID) {
  var input = document.getElementById(inputID);
  var list = document.getElementById(listID);
  var hidden = document.getElementById(hiddenID);
  if (!input || !list || !hidden) return;
  var t = null;
  input.addEventListener('input', function () {
    clearTimeout(t);
    var q = input.value.trim();
    if (q.length < 2) { list.style.display = 'none'; return; }
    t = setTimeout(function () {
      fetch('/agents/stations/api?q=' + encodeURIComponent(q))
        .then(function (r) { return r.json(); })
        .then(function (rows) {
          list.innerHTML = '';
          (rows || []).slice(0, 15).forEach(function (s) {
            var d = document.createElement('div');
            d.textContent = s.stationName + (s.systemName ? ' — ' + s.systemName : '');
            d.addEventListener('mousedown', function () {
              hidden.value = s.stationID;
              input.value = s.stationName;
              list.style.display = 'none';
            });
            list.appendChild(d);
          });
          list.style.display = rows && rows.length ? 'block' : 'none';
        }).catch(function () { list.style.display = 'none'; });
    }, 250);
  });
  input.addEventListener('blur', function () { setTimeout(function () { list.style.display = 'none'; }, 200); });
}
wireStationPicker('homeStationSearch', 'homeStationList', 'homeStationID');
wireStationPicker('industryBaseSearch', 'industryBaseList', 'industryStationID');

// Echo a personality slider's value into its card header as it moves,
// lighting the number up whenever it is off stock.
function rbTraitEcho(slider) {
  var out = slider.closest('.rb-tr').querySelector('output');
  if (!out) return;
  out.textContent = slider.value;
  out.classList.toggle('set', slider.value !== '0');
  rbTraitPaint(slider);
}

// Two-sided meter: from the centre out to the thumb, green on the left half and red on the
// right, over a faint tint of both. A flipped axis (direction:rtl) puts +100 on the left, so
// the thumb's position is mirrored; the colours always follow the side, never the value.
function rbTraitPaint(slider) {
  var v = parseFloat(slider.value) || 0;
  var p = (v + 100) / 2;
  if (getComputedStyle(slider).direction === 'rtl') p = 100 - p;
  var G = '#22c55e', R = '#ef4444', gt = 'rgba(34,197,94,.20)', rt = 'rgba(239,68,68,.20)';
  var fill;
  if (p < 50) {
    fill = 'linear-gradient(to right,' + gt + ' 0,' + gt + ' ' + p + '%,' + G + ' ' + p + '%,' + G + ' 50%,' + rt + ' 50%)';
  } else if (p > 50) {
    fill = 'linear-gradient(to right,' + gt + ' 50%,' + R + ' 50%,' + R + ' ' + p + '%,' + rt + ' ' + p + '%)';
  } else {
    fill = 'linear-gradient(to right,' + gt + ' 50%,' + rt + ' 50%)';
  }
  slider.style.setProperty('--rb-fill', fill);
}
document.querySelectorAll('.rb-tr input[type=range]').forEach(rbTraitPaint);
</script>
{{end}}`

type namedID struct {
	ID   int64
	Name string
}

func (m *Mod) handleProfile(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	charID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if len(parts) >= 3 && parts[2] == "fit" {
		// The fitting tool is global, so this redirects to it.
		http.Redirect(w, r, "/rolebots/fits", http.StatusSeeOther)
		return
	}
	if len(parts) >= 3 && parts[2] == "skills" {
		m.handleSkillsPage(w, r, charID)
		return
	}
	if len(parts) >= 3 && parts[2] == "factory" {
		m.handleFactoryPage(w, r, charID)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		m.handleProfilePost(w, r, ctx, charID)
		return
	}

	m.renderProfilePage(w, r, ctx, charID, false,
		r.URL.Query().Get("msg"), r.URL.Query().Get("err"))
}

// renderProfilePage builds and renders the profile page. With resubmit set, the posted
// values are laid back over the loaded profile so a rejected form comes back as typed,
// with the error on top. Success redirects (POST/redirect/GET) so a refresh cannot re-run the save.
func (m *Mod) renderProfilePage(w http.ResponseWriter, r *http.Request, ctx context.Context,
	charID int64, resubmit bool, msg, errMsg string) {

	rb, err := getRolebot(ctx, m.db, charID)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	if rb == nil {
		espError(w, "rolebots", fmt.Sprintf("character %d is not on the rolebot roster", charID))
		return
	}
	prof, err := m.getProfile(ctx, charID)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	// Home is the character's clone station (the row homeStationFor reads), not a profile field.
	homeStationID := m.homeStationFor(ctx, charID)
	traits, err := getTraits(ctx, m.db, charID)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	if resubmit {
		// On a rejected save the home field shows the operator's typed value.
		if v, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("homeStationID")), 10, 64); err == nil && v != 0 {
			homeStationID = v
		}
		// Overwriting the profile here carries the submission into every part of the form
		// derived from it: region pickers, faction inputs, pilot rows, loyalty narrative.
		applyProfileForm(prof, r)
		// Only one form posts at a time. A rejected personality save carries trait_* keys;
		// a Global Flags save does not, and then the stored traits stay.
		if hasFormPrefix(r, "trait_") {
			traits = formTraits(r)
		}
	}
	status, err := charStatus(ctx, m.db, charID)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	stationName := func(id int64) string {
		return m.stationNameCached(ctx, id)
	}

	// The hulls this pilot actually owns, and what it would fly for each
	// role if it had to choose right now. Ownership comes from the ship
	// rows; the choice is the same scoring the sim does.
	type heldShip struct {
		FitID, HullTypeID int64
		Name              string
		ShipItemID        int64
		Cost              string
		GoodAt            []jobPowerRow
	}
	var heldShips []heldShip
	// The hull comes off the ship entity; there is no fits table (fitgen.go).
	if arows, err := m.db.QueryContext(ctx, `
		SELECT s.fitID, e.typeID, t.typeName, s.shipItemID
		FROM rolebot_fit_ships s
		JOIN entity e ON e.itemID = s.shipItemID AND e.ownerID = s.characterID
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE s.characterID = ? ORDER BY t.typeName ASC`, charID); err == nil {
		for arows.Next() {
			var hs heldShip
			if arows.Scan(&hs.FitID, &hs.HullTypeID, &hs.Name, &hs.ShipItemID) == nil {
				heldShips = append(heldShips, hs)
			}
		}
		arows.Close()
		// Pricing and scoring run their own queries, so they run after the ship cursor is closed.
		for i := range heldShips {
			t := m.ownedShipTemplate(ctx, charID, heldShips[i].ShipItemID, heldShips[i].HullTypeID)
			if t == nil {
				continue
			}
			if cost, err := m.fitCost(ctx, nil, t); err == nil {
				heldShips[i].Cost = fitCostText(cost)
			}
			heldShips[i].GoodAt = m.jobPowers(ctx, t)
		}
	}

	// Best fit per role for this pilot, scored.
	type roleChoice struct {
		Role, Label, FitName string
		FitID, HullTypeID    int64
		Score                float64
		AffinityPct          float64
		SameRace             bool
		Cost                 string
		Green                bool
		Dropped              int // lines this pilot would leave off
		// Style is the fit's intent and what its guns came out as (fitstyle.go). Display only.
		Style string
	}
	var roleChoices []roleChoice
	if skills, err := m.charSkillsFor(ctx, charID); err == nil {
		who := m.charPilotRace(ctx, charID)
		for _, rd := range jobCatalog {
			pool, poolErr := m.candidatePool(ctx, charID, rd.Key)
			if poolErr != nil {
				continue
			}
			rc := roleChoice{Role: rd.Key, Label: rd.Label}
			for _, stored := range pool {
				// Judge this pilot's version of each fit at this job as the sim would: a partial fit
				// it can run beats a whole fit it cannot, and a ship that cannot do the work scores zero.
				eff, err := m.effectiveFitWithSkills(ctx, charID, rd.Key, stored, skills)
				if err != nil || eff == nil {
					continue
				}
				j, err := m.judgeFit(ctx, eff, skills, who)
				if err != nil || j.Unusable || j.Score <= rc.Score {
					continue
				}
				rc.FitID, rc.HullTypeID, rc.FitName = eff.FitID, eff.Fit.HullTypeID, eff.Name
				rc.Score, rc.AffinityPct, rc.SameRace = j.Score, j.Affinity*100, j.SameRace
				rc.Dropped = len(eff.Dropped)
				rc.Style = fitStyleLabel(eff.Style, eff.Lean)
				rc.Green = true
				if cost, err := m.fitCost(ctx, nil, eff); err == nil {
					rc.Cost = fitCostText(cost)
				}
			}
			roleChoices = append(roleChoices, rc)
		}
	}

	simActivity, simPhase, simUpdated, simError := "", "", "", ""
	simHalted := false
	{
		var act, ph string
		var lastErr sql.NullString
		var upd, halted int64
		err := m.db.QueryRowContext(ctx, `
			SELECT activity, phase, updatedAt, halted, lastError
			FROM rolebot_state WHERE characterID = ?`, charID,
		).Scan(&act, &ph, &upd, &halted, &lastErr)
		if err == nil {
			simActivity, simPhase = act, ph
			simHalted, simError = halted != 0, lastErr.String
			if upd > 0 {
				simUpdated = time.Unix(upd, 0).UTC().Format("15:04:05 UTC")
			}
		}
	}

	shipByJob := map[string]string{}
	for _, rc := range roleChoices {
		if rc.Green {
			shipByJob[rc.Role] = rc.FitName
		}
	}
	inclinations, inclNarrative := m.inclinationRows(ctx, charID, prof, traits, shipByJob)
	tune := resolveTuning(traits)
	spaceBands, spaceNarrative := spaceBandRows(tune)
	scopeBandName, scopeRegionNames := m.scopeSummary(ctx, prof, tune, homeStationID)
	employerCorpID, employerCorpName, employerFactionName := m.employerIdentity(ctx, charID)

	espRender(w, "rolebots_profile.html", map[string]any{
		"Page":    "rolebots",
		"Bot":     rb,
		"Profile": prof,
		"Status":  status,
		// Whose politics these are (outlook.go): the numbers are the employer's and are read
		// from it, so the page names the source and offers no input.
		"EmployerCorpID":   employerCorpID,
		"CEOOf":            ceoOfCorp(rb.CharacterID),
		"EmployerCorpName": employerCorpName,
		"EmployerFaction":  employerFactionName,
		"Loyalties":        m.standingsNarrative(ctx, prof),
		"Grace":            m.graceRows(ctx, rb.CharacterID),
		"GraceTier":        graceTierLabel(resolveTuning(traits)),
		"Traits":           traitRows(traits),
		"TZHours":          tzHoursText(tzSlotOf(rb.CharacterID)),
		"TZToday":          tzTodayText(rb.CharacterID),

		"SimActivity":     simActivity,
		"SimPhase":        simPhase,
		"SimUpdated":      simUpdated,
		"SimHalted":       simHalted,
		"SimError":        simError,
		"HomeStationID":   homeStationID,
		"HomeStationName": stationName(homeStationID),
		// Residency (residency.go). Empty band means the switch is off, and
		// the page says that rather than drawing a scope nothing enforces.
		"ScopeBand":    scopeBandName,
		"ScopeRegions": scopeRegionNames,
		"HeldShips":    heldShips,
		"RoleChoices":  roleChoices,
		"Implants":     m.implantProfileView(ctx, charID, resolveTuning(traits)),
		"Wallet":       iskString(status.Balance),

		"IndustryStationName": stationName(prof.IndustryStationID),
		"Inclinations":        inclinations,
		"InclNarrative":       inclNarrative,
		"SpaceBands":          spaceBands,
		"SpaceNarrative":      spaceNarrative,

		"Msg": msg,
		"Err": errMsg,
	})
}

func (m *Mod) handleProfilePost(w http.ResponseWriter, r *http.Request, ctx context.Context, charID int64) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	self := fmt.Sprintf("/rolebot/%d", charID)
	switch r.FormValue("action") {
	case "enabled":
		on := r.FormValue("state") == "on"
		// Refuse possession while a player is logged in: the sim would rewrite the location
		// under the live session.
		if on && m.sessions != nil {
			if sess := m.sessions.FindByCharID(charID); sess != nil {
				redirect(w, r, self, "", fmt.Sprintf(
					"a player is currently logged in on this character (%s) — log them out first", sess.CharName))
				return
			}
		}
		if err := setRolebotEnabled(ctx, m.db, charID, on); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		m.syncPresence()
		m.syncCEOSeat(ctx, charID)
		if on {
			redirect(w, r, self, "RoleBot has taken possession of this character", "")
		} else {
			redirect(w, r, self, "character released — players may log in on it again", "")
		}
	case "reboot":
		if err := m.rebootAndLog(ctx, charID); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		redirect(w, r, self, "sim rebooted — this bot re-plans from scratch on the next pass", "")
	case "wallet":
		m.injectISK(w, r, ctx, charID, self)
	case "profile":
		// Validation failures re-render rather than redirect, so the region grid is not lost.
		reject := func(msg string) { m.renderProfilePage(w, r, ctx, charID, true, "", msg) }
		// Home is the clone station, not a profile column; it is written to the character
		// after the profile save succeeds.
		homeStationID, _ := strconv.ParseInt(r.FormValue("homeStationID"), 10, 64)
		if homeStationID != 0 {
			var one int64
			if err := m.db.QueryRowContext(ctx,
				`SELECT 1 FROM staStations WHERE stationID = ?`, homeStationID).Scan(&one); err != nil {
				reject(fmt.Sprintf("station %d does not exist", homeStationID))
				return
			}
		}
		industryStationID, _ := strconv.ParseInt(r.FormValue("industryStationID"), 10, 64)
		if industryStationID != 0 {
			// The pin is only saved if the station can build something.
			var one int64
			if err := m.db.QueryRowContext(ctx, `
				SELECT 1 FROM ramassemblylines
				WHERE containerID = ? AND activityID = 1 LIMIT 1`, industryStationID).Scan(&one); err != nil {
				reject(fmt.Sprintf("station %d has no manufacturing assembly line", industryStationID))
				return
			}
		}
		// No standings: a pilot's outlook is its employer's, derived on demand and stored nowhere.
		p := &Profile{
			CharacterID:       charID,
			IndustryStationID: industryStationID,
		}
		if err := saveProfile(ctx, m.db, p); err != nil {
			// Nothing was written, so hand the edits back instead of redirecting.
			reject(err.Error())
			return
		}
		// The picked home is the clone. A blank one is left alone: an empty typeahead is more
		// likely a lost form value than a request to unhouse the pilot.
		if homeStationID != 0 {
			// Go through setHomeStationByID (clone.go), not evedb: a home move also invalidates
			// the residency region and settles the pilot out of the academy.
			m.setHomeStationByID(ctx, charID, homeStationID)
			// Clone upkeep looks again now instead of waiting out its interval.
			forgetCloneCheck(charID)
		}
		// The move-out step (moveout.go) looks again instead of keeping its earlier verdict.
		forgetMoveOut(charID)
		// The corp-home step (corphome.go) looks again for the same reason.
		forgetCorpHome(charID)
		redirect(w, r, self, "global flags saved", "")
	case "wipe":
		if err := wipeBot(ctx, m.db, charID); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		m.logBot(charID, logInfo, "sim", "memory wiped by an operator — this pilot has forgotten "+
			"the belts it worked, its appetite for each kind of work and what it was hauling")
		redirect(w, r, self, "memory wiped — this bot re-plans from scratch with no history", "")
	case "traits":
		traits := formTraits(r)
		if err := saveTraits(ctx, m.db, charID, traits); err != nil {
			m.renderProfilePage(w, r, ctx, charID, true, "", err.Error())
			return
		}
		// Says what changed rather than "saved", since the dials are not obvious from outside.
		redirect(w, r, self, traitsSummary(traits), "")
	default:
		redirect(w, r, self, "", "unknown action")
	}
}

// spaceBandRow is one line of the "Space it will fly in" readout: a
// security band and how much of it this pilot is willing to enter.
type spaceBandRow struct {
	Label  string
	BarPct int
	Note   string
}

// spaceBandRows turns the pilot's comfort line (Tuning.PersonalSecFloor, the number
// rulesForBot prices routes with) into three security bands. Every band is open; the
// bars show reluctance, not a fence.
func spaceBandRows(tn Tuning) ([]spaceBandRow, string) {
	floor := tn.PersonalSecFloor
	if floor < -1 {
		floor = -1
	}
	if floor > 1 {
		floor = 1
	}

	band := func(label string, lo, hi float64) spaceBandRow {
		row := spaceBandRow{Label: label}
		bottom := lo
		if floor > bottom {
			bottom = floor
		}
		frac := (hi - bottom) / (hi - lo)
		switch {
		case frac <= 0:
			row.BarPct = 5
			row.Note = "avoids — goes only when the work is worth it"
		case frac >= 0.999:
			row.BarPct = 100
			row.Note = "goes anywhere here"
		default:
			row.BarPct = int(frac * 100)
			if row.BarPct < 5 {
				row.BarPct = 5
			}
			row.Note = fmt.Sprintf("happy down to %.1f, reluctant below", bottom)
		}
		return row
	}

	rows := []spaceBandRow{
		band("High-sec", 0.45, 1.0),
		band("Low-sec", 0.0, 0.45),
		band("Null-sec", -1.0, 0.0),
	}

	narrative := fmt.Sprintf("Comfort line: %.1f security, set by its Caution and Lawfulness. "+
		"Rougher space costs it about %.0f extra jumps of reluctance per 1.0 of security. No band is closed. "+
		"Mining ignores this line.",
		floor, secAversionJumps)
	return rows, narrative
}

// activityLabelOf is the catalogue label for a slug, "" if it has none.
func activityLabelOf(slug string) string {
	if def, ok := activityDefFor(slug); ok {
		return def.Label
	}
	return ""
}

// inclRow is one line of the Inclinations card: an activity as this pilot's
// identity prices it, read-only.
type inclRow struct {
	Label     string
	BarPct    int
	Hot       bool
	DriveWord string
	ShipName  string
	Note      string
	Freshness string
	// Sub marks a row drawn as a sub-activity of the one above it (combat's
	// PvE and PvP).
	Sub bool
}

// combatSubDrives splits the combat pull into rats (PvE) and other pilots (PvP), on the
// same 0..2.5 scale as every row. PvE is the combat disposition itself; PvP adds the
// taste for fights with strangers plus a share of lawlessness. Violence under 0.1
// (the pilotPvPLean gate) leaves no PvP pull.
func combatSubDrives(tn Tuning) (pve, pvp float64) {
	pve = tn.ViolenceAppetite
	if tn.ViolenceAppetite < 0.1 {
		return pve, 0
	}
	nerve := tn.NeutralAggression / neutralAggressionDefault
	if nerve > 1 {
		nerve = 1
	}
	if nerve < 0 {
		nerve = 0
	}
	pvp = tn.ViolenceAppetite*(0.5+0.5*nerve) + 0.35*tn.PiracyAppetite
	return pve, pvp
}

// industrySubDrives are the pulls of the industry umbrella's callings, on the
// same scale as every other row (industrytrip.go prices the trips with these).
type industryDrives struct {
	// Mining is rock or ice, one drive (miningtrip.go).
	Mining, Build, Research float64
}

func industrySubDrives(tn Tuning) industryDrives {
	d := industryDrives{Build: tn.SupplyDrive * tn.IndustryAppetite, Research: tn.ResearchAppetite}
	if def, ok := activityDefFor(ActMining); ok {
		d.Mining = def.Disposition(tn)
	}
	return d
}

// inclinationDrive is the row's headline pull: the entry's disposition, except
// for the industry umbrella, whose disposition is manufacturing's alone (the
// skill planner and the arms read it that way) and whose row must show the
// strongest of its callings.
func inclinationDrive(def ActivityDef, tn Tuning) float64 {
	if def.Slug != ActIndustry {
		return def.Disposition(tn)
	}
	d := industrySubDrives(tn)
	best := d.Build
	for _, v := range []float64{d.Mining, d.Research} {
		if v > best {
			best = v
		}
	}
	return best
}

// inclinationRows derives the Inclinations card from the same numbers the
// sim reads: dispositions from the pilot's resolved Tuning, capability
// from the fit pool (via the already-scored role choices), and freshness
// from the appetite tally in its state row.
func (m *Mod) inclinationRows(ctx context.Context, charID int64, prof *Profile,
	traits map[string]int, shipByJob map[string]string) ([]inclRow, string) {

	tn := resolveTuning(traits)
	var worked map[string]float64
	if st, err := loadState(ctx, m.db, charID); err == nil {
		worked = st.Data.Worked
	}

	driveWord := func(v float64) string {
		switch {
		case v >= 1.8:
			return "hungers for it"
		case v >= 1.15:
			return "drawn to it"
		case v > 0.85:
			return "takes it as it comes"
		case v >= 0.35:
			return "leans away from it"
		default:
			return "avoids it"
		}
	}

	bar := func(drive float64) int {
		// Bar: 2.5 is roughly the ceiling a maxed trait produces.
		pct := int(drive / 2.5 * 100)
		if pct > 100 {
			pct = 100
		}
		if pct < 3 {
			pct = 3
		}
		return pct
	}
	freshTotal := func(total float64) string {
		if f := satiationFactor(total, tn.SatiationHalfLifeSec); f < 0.98 {
			return fmt.Sprintf("worn thin \u2014 %.0f%% fresh", f*100)
		}
		return "fresh"
	}
	fresh := func(slug string) string { return freshTotal(worked[slug]) }
	var rows []inclRow
	for _, def := range activityCatalog {
		// Piracy is a trip of the combat patrol now, drawn under it as PvP;
		// mining (rock or ice) is a trip of industry.
		if def.Tactic {
			continue
		}
		drive := inclinationDrive(def, tn)
		row := inclRow{
			Label:     def.Label,
			DriveWord: driveWord(drive),
			ShipName:  shipByJob[def.Job],
			Hot:       drive >= 1.15,
			BarPct:    bar(drive),
			Freshness: freshTotal(def.memberWorked(worked)),
		}
		if row.ShipName == "" {
			row.Note = "no " + jobLabel(def.Job) + " fit it can fly \u2014 not an option"
		}
		rows = append(rows, row)
		if def.Slug == ActIndustry {
			// The callings industry can be, listed under it.
			sub := industrySubDrives(tn)
			for _, s := range []struct {
				label    string
				drive    float64
				job      string
				slug     string
				research bool
			}{
				{"mining \u2014 rock or ice", sub.Mining, JobMining, ActMining, false},
				{"manufacturing", sub.Build, JobTrade, ActIndustry, false},
				{"research \u2014 the laboratory", sub.Research, JobTrade, ActIndustry, true},
			} {
				r := inclRow{
					Label: s.label, Sub: true, DriveWord: driveWord(s.drive),
					ShipName: shipByJob[s.job], Hot: s.drive >= 1.15, BarPct: bar(s.drive),
					Freshness: fresh(s.slug),
				}
				if s.research && !researchCareer(tn) {
					r.DriveWord = "not its calling"
				}
				if r.ShipName == "" {
					r.Note = "no " + jobLabel(s.job) + " fit it can fly \u2014 not an option"
				}
				rows = append(rows, r)
			}
			continue
		}
		if def.Slug != ActCombat {
			continue
		}
		// The two things combat can be, listed under it.
		pve, pvp := combatSubDrives(tn)
		pveRow := inclRow{
			Label: "PvE \u2014 rats and belts", Sub: true, DriveWord: driveWord(pve),
			ShipName: shipByJob[JobCombat], Hot: pve >= 1.15, BarPct: bar(pve),
			Freshness: fresh(ActCombat),
		}
		if pveRow.ShipName == "" {
			pveRow.Note = "no " + jobLabel(JobCombat) + " fit it can fly \u2014 not an option"
		}
		pvpRow := inclRow{
			Label: "PvP \u2014 pilots, gates and raids", Sub: true, DriveWord: driveWord(pvp),
			ShipName: shipByJob[JobPvP], Hot: pvp >= 1.15, BarPct: bar(pvp),
			Freshness: fresh(ActPiracy),
		}
		if pvp <= 0 {
			pvpRow.DriveWord = "will not"
			pvpRow.BarPct = 0
		}
		if pvpRow.ShipName == "" {
			pvpRow.Note = "no " + jobLabel(JobPvP) + " fit it can fly \u2014 not an option"
		}
		rows = append(rows, pveRow, pvpRow)
	}

	// The narrative: strongest pull plus the deepest belt grudge.
	best, bestDrive := "", 0.0
	for _, def := range activityCatalog {
		if def.Tactic {
			continue
		}
		if d := inclinationDrive(def, tn); d > bestDrive {
			best, bestDrive = def.Label, d
		}
	}
	narrative := ""
	if bestDrive >= 1.15 {
		narrative = "Left to itself, this pilot gravitates to " + best + "."
		if best == activityLabelOf(ActCombat) {
			if pve, pvp := combatSubDrives(tn); pvp > 0 && pvp >= pve {
				narrative += " Its fights are mostly with other pilots."
			} else if pvp <= 0 {
				narrative += " Its fights are with the rats \u2014 it will not start one with a pilot."
			}
		}
	} else {
		narrative = "No strong pull either way — this pilot takes whatever work makes sense."
	}
	worstName, worst := "", 0.0
	for _, id := range m.pirateFactionIDs(ctx) {
		if v := prof.StandingTo(id); v < worst {
			worst, worstName = v, m.standingName(ctx, Standing{ToID: id, Kind: standingFaction})
		}
	}
	if worst <= StandingDisliked && worstName != "" {
		narrative += fmt.Sprintf(" It carries a grudge against %s (%.1f) and hunts them first.", worstName, worst)
	}
	return rows, narrative
}

// traitRow is one personality dial as the profile page renders it. The
// catalogue in tuning.go drives the form, so adding a trait there makes it
// appear here with no template change.
type traitRow struct {
	Key, Label, Low, High   string
	What, LowDesc, HighDesc string
	Value                   int
	Flip                    bool
}

// traitRows builds the trait rows. A flipped axis (TraitDef.Flip) has its ends swapped
// here and its slider reversed with direction:rtl, so the Low pole is still drawn on the
// left. The stored and posted value never changes.
func traitRows(traits map[string]int) []traitRow {
	out := make([]traitRow, 0, len(traitCatalog))
	for _, def := range traitCatalog {
		row := traitRow{
			Key: def.Key, Label: def.Label, Low: def.Low, High: def.High,
			What: def.What, LowDesc: def.LowDesc, HighDesc: def.HighDesc,
			Value: traits[def.Key], Flip: def.Flip,
		}
		if def.Flip {
			row.Low, row.High = def.High, def.Low
			row.LowDesc, row.HighDesc = def.HighDesc, def.LowDesc
		}
		out = append(out, row)
	}
	return out
}

// traitHighlights names the n traits a pilot leans on hardest, as the pole each leans
// to, for the cast card. It reads traitCatalog and ignores Flip, which only changes
// how an axis is drawn. Traits under this floor are dropped, even if the list ends empty.
const traitHighlightFloor = 25

func traitHighlights(traits map[string]int, n int) []string {
	type cand struct {
		mag   int
		label string
	}
	var cands []cand
	for _, def := range traitCatalog {
		v, ok := traits[def.Key]
		if !ok {
			continue
		}
		mag := v
		if mag < 0 {
			mag = -mag
		}
		if mag < traitHighlightFloor {
			continue
		}
		label := def.High
		if v < 0 {
			label = def.Low
		}
		if label == "" {
			continue
		}
		cands = append(cands, cand{mag: mag, label: label})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].mag > cands[j].mag })
	if len(cands) > n {
		cands = cands[:n]
	}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.label)
	}
	return out
}

// formTraits reads the personality sliders. Only keys in the catalogue are
// accepted, so a stale or hand-crafted form cannot write a trait the sim
// does not read.
func formTraits(r *http.Request) map[string]int {
	out := map[string]int{}
	for _, def := range traitCatalog {
		v, err := strconv.Atoi(strings.TrimSpace(r.FormValue("trait_" + def.Key)))
		if err != nil {
			continue
		}
		if v < -100 {
			v = -100
		}
		if v > 100 {
			v = 100
		}
		out[def.Key] = v
	}
	return out
}

// hasFormPrefix reports whether the submission carries any key with this prefix, which
// tells a shared re-render which form failed.
func hasFormPrefix(r *http.Request, prefix string) bool {
	for key := range r.Form {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// applyProfileForm lays a rejected Global Flags submission back over the loaded profile
// so the page shows what the operator typed. A field that cannot be parsed keeps its
// stored value.
func applyProfileForm(p *Profile, r *http.Request) {
	// The home station is not on the profile; the page carries it separately.
	if v, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("industryStationID")), 10, 64); err == nil {
		p.IndustryStationID = v
	}
}

// injectISK moves ISK into or out of a rostered pilot's wallet from its profile page
// (the panel's /giveisk). The write is one guarded UPDATE with a `balance + ? >= 0`
// predicate, so zero rows affected is the refusal and the sim cannot race it. The journal
// row is refType 10 with both owners set to the character, as cmdGiveISK writes it.
func (m *Mod) injectISK(w http.ResponseWriter, r *http.Request, ctx context.Context,
	charID int64, self string) {

	amount, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("amount")), 64)
	if err != nil || amount == 0 {
		redirect(w, r, self, "", "enter an amount of ISK — a negative amount takes it back out")
		return
	}
	res, err := m.db.ExecContext(ctx, `
		UPDATE character_ SET balance = balance + ?
		WHERE characterID = ? AND balance + ? >= 0`, amount, charID, amount)
	if err != nil {
		redirect(w, r, self, "", err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		bal, berr := evedb.GetCharBalance(ctx, m.db, charID)
		if berr != nil {
			redirect(w, r, self, "", berr.Error())
			return
		}
		redirect(w, r, self, "", fmt.Sprintf(
			"this pilot holds %s — taking %.2f ISK out would put the wallet below zero", iskString(bal), -amount))
		return
	}
	newBal, err := evedb.GetCharBalance(ctx, m.db, charID)
	if err != nil {
		redirect(w, r, self, "", err.Error())
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if len(reason) > 100 {
		reason = reason[:100]
	}
	if err := evedb.InsertJournalEntry(ctx, m.db, charID, 10,
		charID, charID, amount, newBal, reason); err != nil {
		rlogf("rolebots: wallet journal %d: %v", charID, err)
	}
	verb, note := "granted", ""
	if amount < 0 {
		verb = "withdrew"
	}
	if reason != "" {
		note = " — " + reason
	}
	m.logBot(charID, logInfo, "wallet", "operator %s %.2f ISK; the wallet now holds %.2f%s",
		verb, math.Abs(amount), newBal, note)
	redirect(w, r, self, fmt.Sprintf("%s %.2f ISK — the wallet now holds %s",
		verb, math.Abs(amount), iskString(newBal)), "")
}

// charStatusRow is the location and ship snapshot on the profile page.
type charStatusRow struct {
	CharacterID   int64
	CharacterName string
	CorpName      string
	Docked        bool
	StationName   string
	SystemName    string
	ShipTypeID    int64
	ShipTypeName  string
	Balance       float64
}

func charStatus(ctx context.Context, db *sql.DB, charID int64) (*charStatusRow, error) {
	st := &charStatusRow{}
	var stationID int64
	err := db.QueryRowContext(ctx, `
		SELECT c.characterID, c.characterName, COALESCE(corp.corporationName, ''),
		       c.stationID, COALESCE(sta.stationName, ''),
		       COALESCE(ms.solarSystemName, ''), COALESCE(t.typeID, 0), COALESCE(t.typeName, ''),
		       c.balance
		FROM character_ c
		LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		LEFT JOIN staStations sta ON sta.stationID = c.stationID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = c.solarSystemID
		LEFT JOIN entity ce ON ce.itemID = c.characterID
		LEFT JOIN entity ship ON ship.itemID = ce.locationID AND ship.ownerID = c.characterID
		LEFT JOIN invTypes t ON t.typeID = ship.typeID
		WHERE c.characterID = ?`, charID,
	).Scan(&st.CharacterID, &st.CharacterName, &st.CorpName,
		&stationID, &st.StationName, &st.SystemName, &st.ShipTypeID, &st.ShipTypeName,
		&st.Balance)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("character %d not found", charID)
	}
	if err != nil {
		return nil, fmt.Errorf("charStatus: %w", err)
	}
	st.Docked = stationID != 0
	return st, nil
}

func listNamed(ctx context.Context, db *sql.DB, query string) ([]namedID, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []namedID
	for rows.Next() {
		var n namedID
		if err := rows.Scan(&n.ID, &n.Name); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
