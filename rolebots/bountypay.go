package rolebots

import (
	"context"
	"math"
	"sync/atomic"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Bounty money: the ISK a kill is worth, and who gets it. The core pays an NPC bounty only for a kill it
// witnessed, to one killer (npc.Kill, combat_embodied.go). Two gaps are covered here:
//
//   - Unwatched kills: the abstract tier resolves fights the core never sees. They pay the attribute-481
//     bounty through the refType-17 journal, scaled by rolebots.abstractBountyRate (1.0 is parity, 0 pays nothing).
//   - The split: an abstract payout is divided by damage dealt; a watched kill is evened out after the fact,
//     the killer handing driven gangmates in system equal shares (handISK, refType 10).
//
// Corp tax applies here because evedb.CreditBounty is a bare credit: the GetCorpTaxRate cut, with the corp share going to its wallet.

// settingAbstractBountyRate scales unwitnessed bounty payouts, 0..1.
const settingAbstractBountyRate = "rolebots.abstractBountyRate"

// concordPayerID is who the journal says paid when the rat's own faction is unknown, the same default
// the core's payout closure uses.
const concordPayerID = 1000125

var abstractBountyRateBits atomic.Uint64

func init() { abstractBountyRateBits.Store(math.Float64bits(1.0)) }

// AbstractBountyRate is the operator's scale on unwitnessed bounty ISK.
func AbstractBountyRate() float64 { return math.Float64frombits(abstractBountyRateBits.Load()) }

func setAbstractBountyRate(v float64) {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	abstractBountyRateBits.Store(math.Float64bits(v))
}

// bountyShare is one participant in a kill: who, and how much of the damage was theirs. Weight is
// relative: shares are normalised over the participants passed in.
type bountyShare struct {
	CharID int64
	Weight float64
}

// payNPCBounties pays the bounty on every DEAD rat in the spawn, divided
// among the participants by damage share and corp-taxed per participant.
// Failures are logged and swallowed: money that fails to arrive must never
// take the fight resolution down with it.
func (m *Mod) payNPCBounties(ctx context.Context, parts []bountyShare, spawn []*ratThreat, sysID int64) {
	rate := AbstractBountyRate()
	if len(parts) == 0 {
		return
	}
	var totalWeight float64
	for _, p := range parts {
		totalWeight += p.Weight
	}
	if totalWeight <= 0 {
		return
	}

	// The record comes before the money and is not behind the ISK rate: abstractBountyRate is an economy
	// dial, and lowering it must not stop CONCORD noticing that pirates are being killed.
	m.awardAbstractSecStatus(parts, totalWeight, spawn, sysID)
	// And the world-event bus (worldevents.go): a module counting kills is not an ISK faucet.
	emitAbstractKills(parts, spawn, sysID)

	if rate <= 0 {
		return
	}

	// The pot: every dead rat's bounty, at the operator's rate.
	type deadRat struct {
		typeID int64
		bounty float64
	}
	var pot float64
	var dead []deadRat
	for _, r := range spawn {
		if r == nil || r.EHP > 0 {
			continue
		}
		b := evedb.GetNPCBounty(ctx, m.db, r.TypeID)
		if b <= 0 {
			continue
		}
		b *= rate
		pot += b
		dead = append(dead, deadRat{r.TypeID, b})
	}
	if pot <= 0 {
		return
	}

	// The fleet's work log (gangreport.go), by rat type. The body count goes to the pilot who did most damage
	// and the money is split; crediting everyone would make a five-rat fight read as fifteen kills.
	top := parts[0]
	for _, p := range parts {
		if p.Weight > top.Weight {
			top = p
		}
	}
	for _, p := range parts {
		frac := p.Weight / totalWeight
		for _, d := range dead {
			var body int64
			if p.CharID == top.CharID {
				body = 1
			}
			m.noteGangHaulFor(ctx, p.CharID, haulKill, d.typeID, "", body, d.bounty*frac)
		}
	}

	for _, p := range parts {
		cut := pot * (p.Weight / totalWeight)
		if cut <= 0 {
			continue
		}
		// The same corp-tax split the core's own closure takes.
		var corpID int64
		_ = m.db.QueryRowContext(ctx,
			`SELECT corporationID FROM character_ WHERE characterID = ?`, p.CharID).Scan(&corpID)
		taxRate := 0.0
		if corpID != 0 {
			if tr := evedb.GetCorpTaxRate(ctx, m.db, corpID); tr > 0 && tr < 1 {
				taxRate = tr
			}
		}
		charCut := cut * (1 - taxRate)
		corpCut := cut - charCut

		// argID1 is the negative typeID, which the client renders as got bounty prize for killing <type>. One
		// journal row per participant, on the biggest rat in the pot.
		argType := dead[0].typeID
		for _, d := range dead {
			if d.bounty > dead[0].bounty {
				argType = d.typeID
			}
		}
		if err := evedb.CreditBounty(ctx, m.db, p.CharID, charCut); err != nil {
			rlogf("rolebots: bounty credit char=%d: %v", p.CharID, err)
			continue
		}
		if bal, err := evedb.GetCharBalance(ctx, m.db, p.CharID); err == nil {
			_ = evedb.InsertBountyJournal(ctx, m.db, p.CharID,
				concordPayerID, p.CharID, -argType, charCut, bal)
		}
		if corpCut > 0 && corpID != 0 {
			if err := evedb.CreditCorpWallet(ctx, m.db, corpID, corpCut); err == nil {
				if cbal, err := evedb.GetCorpBalance(ctx, m.db, corpID); err == nil {
					_ = evedb.InsertBountyJournal(ctx, m.db, corpID,
						concordPayerID, p.CharID, -argType, corpCut, cbal)
				}
				m.corpBook(ctx, corpID, lineTax, corpCut, p.CharID, "bounty tax")
			}
		}
	}
}

// awardAbstractSecStatus pays the security status a batch of rat kills is worth for a fight in an unwatched
// system, one call per participant with the gain summed first (each call writes a chrStandingTransactions
// row). It runs outside the ISK loop so a failed wallet write does not cost standing. What a type is worth
// is asked of the core, which applies the same rule on a loaded grid.
func (m *Mod) awardAbstractSecStatus(parts []bountyShare, totalWeight float64,
	spawn []*ratThreat, sysID int64) {

	gain := modapi.PvP().SecRatGain
	frac := modapi.PvP().SecRatFraction
	if gain == nil || frac == nil || totalWeight <= 0 {
		return
	}
	var total float64
	var argType int64
	for _, r := range spawn {
		if r == nil || r.EHP > 0 {
			continue
		}
		f := frac(r.TypeID)
		if f == 0 {
			continue
		}
		total += f
		if argType == 0 {
			argType = r.TypeID
		}
	}
	if total == 0 {
		return
	}
	for _, p := range parts {
		share := total * (p.Weight / totalWeight)
		if share == 0 {
			continue
		}
		gain(nil, p.CharID, share, concordPayerID, sysID, argType)
	}
}

// shareEmbodiedBounty evens out a core-paid kill: the killer was paid the full after-tax bounty alone and
// hands each driven gangmate in the same system an equal share over a Give ISK transfer. It deals only in
// ISK and never awards security status: the core's death broadcast already paid the killer's record and
// standing is not divisible. awardAbstractSecStatus covers the kills the core never saw.
func (m *Mod) shareEmbodiedBounty(ctx context.Context, killerCharID, killerSystemID, victimTypeID int64) {
	g, _, err := gangForChar(ctx, m.db, killerCharID)
	if err != nil || g == nil {
		return
	}
	// The kill goes on the fleet's work log whatever it was worth, before every early return below, which
	// are all about the money.
	bounty := evedb.GetNPCBounty(ctx, m.db, victimTypeID)
	m.noteGangHaul(ctx, g.GangID, killerCharID, haulKill, victimTypeID, "", 1, 0, bounty)
	if bounty <= 0 {
		return
	}
	taxRate := 0.0
	var corpID int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, killerCharID).Scan(&corpID)
	if corpID != 0 {
		if tr := evedb.GetCorpTaxRate(ctx, m.db, corpID); tr > 0 && tr < 1 {
			taxRate = tr
		}
	}
	net := bounty * (1 - taxRate)

	// Driven gangmates in the same system, the ones in the fight. A mate still in warp did not shoot this rat.
	rows, err := m.db.QueryContext(ctx, `
		SELECT gm.characterID
		FROM rolebot_gang_members gm
		JOIN rolebot_characters rb ON rb.characterID = gm.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = gm.characterID
		WHERE gm.gangID = ? AND gm.characterID <> ? AND c.solarSystemID = ?`,
		g.GangID, killerCharID, killerSystemID)
	if err != nil {
		return
	}
	var mates []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			mates = append(mates, id)
		}
	}
	rows.Close()
	if len(mates) == 0 {
		return
	}
	share := net / float64(len(mates)+1)
	if share < 1 {
		return // not worth the journal rows
	}
	for _, mate := range mates {
		if err := m.handISK(ctx, killerCharID, mate, share); err != nil {
			// The killer's wallet ran out mid-round (it was paid net, so only if something else spent it): stop, do not error.
			return
		}
	}
}
