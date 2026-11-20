package rolebots

// Timezones: when a rolebot plays. Every pilot has a home timezone and a daily play window, read in the server
// machine's local time. Outside its window a pilot is logged off, the same state the online cap uses (popcap.go):
// no sim turn, presence, ball or corporation decisions; only skill training goes on, and there is no catch-up.
//
// The slot is a pure draw from the characterID (tzDraw); rolebot_tz holds only slots that differ from it (corp members
// moved onto their CEO's timezone, source "corp", and generated pilots moved by the seed form, source "gen").
//
// The gate (applyTimezones) runs inside the cap's pass, ahead of the cap arithmetic: a pilot past its hours logs off
// when docked or idle or after rolebots.tz.graceMin, gangs log off whole, and a pilot a player is watching never does
// (popProtected). rolebots.tz.enabled (default on) switches the gate; off, every pilot is in its hours all day.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

const (
	settingTZEnabled    = "rolebots.tz.enabled"
	settingTZCorpShare  = "rolebots.tz.corpShare"
	settingTZGraceMin   = "rolebots.tz.graceMin"
	settingTZBleedMin   = "rolebots.tz.bleedMin"
	settingTZSkipDayPct = "rolebots.tz.skipDayPct"
	settingTZClaimH     = "rolebots.tz.claimAfterHours"

	tzCorpShareDefault  = 0.70
	tzGraceMinDefault   = 45
	tzBleedMinDefault   = 120
	tzSkipDayPctDefault = 8
	tzClaimHDefault     = 24

	settingTZArmsPing  = "rolebots.tz.armsPing"
	settingTZArmsMin   = "rolebots.tz.armsMinNeed"
	settingTZArmsMax   = "rolebots.tz.armsMaxNeed"
	settingTZArmsReach = "rolebots.tz.armsReachJumps"

	tzArmsMinDefault   = 5
	tzArmsMaxDefault   = 40
	tzArmsReachDefault = 10

	// The most pilots the gate logs in or off in one pass. Window edges are
	// spread by the daily drift, so this only binds on the first pass after
	// the switch changes.
	tzMaxChurnPerPass = 40
	// A pilot with less than this left in its window is not recruited into a
	// fleet: it would log off before the fleet got anywhere.
	tzGangHeadroomMin = 90
	// In the last this-many minutes of its hours a docked pilot plans nothing
	// new, so it logs off in its hangar rather than mid-errand.
	tzWindDownMin = 30
	// The CEO-bloc start of a seeded member is within this many minutes of
	// the CEO's own start.
	tzSeedStartSpreadMin = 90
	tzSeedMarker         = "_tzseed"

	tzBlocSalt  = 0x747a626c // "tzbl"
	tzStartSalt = 0x747a7374 // "tzst"
	tzLenSalt   = 0x747a6c6e // "tzln"
	tzDaySalt   = 0x747a6479 // "tzdy"
	tzPickSalt  = 0x747a706b // "tzpk"

	tzDayMin = 1440
	// tzNever is "no window in reach" in minute arithmetic.
	tzNever = int64(1) << 40
)

// tzSlot is one pilot's base window. Start is a server-local minute of the
// day; Offset is the home timezone in hours from the server's, for grouping
// and display only.
type tzSlot struct {
	Offset int
	Start  int
	Len    int
}

// tzBloc buckets an offset: 0 is the server's own timezone, 1 east, 2 west.
func tzBloc(offset int) int {
	switch {
	case offset == 0:
		return 0
	case offset > 0:
		return 1
	default:
		return 2
	}
}

var tzBlocNames = [3]string{"server-local", "east", "west"}

// tzDraw is a pilot's slot when nothing has been written for it. About 55%
// share the server's timezone, 25% sit 6–9h east and 20% 7–10h west. In its
// own local time a pilot starts in the evening (70%), the afternoon (20%) or
// the morning (10%), and plays three to six hours.
func tzDraw(charID int64) tzSlot {
	off := 0
	switch u := hashUnit(charID, tzBlocSalt); {
	case u < 0.55:
	case u < 0.80:
		off = 6 + int(hashUnit(charID, tzBlocSalt+1)*4)
	default:
		off = -(7 + int(hashUnit(charID, tzBlocSalt+1)*4))
	}
	w := hashUnit(charID, tzStartSalt+1)
	var local float64
	switch v := hashUnit(charID, tzStartSalt); {
	case v < 0.70:
		local = 18*60 + w*180
	case v < 0.90:
		local = 12*60 + w*300
	default:
		local = 7*60 + w*240
	}
	return tzSlot{
		Offset: off,
		Start:  tzMod(tzRound5(local)-off*60, tzDayMin),
		Len:    tzDrawLen(charID),
	}
}

// tzDrawLen is a pilot's base session, three to six hours.
func tzDrawLen(charID int64) int {
	return tzRound5(180 + 180*(hashUnit(charID, tzLenSalt)+hashUnit(charID, tzLenSalt+1))/2)
}

// ── the seed form's login windows ───────────────────────────────────────────

// tzWindowOpt is one row of the seed form's timezone mix: the server-clock
// window a pilot's base session starts in. Default is the form's starting
// percentage, the share of free draws (tzDraw) that start in it.
type tzWindowOpt struct {
	Key      string
	From, To int // server-local minutes of the day
	Label    string
	Name     string
	Default  float64
}

var tzWindowOpts = func() []tzWindowOpt {
	names := []string{"After midnight", "Early morning", "Morning", "Afternoon", "Evening", "Late evening"}
	out := make([]tzWindowOpt, len(names))
	for i, n := range names {
		from, to := i*240, (i+1)*240
		out[i] = tzWindowOpt{Key: fmt.Sprintf("h%02d", from/60), From: from, To: to,
			Label: fmt.Sprintf("%02d:00 – %02d:00", from/60, to/60), Name: n}
	}
	// The defaults reproduce the free draw, in tenths summing to exactly 100%.
	const sample = 20000
	rs := make([]ratio, len(out))
	for i := range out {
		rs[i].Key = out[i].Key
	}
	for id := int64(0); id < sample; id++ {
		rs[tzDraw(90_000_000+id*7).Start/240].Weight++
	}
	tenths := allocate(1000, rs)
	for i := range out {
		out[i].Default = float64(tenths[out[i].Key]) / 10
	}
	return out
}()

func tzWindowKeys() []string {
	out := make([]string, len(tzWindowOpts))
	for i, o := range tzWindowOpts {
		out[i] = o.Key
	}
	return out
}

func tzWindowByKey(key string) (tzWindowOpt, bool) {
	for _, o := range tzWindowOpts {
		if o.Key == key {
			return o, true
		}
	}
	return tzWindowOpt{}, false
}

// tzDrawInWindow is a pilot's slot with its base start inside a server-clock
// window. The length is its own draw's. The home offset is read back from the
// start, taking it to be an evening (19:30) login at home; within 3 hours of
// the server's clock counts as the server's timezone.
func tzDrawInWindow(charID int64, w tzWindowOpt) tzSlot {
	start := tzMod(tzRound5(float64(w.From)+hashUnit(charID, tzStartSalt+1)*float64(w.To-w.From-5)), tzDayMin)
	off := tzMod(int(math.Round(float64(19*60+30-start)/60)), 24)
	if off > 12 {
		off -= 24
	}
	if off >= -3 && off <= 3 {
		off = 0
	}
	return tzSlot{Offset: off, Start: start, Len: tzDrawLen(charID)}
}

// tzWindowSlot is the slot a generated pilot gets for a window, and whether it
// needs a stored row: a pilot whose free draw already starts inside the window
// keeps it.
func tzWindowSlot(charID int64, w tzWindowOpt) (tzSlot, bool) {
	if s := tzDraw(charID); s.Start >= w.From && s.Start < w.To {
		return s, false
	}
	return tzDrawInWindow(charID, w), true
}

// tzWindowCover is, per form window, the share of its pilots in their hours
// at each server hour, averaged over two weeks of daily drift and days off.
// The seed form's preview chart multiplies it by the shares. Cached per
// settings.
func tzWindowCover(c tzConfig) [][24]float64 {
	tzCoverMu.Lock()
	defer tzCoverMu.Unlock()
	if tzCoverCache != nil && tzCoverCfg == c {
		return tzCoverCache
	}
	const pilots, days = 300, 14
	out := make([][24]float64, len(tzWindowOpts))
	for i, w := range tzWindowOpts {
		for p := int64(0); p < pilots; p++ {
			id := 90_000_000 + p*7
			s := tzDrawInWindow(id, w)
			for d := int64(0); d < days; d++ {
				for h := 0; h < 24; h++ {
					if tzStateAt(id, s, 20000+d, int64(h*60+30), c).In {
						out[i][h]++
					}
				}
			}
		}
		for h := range out[i] {
			out[i][h] = math.Round(out[i][h]/(pilots*days)*1000) / 1000
		}
	}
	tzCoverCache, tzCoverCfg = out, c
	return out
}

// tzHourLabels is the seed form chart's axis: every third hour named.
func tzHourLabels() []string {
	out := make([]string, 24)
	for h := 0; h < 24; h += 3 {
		out[h] = fmt.Sprintf("%02d:00", h)
	}
	return out
}

var (
	tzCoverMu    sync.Mutex
	tzCoverCache [][24]float64
	tzCoverCfg   tzConfig
)

func tzRound5(v float64) int { return int(math.Round(v/5)) * 5 }

func tzMod(v, n int) int {
	v %= n
	if v < 0 {
		v += n
	}
	return v
}

// ── settings ────────────────────────────────────────────────────────────────

type tzConfig struct {
	On         bool
	CorpShare  float64
	GraceMin   int64
	BleedMin   int64
	SkipDayPct int64
	// ClaimAfterH is how long a finished job may sit before a pilot off for its
	// hours logs in briefly to take it off the line (tzclaim.go); 0 never.
	ClaimAfterH int64
	// The call-to-arms ping (armsping.go).
	ArmsPing       bool
	ArmsMinNeed    int64
	ArmsMaxNeed    int64
	ArmsReachJumps int64
}

func tzDefaults() tzConfig {
	return tzConfig{On: true, CorpShare: tzCorpShareDefault, GraceMin: tzGraceMinDefault,
		BleedMin: tzBleedMinDefault, SkipDayPct: tzSkipDayPctDefault, ClaimAfterH: tzClaimHDefault,
		ArmsPing: true, ArmsMinNeed: tzArmsMinDefault, ArmsMaxNeed: tzArmsMaxDefault,
		ArmsReachJumps: tzArmsReachDefault}
}

var tzCfg atomic.Pointer[tzConfig]

// tzSettings is the cached settings. Before the boot load they are the
// defaults.
func tzSettings() tzConfig {
	if p := tzCfg.Load(); p != nil {
		return *p
	}
	return tzDefaults()
}

func tzOn() bool { return tzSettings().On }

func (c tzConfig) sane() tzConfig {
	c.CorpShare = clampRange(c.CorpShare, 0, 1)
	c.GraceMin = int64(clampRange(float64(c.GraceMin), 0, 600))
	c.BleedMin = int64(clampRange(float64(c.BleedMin), 0, 360))
	c.SkipDayPct = int64(clampRange(float64(c.SkipDayPct), 0, 90))
	c.ClaimAfterH = int64(clampRange(float64(c.ClaimAfterH), 0, 720))
	c.ArmsMinNeed = int64(clampRange(float64(c.ArmsMinNeed), 1, 200))
	c.ArmsMaxNeed = int64(clampRange(float64(c.ArmsMaxNeed), float64(c.ArmsMinNeed), 200))
	c.ArmsReachJumps = int64(clampRange(float64(c.ArmsReachJumps), 0, 40))
	return c
}

// readTZConfig reads every timezone setting. A missing or unparseable value
// keeps its default.
func readTZConfig(ctx context.Context, db *sql.DB) tzConfig {
	c := tzDefaults()
	get := func(key string) (string, bool) {
		v, ok, err := evedb.GetServerSetting(ctx, db, key)
		if err != nil {
			rlogf("rolebots: read %s: %v (using the default)", key, err)
			return "", false
		}
		return strings.TrimSpace(v), ok
	}
	if v, ok := get(settingTZEnabled); ok {
		c.On = v == "1"
	}
	if v, ok := get(settingTZArmsPing); ok {
		c.ArmsPing = v == "1"
	}
	if v, ok := get(settingTZCorpShare); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.CorpShare = f
		}
	}
	for _, d := range []struct {
		key string
		dst *int64
	}{{settingTZGraceMin, &c.GraceMin}, {settingTZBleedMin, &c.BleedMin}, {settingTZSkipDayPct, &c.SkipDayPct},
		{settingTZClaimH, &c.ClaimAfterH},
		{settingTZArmsMin, &c.ArmsMinNeed}, {settingTZArmsMax, &c.ArmsMaxNeed}, {settingTZArmsReach, &c.ArmsReachJumps}} {
		if v, ok := get(d.key); ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				*d.dst = n
			}
		}
	}
	return c.sane()
}

// loadTZSettings reads the settings and the stored slots at boot. Never fatal.
func (m *Mod) loadTZSettings(ctx context.Context) {
	c := readTZConfig(ctx, m.db)
	tzCfg.Store(&c)
	if err := loadTZBook(ctx, m.db); err != nil {
		rlogf("rolebots: timezones: %v (every pilot keeps its drawn slot)", err)
	}
}

// setTZSettings persists the panel form and swaps the cache.
func (m *Mod) setTZSettings(ctx context.Context, c tzConfig) error {
	c = c.sane()
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	for _, kv := range [][2]string{
		{settingTZEnabled, flag(c.On)},
		{settingTZArmsPing, flag(c.ArmsPing)},
		{settingTZArmsMin, strconv.FormatInt(c.ArmsMinNeed, 10)},
		{settingTZArmsMax, strconv.FormatInt(c.ArmsMaxNeed, 10)},
		{settingTZArmsReach, strconv.FormatInt(c.ArmsReachJumps, 10)},
		{settingTZCorpShare, strconv.FormatFloat(c.CorpShare, 'f', 2, 64)},
		{settingTZGraceMin, strconv.FormatInt(c.GraceMin, 10)},
		{settingTZBleedMin, strconv.FormatInt(c.BleedMin, 10)},
		{settingTZSkipDayPct, strconv.FormatInt(c.SkipDayPct, 10)},
		{settingTZClaimH, strconv.FormatInt(c.ClaimAfterH, 10)},
	} {
		if err := evedb.SetServerSetting(ctx, m.db, kv[0], kv[1]); err != nil {
			return fmt.Errorf("setTZSettings %s: %w", kv[0], err)
		}
	}
	tzCfg.Store(&c)
	return nil
}

// ── stored slots ────────────────────────────────────────────────────────────

var (
	tzMu   sync.RWMutex
	tzRows = map[int64]tzSlot{}
)

// tzSlotOf is a pilot's base window: its stored row, or its draw.
func tzSlotOf(charID int64) tzSlot {
	tzMu.RLock()
	s, ok := tzRows[charID]
	tzMu.RUnlock()
	if ok {
		return s
	}
	return tzDraw(charID)
}

func loadTZBook(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT characterID, tzOffset, tzStart, tzLen FROM rolebot_tz`)
	if err != nil {
		return fmt.Errorf("loadTZBook: %w", err)
	}
	defer rows.Close()
	book := map[int64]tzSlot{}
	for rows.Next() {
		var id int64
		var s tzSlot
		if err := rows.Scan(&id, &s.Offset, &s.Start, &s.Len); err != nil {
			return fmt.Errorf("loadTZBook scan: %w", err)
		}
		book[id] = s
	}
	if err := rows.Err(); err != nil {
		return err
	}
	tzMu.Lock()
	tzRows = book
	tzMu.Unlock()
	return nil
}

// storeTZSlot writes one pilot's slot and puts it in the cache, so the gate
// reads it without a reload.
func storeTZSlot(ctx context.Context, db *sql.DB, charID int64, s tzSlot, source string) error {
	if err := writeTZRows(ctx, db, []tzSeedRow{{CharID: charID, Slot: s, Source: source}}); err != nil {
		return err
	}
	tzMu.Lock()
	tzRows[charID] = s
	tzMu.Unlock()
	return nil
}

// writeTZRows upserts slots in one statement per batch.
func writeTZRows(ctx context.Context, db *sql.DB, rows []tzSeedRow) error {
	const batch = 500
	for i := 0; i < len(rows); i += batch {
		chunk := rows[i:min(i+batch, len(rows))]
		var b strings.Builder
		args := make([]any, 0, len(chunk)*5)
		b.WriteString(`INSERT INTO rolebot_tz (characterID, tzOffset, tzStart, tzLen, source) VALUES `)
		for j, r := range chunk {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(?,?,?,?,?)")
			args = append(args, r.CharID, r.Slot.Offset, r.Slot.Start, r.Slot.Len, r.Source)
		}
		b.WriteString(` ON DUPLICATE KEY UPDATE tzOffset = VALUES(tzOffset), tzStart = VALUES(tzStart),
			tzLen = VALUES(tzLen), source = VALUES(source)`)
		if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
			return fmt.Errorf("writeTZRows: %w", err)
		}
	}
	return nil
}

// ── the daily window ────────────────────────────────────────────────────────

// tzWindow is one session in absolute minutes since the local epoch day.
type tzWindow struct{ From, To int64 }

// tzLocal reads a time as (local day number, minute of that day) in the
// server machine's timezone.
func tzLocal(t time.Time) (day, minute int64) {
	lt := t.In(time.Local)
	y, mo, d := lt.Date()
	day = time.Date(y, mo, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
	return day, int64(lt.Hour()*60 + lt.Minute())
}

// tzDayWindow is the session a pilot plays on one local day, or false for a
// day off. Pure in (charID, slot, day, settings).
func tzDayWindow(charID int64, s tzSlot, day int64, c tzConfig) (tzWindow, bool) {
	r := splitmix64(uint64(charID) ^ tzDaySalt + uint64(day)*noiseGolden)
	u := func(k uint64) float64 {
		return float64(splitmix64(r+k*noiseGolden)>>11) / (1 << 53)
	}
	if float64(c.SkipDayPct) > 100*u(1) {
		return tzWindow{}, false
	}
	shift := math.Round(float64(c.BleedMin) * (u(2) + u(3) - 1))
	length := float64(s.Len) + math.Round(60*(u(4)+u(5)-1))
	switch time.Unix(day*86400, 0).UTC().Weekday() {
	case time.Saturday, time.Sunday:
		length += 60
	}
	length = math.Max(length, 60)
	from := day*tzDayMin + int64(s.Start) + int64(shift)
	return tzWindow{From: from, To: from + int64(length)}, true
}

// tzState is where a pilot stands against its hours at one instant.
type tzState struct {
	In bool
	// LeftMin is the minutes until the current window closes (In only).
	LeftMin int64
	// SinceEndMin is the minutes since the last window closed (not In).
	SinceEndMin int64
	// NextMin is the minutes until the next window opens (not In).
	NextMin int64
}

// tzStateAt is a pilot's standing against its hours at (day, minute). A
// window can start up to bleedMin before its own day and run past midnight,
// so the neighbouring days are read too.
func tzStateAt(charID int64, s tzSlot, day, minute int64, c tzConfig) tzState {
	now := day*tzDayMin + minute
	st := tzState{SinceEndMin: tzNever, NextMin: tzNever}
	for d := day - 2; d <= day+2; d++ {
		w, ok := tzDayWindow(charID, s, d, c)
		if !ok {
			continue
		}
		switch {
		case w.From <= now && now < w.To:
			st.In = true
			st.LeftMin = max(st.LeftMin, w.To-now)
		case w.To <= now:
			st.SinceEndMin = min(st.SinceEndMin, now-w.To)
		default:
			st.NextMin = min(st.NextMin, w.From-now)
		}
	}
	return st
}

// tzNow is a pilot's standing right now, on the sim's clock.
func tzNow(charID int64) tzState {
	c := tzSettings()
	if !c.On {
		return tzState{In: true, LeftMin: tzNever}
	}
	day, minute := tzLocal(clock.Now())
	return tzStateAt(charID, tzSlotOf(charID), day, minute, c)
}

// tzShortHours reports whether a pilot is too close to the end of its hours
// to be worth recruiting into a fleet.
func tzShortHours(charID int64) bool {
	if armsHeld(charID) {
		return false
	}
	st := tzNow(charID)
	return st.In && st.LeftMin < tzGangHeadroomMin
}

// tzWindingDown reports whether a pilot is in the last tzWindDownMin of its
// hours, when it starts no errand that would take it out of the station.
func tzWindingDown(charID int64) bool {
	st := tzNow(charID)
	return st.In && st.LeftMin < tzWindDownMin
}

// ── the gate ────────────────────────────────────────────────────────────────

// tzIsOff reports whether a pilot is logged off for being outside its hours.
func tzIsOff(charID int64) bool {
	popMu.Lock()
	defer popMu.Unlock()
	_, off := tzOff[charID]
	return off
}

// tzOffCount is how many pilots are logged off for their hours.
func tzOffCount() int {
	popMu.Lock()
	defer popMu.Unlock()
	return len(tzOff)
}

// tzReady reports whether a pilot past its hours may log off now: docked or idle, at a stopping point, or past the
// grace, and never inside its PvP window.
func tzReady(b *botRun, st tzState, c tzConfig) bool {
	// Crimewatch: nobody logs out of a fight (crimewatch.go botInPvP).
	if botInPvP(b.CharID) {
		return false
	}
	if st.SinceEndMin >= c.GraceMin {
		return true
	}
	if b.State == nil || b.State.Halted || b.State.Phase == "" || b.State.Phase == "idle" {
		return true
	}
	return b.StationID != 0 && interruptibleState(b.State)
}

// tzUnitWantsOff decides one benchable unit: a solo pilot or a whole gang. A gang logs off when more than half of it
// is past its hours and more than half may stop now; a member whose hours ended earlier stays on with its gang.
func tzUnitWantsOff(u []*botRun, day, minute int64, c tzConfig) bool {
	past, ready := 0, 0
	for _, b := range u {
		// A booked Mind Clash fighter, a pilot serving a player and one held
		// by a call to arms (armsping.go) stay on.
		if clashErrandActive(b.CharID) || b.BossID != 0 || armsHeld(b.CharID) {
			return false
		}
		st := tzStateAt(b.CharID, tzSlotOf(b.CharID), day, minute, c)
		if st.In {
			continue
		}
		past++
		if tzReady(b, st, c) {
			ready++
		}
	}
	return past*2 > len(u) && ready*2 > len(u)
}

// tzUnits buckets the roster by shard key, gangs whole, in roster order.
func tzUnits(bots []*botRun) [][]*botRun {
	index := map[int64]int{}
	var units [][]*botRun
	for _, b := range bots {
		k := shardKey(b)
		i, ok := index[k]
		if !ok {
			i = len(units)
			index[k] = i
			units = append(units, nil)
		}
		units[i] = append(units[i], b)
	}
	return units
}

// applyTimezones logs pilots in and off by their hours and returns the ones
// in their hours, for the cap to divide up. boot lifts the churn limit and
// the dwells: before the first presence sync nobody has seen anyone yet.
func (m *Mod) applyTimezones(ctx context.Context, bots []*botRun, now int64, boot bool) []*botRun {
	c := tzSettings()
	day, minute := tzLocal(clock.Now())
	churn := tzMaxChurnPerPass
	if boot {
		churn = len(bots)
	}

	popMu.Lock()
	offAt := make(map[int64]int64, len(tzOff))
	for id, at := range tzOff {
		offAt[id] = at
	}
	onAt := make(map[int64]int64, len(bots))
	for _, b := range bots {
		onAt[b.CharID] = popOnlineAt[b.CharID]
	}
	popMu.Unlock()

	// tzReady reads the state of a pilot whose hours ended inside the grace. A pilot loaded
	// as offline (sim.go simPass) is not hydrated yet, so that narrow set is loaded first.
	if c.On {
		var grace []*botRun
		for _, b := range bots {
			if b.State != nil {
				continue
			}
			if st := tzStateAt(b.CharID, tzSlotOf(b.CharID), day, minute, c); !st.In && st.SinceEndMin < c.GraceMin {
				grace = append(grace, b)
			}
		}
		if err := m.hydrateMissing(ctx, grace); err != nil {
			rlogf("rolebots: timezones: loading pilots in their grace: %v", err)
		}
	}

	out := make([]*botRun, 0, len(bots))
	var bench, back []*botRun
	for _, u := range tzUnits(bots) {
		off, since, called := false, int64(0), false
		for _, b := range u {
			if at, ok := offAt[b.CharID]; ok {
				off, since = true, max(since, at)
			}
			called = called || armsHeld(b.CharID)
		}
		want := c.On && tzUnitWantsOff(u, day, minute, c)
		switch {
		case off && !want:
			// A pilot answering a call to arms logs in now, not after the dwell.
			if !called && (churn < len(u) || (!boot && now-since < popMinOfflineSec)) {
				continue // still off; next pass
			}
			churn -= len(u)
			back = append(back, u...)
			out = append(out, u...)
		case !off && want:
			hold := churn < len(u)
			for _, b := range u {
				if hold {
					break
				}
				if !boot && (m.popProtected(b) || (onAt[b.CharID] > 0 && now-onAt[b.CharID] < popMinOnlineSec)) {
					hold = true
				}
			}
			if hold {
				out = append(out, u...)
				continue
			}
			churn -= len(u)
			bench = append(bench, u...)
		case off:
			// Still outside its hours.
		default:
			out = append(out, u...)
		}
	}
	if len(bench) == 0 && len(back) == 0 {
		return out
	}
	// A returning pilot was loaded as offline and may be unhydrated, and tzResume moves
	// clocks in its state. One that cannot be loaded stays off for this pass.
	if len(back) > 0 {
		if err := m.hydrateMissing(ctx, back); err != nil {
			rlogf("rolebots: timezones: loading returning pilots: %v (they log in next pass)", err)
			skip := make(map[int64]bool, len(back))
			for _, b := range back {
				if b.State == nil {
					skip[b.CharID] = true
				}
			}
			keep := func(list []*botRun) []*botRun {
				kept := list[:0:0]
				for _, b := range list {
					if !skip[b.CharID] {
						kept = append(kept, b)
					}
				}
				return kept
			}
			back, out = keep(back), keep(out)
		}
	}

	popMu.Lock()
	for _, b := range bench {
		popOffline[b.CharID] = now
		tzOff[b.CharID] = now
		delete(popOnlineAt, b.CharID)
	}
	for _, b := range back {
		delete(tzOff, b.CharID)
		// A pilot the cap also holds stays in popOffline; the cap's own pass
		// decides when it comes back.
		if _, cap := popOffline[b.CharID]; cap && popOffline[b.CharID] != offAt[b.CharID] {
			continue
		}
		delete(popOffline, b.CharID)
		popOnlineAt[b.CharID] = now
	}
	popMu.Unlock()

	for _, b := range bench {
		st := tzStateAt(b.CharID, tzSlotOf(b.CharID), day, minute, c)
		if st.In {
			m.logBot(b.CharID, logInfo, "presence", "logged off with its gang, whose hours are over")
			continue
		}
		m.logBot(b.CharID, logInfo, "presence", "logged off for the day — its hours ended %s ago",
			tzDur(st.SinceEndMin))
	}
	for _, b := range back {
		gone := now - offAt[b.CharID]
		tzResume(b, gone)
		if !c.On {
			m.logBot(b.CharID, logInfo, "presence", "logged back in — timezones are off")
			continue
		}
		if armsHeld(b.CharID) {
			continue // armsping.go wrote the line
		}
		st := tzStateAt(b.CharID, tzSlotOf(b.CharID), day, minute, c)
		m.logBot(b.CharID, logInfo, "presence", "logged in for its hours (%s left, away %s)",
			tzDur(st.LeftMin), tzDur(gone/60))
	}
	return out
}

// tzBootGate logs off everyone outside their hours before the boot presence
// sync, so a restart never shows the whole roster for one pass.
func (m *Mod) tzBootGate(ctx context.Context) {
	if !tzOn() {
		return
	}
	bots, err := m.loadRuns(ctx)
	if err != nil {
		rlogf("rolebots: timezones: boot gate: %v", err)
		return
	}
	m.applyTimezones(ctx, bots, clock.Unix(), true)
}

// tzResume moves a returning pilot's running clocks forward by the time it was
// away, so a leg it was flying when it logged off resumes where it stopped
// instead of having finished while nobody flew it. Cooldowns and decays are
// left alone: those were meant to pass.
func tzResume(b *botRun, awaySec int64) {
	if b.State == nil || awaySec <= 0 {
		return
	}
	d := &b.State.Data
	for _, p := range []*int64{&d.LegEndsAt, &d.ActivityStartedAt, &d.PosAtTowerUntil, &d.SiteStartedAt} {
		if *p > 0 {
			*p += awaySec
		}
	}
}

func tzDur(mins int64) string {
	switch {
	case mins >= tzNever:
		return "—"
	case mins >= 60:
		return fmt.Sprintf("%dh%02dm", mins/60, mins%60)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// ── corporations ────────────────────────────────────────────────────────────

// tzOverlap is the fraction of a's base window that b's base window covers.
func tzOverlap(a, b tzSlot) float64 {
	if a.Len <= 0 {
		return 0
	}
	aFrom, aTo := a.Start, a.Start+a.Len
	sum := 0
	for _, k := range []int{-tzDayMin, 0, tzDayMin} {
		bFrom, bTo := b.Start+k, b.Start+b.Len+k
		if lo, hi := max(aFrom, bFrom), min(aTo, bTo); hi > lo {
			sum += hi - lo
		}
	}
	return math.Min(1, float64(sum)/float64(a.Len))
}

// tzCorpMul is how much a pilot's hours suit a corporation run by a bot CEO:
// ×0.75 with no overlap, ×1.15 playing the CEO's hours. 1 for a corporation
// with no bot CEO, for the CEO itself, and with timezones off.
func tzCorpMul(charID, corpID int64) float64 {
	if !tzOn() {
		return 1
	}
	ceo, ok := managedCEO(corpID)
	if !ok || ceo == charID {
		return 1
	}
	return 0.75 + 0.40*tzOverlap(tzSlotOf(charID), tzSlotOf(ceo))
}

// tzSeedMember is one bot member of a bot-CEO corporation as the seed sees it.
type tzSeedMember struct {
	CharID  int64
	Officer bool
}

type tzSeedRow struct {
	CharID int64
	Slot   tzSlot
	Source string
}

// tzCorpSeedPlan moves members of each bot-CEO corporation onto the CEO's timezone until at least share of them play
// in it. Members already in the CEO's bloc count toward the share; officers move first, then a stable salted order.
// A moved member keeps its own session length and starts within tzSeedStartSpreadMin of the CEO. stored is every slot
// already written; those pilots are counted and never moved. Pure.
func tzCorpSeedPlan(ceoOf map[int64]int64, members map[int64][]tzSeedMember, share float64, stored map[int64]tzSlot) []tzSeedRow {
	slot := func(id int64) tzSlot {
		if s, ok := stored[id]; ok {
			return s
		}
		return tzDraw(id)
	}
	corps := make([]int64, 0, len(ceoOf))
	for corpID := range ceoOf {
		corps = append(corps, corpID)
	}
	sort.Slice(corps, func(i, j int) bool { return corps[i] < corps[j] })

	var out []tzSeedRow
	for _, corpID := range corps {
		ceo := slot(ceoOf[corpID])
		ms := members[corpID]
		need := int(math.Ceil(share*float64(len(ms)) - 1e-9))
		var movable []tzSeedMember
		for _, mm := range ms {
			_, fixed := stored[mm.CharID]
			switch {
			case tzBloc(slot(mm.CharID).Offset) == tzBloc(ceo.Offset):
				need--
			case !fixed:
				movable = append(movable, mm)
			}
		}
		sort.SliceStable(movable, func(i, j int) bool {
			if movable[i].Officer != movable[j].Officer {
				return movable[i].Officer
			}
			return hashUnit(movable[i].CharID, tzPickSalt) < hashUnit(movable[j].CharID, tzPickSalt)
		})
		for _, mm := range movable {
			if need <= 0 {
				break
			}
			need--
			own := slot(mm.CharID)
			spread := tzRound5(float64(tzSeedStartSpreadMin) * (2*hashUnit(mm.CharID, tzPickSalt+1) - 1))
			out = append(out, tzSeedRow{CharID: mm.CharID, Source: "corp", Slot: tzSlot{
				Offset: ceo.Offset,
				Start:  tzMod(ceo.Start+spread, tzDayMin),
				Len:    own.Len,
			}})
		}
	}
	return out
}

// migrateTimezoneSeed creates rolebot_tz and, once, moves most of each bot
// CEO's corporation onto the CEO's timezone (tzCorpSeedPlan). Pilots that
// keep their draw get no row. Left unmarked while no bot CEO exists, so a
// later boot still gets its turn.
func migrateTimezoneSeed(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rolebot_tz (
			characterID BIGINT NOT NULL PRIMARY KEY,
			tzOffset    TINYINT NOT NULL,
			tzStart     SMALLINT NOT NULL,
			tzLen       SMALLINT NOT NULL,
			source      VARCHAR(16) NOT NULL DEFAULT ''
		)`); err != nil {
		return fmt.Errorf("migrateTimezoneSeed table: %w", err)
	}
	// Drop the retired offline catch-up settings.
	if _, err := db.ExecContext(ctx, `DELETE FROM server_settings WHERE name LIKE 'rolebots.paper.%'`); err != nil {
		rlogf("rolebots: drop paper settings: %v", err)
	}
	if done, err := traitMarkerSet(ctx, db, tzSeedMarker); err != nil {
		return fmt.Errorf("migrateTimezoneSeed probe: %w", err)
	} else if done {
		return nil
	}
	if ok, err := haveColumn(ctx, db, "character_", "corporationID"); err != nil || !ok {
		return err
	}
	ceoOf := map[int64]int64{}
	rows, err := db.QueryContext(ctx, `SELECT corporationID, characterID FROM rolebot_ceos`)
	if err != nil {
		return fmt.Errorf("migrateTimezoneSeed CEOs: %w", err)
	}
	for rows.Next() {
		var corpID, charID int64
		if err := rows.Scan(&corpID, &charID); err != nil {
			rows.Close()
			return fmt.Errorf("migrateTimezoneSeed CEOs scan: %w", err)
		}
		ceoOf[corpID] = charID
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ceoOf) == 0 {
		return nil
	}

	roles := "0"
	if ok, err := haveColumn(ctx, db, "chrCorporationRoles", "corprole"); err == nil && ok {
		roles = `COALESCE((SELECT r.corprole FROM chrCorporationRoles r WHERE r.characterID = ch.characterID), 0)`
	}
	rows, err = db.QueryContext(ctx, `
		SELECT ch.corporationID, ch.characterID, `+roles+` <> 0
		  FROM character_ ch
		  JOIN rolebot_characters b ON b.characterID = ch.characterID
		  JOIN rolebot_ceos ce ON ce.corporationID = ch.corporationID
		 WHERE b.enabled = 1 AND ch.characterID <> ce.characterID
		 ORDER BY ch.characterID`)
	if err != nil {
		return fmt.Errorf("migrateTimezoneSeed members: %w", err)
	}
	members := map[int64][]tzSeedMember{}
	for rows.Next() {
		var corpID int64
		var mm tzSeedMember
		if err := rows.Scan(&corpID, &mm.CharID, &mm.Officer); err != nil {
			rows.Close()
			return fmt.Errorf("migrateTimezoneSeed members scan: %w", err)
		}
		members[corpID] = append(members[corpID], mm)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if err := loadTZBook(ctx, db); err != nil {
		return err
	}
	tzMu.RLock()
	stored := make(map[int64]tzSlot, len(tzRows))
	for id, s := range tzRows {
		stored[id] = s
	}
	tzMu.RUnlock()

	share := tzCorpShareDefault
	if v, ok, err := evedb.GetServerSetting(ctx, db, settingTZCorpShare); err == nil && ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			share = clampRange(f, 0, 1)
		}
	}
	plan := tzCorpSeedPlan(ceoOf, members, share, stored)
	if err := writeTZRows(ctx, db, plan); err != nil {
		return err
	}
	rlogf("rolebots: timezones: %d member(s) of %d bot-CEO corporation(s) moved onto their CEO's hours",
		len(plan), len(ceoOf))
	return setTraitMarker(ctx, db, tzSeedMarker)
}

// ── the panel ───────────────────────────────────────────────────────────────

// tzCurve is the expected online count per server-local hour today, for the
// given pilots, read at the half hour.
func tzCurve(ids []int64, day int64, c tzConfig) [24]int {
	var out [24]int
	for _, id := range ids {
		s := tzSlotOf(id)
		for h := 0; h < 24; h++ {
			if tzStateAt(id, s, day, int64(h*60+30), c).In {
				out[h]++
			}
		}
	}
	return out
}
