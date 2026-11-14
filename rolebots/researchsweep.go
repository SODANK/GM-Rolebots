package rolebots

// The unclaimed-patent sweep. A win in an R&D patent lottery is a blueprint
// waiting in an agent's station with an expiry. Only researchCollectDue fetches
// one, and it is a throttled per-pilot check reached when the pilot is idle, so
// a pilot busy with something long, or with a disabled roster row, looks late or
// never. The sweep walks the offers instead and clears the throttle stamp of
// each holder, so the next planning pass collects. TaskRndHaul then stows the
// original in the factory hangar, where labPickPrint finds it; labResearchSkillsMet
// (labprints.go) keeps pilots from researching a print they lack skills for.

import (
	"context"

	evedb "gurimoon/db"
)

// sweepUnclaimedPatents nudges every bot holding an uncollected patent into
// looking at it on its next planning pass. It writes only the throttle stamp:
// ClaimPatentOffer needs the pilot at the agent's station, so the sweep cannot
// shorten the trip, only the wait.
func (m *Mod) sweepUnclaimedPatents(ctx context.Context) {
	if m.db == nil {
		return
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.charID, o.offerID
		  FROM rndPatentOffers o
		  JOIN rolebot_characters r ON r.characterID = o.charID
		 WHERE o.state = ? AND o.expiresAt > ?
		 ORDER BY o.expiresAt`, evedb.PatentOfferPending, clock.Unix())
	if err != nil {
		rlogf("rolebots: unclaimed patent sweep: %v", err)
		return
	}
	type held struct{ charID, offerID int64 }
	var pending []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.charID, &h.offerID); err != nil {
			continue
		}
		pending = append(pending, h)
	}
	rows.Close()
	if len(pending) == 0 {
		return
	}

	nudged, seen := 0, map[int64]bool{}
	for _, h := range pending {
		if ctx.Err() != nil {
			return
		}
		if seen[h.charID] {
			continue // researchCollectDue takes them soonest-to-lapse first
		}
		seen[h.charID] = true
		st, err := loadState(ctx, m.db, h.charID)
		if err != nil {
			continue // disabled or purged between the two queries
		}
		if st.Data.RndOfferID == h.offerID {
			continue // already on its way
		}
		if st.Data.RndCheckedAt == 0 {
			continue // already due; writing the same zero back is a wasted row
		}
		st.Data.RndCheckedAt = 0
		if err := saveState(ctx, m.db, st); err != nil {
			rlogf("rolebots: unclaimed patent sweep charID=%d: %v", h.charID, err)
			continue
		}
		nudged++
	}
	if nudged > 0 {
		rlogf("rolebots: %d bot(s) hold an uncollected Tech II patent — sending them to look now (%d offer(s) outstanding)",
			nudged, len(pending))
	}
}

// reportStrayBlueprints counts, once at boot, the blueprint capital sitting where
// its owner cannot see it. It repairs nothing: labFetchStray brings a print home,
// one trip at a time. A figure that falls between restarts means that routine works.
func (m *Mod) reportStrayBlueprints(ctx context.Context) {
	if m.db == nil {
		return
	}
	var prints, bots int64
	err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT e.ownerID)
		  FROM entity e
		  JOIN rolebot_characters r ON r.characterID = e.ownerID
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  JOIN rolebot_profiles p ON p.characterID = e.ownerID
		 WHERE e.flag = 4 AND p.industryStationID <> 0
		   AND e.locationID <> p.industryStationID`).Scan(&prints, &bots)
	if err != nil {
		rlogf("rolebots: stray blueprint census: %v", err)
		return
	}
	if prints == 0 {
		return
	}
	rlogf("rolebots: %d blueprint(s) across %d pilot(s) are sitting outside their own factory — "+
		"invisible to both the build book and the lab until they are fetched home", prints, bots)
}
