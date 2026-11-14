package rolebots

// A corporation's sector: its line of business, read from its charter tags and directive. The
// SCC Exchange files a listing under it (sccbots.go) and mining ops pace themselves by it
// (miningop.go), so it lives outside the exchange's build tag.

import "context"

// corpSectors are the sectors in the SCC Exchange's index order (evedb.SCCSectors): a tie in
// corpSectorFromDirective goes to the earlier one.
var corpSectors = []string{"Mining", "Industry", "Commerce", "Security", "Logistics", "Research"}

// corpSectorFor is the corporation's line of business, the sector it files under on the
// SCC Exchange and the cadence of its mining ops (miningop.go): its declared business
// first (the tags, stored best-appeal first), its directive only when no tag
// names a sector. The directive cannot say Commerce or Research: trading and
// courier hauling tie in it, and research is not an activity.
func (m *Mod) corpSectorFor(ctx context.Context, corpID int64) string {
	var raw string
	_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(tags, '') FROM rolebot_corps WHERE corporationID = ?`, corpID).Scan(&raw)
	if s := corpSectorFromTags(parseTagList(raw)); s != "" {
		return s
	}
	weights, _, _ := m.loadCorpDirective(ctx, corpID)
	return corpSectorFromDirective(weights)
}

// corpTagSector maps a corporation tag to the sector it files under. Tags that
// say nothing about the business (mercenary rides on top of a fighting tag)
// are absent.
var corpTagSector = map[string]string{
	"mining":         "Mining",
	"industry":       "Industry",
	"shipbuilding":   "Industry",
	"cartelindustry": "Industry",
	"research":       "Research",
	"trade":          "Commerce",
	"hauling":        "Logistics",
	tagCombat:        "Security",
	tagSecurity:      "Security",
	tagOutlaw:        "Security",
}

// corpSectorFromTags is the sector of the first tag that names one; "" when
// none does. Pure.
func corpSectorFromTags(tags []string) string {
	for _, t := range tags {
		if s, ok := corpTagSector[t]; ok {
			return s
		}
	}
	return ""
}

// corpSectorFromDirective sums the directive's shares per sector and files
// under the heaviest; ties go to the earlier sector in corpSectors, so the
// answer never depends on map order. Commerce when the directive is empty.
// Pure.
func corpSectorFromDirective(weights map[string]float64) string {
	sum := map[string]float64{}
	for a, w := range weights {
		switch a {
		case ActMining, ActIndMining:
			sum["Mining"] += w
		case ActIndustry:
			sum["Industry"] += w
		case ActCombat, ActPiracy, ActSecurity:
			sum["Security"] += w
		case ActCourier:
			sum["Logistics"] += w
		default:
			sum["Commerce"] += w
		}
	}
	best, bestW := "Commerce", 0.0
	for _, s := range corpSectors {
		if sum[s] > bestW+1e-9 {
			best, bestW = s, sum[s]
		}
	}
	return best
}
