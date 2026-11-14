package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// The ISK-flow ledger: what the bot economy creates and destroys. It reads market_journal forward
// on its primary key from a stored cursor (like the liquidity sweep, liquidity.go) and folds every
// rostered wallet's rows into an hourly ledger keyed by a line from iskFlowLines. A line is a faucet
// (new ISK), a sink (ISK gone from the world) or a transfer (ISK moved between two wallets in the
// economy). Classification is one SQL CASE and the order of its arms matters: overloaded refTypes
// are told apart by reason or owner columns before the general arms run. The counterparty comes from
// the ID band (evedb.MinPlayerEntityID). History is partial: walletretention trims bot journals, so
// the bootstrap recovers corporation wallets in full and only a tail of each bot's.
// See docs/comment-notes-b12.md#isk-flow-ledger.

const (
	// iskFlowEveryTicks is once a minute; tick.Manager runs at 1 s.
	iskFlowEveryTicks = 60
	// iskFlowWindowIDs is how many refIDs one batch covers and iskFlowBatches
	// how many batches one pass runs: a million rows a minute at most, which
	// is more than the shard writes.
	iskFlowWindowIDs = 250_000
	iskFlowBatches   = 4
	// iskFlowBootstrapDays is how far back a fresh ledger reads.
	iskFlowBootstrapDays = 30
	// iskFlowRetentionDays is how long an hour row is kept: five days past the longest window
	// the pages offer.
	iskFlowRetentionDays = 35
)

// Flow directions, as iskFlowLine.Dir.
const (
	flowFaucet     = "faucet"
	flowSink       = "sink"
	flowTransfer   = "transfer"
	flowExcluded   = "excluded"
	flowUnmeasured = "unmeasured"
)

// Wallet kinds, as rolebot_isk_flow.actor.
const (
	flowActorBot  = "bot"
	flowActorCorp = "corp"
)

// Fixed NPC owners the classification names.
const (
	flowOwnerCONCORD    = 1000125
	flowOwnerInsurance  = 1000113
	flowOwnerSCC        = 1000132
	flowOwnerGenolution = 1000140
)

// iskFlowLine is one touch point: a key the ledger stores, and everything
// the catalogue panel says about it.
type iskFlowLine struct {
	Key      string // rolebot_isk_flow.line, at most 24 characters
	Label    string
	Dir      string // flowFaucet | flowSink | flowTransfer | flowExcluded | flowUnmeasured
	Counter  string // who is on the other side
	RefTypes string // journal refTypeIDs, for the operator reading the raw table
	Code     string // where in the code the row is written
	Desc     string
}

// iskFlowLines is every line, in catalogue order. The classification SQL below must emit only
// keys listed here, and every key that is not "unmeasured" must be emitted
// (TestISKFlowLinesAreWellFormed).
var iskFlowLines = []iskFlowLine{
	// ── faucets ─────────────────────────────────────────────────────────
	{Key: "npc_sale", Label: "Sold to an NPC bid", Dir: flowFaucet, Counter: "NPC corporation (seeded bid)", RefTypes: "2",
		Code: "db/marketorders.go PlaceSellOrder / PlaceBuyOrder fill", Desc: "The seeder conjures the ISK; nothing is debited anywhere. The biggest faucet on a bot shard — trade goods, ore, minerals, loot."},
	{Key: "bounty_npc", Label: "NPC bounty", Dir: flowFaucet, Counter: "CONCORD / faction", RefTypes: "17",
		Code: "bountypay.go payNPCBounties (abstract), cmd/gurimoon main.go (live grid)", Desc: "Paid on a rat kill. argID1 carries the rat's type."},
	{Key: "insurance_pay", Label: "Insurance payout", Dir: flowFaucet, Counter: "Pend Insurance (1000113)", RefTypes: "19",
		Code: "botinsure.go claimHullInsurance", Desc: "Paid on a lost hull with a live policy."},
	{Key: "operator_in", Label: "Operator grant", Dir: flowFaucet, Counter: "the operator", RefTypes: "10",
		Code: "esp.go injectISK", Desc: "ISK given from the pilot page. Self-to-self donation rows."},
	{Key: "birth", Label: "Starting wallet", Dir: flowFaucet, Counter: "character creation", RefTypes: "(none)",
		Code: "botgen.go CreateCharacter → noteBotISKFlow", Desc: "A new bot's balance. No journal row is written at birth; the generator books it here directly."},
	{Key: "rent_in", Label: "Rent received", Dir: flowFaucet, Counter: "a tenant corporation", RefTypes: "13",
		Code: "db/officedb.go RentOffice, db/billsdb.go", Desc: "A bot corporation owning a station collects office rent. Rare; a transfer when the tenant is a bot."},
	{Key: "job_fees_in", Label: "Line fees received", Dir: flowFaucet, Counter: "an installer", RefTypes: "56, 58, 59, 60",
		Code: "server/ram InstallJob (assembly-line owner credit)", Desc: "A bot corporation owning an assembly line collects the install fee."},

	// ── sinks ───────────────────────────────────────────────────────────
	{Key: "npc_buy", Label: "Bought from an NPC ask", Dir: flowSink, Counter: "NPC corporation (seeded ask)", RefTypes: "2",
		Code: "db/marketorders.go PlaceBuyOrder fill", Desc: "The seeder's wallet is never credited, so the ISK leaves the world. Modules, charges, blueprints, hulls, fuel."},
	{Key: "skills", Label: "Skillbooks", Dir: flowSink, Counter: "Genolution (1000140)", RefTypes: "2",
		Code: "skills.go / corprank.go → db/skillpurchasedb.go PurchaseAndInjectSkill", Desc: "A skill bought straight from Genolution and injected."},
	{Key: "sales_tax", Label: "Sales tax", Dir: flowSink, Counter: "the station (nobody)", RefTypes: "54",
		Code: "db/marketorders.go (every sale)", Desc: "1% of every sale, debited and credited to no one."},
	{Key: "broker", Label: "Broker fee", Dir: flowSink, Counter: "the station (nobody)", RefTypes: "46 (2 on older mineralbatch.go rows)",
		Code: "db/marketorders.go PlaceBuyOrder / PlaceSellOrder / ModifyOrder; mineralbatch.go topUpOwnSellOrder", Desc: "Paid to leave an order resting, and again on a modify or a top-up of a standing order. Older top-up rows carry refType 2 with reason 'Broker fee'."},
	{Key: "scc", Label: "SCC Exchange fees", Dir: flowSink, Counter: "SCC (1000132)", RefTypes: "46, 54",
		Code: "sccbots.go → db/sccexchange.go", Desc: "Broker fee and transaction tax on the corporate stock exchange."},
	{Key: "clone", Label: "Clone upgrade", Dir: flowSink, Counter: "the medical bay (nobody)", RefTypes: "8 (46 before)",
		Code: "clone.go", Desc: "Clone Activation (refType 8) from the core's BuyCloneGrade, self as both owners. Older rows used the broker-fee refType 46 with self as both owners and no reason."},
	{Key: "insurance_prem", Label: "Insurance premium", Dir: flowSink, Counter: "Pend Insurance (1000113)", RefTypes: "19",
		Code: "botinsure.go", Desc: "The policy's price. Pend is not credited."},
	{Key: "rent", Label: "Office rent (NPC landlord)", Dir: flowSink, Counter: "NPC station owner", RefTypes: "13",
		Code: "corpceo_estate.go → db/officedb.go; corpoffices.go → db/billsdb.go PayBill", Desc: "First month at rental, then the monthly bill. The NPC owner's wallet is a sink."},
	{Key: "job_fees", Label: "Industry install fees (NPC line)", Dir: flowSink, Counter: "NPC assembly-line owner", RefTypes: "56, 58, 59, 60",
		Code: "industry_ram.go, labwork_ram.go, corpjobs.go, posjobs.go", Desc: "The fee to install a job on a line an NPC owns. A tower's own lines are charged to the corporation and credited to nobody."},
	{Key: "war", Label: "War fees", Dir: flowSink, Counter: "CONCORD (1000125)", RefTypes: "49",
		Code: "corpceo_war.go, merc.go, alliancewar.go → cmd/gurimoon warwrites.go; war-sweep bills", Desc: "The declaration fee and each weekly continuation bill."},
	{Key: "alliance_dues", Label: "Alliance maintenance", Dir: flowSink, Counter: "CONCORD (1000125)", RefTypes: "50",
		Code: "alliance.go PayBill", Desc: "The weekly per-member-corporation bill."},
	{Key: "alliance_reg", Label: "Alliance registration", Dir: flowSink, Counter: "CONCORD", RefTypes: "48",
		Code: "alliancefound.go → db/alliancedb.go", Desc: "One billion ISK at founding."},
	{Key: "corp_reg", Label: "Corporation registration", Dir: flowSink, Counter: "SCC (1000132)", RefTypes: "39",
		Code: "corpfound.go → db/corpdb.go", Desc: "The founding fee."},
	{Key: "repair", Label: "Repair bills", Dir: flowSink, Counter: "the station (nobody)", RefTypes: "15",
		Code: "repair.go → db/repairdb.go", Desc: "Hull and module repair at a station."},
	{Key: "bill_other", Label: "Other bills", Dir: flowSink, Counter: "varies", RefTypes: "6",
		Code: "db/billsdb.go PayBill", Desc: "Any bill the pages above do not name."},
	{Key: "listing_fee", Label: "Escrow listing fee", Dir: flowSink, Counter: "the contract system (nobody)", RefTypes: "23",
		Code: "escrow_sell.go, corpprintsale.go → db/escrowdb.go", Desc: "The fee to list a fitted ship or a print for sale. Not refunded."},
	{Key: "operator_out", Label: "Operator withdrawal", Dir: flowSink, Counter: "the operator", RefTypes: "10",
		Code: "esp.go injectISK", Desc: "ISK taken from the pilot page."},

	// ── transfers ───────────────────────────────────────────────────────
	{Key: "trade_bot", Label: "Market trade with a bot", Dir: flowTransfer, Counter: "another rolebot", RefTypes: "2",
		Code: "db/marketorders.go fills (both sides rostered)", Desc: "Counted once on each side, so the gross is twice the ISK that moved."},
	{Key: "trade_corp", Label: "Market trade with a bot corporation", Dir: flowTransfer, Counter: "a bot-run corporation's order", RefTypes: "2",
		Code: "db/marketorders.go fills (corp order on the other side)", Desc: "A corporation treasury order on one side."},
	{Key: "trade_player", Label: "Market trade with a player", Dir: flowTransfer, Counter: "a player or player corporation", RefTypes: "2",
		Code: "db/marketorders.go fills", Desc: "ISK crossing between the roster and the human economy."},
	{Key: "gift_bot", Label: "ISK handed to / from a bot", Dir: flowTransfer, Counter: "another rolebot", RefTypes: "10",
		Code: "broke.go handISK: whip-rounds, ransoms, fleet payouts, settlements", Desc: ""},
	{Key: "gift_corp", Label: "ISK handed to / from a bot corporation", Dir: flowTransfer, Counter: "a bot-run corporation", RefTypes: "10",
		Code: "corplevy.go, corptradelevy.go, corpceo_backing.go, posreq.go, corplibrary.go", Desc: "Harvest and trade levies, CEO backing, requisitions and library purchases paid into the treasury."},
	{Key: "gift_player", Label: "ISK handed to / from a player", Dir: flowTransfer, Counter: "a player", RefTypes: "10",
		Code: "conscript_plan.go remitToBoss, piracy_prey.go ransoms", Desc: ""},
	{Key: "corp_withdraw", Label: "Corporation payout", Dir: flowTransfer, Counter: "a bot corporation → member", RefTypes: "37",
		Code: "corpceo_treasury.go corpWithdraw: wages, stipends, salary, merc pay", Desc: ""},
	{Key: "shares", Label: "Share trades", Dir: flowTransfer, Counter: "shareholder ↔ issuer", RefTypes: "22",
		Code: "corpshares.go, corpceo_backing.go → db/corpshares.go TradeShares", Desc: ""},
	{Key: "dividend", Label: "Dividends", Dir: flowTransfer, Counter: "corporation → shareholders", RefTypes: "38",
		Code: "corpdividend.go → db/corpshares.go PayDividend", Desc: ""},
	{Key: "escrow_sale", Label: "Escrow sales", Dir: flowTransfer, Counter: "buyer ↔ seller", RefTypes: "1",
		Code: "db/escrowdb.go", Desc: "A fitted ship or print sold through escrow."},
	{Key: "courier_out", Label: "Courier contracts issued", Dir: flowTransfer, Counter: "the contract's hauler", RefTypes: "23",
		Code: "courier_issue.go, poscourier.go → db/courierdb.go", Desc: "Reward and collateral put up. The issue row bundles the 1,000 ISK listing fee, which is a sink."},
	{Key: "courier_in", Label: "Courier contracts paid", Dir: flowTransfer, Counter: "the contract's issuer", RefTypes: "23",
		Code: "db/courierdb.go", Desc: "Reward and collateral returned."},
	{Key: "bounty_pool_in", Label: "Pilot bounty collected", Dir: flowTransfer, Counter: "the bounty pool", RefTypes: "17",
		Code: "combat_botvbot.go (kill of a pilot with a bounty)", Desc: "Paid from character_.bounty. Bounties PLACED by bots are the matching outflow; the law sweep's share (pvplaw.go) is an unmeasured faucet."},
	{Key: "bounty_pool_out", Label: "Pilot bounty placed", Dir: flowTransfer, Counter: "the bounty pool", RefTypes: "17",
		Code: "combat_botvbot.go, pvplaw.go → db AddToBounty", Desc: ""},
	{Key: "rent_out", Label: "Office rent to a player corporation", Dir: flowTransfer, Counter: "a player station owner", RefTypes: "13",
		Code: "db/officedb.go, db/billsdb.go", Desc: ""},
	{Key: "job_fees_out", Label: "Industry install fees to a player line", Dir: flowTransfer, Counter: "a player assembly-line owner", RefTypes: "56, 58, 59, 60",
		Code: "server/ram InstallJob", Desc: ""},

	// ── excluded / unmeasured ───────────────────────────────────────────
	{Key: "escrow", Label: "Market escrow", Dir: flowExcluded, Counter: "the order book", RefTypes: "42",
		Code: "db/marketorders.go", Desc: "ISK parked behind a resting bid and returned on cancel or fill. Not income, not cost."},
	{Key: "other", Label: "Unclassified journal rows", Dir: flowExcluded, Counter: "?", RefTypes: "any other",
		Code: "", Desc: "A refType the ledger does not name. Nonzero here means a new touch point needs a line."},
	{Key: "law_bounty", Label: "Law bounty (unmeasured)", Dir: flowUnmeasured, Counter: "the bounty pool", RefTypes: "(none)",
		Code: "pvplaw.go lawBountySweep", Desc: "Written onto character_.bounty with no wallet debit; it becomes real ISK when collected (bounty_pool_in). The collected total overstates transfers by this amount."},
}

var iskFlowLineByKey = map[string]iskFlowLine{}

func init() {
	for _, l := range iskFlowLines {
		iskFlowLineByKey[l.Key] = l
	}
}

// ── classification ──────────────────────────────────────────────────────────

// iskFlowRosterSQL is every wallet the ledger follows: rostered pilots and
// the corporations a bot CEO runs.
const iskFlowRosterSQL = `(SELECT characterID AS id, 'bot' AS kind FROM rolebot_characters
	UNION ALL SELECT corporationID, 'corp' FROM rolebot_ceos)`

// iskFlowInnerSQL is the classified read of one refID window. Columns:
// hour, day, actor, corpID, amount, line. Two placeholders: the exclusive
// low and inclusive high refID.
var iskFlowInnerSQL = fmt.Sprintf(`
	SELECT FLOOR((j.transDate / 10000000 - 11644473600) / 3600) AS hour,
	       FLOOR((j.transDate / 10000000 - 11644473600) / 86400) AS day,
	       r.kind AS actor,
	       CASE WHEN r.kind = 'corp' THEN j.characterID ELSE COALESCE(c.corporationID, 0) END AS corpID,
	       j.amount,
	       CASE
	         WHEN j.refTypeID = 42 THEN 'escrow'
	         WHEN j.refTypeID = 2  AND j.reason = 'Broker fee' THEN 'broker'
	         WHEN j.refTypeID = 2  AND (j.ownerID2 = %[2]d OR j.reason LIKE 'Skill injection%%') THEN 'skills'
	         WHEN j.refTypeID IN (46, 54) AND j.ownerID2 = %[3]d THEN 'scc'
	         WHEN j.refTypeID = 46 AND j.reason LIKE 'Broker fee%%' THEN 'broker'
	         WHEN j.refTypeID = 8 THEN 'clone'
	         WHEN j.refTypeID = 46 AND j.ownerID1 = j.characterID AND j.ownerID2 = j.characterID THEN 'clone'
	         WHEN j.refTypeID = 46 THEN 'broker'
	         WHEN j.refTypeID = 54 THEN 'sales_tax'
	         WHEN j.refTypeID = 2 AND j.amount < 0 THEN
	              CASE WHEN cp.kind IS NULL AND COALESCE(j.ownerID2, 0) < %[1]d THEN 'npc_buy'
	                   WHEN cp.kind = 'bot'  THEN 'trade_bot'
	                   WHEN cp.kind = 'corp' THEN 'trade_corp'
	                   ELSE 'trade_player' END
	         WHEN j.refTypeID = 2 THEN
	              CASE WHEN cp.kind IS NULL AND COALESCE(j.ownerID1, 0) < %[1]d THEN 'npc_sale'
	                   WHEN cp.kind = 'bot'  THEN 'trade_bot'
	                   WHEN cp.kind = 'corp' THEN 'trade_corp'
	                   ELSE 'trade_player' END
	         WHEN j.refTypeID = 17 AND j.amount > 0 AND j.argID1 IS NOT NULL THEN 'bounty_npc'
	         WHEN j.refTypeID = 17 AND j.amount > 0 THEN 'bounty_pool_in'
	         WHEN j.refTypeID = 17 THEN 'bounty_pool_out'
	         WHEN j.refTypeID = 19 AND j.amount > 0 THEN 'insurance_pay'
	         WHEN j.refTypeID = 19 THEN 'insurance_prem'
	         WHEN j.refTypeID = 10 AND j.ownerID1 = j.characterID AND j.ownerID2 = j.characterID THEN
	              CASE WHEN j.amount > 0 THEN 'operator_in' ELSE 'operator_out' END
	         WHEN j.refTypeID = 10 THEN
	              CASE cp.kind WHEN 'bot' THEN 'gift_bot' WHEN 'corp' THEN 'gift_corp' ELSE 'gift_player' END
	         WHEN j.refTypeID = 13 THEN
	              CASE WHEN j.amount > 0 THEN 'rent_in'
	                   WHEN COALESCE(j.ownerID2, 0) < %[1]d THEN 'rent' ELSE 'rent_out' END
	         WHEN j.refTypeID IN (56, 58, 59, 60) THEN
	              CASE WHEN j.amount > 0 THEN 'job_fees_in'
	                   WHEN COALESCE(j.ownerID2, 0) < %[1]d THEN 'job_fees' ELSE 'job_fees_out' END
	         WHEN j.refTypeID = 23 AND j.reason = 'Escrow listing fee' THEN 'listing_fee'
	         WHEN j.refTypeID = 23 THEN CASE WHEN j.amount < 0 THEN 'courier_out' ELSE 'courier_in' END
	         WHEN j.refTypeID = 49 THEN 'war'
	         WHEN j.refTypeID = 50 THEN 'alliance_dues'
	         WHEN j.refTypeID = 48 THEN 'alliance_reg'
	         WHEN j.refTypeID = 39 THEN 'corp_reg'
	         WHEN j.refTypeID = 15 THEN 'repair'
	         WHEN j.refTypeID = 6  THEN 'bill_other'
	         WHEN j.refTypeID = 37 THEN 'corp_withdraw'
	         WHEN j.refTypeID = 22 THEN 'shares'
	         WHEN j.refTypeID = 38 THEN 'dividend'
	         WHEN j.refTypeID = 1  THEN 'escrow_sale'
	         ELSE 'other'
	       END AS line
	  FROM market_journal j
	  JOIN `+iskFlowRosterSQL+` r ON r.id = j.characterID
	  LEFT JOIN `+iskFlowRosterSQL+` cp
	         ON cp.id = CASE WHEN j.refTypeID = 2 THEN (CASE WHEN j.amount < 0 THEN j.ownerID2 ELSE j.ownerID1 END)
	                         WHEN j.ownerID1 = j.characterID THEN j.ownerID2
	                         ELSE j.ownerID1 END
	  LEFT JOIN character_ c ON c.characterID = j.characterID AND r.kind = 'bot'
	 WHERE j.refID > ? AND j.refID <= ?`,
	evedb.MinPlayerEntityID, flowOwnerGenolution, flowOwnerSCC)

// iskFlowMarketInnerSQL is the classified read of one transactionID window
// of market_transactions: hour, categoryID, groupID, systemID, side, counter,
// isk, units. Synthetic price-history rows carry stationID 0 and are skipped.
var iskFlowMarketInnerSQL = fmt.Sprintf(`
	SELECT FLOOR((t.transactionDateTime / 10000000 - 11644473600) / 3600) AS hour,
	       COALESCE(g.categoryID, 0) AS categoryID,
	       COALESCE(it.groupID, 0) AS groupID,
	       COALESCE(s.solarSystemID, 0) AS systemID,
	       CASE WHEN t.transactionType = 1 THEN 'buy' ELSE 'sell' END AS side,
	       CASE WHEN t.clientID < %d THEN 'npc'
	            WHEN rbc.characterID IS NOT NULL THEN 'bot' ELSE 'player' END AS counter,
	       t.price * t.quantity AS isk, t.quantity AS units
	  FROM market_transactions t
	  JOIN rolebot_characters rb ON rb.characterID = t.characterID
	  LEFT JOIN rolebot_characters rbc ON rbc.characterID = t.clientID
	  LEFT JOIN staStations s ON s.stationID = t.stationID
	  LEFT JOIN invTypes it ON it.typeID = t.typeID
	  LEFT JOIN invGroups g ON g.groupID = it.groupID
	 WHERE t.transactionID > ? AND t.transactionID <= ? AND t.stationID > 0`,
	evedb.MinPlayerEntityID)

// ── the sweep ───────────────────────────────────────────────────────────────

var (
	iskFlowBusy = make(chan struct{}, 1)
	// iskFlowLastSweep is when a pass last finished (unix), for the ledger
	// health panel. Memory only: a restart is a fine reason to read "never".
	iskFlowLastSweep atomic.Int64
)

// startISKFlowSweep registers the pass. Off the tick goroutine behind a
// self-overlap guard, like the hub sweep.
func (m *Mod) startISKFlowSweep() {
	m.registerPeriodic("rolebot-iskflow", iskFlowEveryTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		select {
		case iskFlowBusy <- struct{}{}:
		default:
			return true
		}
		goSafe("isk flow sweep", func() {
			defer func() { <-iskFlowBusy }()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			if err := m.iskFlowSweep(ctx); err != nil {
				rlogf("rolebots: isk flow sweep: %v", err)
			}
		})
		return true
	})
}

// iskFlowSweep is one pass: the journal windows, the market windows, the prune. Each half is
// independent, so a failure in one does not leave the other stale; the first error is returned
// after both have run.
func (m *Mod) iskFlowSweep(ctx context.Context) error {
	now := clock.Unix()
	errJ := m.iskFlowJournalPass(ctx, now)
	errM := m.iskFlowMarketPass(ctx, now)
	errG := m.iskFlowGoodsPass(ctx, now)
	// The trade logic's market history (mktdaily.go) rides the same clock.
	errD := m.mktDailyPass(ctx, now)
	m.iskFlowPrune(ctx, now)
	iskFlowLastSweep.Store(clock.Unix())
	clearEconomyMemo()
	for _, err := range []error{errJ, errM, errG, errD} {
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Mod) iskFlowJournalPass(ctx context.Context, now int64) error {
	cursor, err := m.iskFlowCursor(ctx, now)
	if err != nil {
		return err
	}
	var head int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(refID), 0) FROM market_journal`).Scan(&head); err != nil {
		return fmt.Errorf("isk flow head: %w", err)
	}
	for i := 0; i < iskFlowBatches && cursor < head; i++ {
		hi := cursor + iskFlowWindowIDs
		if hi > head {
			hi = head
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_isk_flow (hour, actor, line, isk, inflow, n)
			SELECT x.hour, x.actor, x.line, SUM(x.amount),
			       SUM(CASE WHEN x.amount > 0 THEN x.amount ELSE 0 END), COUNT(*)
			  FROM (`+iskFlowInnerSQL+`) x
			 GROUP BY x.hour, x.actor, x.line
			ON DUPLICATE KEY UPDATE isk = isk + VALUES(isk), inflow = inflow + VALUES(inflow), n = n + VALUES(n)`,
			cursor, hi); err != nil {
			return fmt.Errorf("isk flow batch %d..%d: %w", cursor, hi, err)
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_isk_flow_corp (day, corporationID, line, isk, inflow, n)
			SELECT x.day, x.corpID, x.line, SUM(x.amount),
			       SUM(CASE WHEN x.amount > 0 THEN x.amount ELSE 0 END), COUNT(*)
			  FROM (`+iskFlowInnerSQL+`) x
			 WHERE x.corpID > 0
			 GROUP BY x.day, x.corpID, x.line
			ON DUPLICATE KEY UPDATE isk = isk + VALUES(isk), inflow = inflow + VALUES(inflow), n = n + VALUES(n)`,
			cursor, hi); err != nil {
			return fmt.Errorf("isk flow corp batch %d..%d: %w", cursor, hi, err)
		}
		cursor = hi
		if _, err := m.db.ExecContext(ctx,
			`UPDATE rolebot_isk_flow_cursor SET lastRefID = ? WHERE id = 1`, cursor); err != nil {
			return fmt.Errorf("isk flow cursor: %w", err)
		}
	}
	return nil
}

func (m *Mod) iskFlowMarketPass(ctx context.Context, now int64) error {
	cursor, err := m.iskFlowMarketCursor(ctx, now, mktCursorFlow)
	if err != nil {
		return err
	}
	var head int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(transactionID), 0) FROM market_transactions`).Scan(&head); err != nil {
		return fmt.Errorf("mkt flow head: %w", err)
	}
	for i := 0; i < iskFlowBatches && cursor < head; i++ {
		hi := cursor + iskFlowWindowIDs
		if hi > head {
			hi = head
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_mkt_flow (hour, categoryID, side, counter, isk, units, n)
			SELECT x.hour, x.categoryID, x.side, x.counter, SUM(x.isk), SUM(x.units), COUNT(*)
			  FROM (`+iskFlowMarketInnerSQL+`) x
			 GROUP BY x.hour, x.categoryID, x.side, x.counter
			ON DUPLICATE KEY UPDATE isk = isk + VALUES(isk), units = units + VALUES(units), n = n + VALUES(n)`,
			cursor, hi); err != nil {
			return fmt.Errorf("mkt flow batch %d..%d: %w", cursor, hi, err)
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_mkt_system (hour, systemID, side, counter, isk, n)
			SELECT x.hour, x.systemID, x.side, x.counter, SUM(x.isk), COUNT(*)
			  FROM (`+iskFlowMarketInnerSQL+`) x
			 WHERE x.systemID > 0
			 GROUP BY x.hour, x.systemID, x.side, x.counter
			ON DUPLICATE KEY UPDATE isk = isk + VALUES(isk), n = n + VALUES(n)`,
			cursor, hi); err != nil {
			return fmt.Errorf("mkt system batch %d..%d: %w", cursor, hi, err)
		}
		cursor = hi
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_mkt_flow_cursor (id, lastTxID) VALUES (?, ?)
			ON DUPLICATE KEY UPDATE lastTxID = VALUES(lastTxID)`, mktCursorFlow, cursor); err != nil {
			return fmt.Errorf("mkt flow cursor: %w", err)
		}
	}
	return nil
}

// iskFlowGoodsPass fills rolebot_mkt_group, the by-group ledger behind the goods breakdown
// (econgoods.go). It runs on its own cursor rather than inside the market pass, so on a shard
// whose market cursor is current it still bootstraps its thirty days.
func (m *Mod) iskFlowGoodsPass(ctx context.Context, now int64) error {
	cursor, err := m.iskFlowMarketCursor(ctx, now, mktCursorGoods)
	if err != nil {
		return err
	}
	var head int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(transactionID), 0) FROM market_transactions`).Scan(&head); err != nil {
		return fmt.Errorf("mkt goods head: %w", err)
	}
	for i := 0; i < iskFlowBatches && cursor < head; i++ {
		hi := cursor + iskFlowWindowIDs
		if hi > head {
			hi = head
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_mkt_group (hour, groupID, side, counter, isk, units, n)
			SELECT x.hour, x.groupID, x.side, x.counter, SUM(x.isk), SUM(x.units), COUNT(*)
			  FROM (`+iskFlowMarketInnerSQL+`) x
			 GROUP BY x.hour, x.groupID, x.side, x.counter
			ON DUPLICATE KEY UPDATE isk = isk + VALUES(isk), units = units + VALUES(units), n = n + VALUES(n)`,
			cursor, hi); err != nil {
			return fmt.Errorf("mkt goods batch %d..%d: %w", cursor, hi, err)
		}
		cursor = hi
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_mkt_flow_cursor (id, lastTxID) VALUES (?, ?)
			ON DUPLICATE KEY UPDATE lastTxID = VALUES(lastTxID)`, mktCursorGoods, cursor); err != nil {
			return fmt.Errorf("mkt goods cursor: %w", err)
		}
	}
	return nil
}

// iskFlowPrune drops hour rows past retention. Every ledger is keyed on its
// time bucket first, so each DELETE is a primary-key range.
func (m *Mod) iskFlowPrune(ctx context.Context, now int64) {
	cutHour := now/3600 - iskFlowRetentionDays*24
	cutDay := now/86400 - iskFlowRetentionDays
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{`DELETE FROM rolebot_isk_flow WHERE hour < ?`, cutHour},
		{`DELETE FROM rolebot_mkt_flow WHERE hour < ?`, cutHour},
		{`DELETE FROM rolebot_mkt_system WHERE hour < ?`, cutHour},
		{`DELETE FROM rolebot_mkt_group WHERE hour < ?`, cutHour},
		{`DELETE FROM rolebot_isk_flow_corp WHERE day < ?`, cutDay},
		{`DELETE FROM rolebot_mkt_daily WHERE day < ?`, now/86400 - mktDailyRetentionDays},
	} {
		if _, err := m.db.ExecContext(ctx, q.sql, q.arg); err != nil {
			rlogf("rolebots: isk flow prune: %v", err)
		}
	}
}

// ── cursors ─────────────────────────────────────────────────────────────────

// iskFlowCursor is the last refID the ledger has read. A fresh ledger starts
// iskFlowBootstrapDays back, found by binary search on refID (the journal has no index on
// transDate and refID grows with time). Retention has deleted rows, so each probe asks for the
// first row at or after a refID.
func (m *Mod) iskFlowCursor(ctx context.Context, now int64) (int64, error) {
	var cur int64
	err := m.db.QueryRowContext(ctx,
		`SELECT lastRefID FROM rolebot_isk_flow_cursor WHERE id = 1`).Scan(&cur)
	if err == nil {
		return cur, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("isk flow cursor: %w", err)
	}
	start, err := m.iskFlowBootstrapRefID(ctx, evedb.UnixToWin32(now-iskFlowBootstrapDays*86400))
	if err != nil {
		return 0, err
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_isk_flow_cursor (id, lastRefID, bootstrappedAt) VALUES (1, ?, ?)
		ON DUPLICATE KEY UPDATE lastRefID = VALUES(lastRefID), bootstrappedAt = VALUES(bootstrappedAt)`,
		start, now); err != nil {
		return 0, fmt.Errorf("isk flow bootstrap: %w", err)
	}
	return start, nil
}

// iskFlowBootstrapRefID returns the exclusive cursor to start from so the first row read is the
// first with transDate >= target. A journal with nothing that recent returns the head; an empty
// journal returns 0.
func (m *Mod) iskFlowBootstrapRefID(ctx context.Context, target int64) (int64, error) {
	var lo, hi sql.NullInt64
	if err := m.db.QueryRowContext(ctx,
		`SELECT MIN(refID), MAX(refID) FROM market_journal`).Scan(&lo, &hi); err != nil {
		return 0, fmt.Errorf("isk flow bounds: %w", err)
	}
	if !hi.Valid {
		return 0, nil
	}
	probe := func(id int64) (refID, transDate int64, ok bool, err error) {
		err = m.db.QueryRowContext(ctx,
			`SELECT refID, transDate FROM market_journal WHERE refID >= ? ORDER BY refID LIMIT 1`,
			id).Scan(&refID, &transDate)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, false, nil
		}
		return refID, transDate, err == nil, err
	}
	l, h := lo.Int64, hi.Int64
	for i := 0; i < 64 && l < h; i++ {
		mid := l + (h-l)/2
		id, td, ok, err := probe(mid)
		if err != nil {
			return 0, fmt.Errorf("isk flow probe: %w", err)
		}
		if !ok {
			h = mid - 1
			continue
		}
		if td >= target {
			h = id
		} else {
			l = id + 1
		}
	}
	id, td, ok, err := probe(l)
	if err != nil {
		return 0, fmt.Errorf("isk flow probe: %w", err)
	}
	if !ok || td < target {
		return hi.Int64, nil
	}
	return id - 1, nil
}

// Rows of rolebot_mkt_flow_cursor: one per ledger read off the tape.
const (
	mktCursorFlow  = 1 // rolebot_mkt_flow + rolebot_mkt_system
	mktCursorGoods = 2 // rolebot_mkt_group
)

// iskFlowMarketCursor is a transactions cursor. The bootstrap reads the
// same 30 days through idx_mt_type_date_agg, as liquidityCursor does.
func (m *Mod) iskFlowMarketCursor(ctx context.Context, now int64, id int) (int64, error) {
	var cur int64
	err := m.db.QueryRowContext(ctx,
		`SELECT lastTxID FROM rolebot_mkt_flow_cursor WHERE id = ?`, id).Scan(&cur)
	if err == nil {
		return cur, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("mkt flow cursor: %w", err)
	}
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MIN(transactionID), 0) FROM market_transactions
		 WHERE transactionType = 1 AND transactionDateTime >= ?`,
		evedb.UnixToWin32(now-iskFlowBootstrapDays*86400)).Scan(&cur); err != nil {
		return 0, fmt.Errorf("mkt flow bootstrap: %w", err)
	}
	if cur > 0 {
		cur-- // exclusive
	}
	return cur, nil
}

// ── hooks and health ────────────────────────────────────────────────────────

// noteISKFlow books ISK that reached a wallet without a journal row, so the ledger still sees
// it. Best-effort: the thing it records has already happened.
func (m *Mod) noteISKFlow(ctx context.Context, actor, line string, isk float64, corpID int64) {
	if m.db == nil || isk == 0 {
		return
	}
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_isk_flow (hour, actor, line, isk, inflow, n) VALUES (?, ?, ?, ?, ?, 1)
		ON DUPLICATE KEY UPDATE isk = isk + VALUES(isk), inflow = inflow + VALUES(inflow), n = n + 1`,
		now/3600, actor, line, isk, math.Max(isk, 0)); err != nil {
		rlogf("rolebots: isk flow note %s: %v", line, err)
		return
	}
	if corpID > 0 {
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_isk_flow_corp (day, corporationID, line, isk, inflow, n) VALUES (?, ?, ?, ?, ?, 1)
			ON DUPLICATE KEY UPDATE isk = isk + VALUES(isk), inflow = inflow + VALUES(inflow), n = n + 1`,
			now/86400, corpID, line, isk, math.Max(isk, 0)); err != nil {
			rlogf("rolebots: isk flow corp note %s: %v", line, err)
		}
	}
}

// noteBotISKFlow is noteISKFlow for a pilot, resolving its corporation.
func (m *Mod) noteBotISKFlow(ctx context.Context, charID int64, line string, isk float64) {
	var corpID int64
	if m.db != nil {
		_ = m.db.QueryRowContext(ctx,
			`SELECT COALESCE(corporationID, 0) FROM character_ WHERE characterID = ?`, charID).Scan(&corpID)
	}
	m.noteISKFlow(ctx, flowActorBot, line, isk, corpID)
}

// iskFlowStatus is the ledger-health readout.
type iskFlowStatus struct {
	Cursor, Head   int64
	BootstrappedAt int64 // unix; 0 = never bootstrapped
	BehindSecs     int64 // age of the oldest unread row; 0 when caught up
	LastSweepAt    int64 // unix; 0 = not since boot
	MarketCursor   int64
	MarketHead     int64
}

// Behind reports whether the ledger has unread journal rows.
func (s iskFlowStatus) Behind() bool { return s.Head > s.Cursor }

func (m *Mod) readISKFlowStatus(ctx context.Context) (iskFlowStatus, error) {
	var s iskFlowStatus
	s.LastSweepAt = iskFlowLastSweep.Load()
	err := m.db.QueryRowContext(ctx,
		`SELECT lastRefID, bootstrappedAt FROM rolebot_isk_flow_cursor WHERE id = 1`).
		Scan(&s.Cursor, &s.BootstrappedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return s, fmt.Errorf("isk flow status: %w", err)
	}
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(refID), 0) FROM market_journal`).Scan(&s.Head); err != nil {
		return s, fmt.Errorf("isk flow status head: %w", err)
	}
	if s.Head > s.Cursor {
		var td int64
		if err := m.db.QueryRowContext(ctx,
			`SELECT transDate FROM market_journal WHERE refID > ? ORDER BY refID LIMIT 1`,
			s.Cursor).Scan(&td); err == nil && td > 0 {
			if age := clock.Unix() - evedb.Win32ToUnix(td); age > 0 {
				s.BehindSecs = age
			}
		}
	}
	_ = m.db.QueryRowContext(ctx,
		`SELECT lastTxID FROM rolebot_mkt_flow_cursor WHERE id = 1`).Scan(&s.MarketCursor)
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(transactionID), 0) FROM market_transactions`).Scan(&s.MarketHead)
	return s, nil
}
