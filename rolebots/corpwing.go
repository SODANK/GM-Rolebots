package rolebots

import (
	"context"
	"strconv"
	"sync"

	"gurimoon/seclaw"
)

// How many of a corporation's pilots may be under arms at once.
// The doctrine (corpdoctrine.go) says what a wing does; this caps its size. Doctrine comes
// from the violence tag and size from the industrial tag.
// The cap is enforced in the activity opportunity (appetite.go), the chooser's one hard
// gate, so a full wing never wins the draw and the pilot picks another activity.
// The headcount is one grouped query for every corporation, cached for wingCensusSec.

const (
	// wingCensusSec is how long one census of who is under arms stands. It is shorter than a
	// combat stint, so a freed seat is offered again quickly.
	wingCensusSec = 60
	// wingFloor is the smallest wing any classified employer may field. A fraction of a small
	// roster rounds to zero, which would disarm the corporation.
	wingFloor = 1
)

type wingCensus struct {
	at int64
	// underArms is corpID to how many of its pilots are on a combat activity now; roster is
	// corpID to how many enabled bots it employs.
	underArms map[int64]int
	roster    map[int64]int
}

var (
	wingMu     sync.Mutex
	wingCached *wingCensus
	// wingFlight is closed when the census being read finishes; nil when none is in flight. The
	// sim pass runs eight pilots at once and they all find the census expired in the same second,
	// so one reads it and the rest take its answer instead of each grouping the whole roster.
	wingFlight  chan struct{}
	wingFlightC *wingCensus
)

// wingCensusNow returns the current census, refreshing it at most once per wingCensusSec.
func (m *Mod) wingCensusNow(ctx context.Context) *wingCensus {
	now := clock.Unix()
	wingMu.Lock()
	if wingCached != nil && now-wingCached.at < wingCensusSec {
		c := wingCached
		wingMu.Unlock()
		return c
	}
	if ch := wingFlight; ch != nil {
		wingMu.Unlock()
		select {
		case <-ch:
			wingMu.Lock()
			c := wingFlightC
			wingMu.Unlock()
			if c != nil {
				return c
			}
		case <-ctx.Done():
		}
		return &wingCensus{at: now, underArms: map[int64]int{}, roster: map[int64]int{}}
	}
	ch := make(chan struct{})
	wingFlight = ch
	wingMu.Unlock()
	var c *wingCensus
	// Released whatever happens, so a panic in the read cannot leave the others waiting.
	defer func() {
		wingMu.Lock()
		wingFlight, wingFlightC = nil, c
		wingMu.Unlock()
		close(ch)
	}()
	c = m.readWingCensus(ctx, now)
	return c
}

// readWingCensus is one census read. A failed read gives an empty census, which is not cached.
func (m *Mod) readWingCensus(ctx context.Context, now int64) *wingCensus {
	c := &wingCensus{at: now, underArms: map[int64]int{}, roster: map[int64]int{}}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.corporationID, COALESCE(s.activity, ''), COUNT(*)
		FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		LEFT JOIN rolebot_state s ON s.characterID = b.characterID
		WHERE b.enabled = 1
		GROUP BY c.corporationID, COALESCE(s.activity, '')`)
	if err != nil {
		// A failed census returns an empty one. wingAllows reads a missing roster as
		// unclassified and lets the pilot through.
		return c
	}
	defer rows.Close()
	for rows.Next() {
		var corpID int64
		var activity string
		var n int
		if rows.Scan(&corpID, &activity, &n) != nil {
			continue
		}
		c.roster[corpID] += n
		// A pilot mustering for its employer's patrol counts as under arms. Members of a live
		// patrol already count through ActCombat; this arm covers the muster window.
		if activity == ActCombat || activity == ActSecurity || activity == ActPiracy {
			c.underArms[corpID] += n
		}
	}

	wingMu.Lock()
	wingCached = c
	wingMu.Unlock()
	return c
}

// forgetWingCensus drops the cache. The economy purge calls it.
func forgetWingCensus() {
	wingMu.Lock()
	wingCached = nil
	wingMu.Unlock()
}

// wingAllows reports whether this pilot's employer has room for one more gun, and a
// reason string. An unclassified employer (no rolebot_corps row, so no declared policy)
// is always allowed. DoctrineNone is a real answer: that corporation fields no wing.
func (m *Mod) wingAllows(ctx context.Context, b *botRun) (bool, string) {
	if b == nil || b.CorpID == 0 {
		return true, ""
	}
	// An outlaw's record only rises by killing rats (appetite.go pulls combat up for that),
	// so a full wing must not block it.
	if b.SecStatus <= seclaw.OutlawStatus {
		return true, ""
	}
	doc, frac, known := m.corpDoctrineOf(ctx, b.CorpID)
	if !known {
		return true, ""
	}
	// An employer with no wing does not ground its pilots: killing belt rats for bounty is
	// earning. The activity stays allowed; allowsDoctrine and pickCombatTactic keep the tactics shut.
	if doc == DoctrineNone || frac <= 0 {
		return true, ""
	}

	// A pilot already under arms holds a seat and is not refused, so a full wing does not
	// empty itself.
	if b.State != nil && (b.State.Activity == ActCombat ||
		b.State.Activity == ActSecurity || b.State.Activity == ActPiracy) {
		return true, ""
	}

	c := m.wingCensusNow(ctx)
	roster := c.roster[b.CorpID]
	if roster == 0 {
		return true, ""
	}
	limit := int(float64(roster)*frac + 0.5)
	if limit < wingFloor {
		limit = wingFloor
	}
	if c.underArms[b.CorpID] < limit {
		return true, ""
	}
	return false, "employer's " + doc.String() + " wing is at strength (" +
		strconv.Itoa(limit) + " of " + strconv.Itoa(roster) + ")"
}

func init() {
	// The census counts pilots per corporation; either kind of departure changes it.
	registerCharForget("wingCached", func(int64) { forgetWingCensus() })
	registerCorpForget("wingCached", func(int64) { forgetWingCensus() })
}
