package rolebots

import (
	"context"
	"strings"

	"gurimoon/esp"
)

// A place is where an order is about: a solar system, a constellation or a region. One field on the
// Management page takes any of the three, found by a few letters (corpfind.go). The map's own numbers
// say which a place is, so one ID stands for all three; a system's goes wherever a system's did, and
// an area (a constellation or a region) keeps a fleet inside it (gangSite).

const (
	placeRegion        = "region"
	placeConstellation = "constellation"
	placeSystem        = "system"
)

// placeKindOf is what a map ID is, "" for none. Regions are numbered from ten million, constellations
// from twenty, solar systems from thirty. Pure.
func placeKindOf(id int64) string {
	switch id / 10_000_000 {
	case 1:
		return placeRegion
	case 2:
		return placeConstellation
	case 3:
		return placeSystem
	}
	return ""
}

// isArea reports whether a place holds systems rather than being one. Pure.
func isArea(id int64) bool {
	k := placeKindOf(id)
	return k == placeRegion || k == placeConstellation
}

// splitPlace is a place as a system and an area, one of them 0. Pure.
func splitPlace(id int64) (systemID, areaID int64) {
	if isArea(id) {
		return 0, id
	}
	return id, 0
}

// placeName is a place in words: "Alentene", "Amevync constellation", "Sinq Laison region".
func (m *Mod) placeName(ctx context.Context, id int64) string {
	var name string
	switch placeKindOf(id) {
	case placeRegion:
		_ = m.db.QueryRowContext(ctx, `SELECT regionName FROM mapRegions WHERE regionID = ?`, id).Scan(&name)
	case placeConstellation:
		_ = m.db.QueryRowContext(ctx, `SELECT constellationName FROM mapConstellations WHERE constellationID = ?`, id).Scan(&name)
	default:
		return m.nameOfSystem(ctx, id)
	}
	return strings.TrimSpace(name + " " + placeKindOf(id))
}

// placeCell is a place as a page draws it: a system tinted by its security, an area in plain type.
func (m *Mod) placeCell(ctx context.Context, id int64) string {
	if isArea(id) {
		return igbF3("<b>" + esp.IGBText(m.placeName(ctx, id)) + "</b>")
	}
	return m.systemCell(ctx, id)
}

// holds reports whether a system is a place or lies in it. Pure.
func (u *universe) holds(place, sysID int64) bool {
	ms, ok := u.meta[sysID]
	if !ok || place == 0 {
		return false
	}
	return sysID == place || ms.RegionID == place || ms.ConstellationID == place
}

// areaRegion is the region an area is or lies in, 0 for none.
func (u *universe) areaRegion(area int64) int64 {
	for _, ms := range u.meta {
		if ms.RegionID == area || ms.ConstellationID == area {
			return ms.RegionID
		}
	}
	return 0
}

// areaGround is how many systems of an area a combat fleet could sweep: with belts, in its security
// band, and on the gate network. It is the chair's answer before a fleet is raised for nowhere.
func (m *Mod) areaGround(ctx context.Context, u *universe, area int64, secMin float64) int {
	n := 0
	for id, ms := range u.meta {
		if u.holds(area, id) && ms.Security >= secMin && len(u.adj[id]) > 0 && u.hasBelts(ctx, m.db, id) {
			n++
		}
	}
	return n
}

// areaSystem is the nearest system of an area by gate that take allows, one that first prefers when
// several are as near; 0 when the area has none in reach. Pure but for the calls it is given.
func (u *universe) areaSystem(from, area int64, secFloor float64, take, first func(int64) bool) int64 {
	seen := map[int64]bool{from: true}
	frontier := []int64{from}
	for len(frontier) > 0 {
		var best int64
		bestFirst := false
		var next []int64
		for _, id := range frontier {
			if u.holds(area, id) && take(id) {
				f := first(id)
				if best == 0 || (f && !bestFirst) || (f == bestFirst && id < best) {
					best, bestFirst = id, f
				}
			}
			for _, nb := range u.adj[id] {
				ms, ok := u.meta[nb]
				if !ok || seen[nb] || dispSec(ms.Security) < secFloor {
					continue
				}
				seen[nb] = true
				next = append(next, nb)
			}
		}
		if best != 0 {
			return best
		}
		frontier = next
	}
	return 0
}

// ── finding a place ─────────────────────────────────────────────────────────

var placeFind findIndex

// placeEntries is every region, constellation and system by name. Three reads rather than a UNION,
// which refuses map tables whose names are collated differently.
func (m *Mod) placeEntries(ctx context.Context) []findEntry {
	return placeFind.get(func() ([]findEntry, error) {
		var out []findEntry
		for _, q := range []string{
			`SELECT regionID, regionName, '' FROM mapRegions`,
			`SELECT c.constellationID, c.constellationName, COALESCE(r.regionName, '')
			   FROM mapConstellations c LEFT JOIN mapRegions r ON r.regionID = c.regionID`,
			`SELECT s.solarSystemID, s.solarSystemName, COALESCE(r.regionName, '')
			   FROM mapSolarSystems s LEFT JOIN mapRegions r ON r.regionID = s.regionID`,
		} {
			es, err := m.readFindEntries(ctx, q)
			if err != nil {
				return nil, err
			}
			for _, e := range es {
				in := e.Group
				e.Group = placeKindOf(e.ID)
				if in != "" {
					e.Group += ", " + in
				}
				out = append(out, e)
			}
		}
		return out, nil
	})
}

// cutPlaceKind takes the kind off the end of what was typed: "Sinq Laison (region)", "sinq laison
// region". Pure.
func cutPlaceKind(words string) (name, kind string) {
	words = strings.Join(strings.Fields(words), " ")
	l := strings.ToLower(words)
	for _, k := range []string{placeRegion, placeConstellation, placeSystem} {
		for _, suffix := range []string{" (" + k + ")", " " + k} {
			if strings.HasSuffix(l, suffix) {
				return strings.TrimSpace(words[:len(words)-len(suffix)]), k
			}
		}
	}
	return words, ""
}

// resolvePlace is the one place the words mean, or (ok false) the places they could mean. A name that
// is a region's and a system's both is neither until the kind is said. Pure.
func resolvePlace(words string, entries []findEntry, limit int) (place findEntry, ok bool, hits []findHit) {
	name, kind := cutPlaceKind(words)
	if kind != "" {
		var of []findEntry
		for _, e := range entries {
			if placeKindOf(e.ID) == kind {
				of = append(of, e)
			}
		}
		entries = of
	}
	lower := strings.ToLower(name)
	for _, e := range entries {
		if lower != "" && e.lower == lower {
			hits = append(hits, findHit{e, 1000})
		}
	}
	switch {
	case len(hits) == 1:
		return hits[0].findEntry, true, nil
	case len(hits) > 1:
		return findEntry{}, false, hits
	}
	hits = fuzzyRank(name, entries, limit)
	if findClear(hits) {
		return hits[0].findEntry, true, nil
	}
	return findEntry{}, false, hits
}

// placeWords is what to put in the field for a place so it means that one: the kind is named for an
// area, and for a system whose name another place shares. Pure.
func placeWords(e findEntry, entries []findEntry) string {
	kind := placeKindOf(e.ID)
	if kind == placeSystem {
		shared := false
		for _, o := range entries {
			shared = shared || (o.ID != e.ID && o.lower == e.lower)
		}
		if !shared {
			return e.Name
		}
	}
	return e.Name + " (" + kind + ")"
}

// hintPlace is the hint under a location field.
const hintPlace = "A system, a constellation or a region. Type part of the name, or press Find."

// placeLine is the location field of a form.
func (p *mgPage) placeLine(label, value string) string {
	return p.line(label, "place", value, 24, hintPlace, "location")
}

// offerPlaces sets the page's "did you mean" list to places.
func (p *mgPage) offerPlaces(hits []findHit) {
	entries := p.m.placeEntries(p.ctx)
	p.suggest = p.suggest[:0]
	for _, h := range hits {
		p.suggest = append(p.suggest, p.again(h.Name, "place", placeWords(h.findEntry, entries))+" "+p.pg.dim(esp.IGBText(h.Group)))
	}
}

// placeField reads the location field: the one place the letters mean, else a refusal with the near
// ones to click. An area nothing can be hunted in is refused.
func (p *mgPage) placeField() (id int64, name, bad string) {
	words := strings.TrimSpace(p.q.Get("place"))
	if words == "" {
		return 0, "", "Say where: a system, a constellation or a region."
	}
	e, ok, hits := resolvePlace(words, p.m.placeEntries(p.ctx), mgSuggestMax)
	if !ok {
		p.offerPlaces(hits)
		if len(hits) == 0 {
			return 0, "", "No system, constellation or region looks like \"" + words + "\"."
		}
		return 0, "", "\"" + words + "\" could be more than one place."
	}
	name = p.m.placeName(p.ctx, e.ID)
	if isArea(e.ID) {
		if u, err := p.m.universe(p.ctx); err == nil && p.m.areaGround(p.ctx, u, e.ID, combatConfigFor("").SecMin) == 0 {
			return 0, "", "There is no ground for a fleet in the " + name + ": no system there has belts, a gate and the security for it."
		}
	}
	return e.ID, name, ""
}

// placeNote is what a preview says of a place: a system's security, or how much of an area there is.
func (p *mgPage) placeNote(id int64) string {
	if !isArea(id) {
		return p.m.systemCell(p.ctx, id)
	}
	u, err := p.m.universe(p.ctx)
	if err != nil {
		return ""
	}
	n := p.m.areaGround(p.ctx, u, id, combatConfigFor("").SecMin)
	return p.pg.soft("(" + plural(n, "system", "systems") + " to sweep)")
}
