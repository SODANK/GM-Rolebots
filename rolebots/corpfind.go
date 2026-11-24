package rolebots

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Finding things by a few letters. The in-game browser runs no script, so a field cannot suggest
// as it is typed. What the Management page can do instead is take whatever was typed (a part of a
// name, the words in any order, a slip of the keys) and answer with the things it could mean, as a
// list to click. fuzzyRank is the ranking, pure; the index is the names to rank, read once and kept.

// findEntry is one name that can be found.
type findEntry struct {
	ID    int64
	Name  string
	Group string // what it is, for the list: "Mineral", "Frigate"
	lower string
	words []string
}

func newFindEntry(id int64, name, group string) findEntry {
	l := strings.ToLower(name)
	return findEntry{ID: id, Name: name, Group: group, lower: l, words: findWords(l)}
}

// findWords splits a lower-case name or query into words.
func findWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	})
}

// findHit is a ranked entry.
type findHit struct {
	findEntry
	Score int
}

// editDistance is the number of single-letter slips between two words (a letter added, dropped or
// changed, or two neighbours swapped), stopping early once it passes max. Pure.
func editDistance(a, b string, max int) int {
	ra, rb := []rune(a), []rune(b)
	if d := len(ra) - len(rb); d > max || -d > max {
		return max + 1
	}
	prev2 := make([]int, len(rb)+1)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		low := cur[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			v := min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				v = min(v, prev2[j-2]+1)
			}
			cur[j] = v
			low = min(low, v)
		}
		if low > max {
			return max + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(rb)]
}

// slack is how many slips a query word may hold and still mean a name's word: none for a short
// word, one for a word of four to seven letters, two beyond. Pure.
func slack(n int) int {
	switch {
	case n < 4:
		return 0
	case n < 8:
		return 1
	}
	return 2
}

// wordScore is how well one query word matches a name's words, 0 for not at all. A word that
// starts a name's word beats one inside it, which beats a misspelling. Pure.
func wordScore(q string, words []string) int {
	best := 0
	for _, w := range words {
		s := 0
		switch {
		case w == q:
			s = 100
		case strings.HasPrefix(w, q):
			s = 90 - min(len(w)-len(q), 30)/3
		case len(q) >= 3 && strings.Contains(w, q):
			s = 60
		default:
			k := slack(len(q))
			if k == 0 {
				continue
			}
			// The word whole, or the front of the name's word as long as the query.
			d := editDistance(q, w, k)
			if len(w) > len(q) {
				d = min(d, editDistance(q, w[:len(q)], k))
			}
			if d <= k {
				s = 45 - 10*d
			}
		}
		best = max(best, s)
	}
	return best
}

// fuzzyScore is how well a name answers a query, 0 for not at all: the name itself or its start,
// else every query word found among its words in any order, a misspelt word counting for less.
// Pure.
func fuzzyScore(query string, qwords []string, e findEntry) int {
	if query == e.lower {
		return 1000
	}
	if strings.HasPrefix(e.lower, query) {
		return 900 - min(len(e.lower)-len(query), 100)
	}
	if len(qwords) == 0 {
		return 0
	}
	total := 0
	for _, q := range qwords {
		s := wordScore(q, e.words)
		if s == 0 {
			return 0
		}
		total += s
	}
	score := total * 5 / len(qwords)
	// Fewer words left over in the name is a closer answer.
	score -= 3 * max(0, len(e.words)-len(qwords))
	if strings.Contains(e.lower, query) {
		score += 80
	}
	return score
}

// fuzzyRank is the entries a query could mean, best first, at most limit. Pure.
func fuzzyRank(query string, entries []findEntry, limit int) []findHit {
	query = strings.Join(strings.Fields(strings.ToLower(query)), " ")
	if query == "" {
		return nil
	}
	qwords := findWords(query)
	var out []findHit
	for _, e := range entries {
		if s := fuzzyScore(query, qwords, e); s > 0 {
			out = append(out, findHit{e, s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if len(out[i].Name) != len(out[j].Name) {
			return len(out[i].Name) < len(out[j].Name)
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// findClear is whether the best hit is plainly the one meant: the only hit, or far ahead of the
// next, or the name exactly. Pure.
func findClear(hits []findHit) bool {
	switch len(hits) {
	case 0:
		return false
	case 1:
		return true
	}
	return hits[0].Score >= 1000 || (hits[0].Score >= 900 && hits[0].Score-hits[1].Score >= 150)
}

// ── the indexes ─────────────────────────────────────────────────────────────

const findIndexTTL = time.Hour

type findIndex struct {
	mu      sync.Mutex
	at      time.Time
	entries []findEntry
}

var (
	marketFind    findIndex
	blueprintFind findIndex
	systemFind    findIndex
)

// get is the entries, read again when the kept ones are old. A read that fails keeps what there is.
func (ix *findIndex) get(read func() ([]findEntry, error)) []findEntry {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.entries != nil && time.Since(ix.at) < findIndexTTL {
		return ix.entries
	}
	if es, err := read(); err == nil && len(es) > 0 {
		ix.entries, ix.at = es, time.Now()
	}
	return ix.entries
}

func (m *Mod) readFindEntries(ctx context.Context, q string) ([]findEntry, error) {
	rows, err := m.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []findEntry
	for rows.Next() {
		var id int64
		var name, group string
		if err := rows.Scan(&id, &name, &group); err != nil {
			return nil, err
		}
		out = append(out, newFindEntry(id, name, group))
	}
	return out, rows.Err()
}

// findMarketTypes is what the market trades that a few letters could mean.
func (m *Mod) findMarketTypes(ctx context.Context, words string, limit int) []findHit {
	es := marketFind.get(func() ([]findEntry, error) {
		return m.readFindEntries(ctx, `
			SELECT t.typeID, t.typeName, COALESCE(g.groupName, '') FROM invTypes t
			  LEFT JOIN invGroups g ON g.groupID = t.groupID
			 WHERE t.published = 1 AND t.marketGroupID IS NOT NULL`)
	})
	return fuzzyRank(words, es, limit)
}

// findBlueprints is the blueprints that a few letters could mean; the name may leave out
// "Blueprint".
func (m *Mod) findBlueprints(ctx context.Context, words string, limit int) []findHit {
	es := blueprintFind.get(func() ([]findEntry, error) {
		return m.readFindEntries(ctx, `
			SELECT t.typeID, t.typeName, COALESCE(g.groupName, '') FROM invBlueprintTypes bt
			  JOIN invTypes t ON t.typeID = bt.blueprintTypeID
			  LEFT JOIN invGroups g ON g.groupID = t.groupID
			 WHERE t.published = 1`)
	})
	return fuzzyRank(words, es, limit)
}

// findSystems is the solar systems that a few letters could mean.
func (m *Mod) findSystems(ctx context.Context, words string, limit int) []findHit {
	es := systemFind.get(func() ([]findEntry, error) {
		return m.readFindEntries(ctx, `
			SELECT s.solarSystemID, s.solarSystemName, COALESCE(r.regionName, '') FROM mapSolarSystems s
			  LEFT JOIN mapRegions r ON r.regionID = s.regionID`)
	})
	return fuzzyRank(words, es, limit)
}
