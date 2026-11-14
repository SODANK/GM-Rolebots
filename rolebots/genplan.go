package rolebots

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
)

// A generated pilot's identity, decided before anything is written.
//
// A seed run has two phases. This file turns the operator's ratios into a list of
// pilots (plannedBot) and botgen.go executes it, so a plan can be counted,
// previewed and refused before any account row exists.
//
// An archetype is not a job title. What a rolebot does comes from its personality
// (appetite.go), so each archetype names a chargen chain (department, field,
// speciality, resolved by name) and trait offsets, and the two must agree.

// archetype is one kind of pilot.
type archetype struct {
	Key   string
	Label string
	Blurb string

	// Departments and Fields are chargen names, tried in order; the first that
	// exists for the drawn race wins. The department decides the school, since a
	// school offers exactly two.
	Departments []string
	Fields      []string

	// Traits is the personality offset on the module's -100..+100 scale
	// (traitCatalog in tuning.go). Several read backwards from their label: Caution
	// high is careless and Patience high is hurried.
	Traits map[string]int

	// AttrBias names the two chargen attributes the 5 bonus points lean toward, so
	// a trader trains Trade skills faster than a gunner.
	AttrBias [2]string

	// DefaultWeight is this archetype's share of a fresh seed form, in tenths of a percent.
	// 0 takes an even share of what the fixed ones leave (defaultArchetypeWeights).
	DefaultWeight int
}

// FormPct is DefaultWeight as the seed form's percentage box shows it.
func (a archetype) FormPct() string {
	return strconv.FormatFloat(float64(defaultArchetypeWeights()[a.Key])/10, 'f', -1, 64)
}

// defaultArchetypeWeights is the form's opening split, in tenths of a percent, summing to
// 1000: each archetype with a DefaultWeight keeps it, the rest share what is left evenly
// (the first ones take the remainder). The form and defaultGenSpec both read it. Pure.
func defaultArchetypeWeights() map[string]int {
	out := make(map[string]int, len(archetypes))
	fixed, free := 0, 0
	for _, a := range archetypes {
		if a.DefaultWeight > 0 {
			out[a.Key] = a.DefaultWeight
			fixed += a.DefaultWeight
		} else {
			free++
		}
	}
	if free == 0 {
		return out
	}
	each, rem := (1000-fixed)/free, (1000-fixed)%free
	for _, a := range archetypes {
		if a.DefaultWeight > 0 {
			continue
		}
		out[a.Key] = each
		if rem > 0 {
			out[a.Key]++
			rem--
		}
	}
	return out
}

// archetypes is the catalogue, in the order the seed form shows them.
var archetypes = []archetype{
	{
		Key: "miner", Label: "Miner",
		Blurb:       "Mines asteroid belts. Fills the ore hold, sells or refines it, returns to the belt.",
		Departments: []string{"Industry"},
		Fields:      []string{"Mining"},
		Traits: map[string]int{
			TraitCaution:    -45, // careful
			TraitAggression: -55, // will not shoot
			TraitPatience:   25, // hurried: ore sold at the bid is quick money
			TraitWanderlust: 30, // prospects: a different belt every trip
			TraitSupply:     35,  // feeds the market
			TraitGreed:      -10,
			TraitToil:       -70, // blue collar: the trait that owns mining
		},
		AttrBias: [2]string{"memory", "intelligence"},
	},
	{
		Key: "industrialist", Label: "Industrialist",
		Blurb:       "Manufacturing. Buys or mines inputs, runs factory jobs, lists the output on the market.",
		Departments: []string{"Industry", "Engineering"},
		Fields:      []string{"Manufacturing", "Ship Repair"},
		Traits: map[string]int{
			TraitSupply:     80, // the supplier pole outright
			TraitPatience:   -55,
			TraitWanderlust: -55, // a factory is a place you stay
			TraitAggression: -45,
			TraitCaution:    -35,
			TraitGreed:      20,
			// A shade blue of the middle: while a job runs it digs its own materials
			// rather than playing the market.
			TraitToil: -20,
		},
		AttrBias: [2]string{"memory", "intelligence"},
	},
	{
		Key: "trader", Label: "Trader",
		Blurb:       "Buys low and sells high. Hauls between stations and keeps standing buy and sell orders.",
		Departments: []string{"Business"},
		Fields:      []string{"Market Fundamental", "Management", "Public Relations"},
		Traits: map[string]int{
			TraitGreed:      65,
			TraitWanderlust: 35,
			TraitPatience:   40, // hurried: wants turnover
			TraitAggression: -35,
			TraitLoyalty:    30, // goes where the work is
			TraitSupply:     -15,
			TraitLeadership: 10, // deals with people for a living
			TraitToil:       70, // white collar: the trade book is desk work
		},
		AttrBias: [2]string{"charisma", "memory"},
	},
	{
		Key: "hauler", Label: "Hauler",
		Blurb:       "Moves other people's cargo for a fee. Takes courier contracts and flies freight between stations.",
		Departments: []string{"Operations", "Industry"},
		Fields:      []string{"Pilot", "Market Fundamental"},
		Traits: map[string]int{
			TraitGreed:      -50, // generous: works for the fee, not the margin
			TraitWanderlust: 60,  // lives on the road
			TraitToil:       -35, // hands-on work, short of the rocks
			TraitCaution:    -30, // collateral is at stake
			TraitAggression: -40,
			TraitSupply:     -10,
		},
		AttrBias: [2]string{"perception", "memory"},
	},
	{
		Key: "combat", Label: "Combat",
		Blurb:       "Earns with guns. Shoots NPCs in belts for bounties and loot, and engages players it holds negative standing toward.",
		Departments: []string{"Gunnery", "Operations"},
		Fields:      []string{"Advanced Gunnery", "Improved Laser  Weapons", "Improved Projectile Weapons", "Improved Hybrid Weapons", "Rocketry", "Recon"},
		Traits: map[string]int{
			TraitAggression: 75,
			TraitCaution:    45, // careless: fights into structure
			TraitLawfulness: 30,
			TraitPatience:   35,
			TraitSupply:     -50, // buys what it needs, earns with guns
			TraitWanderlust: 20,
			TraitLeadership: 5,
		},
		AttrBias: [2]string{"perception", "willpower"},
	},
	{
		Key: "researcher", Label: "Researcher",
		Blurb:       "Works the laboratory. Researches blueprints to full efficiency, copies them and sells the copies.",
		Departments: []string{"Science"},
		Fields:      []string{"Applied Science", "Physics", "Biology"},
		// The centre sits where ResearchAppetite (tuning.go) clears researchCareerMin with room
		// for the jitter blendTraits adds: Toil near the laboratory peak, unhurried, a maker
		// rather than a buyer, and rooted to one bench.
		Traits: map[string]int{
			TraitToil:       35,
			TraitPatience:   -45,
			TraitSupply:     70,
			TraitWanderlust: -40,
			TraitAggression: -45,
			TraitCaution:    -30,
		},
		AttrBias: [2]string{"intelligence", "memory"},
		// About the share the one-time seed gave the roster (INDUSTRY_RESEARCH_GAPS.md D3).
		DefaultWeight: 80,
	},
}

// legacyArchetypes are the two retired archetypes (outlaw, drifter). They stay
// so a stored rolebot_generated row still resolves to a real archetype;
// criminality is now a disposition.
var legacyArchetypes = []archetype{
	{
		Key: "outlaw", Label: "Outlaw (retired)",
		Blurb:       "Piracy. Retired as an archetype — generate a Criminal Combat pilot instead.",
		Departments: []string{"Gunnery", "Operations"},
		Fields:      []string{"Advanced Gunnery", "Recon", "Rocketry"},
		Traits: map[string]int{
			TraitAggression: 85,
			TraitLawfulness: 80, // outlaw pole
			TraitCaution:    60,
			TraitLoyalty:    60, // holds no grudges, takes any work
			TraitGreed:      45,
			TraitSupply:     -60,
		},
		AttrBias: [2]string{"perception", "willpower"},
	},
	{
		Key: "drifter", Label: "Drifter (retired)",
		Blurb: "No specialisation. Retired — it generated pilots with nothing to do.",
		Traits: map[string]int{
			TraitWanderlust: 45,
			TraitLoyalty:    35,
		},
		AttrBias: [2]string{"intelligence", "perception"},
	},
}

// archetypeByKey resolves a key from either catalogue: the form offers only the
// live four, but every reader of a stored row must understand the retired two.
func archetypeByKey(k string) (archetype, bool) {
	for _, a := range archetypes {
		if a.Key == k {
			return a, true
		}
	}
	for _, a := range legacyArchetypes {
		if a.Key == k {
			return a, true
		}
	}
	return archetype{}, false
}

// ── disposition ─────────────────────────────────────────────────────────────

// disposition is whether a Capsuleer works inside the law or outside it. The
// criminal offsets are the retired Outlaw archetype's, so a criminal Miner is a
// rock thief and a criminal Trader smuggles.
type disposition struct {
	Key    string
	Label  string
	Blurb  string
	Traits map[string]int
}

var dispositions = []disposition{
	{
		Key: "lawful", Label: "Lawful",
		Blurb:  "Keeps to legal work and policed space. Whatever its trade is, it does it above board.",
		Traits: map[string]int{TraitLawfulness: -55, TraitCaution: -25},
	},
	{
		Key: "criminal", Label: "Criminal",
		Blurb:  "Works outside the law, at whatever its trade happens to be — piracy, rock theft, smuggling.",
		Traits: map[string]int{TraitLawfulness: 75, TraitCaution: 45, TraitLoyalty: 40, TraitGreed: 25, TraitLeadership: 10},
	},
}

func dispositionByKey(k string) (disposition, bool) {
	for _, d := range dispositions {
		if d.Key == k {
			return d, true
		}
	}
	return disposition{}, false
}

// ── maturity ────────────────────────────────────────────────────────────────

// maturity is how much sheet a pilot arrives with. Green has chargen skills
// only; the higher tiers are sheets written per (archetype, tier, race) in
// sheets.go, stated in ship classes and weapon sizes.
type maturity struct {
	Key   string
	Label string
	Blurb string
	// Trained reports whether this tier grants anything beyond the character
	// creator's package, and so whether the backstory in history.go applies.
	Trained bool
	// LeadershipLean moves the centre of the leadership draw (leadership.go): years
	// in the chair make somebody the one others look to.
	LeadershipLean int
}

var maturities = []maturity{
	{"green", "Green", "Character-creation skills only. Flies a frigate. The module trains it from there.", false, -10},
	{"seasoned", "Seasoned", "Just arriving at cruisers: racial frigate IV, cruiser II, medium weapons and solid engineering and navigation. Traders and industrialists get their racial hauler and their trade instead.", true, 5},
	{"veteran", "Veteran", "Frigate line maxed, cruisers grounded, battleship I, and leadership. Years of employment history behind it.", true, 30},
}

func maturityByKey(k string) (maturity, bool) {
	for _, mt := range maturities {
		if mt.Key == k {
			return mt, true
		}
	}
	return maturity{}, false
}

// ── the plan ────────────────────────────────────────────────────────────────

// ratio is one weighted option in a mix. Weights are relative; the panel shows
// and normalises them as percentages.
type ratio struct {
	Key    string
	Weight int
}

// allocate splits count across rs by largest remainder: every option gets its
// floor and the leftover goes to the options the rounding robbed most. Projected
// head counts are then exact, which independent rolls (rollRatio) give only on
// average. An all-zero mix spreads evenly.
func allocate(count int, rs []ratio) map[string]int {
	out := map[string]int{}
	if count <= 0 || len(rs) == 0 {
		return out
	}
	total := 0
	for _, r := range rs {
		if r.Weight > 0 {
			total += r.Weight
		}
	}
	if total == 0 {
		for i, r := range rs {
			out[r.Key] = count / len(rs)
			if i < count%len(rs) {
				out[r.Key]++
			}
		}
		return out
	}

	type rem struct {
		key string
		r   int // remainder numerator: (count*weight) mod total
	}
	var rems []rem
	assigned := 0
	for _, r := range rs {
		if r.Weight <= 0 {
			continue
		}
		n := count * r.Weight / total
		out[r.Key] = n
		assigned += n
		rems = append(rems, rem{r.Key, (count * r.Weight) % total})
	}
	// Largest remainder first; ties break on the order the options were
	// declared, so the same spec always allocates the same way.
	sort.SliceStable(rems, func(i, j int) bool { return rems[i].r > rems[j].r })
	for i := 0; assigned < count && len(rems) > 0; i++ {
		out[rems[i%len(rems)].key]++
		assigned++
	}
	return out
}

// rollRatio draws one key from a weighted list. An all-zero list draws
// uniformly rather than failing: a form submitted with every weight cleared
// means "no preference", not "generate nothing".
func rollRatio(rng *rand.Rand, rs []ratio) string {
	total := 0
	for _, r := range rs {
		if r.Weight > 0 {
			total += r.Weight
		}
	}
	if total == 0 {
		if len(rs) == 0 {
			return ""
		}
		return rs[rng.Intn(len(rs))].Key
	}
	n := rng.Intn(total)
	for _, r := range rs {
		if r.Weight <= 0 {
			continue
		}
		if n < r.Weight {
			return r.Key
		}
		n -= r.Weight
	}
	return rs[len(rs)-1].Key
}

// GenSpec is the whole operator-facing request: how many pilots, and the four
// mixes they are drawn from.
type GenSpec struct {
	Count int

	Archetypes   []ratio // by archetype key
	Bloodlines   []ratio // by bloodlineID (as a decimal string key)
	Dispositions []ratio // by disposition key (lawful / criminal)
	Maturities   []ratio // by maturity key
	Timezones    []ratio // by login window key (tzWindowOpts); empty keeps each pilot's own draw

	// GenderFemalePct is the share of the population that is female, 0..100.
	GenderFemalePct int

	// WalletMin/Max is the starting balance band, in ISK. Each pilot draws
	// uniformly inside it.
	WalletMin, WalletMax float64
}

// plannedBot is one resolved pilot, everything decided, nothing written.
type plannedBot struct {
	// Seq is the N in RoleBotN, the account number, which several pilots share. Slot
	// is this pilot's position on that account, 0-based.
	Seq         int
	Slot        int
	AccountName string

	Archetype   archetype
	Disposition disposition
	Maturity    maturity
	// TZWindow is a tzWindowOpts key, or "" to keep the pilot's own draw.
	TZWindow string

	BloodlineID   int64
	BloodlineName string
	RaceID        int64
	Gender        int64
	Name          string

	// The chargen chain, resolved against this database.
	SchoolID     int64
	SchoolName   string
	DepartmentID int64
	DeptName     string
	FieldID      int64
	FieldName    string
	SpecialityID int64
	SpecName     string
	AncestryID   int64

	// Chargen bonus picks, 5 points across the five attributes.
	Attrs map[string]int64

	Balance float64
}

// careerPicker resolves archetype to (school, department, field, speciality) for
// a race, by name, against this database. It is a picker, not a table, because
// the chain differs per race and a hand-added school must be able to appear.
type careerPicker struct {
	// schools[raceID] to schoolIDs
	schools map[int64][]int64
	// depts[schoolID] to (id, name)
	depts map[int64][]namedID
	// fields[deptID] to (id, name)
	fields map[int64][]namedID
	// specs[fieldID] to (id, name)
	specs map[int64][]namedID
	// schoolName[schoolID]
	schoolName map[int64]string
	// ancestries[bloodlineID] to ancestryIDs
	ancestries map[int64][]int64
}

func loadCareerPicker(ctx context.Context, m *Mod) (*careerPicker, error) {
	cp := &careerPicker{
		schools:    map[int64][]int64{},
		depts:      map[int64][]namedID{},
		fields:     map[int64][]namedID{},
		specs:      map[int64][]namedID{},
		schoolName: map[int64]string{},
		ancestries: map[int64][]int64{},
	}

	rows, err := m.db.QueryContext(ctx, `SELECT schoolID, schoolName, raceID FROM chrSchools ORDER BY schoolID`)
	if err != nil {
		return nil, fmt.Errorf("loadCareerPicker schools: %w", err)
	}
	for rows.Next() {
		var id, race int64
		var name string
		if err := rows.Scan(&id, &name, &race); err != nil {
			rows.Close()
			return nil, err
		}
		cp.schools[race] = append(cp.schools[race], id)
		cp.schoolName[id] = name
	}
	rows.Close()

	// Entries with no skills attached are skipped: a few hand-added ones have
	// all-zero skill columns and would give an emptier sheet than chargen can.
	if err := loadNamed(ctx, m, cp.depts,
		`SELECT schoolID, departmentID, departmentName FROM chrDepartments
		 WHERE COALESCE(skillTypeID1,0) + COALESCE(skillTypeID2,0) + COALESCE(skillTypeID3,0) > 0
		 ORDER BY departmentID`); err != nil {
		return nil, err
	}
	if err := loadNamed(ctx, m, cp.fields,
		`SELECT departmentID, fieldID, fieldName FROM chrFields
		 WHERE COALESCE(skillTypeID1,0) + COALESCE(skillTypeID2,0) > 0
		 ORDER BY fieldID`); err != nil {
		return nil, err
	}
	if err := loadNamed(ctx, m, cp.specs,
		`SELECT fieldID, specialityID, specialityName FROM chrSpecialities
		 WHERE COALESCE(skillTypeID1,0) + COALESCE(skillTypeID2,0) > 0
		 ORDER BY specialityID`); err != nil {
		return nil, err
	}

	arows, err := m.db.QueryContext(ctx, `SELECT bloodlineID, ancestryID FROM chrAncestries ORDER BY ancestryID`)
	if err != nil {
		return nil, fmt.Errorf("loadCareerPicker ancestries: %w", err)
	}
	for arows.Next() {
		var bl, id int64
		if err := arows.Scan(&bl, &id); err != nil {
			arows.Close()
			return nil, err
		}
		cp.ancestries[bl] = append(cp.ancestries[bl], id)
	}
	arows.Close()

	return cp, nil
}

func loadNamed(ctx context.Context, m *Mod, into map[int64][]namedID, query string) error {
	rows, err := m.db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("loadCareerPicker: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var parent int64
		var n namedID
		if err := rows.Scan(&parent, &n.ID, &n.Name); err != nil {
			return err
		}
		into[parent] = append(into[parent], n)
	}
	return rows.Err()
}

// matchName finds the first candidate whose name matches a wanted name, ignoring
// case and internal whitespace runs. The tolerance is needed: this database has a
// field named "Improved Laser  Weapons" with two spaces.
func matchName(cands []namedID, want string) (namedID, bool) {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }
	w := norm(want)
	for _, c := range cands {
		if norm(c.Name) == w {
			return c, true
		}
	}
	return namedID{}, false
}

// pickCareer walks the chargen chain for one pilot. Every level falls back to a
// random choice among what the race offers, so an archetype whose preferred
// department is missing still gives a legal character.
func (cp *careerPicker) pickCareer(rng *rand.Rand, raceID int64, a archetype) (school, dept, field, spec namedID, err error) {
	schools := cp.schools[raceID]
	if len(schools) == 0 {
		return school, dept, field, spec, fmt.Errorf("race %d has no schools", raceID)
	}

	// Department first, because it decides the school: a school offers exactly two,
	// so asking for Gunnery is asking for the war academy.
	type cand struct {
		schoolID int64
		d        namedID
	}
	var wanted, all []cand
	for _, sid := range schools {
		for _, d := range cp.depts[sid] {
			c := cand{sid, d}
			all = append(all, c)
			for _, want := range a.Departments {
				if norm := strings.EqualFold(strings.Join(strings.Fields(d.Name), " "), want); norm {
					wanted = append(wanted, c)
				}
			}
		}
	}
	pool := wanted
	if len(pool) == 0 {
		pool = all
	}
	if len(pool) == 0 {
		return school, dept, field, spec, fmt.Errorf("race %d has no departments with skills", raceID)
	}
	chosen := pool[rng.Intn(len(pool))]
	school = namedID{chosen.schoolID, cp.schoolName[chosen.schoolID]}
	dept = chosen.d

	fields := cp.fields[dept.ID]
	if len(fields) == 0 {
		return school, dept, field, spec, fmt.Errorf("department %d (%s) has no fields with skills", dept.ID, dept.Name)
	}
	field = namedID{}
	for _, want := range a.Fields {
		if f, ok := matchName(fields, want); ok {
			field = f
			break
		}
	}
	if field.ID == 0 {
		field = fields[rng.Intn(len(fields))]
	}

	specs := cp.specs[field.ID]
	if len(specs) == 0 {
		// A field with no speciality rows is legal: CreateCharacter grants nothing from
		// that source (GetSpecialitySkills returns nil on ErrNoRows).
		return school, dept, field, namedID{}, nil
	}
	spec = specs[rng.Intn(len(specs))]
	return school, dept, field, spec, nil
}

// rollAttrs spreads the chargen bonus: 5 points across the five attributes, none
// above 3, as the real client does. The archetype's two bias attributes win two
// draws in three.
func rollAttrs(rng *rand.Rand, a archetype) map[string]int64 {
	keys := []string{"intelligence", "charisma", "perception", "memory", "willpower"}
	out := map[string]int64{}
	for _, k := range keys {
		out[k] = 0
	}
	biased := map[string]bool{a.AttrBias[0]: true, a.AttrBias[1]: true}
	for pts := 0; pts < 5; pts++ {
		for try := 0; try < 24; try++ {
			k := keys[rng.Intn(len(keys))]
			if !biased[k] && rng.Intn(3) != 0 {
				continue // biased attributes win two draws in three
			}
			if out[k] >= 3 {
				continue
			}
			out[k]++
			break
		}
	}
	return out
}

// buildPlan turns a spec into the concrete list of pilots. Nothing is written and
// no name is minted; names are drawn during execution, where the database can be
// consulted per candidate.
func (m *Mod) buildPlan(ctx context.Context, spec GenSpec, rng *rand.Rand, startSeq int) ([]plannedBot, error) {
	if spec.Count <= 0 {
		return nil, fmt.Errorf("nothing to generate: count is %d", spec.Count)
	}
	cp, err := loadCareerPicker(ctx, m)
	if err != nil {
		return nil, err
	}

	blNames := map[int64]string{}
	blRace := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx, `SELECT bloodlineID, bloodlineName, raceID FROM chrBloodlines`)
	if err != nil {
		return nil, fmt.Errorf("buildPlan bloodlines: %w", err)
	}
	for rows.Next() {
		var id, race int64
		var name string
		if err := rows.Scan(&id, &name, &race); err != nil {
			rows.Close()
			return nil, err
		}
		blNames[id], blRace[id] = name, race
	}
	rows.Close()

	// Each axis is allocated across the whole run and then shuffled, not rolled per
	// pilot: this keeps the promised head counts and independent axes.
	archKeys := shuffledDraws(rng, spec.Count, spec.Archetypes, archetypeKeys())
	blKeys := shuffledDraws(rng, spec.Count, spec.Bloodlines, ratioKeys(spec.Bloodlines))
	matKeys := shuffledDraws(rng, spec.Count, spec.Maturities, maturityKeys())
	dispKeys := shuffledDraws(rng, spec.Count, spec.Dispositions, dispositionKeys())
	// A mix stored before the timezone axis existed has no entries; its keys
	// come back "" and each pilot keeps its own draw.
	tzKeys := shuffledDraws(rng, spec.Count, spec.Timezones, tzWindowKeys())

	out := make([]plannedBot, 0, spec.Count)
	for i := 0; i < spec.Count; i++ {
		// Pilots are packed charsPerAccount to an account in order, so a wipe range
		// removes whole accounts and strands no pilot on a half-deleted one.
		pb := plannedBot{
			Seq:  startSeq + i/charsPerAccount,
			Slot: i % charsPerAccount,
		}
		pb.AccountName = fmt.Sprintf("%s%d", botAccountPrefix, pb.Seq)

		a, ok := archetypeByKey(archKeys[i])
		if !ok {
			a = archetypes[0]
		}
		pb.Archetype = a

		d, ok := dispositionByKey(dispKeys[i])
		if !ok {
			d = dispositions[0] // lawful
		}
		pb.Disposition = d

		mt, ok := maturityByKey(matKeys[i])
		if !ok {
			mt = maturities[0]
		}
		pb.Maturity = mt

		pb.TZWindow = tzKeys[i]

		var blID int64
		fmt.Sscan(blKeys[i], &blID)
		if _, ok := pools()[blID]; !ok {
			// The ratio named a bloodline with no name pool: fall back to an empire one from
			// defaultMixBloodlines, which excludes Jove (names.go).
			gens := defaultMixBloodlines()
			blID = gens[rng.Intn(len(gens))]
		}
		pb.BloodlineID = blID
		pb.BloodlineName = blNames[blID]
		pb.RaceID = blRace[blID]

		pb.Gender = genderFor(rng, blID, spec.GenderFemalePct)

		school, dept, field, spec2, err := cp.pickCareer(rng, pb.RaceID, a)
		if err != nil {
			return nil, fmt.Errorf("capsuleer %d (%s): %w", pb.Seq, pb.BloodlineName, err)
		}
		pb.SchoolID, pb.SchoolName = school.ID, school.Name
		pb.DepartmentID, pb.DeptName = dept.ID, dept.Name
		pb.FieldID, pb.FieldName = field.ID, field.Name
		pb.SpecialityID, pb.SpecName = spec2.ID, spec2.Name

		if anc := cp.ancestries[blID]; len(anc) > 0 {
			pb.AncestryID = anc[rng.Intn(len(anc))]
		}

		pb.Attrs = rollAttrs(rng, a)

		pb.Balance = walletFor(rng, spec.WalletMin, spec.WalletMax, mt)

		out = append(out, pb)
	}
	return out, nil
}

// genderFor draws one pilot's gender from the operator's female share, except for
// a bloodline the client has no female mesh for, where the mesh wins (names.go,
// maleOnlyBloodlines).
func genderFor(rng *rand.Rand, bloodlineID int64, femalePct int) int64 {
	if maleOnlyBloodlines[bloodlineID] {
		return genderMale
	}
	if rng.Intn(100) < femalePct {
		return genderFemale
	}
	return genderMale
}

// walletFor draws a starting balance inside the operator's band, leaning it by
// tier: green clusters near the floor, veteran near the ceiling, seasoned across
// the middle.
func walletFor(rng *rand.Rand, min, max float64, mt maturity) float64 {
	lo, hi := min, max
	if hi < lo {
		lo, hi = hi, lo
	}
	if hi <= lo {
		return lo
	}
	u := rng.Float64()
	var f float64
	switch mt.Key {
	case "veteran":
		f = 1 - math.Pow(1-u, 2.5)
	case "seasoned":
		f = 0.25 + 0.6*u
	default: // green
		f = math.Pow(u, 2.5)
	}
	return lo + f*(hi-lo)
}

// shuffledDraws allocates count draws over rs and returns them shuffled, one
// per pilot. order is the declaration order the allocation is spent in, which
// only matters for tie-breaking before the shuffle.
func shuffledDraws(rng *rand.Rand, count int, rs []ratio, order []string) []string {
	alloc := allocate(count, rs)
	out := make([]string, 0, count)
	for _, k := range order {
		for n := alloc[k]; n > 0; n-- {
			out = append(out, k)
		}
	}
	// An allocation can come up short only when the mix names nothing the
	// order knows about; fall back to a straight roll rather than a short plan.
	for len(out) < count {
		out = append(out, rollRatio(rng, rs))
	}
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out[:count]
}

func ratioKeys(rs []ratio) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Key)
	}
	return out
}

func archetypeKeys() []string {
	out := make([]string, 0, len(archetypes))
	for _, a := range archetypes {
		out = append(out, a.Key)
	}
	return out
}

func maturityKeys() []string {
	out := make([]string, 0, len(maturities))
	for _, mt := range maturities {
		out = append(out, mt.Key)
	}
	return out
}

func dispositionKeys() []string {
	out := make([]string, 0, len(dispositions))
	for _, d := range dispositions {
		out = append(out, d.Key)
	}
	return out
}
