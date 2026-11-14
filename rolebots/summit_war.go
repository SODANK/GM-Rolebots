package rolebots

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// War stories on the Summit (summit.go). One war, one thread, war:<warID>, which collects:
//   - the opening (relDiffWars sees the declaration): the declarer's CEO announces it or the
//     defender's CEO cries foul; the other CEO answers hours later.
//   - kills between the two sides (noteKillIncidents): the killer's side gloats, the loser's side
//     vows; a capsuleer who shot a bot of a warring corporation can be named.
//   - a daily tally while the shooting lasts, a jeer when it goes quiet, and "the war is mutual"
//     when a CEO accepts (ceoLog).
//   - the end (retracted or finished): both CEOs claim it, an uninvolved pilot posts the numbers.
// Only a war with a bot-run corporation speaking for each side (the owner, or an alliance's
// executor corporation) gets a story.

// summitWarSide is one side of a war as the desk speaks for it.
type summitWarSide struct {
	Owner   int64 // corporation or alliance at war
	Speaker int64 // the bot-run corporation that speaks for it
	CEO     int64
	Name    string
	Ticker  string
}

func summitWarKey(warID int64) string { return "war:" + strconv.FormatInt(warID, 10) }

// summitWarSideOf resolves an owner to its speaking corporation, ok only
// when that corporation is bot-run.
func (m *Mod) summitWarSideOf(ctx context.Context, owner int64) (summitWarSide, bool) {
	s := summitWarSide{Owner: owner, Speaker: owner}
	var exec int64
	if err := m.db.QueryRowContext(ctx, `SELECT executorCorpID FROM alliances WHERE allianceID = ?`, owner).
		Scan(&exec); err == nil && exec != 0 {
		s.Speaker = exec
	}
	ceo, ok := managedCEO(s.Speaker)
	if !ok {
		return s, false
	}
	s.CEO = ceo
	s.Name = ownerNameOf(ctx, m.db, owner)
	if s.Speaker == owner {
		_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(tickerName, '') FROM corporation WHERE corporationID = ?`, owner).Scan(&s.Ticker)
	} else {
		_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(shortName, '') FROM alliances WHERE allianceID = ?`, owner).Scan(&s.Ticker)
	}
	return s, true
}

func (m *Mod) summitWarSides(ctx context.Context, w evedb.War) (d, a summitWarSide, ok bool) {
	d, okD := m.summitWarSideOf(ctx, w.DeclaredByID)
	a, okA := m.summitWarSideOf(ctx, w.AgainstID)
	return d, a, okD && okA
}

// summitWarMotive is the declaring CEO's motive in a forum voice, from its
// own war book. "" when the war was not one of its own.
var summitWarMotives = map[string][]string{
	warMotiveGrievance:   {"for every pilot of ours they put in the ground", "for a list of debts that only got longer", "because they mistook our patience for weakness"},
	warMotiveGank:        {"for the haulers they ganked under CONCORD's nose", "because a pilot who ganks in policed space answers for it out here", "for the freighters they burned at the gates"},
	warMotivePredation:   {"because they are soft and we are hungry", "because their hangars are full and their guns are not", "because somebody was going to, and it may as well be us"},
	warMotiveCompetition: {"for the belts they keep calling theirs", "because there is not room in this region for both of us", "over the market they have been undercutting for months"},
	warMotiveAlly:        {"because they raised arms against our friends", "on behalf of people we gave our word to", "because an attack on our friends is an attack on us"},
	warMotiveDefence:     {"because they would not leave us alone", "because answering is cheaper than burying more pilots", "because they started this and we intend to finish it"},
	warMotiveContract:    {"because we were paid to", "on a contract we intend to honour", "on somebody else's ISK and our own guns"},
}

func (m *Mod) summitWarMotive(ctx context.Context, warID int64) string {
	var motive string
	_ = m.db.QueryRowContext(ctx, `SELECT motive FROM rolebot_ceo_wars WHERE warID = ?`, warID).Scan(&motive)
	if ps := summitWarMotives[motive]; len(ps) > 0 {
		return ps[rand.Intn(len(ps))]
	}
	return ""
}

// summitWarFacts are the slots every war beat carries, from the speaker's
// side: us/them, and sideA/sideB for the declarer and defender.
func summitWarFacts(d, a summitWarSide, speakerIsD bool) summitFacts {
	us, them := d, a
	if !speakerIsD {
		us, them = a, d
	}
	f := summitFacts{"sideA": d.Name, "sideB": a.Name, "us": us.Name, "them": them.Name}
	if us.Ticker != "" {
		f["usTicker"] = us.Ticker
	}
	if them.Ticker != "" {
		f["themTicker"] = them.Ticker
	}
	return f
}

// summitWarTally counts what each side destroyed of the other since the
// declaration: kills and ISK by the declarer (dK, dISK) and the defender.
func (m *Mod) summitWarTally(ctx context.Context, d, a summitWarSide, since int64) (dK, aK int, dISK, aISK float64) {
	minKill := m.minKillSince(ctx, since)
	if minKill == 0 {
		return
	}
	count := func(victim, killer int64) (int, float64) {
		var n int
		var isk float64
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(SUM(k.totalValue), 0) FROM killmails k
			 WHERE k.killID >= ? AND k.isPod = 0 AND (k.victimCorpID = ? OR k.victimAllianceID = ?)
			   AND EXISTS (SELECT 1 FROM killmail_attackers x
			                WHERE x.killID = k.killID AND (x.corpID = ? OR x.allianceID = ?))`,
			minKill, victim, victim, killer, killer).Scan(&n, &isk)
		return n, isk
	}
	dK, dISK = count(a.Owner, d.Owner)
	aK, aISK = count(d.Owner, a.Owner)
	return
}

func summitDays(n int64) string {
	if n <= 1 {
		return "one day"
	}
	return strconv.FormatInt(n, 10) + " days"
}

// ── the opening and the end (relDiffWars) ───────────────────────────────────

// summitWarChange is relDiffWars' hook: tag "dec" for a new war, "ret" for
// a retraction, "end" for a war that finished.
func (m *Mod) summitWarChange(ctx context.Context, w evedb.War, tag string) {
	if !summitCategoryOn(summitCatWar) {
		return
	}
	d, a, ok := m.summitWarSides(ctx, w)
	if !ok {
		return
	}
	key := summitWarKey(w.WarID)
	src := fmt.Sprintf("war:%d:", w.WarID)
	switch tag {
	case "dec":
		motive := m.summitWarMotive(ctx, w.WarID)
		// A fighting declarer announces its own war; otherwise, as often as
		// not, the defender is the one who takes it to the Summit.
		declarerOpens := rand.Intn(2) == 0
		if c, err := getCEO(ctx, m.db, d.Speaker); err == nil && c != nil && ceoCombatOriented(ceoCharterOf(ctx, m, c)) {
			declarerOpens = true
		}
		openKind, replyKind := "war.declare", "war.defiant"
		openSide, replySide := d, a
		if !declarerOpens {
			openKind, replyKind = "war.attacked", "war.justify"
			openSide, replySide = a, d
		}
		of := summitWarFacts(d, a, openSide.Owner == d.Owner)
		rf := summitWarFacts(d, a, replySide.Owner == d.Owner)
		if motive != "" {
			of["motive"], rf["motive"] = motive, motive
		}
		m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatWar, Kind: openKind, Heat: 100,
			SourceKey: src + "open", Facts: of, SideA: d.Speaker, SideB: a.Speaker,
			SpeakCorp: openSide.Speaker, AuthorID: openSide.CEO, Opens: true,
			DelayMin: 5 * time.Minute, DelayMax: 40 * time.Minute, TTL: 72 * time.Hour})
		m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatWar, Kind: replyKind, Heat: 85,
			SourceKey: src + "reply", Facts: rf, SideA: d.Speaker, SideB: a.Speaker,
			SpeakCorp: replySide.Speaker, AuthorID: replySide.CEO,
			DelayMin: time.Hour, DelayMax: 6 * time.Hour, TTL: 48 * time.Hour})
	case "ret", "end":
		since := evedb.Win32ToUnix(w.TimeDeclared)
		days := (clock.Unix() - since) / 86400
		dK, aK, dISK, aISK := m.summitWarTally(ctx, d, a, since)
		how := "ended"
		if tag == "ret" {
			how = "retracted"
		}
		for _, side := range []struct {
			s      summitWarSide
			isD    bool
			k, l   int
			kI, lI float64
		}{{d, true, dK, aK, dISK, aISK}, {a, false, aK, dK, aISK, dISK}} {
			f := summitWarFacts(d, a, side.isD)
			f["days"] = summitDays(days + 1)
			f["how"] = how
			f["ourKills"], f["ourLosses"] = strconv.Itoa(side.k), strconv.Itoa(side.l)
			if side.kI > 0 {
				f["ourISK"] = iskString(side.kI)
			}
			if side.lI > 0 {
				f["lostISK"] = iskString(side.lI)
			}
			kind := "war.claimwin"
			if side.kI < side.lI {
				kind = "war.claimspin"
			}
			if side.k+side.l == 0 {
				kind = "war.claimquiet"
			}
			m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatWar, Kind: kind, Heat: 80,
				SourceKey: src + "claim:" + strconv.FormatInt(side.s.Owner, 10), Facts: f,
				SideA: d.Speaker, SideB: a.Speaker, SpeakCorp: side.s.Speaker, AuthorID: side.s.CEO,
				DelayMin: 20 * time.Minute, DelayMax: 5 * time.Hour, TTL: 48 * time.Hour})
		}
		of := summitWarFacts(d, a, true)
		of["days"] = summitDays(days + 1)
		of["how"] = how
		of["aKills"], of["bKills"] = strconv.Itoa(dK), strconv.Itoa(aK)
		of["aISK"], of["bISK"] = iskString(dISK), iskString(aISK)
		kind := "war.observer"
		if dK+aK == 0 {
			kind = "war.observerdull"
		}
		m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatWar, Kind: kind, Heat: 70,
			SourceKey: src + "observer", Facts: of, SideA: d.Speaker, SideB: a.Speaker,
			DelayMin: 3 * time.Hour, DelayMax: 10 * time.Hour, TTL: 48 * time.Hour})
	}
}

// ── kills (noteKillIncidents) ───────────────────────────────────────────────

// summitActiveWarAmong finds a live war between the owners on one side and
// the owners on the other.
func (m *Mod) summitActiveWarAmong(ctx context.Context, victims, killers []int64) (*evedb.War, bool) {
	if len(victims) == 0 || len(killers) == 0 {
		return nil, false
	}
	in := func(ids []int64) (string, []any) {
		ph := make([]string, len(ids))
		args := make([]any, len(ids))
		for i, id := range ids {
			ph[i], args[i] = "?", id
		}
		return strings.Join(ph, ","), args
	}
	vp, va := in(victims)
	kp, ka := in(killers)
	args := append(append(append(append([]any{}, va...), ka...), ka...), va...)
	var w evedb.War
	err := m.db.QueryRowContext(ctx, `
		SELECT warID, declaredByID, againstID, timeDeclared FROM wars
		 WHERE timeFinished IS NULL AND retracted IS NULL
		   AND ((declaredByID IN (`+vp+`) AND againstID IN (`+kp+`))
		     OR (declaredByID IN (`+kp+`) AND againstID IN (`+vp+`)))
		 ORDER BY warID DESC LIMIT 1`, args...).
		Scan(&w.WarID, &w.DeclaredByID, &w.AgainstID, &w.TimeDeclared)
	if err != nil {
		return nil, false
	}
	return &w, true
}

func summitKillHeat(base, isk float64) float64 {
	return base + math.Min(30, 6*math.Log10(1+isk/1e6))
}

// summitKill is noteKillIncidents' hook: a kill between two sides at war
// becomes a gloat from the killers, and a vow (or a capsuleer named) from
// the side that lost the ship.
func (m *Mod) summitKill(ctx context.Context, killID int64, rec evedb.KillmailRecord, value float64) {
	if !summitCategoryOn(summitCatWar) || killID == 0 || rec.VictimCorpID == 0 {
		return
	}
	victims := []int64{rec.VictimCorpID}
	if rec.VictimAllianceID != 0 {
		victims = append(victims, rec.VictimAllianceID)
	}
	var killers []int64
	seen := map[int64]bool{}
	for _, at := range rec.Attackers {
		if at.IsNPC {
			continue
		}
		for _, id := range []int64{at.CorpID, at.AllianceID} {
			if id != 0 && !seen[id] && id != rec.VictimCorpID && id != rec.VictimAllianceID {
				seen[id] = true
				killers = append(killers, id)
			}
		}
	}
	w, ok := m.summitActiveWarAmong(ctx, victims, killers)
	if !ok {
		return
	}
	d, a, ok := m.summitWarSides(ctx, *w)
	if !ok {
		return
	}
	victimIsD := w.DeclaredByID == rec.VictimCorpID || w.DeclaredByID == rec.VictimAllianceID
	vSide, kSide := a, d
	if victimIsD {
		vSide, kSide = d, a
	}
	key := summitWarKey(w.WarID)
	src := fmt.Sprintf("war:%d:kill:%d:", w.WarID, killID)
	ship := m.typeNameOr(ctx, rec.ShipTypeID)
	system := systemNameOf(ctx, m, rec.SolarSystemID)

	// Who shot: the final blow, any bot of the killing side, any capsuleer.
	var finalName string
	var killerBot, capsuleer int64
	var capsuleerName, capsuleerCorp string
	for _, at := range rec.Attackers {
		if at.IsNPC {
			continue
		}
		onSide := at.CorpID == kSide.Owner || at.AllianceID == kSide.Owner || at.CorpID == kSide.Speaker
		if at.FinalBlow {
			finalName = at.Name
		}
		if onSide && (at.FinalBlow || killerBot == 0) && m.isSynthetic(at.CharID) {
			killerBot = at.CharID
		}
		if capsuleer == 0 && m.isCapsuleer(ctx, at.CharID) {
			capsuleer, capsuleerName = at.CharID, at.Name
			capsuleerCorp = corpNameOf(ctx, m.db, at.CorpID)
		}
	}
	if finalName == "" {
		finalName = m.charNameOr(ctx, killerBot)
	}
	victimBot := rec.VictimIsBot || m.isSynthetic(rec.VictimCharID)

	gf := summitWarFacts(d, a, kSide.Owner == d.Owner)
	gf["ship"], gf["system"], gf["victim"] = ship, system, rec.VictimName
	if finalName != "" && finalName != "nobody" {
		gf["killer"] = finalName
	}
	if value > 0 {
		gf["isk"] = iskString(value)
	}
	gKind := "war.gloat"
	base := 40.0
	if rec.IsPod {
		gKind, base = "war.pod", 30
	}
	// The pilot that landed the blow brags about it itself when it is a bot
	// of the killing corporation; otherwise a corpmate does.
	var gAuthor int64
	if killerBot != 0 {
		var corp int64
		_ = m.db.QueryRowContext(ctx, `SELECT corporationID FROM character_ WHERE characterID = ?`, killerBot).Scan(&corp)
		if corp == kSide.Speaker {
			gAuthor = killerBot
		}
	}
	m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatWar, Kind: gKind,
		Heat: summitKillHeat(base, value), SourceKey: src + "gloat", Facts: gf,
		SideA: d.Speaker, SideB: a.Speaker, SpeakCorp: kSide.Speaker, AuthorID: gAuthor,
		DelayMin: 10 * time.Minute, DelayMax: 90 * time.Minute, TTL: 12 * time.Hour})

	if !victimBot || rec.IsPod {
		return
	}
	vf := summitWarFacts(d, a, vSide.Owner == d.Owner)
	vf["ship"], vf["system"], vf["victim"] = ship, system, rec.VictimName
	if value > 0 {
		vf["isk"] = iskString(value)
	}
	if capsuleer != 0 && summitDialsNow().NameCapsuleers {
		vf["capsuleer"] = capsuleerName
		if capsuleerCorp != "" {
			vf["capsuleerCorp"] = capsuleerCorp
		}
		m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatWar, Kind: "war.named",
			Heat: summitKillHeat(45, value), SourceKey: src + "named", Facts: vf,
			SideA: d.Speaker, SideB: a.Speaker, SpeakCorp: vSide.Speaker,
			DelayMin: 15 * time.Minute, DelayMax: 2 * time.Hour, TTL: 18 * time.Hour})
		return
	}
	// The pilot who lost the ship says so itself, if it is in the speaking
	// corporation; otherwise a corpmate.
	var vAuthor int64
	if rec.VictimCorpID == vSide.Speaker {
		vAuthor = rec.VictimCharID
	}
	m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatWar, Kind: "war.vow",
		Heat: summitKillHeat(35, value), SourceKey: src + "vow", Facts: vf,
		SideA: d.Speaker, SideB: a.Speaker, SpeakCorp: vSide.Speaker, AuthorID: vAuthor,
		DelayMin: 20 * time.Minute, DelayMax: 2 * time.Hour, TTL: 12 * time.Hour})
}

// ── the daily tally (hourly sweep) ──────────────────────────────────────────

// summitWarTallies files, once per war-day, a tally if the two sides traded
// kills in the last day, or a jeer every third quiet day.
func (m *Mod) summitWarTallies(ctx context.Context) {
	keys, err := m.summitOpenStoryKeys(ctx, summitCatWar)
	if err != nil {
		return
	}
	now := clock.Unix()
	for _, key := range keys {
		warID, err := strconv.ParseInt(strings.TrimPrefix(key, "war:"), 10, 64)
		if err != nil || ctx.Err() != nil {
			continue
		}
		w, err := evedb.GetWar(ctx, m.db, warID)
		if err != nil || w == nil || w.TimeFinished.Valid || w.Retracted.Valid {
			continue
		}
		since := evedb.Win32ToUnix(w.TimeDeclared)
		day := (now - since) / 86400
		if day < 1 {
			continue
		}
		d, a, ok := m.summitWarSides(ctx, *w)
		if !ok {
			continue
		}
		rdK, raK, _, _ := m.summitWarTally(ctx, d, a, now-86400)
		speakD := day%2 == 0
		sp := a
		if speakD {
			sp = d
		}
		if rdK+raK > 0 {
			dK, aK, dISK, aISK := m.summitWarTally(ctx, d, a, since)
			f := summitWarFacts(d, a, speakD)
			f["day"] = strconv.FormatInt(day+1, 10)
			k, l, kI, lI := aK, dK, aISK, dISK
			if speakD {
				k, l, kI, lI = dK, aK, dISK, aISK
			}
			f["ourKills"], f["ourLosses"] = strconv.Itoa(k), strconv.Itoa(l)
			if kI > 0 {
				f["ourISK"] = iskString(kI)
			}
			if lI > 0 {
				f["lostISK"] = iskString(lI)
			}
			kind := "war.tally"
			if kI < lI {
				kind = "war.tallybad"
			}
			m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatWar, Kind: kind, Heat: 55,
				SourceKey: fmt.Sprintf("war:%d:tally:%d", warID, day), Facts: f,
				SideA: d.Speaker, SideB: a.Speaker, SpeakCorp: sp.Speaker, AuthorID: sp.CEO,
				DelayMin: 0, DelayMax: 3 * time.Hour, TTL: 20 * time.Hour})
			continue
		}
		if day%3 == 0 {
			f := summitWarFacts(d, a, speakD)
			f["days"] = summitDays(day)
			m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatWar, Kind: "war.quiet", Heat: 30,
				SourceKey: fmt.Sprintf("war:%d:quiet:%d", warID, day), Facts: f,
				SideA: d.Speaker, SideB: a.Speaker, SpeakCorp: sp.Speaker,
				DelayMin: 0, DelayMax: 6 * time.Hour, TTL: 24 * time.Hour})
		}
	}
}

func (m *Mod) summitOpenStoryKeys(ctx context.Context, category string) ([]string, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT storyKey FROM rolebot_summit_stories WHERE category = ? AND state = ? AND threadID > 0`,
		category, summitStoryOpen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			out = append(out, k)
		}
	}
	return out, rows.Err()
}

// summitWarBetween is the live war between two corporations, either way
// round, through their alliances too. 0 when there is none.
func (m *Mod) summitWarBetween(ctx context.Context, corpA, corpB int64) *evedb.War {
	side := func(c int64) []int64 {
		out := []int64{c}
		if al := m.corpAllianceID(ctx, c); al != 0 {
			out = append(out, al)
		}
		return out
	}
	w, ok := m.summitActiveWarAmong(ctx, side(corpA), side(corpB))
	if !ok {
		return nil
	}
	return w
}

// ── the kill feed ───────────────────────────────────────────────────────────

// summitKillQ hands killmails from the world-event bus to one worker, so the
// bus goroutine never waits on the desk's queries.
var summitKillQ = make(chan evedb.KillmailRecordedEvent, 256)

// summitWatchKills subscribes to every recorded killmail. The bus carries
// all of them: a bot's own, a capsuleer's kill of a bot, a bot's of a
// capsuleer (noteKillIncidents sees only the first kind).
func (m *Mod) summitWatchKills() {
	evedb.SubscribeWorldEvents(func(ev evedb.WorldEvent) {
		k, ok := ev.(evedb.KillmailRecordedEvent)
		if !ok || !Master() || !summitCategoryOn(summitCatWar) {
			return
		}
		select {
		case summitKillQ <- k:
		default:
			summitDrop("killqueue")
		}
	})
	go func() {
		for k := range summitKillQ {
			m.summitKillOne(k)
		}
	}()
}

// summitKillOne is one killmail off the feed. A panic loses that kill, not
// the worker.
func (m *Mod) summitKillOne(k evedb.KillmailRecordedEvent) {
	defer func() {
		if r := recover(); r != nil {
			rlogf("rolebots: summit kill %d panicked: %v", k.KillID, r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var value float64
	_ = m.db.QueryRowContext(ctx, `SELECT totalValue FROM killmails WHERE killID = ?`, k.KillID).Scan(&value)
	m.summitKill(ctx, k.KillID, k.Rec, value)
}
