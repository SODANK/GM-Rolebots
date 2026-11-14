package rolebots

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// The RoleBot Generation and Generated Characters tabs: the migration tool, the seed form,
// the population list and the total purge. They are separate from the Main Cast, which holds
// hand-built pilots and excludes the generated population (espcast.go). The sub-tab strip
// (esptabs.go) joins them under one sidebar entry.

const (
	// settingGenTarget is how many generated pilots should exist, 0 by default. It persists so
	// the seed form comes up pre-filled.
	settingGenTarget = "rolebots.genTarget"

	// settingMaxSeedBatch caps one seed run. Editable because the shipped
	// default is a guard against a mistyped number, not a technical limit.
	settingMaxSeedBatch = "rolebots.maxSeedBatch"

	// settingGenSpec is the whole mix the form was last submitted with, as JSON. A replacement
	// rolled after a migration (migrate.go) has no form and rolls from this mix.
	settingGenSpec = "rolebots.genSpec"

	// wipeAllConfirm is the phrase an operator types to purge the whole generated population.
	// It is a fixed phrase so it cannot be copied from a range name without reading.
	wipeAllConfirm = "WIPE ALL ROLEBOTS"
)

// maxSeedBatch reads the run cap, falling back to the shipped default.
func (m *Mod) maxSeedBatch(ctx context.Context) int {
	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingMaxSeedBatch)
	if err != nil || !ok {
		return defaultMaxBatch
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return defaultMaxBatch
	}
	if n > maxBatchLimit {
		return maxBatchLimit
	}
	return n
}

// handleSeed serves /rolebots/seed.
func (m *Mod) handleSeed(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		m.handleSeedPost(w, r, ctx)
		return
	}

	pop, err := m.generatedPopulation(ctx)
	if err != nil {
		espError(w, "rolebot-seed", err.Error())
		return
	}
	stats, err := namePoolStats(ctx, m.db)
	if err != nil {
		espError(w, "rolebot-seed", err.Error())
		return
	}
	raceNames := map[int64]string{}
	rows, err := m.db.QueryContext(ctx, `SELECT raceID, raceName FROM chrRaces`)
	if err == nil {
		for rows.Next() {
			var id int64
			var n string
			if rows.Scan(&id, &n) == nil {
				raceNames[id] = n
			}
		}
		rows.Close()
	}

	// Bloodlines grouped by race in the usual empire order. Jove Modifier appears at 0% (the
	// client can draw it and it borrows names); Jove Static never does, with no face or pool (names.go).
	type blRow struct {
		poolStat
		RaceName string
	}
	byRace := map[string][]blRow{}
	var raceOrder []string
	for _, s := range stats {
		rn := raceNames[s.RaceID]
		if rn == "" {
			rn = fmt.Sprintf("race %d", s.RaceID)
		}
		if _, seen := byRace[rn]; !seen {
			raceOrder = append(raceOrder, rn)
		}
		byRace[rn] = append(byRace[rn], blRow{s, rn})
	}
	sort.Strings(raceOrder)

	count := 0
	if v, ok, _ := evedb.GetServerSetting(ctx, m.db, settingGenTarget); ok {
		count, _ = strconv.Atoi(strings.TrimSpace(v))
	}
	if q := strings.TrimSpace(r.URL.Query().Get("n")); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			count = n
		}
	}
	nextSeq, _ := nextBotSeq(ctx, m.db)

	// The even split is over the bloodlines in a default population. An off-by-default bloodline
	// loads at 0% and sits out, so the twelve empire shares still read 8.3.
	totalCombos, evenShares := 0, 0
	for _, s := range stats {
		totalCombos += s.Combos
		if !s.OffByDefault {
			evenShares++
		}
	}

	capStatus, err := m.popStatus(ctx)
	if err != nil {
		espError(w, "rolebot-seed", err.Error())
		return
	}

	// The migration card. Its subject box is ?migrate=, the parameter a Migrate link on
	// Generated Characters sets and a refused migration bounces back through.
	targets, err := m.listMigrationTargets(ctx)
	if err != nil {
		espError(w, "rolebot-seed", err.Error())
		return
	}
	_, mixSource, _ := m.lastGenSpec(ctx)

	espRender(w, "rolebots_seed.html", map[string]any{
		"Page":           "rolebots",
		"Tab":            "gen",
		"Targets":        targets,
		"MigrateSubject": strings.TrimSpace(r.URL.Query().Get("migrate")),
		"MixSource":      mixSource,
		"Pop":            pop,
		"Cap":            capStatus,
		"Proj":           projStatus(),
		"Preload":        preloadStatus(),
		"Run":            currentRun.snapshot(),
		"Count":          count,
		"NextSeq":        nextSeq,
		"Prefix":         botAccountPrefix,
		"MaxBatch":       m.maxSeedBatch(ctx),
		"MaxLimit":       maxBatchLimit,
		"PerAccount":     charsPerAccount,
		"Archetypes":     archetypes,
		"Dispositions":   dispositions,
		"Maturities":     maturities,
		"TZWindows":      tzWindowOpts,
		"TZCover":        tzWindowCover(tzSettings()),
		"TZClock":        clock.Now().In(time.Local).Format("15:04 MST"),
		"TZ":             tzSettings(),
		"TZHourLabels":   tzHourLabels(),
		"RaceOrder":      raceOrder,
		"ByRace":         byRace,
		"Combos":         totalCombos,
		"PoseCount":      len(poseBank),
		// The even split, to one decimal, so the form and the "Even split"
		// button agree without the template doing arithmetic.
		"EvenBloodline": evenPct(evenShares),
		"WipeConfirm":   wipeAllConfirm,
		"Msg":           r.URL.Query().Get("msg"),
		"Err":           r.URL.Query().Get("err"),
	})
}

// formPercents reads `prefix<key>` percentage inputs into a ratio list. A blank or unparseable
// box is 0, which excludes the option. Values are stored in tenths of a percent (an even split
// over twelve is 8.3). A set that does not sum to 100 is not refused: weights are relative, so
// the page reports what it normalised to.
func formPercents(r *http.Request, prefix string, keys []string) []ratio {
	out := make([]ratio, 0, len(keys))
	for _, k := range keys {
		v, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue(prefix+k)), 64)
		if v < 0 {
			v = 0
		}
		out = append(out, ratio{Key: k, Weight: int(math.Round(v * 10))})
	}
	return out
}

// pctSum totals a percentage mix back into whole percent, for the flash
// message that tells an operator what their numbers actually came to.
func pctSum(rs []ratio) float64 {
	t := 0
	for _, r := range rs {
		if r.Weight > 0 {
			t += r.Weight
		}
	}
	return float64(t) / 10
}

// evenPct is the share each of n options gets in an even split, to one
// decimal. Twelve bloodlines is 8.3 each.
func evenPct(n int) string {
	if n <= 0 {
		return "0"
	}
	return strconv.FormatFloat(math.Round(1000/float64(n))/10, 'f', 1, 64)
}

// migrateBack is where a refused migration lands: the page it came from with the subject box
// still holding what the operator typed. ?migrate= is always the contents of that box.
func migrateBack(r *http.Request) string {
	subj := strings.TrimSpace(r.FormValue("subject"))
	if subj == "" {
		return "/rolebots/seed"
	}
	return "/rolebots/seed?migrate=" + url.QueryEscape(subj)
}

func (m *Mod) handleSeedPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	switch r.FormValue("action") {

	case "generate":
		spec, err := m.specFromForm(ctx, r)
		if err != nil {
			redirect(w, r, "/rolebots/seed", "", err.Error())
			return
		}
		// Remember the count so the form comes back as left. This page is the only place it is set.
		_ = evedb.SetServerSetting(ctx, m.db, settingGenTarget, strconv.Itoa(spec.Count))
		// And the whole mix, which a replacement rolls from when a Capsuleer is handed to a player
		// account. Soft: failing to remember it never stops a confirmed run.
		if err := m.saveGenSpec(ctx, spec); err != nil {
			rlogf("rolebots: could not store the generation mix: %v", err)
		}
		if err := m.StartGeneration(spec, m.maxSeedBatch(ctx)); err != nil {
			redirect(w, r, "/rolebots/seed", "", err.Error())
			return
		}
		msg := fmt.Sprintf("seeding %d Capsuleer(s) on %d account(s) — this page follows the run",
			spec.Count, accountsFor(spec.Count))
		// Say so when a mix was normalised: shares totalling 80% are read as proportions of 80,
		// not as 20% of nothing.
		if s := pctSum(spec.Bloodlines); s > 0 && math.Abs(s-100) > 0.5 {
			msg += fmt.Sprintf(" (bloodline shares totalled %.1f%% and were scaled to 100%%)", s)
		}
		redirect(w, r, "/rolebots/seed", msg, "")

	case "popcap":
		// The cap is stored and cached; nothing is logged off here. The sim pass owns that decision
		// (popcap.go) because it has the live player count and every pilot's state.
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("maxOnline")))
		if err != nil || n < 0 {
			redirect(w, r, "/rolebots/seed", "", "the online cap must be a number, 0 (unlimited) or more")
			return
		}
		if err := evedb.SetServerSetting(ctx, m.db, settingMaxOnline, strconv.Itoa(n)); err != nil {
			redirect(w, r, "/rolebots/seed", "", err.Error())
			return
		}
		setMaxOnline(n)
		if n == 0 {
			redirect(w, r, "/rolebots/seed", "online cap cleared — every active rolebot comes back on", "")
			return
		}
		redirect(w, r, "/rolebots/seed", fmt.Sprintf(
			"online cap set to %d Capsuleer(s) — bots yield seats to players as they connect", n), "")

	case "projcap":
		// The ceiling on projections: how many pilots may hold a ball in space at once, separate
		// from the online cap above. Stored and cached; the reconcile (projcap.go) owns withdrawal.
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("maxProjections")))
		if err != nil || n < 0 {
			redirect(w, r, "/rolebots/seed", "", "the projection cap must be a number, 0 (unlimited) or more")
			return
		}
		if err := evedb.SetServerSetting(ctx, m.db, settingMaxProjections, strconv.Itoa(n)); err != nil {
			redirect(w, r, "/rolebots/seed", "", err.Error())
			return
		}
		setMaxProjections(n)
		if n == 0 {
			redirect(w, r, "/rolebots/seed", "projection cap cleared — every watched grid fills as far as it wants", "")
			return
		}
		redirect(w, r, "/rolebots/seed", fmt.Sprintf(
			"projection cap set to %d ball(s) in space — grids a player is watching keep their seats first", n), "")

	case "preloadring":
		// The ring switch (preload.go) is a checkbox: the ring reaches one gate jump, which is not an
		// operator decision. The projection cap above is the dial for how much it costs.
		on := strings.TrimSpace(r.FormValue("preloadRing")) == "1"
		if on && !modapi.CanLoadSystems() {
			redirect(w, r, "/rolebots/seed", "", "this server has no system-loading seam — the ring cannot be switched on")
			return
		}
		v := "0"
		if on {
			v = "1"
		}
		if err := evedb.SetServerSetting(ctx, m.db, settingPreloadRing, v); err != nil {
			redirect(w, r, "/rolebots/seed", "", err.Error())
			return
		}
		setPreloadRing(on)
		if !on {
			// Nothing is released here. The sweep holds a system for preloadMinHoldSec after it leaves the
			// ring, so pinned systems go on the following passes, not in one frame.
			redirect(w, r, "/rolebots/seed", "ring preload off — pinned systems are released as they cool", "")
			return
		}
		redirect(w, r, "/rolebots/seed", "ring preload on — systems one gate from a player load and fill ahead of them", "")

	case "preloaddocked":
		// The docked pin (preloaddocked.go). Default on; this is the way off.
		on := strings.TrimSpace(r.FormValue("preloadDocked")) == "1"
		if on && !modapi.CanLoadSystems() {
			redirect(w, r, "/rolebots/seed", "", "this server has no system-loading seam — the docked pin cannot be switched on")
			return
		}
		v := "0"
		if on {
			v = "1"
		}
		if err := evedb.SetServerSetting(ctx, m.db, settingPreloadDocked, v); err != nil {
			redirect(w, r, "/rolebots/seed", "", err.Error())
			return
		}
		setPreloadDocked(on)
		if !on {
			redirect(w, r, "/rolebots/seed", "docked pin off — a system is torn down when its last pilot docks, and rebuilt from parked rows on undock", "")
			return
		}
		redirect(w, r, "/rolebots/seed", "docked pin on — the system a pilot is docked in keeps flying its bots", "")

	case "runlimit":
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("maxBatch")))
		if err != nil || n <= 0 {
			redirect(w, r, "/rolebots/seed", "", "run limit must be a positive number of Capsuleers")
			return
		}
		if n > maxBatchLimit {
			redirect(w, r, "/rolebots/seed", "", fmt.Sprintf(
				"%d is above the hard ceiling of %d Capsuleers per run — split the seed into two runs", n, maxBatchLimit))
			return
		}
		if err := evedb.SetServerSetting(ctx, m.db, settingMaxSeedBatch, strconv.Itoa(n)); err != nil {
			redirect(w, r, "/rolebots/seed", "", err.Error())
			return
		}
		redirect(w, r, "/rolebots/seed", fmt.Sprintf("run limit set to %d Capsuleers", n), "")

	case "wipeall":
		// The typed confirmation is the gate: the purge is irreversible and takes characters with
		// real assets with it, so a stray Enter cannot trigger it.
		got := strings.ToUpper(strings.Join(strings.Fields(r.FormValue("confirm")), " "))
		if got != wipeAllConfirm {
			redirect(w, r, "/rolebots/seed", "", fmt.Sprintf(
				"not confirmed — type %s exactly into the confirmation box", wipeAllConfirm))
			return
		}
		if err := m.StartWipeAll(); err != nil {
			redirect(w, r, "/rolebots/seed", "", err.Error())
			return
		}
		redirect(w, r, "/rolebots/seed",
			"purging every generated Capsuleer — this page follows the run", "")

	case "fixportraits":
		fixed, err := m.repairBrokenAppearance(ctx)
		if err != nil {
			redirect(w, r, "/rolebots/seed", "", err.Error())
			return
		}
		if len(fixed) == 0 {
			redirect(w, r, "/rolebots/seed", "no generated character is wearing an option the client cannot draw", "")
			return
		}
		redirect(w, r, "/rolebots/seed", fmt.Sprintf(
			"repaired %d character(s): %s — the client caches portraits it has already drawn, so drop the stored render on each character page to see the change",
			len(fixed), strings.Join(fixed, ", ")), "")

	case "migrate":
		// Handing a Capsuleer to a player account. Two lookups first, so a
		// mistyped name or a full account is refused with nothing written.
		cand, err := m.resolveMigrationSubject(ctx, r.FormValue("subject"))
		if err != nil {
			redirect(w, r, migrateBack(r), "", err.Error())
			return
		}
		acctID, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("accountID")), 10, 64)
		if acctID == 0 {
			redirect(w, r, migrateBack(r), "", "pick the account to hand "+cand.CharacterName+" to")
			return
		}
		res, err := m.MigrateToPlayerAccount(ctx, cand.CharacterID, acctID)
		if err != nil {
			redirect(w, r, migrateBack(r), "", err.Error())
			return
		}
		// The move is done and stays done. A replacement that will not roll is
		// reported as a second sentence, never as a failed migration.
		msg := fmt.Sprintf("%s now belongs to %s, docked at %s",
			res.CharacterName, res.AccountName, res.HomeName)
		if res.CorpName != "" {
			msg += fmt.Sprintf(", still employed by %s", res.CorpName)
		}
		msg += ". It is no longer a rolebot — cast it from Main Cast if you want it flown"
		if res.LeftBehindAt != "" {
			msg += fmt.Sprintf("; its ships and modules are still at %s", res.LeftBehindAt)
		}
		msg += "."
		if res.FromEmptied {
			msg += fmt.Sprintf(" %s held nobody else and was removed, so no replacement was rolled.", res.FromAccount)
		} else if err := m.rollReplacement(ctx, res.Seq, res.Slot, res.FromAccountID, res.FromAccount); err != nil {
			msg += " No replacement was rolled: " + err.Error()
		} else {
			msg += fmt.Sprintf(" Rolling a replacement into %s.", res.Replacing)
		}
		redirect(w, r, "/rolebots/seed", msg, "")

	case "enableall", "disableall":
		on := r.FormValue("action") == "enableall"
		ids, err := int64Column(ctx, m, `SELECT characterID FROM rolebot_generated`)
		if err != nil {
			redirect(w, r, "/rolebots/seed", "", err.Error())
			return
		}
		n := 0
		for _, id := range ids {
			if err := setRolebotEnabled(ctx, m.db, id, on); err == nil {
				n++
			}
		}
		m.syncPresence()
		m.reconcileCEOSeats(ctx)
		verb := "deactivated"
		if on {
			verb = "activated"
		}
		redirect(w, r, "/rolebots/seed", fmt.Sprintf("%d generated Capsuleer(s) %s", n, verb), "")

	default:
		redirect(w, r, "/rolebots/seed", "", "unknown action")
	}
}

// specFromForm reads the whole seed form. Validation happens here rather than
// inside the generator so a bad mix is refused with the form still on screen,
// before a single account exists.
func (m *Mod) specFromForm(ctx context.Context, r *http.Request) (GenSpec, error) {
	var spec GenSpec

	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("count")))
	if err != nil || n <= 0 {
		return spec, fmt.Errorf("how many characters? (got %q)", r.FormValue("count"))
	}
	spec.Count = n

	spec.Archetypes = formPercents(r, "arch_", archetypeKeys())
	spec.Dispositions = formPercents(r, "law_", dispositionKeys())
	spec.Maturities = formPercents(r, "mat_", maturityKeys())
	spec.Timezones = formPercents(r, "tz_", tzWindowKeys())

	blKeys := make([]string, 0, 12)
	for _, bl := range nameableBloodlines() {
		blKeys = append(blKeys, strconv.FormatInt(bl, 10))
	}
	spec.Bloodlines = formPercents(r, "bl_", blKeys)

	// A mix with every share cleared spreads evenly (allocate). Only the bloodline axis is checked:
	// clearing it by accident would spread evenly over all twelve and look like the form was ignored.
	any := false
	for _, rt := range spec.Bloodlines {
		if rt.Weight > 0 {
			any = true
			break
		}
	}
	if !any {
		return spec, fmt.Errorf("every bloodline share is zero — give at least one a percentage, or press Even split")
	}

	spec.GenderFemalePct, _ = strconv.Atoi(strings.TrimSpace(r.FormValue("femalePct")))
	if spec.GenderFemalePct < 0 {
		spec.GenderFemalePct = 0
	}
	if spec.GenderFemalePct > 100 {
		spec.GenderFemalePct = 100
	}

	spec.WalletMin, _ = strconv.ParseFloat(strings.TrimSpace(r.FormValue("walletMin")), 64)
	spec.WalletMax, _ = strconv.ParseFloat(strings.TrimSpace(r.FormValue("walletMax")), 64)
	if spec.WalletMin < 0 {
		spec.WalletMin = 0
	}
	if spec.WalletMax < spec.WalletMin {
		spec.WalletMax = spec.WalletMin
	}

	// Name headroom, checked before anything is created: 400 pilots from a bloodline with 200
	// combinations would spend the last pilots on middle-initial fallbacks.
	stats, err := namePoolStats(ctx, m.db)
	if err == nil {
		totalW := 0
		for _, rt := range spec.Bloodlines {
			totalW += rt.Weight
		}
		byID := map[int64]poolStat{}
		for _, s := range stats {
			byID[s.BloodlineID] = s
		}
		for _, rt := range spec.Bloodlines {
			if rt.Weight <= 0 {
				continue
			}
			var id int64
			fmt.Sscan(rt.Key, &id)
			s, ok := byID[id]
			if !ok {
				continue
			}
			expect := spec.Count * rt.Weight / totalW
			if expect > s.Combos {
				return spec, fmt.Errorf(
					"%s would need about %d names but its pool only makes %d — lower the weight or add names to namedata.go",
					s.BloodlineName, expect, s.Combos)
			}
		}
	}

	return spec, nil
}

// handleGenRosterPost handles the per-row actions on the population list: activate, deactivate
// or purge one pilot. Purge runs the same total purge as a range wipe, on a range of one, so
// there is one deletion path.
func (m *Mod) handleGenRosterPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	back := "/rolebots/generated"
	if q := r.FormValue("back"); q != "" {
		back += "?" + q
	}
	charID, _ := strconv.ParseInt(r.FormValue("charID"), 10, 64)
	if charID == 0 {
		redirect(w, r, back, "", "invalid character ID")
		return
	}

	switch r.FormValue("action") {
	case "enable", "disable":
		on := r.FormValue("action") == "enable"
		if err := setRolebotEnabled(ctx, m.db, charID, on); err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		m.syncPresence()
		m.syncCEOSeat(ctx, charID)
		if on {
			redirect(w, r, back, fmt.Sprintf("Capsuleer %d activated", charID), "")
		} else {
			redirect(w, r, back, fmt.Sprintf("Capsuleer %d deactivated", charID), "")
		}
	case "purge":
		// One character, not one account. The account keeps its other
		// characters and is removed only if this was the last of them.
		var name string
		_ = m.db.QueryRowContext(ctx,
			`SELECT characterName FROM character_ WHERE characterID = ?`, charID).Scan(&name)
		if err := m.PurgeOneCharacter(ctx, charID); err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		if name == "" {
			name = strconv.FormatInt(charID, 10)
		}
		redirect(w, r, back, name+" deleted, with everything it owned", "")
	default:
		redirect(w, r, back, "", "unknown action")
	}
}

// genRow is one line of the population list. It is package-level because the render test builds
// one: a row type local to the handler cannot be executed by a test.
type genRow struct {
	Seq         int
	AccountName string
	CharacterID int64
	Name        string
	Archetype   string
	// Standing is the row's Lawfulness column. A row with no disposition axis shows the security
	// band it was generated with, labelled as such.
	Standing   string
	Maturity   string
	PoseSource string
	Bloodline  string
	Gender     string
	Enabled    bool
	// Offline is the online cap holding this pilot off the server (popcap.go): enabled but not
	// connected. It stops the list reporting a busy activity for a pilot taking no turns.
	Offline  bool
	Activity string
	Phase    string
	Halted   bool
	Where    string
	Balance  float64
	Created  string
	Search   string
}

// handleGenRoster serves /rolebots/generated, the paged population list.
func (m *Mod) handleGenRoster(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		m.handleGenRosterPost(w, r, ctx)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	arch := strings.TrimSpace(r.URL.Query().Get("arch"))

	where := "1=1"
	var args []any
	if q != "" {
		where += ` AND (c.characterName LIKE CONCAT('%', ?, '%')
		            OR g.accountName LIKE CONCAT('%', ?, '%')
		            OR CAST(g.seq AS CHAR) = ?)`
		args = append(args, q, q, q)
	}
	if arch != "" {
		where += " AND g.archetype = ?"
		args = append(args, arch)
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT g.seq, g.accountName, g.characterID, COALESCE(c.characterName, '(deleted)'),
		       g.archetype, g.lawfulness, g.band, g.maturity, g.poseSource,
		       COALESCE(bl.bloodlineName, ''), g.gender,
		       COALESCE(b.enabled, 0), COALESCE(s.activity, ''), COALESCE(s.phase, ''),
		       COALESCE(s.halted, 0),
		       COALESCE(st.stationName, ''), COALESCE(ms.solarSystemName, ''),
		       COALESCE(c.balance, 0), g.createdDate
		FROM rolebot_generated g
		LEFT JOIN character_ c ON c.characterID = g.characterID
		LEFT JOIN chrBloodlines bl ON bl.bloodlineID = g.bloodlineID
		LEFT JOIN rolebot_characters b ON b.characterID = g.characterID
		LEFT JOIN rolebot_state s ON s.characterID = g.characterID
		LEFT JOIN staStations st ON st.stationID = c.stationID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = c.solarSystemID
		WHERE `+where+`
		ORDER BY g.seq
		LIMIT 1000`, args...)
	if err != nil {
		espError(w, "rolebot-gen", err.Error())
		return
	}
	defer rows.Close()

	archLabel := map[string]string{}
	for _, a := range archetypes {
		archLabel[a.Key] = a.Label
	}
	for _, a := range legacyArchetypes {
		archLabel[a.Key] = a.Label
	}
	dispLabel := map[string]string{}
	for _, d := range dispositions {
		dispLabel[d.Key] = d.Label
	}
	// The retired security bands, so a row generated before the disposition
	// axis still reads as something rather than as an empty cell.
	bandLabel := map[string]string{
		"high": "Highsec (retired)",
		"low":  "Lowsec (retired)",
		"null": "Nullsec (retired)",
	}
	matLabel := map[string]string{}
	for _, mt := range maturities {
		matLabel[mt.Key] = mt.Label
	}

	var out []genRow
	for rows.Next() {
		var g genRow
		var gender, enabled, halted int64
		var station, system string
		var created int64
		var law, band string
		if err := rows.Scan(&g.Seq, &g.AccountName, &g.CharacterID, &g.Name,
			&g.Archetype, &law, &band, &g.Maturity, &g.PoseSource, &g.Bloodline, &gender,
			&enabled, &g.Activity, &g.Phase, &halted, &station, &system,
			&g.Balance, &created); err != nil {
			espError(w, "rolebot-gen", err.Error())
			return
		}
		g.Gender = "male"
		if gender == genderFemale {
			g.Gender = "female"
		}
		g.Enabled, g.Halted = enabled != 0, halted != 0
		g.Offline = g.Enabled && popIsOffline(g.CharacterID)
		if station != "" {
			g.Where = "Docked — " + station
		} else if system != "" {
			g.Where = "In space — " + system
		}
		if created > 0 {
			g.Created = time.Unix(created, 0).Format("2006-01-02 15:04")
		}
		if l, ok := archLabel[g.Archetype]; ok {
			g.Archetype = l
		}
		switch {
		case law != "":
			g.Standing = dispLabel[law]
			if g.Standing == "" {
				g.Standing = law
			}
		case band != "":
			g.Standing = bandLabel[band]
			if g.Standing == "" {
				g.Standing = band
			}
		}
		if l, ok := matLabel[g.Maturity]; ok {
			g.Maturity = l
		}
		g.Search = strings.ToLower(strings.Join([]string{
			g.Name, g.AccountName, g.Archetype, g.Bloodline, g.Standing, g.Maturity,
		}, " "))
		out = append(out, g)
	}

	pop, _ := m.generatedPopulation(ctx)
	espRender(w, "rolebots_generated.html", map[string]any{
		"Page":       "rolebots",
		"Tab":        "pop",
		"Rows":       out,
		"Pop":        pop,
		"Q":          q,
		"Arch":       arch,
		"Archetypes": archetypes,
		"Run":        currentRun.snapshot(),
		"Msg":        r.URL.Query().Get("msg"),
		"Err":        r.URL.Query().Get("err"),
	})
}
