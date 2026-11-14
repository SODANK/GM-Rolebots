//go:build mindclash

package rolebots

// Mind Clash: the bettors and the fans (docs/MIND_CLASH_BOTS.md §8-§9).
// Bettors are greedy pilots (Envy above 30) with money over their floor. Each stakes
// wallet x (0.5% + Envy x 0.02%) on a card, spread over two or three fights: about 60% after
// the card locks, the rest once the weigh-in is published. Their model is the rating gap and
// the tale of the tape, bent by three biases that misprice the pools:
//   - fandom: a lean toward a fighter of their own race or corporation;
//   - favourite-longshot: favourites and big names are overbacked;
//   - recency: the last result counts too much.
// Fans: a bot with 5,000,000 ISK or more on one fight, within eight jumps and idle, may fly
// to the venue and dock there. Capped per card; fans are never un-benched.

import (
	"context"
	"database/sql"
	"math"
	"math/rand"
	"sort"

	evedb "gurimoon/db"
)

const (
	clashBetMinGreed    = 30
	clashBettorsPerPass = 250
	// clashBettorSlicePct is the stable share of greedy pilots who bet on any
	// one card: about 1,500 of a full roster's ~3,500, as the design asks,
	// and a different 45% each card.
	clashBettorSlicePct = 45
	clashFanStake       = 5_000_000.0
	clashFanMaxJumps    = 8
)

// clashBetPass runs one round of betting on every card whose tote is open.
func (m *Mod) clashBetPass(ctx context.Context) {
	if clashBetMul() <= 0 {
		return
	}
	cards, err := evedb.MCUpcomingCards(ctx, m.db)
	if err != nil {
		return
	}
	rng := rand.New(rand.NewSource(clock.Now().UnixNano()))
	for _, c := range cards {
		phase := -1
		switch c.State {
		case evedb.MCCardLocked:
			phase = 0
		case evedb.MCCardWeighed, evedb.MCCardCheckin:
			phase = 1
		}
		if phase < 0 {
			continue
		}
		m.clashBetCard(ctx, c, phase, rng)
		if c.State == evedb.MCCardCheckin || (c.State == evedb.MCCardWeighed && c.FirstBell-clock.Unix() < 6*3600) {
			m.clashFans(ctx, c)
		}
	}
}

type clashMarketView struct {
	m      *evedb.MCMarket
	slot   *evedb.MCCardSlot
	red    *evedb.MCClasher
	blue   *evedb.MCClasher
	pRed   float64 // the bettors' common model, before biases
	wiredR bool
	wiredB bool
}

func (m *Mod) clashBetCard(ctx context.Context, c *evedb.MCCard, phase int, rng *rand.Rand) {
	mkts, err := evedb.MCCardMarkets(ctx, m.db, c.ID)
	if err != nil {
		return
	}
	slots, _ := evedb.MCCardSlots(ctx, m.db, c.ID)
	bySlot := map[int64]*evedb.MCCardSlot{}
	var ids []int64
	for _, s := range slots {
		bySlot[s.ID] = s
		ids = append(ids, s.RedID, s.BlueID)
	}
	cs, _ := evedb.MCGetClashers(ctx, m.db, ids)
	var views []clashMarketView
	for _, mk := range mkts {
		if mk.State != "open" || (mk.Kind != evedb.MCMktWinner && mk.Kind != evedb.MCMktMethod) {
			continue
		}
		s := bySlot[mk.SlotID]
		if s == nil || cs[s.RedID] == nil || cs[s.BlueID] == nil {
			continue
		}
		v := clashMarketView{m: mk, slot: s, red: cs[s.RedID], blue: cs[s.BlueID]}
		v.pRed = evedb.MCWinProb(v.red.Rating, v.red.RD, v.blue.Rating, v.blue.RD)
		if phase == 1 {
			// The weigh-in is published: wired fighters, condition and form
			// move it.
			v.wiredR = evedb.MCGetWeighIn(ctx, m.db, s.ID, s.RedID).Wired
			v.wiredB = evedb.MCGetWeighIn(ctx, m.db, s.ID, s.BlueID).Wired
			if v.wiredR {
				v.pRed += 0.06
			}
			if v.wiredB {
				v.pRed -= 0.06
			}
			if v.red.Condition() == "Battered" {
				v.pRed -= 0.05
			}
			if v.blue.Condition() == "Battered" {
				v.pRed += 0.05
			}
			v.pRed += 0.03*v.red.Form - 0.03*v.blue.Form
		}
		v.pRed = clampF(v.pRed, 0.05, 0.95)
		views = append(views, v)
	}
	if len(views) == 0 {
		return
	}
	// The bettors: greedy pilots with money, not yet bet in this phase,
	// online.
	rows, err := m.db.QueryContext(ctx, `SELECT r.characterID, c.balance, t.value, COALESCE(b.raceID, 0), c.corporationID
		FROM rolebot_characters r
		JOIN character_ c ON c.characterID = r.characterID
		JOIN rolebot_traits t ON t.characterID = r.characterID AND t.trait = ? AND t.value > ?
		LEFT JOIN chrBloodlines b ON b.bloodlineID = c.bloodlineID
		WHERE r.enabled = 1 AND c.balance > 1000000
		AND MOD(r.characterID + ?, 100) < ?
		AND NOT EXISTS (SELECT 1 FROM rolebot_clash_bets x WHERE x.cardID = ? AND x.characterID = r.characterID AND x.phase = ?)
		ORDER BY RAND() LIMIT ?`, TraitGreed, clashBetMinGreed, c.ID, clashBettorSlicePct, c.ID, phase, clashBettorsPerPass)
	if err != nil {
		return
	}
	type bettor struct {
		id, corp int64
		bal      float64
		greed    int
		race     int
	}
	var bs []bettor
	for rows.Next() {
		var b bettor
		if rows.Scan(&b.id, &b.bal, &b.greed, &b.race, &b.corp) == nil && !popIsOffline(b.id) {
			bs = append(bs, b)
		}
	}
	rows.Close()
	rules := evedb.LoadMCRules(ctx, m.db)
	placed := 0
	for _, b := range bs {
		allowance := clashAllowance(spendable(b.bal), b.greed, phase, clashBetMul())
		if allowance < rules.BetMin {
			_, _ = m.db.ExecContext(ctx, `INSERT IGNORE INTO rolebot_clash_bets (cardID, characterID, phase, at) VALUES (?, ?, ?, ?)`,
				c.ID, b.id, phase, clock.Unix())
			continue
		}
		n := 2 + rng.Intn(2)
		staked := 0.0
		picks := clashPickBets(views, b.race, b.corp, n, rng)
		for _, p := range picks {
			stake := math.Floor(allowance / float64(len(picks)))
			if stake < rules.BetMin {
				continue
			}
			if _, err := evedb.MCPlaceBet(ctx, m.db, b.id, p.marketID, p.outcome, stake); err == nil {
				staked += stake
				placed++
			}
		}
		_, _ = m.db.ExecContext(ctx, `INSERT INTO rolebot_clash_bets (cardID, characterID, phase, staked, at) VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE staked = staked + VALUES(staked)`, c.ID, b.id, phase, staked, clock.Unix())
	}
	if placed > 0 {
		rlogf("rolebots: Mind Clash — %d bets on card %d (phase %d)", placed, c.ID, phase)
	}
}

// clashAllowance is a bettor's stake on one card in one phase: wallet ×
// (0.5% + Envy × 0.02%), 60% of it after lock and 40% after the weigh-in.
func clashAllowance(spendable float64, greed, phase int, mul float64) float64 {
	a := spendable * (0.005 + float64(greed)*0.0002) * mul
	if phase == 0 {
		return a * 0.6
	}
	return a * 0.4
}

type clashPick struct {
	marketID int64
	outcome  string
	value    float64
}

// clashPickBets is one bettor's judgement: their biased probability against
// the pool's price, best value first, with some noise so the crowd does not
// move as one.
func clashPickBets(views []clashMarketView, race int, corp int64, n int, rng *rand.Rand) []clashPick {
	var all []clashPick
	for _, v := range views {
		p := v.pRed
		// fandom: the empire's own, and the host's own stable
		if v.red.Race == race || (corp != 0 && v.red.CorpID == corp) {
			p += 0.08
		}
		if v.blue.Race == race || (corp != 0 && v.blue.CorpID == corp) {
			p -= 0.08
		}
		// recency: the last result counts too much
		p += clashRecency(v.red) - clashRecency(v.blue)
		p = clampF(p, 0.03, 0.97)
		// favourite-longshot: favourites look surer than they are
		a, b := math.Pow(p, 1.4), math.Pow(1-p, 1.4)
		p = a / (a + b)
		const draw = 0.04
		probs := map[string]float64{"red": p * (1 - draw), "blue": (1 - p) * (1 - draw), "draw": draw}
		if v.m.Kind == evedb.MCMktMethod {
			stop := 0.45
			probs = map[string]float64{"red_stop": p * (1 - draw) * stop, "red_dec": p * (1 - draw) * (1 - stop),
				"blue_stop": (1 - p) * (1 - draw) * stop, "blue_dec": (1 - p) * (1 - draw) * (1 - stop), "draw": draw}
		}
		for o, q := range probs {
			odds := v.m.Odds(o)
			if odds <= 0 {
				odds = v.m.Opening[o]
			}
			if odds <= 0 {
				odds = 1 / math.Max(q, 0.02)
			}
			val := q*odds - 1 + rng.NormFloat64()*0.15
			if v.m.Kind == evedb.MCMktMethod {
				val -= 0.1 // most bettors stick to the winner
			}
			all = append(all, clashPick{v.m.ID, o, val})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].value > all[j].value })
	var out []clashPick
	usedMarket := map[int64]bool{}
	for _, p := range all {
		if len(out) >= n {
			break
		}
		if usedMarket[p.marketID] {
			continue
		}
		usedMarket[p.marketID] = true
		out = append(out, p)
	}
	return out
}

func clampF(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

// clashRecency is the last result's pull: a fresh win looks like form.
func clashRecency(c *evedb.MCClasher) float64 {
	if c.Wins+c.Losses+c.Draws == 0 || evedb.MCNow()-c.LastFightAt > 21*86400 {
		return 0
	}
	if c.Wins >= c.Losses {
		return 0.04
	}
	return -0.04
}

// clashFans sends the biggest backers of a fight to the venue.
func (m *Mod) clashFans(ctx context.Context, c *evedb.MCCard) {
	limit := int(clashMaxFans.Load())
	if limit <= 0 {
		return
	}
	var already int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_clash WHERE errandCard = ? AND errandKind = 'fan'`, c.ID).Scan(&already)
	if already >= limit {
		return
	}
	venueSys, err := evedb.GetSolarSystemForStation(ctx, m.db, c.VenueStationID)
	if err != nil {
		return
	}
	rows, err := m.db.QueryContext(ctx, `SELECT b.bettorID, m.slotID, SUM(b.stake) s FROM mcToteBets b
		JOIN mcToteMarkets m ON m.marketID = b.marketID
		JOIN rolebot_characters r ON r.characterID = b.bettorID AND r.enabled = 1
		WHERE m.cardID = ? AND b.state = 'open'
		GROUP BY b.bettorID, m.slotID HAVING s >= ? ORDER BY s DESC LIMIT 200`, c.ID, clashFanStake)
	if err != nil {
		return
	}
	type fan struct{ id, slot int64 }
	var fans []fan
	for rows.Next() {
		var f fan
		var s float64
		if rows.Scan(&f.id, &f.slot, &s) == nil {
			fans = append(fans, f)
		}
	}
	rows.Close()
	sent := 0
	for _, f := range fans {
		if already+sent >= limit {
			break
		}
		if popIsOffline(f.id) {
			continue // fans get no un-benching
		}
		clashErrandMu.RLock()
		_, busy := clashErrands[f.id]
		clashErrandMu.RUnlock()
		if busy {
			continue
		}
		var phase string
		var sys sql.NullInt64
		_ = m.db.QueryRowContext(ctx, `SELECT phase FROM rolebot_state WHERE characterID = ?`, f.id).Scan(&phase)
		if phase != "" && phase != string(TaskIdle) {
			continue // busy with another task
		}
		_ = m.db.QueryRowContext(ctx, `SELECT solarSystemID FROM character_ WHERE characterID = ?`, f.id).Scan(&sys)
		if path, err := m.route(ctx, sys.Int64, venueSys, travelRules{}); err != nil || len(path) > clashFanMaxJumps {
			continue
		}
		m.clashSetErrand(ctx, f.id, c, f.slot, "fan")
		m.logBot(f.id, logInfo, "clash", "heads to %s to cheer from the house", c.VenueName)
		sent++
	}
}
