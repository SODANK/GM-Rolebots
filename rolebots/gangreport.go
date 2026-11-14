package rolebots

// The after-action report: the mail a fleet commander sends its corporation's
// CEO when the op stands down, and the work log behind it.
//
// The other gang tables are books of what is owed and are emptied before a report
// could be written. This file keeps rolebot_gang_haul instead: one row per (gang,
// pilot, kind, type), only added to, read once and deleted with the gang.
//
// The mail body is EVE-HTML, where a newline is whitespace. Every line ends in
// <br>, headings are <b>, and nothing is indented with spaces. The separator is a
// middle dot.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// The kinds of work a haul row records.
const (
	haulOre  = "ore"
	haulLoot = "loot"
	haulKill = "kill"
)

// haulRow is one line of the work log.
type haulRow struct {
	CharID   int64
	Kind     string
	TypeID   int64
	TypeName string
	Units    int64
	M3       float64
	ISK      float64
}

// noteGangHaul adds work to the fleet's log. The upsert sums every column, so
// repeated loads land on one row. typeName is stored so a report does not blank
// when a type lookup misses.
func (m *Mod) noteGangHaul(ctx context.Context, gangID, charID int64, kind string,
	typeID int64, typeName string, units int64, m3, isk float64) {

	if gangID == 0 || charID == 0 || kind == "" {
		return
	}
	if units <= 0 && isk <= 0 && m3 <= 0 {
		return
	}
	if typeName == "" {
		typeName = m.typeNameOr(ctx, typeID)
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_gang_haul
			(gangID, characterID, kind, typeID, typeName, units, m3, isk, updatedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			units = units + VALUES(units),
			m3 = m3 + VALUES(m3),
			isk = isk + VALUES(isk),
			typeName = VALUES(typeName),
			updatedAt = VALUES(updatedAt)`,
		gangID, charID, kind, typeID, typeName, units, m3, isk, clock.Unix()); err != nil {
		rlogf("rolebots: note gang haul gang=%d char=%d kind=%s: %v", gangID, charID, kind, err)
	}
}

// gangHaulRow is one line for noteGangHaulBatch.
type gangHaulRow struct {
	CharID   int64
	Kind     string
	TypeID   int64
	TypeName string
	Units    int64
	M3, ISK  float64
}

// noteGangHaulBatch is noteGangHaul for many lines in one upsert.
func (m *Mod) noteGangHaulBatch(ctx context.Context, gangID int64, rows []gangHaulRow) {
	if gangID == 0 || len(rows) == 0 {
		return
	}
	q := `INSERT INTO rolebot_gang_haul (gangID, characterID, kind, typeID, typeName, units, m3, isk, updatedAt) VALUES `
	args := make([]any, 0, 9*len(rows))
	now := clock.Unix()
	n := 0
	for _, r := range rows {
		if r.CharID == 0 || r.Kind == "" || (r.Units <= 0 && r.ISK <= 0 && r.M3 <= 0) {
			continue
		}
		name := r.TypeName
		if name == "" {
			name = m.typeNameOr(ctx, r.TypeID)
		}
		if n > 0 {
			q += ", "
		}
		q += "(?, ?, ?, ?, ?, ?, ?, ?, ?)"
		args = append(args, gangID, r.CharID, r.Kind, r.TypeID, name, r.Units, r.M3, r.ISK, now)
		n++
	}
	if n == 0 {
		return
	}
	q += ` ON DUPLICATE KEY UPDATE units = units + VALUES(units), m3 = m3 + VALUES(m3),
		isk = isk + VALUES(isk), typeName = VALUES(typeName), updatedAt = VALUES(updatedAt)`
	if _, err := m.db.ExecContext(ctx, q, args...); err != nil {
		rlogf("rolebots: note gang haul batch gang=%d: %v", gangID, err)
	}
}

// noteGangHaulFor is noteGangHaul for a caller that knows the pilot but not the
// fleet (the bounty payers). The SELECT returns no row for a pilot in no gang, so
// a solo kill is one no-op INSERT.
func (m *Mod) noteGangHaulFor(ctx context.Context, charID int64, kind string,
	typeID int64, typeName string, units int64, isk float64) {

	if charID == 0 || kind == "" {
		return
	}
	if units <= 0 && isk <= 0 {
		return
	}
	if typeName == "" {
		typeName = m.typeNameOr(ctx, typeID)
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_gang_haul
			(gangID, characterID, kind, typeID, typeName, units, m3, isk, updatedAt)
		SELECT gangID, ?, ?, ?, ?, ?, 0, ?, ?
		  FROM rolebot_gang_members WHERE characterID = ?
		ON DUPLICATE KEY UPDATE
			units = units + VALUES(units),
			isk = isk + VALUES(isk),
			typeName = VALUES(typeName),
			updatedAt = VALUES(updatedAt)`,
		charID, kind, typeID, typeName, units, isk, clock.Unix(), charID); err != nil {
		rlogf("rolebots: note gang haul char=%d kind=%s: %v", charID, kind, err)
	}
}

// noteGangOrePaid records the commander's payment on the ore row it paid for. It
// is an UPDATE, never an INSERT, so it cannot create an ore row with no ore.
func (m *Mod) noteGangOrePaid(ctx context.Context, gangID, charID, typeID int64, isk float64) {
	if gangID == 0 || charID == 0 || isk <= 0 {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_gang_haul SET isk = isk + ?, updatedAt = ?
		WHERE gangID = ? AND characterID = ? AND kind = ? AND typeID = ?`,
		isk, clock.Unix(), gangID, charID, haulOre, typeID); err != nil {
		rlogf("rolebots: note gang ore paid gang=%d char=%d: %v", gangID, charID, err)
	}
}

// gangHaulLog is the whole work log for one fleet.
func (m *Mod) gangHaulLog(ctx context.Context, gangID int64) ([]haulRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, kind, typeID, typeName, units, m3, isk
		FROM rolebot_gang_haul WHERE gangID = ?`, gangID)
	if err != nil {
		return nil, fmt.Errorf("gangHaulLog: %w", err)
	}
	defer rows.Close()
	var out []haulRow
	for rows.Next() {
		var h haulRow
		if rows.Scan(&h.CharID, &h.Kind, &h.TypeID, &h.TypeName,
			&h.Units, &h.M3, &h.ISK) == nil {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

// ── formatting ──────────────────────────────────────────────────────────────

// groupDigits puts thousands separators into an already-rendered integer string.
func groupDigits(s string) string {
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fmtUnits renders a whole-number count.
func fmtUnits(n int64) string { return groupDigits(strconv.FormatInt(n, 10)) }

// fmtISK renders money as whole, grouped ISK.
func fmtISK(v float64) string {
	if v < 0 {
		return "-" + fmtISK(-v)
	}
	return groupDigits(strconv.FormatInt(int64(v+0.5), 10)) + " ISK"
}

// fmtM3 renders volume with one decimal under 10 m3 and none above.
func fmtM3(v float64) string {
	if v <= 0 {
		return "0 m3"
	}
	if v < 10 {
		return fmt.Sprintf("%.1f m3", v)
	}
	return groupDigits(strconv.FormatInt(int64(v+0.5), 10)) + " m3"
}

// fmtDuration renders the op's length in the largest units that fit.
func fmtDuration(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	d, h, mi := sec/86400, (sec%86400)/3600, (sec%3600)/60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %02dm", d, h, mi)
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, mi)
	default:
		return fmt.Sprintf("%dm", mi)
	}
}

// fmtStamp formats a timestamp in the client's date style. It formats a stamp
// the sim already produced, so it is not a clock read.
func fmtStamp(unix int64) string {
	if unix <= 0 {
		return "unknown"
	}
	return time.Unix(unix, 0).Format("2006.01.02 15:04")
}

// activityLabel is the fleet's purpose in words a CEO reads, not the state slug.
func activityLabel(activity string) string {
	switch activity {
	case ActMining:
		return "Mining operation"
	case ActIndMining:
		return "Supply run (mining for the corporation's factory)"
	case ActCombat:
		return "Bounty hunting"
	case ActSecurity:
		return "Security patrol (home ground)"
	case ActPiracy:
		return "Raiding"
	case "":
		return "Standby"
	}
	return activity
}

// roleLabel spells a seat out; the slugs ("logi", "combat") are ambiguous in a
// letter.
func roleLabel(role string) string {
	switch role {
	case RoleMiner:
		return "miner"
	case RoleHauler:
		return "hauler"
	case RoleCombat:
		return "gunner"
	case RoleTrader:
		return "trader"
	case RoleHunter:
		return "hunter"
	case RoleTackler:
		return "tackler"
	case RoleLogi:
		return "logistics"
	case RoleEwar:
		return "electronic warfare"
	case RoleEscort:
		return "escort"
	}
	return role
}

// ── the letter ──────────────────────────────────────────────────────────────

// line writes one body line ending in <br>.
func line(b *strings.Builder, format string, args ...any) {
	fmt.Fprintf(b, format, args...)
	b.WriteString("<br>")
}

// heading writes a blank line, then a section title.
func heading(b *strings.Builder, title string) {
	b.WriteString("<br><b>")
	b.WriteString(title)
	b.WriteString("</b><br>")
}

// mailFleetReport mails the commander's after-action report to the CEO of a
// player corporation when its fleet stands down. Call it once per gang, before
// deleteGang, which erases the roster and the work log.
func (m *Mod) mailFleetReport(ctx context.Context, g *Gang, why string) {
	if g == nil || g.CorpID == 0 {
		return
	}
	facts, err := getAnyCorpFacts(ctx, m.db, g.CorpID)
	if err != nil || !facts.Player || facts.CEOID == 0 {
		return
	}
	members, err := gangMembers(ctx, m.db, g.GangID)
	if err != nil {
		rlogf("rolebots: fleet report for gang %d: %v", g.GangID, err)
		return
	}
	log, err := m.gangHaulLog(ctx, g.GangID)
	if err != nil {
		rlogf("rolebots: fleet report log for gang %d: %v", g.GangID, err)
	}
	subject := mailSubject("Fleet report: " + g.Name)
	body := m.fleetReportBody(ctx, g, why, members, log)
	// A fleet the chair ordered also reports to the poster, whatever the CEO's
	// mailbox does.
	if g.State.OrderID != 0 {
		if c, err := m.getCorpCommand(ctx, g.State.OrderID); err == nil && c != nil && c.PosterID != facts.CEOID {
			m.mailPilot(ctx, c.PosterID, g.LeaderCharID, subject, body)
		}
	}
	// The CEO mailbox may be suppressed, so members also get a coalesced bulletin
	// (corpnews.go). It is routine unless a capsuleer flew in the fleet.
	var rider int64
	for _, gm := range members {
		if !gm.Driven && m.isCapsuleer(ctx, gm.CharacterID) {
			rider = gm.CharacterID
			break
		}
	}
	if why != "" {
		m.queueCorpNews(ctx, g.CorpID, newsOps,
			fmt.Sprintf("Fleet %s stood down with %d pilot(s): %s.", g.Name, len(members), why), rider)
	} else {
		m.queueCorpNews(ctx, g.CorpID, newsOps,
			fmt.Sprintf("Fleet %s stood down with %d pilot(s).", g.Name, len(members)), rider)
	}
	// The same news goes live from the commander (chatter.go); the fleet's open
	// seat offer closes with it.
	closeFleetOfferFor(g.GangID)
	m.chatterNote(ctx, chatterEvent{Kind: "fleet.down", ChannelID: g.CorpID, CorpID: g.CorpID,
		ActorID: g.LeaderCharID, Slots: map[string]string{"fleet": g.Name, "why": why}, Ref: g.GangID})
	// evemail.created is FILETIME, not Unix seconds: the client's util.FmtDate
	// throws on a Unix-scale value and the mail list fails to render.
	created := win32From(clock.Now())
	msgID, err := evedb.SendEveMail(ctx, m.db, facts.CEOID, g.LeaderCharID, subject, body, created)
	if err != nil {
		rlogf("rolebots: fleet report mail for gang %d: %v", g.GangID, err)
		return
	}
	// A suppressed mailbox returns messageID 0 with a nil error. Do not push a mail
	// that does not exist.
	if msgID != 0 {
		if h := modapi.Gangs(); h.PushMail != nil {
			h.PushMail(facts.CEOID, msgID, g.LeaderCharID, subject, created)
		}
	}
}

// fleetReportBody builds the letter apart from the mail layer, so the harness
// can read one without a mailbox.
func (m *Mod) fleetReportBody(ctx context.Context, g *Gang, why string,
	members []GangMember, log []haulRow) string {

	names := map[int64]string{}
	nameOf := func(id int64) string {
		if n, ok := names[id]; ok {
			return n
		}
		var n string
		if err := m.db.QueryRowContext(ctx,
			`SELECT characterName FROM character_ WHERE characterID = ?`, id).Scan(&n); err != nil || n == "" {
			n = fmt.Sprintf("character %d", id)
		}
		names[id] = n
		return n
	}

	var b strings.Builder
	end := clock.Unix()
	dur := end - g.Created
	if dur < 0 {
		dur = 0
	}

	line(&b, "<b>%s</b>", g.Name)
	line(&b, "After-action report")
	b.WriteString("<br>")
	line(&b, "Commander · %s", nameOf(g.LeaderCharID))
	line(&b, "Purpose · %s", activityLabel(g.Activity))
	if g.State.StagingStationID != 0 {
		line(&b, "Staging · %s", m.stationLabel(ctx, g.State.StagingStationID))
	}
	if w := m.fleetWorkedAt(ctx, g); len(w) > 0 {
		line(&b, "Worked · %s", strings.Join(w, " · "))
	}
	line(&b, "Formed · %s", fmtStamp(g.Created))
	line(&b, "Stood down · %s, after %s", fmtStamp(end), fmtDuration(dur))
	line(&b, "Reason · %s", why)

	// ── the roster ──
	heading(&b, fmt.Sprintf("ROSTER (%d)", len(members)))
	sort.SliceStable(members, func(i, j int) bool {
		if (members[i].CharacterID == g.LeaderCharID) != (members[j].CharacterID == g.LeaderCharID) {
			return members[i].CharacterID == g.LeaderCharID
		}
		return members[i].Role < members[j].Role
	})
	for _, mem := range members {
		seat := roleLabel(mem.Role)
		if mem.CharacterID == g.LeaderCharID {
			seat = "commander, flying " + seat
		}
		row := fmt.Sprintf("%s · %s", nameOf(mem.CharacterID), seat)
		// A seconded pilot is named as one, with the corporation it came from.
		if mem.RecruitCorpID != 0 && mem.RecruitCorpID != g.CorpID {
			row += fmt.Sprintf(" · seconded from %s", m.corpNameOf(ctx, mem.RecruitCorpID))
		}
		if !mem.Driven {
			row += " · flew along, under their own orders"
		}
		line(&b, "%s", row)
	}

	m.writeOreSection(&b, log, nameOf)
	m.writeKillSection(&b, log, nameOf)
	m.writeLootSection(&b, log, nameOf)

	if len(log) == 0 {
		heading(&b, "WORK")
		line(&b, "Nothing was brought in. The fleet stood down before it landed a "+
			"single load — check the muster and the staging station.")
	}

	b.WriteString("<br>")
	line(&b, "%s", nameOf(g.LeaderCharID))
	line(&b, "Fleet Commander, %s", m.corpNameOf(ctx, g.CorpID))
	return b.String()
}

// typeTotal is the log folded to one entry per type for one kind.
type typeTotal struct {
	TypeID   int64
	TypeName string
	Units    int64
	M3       float64
	ISK      float64
}

func foldByType(log []haulRow, kind string) []typeTotal {
	idx := map[int64]*typeTotal{}
	for _, h := range log {
		if h.Kind != kind {
			continue
		}
		t, ok := idx[h.TypeID]
		if !ok {
			t = &typeTotal{TypeID: h.TypeID, TypeName: h.TypeName}
			idx[h.TypeID] = t
		}
		t.Units += h.Units
		t.M3 += h.M3
		t.ISK += h.ISK
	}
	out := make([]typeTotal, 0, len(idx))
	for _, t := range idx {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Units != out[j].Units {
			return out[i].Units > out[j].Units
		}
		return out[i].TypeName < out[j].TypeName
	})
	return out
}

// foldByPilot returns the pilots with rows of this kind, biggest contributor
// first, and each pilot's per-type totals.
func foldByPilot(log []haulRow, kind string) ([]int64, map[int64][]typeTotal) {
	per := map[int64][]haulRow{}
	for _, h := range log {
		if h.Kind == kind {
			per[h.CharID] = append(per[h.CharID], h)
		}
	}
	out := map[int64][]typeTotal{}
	ids := make([]int64, 0, len(per))
	for id, rows := range per {
		out[id] = foldByType(rows, kind)
		ids = append(ids, id)
	}
	total := func(id int64) int64 {
		var n int64
		for _, t := range out[id] {
			n += t.Units
		}
		return n
	}
	sort.Slice(ids, func(i, j int) bool {
		if a, b := total(ids[i]), total(ids[j]); a != b {
			return a > b
		}
		return ids[i] < ids[j]
	})
	return ids, out
}

// writeOreSection lists the ore dug, how much of it, and who dug it.
func (m *Mod) writeOreSection(b *strings.Builder, log []haulRow, nameOf func(int64) string) {
	types := foldByType(log, haulOre)
	if len(types) == 0 {
		return
	}
	var units int64
	var m3, paid float64
	for _, t := range types {
		units += t.Units
		m3 += t.M3
		paid += t.ISK
	}
	heading(b, "ORE DELIVERED")
	line(b, "%s units · %s", fmtUnits(units), fmtM3(m3))
	if paid > 0 {
		line(b, "Paid to the miners · %s", fmtISK(paid))
	}
	b.WriteString("<br>")
	for _, t := range types {
		line(b, "%s · %s units · %s", t.TypeName, fmtUnits(t.Units), fmtM3(t.M3))
	}

	heading(b, "WHO CUT IT")
	ids, per := foldByPilot(log, haulOre)
	for _, id := range ids {
		var u int64
		var v, isk float64
		var parts []string
		for _, t := range per[id] {
			u += t.Units
			v += t.M3
			isk += t.ISK
			parts = append(parts, fmt.Sprintf("%s %s", t.TypeName, fmtUnits(t.Units)))
		}
		row := fmt.Sprintf("<b>%s</b> · %s units · %s", nameOf(id), fmtUnits(u), fmtM3(v))
		if isk > 0 {
			row += fmt.Sprintf(" · paid %s", fmtISK(isk))
		}
		line(b, "%s", row)
		line(b, "%s", strings.Join(parts, " · "))
	}
}

// writeKillSection lists what the pack shot by rat type, with the bounty it
// carried.
func (m *Mod) writeKillSection(b *strings.Builder, log []haulRow, nameOf func(int64) string) {
	types := foldByType(log, haulKill)
	if len(types) == 0 {
		return
	}
	var kills int64
	var bounty float64
	for _, t := range types {
		kills += t.Units
		bounty += t.ISK
	}
	heading(b, "KILLS")
	line(b, "%s rats destroyed · %s in bounties", fmtUnits(kills), fmtISK(bounty))
	b.WriteString("<br>")
	for _, t := range types {
		line(b, "%s × %s · %s", t.TypeName, fmtUnits(t.Units), fmtISK(t.ISK))
	}

	heading(b, "WHO SHOT WHAT")
	ids, per := foldByPilot(log, haulKill)
	for _, id := range ids {
		var n int64
		var isk float64
		var parts []string
		for _, t := range per[id] {
			n += t.Units
			isk += t.ISK
			parts = append(parts, fmt.Sprintf("%s × %s", t.TypeName, fmtUnits(t.Units)))
		}
		line(b, "<b>%s</b> · %s kills · %s", nameOf(id), fmtUnits(n), fmtISK(isk))
		line(b, "%s", strings.Join(parts, " · "))
	}
}

// writeLootSection lists what came home off the wrecks and whose hold carried it.
func (m *Mod) writeLootSection(b *strings.Builder, log []haulRow, nameOf func(int64) string) {
	types := foldByType(log, haulLoot)
	if len(types) == 0 {
		return
	}
	var units int64
	for _, t := range types {
		units += t.Units
	}
	heading(b, "SALVAGE RECOVERED")
	line(b, "%s items across %d type(s)", fmtUnits(units), len(types))
	b.WriteString("<br>")
	for _, t := range types {
		line(b, "%s × %s", t.TypeName, fmtUnits(t.Units))
	}

	heading(b, "WHO CARRIED IT")
	ids, per := foldByPilot(log, haulLoot)
	for _, id := range ids {
		var n int64
		var parts []string
		for _, t := range per[id] {
			n += t.Units
			parts = append(parts, fmt.Sprintf("%s × %s", t.TypeName, fmtUnits(t.Units)))
		}
		line(b, "<b>%s</b> · %s items", nameOf(id), fmtUnits(n))
		line(b, "%s", strings.Join(parts, " · "))
	}
}

// fleetWorkedAt returns where the op happened. The state carries a different
// field per activity, none of them the staging station. A fleet that never
// picked a site returns nothing.
func (m *Mod) fleetWorkedAt(ctx context.Context, g *Gang) []string {
	var out []string
	add := func(label string, sysID int64, name string) {
		if sysID == 0 && name == "" {
			return
		}
		if name == "" {
			name = m.nameOfSystem(ctx, sysID)
		}
		if name == "" {
			return
		}
		out = append(out, label+" "+name)
	}
	switch g.Activity {
	case ActMining, ActIndMining:
		add("belts in", g.State.SystemID, g.State.SystemName)
		if g.State.OreName != "" {
			out = append(out, "cutting "+g.State.OreName)
		}
	case ActCombat, ActSecurity:
		add("sweeping", g.State.HuntSystemID, g.State.HuntSystemName)
		add("answering distress in", g.State.ReliefSystemID, g.State.ReliefSystemName)
	case ActPiracy:
		add("prowling", g.State.PirSystemID, g.State.PirSystemName)
	}
	return out
}
