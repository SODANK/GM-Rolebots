package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Postings: which of its offices a corporation's member lives and works at. Every member row carries
// a Base (chrCorporationRoles.baseID), which the corp window's Edit Member offers from the
// corporation's offices and which scopes the rolesAtBase mask.
// For a member of a player corporation the Base is where its clone lives (corphome.go planPlayerCorpHQ,
// clone.go cloneUpkeep). Its work address follows the Base office's purpose: industry builds at the
// factory, R&D researches at the laboratory, a miner refines at the mining office (postedWorkStation).
// A bot CEO sets Bases from the job each arm does (corparm.go), falling back to the headquarters when
// the office is not held. A posting stands at least postingMinSec unless the office is lost or the arm
// changes. A Base a human chose is final until its office goes. First postings are spread over
// postingSpreadSec. Human members are posted by the same rule and told by letter (corpdesk.go).

const (
	// postingMinSec is the least time a posting stands.
	postingMinSec = 7 * 86400
	// postingSpreadSec spreads the roster's first postings.
	postingSpreadSec = 24 * 3600
	// settingPostingsSince is when the posting rule first ran (unix), the
	// anchor of the first-posting spread.
	settingPostingsSince = "rolebots.corp.postingsSince"

	postSetCEO    = "ceo"
	postSetHuman  = "human"
	postSetMember = "member"
)

// armPurpose is the office purpose an arm's job is done at. Pure.
func armPurpose(a corpArm) int {
	switch a {
	case armIndustry:
		return officeFactory
	case armResearch:
		return officeLab
	case armMining:
		return officeMining
	case armSecurity:
		return officeStaging
	case armSales:
		return officeSales
	case armInfrastructure:
		// Tower supplies stage at the Security staging office or the HQ;
		// the arm lives where its legs start.
		return officeHQ
	}
	return officeHQ
}

// postOffice is one office as the posting pass sees it.
type postOffice struct {
	StationID int64
	SystemID  int64
	Purposes  int
	Dropping  bool
}

// postMember is one member as the posting pass sees it.
type postMember struct {
	CharID    int64
	Human     bool
	Arm       corpArm
	Base      int64 // chrCorporationRoles.baseID, 0 for none
	HomeSys   int64 // where it lives now, for "nearest"
	SetBy     string
	PostedArm corpArm
	PostedAt  int64
	DueAt     int64 // its first posting may not come before this
}

// posting is one decision: post this member to this office.
type posting struct {
	CharID    int64
	StationID int64
	Arm       corpArm
	Purpose   int
	Why       string
}

// postingTarget is the purpose a member should be posted for, and the
// offices that serve it. Falls back: a laboratory to the factory, anything
// to the headquarters. Pure.
func postingTarget(arm corpArm, offices []postOffice) (int, []postOffice) {
	serving := func(bit int) []postOffice {
		var out []postOffice
		for _, o := range offices {
			if !o.Dropping && o.Purposes&bit != 0 {
				out = append(out, o)
			}
		}
		return out
	}
	want := armPurpose(arm)
	// A front or a capital the arm stages to (corpfront.go) serves it beside its own office; the
	// quota (postingQuotas) decides how many go there.
	stage := serving(stageBits(arm))
	for _, bit := range []int{want, fallbackPurpose(want), officeHQ} {
		if bit == 0 {
			continue
		}
		if list := serving(bit); len(list) > 0 {
			return bit, mergeOffices(list, stage)
		}
	}
	if len(stage) > 0 {
		return want, stage
	}
	return 0, nil
}

// mergeOffices is a with b's offices added, each once. Pure.
func mergeOffices(a, b []postOffice) []postOffice {
	if len(b) == 0 {
		return a
	}
	out := append([]postOffice(nil), a...)
	seen := map[int64]bool{}
	for _, o := range a {
		seen[o.StationID] = true
	}
	for _, o := range b {
		if !seen[o.StationID] {
			seen[o.StationID] = true
			out = append(out, o)
		}
	}
	return out
}

// servesArm reports whether an office with these purposes takes an arm's members: its own purpose,
// or a front or capital the arm stages to. Pure.
func servesArm(purposes int, arm corpArm, purpose int) bool {
	return purposes&(purpose|stageBits(arm)) != 0
}

// fallbackPurpose is where an arm goes when its own office is missing,
// before the headquarters. Pure.
func fallbackPurpose(bit int) int {
	if bit == officeLab {
		return officeFactory
	}
	return 0
}

// planPostings decides who moves. dist answers jumps from a member's system
// to an office's (-1 for unreachable). Pure.
func planPostings(members []postMember, offices []postOffice, dist func(fromSys, toSys int64) int, now int64) []posting {
	return planPostingsQuota(members, offices, dist, now, nil)
}

// armQuota splits a department's n members over its bases by weight, largest
// remainder first; while there are members enough, no weighted base is left
// with nobody. Pure.
func armQuota(n int, weights map[int64]int) map[int64]int {
	total := 0
	var sts []int64
	for st, w := range weights {
		if w > 0 {
			total += w
			sts = append(sts, st)
		}
	}
	out := map[int64]int{}
	if total == 0 || n <= 0 {
		return out
	}
	sort.Slice(sts, func(i, j int) bool { return sts[i] < sts[j] })
	type rem struct {
		st int64
		r  float64
	}
	var rems []rem
	given := 0
	for _, st := range sts {
		exact := float64(n) * float64(weights[st]) / float64(total)
		q := int(exact)
		out[st] = q
		given += q
		rems = append(rems, rem{st, exact - float64(q)})
	}
	sort.SliceStable(rems, func(i, j int) bool { return rems[i].r > rems[j].r })
	for i := 0; given < n; i++ {
		out[rems[i%len(rems)].st]++
		given++
	}
	if n >= len(sts) {
		for _, st := range sts {
			if out[st] > 0 {
				continue
			}
			var top int64
			for _, s2 := range sts {
				if top == 0 || out[s2] > out[top] {
					top = s2
				}
			}
			if out[top] > 1 {
				out[top]--
				out[st]++
			}
		}
	}
	return out
}

// planPostingsQuota is planPostings with a headcount per base for some
// departments (armQuota). A department with several bases is posted over
// them, and a member the CEO posted moves to a base still short of its share
// when its own base has more than its share. Pure.
func planPostingsQuota(members []postMember, offices []postOffice, dist func(fromSys, toSys int64) int, now int64,
	quota map[corpArm]map[int64]int) []posting {
	byStation := make(map[int64]postOffice, len(offices))
	for _, o := range offices {
		byStation[o.StationID] = o
	}
	load := map[int64]int{}
	armLoad := map[corpArm]map[int64]int{}
	for _, mb := range members {
		if o, ok := byStation[mb.Base]; ok && !o.Dropping {
			load[mb.Base]++
			if armLoad[mb.Arm] == nil {
				armLoad[mb.Arm] = map[int64]int{}
			}
			armLoad[mb.Arm][mb.Base]++
		}
	}
	// short is the serving offices still under the department's quota.
	short := func(arm corpArm, serving []postOffice) []postOffice {
		q := quota[arm]
		if len(q) == 0 {
			return nil
		}
		var out []postOffice
		for _, o := range serving {
			if armLoad[arm][o.StationID] < q[o.StationID] {
				out = append(out, o)
			}
		}
		return out
	}
	var out []posting
	for _, mb := range members {
		cur, valid := byStation[mb.Base]
		valid = valid && !cur.Dropping
		purpose, serving := postingTarget(mb.Arm, offices)
		if len(serving) == 0 {
			continue
		}
		if q := quota[mb.Arm]; len(q) > 0 {
			// A department with a quota is posted only to the bases it has
			// one for.
			var inQuota []postOffice
			for _, o := range serving {
				if _, ok := q[o.StationID]; ok {
					inQuota = append(inQuota, o)
				}
			}
			if len(inQuota) > 0 {
				serving = inQuota
			}
		}
		why := ""
		switch {
		case valid && (mb.SetBy == postSetHuman || mb.SetBy == postSetMember):
			continue // a human's choice stands while the office does
		case !valid && mb.Base != 0:
			why = "its office is going"
		case !valid:
			if now < mb.DueAt {
				continue
			}
			why = "its first posting"
		case mb.SetBy == postSetCEO && mb.PostedArm != mb.Arm:
			why = "a new job"
		case mb.SetBy == "":
			// A Base nobody here set (hand-set before postings existed): treat it as a human's choice.
			continue
		case mb.SetBy == postSetCEO && purpose == armPurpose(mb.Arm) && purpose != officeHQ &&
			!servesArm(cur.Purposes, mb.Arm, purpose):
			// The arm has an office of its own (just rented, or the job moved there). That is not churn:
			// postings run before the estate power, so without this a new office would sit empty for a week.
			why = "its job has an office of its own now"
		case now-mb.PostedAt >= postingMinSec && !servesArm(cur.Purposes, mb.Arm, purpose):
			why = "its job is done elsewhere"
		case mb.SetBy == postSetCEO && len(quota[mb.Arm]) > 0 && overQuota(quota[mb.Arm], armLoad[mb.Arm], mb.Base) &&
			len(short(mb.Arm, serving)) > 0:
			// The department has a base short of hands and this one has more
			// than its share: a new base is no use with nobody posted to it.
			why = "its department needs hands at another base"
		default:
			continue
		}
		cands := serving
		if s := short(mb.Arm, serving); len(s) > 0 {
			cands = s
		}
		best := pickPostOffice(mb, cands, dist, load)
		if best == 0 || best == mb.Base {
			continue
		}
		if valid {
			load[mb.Base]--
			if armLoad[mb.Arm] != nil {
				armLoad[mb.Arm][mb.Base]--
			}
		}
		load[best]++
		if armLoad[mb.Arm] == nil {
			armLoad[mb.Arm] = map[int64]int{}
		}
		armLoad[mb.Arm][best]++
		out = append(out, posting{CharID: mb.CharID, StationID: best, Arm: mb.Arm, Purpose: purpose, Why: why})
	}
	return out
}

// overQuota reports whether a base holds more of a department than its share.
// A base the quota does not name is over by definition. Pure.
func overQuota(q, load map[int64]int, station int64) bool {
	want, ok := q[station]
	return !ok || load[station] > want
}

// pickPostOffice is the nearest serving office to the member, ties to the
// one with fewer members, then the lowest station ID. Pure.
func pickPostOffice(mb postMember, serving []postOffice, dist func(fromSys, toSys int64) int, load map[int64]int) int64 {
	list := append([]postOffice(nil), serving...)
	d := func(o postOffice) int {
		if mb.HomeSys == 0 {
			return 0
		}
		if n := dist(mb.HomeSys, o.SystemID); n >= 0 {
			return n
		}
		return 1 << 20
	}
	sort.SliceStable(list, func(i, j int) bool {
		di, dj := d(list[i]), d(list[j])
		if di != dj {
			return di < dj
		}
		if load[list[i].StationID] != load[list[j].StationID] {
			return load[list[i].StationID] < load[list[j].StationID]
		}
		return list[i].StationID < list[j].StationID
	})
	return list[0].StationID
}

// postedWorkStationFor is where a posted member does its job: its own Base when that office serves the
// department, else postedWorkStation's pick (the factory for industry; the laboratory, else the
// factory, for R&D; the mining office, else the factory, for refining; 0 for other arms). Pure.
func postedWorkStationFor(arm corpArm, base int64, offices []postOffice) int64 {
	want := postedWorkStation(arm, offices)
	if want == 0 || base == 0 {
		return want
	}
	bit := armPurpose(arm)
	for _, o := range offices {
		if o.StationID == base && !o.Dropping && o.Purposes&bit != 0 {
			return base
		}
	}
	return want
}

func postedWorkStation(arm corpArm, offices []postOffice) int64 {
	first := func(bit int) int64 {
		var best int64
		for _, o := range offices {
			if !o.Dropping && o.Purposes&bit != 0 && (best == 0 || o.StationID < best) {
				best = o.StationID
			}
		}
		return best
	}
	switch arm {
	case armIndustry:
		return first(officeFactory)
	case armResearch:
		if s := first(officeLab); s != 0 {
			return s
		}
		return first(officeFactory)
	case armMining:
		if s := first(officeMining); s != 0 {
			return s
		}
		return first(officeFactory)
	}
	return 0
}

// postingOwnsFactory is whether the posting pass decides this pilot's
// factory: a member of a bot-run corporation while postings are on. Every
// other writer of the factory stands down for it (rememberIndustryBase).
func (m *Mod) postingOwnsFactory(b *botRun) bool {
	if b == nil || b.CorpID == 0 || !postingsOn() {
		return false
	}
	_, ok := managedCEO(b.CorpID)
	return ok
}

// loadPostOffices reads the corporation's offices with their purposes, plus
// the headquarters as an address even where no office stands under it.
func (m *Mod) loadPostOffices(ctx context.Context, corpID int64) ([]postOffice, error) {
	purposes, err := m.syncOfficePurposes(ctx, corpID)
	if err != nil {
		return nil, err
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.stationID, s.solarSystemID, r.state = 'dropping'
		  FROM rolebot_corp_offices r
		  JOIN staStations s ON s.stationID = r.stationID
		 WHERE r.corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("loadPostOffices: %w", err)
	}
	var out []postOffice
	seen := map[int64]bool{}
	for rows.Next() {
		var o postOffice
		if rows.Scan(&o.StationID, &o.SystemID, &o.Dropping) == nil {
			o.Purposes = purposes[o.StationID]
			out = append(out, o)
			seen[o.StationID] = true
		}
	}
	rows.Close()
	var hq, hqSys int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(c.stationID, 0), COALESCE(s.solarSystemID, 0)
		  FROM corporation c LEFT JOIN staStations s ON s.stationID = c.stationID
		 WHERE c.corporationID = ?`, corpID).Scan(&hq, &hqSys)
	if hq != 0 && !seen[hq] {
		out = append(out, postOffice{StationID: hq, SystemID: hqSys, Purposes: officeHQ})
	}
	return out, nil
}

// postingsSince is when the posting rule first ran; stored on the first
// call so a restart keeps the spread where it was.
func (m *Mod) postingsSince(ctx context.Context) int64 {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingPostingsSince); err == nil && ok {
		var n int64
		if _, err := fmt.Sscan(strings.TrimSpace(v), &n); err == nil && n > 0 {
			return n
		}
	}
	now := clock.Unix()
	if err := evedb.SetServerSetting(ctx, m.db, settingPostingsSince, fmt.Sprint(now)); err != nil {
		rlogf("rolebots: store %s: %v", settingPostingsSince, err)
	}
	return now
}

// loadPostMembers reads the members the pass may post: enabled bots, and
// humans while human postings are on.
func (m *Mod) loadPostMembers(ctx context.Context, corpID int64, since int64) ([]postMember, []corpMember, error) {
	cms, err := m.corpMembers(ctx, corpID, 0)
	if err != nil {
		return nil, nil, err
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID, COALESCE(r.baseID, 0), COALESCE(s.solarSystemID, 0),
		       COALESCE(p.setBy, ''), COALESCE(p.arm, ''), COALESCE(p.postedAt, 0)
		  FROM character_ c
		  LEFT JOIN chrCorporationRoles r ON r.characterID = c.characterID
		  LEFT JOIN staStations s ON s.stationID = c.homeStationID
		  LEFT JOIN rolebot_corp_postings p ON p.characterID = c.characterID AND p.corporationID = c.corporationID
		 WHERE c.corporationID = ?`, corpID)
	if err != nil {
		return nil, nil, fmt.Errorf("loadPostMembers: %w", err)
	}
	type extra struct {
		base, sys, at int64
		setBy, arm    string
	}
	ex := map[int64]extra{}
	for rows.Next() {
		var id int64
		var e extra
		if rows.Scan(&id, &e.base, &e.sys, &e.setBy, &e.arm, &e.at) == nil {
			ex[id] = e
		}
	}
	rows.Close()
	var out []postMember
	var kept []corpMember
	for _, cm := range cms {
		if cm.Human() && !humanPostingsOn() {
			continue
		}
		if !cm.Human() && !cm.Enabled {
			continue
		}
		e := ex[cm.CharID]
		seat, _ := armOf(cm.CharID, corpID)
		out = append(out, postMember{
			CharID: cm.CharID, Human: cm.Human(), Arm: seat.Arm, Base: e.base, HomeSys: e.sys,
			SetBy: e.setBy, PostedArm: corpArm(e.arm), PostedAt: e.at,
			DueAt: postingDueAt(since, cm),
		})
		kept = append(kept, cm)
	}
	return out, kept, nil
}

// postingDueAt is the earliest a member's first posting may come. The spread keeps a roster that
// predates the rule from flying out at once, so a new member and a human are posted on the next
// pass. Pure.
func postingDueAt(since int64, cm corpMember) int64 {
	if cm.Human() {
		return 0
	}
	if cm.Since > 0 && evedb.Win32ToUnix(cm.Since) > since {
		return 0
	}
	return since + cm.CharID%postingSpreadSec
}

// ceoPostings is the roles power's posting step: post who should move, then
// keep every bot's work address in step with its posting.
func (m *Mod) ceoPostings(ctx context.Context, c *corpCEO) error {
	if !postingsOn() {
		return nil
	}
	offices, err := m.loadPostOffices(ctx, c.CorpID)
	if err != nil || len(offices) == 0 {
		return err
	}
	members, cms, err := m.loadPostMembers(ctx, c.CorpID, m.postingsSince(ctx))
	if err != nil {
		return err
	}
	// Jumps from every office, one walk each: a handful per corporation.
	fromOffice := map[int64]map[int64]int{}
	for _, o := range offices {
		if _, ok := fromOffice[o.SystemID]; ok {
			continue
		}
		mp, err := m.corpJumpMap(ctx, o.SystemID)
		if err != nil {
			return err
		}
		fromOffice[o.SystemID] = mp
	}
	dist := func(fromSys, toSys int64) int {
		if d, ok := fromOffice[toSys][fromSys]; ok {
			return d
		}
		return -1
	}
	now := clock.Unix()
	plan := planPostingsQuota(members, offices, dist, now, m.postingQuotas(ctx, c.CorpID, members, offices))

	byChar := map[int64]corpMember{}
	for _, cm := range cms {
		byChar[cm.CharID] = cm
	}
	moved := map[int]int{}
	for _, p := range plan {
		cm := byChar[p.CharID]
		if err := m.writePosting(ctx, c.CorpID, p.CharID, p.StationID, p.Arm, postSetCEO); err != nil {
			m.logBot(c.CharID, logError, "ceo", "posting %d: %v", p.CharID, err)
			continue
		}
		moved[p.Purpose]++
		label := officePurposeLabel(purposeOf(offices, p.StationID))
		if cm.Human() {
			m.mailPostingLetter(ctx, c, cm, p, offices)
		} else {
			m.logBot(p.CharID, logInfo, "corp", "posted to %s, the %s (%s)",
				m.stationLabel(ctx, p.StationID), label, p.Why)
		}
	}
	if len(plan) > 0 {
		var parts []string
		for _, pn := range purposeNames {
			if n := moved[pn.bit]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d to the %s", n, pn.name))
			}
		}
		m.ceoLog(ctx, c, powerRoles, "basepost", 0, float64(len(plan)),
			"posted %d member(s): %s", len(plan), strings.Join(parts, ", "))
	}
	m.syncPostedWork(ctx, c.CorpID, members, offices)
	return nil
}

// postingQuotas is the headcount each department base should carry: R&D spread over its bases by the
// originals each holds, Industry by the production queued at each (corpbuildbases.go). A department
// with one base or none gets no quota and uses the nearest-office rule. A front or a capital
// (corpfront.go) overrides: stageQuotas.
func (m *Mod) postingQuotas(ctx context.Context, corpID int64, members []postMember, offices []postOffice) map[corpArm]map[int64]int {
	out := m.departmentQuotas(ctx, corpID, members)
	front := m.loadCorpFront(ctx, corpID)
	return stageQuotas(out, members, offices, front.Since, clock.Unix())
}

// departmentQuotas is postingQuotas before a front or capital is laid over it.
func (m *Mod) departmentQuotas(ctx context.Context, corpID int64, members []postMember) map[corpArm]map[int64]int {
	out := map[corpArm]map[int64]int{}
	count := map[corpArm]int{}
	for _, mb := range members {
		count[mb.Arm]++
	}
	if labs := m.armBaseStations(ctx, corpID, armResearch); len(labs) > 1 {
		w := map[int64]int{}
		for _, st := range labs {
			ps, _ := m.libraryPrints(ctx, corpID, st)
			w[st] = max(1, len(ps))
		}
		out[armResearch] = armQuota(count[armResearch], w)
	}
	if facs := m.industryBaseStations(ctx, corpID); len(facs) > 1 {
		open, _ := m.openWorkFor(ctx, corpID)
		out[armIndustry] = armQuota(count[armIndustry], buildBaseWeights(m.buildBases(ctx, corpID, open)))
	}
	return out
}

// purposeOf is an office's purposes in the list.
func purposeOf(offices []postOffice, stationID int64) int {
	for _, o := range offices {
		if o.StationID == stationID {
			return o.Purposes
		}
	}
	return 0
}

// writePosting sets a member's Base and records who set it. The member's
// session hears about it (MemberRolesChanged pushes baseID), and its address
// is looked at again on its next plan.
func (m *Mod) writePosting(ctx context.Context, corpID, charID, stationID int64, arm corpArm, setBy string) error {
	old, _ := evedb.GetCorporationMemberInfo(ctx, m.db, charID)
	base := stationID
	if err := evedb.UpdateCorpMemberRow(ctx, m.db, charID, &evedb.CorpMemberUpdate{BaseID: &base}); err != nil {
		return err
	}
	if h := modapi.Corp().MemberRolesChanged; h != nil {
		h(charID, old)
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_postings (characterID, corporationID, stationID, setBy, arm, postedAt)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), stationID = VALUES(stationID),
			setBy = VALUES(setBy), arm = VALUES(arm), postedAt = VALUES(postedAt)`,
		charID, corpID, stationID, setBy, string(arm), clock.Unix()); err != nil {
		return fmt.Errorf("record posting: %w", err)
	}
	forgetCorpHome(charID)
	forgetCloneCheck(charID)
	return nil
}

// syncPostedWork keeps every bot member's factory equal to the one its
// posting implies: its office's workplace for industry, R&D and mining, none
// for anyone else. One read for the whole corporation; a write only where it
// differs.
func (m *Mod) syncPostedWork(ctx context.Context, corpID int64, members []postMember, offices []postOffice) {
	if len(members) == 0 {
		return
	}
	ids := make([]any, 0, len(members))
	for _, mb := range members {
		if !mb.Human {
			ids = append(ids, mb.CharID)
		}
	}
	if len(ids) == 0 {
		return
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, industryStationID FROM rolebot_profiles
		 WHERE characterID IN (`+placeholders(len(ids))+`)`, ids...)
	if err != nil {
		return
	}
	have := map[int64]int64{}
	for rows.Next() {
		var id, st int64
		if rows.Scan(&id, &st) == nil {
			have[id] = st
		}
	}
	rows.Close()
	for _, mb := range members {
		if mb.Human {
			continue
		}
		want := postedWorkStationFor(mb.Arm, mb.Base, offices)
		cur, found := have[mb.CharID]
		if (found && cur == want) || (!found && want == 0) {
			continue
		}
		if err := setIndustryBaseByID(ctx, m.db, mb.CharID, want); err != nil {
			rlogf("rolebots: posted work %d: %v", mb.CharID, err)
		}
	}
}

// memberBase is where a member of a player corporation lives: its Base while that is one of the
// corporation's offices (or the HQ), else the headquarters. With postings off it is the headquarters.
func (m *Mod) memberBase(ctx context.Context, charID, corpID, hq int64) int64 {
	if !postingsOn() {
		return hq
	}
	var base int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(baseID, 0) FROM chrCorporationRoles WHERE characterID = ?`, charID).Scan(&base); err != nil {
		if err != sql.ErrNoRows {
			return hq
		}
	}
	if base == 0 || base == hq || !evedb.IsCorpBaseStation(ctx, m.db, corpID, base) {
		return hq
	}
	return base
}

// clearPinsMarker marks the one-shot release of the old operator address
// pins (rolebot_profiles.homePinned).
const clearPinsMarker = "_unpin1015"

// migrateClearHomePins releases every old address pin, once. Nothing live
// sets one any more (pinPilotAddress has no caller), and a pin would hold a
// pilot out of its corporation's postings; the operator chose to clear them.
func migrateClearHomePins(ctx context.Context, db *sql.DB) error {
	if done, err := traitMarkerSet(ctx, db, clearPinsMarker); err != nil {
		return fmt.Errorf("migrateClearHomePins probe: %w", err)
	} else if done {
		return nil
	}
	res, err := db.ExecContext(ctx, `UPDATE rolebot_profiles SET homePinned = 0 WHERE homePinned <> 0`)
	if err != nil {
		return fmt.Errorf("migrateClearHomePins: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		rlogf("rolebots: released %d old address pin(s) — corporation postings decide where they live now", n)
	}
	return setTraitMarker(ctx, db, clearPinsMarker)
}

// postedCloneStation is where a member of a player corporation keeps its clone while postings are on:
// its Base when that has a medical bay, else the nearest bay (corpHomeTargets, the answer
// planPlayerCorpHQ moves it to). ok is false for everyone else. It runs only on clone upkeep's slow path.
func (m *Mod) postedCloneStation(ctx context.Context, b *botRun) (stationID, systemID int64, ok bool) {
	if !postingsOn() || b == nil || b.CorpID == 0 || (b.Profile != nil && b.Profile.HomePinned) {
		return 0, 0, false
	}
	facts, err := getAnyCorpFacts(ctx, m.db, b.CorpID)
	if err != nil || facts == nil || !facts.Player || corpIsSchool(ctx, m.db, b.CorpID) {
		return 0, 0, false
	}
	var hq int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(stationID, 0) FROM corporation WHERE corporationID = ?`, b.CorpID).Scan(&hq)
	base := m.memberBase(ctx, b.CharID, b.CorpID, hq)
	if base == 0 {
		return 0, 0, false
	}
	clone, _, err := m.corpHomeTargets(ctx, base)
	if err != nil || clone == 0 {
		return 0, 0, false
	}
	_, sys := m.stationOwnerAndSystem(ctx, clone)
	return clone, sys, sys != 0
}

// onMemberBaseChanged is esp.MemberBaseChangedHook: a Base set in the corp window. A human's edit is
// final for the posting pass until the office goes. It runs on the RPC goroutine, so the writes go
// to their own.
func (m *Mod) onMemberBaseChanged(corpID, charID, actorID, oldBase, newBase int64) {
	if m.isSynthetic(actorID) {
		return // the module's own writes never come this way, but be sure
	}
	goSafe("member base changed", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if newBase == 0 {
			if _, err := m.db.ExecContext(ctx,
				`DELETE FROM rolebot_corp_postings WHERE characterID = ?`, charID); err != nil {
				rlogf("rolebots: clear posting %d: %v", charID, err)
			}
		} else {
			setBy := postSetHuman
			if actorID == charID {
				setBy = postSetMember
			}
			arm := ""
			if seat, ok := armOf(charID, corpID); ok {
				arm = string(seat.Arm)
			}
			if _, err := m.db.ExecContext(ctx, `
				INSERT INTO rolebot_corp_postings (characterID, corporationID, stationID, setBy, arm, postedAt)
				VALUES (?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), stationID = VALUES(stationID),
					setBy = VALUES(setBy), arm = VALUES(arm), postedAt = VALUES(postedAt)`,
				charID, corpID, newBase, setBy, arm, clock.Unix()); err != nil {
				rlogf("rolebots: record posting %d: %v", charID, err)
			}
		}
		forgetCorpHome(charID)
		forgetCloneCheck(charID)
		if m.isSynthetic(charID) {
			m.logBot(charID, logInfo, "corp", "a director posted it to %s", m.stationLabel(ctx, newBase))
		}
	})
}

// ── the human letter ────────────────────────────────────────────────────────

// basePath is the signed link that makes an office a member's Base.
func basePath(corpID, charID, stationID int64) string {
	t := signDesk(deskKeyAny(), "base", corpID, charID, stationID)
	return fmt.Sprintf("/ig/corpdesk?corp=%d&c=%d&base=%d&t=%s", corpID, charID, stationID, t)
}

// mailPostingLetter tells a human member where the CEO has posted it, and
// offers every other office.
func (m *Mod) mailPostingLetter(ctx context.Context, c *corpCEO, cm corpMember, p posting, offices []postOffice) {
	corpName := m.corpNameOf(ctx, c.CorpID)
	var b strings.Builder
	line(&b, "%s,", cm.Name)
	b.WriteString("<br>")
	line(&b, "I am posting you to <b>%s</b>, our %s.",
		m.stationLabel(ctx, p.StationID), officePurposeLabel(purposeOf(offices, p.StationID)))
	if p.Arm != "" {
		line(&b, "That is where the %s work is done.", string(p.Arm))
	}
	line(&b, "It is your Base now: your Base roles apply there.")
	var others []string
	for _, o := range offices {
		if o.Dropping || o.StationID == p.StationID {
			continue
		}
		others = append(others, igbLink(basePath(c.CorpID, cm.CharID, o.StationID),
			fmt.Sprintf("%s (%s)", m.stationLabel(ctx, o.StationID), officePurposeLabel(o.Purposes))))
	}
	if len(others) > 0 {
		b.WriteString("<br>")
		line(&b, "If you would rather work from another of our offices, choose it and I will not move you again:")
		for _, l := range others {
			line(&b, "%s", l)
		}
	}
	from, hat := m.letterFrom(ctx, c, mailFromPersonnel)
	b.WriteString(m.signature(ctx, from, hat, corpName))
	m.mailPilot(ctx, cm.CharID, from, "Your posting: "+m.stationLabel(ctx, p.StationID), b.String())
}

// memberChoosesBase applies a human member's pick from the letter: the
// office must still be the corporation's and not being given up.
func (m *Mod) memberChoosesBase(ctx context.Context, corpID, charID, stationID int64) (string, error) {
	var cur int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&cur)
	if cur != corpID {
		return "", fmt.Errorf("you are no longer a member")
	}
	var dropping int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_offices
		 WHERE corporationID = ? AND stationID = ? AND state = 'dropping'`, corpID, stationID).Scan(&dropping)
	if dropping > 0 || !evedb.IsCorpBaseStation(ctx, m.db, corpID, stationID) {
		return "", fmt.Errorf("that office is no longer ours")
	}
	arm := corpArm("")
	if seat, ok := armOf(charID, corpID); ok {
		arm = seat.Arm
	}
	if err := m.writePosting(ctx, corpID, charID, stationID, arm, postSetMember); err != nil {
		return "", err
	}
	return m.stationLabel(ctx, stationID), nil
}
