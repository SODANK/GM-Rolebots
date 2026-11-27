package rolebots

import (
	"context"
	"math"
	"math/rand"
	"sync/atomic"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
	"gurimoon/seclaw"
)

// Law in the tier that has no grid. The abstract bot-versus-bot passes
// (combat_botvbot.go, combat_engage.go) resolve fights with nobody watching, so
// three pieces apply the law there: huntLegal (rules of engagement for bounty
// hunts), abstractConcord (the response stopwatch) and the law bounty sweep.

const (
	// lawBountyMinSec is where the law starts paying for a head: a pilot with a
	// repeated record, not a single exemption-ladder miss.
	lawBountyMinSec = -2.0
	// lawBountyMaxPoints caps the multiplier, so the board never holds a price
	// the faucet cannot pay.
	lawBountyMaxPoints = 10
	// lawBountyEveryTicks is the sweep cadence.
	lawBountyEveryTicks = 600

	// concordShipTypeID is the hull the killmail names, the same cruiser the
	// core's response wave sends (cmd/gurimoon concord.go).
	concordShipTypeID = 3885
	concordName       = "CONCORD Police Captain"
)

var (
	lawBountyBusy   atomic.Bool
	concordKillsAll atomic.Int64 // counted forward since boot, for the ESP line
)

// concordOn reports the abstract CONCORD switch: 0 is off, anything else on.
func concordOn() bool          { return dialConcordRate.Get() > 0 }
func lawBountyPerSec() float64 { return math.Floor(dialLawBounty.Get()) }

// ── the hunt's rules of engagement ──────────────────────────────────────────

// huntMark holds the facts that make a bountied pilot a legal target in policed
// space. A bounty alone is a reason to chase, not a licence.
type huntMark struct {
	Outlaw bool // security record at or below the outlaw line
	Heat   bool // a live criminal flag (the only one this tier can see)
	AtWar  bool // the hunter's owners and the mark's are at war
}

// huntLegal answers whether a bounty hunt may be flown at this mark in this
// band. Pure. Outside policed space the price is enough. Inside it only the
// three facts roeAllows recognises count, so hunts and engagements agree.
func huntLegal(doc Doctrine, band SecBand, mk huntMark) (bool, string) {
	if doc == DoctrineNone {
		return false, "no combat wing"
	}
	if band != BandPoliced {
		return true, "bounty outside policed space"
	}
	switch {
	case mk.Outlaw:
		return true, "outlaw"
	case mk.Heat:
		return true, "criminal"
	case mk.AtWar:
		return true, "at war"
	}
	return false, "policed space"
}

// huntMarkLegal reads the mark's facts and asks huntLegal. tb is the mark's
// loaded run; a nil State has no heat.
func (m *Mod) huntMarkLegal(ctx context.Context, lb *botRun, doc Doctrine, tb *botRun) bool {
	if tb == nil {
		return false
	}
	band := secBand(m.systemSecurity(ctx, tb.SystemID))
	mk := huntMark{Outlaw: tb.SecStatus <= seclaw.OutlawStatus}
	if tb.State != nil {
		mk.Heat = m.piracyHeatUntil(tb) > clock.Unix()
	}
	if warEverywhere() {
		mk.AtWar = relAtWar(lb.CorpID, m.corpAllianceID(ctx, lb.CorpID),
			tb.CorpID, m.corpAllianceID(ctx, tb.CorpID))
	}
	ok, _ := huntLegal(doc, band, mk)
	return ok
}

// ── who in a fleet may fire ─────────────────────────────────────────────────

// lawfulShot answers whether a pilot may open fire on a mark in this band. Pure. Outside
// policed space nothing here stops a shot. Inside it the mark must be an outlaw, carry a
// criminal flag, or be at war with the shooter's own corp or alliance.
func lawfulShot(band SecBand, markOutlaw, markCriminal, atWar bool) bool {
	return band != BandPoliced || markOutlaw || markCriminal || atWar
}

// lawfulAttackers is the part of a side that may open fire on every pilot of the other side
// in sysID. A fleet picks its mark on its commander's war (roe.go), but the core judges each
// shot on the shooter's own corp and alliance, so a gangmate from a corp outside that war
// stands aside instead of committing a crime. Outside policed space the side is returned
// whole.
func (m *Mod) lawfulAttackers(ctx context.Context, sysID int64, band SecBand, ours, theirs []*pvpCombatant) []*pvpCombatant {
	if band != BandPoliced {
		return ours
	}
	out := make([]*pvpCombatant, 0, len(ours))
	for _, a := range ours {
		if a == nil {
			continue
		}
		lawful := true
		for _, v := range theirs {
			if v == nil {
				continue
			}
			atWar := warEverywhere() && relAtWar(a.CorpID, m.corpAllianceID(ctx, a.CorpID),
				v.CorpID, m.corpAllianceID(ctx, v.CorpID))
			if !lawfulShot(band, v.SecStatus <= seclaw.OutlawStatus, criminalSince(sysID, v.CharID) > 0, atWar) {
				lawful = false
				break
			}
		}
		if lawful {
			out = append(out, a)
		}
	}
	return out
}

// ── CONCORD, abstractly ─────────────────────────────────────────────────────

// concordCatches is the stopwatch: CONCORD lands on an offender that is still
// on grid when the band's delay (concordDelayFor) runs out. Pure. fightSec is
// how long the crime took, leaveSec how long the offender then needs to be
// gone (escapeWindowSec). Below the police band nobody comes.
func concordCatches(security, fightSec, leaveSec float64) bool {
	delay, policed := concordDelayFor(security)
	if !policed {
		return false
	}
	return math.Max(fightSec, 0)+math.Max(leaveSec, 0) > delay
}

// concordLeaveSec is how long one offender needs to leave after the crime, by
// the escape-clock dials (pvpescape.go). The dials apply even with lethality
// off, because the stopwatch has no other reading of a hull.
func concordLeaveSec(c *pvpCombatant) float64 {
	if c == nil {
		return 0
	}
	return escapeWindowSec(c.AlignSec, dialReactSec.Get(), dialWarpSpoolSec.Get(), dialAlignScale.Get())
}

// abstractConcord answers the crimes a roster pass just billed. offenders are
// the pilots the exemption ladder charged, fightSec the seconds the fight ran.
// gone is true when the offenders' side broke off and got out inside the fight,
// so its leaving time is already in fightSec. An offender still on grid when the
// band's delay ends dies; one that is gone keeps its flag (criminalcarry.go).
// Skipped for a system a player has loaded: its grid has the real response.
func (m *Mod) abstractConcord(ctx context.Context, offenders []*pvpCombatant, sysID int64, fightSec float64, gone bool) {
	// The crime flags the pilot whatever comes next, in every band the ladder charged it in:
	// lowsec has no CONCORD but its gate and station guns, and everybody, may shoot a criminal.
	m.flagAbstractOffenders(offenders, sysID)
	if len(offenders) == 0 || !concordOn() || m.systemProjected(sysID) {
		return
	}
	security := 0.0
	if u, err := m.universe(ctx); err == nil && u != nil {
		security = u.meta[sysID].Security
	}
	if _, policed := concordDelayFor(security); !policed {
		return
	}
	for _, off := range offenders {
		// A pilot the fight already killed has nothing left to take.
		if off == nil || off.CharID == 0 || off.EHP <= 0 {
			continue
		}
		leave := concordLeaveSec(off)
		if gone {
			leave = 0
		}
		if !concordCatches(security, fightSec, leave) {
			rememberCriminal(off.CharID, clock.Unix())
			m.logBot(off.CharID, logInfo, "security",
				"left %s (%.1f) before CONCORD arrived, still flagged", m.nameOfSystem(ctx, sysID), security)
			continue
		}
		m.concordKill(ctx, off, sysID, security)
	}
}

// flagAbstractOffenders raises the global criminal flag on every live pilot an abstract charge
// found guilty. The flag is carried (criminalcarry.go), so a projection born from the pilot
// later enters space flagged, and a projection already standing in sysID gets it in the core's
// table at once. Without it a bot that committed a crime off-grid embodied clean, and a pilot
// who shot it was billed as the aggressor.
func (m *Mod) flagAbstractOffenders(offenders []*pvpCombatant, sysID int64) {
	now := clock.Unix()
	enter := modapi.PvP().CriminalEntered
	for _, off := range offenders {
		if off == nil || off.CharID == 0 || (off.abstractCombatant != nil && off.EHP <= 0) {
			continue
		}
		rememberCriminal(off.CharID, now)
		if enter == nil {
			continue
		}
		if eb := liveProjection(off.CharID); eb != nil && eb.ent != nil && eb.ss != nil && eb.systemID == sysID {
			enter(eb.ent, eb.ss, now)
		}
	}
}

// concordKill destroys one offender's ship, and its capsule on the shard's
// ordinary pod odds, with CONCORD on the killmail.
func (m *Mod) concordKill(ctx context.Context, off *pvpCombatant, sysID int64, security float64) {
	var x, y, z float64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
		off.ShipID).Scan(&x, &y, &z)
	hits := []destiny.KillHit{{
		Name:       concordName,
		CorpID:     concordPayerID,
		ShipTypeID: concordShipTypeID,
		Damage:     math.Max(off.MaxEHP, 1),
		IsNPC:      true,
		FinalBlow:  true,
		Time:       clock.Unix(),
	}}
	where := m.nameOfSystem(ctx, sysID)
	capsuleID := m.loseHull(ctx, off.CharID, off.Name, off.ShipID, sysID, x, y, z, concordName, hits)
	concordKillsAll.Add(1)
	m.logBot(off.CharID, logInfo, "security",
		"CONCORD answered the crime in %s (%.1f) and destroyed the ship", where, security)
	if capsuleID != 0 && rand.Float64() < podKillBase() {
		m.podBotAbstract(ctx, off.CharID, off.Name, sysID, capsuleID, x, y, z, concordName, hits)
	}
}

// ── the law bounty ──────────────────────────────────────────────────────────

// lawBountyFor is the price the law puts on a record. Pure; the sweep's SQL
// restates it, and the test pins both to the same table.
func lawBountyFor(secStatus, perPoint float64) float64 {
	if perPoint <= 0 || secStatus > lawBountyMinSec {
		return 0
	}
	pts := math.Floor(-secStatus)
	if pts > lawBountyMaxPoints {
		pts = lawBountyMaxPoints
	}
	return pts * perPoint
}

// startLawBounty registers the sweep. It only raises a bounty, never cuts one
// somebody else filed, and it prices roster pilots only.
func (m *Mod) startLawBounty() {
	m.registerPeriodic("rolebot-law-bounty", lawBountyEveryTicks, func(uint64) bool {
		if !Master() || lawBountyPerSec() <= 0 {
			return true
		}
		if !lawBountyBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("law bounty sweep", func() {
			defer lawBountyBusy.Store(false)
			m.lawBountySweep(context.Background())
		})
		return true
	})
}

func (m *Mod) lawBountySweep(ctx context.Context) {
	per := lawBountyPerSec()
	if per <= 0 {
		return
	}
	top := per * lawBountyMaxPoints
	res, err := m.db.ExecContext(ctx, `
		UPDATE character_ c
		JOIN rolebot_characters rb ON rb.characterID = c.characterID AND rb.enabled = 1
		SET c.bounty = LEAST(?, ? * FLOOR(-c.securityRating))
		WHERE c.securityRating <= ?
		  AND c.bounty < LEAST(?, ? * FLOOR(-c.securityRating))`,
		top, per, lawBountyMinSec, top, per)
	if err != nil {
		rlogf("rolebots: law bounty sweep: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		rlogf("rolebots: law bounty priced %d record(s)", n)
	}
}

// ── bounties the law and the victim's corporation file ──────────────────────

// fileCrimeBounties puts a price on every killer of a lost ship whose
// employer's colleagues died to a crime or to a war (corprel.go). A lawful
// kill between strangers files nothing. The dead pilot's corporation pays, only
// when bountyFilingAllowed, once per killer per fight. A head has no ceiling:
// the payers' purses are the only bound.
func (m *Mod) fileCrimeBounties(ctx context.Context, dead, killers, offenders []*pvpCombatant) {
	price := float64(PirateBountyISK())
	if price <= 0 || len(dead) == 0 {
		return
	}
	isOffender := map[int64]bool{}
	for _, o := range offenders {
		if o != nil {
			isOffender[o.CharID] = true
		}
	}
	filed := map[[2]int64]bool{}
	purses := map[int64]*bountyPurse{}
	for _, d := range dead {
		if d == nil || d.CorpID == 0 {
			continue
		}
		for _, k := range killers {
			if k == nil || k.CharID == 0 || k.Dealt <= 0 || k.CorpID == d.CorpID {
				continue
			}
			key := [2]int64{d.CorpID, k.CharID}
			if filed[key] {
				continue
			}
			why := ""
			switch {
			case isOffender[k.CharID]:
				why = "a crime"
			case warEverywhere() && relAtWar(d.CorpID, m.corpAllianceID(ctx, d.CorpID),
				k.CorpID, m.corpAllianceID(ctx, k.CorpID)):
				why = "a war kill"
			}
			if why == "" {
				continue
			}
			purse, ok := purses[d.CorpID]
			if !ok {
				purse = m.bountyPurseOf(ctx, d.CorpID)
				purses[d.CorpID] = purse
			}
			if purse == nil || !bountyFilingAllowed(purse.Spent, purse.Profit, purse.Balance, price) {
				continue
			}
			if m.corpFilesBounty(ctx, d.CorpID, k, price) {
				purse.Spent += price
				purse.Balance -= price
				filed[key] = true
				m.logBot(k.CharID, logInfo, "combat",
					"a price of %.0f ISK went on its head for %s: %s", price, d.Name, why)
				m.noteBountyIncident(ctx, d.CorpID, k, price)
				if ceo, ok := managedCEO(d.CorpID); ok {
					m.noteGraceFavour(d.CharID, ceo)
				}
			}
		}
	}
}

// corpFilesBounty moves price out of a corporation's wallet onto a head in one
// transaction. False if the corporation cannot afford it or the head is gone.
func (m *Mod) corpFilesBounty(ctx context.Context, corpID int64, target *pvpCombatant, price float64) bool {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE character_ SET bounty = bounty + ? WHERE characterID = ?`,
		price, target.CharID)
	if err != nil {
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	ok, bal, err := evedb.TryChargeCorpISK(ctx, tx, corpID, price)
	if err != nil || !ok {
		return false
	}
	if err := tx.Commit(); err != nil {
		return false
	}
	_ = evedb.InsertJournalEntry(ctx, m.db, corpID, 17, corpID, target.CharID, -price, bal,
		evedb.JournalReason("Bounty placed on "+target.Name))
	m.corpBook(ctx, corpID, lineBounties, -price, target.CharID, "bounty on "+target.Name)
	return true
}

const (
	// bountyFloorISK is the treasury a corporation keeps after filing.
	bountyFloorISK = 5_000_000
	// bountyProfitShare is the share of a week's operating profit a corporation
	// may spend on bounties.
	bountyProfitShare = 0.25
)

// bountyPurse is what one corporation can spend on bounties this fight.
type bountyPurse struct {
	Spent, Profit, Balance float64
}

// bountyPurseOf reads a corporation's bounty purse: bounties filed in the last
// week, operating profit over the same week and balance. It returns nil for a
// corporation the module does not run, which files nothing.
func (m *Mod) bountyPurseOf(ctx context.Context, corpID int64) *bountyPurse {
	if _, managed := managedCEO(corpID); !managed {
		return nil
	}
	since := clock.Unix() - 7*86400
	lines, err := m.corpPnL(ctx, corpID, since)
	if err != nil {
		return nil
	}
	bal, err := evedb.GetCorpBalance(ctx, m.db, corpID)
	if err != nil {
		return nil
	}
	p := &bountyPurse{Profit: payrollOperatingProfit(lines), Balance: bal}
	for _, l := range lines {
		if l.Line == lineBounties {
			p.Spent = l.Out - l.In
		}
	}
	// The share is of the profit before bounties.
	p.Profit += p.Spent
	return p
}

// bountyFilingAllowed: a filing must leave the treasury at bountyFloorISK and
// keep the week's filings within bountyProfitShare of the week's operating
// profit. A corporation running at a loss files nothing. Pure.
func bountyFilingAllowed(spent7d, profit7d, balance, price float64) bool {
	if price <= 0 || balance-price < bountyFloorISK {
		return false
	}
	return spent7d+price <= bountyProfitShare*math.Max(0, profit7d)
}
