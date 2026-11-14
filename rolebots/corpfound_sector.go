package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

// Sector balance for self-founding: no line of business takes more than its
// share of the live bot-founded corporations. Founding is a race for one
// global slot (corpFoundPaceSec), and traders win it: they are the richest
// leaders and dock between every job. The gate refuses a founder whose sector
// is full, leaving the slot to the others, and a sector below its limit rolls
// harder (foundSectorBoost) so it fills before the cap runs out. A
// research-career industrialist whose charter leads with manufacturing may
// found a laboratory instead while research is the scarcer of the two; its
// traits are walked to the research intent (toil.go ceoResearchIntent) so the
// charter keeps declaring it.

const (
	// corpFoundSectorShare is the most of the live bot-founded corporations one
	// sector may hold, after the new one. An even split of the six sectors is a
	// sixth; a fifth leaves slack, and six fifths over the whole means some
	// sector always has room.
	corpFoundSectorShare = 0.20
	// corpFoundSectorFloor is the count every sector may reach whatever the
	// share says, so the first foundings are not refused.
	corpFoundSectorFloor = 3
	// corpFoundScarceBoostMax is the most a sector's scarcity multiplies a
	// founder's review roll by (foundSectorBoost).
	corpFoundScarceBoostMax = 3.0
)

// liveFoundedSectors counts the live bot-founded corporations per sector: its
// charter tags, else the founder's sector at founding (an untagged new
// corporation), else the directive (one query, for rows older than the sector
// column).
func (m *Mod) liveFoundedSectors(ctx context.Context, live []foundedCorp) map[string]int {
	out := map[string]int{}
	for _, f := range live {
		out[m.foundedSector(ctx, f)]++
	}
	return out
}

// foundedSector is one bot-founded corporation's sector for the balance.
func (m *Mod) foundedSector(ctx context.Context, f foundedCorp) string {
	if s := corpSectorFromTags(parseTagList(f.Tags)); s != "" {
		return s
	}
	if f.Sector != "" {
		return f.Sector
	}
	return m.corpSectorFor(ctx, f.CorpID)
}

// foundSectorsTTL: how long the review's sector look reuses one count.
const foundSectorsTTL = 600

// foundSectorsCache is the last sector count for the review's look ahead of the
// roll; a founding clears it.
var foundSectorsCache struct {
	sync.Mutex
	at     int64
	counts map[string]int
	total  int
}

// forgetFoundSectors drops the cached count after a founding.
func forgetFoundSectors() {
	foundSectorsCache.Lock()
	foundSectorsCache.at = 0
	foundSectorsCache.Unlock()
}

// cachedFoundSectors is the sector count and live total, read at most every
// foundSectorsTTL.
func (m *Mod) cachedFoundSectors(ctx context.Context) (map[string]int, int, error) {
	foundSectorsCache.Lock()
	defer foundSectorsCache.Unlock()
	now := clock.Unix()
	if foundSectorsCache.at != 0 && now-foundSectorsCache.at < foundSectorsTTL {
		return foundSectorsCache.counts, foundSectorsCache.total, nil
	}
	live, err := m.liveFoundedCorps(ctx)
	if err != nil {
		return nil, 0, err
	}
	foundSectorsCache.counts = m.liveFoundedSectors(ctx, live)
	foundSectorsCache.total = len(live)
	foundSectorsCache.at = now
	return foundSectorsCache.counts, foundSectorsCache.total, nil
}

// founderSectorFits reports whether a would-be founder's sector has room, or
// it could found a laboratory instead (foundResearchTraits), and the boost its
// roll gets for the sector it would found in. A read error lets it through
// unboosted: foundNow checks the balance again.
func (m *Mod) founderSectorFits(ctx context.Context, charID int64, traits map[string]int) (bool, float64) {
	counts, total, err := m.cachedFoundSectors(ctx)
	if err != nil {
		return true, 1
	}
	tags := charterTags(charID, traits)
	sector := corpSectorFromTags(tags)
	if foundSectorOpen(sector, counts, total) {
		return true, foundSectorBoost(sector, counts, total)
	}
	if _, ok := foundResearchTraits(charID, traits, tags, counts, total); ok {
		return true, foundSectorBoost("Research", counts, total)
	}
	return false, 0
}

// foundSectorBoost is how much a founder's review roll is multiplied for its
// sector: the sector's limit over its count after the founding, between 1 and
// corpFoundScarceBoostMax. A sector at a third of its limit rolls three times
// as often, one near its limit at the plain rate. Pure.
func foundSectorBoost(sector string, counts map[string]int, total int) float64 {
	if sector == "" {
		return 1
	}
	b := float64(foundSectorLimit(total)) / float64(counts[sector]+1)
	return math.Max(1, math.Min(b, corpFoundScarceBoostMax))
}

// foundScarcityOrder sorts ready founders so the sector holding the fewest
// live bot-founded corporations comes first, keeping the order within a
// sector. Rows that are not ready stay behind, in their order. Pure.
func foundScarcityOrder(rows []foundProbeRow, counts map[string]int) {
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := rows[i].Ready(), rows[j].Ready()
		if ri != rj {
			return ri
		}
		if !ri {
			return false
		}
		return counts[rows[i].Sector] < counts[rows[j].Sector]
	})
}

// foundSectorLimit is how many live bot-founded corporations one sector may
// hold once the next founding lands, with total live today. Pure.
func foundSectorLimit(total int) int {
	return max(corpFoundSectorFloor, int(math.Ceil(corpFoundSectorShare*float64(total+1))))
}

// foundSectorOpen reports whether a founding in sector fits the balance. A
// founder whose tags name no sector is never refused. Pure.
func foundSectorOpen(sector string, counts map[string]int, total int) bool {
	return sector == "" || counts[sector]+1 <= foundSectorLimit(total)
}

// foundResearchTraits is the founder's traits walked to the research intent
// when it should found a laboratory rather than a factory: its charter leads
// with industry or shipbuilding, research is its living (ResearchAppetite at
// researchCareerMin), research holds no more corporations than industry and
// has room. ok=false leaves the founder as it is. Pure.
func foundResearchTraits(charID int64, traits map[string]int, tags []string,
	counts map[string]int, total int) (map[string]int, bool) {
	if len(tags) == 0 || (tags[0] != "industry" && tags[0] != "shipbuilding") {
		return nil, false
	}
	if resolveTuning(traits).ResearchAppetite < researchCareerMin {
		return nil, false
	}
	if counts["Research"] > counts["Industry"] || !foundSectorOpen("Research", counts, total) {
		return nil, false
	}
	next, ok := nudgeTraitsToIntent(charID, ceoResearchIntent, traits)
	if !ok {
		return nil, false
	}
	return next, true
}

// balanceFounder applies the sector balance to f. It may turn f into a
// research founder: f's traits, tags, kind and propensity are replaced and the
// traits to persist on success are returned. own is the refusal (errFoundSkip)
// for f's own sector, nil when it fits; when nothing was converted, a non-nil
// own refuses the founding.
func (m *Mod) balanceFounder(ctx context.Context, f *founderFacts, live []foundedCorp) (nudged map[string]int, own error) {
	counts := m.liveFoundedSectors(ctx, live)
	if sector := corpSectorFromTags(f.Tags); !foundSectorOpen(sector, counts, len(live)) {
		own = fmt.Errorf("%w: %s already holds %d of %d bot-founded corporations (limit %d)",
			errFoundSkip, sector, counts[sector], len(live), foundSectorLimit(len(live)))
	}
	if next, ok := foundResearchTraits(f.CharID, f.Traits, f.Tags, counts, len(live)); ok {
		f.Traits = next
		f.Tags = charterTags(f.CharID, next)
		f.Kind = titleKindOf(f.Tags)
		f.Propensity = resolveTuning(next).FoundPropensity
		return next, own
	}
	return nil, own
}

// saveFounderTraits writes the traits balanceFounder changed, once the
// corporation exists.
func (m *Mod) saveFounderTraits(ctx context.Context, charID int64, before, after map[string]int, corpName string) {
	var write [][3]any
	for k, v := range after {
		if before[k] != v {
			write = append(write, [3]any{charID, k, v})
		}
	}
	if err := upsertTraits(ctx, m.db, write); err != nil {
		rlogf("rolebots: founder %d research identity: %v", charID, err)
		return
	}
	m.logBot(charID, logInfo, "corp", "took up research to found %s (declares %s)",
		corpName, strings.Join(charterTags(charID, after), ", "))
}
