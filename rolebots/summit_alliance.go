package rolebots

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// Alliance and diplomacy stories on the Summit (summit.go), and the war verbs a CEO decides (a war
// made mutual). All of it comes through ceoLog, the one place a bot CEO's decisions pass.
//
//   - alliance:<allianceID> is an alliance's house thread, opened by its
//     founder: joins, departures, expulsions, the executor seat and the
//     alliance's declarations on others are replies.
//   - pact:<lo>:<hi> is two corporations' diplomacy: a pact, a peace,
//     reparations. Both sides must be bot-run; the other side answers.
//   - pactbreak:<lo>:<hi>:<day> is a broken pact, its own thread: the
//     wronged CEO opens it, the breaker answers, an outsider remarks.

// summitCEO is ceoLog's hook. detail is the ledger line without the
// officer's label.
func (m *Mod) summitCEO(ctx context.Context, c *corpCEO, power, verb string, subjectID int64, amount float64, detail string) {
	if c == nil || !summitOn() {
		return
	}
	switch power {
	case powerWar:
		if verb == "mutual" {
			m.summitWarMutual(ctx, c, subjectID)
		}
	case powerAlliance:
		if summitCategoryOn(summitCatAlliance) {
			m.summitAllianceEvent(ctx, c, verb, subjectID, amount)
		}
	case powerDiplomacy:
		if summitCategoryOn(summitCatAlliance) {
			m.summitDiplomacy(ctx, c, verb, subjectID, amount, detail)
		}
	}
}

func (m *Mod) summitWarMutual(ctx context.Context, c *corpCEO, other int64) {
	if !summitCategoryOn(summitCatWar) {
		return
	}
	w := m.summitWarBetween(ctx, c.CorpID, other)
	if w == nil {
		return
	}
	d, a, ok := m.summitWarSides(ctx, *w)
	if !ok {
		return
	}
	f := summitWarFacts(d, a, d.Speaker == c.CorpID)
	m.summitFile(ctx, summitBeat{StoryKey: summitWarKey(w.WarID), Category: summitCatWar, Kind: "war.mutual",
		Heat: 60, SourceKey: fmt.Sprintf("war:%d:mutual", w.WarID), Facts: f,
		SideA: d.Speaker, SideB: a.Speaker, SpeakCorp: c.CorpID, AuthorID: c.CharID,
		DelayMin: 10 * time.Minute, DelayMax: 90 * time.Minute, TTL: 36 * time.Hour})
}

// summitAllianceFacts names an alliance: alliance, allianceTicker.
func (m *Mod) summitAllianceFacts(ctx context.Context, allianceID int64) summitFacts {
	f := summitFacts{}
	var name, short string
	_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(allianceName, ''), COALESCE(shortName, '') FROM alliances WHERE allianceID = ?`,
		allianceID).Scan(&name, &short)
	if name != "" {
		f["alliance"] = name
	}
	if short != "" {
		f["allianceTicker"] = short
	}
	return f
}

func (m *Mod) summitCorpFacts(ctx context.Context, f summitFacts, prefix string, corpID int64) {
	var name, tick string
	_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(corporationName, ''), COALESCE(tickerName, '') FROM corporation WHERE corporationID = ?`,
		corpID).Scan(&name, &tick)
	if name != "" {
		f[prefix] = name
	}
	if tick != "" {
		f[prefix+"Ticker"] = tick
	}
}

func summitDay() int64 { return clock.Unix() / 86400 }

func (m *Mod) summitAllianceEvent(ctx context.Context, c *corpCEO, verb string, subjectID int64, amount float64) {
	allianceID := m.corpAllianceID(ctx, c.CorpID)
	switch verb {
	case "found", "joined", "leave":
		allianceID = subjectID
	}
	if allianceID == 0 {
		return
	}
	key := "alliance:" + strconv.FormatInt(allianceID, 10)
	src := key + ":"
	f := m.summitAllianceFacts(ctx, allianceID)
	if f["alliance"] == "" {
		return
	}
	m.summitCorpFacts(ctx, f, "us", c.CorpID)
	file := func(b summitBeat) {
		b.StoryKey, b.Category = key, summitCatAlliance
		if b.SideA == 0 {
			b.SideA = c.CorpID
		}
		m.summitFile(ctx, b)
	}
	switch verb {
	case "found":
		file(summitBeat{Kind: "alliance.found", Heat: 90, SourceKey: src + "found", Facts: f,
			SpeakCorp: c.CorpID, AuthorID: c.CharID, Opens: true,
			DelayMin: 10 * time.Minute, DelayMax: time.Hour, TTL: 72 * time.Hour})
		file(summitBeat{Kind: "alliance.doubt", Heat: 45, SourceKey: src + "doubt", Facts: f,
			DelayMin: 3 * time.Hour, DelayMax: 12 * time.Hour, TTL: 36 * time.Hour})
	case "joined":
		file(summitBeat{Kind: "alliance.joined", Heat: 55, SourceKey: src + fmt.Sprintf("join:%d:%d", c.CorpID, summitDay()),
			Facts: f, SpeakCorp: c.CorpID, AuthorID: c.CharID,
			DelayMin: 15 * time.Minute, DelayMax: 2 * time.Hour, TTL: 36 * time.Hour})
		var exec int64
		_ = m.db.QueryRowContext(ctx, `SELECT executorCorpID FROM alliances WHERE allianceID = ?`, allianceID).Scan(&exec)
		if _, ok := managedCEO(exec); ok && exec != c.CorpID {
			wf := summitFacts{}
			for k, v := range f {
				wf[k] = v
			}
			wf["them"] = wf["us"]
			delete(wf, "us")
			delete(wf, "usTicker")
			file(summitBeat{Kind: "alliance.welcome", Heat: 40, SourceKey: src + fmt.Sprintf("welcome:%d:%d", c.CorpID, summitDay()),
				Facts: wf, SpeakCorp: exec, DelayMin: time.Hour, DelayMax: 5 * time.Hour, TTL: 24 * time.Hour})
		}
	case "leave":
		file(summitBeat{Kind: "alliance.left", Heat: 60, SourceKey: src + fmt.Sprintf("left:%d:%d", c.CorpID, summitDay()),
			Facts: f, SpeakCorp: c.CorpID, AuthorID: c.CharID,
			DelayMin: 15 * time.Minute, DelayMax: 2 * time.Hour, TTL: 36 * time.Hour})
	case "kick":
		m.summitCorpFacts(ctx, f, "them", subjectID)
		file(summitBeat{Kind: "alliance.kick", Heat: 75, SourceKey: src + fmt.Sprintf("kick:%d:%d", subjectID, summitDay()),
			Facts: f, SideB: subjectID, SpeakCorp: c.CorpID, AuthorID: c.CharID,
			DelayMin: 15 * time.Minute, DelayMax: 90 * time.Minute, TTL: 36 * time.Hour})
		if ceo, ok := managedCEO(subjectID); ok {
			kf := summitFacts{"alliance": f["alliance"]}
			m.summitCorpFacts(ctx, kf, "us", subjectID)
			m.summitCorpFacts(ctx, kf, "them", c.CorpID)
			file(summitBeat{Kind: "alliance.kicked", Heat: 70, SourceKey: src + fmt.Sprintf("kicked:%d:%d", subjectID, summitDay()),
				Facts: kf, SideA: subjectID, SideB: c.CorpID, SpeakCorp: subjectID, AuthorID: ceo,
				DelayMin: time.Hour, DelayMax: 6 * time.Hour, TTL: 36 * time.Hour})
		}
	case "executor":
		ceo, ok := managedCEO(subjectID)
		if !ok {
			return
		}
		m.summitCorpFacts(ctx, f, "us", subjectID)
		file(summitBeat{Kind: "alliance.executor", Heat: 50, SourceKey: src + fmt.Sprintf("exec:%d:%d", subjectID, summitDay()),
			Facts: f, SideA: subjectID, SpeakCorp: subjectID, AuthorID: ceo,
			DelayMin: 30 * time.Minute, DelayMax: 3 * time.Hour, TTL: 36 * time.Hour})
	case "allrel":
		rel := int64(amount)
		if rel == 0 {
			return
		}
		f["them"] = ownerNameOf(ctx, m.db, subjectID)
		kind := "alliance.friend"
		switch rel {
		case evedb.AllianceRelationshipEnemy, evedb.AllianceRelationshipCompetitor:
			kind = "alliance.enemy"
		}
		f["standing"] = strings.ToLower(evedb.AllianceRelationshipName(rel))
		file(summitBeat{Kind: kind, Heat: 50, SourceKey: src + fmt.Sprintf("rel:%d:%d:%d", subjectID, rel, summitDay()),
			Facts: f, SideB: subjectID, SpeakCorp: c.CorpID, AuthorID: c.CharID,
			DelayMin: 20 * time.Minute, DelayMax: 3 * time.Hour, TTL: 36 * time.Hour})
	}
}

func summitPairKey(prefix string, a, b int64) string {
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("%s:%d:%d", prefix, a, b)
}

func (m *Mod) summitDiplomacy(ctx context.Context, c *corpCEO, verb string, other int64, amount float64, detail string) {
	otherCEO, ok := managedCEO(other)
	if !ok || other == c.CorpID {
		return // bots only talk about bots
	}
	f := summitFacts{}
	m.summitCorpFacts(ctx, f, "us", c.CorpID)
	m.summitCorpFacts(ctx, f, "them", other)
	if f["us"] == "" || f["them"] == "" {
		return
	}
	// The other side's facts are the same pair seen from across the table.
	g := summitFacts{"us": f["them"], "them": f["us"]}
	if t := f["themTicker"]; t != "" {
		g["usTicker"] = t
	}
	if t := f["usTicker"]; t != "" {
		g["themTicker"] = t
	}
	day := summitDay()
	pair := summitPairKey("pact", c.CorpID, other)
	both := func(kind, reply string, heat float64) {
		src := fmt.Sprintf("%s:%s:%d", pair, kind, day)
		m.summitFile(ctx, summitBeat{StoryKey: pair, Category: summitCatAlliance, Kind: kind, Heat: heat,
			SourceKey: src, Facts: f, SideA: c.CorpID, SideB: other, SpeakCorp: c.CorpID, AuthorID: c.CharID,
			Opens: true, DelayMin: 15 * time.Minute, DelayMax: 2 * time.Hour, TTL: 48 * time.Hour})
		m.summitFile(ctx, summitBeat{StoryKey: pair, Category: summitCatAlliance, Kind: reply, Heat: heat - 15,
			SourceKey: src + ":reply", Facts: g, SideA: c.CorpID, SideB: other, SpeakCorp: other, AuthorID: otherCEO,
			DelayMin: time.Hour, DelayMax: 6 * time.Hour, TTL: 36 * time.Hour})
	}
	switch verb {
	case "pact":
		f["pact"], g["pact"] = "a non-aggression pact", "a non-aggression pact"
		if !strings.Contains(detail, "non-aggression") {
			f["pact"], g["pact"] = "a pact of friendship", "a pact of friendship"
		}
		both("pact.signed", "pact.ack", 65)
	case "peace":
		both("pact.peace", "pact.peaceack", 70)
	case "reparations":
		if amount > 0 {
			f["isk"], g["isk"] = iskString(amount), iskString(amount)
		}
		both("pact.reparation", "pact.repack", 60)
	case "pactbreak":
		// c is the wronged side; other broke it. Its own thread.
		key := fmt.Sprintf("%s:%d", summitPairKey("pactbreak", c.CorpID, other), day)
		m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatAlliance, Kind: "pact.broken", Heat: 95,
			SourceKey: key + ":open", Facts: f, SideA: c.CorpID, SideB: other, SpeakCorp: c.CorpID, AuthorID: c.CharID,
			Opens: true, DelayMin: 10 * time.Minute, DelayMax: time.Hour, TTL: 72 * time.Hour})
		m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatAlliance, Kind: "pact.excuse", Heat: 80,
			SourceKey: key + ":excuse", Facts: g, SideA: c.CorpID, SideB: other, SpeakCorp: other, AuthorID: otherCEO,
			DelayMin: time.Hour, DelayMax: 6 * time.Hour, TTL: 48 * time.Hour})
		of := summitFacts{"wronged": f["us"], "breaker": f["them"]}
		m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatAlliance, Kind: "pact.observer", Heat: 60,
			SourceKey: key + ":observer", Facts: of, SideA: c.CorpID, SideB: other,
			DelayMin: 3 * time.Hour, DelayMax: 12 * time.Hour, TTL: 48 * time.Hour})
	}
}
