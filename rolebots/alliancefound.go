package rolebots

// Founding an alliance: a leader with allies starting a bloc of its own.
//
// Who: a leader with two allies at seekAllyAt, no war, and the 1B fee co-funded by its eager allies, the
// richest founding with a third in hand (allianceFoundVerdict, allianceCoFund; docs/ALLIANCES.md).
// When: at the end of the diplomacy pass (corpdiplomacy_pass.go) and from the seeker. Gates: the founding
// switch (settingAllianceFound), the cap (settingAllianceFoundMax) and one global pace, as in corpfound.go.
// What: Create Alliance minus the wire: refusal gates, allies' shares to the founder, the fee (both
// refunded on failure), the alliance with the founder as executor, wars adopted, allies invited by letter.
// Names never use a client logo slot (db/alliancelogoslots.go) or an official alliance's name or ticker;
// the generator is checked by corpbrand.go brandProblem.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	evedb "gurimoon/db"
)

const (
	// allianceFoundPaceSec is the least time between two foundings,
	// server-wide.
	allianceFoundPaceSec = 600
	// allianceFoundRerolls bounds the name search.
	allianceFoundRerolls = 48
	allianceFoundFee     = evedb.AllianceCreationCost
)

var (
	allianceFoundLastAt atomic.Int64
	// allianceFoundMu: one founding at a time. Two CEO thinks on different
	// workers passing the cap check together would both found.
	allianceFoundMu sync.Mutex
)

// liveFoundedAlliances counts the bot-founded alliances still standing.
func (m *Mod) liveFoundedAlliances(ctx context.Context) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_founded_alliances f
		  JOIN alliances a ON a.allianceID = f.allianceID AND a.deleted = 0`).Scan(&n)
	return n
}

// allianceConsiderFounding is the decision, from the diplomacy pass
// (ceoConsiderAlliance) and the seeker alike. Nobody's fault when it does
// not found; it only ever logs when it does, or when the fee was taken.
func (m *Mod) allianceConsiderFounding(ctx context.Context, c *corpCEO, ch Charter, allies []int64) error {
	if !allianceFoundOn() {
		return nil
	}
	// The caller's allies (StandingDevoted from diplomacy) widened to the seeker's line.
	allies = allianceUnion(allies, relAlliesAt(c.CorpID, allianceSeekAllyAt()))
	// The cheap gates first: both verdicts need two allies, and the pace and the cap bind either way.
	if len(allies) < 2 {
		return nil
	}
	if last := allianceFoundLastAt.Load(); last > 0 && clock.Unix()-last < allianceFoundPaceSec {
		return nil
	}
	if m.liveFoundedAlliances(ctx) >= allianceFoundMax() {
		return nil
	}
	var declared int64
	if n, err := evedb.CountActiveDeclaredWars(ctx, m.db, c.CorpID); err == nil {
		declared = n
	}
	if declared > 0 {
		return nil // the core refuses a founder with declared wars
	}
	// All free capital, not the war chest's share: a founding is a one-off spend and the chest fraction
	// would bar every corporation under 4B.
	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return nil
	}
	cluster := append([]allianceFunder{{CorpID: c.CorpID, Free: free}}, m.allianceFoundCluster(ctx, allies)...)
	ok, why, shares := allianceFoundVerdict(ch.AllianceAppetite, len(allies), c.CorpID, cluster, allianceFoundFee, int(declared))
	motive := ""
	if !ok {
		// Not a leader's bloc, but maybe an alliance to hold 0.0 (frontier.go): tower-hungry allies
		// with a foot on the frontier.
		ready := m.corpMinesAMoon(ctx, c.CorpID) || m.forwardOfficeOf(ctx, c.CorpID) != 0
		towerAllies := 0
		for _, a := range allies {
			if m.corpMinesAMoon(ctx, a) || m.forwardOfficeOf(ctx, a) != 0 {
				towerAllies++
			}
		}
		// The same co-funding; a cluster that would not fund it leaves the founder paying alone.
		chest := free
		cs, total, fundOK, _ := allianceCoFund(c.CorpID, cluster, allianceFoundFee)
		if fundOK {
			chest = total
		}
		ok, why = sovFoundVerdict(ch.AllianceAppetite, ch.PosAppetite, len(allies), towerAllies, ready,
			chest, allianceFoundFee, int(declared))
		if !ok {
			return nil
		}
		shares = nil
		if fundOK {
			shares = cs
		}
		motive = foundMotiveSov
	}
	if !allianceFoundMu.TryLock() {
		return nil
	}
	defer allianceFoundMu.Unlock()
	// Again under the lock: another worker may have founded since the cheap gates.
	now := clock.Unix()
	if last := allianceFoundLastAt.Load(); last > 0 && now-last < allianceFoundPaceSec {
		return nil
	}
	if m.liveFoundedAlliances(ctx) >= allianceFoundMax() {
		return nil
	}
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return nil
	}
	name, ticker, ok := m.pickAllianceName(ctx, c, *facts)
	if !ok {
		m.logBot(c.CharID, logInfo, "ceo", "alliance founding: no free name after %d rerolls", allianceFoundRerolls)
		return nil
	}
	if err := evedb.AllianceCreateRefusal(ctx, m.db, c.CorpID, name, ticker); err != nil {
		m.logBot(c.CharID, logInfo, "ceo", "alliance founding refused: %v", err)
		return nil
	}
	// The allies' shares come in first, so the founder's wallet holds the whole fee when CONCORD charges it.
	paid := m.allianceCollectShares(ctx, c, name, shares)
	if err := evedb.ChargeAllianceCreation(ctx, m.db, c.CorpID, c.CharID); err != nil {
		m.logBot(c.CharID, logInfo, "ceo", "alliance founding: %v", err)
		m.allianceReturnShares(ctx, c, name, paid)
		return nil
	}
	desc := fmt.Sprintf("%s, founded by %s and its allies.", name, facts.Name)
	if motive == foundMotiveSov {
		desc = fmt.Sprintf("%s, founded by %s and its allies to hold unclaimed 0.0.", name, facts.Name)
	}
	allianceID, err := evedb.CreateAlliance(ctx, m.db, c.CorpID, c.CharID, name, ticker, desc, "")
	if err != nil {
		m.logBot(c.CharID, logError, "ceo", "alliance founding %q: %v (refunding the fee)", name, err)
		if rerr := evedb.RefundAllianceCreation(ctx, m.db, c.CorpID); rerr != nil {
			m.logBot(c.CharID, logError, "ceo", "alliance founding refund: %v", rerr)
		}
		m.allianceReturnShares(ctx, c, name, paid)
		return nil
	}
	allianceFoundLastAt.Store(now)
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_founded_alliances (allianceID, corporationID, foundedAt, motive) VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), foundedAt = VALUES(foundedAt), motive = VALUES(motive)`,
		allianceID, c.CorpID, now, motive); err != nil {
		rlogf("rolebots: founded alliances row: %v", err)
	}
	// What the wire does after CreateAlliance (cmd/gurimoon/main.go handleBoundCreateAlliance): the name
	// bulks, the session change, and the defensive wars moved onto the alliance.
	m.allianceInvalidateNames()
	m.allianceMembership(c.CorpID, 0, allianceID)
	m.allianceChanged("details", allianceID, 0)
	if adopted, err := evedb.AdoptCorpWars(ctx, m.db, c.CorpID, allianceID); err == nil {
		m.allianceAnnounceAdoptions(adopted)
	}
	m.allianceInvalidateWarCache()
	m.refreshRelations(ctx)
	m.corpBook(ctx, c.CorpID, lineAllianceWar, -allianceFoundFee, evedb.OwnerCONCORDBills, "alliance founding fee")
	m.ceoLog(ctx, c, powerAlliance, "found", allianceID, allianceFoundFee, "founded %s [%s] — %s", name, ticker, why)
	m.allianceBook(ctx, allianceID, c.CorpID, "founded", c.CorpID, allianceFoundFee, "%s founded %s [%s]", facts.Name, name, ticker)
	for _, a := range allianceSortedIDs(paid) {
		amt := paid[a]
		m.corpBook(ctx, c.CorpID, lineCapital, amt, a, "alliance founding share")
		m.corpBook(ctx, a, lineAllianceFound, -amt, c.CorpID, "founding share of "+name)
		m.allianceBook(ctx, allianceID, a, "cofund", c.CorpID, amt, "%s put %s into the founding", ownerNameOf(ctx, m.db, a), iskString(amt))
	}
	// The invitations. Applications run corporation to alliance only, so allies apply themselves
	// (alliance.go allianceSeek); the letter is the nudge.
	for _, a := range allies {
		var ceoID int64
		if m.db.QueryRowContext(ctx, `SELECT ceoID FROM corporation WHERE corporationID = ?`, a).Scan(&ceoID) != nil || ceoID == 0 {
			continue
		}
		body := fmt.Sprintf("%s has founded %s [%s] and asks its allies to fly under one banner. Your corporation is welcome to apply.", facts.Name, name, ticker)
		if amt := paid[a]; amt > 0 {
			body = fmt.Sprintf("%s has founded %s [%s] with your %s share of the charter fee. Your corporation is welcome to apply.", facts.Name, name, ticker, iskString(amt))
		}
		m.mailPilot(ctx, ceoID, c.CharID, fmt.Sprintf("%s invites you to %s", facts.Name, name), body)
	}
	return nil
}

// ── the co-funding ──────────────────────────────────────────────────────────

// lineAllianceFound is an ally's share of a founding fee on its own books; the founder books the same
// sum as lineCapital, so it is not read as earnings.
const lineAllianceFound = "alliance"

func init() { corpLineLabel[lineAllianceFound] = "Alliance founding" }

// allianceFoundCluster is the allies that would put money into a founding: managed, in no alliance, no
// declared war, and an AllianceAppetite of allianceFoundAppetite or more. Each comes with its free
// capital under its own charter.
func (m *Mod) allianceFoundCluster(ctx context.Context, allies []int64) []allianceFunder {
	var out []allianceFunder
	for _, a := range allies {
		ceoID, ok := managedCEO(a)
		if !ok || ceoID == 0 || m.corpAllianceID(ctx, a) != 0 {
			continue
		}
		if n, err := evedb.CountActiveDeclaredWars(ctx, m.db, a); err != nil || n > 0 {
			continue
		}
		ach := ceoCharterOf(ctx, m, &corpCEO{CorpID: a, CharID: ceoID})
		if ach.AllianceAppetite < allianceFoundAppetite {
			continue
		}
		free, _, err := m.corpFreeCapital(ctx, a, ach)
		if err != nil || free <= 0 {
			continue
		}
		out = append(out, allianceFunder{CorpID: a, Free: free})
	}
	return out
}

// allianceCollectShares moves each ally's share into the founder's wallet. An ally whose transfer fails is
// skipped; the fee charge then refuses when the wallet falls short. paid is what arrived.
func (m *Mod) allianceCollectShares(ctx context.Context, c *corpCEO, name string, shares map[int64]float64) map[int64]float64 {
	paid := map[int64]float64{}
	for _, a := range allianceSortedIDs(shares) {
		amt := shares[a]
		if _, _, err := m.corpWithdraw(ctx, a, c.CorpID, amt, evedb.JournalReason("Alliance founding: "+name)); err != nil {
			m.logBot(c.CharID, logInfo, "ceo", "alliance founding: share of %s from %d: %v", iskString(amt), a, err)
			continue
		}
		paid[a] = amt
	}
	return paid
}

// allianceReturnShares pays the collected shares back when the founding fails.
func (m *Mod) allianceReturnShares(ctx context.Context, c *corpCEO, name string, paid map[int64]float64) {
	for _, a := range allianceSortedIDs(paid) {
		if _, _, err := m.corpWithdraw(ctx, c.CorpID, a, paid[a], evedb.JournalReason("Alliance founding returned: "+name)); err != nil {
			m.logBot(c.CharID, logError, "ceo", "alliance founding: returning %s to %d: %v", iskString(paid[a]), a, err)
		}
	}
}

// allianceSortedIDs is a map's keys in ascending order.
func allianceSortedIDs(m map[int64]float64) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// allianceUnion merges two ID lists, sorted, without repeats.
func allianceUnion(a, b []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, l := range [][]int64{a, b} {
		for _, id := range l {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ── the name ────────────────────────────────────────────────────────────────

// Alliance vocabulary. Organisation words a bloc of corporations is called
// by; none of them is a faction or region word of New Eden's own (those are
// in the lore list brandProblem checks).
var (
	allianceOrgs = []string{"Coalition", "Pact", "Accord", "Union", "Compact", "League", "Covenant",
		"Bloc", "Assembly", "Combine", "Entente", "Concordat", "Consortium", "Confederacy"}
	allianceAdjs = []string{"Free", "United", "Northern", "Southern", "Eastern", "Western", "Outer",
		"Greater", "Allied", "Open", "Sovereign", "Common", "Mutual", "Border", "Frontier"}
	allianceNouns = []string{"Traders", "Miners", "Industries", "Captains", "Pilots", "Holdings",
		"Stations", "Routes", "Belts", "Frontiers", "Colonies", "Shipyards", "Marches", "Reaches", "Lanes"}
)

// generateAllianceName builds one candidate from the founder's region and a
// reroll. Deterministic. Four templates, picked by the reroll.
func generateAllianceName(founderID int64, region string, reroll int) string {
	h := func(slot string) int { return tvHash(founderID, fmt.Sprintf("alliance|%d|%s", reroll, slot)) }
	org := allianceOrgs[h("org")%len(allianceOrgs)]
	adj := allianceAdjs[h("adj")%len(allianceAdjs)]
	noun := allianceNouns[h("noun")%len(allianceNouns)]
	// The region as it leads a name ("Forge", "Lonetrek"; "" for a region
	// never named) and as it follows one ("of the Forge").
	lead := regionLead(region, "")
	of := regionOf(region)
	switch reroll % 4 {
	case 0:
		if lead != "" {
			return lead + " " + org
		}
		fallthrough
	case 1:
		return adj + " " + noun + " " + org
	case 2:
		if of != "" {
			return org + " " + of
		}
		fallthrough
	default:
		return org + " of " + adj + " " + noun
	}
}

// pickAllianceName walks the rerolls until a name passes the lore check, the
// client's shape rules and the taken list, then finds it a ticker the same
// way. ok is false when none did.
func (m *Mod) pickAllianceName(ctx context.Context, c *corpCEO, facts CorpFacts) (name, ticker string, ok bool) {
	lore := m.brandLoreNow(ctx)
	for r := 0; r < allianceFoundRerolls; r++ {
		cand := generateAllianceName(c.CorpID, facts.RegionName, r)
		if brandProblem(cand, lore) != "" || evedb.AllianceNameRefusal(cand) != nil {
			continue
		}
		for _, t := range evedb.SuggestedAllianceShortNames(ctx, m.db, cand) {
			t = strings.ToUpper(strings.TrimSpace(t))
			if evedb.AllianceTickerRefusal(t) != nil {
				continue
			}
			if evedb.AllianceNamesTaken(ctx, m.db, cand, t) == nil {
				return cand, t, true
			}
		}
	}
	return "", "", false
}
