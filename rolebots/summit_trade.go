package rolebots

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// Recruitment and trade on the Summit (summit.go).
//
//   - recruit:<corpID> is one living ad per bot-run corporation that takes
//     applications. The hourly sweep bumps it at most once every
//     summitRecruitEvery, staggered by corporation so they do not all bump
//     together, with this week's facts: headquarters, headcount, the work
//     the charter names, the kills of the week.
//   - shop:<charID> is one sale thread per selling bot, bumped when it lists
//     something worth announcing, at most once per summitShopEvery.

const (
	summitRecruitEvery = 84 * time.Hour // three and a half days
	summitShopEvery    = 48 * time.Hour
	summitShopMinISK   = 20e6
	summitShopPct      = 10
)

// summitTagWords is a charter tag as a recruiter would say it.
var summitTagWords = map[string]string{
	"mining": "mining", "industry": "manufacturing", "shipbuilding": "shipbuilding",
	"research": "research", "trade": "trade", "hauling": "hauling",
	"combat": "combat", "security": "security work", "outlaw": "piracy",
	"cartelindustry": "supply work for the cartels",
}

func summitJoinWords(ws []string) string {
	switch len(ws) {
	case 0:
		return ""
	case 1:
		return ws[0]
	}
	return strings.Join(ws[:len(ws)-1], ", ") + " and " + ws[len(ws)-1]
}

// summitPeriod is which bump window a subject is in, staggered by its ID.
func summitPeriod(now, id int64, every time.Duration) int64 {
	p := int64(every / time.Second)
	off := int64(splitmix64(uint64(id)) % uint64(p))
	return (now + off) / p
}

// summitRecruitSweep files a recruitment bump for each bot corporation due
// one. Hourly.
func (m *Mod) summitRecruitSweep(ctx context.Context) {
	ceos, err := listCEOs(ctx, m.db)
	if err != nil {
		return
	}
	now := clock.Unix()
	for _, c := range ceos {
		if ctx.Err() != nil {
			return
		}
		if !c.powerOn(powerHR) {
			continue
		}
		period := summitPeriod(now, c.CorpID, summitRecruitEvery)
		key := "recruit:" + strconv.FormatInt(c.CorpID, 10)
		f := summitFacts{}
		m.summitCorpFacts(ctx, f, "us", c.CorpID)
		if f["us"] == "" {
			continue
		}
		var members int
		_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM character_ WHERE corporationID = ? AND accountID > 0`,
			c.CorpID).Scan(&members)
		if members < 3 {
			continue // a corporation of two is not a recruiting office
		}
		f["members"] = strconv.Itoa(members)
		if hq := m.corpHQStation(ctx, c.CorpID); hq != 0 {
			if name := m.nameOfStation(ctx, hq); name != "" {
				f["hq"] = name
			}
		}
		var work []string
		for _, t := range ceoCharterOf(ctx, m, c).Tags {
			if w := summitTagWords[t]; w != "" {
				work = append(work, w)
			}
		}
		if w := summitJoinWords(work); w != "" {
			f["work"] = w
		}
		if minKill := m.minKillSince(ctx, now-7*86400); minKill > 0 {
			var kills int
			_ = m.db.QueryRowContext(ctx, `
				SELECT COUNT(DISTINCT killID) FROM killmail_attackers WHERE corpID = ? AND killID >= ? AND isNPC = 0`,
				c.CorpID, minKill).Scan(&kills)
			if kills > 0 {
				f["kills"] = strconv.Itoa(kills)
			}
		}
		author := c.CharID
		if rand.Intn(2) == 0 {
			author = 0 // an officer or a member writes this one
		}
		m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatRecruit, Kind: "recruit.ad", Heat: 20,
			SourceKey: fmt.Sprintf("%s:%d", key, period), Facts: f, SideA: c.CorpID,
			SpeakCorp: c.CorpID, AuthorID: author, Opens: true,
			DelayMin: 0, DelayMax: 4 * time.Hour, TTL: 30 * time.Hour})
	}
}

// summitMarketListed is registerOrder's hook through chatterMarketListed: a
// bot put a sell order up. Cheap gates on the caller's goroutine.
func (m *Mod) summitMarketListed(charID, typeID, stationID, sysID int64, typeName string, price float64, vol int64) {
	if !summitCategoryOn(summitCatMarket) || typeID == 0 || vol <= 0 || price*float64(vol) < summitShopMinISK {
		return
	}
	if rand.Intn(100) >= summitShopPct {
		return
	}
	goSafe("summit shop", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if !m.isSynthetic(charID) {
			return
		}
		if cats, err := m.typeCategories(ctx); err != nil || marketQuietCategories[cats[typeID]] {
			return
		}
		if typeName == "" {
			typeName = m.typeNameOr(ctx, typeID)
		}
		if sysID == 0 {
			sysID = m.stationSystemOf(ctx, stationID)
		}
		f := summitFacts{
			"item":  typeName,
			"units": unitsWords(vol),
			"price": iskString(price),
			"total": iskString(price * float64(vol)),
		}
		if name := m.nameOfStation(ctx, stationID); name != "" {
			f["station"] = name
		}
		if sysID != 0 {
			f["system"] = systemNameOf(ctx, m, sysID)
		}
		key := "shop:" + strconv.FormatInt(charID, 10)
		m.summitFile(ctx, summitBeat{StoryKey: key, Category: summitCatMarket, Kind: "shop.list", Heat: 15,
			SourceKey: fmt.Sprintf("%s:%d", key, summitPeriod(clock.Unix(), charID, summitShopEvery)),
			Facts:     f, AuthorID: charID, Opens: true,
			DelayMin: 5 * time.Minute, DelayMax: time.Hour, TTL: 24 * time.Hour})
	})
}
