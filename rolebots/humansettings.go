package rolebots

// The human-member dials: what a player who joins a bot-run corporation is told,
// offered and promoted to (corphuman.go, corpnews.go, corpassign.go, corpmerit.go).
//
// Each setting is a const key, an atomic cache for the hot path, a boot loader that is
// never fatal, and a setter that writes the setting before it updates the cache.
// The first four switches default off. Three only touch human members; the merit ladder
// also grants and strips real corp roles on every bot member.
// Who may refuse a human applicant has no switch: only the CEO's Tolerance may turn a
// player away (corpceo_hr.go).

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

const (
	// settingHumanPipeline: applicants are mailed their answer, unanswered
	// offers lapse, and a new member is welcomed.
	settingHumanPipeline = "rolebots.corp.humanPipeline"
	// settingCorpNews: corporations with a human member get bulletins in the
	// Corp mail tab.
	settingCorpNews = "rolebots.corp.news"
	// settingAssignments: human members are offered paid assignments.
	settingAssignments = "rolebots.corp.assignments"
	// settingMerit: every member of a bot-run corporation earns rank, and rank
	// carries roles, a title and a pay multiplier.
	settingMerit = "rolebots.corp.merit"
	// settingPostings: members of a player corporation live at their Base
	// (chrCorporationRoles.baseID) and a bot CEO posts them across its offices by arm
	// job (corpposting.go). Default on; off puts every member at headquarters.
	settingPostings = "rolebots.corp.postings"
	// settingHumanPostings: a bot CEO posts human members too, by mail with links to
	// choose another office (corpposting.go). Default on.
	settingHumanPostings = "rolebots.corp.humanPostings"

	// settingLabExpand: when R&D members are refused at a research base for want of a line,
	// the CEO grows the department into another lab office (corpexpand.go) and the R&D
	// Director splits the library across its bases (corpprintmove.go). Default on.
	settingLabExpand = "rolebots.corp.labExpand"
	// settingFactoryExpand: the same for Industry lines: build orders, Materials and the
	// buyback run at every Industry base (corpbuildbases.go). Default on.
	settingFactoryExpand = "rolebots.corp.factoryExpand"
	// settingPrintMoves: the corporation carries prints between its bases: originals by a
	// Director or the CEO, copies by any member posted where they sit (corpprintmove.go). Default on.
	settingPrintMoves = "rolebots.corp.printMoves"
	// settingBaseRoles: department job roles (Factory Manager, hangar views) are granted at
	// Base, so a member runs corporation jobs only at its posted office. Needs the core
	// setting corp.locationRoles. Default off.
	settingBaseRoles = "rolebots.corp.baseRoles"

	settingPrintMoveWage      = "rolebots.corp.printMoveWage"
	settingPrintMoveHoldHours = "rolebots.corp.printMoveHumanHoldH"

	settingHumanOfferHours   = "rolebots.corp.humanOfferHours"
	settingNewsMinHours      = "rolebots.corp.newsMinHours"
	settingAssignOpenMax     = "rolebots.corp.assignOpenMax"
	settingAssignExpireHours = "rolebots.corp.assignExpireHours"
	// settingAssignBoardMax: jobs a corporation keeps on its Assignments
	// board (corpassign.go). settingAssignNoticeHours: the least time between
	// two "new work on the board" letters to one member.
	settingAssignBoardMax    = "rolebots.corp.assignBoardMax"
	settingAssignNoticeHours = "rolebots.corp.assignNoticeHours"

	// settingNPCChair: every NPC corporation holding bots is run by its own NPC CEO
	// (npcchair.go). Default off; off, the weekly promotion pass fills NPC seats.
	settingNPCChair = "rolebots.corp.npcChair"
	// settingNPCChairSpendPermille and settingNPCChairSpendCapM: an NPC
	// chair spends at most this share of its wallet (per mille) per week,
	// and never more than the cap (millions of ISK).
	settingNPCChairSpendPermille = "rolebots.corp.npcChairSpendPermille"
	settingNPCChairSpendCapM     = "rolebots.corp.npcChairSpendCapM"
	// settingNPCChairOffices: how many owner offices an NPC chair keeps
	// (npcchair_offices.go), headquarters included.
	settingNPCChairOffices = "rolebots.corp.npcChairOffices"

	// settingLadderPreempt: a full corporation's CEO sets aside a long training for the
	// next member-limit skill (corpceo_skills.go). Default on.
	settingLadderPreempt = "rolebots.corp.ladderPreempt"
	// settingFoundDemandRatio: the demand pass founds a corporation when the bots
	// waiting in NPC corporations outnumber the free openings this many times over
	// (corpfound_demand.go).
	settingFoundDemandRatio = "rolebots.corpFound.demandRatio"
	// settingExpandLoadHours and settingExpandMembersPerBase: Industry grows into another
	// factory office when a base's queued production per line passes the hours, or its
	// posted Industry members pass the count, even with lines free (corpexpand.go).
	settingExpandLoadHours      = "rolebots.corp.expandLoadHours"
	settingExpandMembersPerBase = "rolebots.corp.expandMembersPerBase"
	// settingGrowthSharePct: how much of its payroll and dividends (percent) a
	// corporation under the tower line holds back to grow on (corpgrowth.go).
	settingGrowthSharePct = "rolebots.corp.growthSharePct"
)

var (
	humanPipelineV atomic.Bool
	corpNewsV      atomic.Bool
	assignmentsV   atomic.Bool
	meritV         atomic.Bool
	postingsV      atomic.Bool
	humanPostingsV atomic.Bool
	labExpandV     atomic.Bool
	factoryExpandV atomic.Bool
	printMovesV    atomic.Bool
	baseRolesV     atomic.Bool
	npcChairV      atomic.Bool
	ladderPreemptV atomic.Bool

	foundDemandRatioV     atomic.Int64
	expandLoadHoursV      atomic.Int64
	expandMembersPerBaseV atomic.Int64
	growthSharePctV       atomic.Int64

	npcChairSpendPermilleV atomic.Int64
	npcChairSpendCapMV     atomic.Int64
	npcChairOfficesV       atomic.Int64

	printMoveWageV     atomic.Int64
	printMoveHoldHourV atomic.Int64

	humanOfferHoursV   atomic.Int64
	newsMinHoursV      atomic.Int64
	assignOpenMaxV     atomic.Int64
	assignExpireHoursV atomic.Int64
	assignBoardMaxV    atomic.Int64
	assignNoticeHoursV atomic.Int64
)

func humanPipelineOn() bool { return humanPipelineV.Load() }
func postingsOn() bool      { return postingsV.Load() }
func humanPostingsOn() bool { return humanPostingsV.Load() }
func labExpandOn() bool     { return labExpandV.Load() }
func factoryExpandOn() bool { return factoryExpandV.Load() }
func printMovesOn() bool    { return printMovesV.Load() }
func baseRolesOn() bool     { return baseRolesV.Load() }
func corpNewsOn() bool      { return corpNewsV.Load() }
func assignmentsOn() bool   { return assignmentsV.Load() }
func meritOn() bool         { return meritV.Load() }
func npcChairOn() bool      { return npcChairV.Load() }
func ladderPreemptOn() bool { return ladderPreemptV.Load() }

func foundDemandRatio() int64     { return humanDialValue(settingFoundDemandRatio) }
func expandLoadSec() int64        { return humanDialValue(settingExpandLoadHours) * 3600 }
func expandMembersPerBase() int64 { return humanDialValue(settingExpandMembersPerBase) }
func growthShare() float64        { return float64(humanDialValue(settingGrowthSharePct)) / 100 }

func npcChairSpendPermille() int64 { return humanDialValue(settingNPCChairSpendPermille) }
func npcChairSpendCapM() int64     { return humanDialValue(settingNPCChairSpendCapM) }
func npcChairOffices() int64       { return humanDialValue(settingNPCChairOffices) }

// humanDial is one whole-number human-member setting.
type humanDial struct {
	key      string
	label    string
	def, min int64
	max      int64
	cache    *atomic.Int64
}

func humanDials() []humanDial {
	return []humanDial{
		{settingHumanOfferHours, "Unanswered offer lapses after (hours)", 72, 1, 24 * 30, &humanOfferHoursV},
		{settingNewsMinHours, "At most one bulletin per (hours)", 6, 1, 24 * 7, &newsMinHoursV},
		{settingAssignOpenMax, "Assignments a member may hold at once", 2, 1, 10, &assignOpenMaxV},
		{settingAssignExpireHours, "Assignment expires after (hours)", 72, 6, 24 * 30, &assignExpireHoursV},
		{settingAssignBoardMax, "Jobs kept on the Assignments board", 4, 1, 20, &assignBoardMaxV},
		{settingAssignNoticeHours, "At most one \"new work\" letter per member per (hours)", 48, 12, 24 * 14, &assignNoticeHoursV},
		{settingPrintMoveWage, "Print carried between bases: pay per jump (ISK)", 40000, 1000, 10_000_000, &printMoveWageV},
		{settingPrintMoveHoldHours, "A capsuleer's claim on a print move holds bots off (hours)", 24, 1, 24 * 7, &printMoveHoldHourV},
		{settingNPCChairSpendPermille, "NPC chair: weekly spend, per mille of the corp wallet", 5, 1, 100, &npcChairSpendPermilleV},
		{settingNPCChairSpendCapM, "NPC chair: weekly spend cap (millions of ISK)", 50, 1, 100_000, &npcChairSpendCapMV},
		{settingNPCChairOffices, "NPC chair: owner offices kept, headquarters included", 3, 1, 5, &npcChairOfficesV},
		{settingFoundDemandRatio, "Found a corporation when bots waiting outnumber free openings (times over)", 3, 1, 100, &foundDemandRatioV},
		{settingExpandLoadHours, "Industry expands when a base's queue per line passes (hours)", 48, 6, 24 * 14, &expandLoadHoursV},
		{settingExpandMembersPerBase, "Industry expands when a base's posted members pass", 30, 5, 500, &expandMembersPerBaseV},
		{settingGrowthSharePct, "Payroll and dividends held back while under the tower line (percent)", 35, 1, 90, &growthSharePctV},
	}
}

func (d humanDial) get() int64 {
	if v := d.cache.Load(); v > 0 {
		return v
	}
	return d.def
}

func humanDialValue(key string) int64 {
	for _, d := range humanDials() {
		if d.key == key {
			return d.get()
		}
	}
	return 0
}

func humanOfferSec() int64   { return humanDialValue(settingHumanOfferHours) * 3600 }
func newsMinSec() int64      { return humanDialValue(settingNewsMinHours) * 3600 }
func assignOpenMax() int     { return int(humanDialValue(settingAssignOpenMax)) }
func assignExpireSec() int64 { return humanDialValue(settingAssignExpireHours) * 3600 }
func assignBoardMax() int    { return int(humanDialValue(settingAssignBoardMax)) }
func assignNoticeSec() int64 { return humanDialValue(settingAssignNoticeHours) * 3600 }
func printMoveWagePerJump() float64 {
	return float64(humanDialValue(settingPrintMoveWage))
}
func printMoveHoldSec() int64 { return humanDialValue(settingPrintMoveHoldHours) * 3600 }

// humanSwitch is one on/off switch for the panel. def is the value when no server
// setting exists.
type humanSwitch struct {
	key   string
	label string
	cache *atomic.Bool
	def   bool
}

func humanSwitches() []humanSwitch {
	return []humanSwitch{
		{settingHumanPipeline, "Applicant mail, offer expiry, welcome", &humanPipelineV, true},
		{settingCorpNews, "Corp bulletins (Corp mail tab)", &corpNewsV, true},
		{settingAssignments, "Paid assignments for human members", &assignmentsV, true},
		{settingMerit, "Merit ranks (roles, titles, pay) for every member", &meritV, true},
		{settingPostings, "Members live at their Base; the CEO posts them to offices by job", &postingsV, true},
		{settingHumanPostings, "The CEO posts human members too (by mail, with a choice)", &humanPostingsV, true},
		{settingLabExpand, "R&D grows into another lab office when its bases run out of lines", &labExpandV, true},
		{settingFactoryExpand, "Industry grows into another factory office when its lines run out", &factoryExpandV, true},
		{settingPrintMoves, "Prints are carried between the corporation's bases", &printMovesV, true},
		{settingBaseRoles, "Department job roles held at Base only (needs core corp.locationRoles)", &baseRolesV, false},
		{settingNPCChair, "NPC corporations are run by their own NPC CEO (directive, Security desk, seats, doctrine)", &npcChairV, false},
		{settingLadderPreempt, "A full corporation's CEO sets aside a long training for the member-limit skill", &ladderPreemptV, true},
	}
}

// loadHumanSettings reads every human-member dial at boot. Never fatal.
func (m *Mod) loadHumanSettings(ctx context.Context) {
	for _, sw := range humanSwitches() {
		sw.cache.Store(sw.def)
		v, ok, err := evedb.GetServerSetting(ctx, m.db, sw.key)
		if err != nil {
			rlogf("rolebots: read %s: %v (using the default)", sw.key, err)
			continue
		}
		if ok {
			sw.cache.Store(strings.TrimSpace(v) == "1")
		}
	}
	for _, d := range humanDials() {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, d.key)
		if err != nil {
			rlogf("rolebots: read %s: %v (using the default)", d.key, err)
			continue
		}
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n > 0 {
			d.cache.Store(min(max(n, d.min), d.max))
		}
	}
}

// setHumanSwitch persists one of the switches.
func (m *Mod) setHumanSwitch(ctx context.Context, key string, on bool) error {
	for _, sw := range humanSwitches() {
		if sw.key != key {
			continue
		}
		val := "0"
		if on {
			val = "1"
		}
		if err := evedb.SetServerSetting(ctx, m.db, key, val); err != nil {
			return fmt.Errorf("setHumanSwitch %s: %w", key, err)
		}
		sw.cache.Store(on)
		return nil
	}
	return fmt.Errorf("setHumanSwitch: %s is not a human-member switch", key)
}

// setHumanDial persists one numeric dial, clamped.
func (m *Mod) setHumanDial(ctx context.Context, key string, n int64) error {
	for _, d := range humanDials() {
		if d.key != key {
			continue
		}
		n = min(max(n, d.min), d.max)
		if err := evedb.SetServerSetting(ctx, m.db, key, strconv.FormatInt(n, 10)); err != nil {
			return fmt.Errorf("setHumanDial %s: %w", key, err)
		}
		d.cache.Store(n)
		return nil
	}
	return fmt.Errorf("setHumanDial: %s is not a human-member dial", key)
}
