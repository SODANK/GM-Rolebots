package rolebots

// The Skills tab of Bot Population: the roster's education in aggregate.
//
// Panels: the skill-point curve (bucketed, with percentiles, since the mean misleads on a
// roster seeded in waves), what is training now (chrSkillTraining, split by level),
// overlap (how many skills anybody holds, and the effective count of distinct skills in
// training, the inverse Herfindahl), and why (the planner's goal kinds in skills.go).
// Reach counts stale plans and training the floor (skillfloor.go) put there, not a plan.
//
// Every figure is recomputed from the tables the roster acts on. Only a short memo of the
// assembled board is kept, and the previous pass is served if a rebuild fails. No pilot is named.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// skillBoardMemoTTL is how long an assembled board is served for. Two
	// minutes: the pass behind it is ~10 seconds of grouped scans, and
	// nothing on this page moves faster than the skill lane that feeds it.
	skillBoardMemoTTL = 120

	// How many rows each table carries. The training front and the coverage
	// table are long tails; the top of each is the shape of them.
	skillBoardTrainRows = 40
	skillBoardCoverRows = 60
	skillBoardQueueRows = 25
	skillBoardGroupRows = 18

	// attrSkillLevel is the trained level on a skill entity's attribute row.
	// Its sibling attrSkillPoints (276) is defined in clone.go.
	attrSkillLevel = 280

	// skillCoreShare is the coverage above which a skill is part of the common core: nine
	// pilots in ten. Half a roster is a faction, not a core, and requiring everyone would let
	// one unenrolled pilot empty the set.
	skillCoreShare = 0.90
)

// ── row types ───────────────────────────────────────────────────────────────
//
// Every displayed number is a preformatted string or a method on its row.
// Module pages are parsed with no funcMap at all (TestESPPagesParse), so a
// template that formats anything itself is a page that cannot be rendered.

// skillSPBand is one bar of the skill-point histogram.
type skillSPBand struct {
	Label  string
	Min    int64 // inclusive floor
	Pilots int
	Share  float64
}

func (b skillSPBand) SharePc() int     { return int(b.Share*100 + 0.5) }
func (b skillSPBand) ShareStr() string { return fmt.Sprintf("%.1f%%", b.Share*100) }

// skillTrainRow is one skill on the training front.
type skillTrainRow struct {
	TypeID  int64
	Name    string
	Group   string
	Pilots  int
	Planned int    // of those, how many carry this skill in their current plan
	Levels  [6]int // index 1..5: how many are training each level
	Share   float64
}

func (r skillTrainRow) ShareStr() string { return fmt.Sprintf("%.1f%%", r.Share*100) }

// PlannedShare is the 0..1 fraction of the pilots training this skill that a plan still
// asks for. The rest come from the floor, an operator's own queue or a rewritten plan:
// the planner did not ask for them.
func (r skillTrainRow) PlannedShare() float64 {
	if r.Pilots <= 0 {
		return 0
	}
	return float64(r.Planned) / float64(r.Pilots)
}

func (r skillTrainRow) PlannedPc() int { return int(r.PlannedShare()*100 + 0.5) }

func (r skillTrainRow) PlannedStr() string {
	if r.Pilots <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d%%", r.PlannedPc())
}

// LevelStr renders the level mix as roman numerals with counts, biggest
// first. One cell rather than five columns: the question is which level this
// skill is mostly being taken to, not the whole distribution of it.
func (r skillTrainRow) LevelStr() string {
	type lv struct{ level, n int }
	var ls []lv
	for i := 1; i <= 5; i++ {
		if r.Levels[i] > 0 {
			ls = append(ls, lv{i, r.Levels[i]})
		}
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].n > ls[j].n })
	out := ""
	for i, l := range ls {
		if i > 0 {
			out += "   "
		}
		out += fmt.Sprintf("%s·%d", romanLevel(l.level), l.n)
	}
	return out
}

// skillCoverRow is one skill against the whole roster. Reused for the queue
// table, where Holders is queued steps and AvgLevel is the level they run to.
type skillCoverRow struct {
	TypeID   int64
	Name     string
	Group    string
	Holders  int
	Share    float64
	AvgLevel float64
	SP       int64
}

func (r skillCoverRow) ShareStr() string    { return fmt.Sprintf("%.1f%%", r.Share*100) }
func (r skillCoverRow) SharePc() int        { return int(r.Share*100 + 0.5) }
func (r skillCoverRow) AvgLevelStr() string { return fmt.Sprintf("%.2f", r.AvgLevel) }
func (r skillCoverRow) SPStr() string       { return spShort(r.SP) }

// skillGroupRow is where the points went, by skill group.
type skillGroupRow struct {
	Name    string
	Skills  int // distinct skills of this group anybody holds
	Holders int // (pilot, skill) pairs, not distinct pilots
	SP      int64
	Share   float64
}

func (r skillGroupRow) ShareStr() string { return fmt.Sprintf("%.1f%%", r.Share*100) }
func (r skillGroupRow) SharePc() int     { return int(r.Share*100 + 0.5) }
func (r skillGroupRow) SPStr() string    { return spShort(r.SP) }

// skillCoverBand is the overlap histogram: how many distinct skills are held
// by what share of the roster.
type skillCoverBand struct {
	Label  string
	Skills int
	Share  float64 // of the skills anybody holds at all
}

func (b skillCoverBand) SharePc() int { return int(b.Share*100 + 0.5) }

// skillKindRow is one goal kind's share of everything currently planned.
type skillKindRow struct {
	Kind   string
	Label  string
	Blurb  string
	Steps  int
	Pilots int
	Hours  float64
	// Pending is steps planned but not queued, because the pilot cannot buy
	// the book yet. Concentrated pending on one kind is that kind being
	// priced out rather than being chosen against.
	Pending int
	Share   float64
}

func (r skillKindRow) ShareStr() string { return fmt.Sprintf("%.1f%%", r.Share*100) }
func (r skillKindRow) SharePc() int     { return int(r.Share*100 + 0.5) }
func (r skillKindRow) DaysStr() string  { return fmt.Sprintf("%.0f", r.Hours/24) }

// skillCountRow is a labelled count with a share, used by the plan freshness panel and
// the two personality dial panels.
type skillCountRow struct {
	Label  string
	Pilots int
	Share  float64
}

func (r skillCountRow) SharePc() int     { return int(r.Share*100 + 0.5) }
func (r skillCountRow) ShareStr() string { return fmt.Sprintf("%.1f%%", r.Share*100) }

// skillPrefRowAgg is one operator override, counted across the roster.
type skillPrefRowAgg struct {
	TypeID int64
	Name   string
	Mode   string
	Pilots int
}

// ── the board ───────────────────────────────────────────────────────────────

// skillBoard is everything the page draws, assembled once and memoed.
type skillBoard struct {
	BuiltAt int64

	Roster   int
	Enabled  int
	AutoOff  int // pilots that do not train themselves
	NoSheet  int // on the roster with no skill entity at all
	Training int
	Idle     int // nothing in training
	Queued   int // at least one step waiting behind whatever is training

	TotalSP  int64
	MeanSP   int64
	MedianSP int64
	P10      int64
	P25      int64
	P75      int64
	P90      int64
	MaxSP    int64
	// TopDecileShare is the share of all skill points held by the richest tenth of the
	// roster. It shows whether the mean describes the population or only its oldest wave.
	TopDecileShare float64
	MeanSkills     float64
	MedianSkills   int

	Bands  []skillSPBand
	Groups []skillGroupRow

	Front          []skillTrainRow
	FrontTop10     float64 // share of the front held by its ten biggest skills
	FrontEffective float64 // 1 / sum of share squared: distinct skills, effectively
	FrontLevels    [6]int
	FrontPlanned   int // pilots training something their own plan asked for

	QueueRows  []skillCoverRow
	QueueSteps int
	QueueDepth float64
	QueueEmpty int

	Cover           []skillCoverRow
	CoverBands      []skillCoverBand
	DistinctSkills  int
	CatalogueSkills int
	CoreSkills      int
	CorePerPilot    float64

	Kinds       []skillKindRow
	KindSteps   int
	KindPending int

	PlanAges     []skillCountRow
	PlanReachHrs float64
	PlansPerPass int
	PaceSec      float64

	// The reconsider sweep (skillsweep.go). Epoch 0 means none has ever been
	// asked for; Swept is how many pilots have been round since the stamp,
	// which is the progress bar, and Sweeping is what is left.
	ReplanEpoch int64
	Swept       int
	Sweeping    int
	// HorizonMin is the operator's plan horizon in minutes, the centre of the patience bend.
	HorizonMin float64

	Satisfied []skillCountRow
	GroupCaps []skillCountRow
	Prefs     []skillPrefRowAgg

	// Notes lists soft failures. The roster census and the skill-point pass fail the page;
	// every other pass is a panel that says it is missing rather than showing zeroes.
	Notes []string
}

// Age is how long ago the board was assembled, for the refresh line.
func (b *skillBoard) Age() string {
	if b.BuiltAt == 0 {
		return "just now"
	}
	d := clock.Unix() - b.BuiltAt
	switch {
	case d < 2:
		return "just now"
	case d < 90:
		return fmt.Sprintf("%ds ago", d)
	default:
		return fmt.Sprintf("%dm ago", d/60)
	}
}

// The headline figures, formatted. Methods rather than extra map keys so the
// template reads .B.X throughout and cannot be handed a stale copy of one.
func (b *skillBoard) TotalSPStr() string  { return spShort(b.TotalSP) }
func (b *skillBoard) MeanSPStr() string   { return spShort(b.MeanSP) }
func (b *skillBoard) MedianSPStr() string { return spShort(b.MedianSP) }
func (b *skillBoard) P10Str() string      { return spShort(b.P10) }
func (b *skillBoard) P25Str() string      { return spShort(b.P25) }
func (b *skillBoard) P75Str() string      { return spShort(b.P75) }
func (b *skillBoard) P90Str() string      { return spShort(b.P90) }
func (b *skillBoard) MaxSPStr() string    { return spShort(b.MaxSP) }

func (b *skillBoard) TopDecileStr() string { return fmt.Sprintf("%.0f%%", b.TopDecileShare*100) }
func (b *skillBoard) MeanSkillsStr() string {
	return fmt.Sprintf("%.1f", b.MeanSkills)
}
func (b *skillBoard) CorePerPilotStr() string { return fmt.Sprintf("%.1f", b.CorePerPilot) }
func (b *skillBoard) FrontTop10Str() string   { return fmt.Sprintf("%.0f%%", b.FrontTop10*100) }
func (b *skillBoard) FrontEffectiveStr() string {
	return fmt.Sprintf("%.1f", b.FrontEffective)
}
func (b *skillBoard) QueueDepthStr() string { return fmt.Sprintf("%.1f", b.QueueDepth) }

// HorizonStr is the plan horizon as a person reads it, and HorizonHours is
// what the form's field is filled with.
func (b *skillBoard) HorizonStr() string { return durationText(b.HorizonMin) }
func (b *skillBoard) HorizonHours() int  { return int(b.HorizonMin/60 + 0.5) }
func (b *skillBoard) PatientStr() string { return durationText(b.HorizonMin * skillHorizonPatientMul) }
func (b *skillBoard) HurriedStr() string { return durationText(b.HorizonMin * skillHorizonHurriedMul) }

// SweepAsked says whether a replan sweep is running; the other Sweep fields are its progress.
// SweepETA is the lane's time for what is left at its measured pace, as PlanReach computes
// it for the whole roster.
func (b *skillBoard) SweepAsked() bool { return b.ReplanEpoch > 0 }
func (b *skillBoard) SweepStamp() string {
	if b.ReplanEpoch <= 0 {
		return "—"
	}
	return time.Unix(b.ReplanEpoch, 0).UTC().Format("2006-01-02 15:04 UTC")
}
func (b *skillBoard) SweepPc() int {
	total := b.Swept + b.Sweeping
	if total <= 0 {
		return 0
	}
	return int(float64(b.Swept)/float64(total)*100 + 0.5)
}
func (b *skillBoard) SweepETA() string {
	if b.Sweeping <= 0 || b.PlansPerPass <= 0 || b.PaceSec <= 0 {
		return "—"
	}
	h := float64(b.Sweeping) * b.PaceSec / float64(b.PlansPerPass) / 3600
	if h < 48 {
		return fmt.Sprintf("%.1f hours", h)
	}
	return fmt.Sprintf("%.1f days", h/24)
}
func (b *skillBoard) PlanReachStr() string {
	if b.PlanReachHrs <= 0 {
		return "—"
	}
	if b.PlanReachHrs < 48 {
		return fmt.Sprintf("%.1f hours", b.PlanReachHrs)
	}
	return fmt.Sprintf("%.1f days", b.PlanReachHrs/24)
}

// PlannedFrontStr is the share of everything training right now that a
// surviving plan actually asked for.
func (b *skillBoard) PlannedFrontStr() string {
	if b.Training <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", float64(b.FrontPlanned)/float64(b.Training)*100)
}

func (b *skillBoard) FrontLevelStr() string {
	out := ""
	for i := 1; i <= 5; i++ {
		if b.FrontLevels[i] == 0 {
			continue
		}
		if out != "" {
			out += "   "
		}
		out += fmt.Sprintf("%s·%d", romanLevel(i), b.FrontLevels[i])
	}
	if out == "" {
		return "—"
	}
	return out
}

// spShort renders skill points the way iskShort renders money. A mature pilot
// counts them in the millions, and a raw 4,281,553 in a table of them is
// unreadable; the two share a formatter because they are read the same way.
func spShort(v int64) string { return iskShort(float64(v)) }

var skillBoardMemo struct {
	mu      sync.Mutex
	board   *skillBoard
	builtAt int64
}

// skillBoardRows serves the memo, rebuilding when it is stale or when asked.
func (m *Mod) skillBoardRows(ctx context.Context, fresh bool) *skillBoard {
	skillBoardMemo.mu.Lock()
	defer skillBoardMemo.mu.Unlock()

	now := clock.Unix()
	if !fresh && skillBoardMemo.board != nil && now-skillBoardMemo.builtAt < skillBoardMemoTTL {
		return skillBoardMemo.board
	}
	b, err := m.buildSkillBoard(ctx)
	if err != nil {
		if skillBoardMemo.board != nil {
			rlogf("rolebots: skill board: %v (serving the previous pass)", err)
			return skillBoardMemo.board
		}
		return nil
	}
	b.BuiltAt = now
	skillBoardMemo.board, skillBoardMemo.builtAt = b, now
	return b
}

// buildSkillBoard runs the passes. The roster census and the skill-point pass
// are the spine and return an error; every other pass records a note and
// leaves its own panel empty.
func (m *Mod) buildSkillBoard(ctx context.Context) (*skillBoard, error) {
	b := &skillBoard{ReplanEpoch: replanEpoch(), HorizonMin: skillPlanHorizonMin()}
	if err := m.skillBoardCensus(ctx, b); err != nil {
		return nil, err
	}
	if err := m.skillBoardSheets(ctx, b); err != nil {
		return nil, err
	}
	soft := func(what string, err error) {
		if err != nil {
			b.Notes = append(b.Notes, what+": "+err.Error())
			rlogf("rolebots: skill board: %s: %v", what, err)
		}
	}
	soft("the skill catalogue", m.skillBoardCoverage(ctx, b))
	soft("what is training", m.skillBoardFront(ctx, b))
	soft("the queue", m.skillBoardQueue(ctx, b))
	soft("the plan mix", m.skillBoardKinds(ctx, b))
	soft("plan freshness", m.skillBoardPlanAges(ctx, b))
	soft("the personality dials", m.skillBoardDials(ctx, b))
	soft("operator overrides", m.skillBoardPrefs(ctx, b))
	return b, nil
}

// skillBoardCensus counts the roster and how much of it trains itself.
func (m *Mod) skillBoardCensus(ctx context.Context, b *skillBoard) error {
	err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(rc.enabled = 1), 0),
		       COALESCE(SUM(COALESCE(pr.autoSkills, 1) = 0), 0)
		  FROM rolebot_characters rc
		  LEFT JOIN rolebot_profiles pr ON pr.characterID = rc.characterID`).
		Scan(&b.Roster, &b.Enabled, &b.AutoOff)
	if err != nil {
		return fmt.Errorf("roster census: %w", err)
	}
	return nil
}

// skillBoardSheets is the expensive pass: every rostered pilot's total skill points and
// how many skills they spread over. A skill entity with no attribute-276 row counts as a
// fresh level 1 (250 SP), the same COALESCE characterTotalSP uses; the two must agree.
func (m *Mod) skillBoardSheets(ctx context.Context, b *skillBoard) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.ownerID, SUM(COALESCE(ea.valueInt, 250)), COUNT(*)
		  FROM rolebot_characters rc
		  JOIN entity    e ON e.ownerID = rc.characterID
		  JOIN invTypes  t ON t.typeID  = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		  LEFT JOIN entity_attributes ea
		         ON ea.itemID = e.itemID AND ea.attributeID = ?
		 WHERE g.categoryID = 16 AND e.flag IN (7, 61)
		 GROUP BY e.ownerID`, attrSkillPoints)
	if err != nil {
		return fmt.Errorf("skill points: %w", err)
	}
	defer rows.Close()

	sps := make([]int64, 0, b.Roster)
	skills := make([]int, 0, b.Roster)
	for rows.Next() {
		var owner, sp int64
		var n int
		if err := rows.Scan(&owner, &sp, &n); err != nil {
			return fmt.Errorf("skill points scan: %w", err)
		}
		sps = append(sps, sp)
		skills = append(skills, n)
		b.TotalSP += sp
		b.MeanSkills += float64(n)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("skill points: %w", err)
	}
	b.NoSheet = b.Roster - len(sps)
	if b.NoSheet < 0 {
		b.NoSheet = 0
	}
	if len(sps) == 0 {
		return nil
	}
	sort.Slice(sps, func(i, j int) bool { return sps[i] < sps[j] })
	sort.Ints(skills)
	b.MeanSP = b.TotalSP / int64(len(sps))
	b.MeanSkills /= float64(len(sps))
	b.MedianSkills = skills[len(skills)/2]
	b.P10, b.P25 = pctlSP(sps, 0.10), pctlSP(sps, 0.25)
	b.MedianSP = pctlSP(sps, 0.50)
	b.P75, b.P90 = pctlSP(sps, 0.75), pctlSP(sps, 0.90)
	b.MaxSP = sps[len(sps)-1]

	if top := len(sps) / 10; top > 0 && b.TotalSP > 0 {
		var topSP int64
		for _, sp := range sps[len(sps)-top:] {
			topSP += sp
		}
		b.TopDecileShare = float64(topSP) / float64(b.TotalSP)
	}
	b.Bands = spBands(sps)
	return nil
}

// pctlSP reads a percentile off a sorted slice. Nearest-rank rather than
// interpolated: these are counts of pilots, and half a pilot is not a
// clarification.
func pctlSP(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// spBandFloors are the histogram's edges, in skill points.
var spBandFloors = []struct {
	Label string
	Min   int64
}{
	{"under 100k", 0},
	{"100k – 250k", 100_000},
	{"250k – 500k", 250_000},
	{"500k – 1m", 500_000},
	{"1m – 2m", 1_000_000},
	{"2m – 5m", 2_000_000},
	{"5m – 10m", 5_000_000},
	{"10m and up", 10_000_000},
}

// spBands buckets a sorted slice of per-pilot totals. A value takes the last floor it
// clears, since the floors ascend.
func spBands(sorted []int64) []skillSPBand {
	out := make([]skillSPBand, len(spBandFloors))
	for i, f := range spBandFloors {
		out[i] = skillSPBand{Label: f.Label, Min: f.Min}
	}
	for _, sp := range sorted {
		i := 0
		for j, f := range spBandFloors {
			if sp >= f.Min {
				i = j
			}
		}
		out[i].Pilots++
	}
	for i := range out {
		if len(sorted) > 0 {
			out[i].Share = float64(out[i].Pilots) / float64(len(sorted))
		}
	}
	return out
}

// skillBoardCoverage is the per-skill pass: who holds what, to what level, and how many
// points sit in it. One scan feeds the coverage table, the overlap histogram and the
// group rollup, which folds the same rowset.
func (m *Mod) skillBoardCoverage(ctx context.Context, b *skillBoard) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, t.typeName, g.groupName,
		       COUNT(*), SUM(COALESCE(lv.valueInt, 1)), SUM(COALESCE(sp.valueInt, 250))
		  FROM rolebot_characters rc
		  JOIN entity    e ON e.ownerID = rc.characterID
		  JOIN invTypes  t ON t.typeID  = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		  LEFT JOIN entity_attributes lv
		         ON lv.itemID = e.itemID AND lv.attributeID = ?
		  LEFT JOIN entity_attributes sp
		         ON sp.itemID = e.itemID AND sp.attributeID = ?
		 WHERE g.categoryID = 16 AND e.flag IN (7, 61)
		 GROUP BY e.typeID, t.typeName, g.groupName`, attrSkillLevel, attrSkillPoints)
	if err != nil {
		return fmt.Errorf("skill coverage: %w", err)
	}
	defer rows.Close()

	var all []skillCoverRow
	byGroup := map[string]*skillGroupRow{}
	for rows.Next() {
		var r skillCoverRow
		var lvSum int64
		if err := rows.Scan(&r.TypeID, &r.Name, &r.Group, &r.Holders, &lvSum, &r.SP); err != nil {
			return fmt.Errorf("skill coverage scan: %w", err)
		}
		if r.Holders > 0 {
			r.AvgLevel = float64(lvSum) / float64(r.Holders)
		}
		if b.Roster > 0 {
			r.Share = float64(r.Holders) / float64(b.Roster)
		}
		all = append(all, r)

		g := byGroup[r.Group]
		if g == nil {
			g = &skillGroupRow{Name: r.Group}
			byGroup[r.Group] = g
		}
		g.Skills++
		g.Holders += r.Holders
		g.SP += r.SP
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("skill coverage: %w", err)
	}
	b.DistinctSkills = len(all)

	// How many skills the game has: the denominator for the roster's breadth. A missing
	// count renders as a dash instead of failing the panel.
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID
		 WHERE g.categoryID = 16 AND t.published = 1`).Scan(&b.CatalogueSkills)

	// The overlap histogram and the common core. CorePerPilot is the sum of the core's
	// coverage shares, the exact expected number of core skills on one sheet.
	bands := []struct {
		label string
		min   float64
	}{
		{"held by 90% or more of the roster", 0.90},
		{"50 – 90%", 0.50},
		{"10 – 50%", 0.10},
		{"1 – 10%", 0.01},
		{"under 1%", 0},
	}
	counts := make([]int, len(bands))
	for _, r := range all {
		for i, band := range bands {
			if r.Share >= band.min {
				counts[i]++
				break
			}
		}
		if r.Share >= skillCoreShare {
			b.CoreSkills++
			b.CorePerPilot += r.Share
		}
	}
	for i, band := range bands {
		cb := skillCoverBand{Label: band.label, Skills: counts[i]}
		if len(all) > 0 {
			cb.Share = float64(counts[i]) / float64(len(all))
		}
		b.CoverBands = append(b.CoverBands, cb)
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].Holders != all[j].Holders {
			return all[i].Holders > all[j].Holders
		}
		return all[i].SP > all[j].SP
	})
	if len(all) > skillBoardCoverRows {
		all = all[:skillBoardCoverRows]
	}
	b.Cover = all

	groups := make([]skillGroupRow, 0, len(byGroup))
	for _, g := range byGroup {
		if b.TotalSP > 0 {
			g.Share = float64(g.SP) / float64(b.TotalSP)
		}
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].SP > groups[j].SP })
	if len(groups) > skillBoardGroupRows {
		groups = groups[:skillBoardGroupRows]
	}
	b.Groups = groups
	return nil
}

// skillBoardFront is what is training this second, attributed. The LEFT JOIN is the
// attribution: a skill the pilot's current plan lacks was put there by something other
// than the planner.
func (m *Mod) skillBoardFront(ctx context.Context, b *skillBoard) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT st.typeID, t.typeName, g.groupName, st.targetLevel,
		       COUNT(*), COALESCE(SUM(p.typeID IS NOT NULL), 0)
		  FROM rolebot_characters rc
		  JOIN chrSkillTraining st ON st.characterID = rc.characterID
		  JOIN invTypes  t ON t.typeID  = st.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		  LEFT JOIN (SELECT DISTINCT characterID, typeID FROM rolebot_skill_plan) p
		         ON p.characterID = st.characterID AND p.typeID = st.typeID
		 GROUP BY st.typeID, t.typeName, g.groupName, st.targetLevel`)
	if err != nil {
		return fmt.Errorf("training front: %w", err)
	}
	defer rows.Close()

	byType := map[int64]*skillTrainRow{}
	var order []int64
	for rows.Next() {
		var typeID int64
		var name, group string
		var level, n, planned int
		if err := rows.Scan(&typeID, &name, &group, &level, &n, &planned); err != nil {
			return fmt.Errorf("training front scan: %w", err)
		}
		r := byType[typeID]
		if r == nil {
			r = &skillTrainRow{TypeID: typeID, Name: name, Group: group}
			byType[typeID] = r
			order = append(order, typeID)
		}
		r.Pilots += n
		r.Planned += planned
		if level >= 1 && level <= 5 {
			r.Levels[level] += n
			b.FrontLevels[level] += n
		}
		b.Training += n
		b.FrontPlanned += planned
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("training front: %w", err)
	}
	b.Idle = b.Roster - b.Training
	if b.Idle < 0 {
		b.Idle = 0
	}

	front := make([]skillTrainRow, 0, len(order))
	for _, id := range order {
		r := byType[id]
		if b.Training > 0 {
			r.Share = float64(r.Pilots) / float64(b.Training)
		}
		front = append(front, *r)
	}
	sort.Slice(front, func(i, j int) bool { return front[i].Pilots > front[j].Pilots })

	var h float64
	for _, r := range front {
		h += r.Share * r.Share
	}
	if h > 0 {
		b.FrontEffective = 1 / h
	}
	for i, r := range front {
		if i >= 10 {
			break
		}
		b.FrontTop10 += r.Share
	}
	if len(front) > skillBoardTrainRows {
		front = front[:skillBoardTrainRows]
	}
	b.Front = front
	return nil
}

// skillBoardQueue counts the steps already in chrSkillQueue behind the front. It counts
// steps rather than pilots because a queue is a plan's residue.
func (m *Mod) skillBoardQueue(ctx context.Context, b *skillBoard) error {
	var chars int
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT q.characterID)
		  FROM rolebot_characters rc
		  JOIN chrSkillQueue q ON q.characterID = rc.characterID`).
		Scan(&b.QueueSteps, &chars); err != nil {
		return fmt.Errorf("queue depth: %w", err)
	}
	b.Queued = chars
	b.QueueEmpty = b.Roster - chars
	if b.QueueEmpty < 0 {
		b.QueueEmpty = 0
	}
	if chars > 0 {
		b.QueueDepth = float64(b.QueueSteps) / float64(chars)
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT q.typeID, t.typeName, g.groupName, COUNT(*), AVG(q.targetLevel)
		  FROM rolebot_characters rc
		  JOIN chrSkillQueue q ON q.characterID = rc.characterID
		  JOIN invTypes  t ON t.typeID  = q.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 GROUP BY q.typeID, t.typeName, g.groupName
		 ORDER BY COUNT(*) DESC
		 LIMIT ?`, skillBoardQueueRows)
	if err != nil {
		return fmt.Errorf("queue contents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r skillCoverRow
		if err := rows.Scan(&r.TypeID, &r.Name, &r.Group, &r.Holders, &r.AvgLevel); err != nil {
			return fmt.Errorf("queue contents scan: %w", err)
		}
		if b.QueueSteps > 0 {
			r.Share = float64(r.Holders) / float64(b.QueueSteps)
		}
		b.QueueRows = append(b.QueueRows, r)
	}
	return rows.Err()
}

// skillKindLabels names the planner's goal kinds in display order. Keys are the goal*
// constants in skills.go; an unknown key shows under its raw name rather than being dropped.
var skillKindLabels = []struct {
	Kind, Label, Blurb string
}{
	{goalFit, "Opens a ship", "the level a fit it wants to fly actually demands"},
	{goalDeepen, "Deeper into a ship", "past the gate, up to the level its patience calls satisfied"},
	{goalSupport, "Support levers", "what makes the ship it already flies work — no fit ever asks for these"},
	{goalIndLever, "Factory levers", "throughput on manufacturing and refining it can already do"},
	{goalRefine, "Refining", "the level that turns a hold of rock into metal somebody can buy"},
	{goalIndGate, "Factory gates", "a blueprint it is locked out of installing at all"},
	{goalMarket, "Market levers", "more orders open at once, and a thinner cut on each"},
	{goalLab, "Lab levers", "blueprint research that finishes sooner"},
	{goalLabSlot, "Lab slots", "a second research job running beside the first"},
	{goalResearch, "R&D fields", "an agent collaboration, then its share of the patent draw"},
	{goalImplant, "Cybernetics", "the level the implants it holds or wants demand before they can go in"},
	{goalLearning, "Learning", "trains everything else faster — priced as time given back"},
	{goalForced, "Pinned", "an operator said so, and the arithmetic is not consulted"},
	{goalJove, "Jove hulls", "a Jove pilot's own ship line, ahead of the arithmetic"},
	{goalCommand, "Command", "Leadership for a fleet seat; corporation management for a full door"},
	{goalProcessing, "Ore processing", "Science IV, then Metallurgy, then the processing skill for the rock its bills need"},
	{goalDeepCore, "Deep core mining", "Mining V, Astrogeology V, then Deep Core Mining — the gate to Mercoxit"},
	{goalIce, "Barges & ice", "Astrogeology III and Industry V to a mining barge, then Ice Harvesting, then a bigger harvest"},
	{goalMineCruiser, "Mining cruisers", "the racial Cruiser skill for an Osprey or Scythe, then its levels once it flies one"},
	{goalLift, "Heavy lifts", "racial Industrial, General Freight and Hull Upgrades for an Infrastructure member"},
	{"", "Not recorded", "planned before the kind was written down — gone within one replan"},
}

// skillKindCase classifies a plan row by recorded kind, else by the why text. The goal
// literals stay inline, never `?` placeholders: MariaDB then puts every row in one bucket.
// See docs/comment-notes-b08.md#skill-kind-case.
const skillKindCase = `CASE
	WHEN p.kind <> '' THEN p.kind
	WHEN p.why LIKE 'pinned by an operator%' THEN 'forced'
	WHEN p.why = 'the Jove hulls it was born to fly' THEN 'jove'
	WHEN p.why = 'Science IV opens Metallurgy, the gate to the ore processing skills' THEN 'processing'
	WHEN p.why LIKE 'Metallurgy opens %' THEN 'processing'
	WHEN p.why LIKE 'refines % at a better yield' THEN 'processing'
	WHEN p.why LIKE 'the road to Deep Core Mining%' THEN 'deepcore'
	WHEN p.why IN ('the road to a mining cruiser',
	               'a bigger yield out of its mining cruiser') THEN 'minecruiser'
	WHEN p.why IN ('the road to a mining cruiser',
	               'a bigger yield out of its mining cruiser') THEN 'minecruiser'
	WHEN p.why = 'lifts its corporation''s heavy starbase pieces' THEN 'lift'
	WHEN p.why IN ('the road to a mining barge, and the ice beyond it',
	               'towers burn what ice refines into',
	               'a bigger harvest out of its barge') THEN 'ice'
	WHEN p.why LIKE 'opens %blueprint(s)%' THEN 'industry-gate'
	WHEN p.why = 'its factory and refinery run on this' THEN 'industry'
	WHEN p.why = 'turns a hold of rock into metal somebody can buy' THEN 'refine'
	WHEN p.why = 'the operator set a refining floor' THEN 'forced'
	WHEN p.why IN ('opens an R&D collaboration',
	               'raises its patent allocation rate') THEN 'research'
	WHEN p.why LIKE 'lets it plug in %' THEN 'implant'
	WHEN p.why IN ('lets it lead one of its corporation''s fleets',
	               'makes room for the applicants waiting at its door',
	               'lifts its corporation''s member limit, which it has reached',
	               'anchors its corporation''s starbase structures') THEN 'command'
	WHEN p.why = 'its blueprint research runs on this' THEN 'lab'
	WHEN p.why = 'runs a second research job beside the first' THEN 'labslot'
	WHEN p.why = 'more of the market open to it at once' THEN 'market'
	WHEN p.why = 'trains everything else faster' THEN 'learning'
	WHEN p.why LIKE 'deeper into %' THEN 'deepen'
	WHEN p.why LIKE '% runs on this' THEN 'support'
	WHEN p.why LIKE 'opens %' THEN 'fit'
	ELSE '' END`

// skillBoardKinds is the plan mix: what the current plans are for. It groups in the
// database, one row per plan step. CASE branch order matters: exact matches come before
// the LIKE that would also catch them, and the three "opens" branches go narrowest first.
func (m *Mod) skillBoardKinds(ctx context.Context, b *skillBoard) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT `+skillKindCase+` AS k,
		       COUNT(*), COUNT(DISTINCT p.characterID),
		       COALESCE(SUM(p.minutes), 0), COALESCE(SUM(p.applied = 0), 0)
		  FROM rolebot_characters rc
		  JOIN rolebot_skill_plan p ON p.characterID = rc.characterID
		 WHERE p.typeID > 0
		 GROUP BY k`)
	if err != nil {
		return fmt.Errorf("plan mix: %w", err)
	}
	defer rows.Close()

	type agg struct {
		steps, pilots, pending int
		minutes                float64
	}
	got := map[string]*agg{}
	for rows.Next() {
		var kind string
		var a agg
		if err := rows.Scan(&kind, &a.steps, &a.pilots, &a.minutes, &a.pending); err != nil {
			return fmt.Errorf("plan mix scan: %w", err)
		}
		got[kind] = &a
		b.KindSteps += a.steps
		b.KindPending += a.pending
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("plan mix: %w", err)
	}

	emit := func(kind, label, blurb string, a *agg) {
		r := skillKindRow{Kind: kind, Label: label, Blurb: blurb,
			Steps: a.steps, Pilots: a.pilots, Hours: a.minutes / 60, Pending: a.pending}
		if b.KindSteps > 0 {
			r.Share = float64(a.steps) / float64(b.KindSteps)
		}
		b.Kinds = append(b.Kinds, r)
	}
	for _, k := range skillKindLabels {
		if a := got[k.Kind]; a != nil {
			emit(k.Kind, k.Label, k.Blurb, a)
			delete(got, k.Kind)
		}
	}
	rest := make([]string, 0, len(got))
	for kind := range got {
		rest = append(rest, kind)
	}
	sort.Strings(rest)
	for _, kind := range rest {
		emit(kind, kind, "a goal kind this page has not been told about", got[kind])
	}
	sort.SliceStable(b.Kinds, func(i, j int) bool { return b.Kinds[i].Steps > b.Kinds[j].Steps })
	return nil
}

// planAgeBands are the freshness buckets. The first is the replan interval
// itself (skillReplanSec), because that is the promise the lane makes;
// everything past it is a pilot the lane has not got back to.
var planAgeBands = []struct {
	Label string
	Under int64 // seconds; 0 = no ceiling
}{
	{"planned within the hour", 3600},
	{"1 – 3 hours ago", 3 * 3600},
	{"3 – 24 hours ago", 24 * 3600},
	{"1 – 7 days ago", 7 * 24 * 3600},
	{"over a week ago", 0},
}

// skillBoardPlanAges measures the lane's reach: how long ago each pilot's
// plan was built, and how long a full circuit of the roster takes at the pace
// the lane is currently managing.
func (m *Mod) skillBoardPlanAges(ctx context.Context, b *skillBoard) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT p.characterID, MAX(p.plannedAt)
		  FROM rolebot_characters rc
		  JOIN rolebot_skill_plan p ON p.characterID = rc.characterID
		 GROUP BY p.characterID`)
	if err != nil {
		return fmt.Errorf("plan ages: %w", err)
	}
	defer rows.Close()

	now := clock.Unix()
	epoch := replanEpoch()
	counts := make([]int, len(planAgeBands))
	planned := 0
	for rows.Next() {
		var charID, at int64
		if err := rows.Scan(&charID, &at); err != nil {
			return fmt.Errorf("plan ages scan: %w", err)
		}
		planned++
		// Sweep progress: a pilot whose plan was built at or after the stamp has been round
		// since the sweep started. Anyone else, including a never-planned pilot, has not.
		if epoch > 0 && at >= epoch {
			b.Swept++
		}
		age := now - at
		for i, band := range planAgeBands {
			if band.Under == 0 || age < band.Under {
				counts[i]++
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("plan ages: %w", err)
	}
	for i, band := range planAgeBands {
		r := skillCountRow{Label: band.Label, Pilots: counts[i]}
		if b.Roster > 0 {
			r.Share = float64(counts[i]) / float64(b.Roster)
		}
		b.PlanAges = append(b.PlanAges, r)
	}
	never := b.Roster - planned
	if never < 0 {
		never = 0
	}
	r := skillCountRow{Label: "never planned", Pilots: never}
	if b.Roster > 0 {
		r.Share = float64(never) / float64(b.Roster)
	}
	b.PlanAges = append(b.PlanAges, r)

	// What the lane can do, from its two numbers: pilots asked to rethink per pass and how
	// often a pass runs. Measured against pilots that plan at all; automation off is never batched.
	b.PaceSec = m.skillPaceSec()
	b.PlansPerPass = m.skillPlansPerPass(ctx)
	self := b.Roster - b.AutoOff
	if b.PlansPerPass > 0 && b.PaceSec > 0 && self > 0 {
		b.PlanReachHrs = float64(self) * b.PaceSec / float64(b.PlansPerPass) / 3600
	}
	if b.ReplanEpoch > 0 {
		if b.Sweeping = self - b.Swept; b.Sweeping < 0 {
			b.Sweeping = 0
		}
	}
	return nil
}

// skillBoardDials reports the two personality dials that shape a plan, derived as
// resolveTuning derives them (bendInt over the trait) since a Tuning is not stored. A pilot
// with no trait row gets the default, so the query is a LEFT JOIN from the roster.
func (m *Mod) skillBoardDials(ctx context.Context, b *skillBoard) error {
	dial := func(trait string, atLow, atZero, atHigh int) ([]skillCountRow, error) {
		rows, err := m.db.QueryContext(ctx, `
			SELECT COALESCE(tr.value, 0), COUNT(*)
			  FROM rolebot_characters rc
			  LEFT JOIN rolebot_traits tr
			         ON tr.characterID = rc.characterID AND tr.trait = ?
			 GROUP BY COALESCE(tr.value, 0)`, trait)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		byValue := map[int]int{}
		for rows.Next() {
			var v, n int
			if err := rows.Scan(&v, &n); err != nil {
				return nil, err
			}
			resolved := atZero
			if t := traitScale(v); t != 0 {
				resolved = bendInt(t, atLow, atZero, atHigh)
			}
			byValue[resolved] += n
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		keys := make([]int, 0, len(byValue))
		for k := range byValue {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		out := make([]skillCountRow, 0, len(keys))
		for _, k := range keys {
			r := skillCountRow{Pilots: byValue[k]}
			if b.Roster > 0 {
				r.Share = float64(byValue[k]) / float64(b.Roster)
			}
			r.Label = fmt.Sprintf("%d", k)
			out = append(out, r)
		}
		return out, nil
	}

	sat, err := dial(TraitPatience, 4, skillSatisfiedLevelDefault, 2)
	if err != nil {
		return fmt.Errorf("satisfied level: %w", err)
	}
	for i := range sat {
		// A satisfied level reads as a level, not as a count.
		n := 0
		_, _ = fmt.Sscanf(sat[i].Label, "%d", &n)
		sat[i].Label = "satisfied at " + romanLevel(n)
	}
	b.Satisfied = sat

	caps, err := dial(TraitWanderlust, 6, skillGroupCapDefault, 2)
	if err != nil {
		return fmt.Errorf("group cap: %w", err)
	}
	for i := range caps {
		caps[i].Label += " step(s) per group, per plan"
	}
	b.GroupCaps = caps
	return nil
}

// skillBoardPrefs counts the operator's own arguments with the planner.
func (m *Mod) skillBoardPrefs(ctx context.Context, b *skillBoard) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT pf.typeID, COALESCE(t.typeName, CONCAT('type ', pf.typeID)), pf.mode, COUNT(*)
		  FROM rolebot_skill_prefs pf
		  LEFT JOIN invTypes t ON t.typeID = pf.typeID
		 GROUP BY pf.typeID, t.typeName, pf.mode
		 ORDER BY COUNT(*) DESC
		 LIMIT 20`)
	if err != nil {
		return fmt.Errorf("overrides: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r skillPrefRowAgg
		if err := rows.Scan(&r.TypeID, &r.Name, &r.Mode, &r.Pilots); err != nil {
			return fmt.Errorf("overrides scan: %w", err)
		}
		b.Prefs = append(b.Prefs, r)
	}
	return rows.Err()
}

// ── the handler ─────────────────────────────────────────────────────────────

func (m *Mod) handleSkillBoard(w http.ResponseWriter, r *http.Request) {
	// The two grouped scans take seconds on a big roster and the request context dies with
	// the browser tab, so the memo is built under its own timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		m.handleSkillBoardPost(w, r, ctx)
		return
	}

	b := m.skillBoardRows(ctx, r.URL.Query().Get("fresh") == "1")
	if b == nil {
		espError(w, "rolebot-skills", "could not read the roster's skills")
		return
	}

	espRender(w, "rolebots_skillboard.html", map[string]any{
		"Page": "rolebots",
		"Tab":  "skills",
		"B":    b,

		// The planner's own constants, so the explainer matches the code. These are the defaults;
		// three of them are bent by personality.
		"Satisfied":   romanLevel(skillSatisfiedLevelDefault),
		"HorizonShip": durationText(skillPlanHorizonMinDefault),
		"HorizonMinH": int(skillHorizonMinMinutes / 60),
		"HorizonMaxH": int(skillHorizonMaxMinutes / 60),
		"RefineFloor": refineSkillFloor(),
		"RefineMaxLv": refineFloorMaxLevel,
		// The bar the floor exists to get pilots over, so the control states what it buys.
		// See oreRefiners on the Ore page for who is short.
		"RefineBar":    fmt.Sprintf("%.0f%%", oreIntrinsicSpread*refineWorthEdge*100),
		"StepCap":      durationText(skillStepMaxMinutesDefault),
		"GroupCap":     skillGroupCapDefault,
		"LearnHorizon": durationText(skillLearningHorizonMinDefault),
		"WalletFloor":  iskString(skillWalletFloorDefault),
		"MaxSteps":     skillMaxPlanSteps,
		"EvalBudget":   skillEvalBudget,
		"ReplanMin":    skillReplanSec / 60,
		"IdleReplan":   skillIdleReplanSec / 60,
		"Workers":      skillPlanWorkers,
		"FloorSteps":   skillFloorMaxSteps,
		"FloorPerPass": skillFloorPerPass,
		"CoreShare":    fmt.Sprintf("%.0f%%", skillCoreShare*100),

		"Msg": r.URL.Query().Get("msg"),
		"Err": r.URL.Query().Get("err"),
	})
}

// handleSkillBoardPost operates the two roster-wide levers. Neither plans: one stamps a
// timestamp the lane reads, the other stores a number the next plan uses. Both return at
// once and drop the page's memo.
func (m *Mod) handleSkillBoardPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	const self = "/rolebots/skills"
	switch r.FormValue("action") {
	case "replan-all":
		if err := m.requestRosterReplan(ctx); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		clearSkillBoardMemo()
		// The confirmation is costed from the census and the lane's two numbers, not by
		// rebuilding the board, which would pay for the grouped scans twice.
		msg := "every pilot will reconsider its training — the lane takes them in batches"
		var c skillBoard
		if err := m.skillBoardCensus(ctx, &c); err == nil {
			per, pace := m.skillPlansPerPass(ctx), m.skillPaceSec()
			left := c.Roster - c.AutoOff
			eta := "—"
			if per > 0 && pace > 0 && left > 0 {
				if h := float64(left) * pace / float64(per) / 3600; h < 48 {
					eta = fmt.Sprintf("%.1f hours", h)
				} else {
					eta = fmt.Sprintf("%.1f days", h/24)
				}
			}
			msg = fmt.Sprintf("%d pilot(s) queued to reconsider — the lane takes them "+
				"about %d at a time and needs roughly %s to get round them all. "+
				"Nothing is interrupted meanwhile: each pilot keeps training what it "+
				"has until its own turn comes.", left, per, eta)
		}
		redirect(w, r, self, msg, "")
	case "replan-stop":
		if err := m.clearRosterReplan(ctx); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		clearSkillBoardMemo()
		redirect(w, r, self, "sweep cleared — pilots already round keep their new plans, "+
			"and the rest go back to the ordinary hourly cadence", "")
	case "horizon":
		hours, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("hours")), 64)
		if err != nil || hours <= 0 {
			redirect(w, r, self, "", "the horizon must be a number of hours")
			return
		}
		want := clampPlanHorizon(hours * 60)
		if err := m.setPlanHorizon(ctx, hours*60); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		clearSkillBoardMemo()
		msg := fmt.Sprintf("plans now cover about %s of training, spread by patience "+
			"from %s to %s. Plans already written keep the length they were built with — "+
			"press Reconsider to rebuild them now.",
			durationText(want), durationText(want*skillHorizonHurriedMul),
			durationText(want*skillHorizonPatientMul))
		if want != hours*60 {
			msg += fmt.Sprintf(" (asked for %.0f hours; held to the %.0f–%.0f hour range)",
				hours, skillHorizonMinMinutes/60, skillHorizonMaxMinutes/60)
		}
		redirect(w, r, self, msg, "")
	case "refine-floor":
		level, err := strconv.Atoi(strings.TrimSpace(r.FormValue("level")))
		if err != nil || level < 0 {
			redirect(w, r, self, "", "the refining floor must be a skill level, 0 to 5")
			return
		}
		want := clampRefineFloor(level)
		if err := m.setRefineFloor(ctx, want); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		clearSkillBoardMemo()
		// The dial changes what the next plan contains, not what is queued, so the message says which.
		msg := "refining floor cleared — nobody is forced to train Refining any more, " +
			"though the ordinary ranking still offers it"
		if want > 0 {
			msg = fmt.Sprintf("every pilot will train Refining to level %d. "+
				"It is a rank 1 skill, so this is hours rather than days — but plans "+
				"already written do not carry it: press Reconsider to rebuild them now.",
				want)
		}
		if want != level {
			msg += fmt.Sprintf(" (asked for %d; a skill level is 0 to %d)", level, refineFloorMaxLevel)
		}
		redirect(w, r, self, msg, "")
	default:
		http.Redirect(w, r, self, http.StatusSeeOther)
	}
}

// clearSkillBoardMemo drops the assembled board so the next read rebuilds it.
// Pressing a lever and being shown the board from before it is how an
// operator concludes the lever does nothing.
func clearSkillBoardMemo() {
	skillBoardMemo.mu.Lock()
	skillBoardMemo.board, skillBoardMemo.builtAt = nil, 0
	skillBoardMemo.mu.Unlock()
}
