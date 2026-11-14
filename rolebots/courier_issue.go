package rolebots

// Bot courier contract issuance.
// A bot with scattered assets posts courier contracts instead of hauling everything itself.
// Entry points: warehouseRun (general scattered assets) and postRelocationCouriers (basing
// moves: corp join, factory shift, drift re-settle).

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"

	evedb "gurimoon/db"
	"gurimoon/server"
)

// ── config defaults ─────────────────────────────────────────────────────────

const (
	courierISKPerJump     = 15000.0 // base reward per jump
	courierMinJumpsToPost = 5       // don't bother for close stuff
	courierMaxRewardPct   = 0.25    // cap reward at 25% of cargo value
	courierCollateralMul  = 1.1     // collateral = value times this
	courierDefaultExpiry  = 7       // days
)

// ── reward pricing ──────────────────────────────────────────────────────────

// rewardForCourier prices a courier contract: jumps times courierISKPerJump, scaled by
// 1 + volume/CourierMaxVolumeM3, floored at the listing fee plus 5000 and capped at
// courierMaxRewardPct of the cargo value.
func rewardForCourier(jumps int, volume, value float64) float64 {
	iskPerJump := courierISKPerJump
	maxPct := courierMaxRewardPct

	base := float64(jumps) * iskPerJump
	volBonus := 1 + volume/evedb.CourierMaxVolumeM3
	reward := base * volBonus

	floor := evedb.CourierBasicFee + 5000
	if reward < floor {
		reward = floor
	}
	cap := value * maxPct
	if cap > 0 && reward > cap {
		reward = cap
	}
	return reward
}

// ── decision: haul vs. post ─────────────────────────────────────────────────

// shouldPostCourier decides whether a warehouse stop becomes a courier contract rather
// than a self-haul. It is also where haulers get their work: a pilot with little taste for
// hauling posts stranded goods from closer in (outsourceMinJumps), and a hauler carries
// its own.
func (m *Mod) shouldPostCourier(ctx context.Context, b *botRun, s warehouseStop) bool {
	if !courierWarehouseEnabled(ctx, m.db) {
		return false
	}
	minJumps := courierMinJumpsToPost
	if vs, ok, _ := evedb.GetServerSetting(ctx, m.db, "courier.minJumpsToPost"); ok {
		if v, err := strconv.Atoi(vs); err == nil && v >= 0 {
			minJumps = v
		}
	}
	if courierOutsourceEnabled(ctx, m.db) {
		var post bool
		minJumps, post = outsourceMinJumps(b.Tune, minJumps)
		if !post {
			return false
		}
		if m.openCouriersIssued(ctx, b.CharID) >= courierOutsourceMaxOpen(ctx, m.db) {
			return false
		}
	}
	if s.Jumps < minJumps {
		return false
	}
	if s.Value <= 0 {
		return false
	}
	reward := rewardForCourier(s.Jumps, 0, s.Value) // volume not known yet, conservative
	totalCost := reward + evedb.CourierBasicFee

	var balance float64
	m.db.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`,
		b.CharID).Scan(&balance)
	return spendable(balance) >= totalCost
}

// ── posting a contract from a warehouse stop ────────────────────────────────

// issueCourierContract posts courier contracts for goods at a remote station, to be hauled
// to the bot's home station, and returns how many it posted. Zero with no error means
// nothing worth a contract or the wallet ran out; the warehouse run then hauls it itself
// (warehouse.go).
func (m *Mod) issueCourierContract(ctx context.Context, b *botRun, s warehouseStop) (int, error) {
	homeStation := m.homeStationFor(ctx, b.CharID)
	if homeStation == 0 {
		return 0, fmt.Errorf("no home station to courier to")
	}
	return m.postCourierForStation(ctx, b, s.StationID, s.SystemID, homeStation, nil)
}

// ── posting relocation couriers ─────────────────────────────────────────────

// postRelocationCouriers posts courier contracts for all hangar items at fromStation, to be
// delivered to toStation.
func (m *Mod) postRelocationCouriers(ctx context.Context, b *botRun,
	fromStation, toStation int64) {

	if fromStation == 0 || toStation == 0 || fromStation == toStation {
		return
	}
	if !courierRelocationEnabled(ctx, m.db) {
		return
	}

	fromSysID := m.stationSystemOf(ctx, fromStation)
	if fromSysID == 0 {
		return
	}

	if _, err := m.postCourierForStation(ctx, b, fromStation, fromSysID, toStation, nil); err != nil {
		m.logBot(b.CharID, logInfo, "courier",
			"could not post relocation courier from %s: %v",
			m.stationLabel(ctx, fromStation), err)
	}
}

// ── core posting logic ──────────────────────────────────────────────────────

// postCourierForStation reads items at fromStation, batches them under 20k m3, and posts
// courier contracts to toStation. onlyItems, when given, limits the contract to those
// stacks (dead ammunition sent to a buyer, deadstock_pass.go); nil takes everything.
func (m *Mod) postCourierForStation(ctx context.Context, b *botRun,
	fromStation, fromSysID, toStation int64, onlyItems []int64) (int, error) {

	// Read hangar items at the remote station.
	type item struct {
		ItemID   int64
		TypeID   int64
		Quantity int64
		Volume   float64 // per unit
		Value    float64 // market value per unit (sell VWAP, then trades, then basePrice)
	}

	only, args := "", []any{b.CharID, fromStation}
	if len(onlyItems) > 0 {
		only = ` AND e.itemID IN (` + placeholders(len(onlyItems)) + `)`
		args = append(args, int64Args(onlyItems)...)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.quantity,
		       COALESCE(t.volume, 0)
		  FROM entity e
		  JOIN invTypes t  ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4
		   AND e.singleton = 0
		   AND g.categoryID IN (`+warehouseSweepCategories+`)`+only,
		args...)
	if err != nil {
		return 0, fmt.Errorf("query remote items: %w", err)
	}
	defer rows.Close()

	var items []item
	var totalVolume float64
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ItemID, &it.TypeID, &it.Quantity,
			&it.Volume); err != nil {
			return 0, fmt.Errorf("scan item: %w", err)
		}
		items = append(items, it)
		totalVolume += it.Volume * float64(it.Quantity)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("rows: %w", err)
	}
	if len(items) == 0 {
		return 0, nil
	}

	// Price items at market value for collateral with the killboard's chain (db.TypeValues):
	// sell-order VWAP, then 30-day trade VWAP, then basePrice.
	typeIDs := make([]int64, 0, len(items))
	for _, it := range items {
		typeIDs = append(typeIDs, it.TypeID)
	}
	prices, err := evedb.TypeValues(ctx, m.db, typeIDs)
	if err != nil {
		return 0, fmt.Errorf("price items: %w", err)
	}

	var totalValue float64
	for i := range items {
		if tv, ok := prices[items[i].TypeID]; ok {
			items[i].Value = tv.Value
		}
		totalValue += items[i].Value * float64(items[i].Quantity)
	}

	if totalValue < warehouseMinValue {
		return 0, nil // nothing worth couriering
	}

	// Resolve geography.
	fromRegionID := m.stationRegionOf(ctx, fromStation)
	toRec, _ := m.stationStaticRec(ctx, toStation)
	toSysID, toRegionID := toRec.SystemID, toRec.RegionID

	// Jumps drive the reward.
	jumps, err := m.jumpsBetween(ctx, b, fromSysID, toSysID)
	if err != nil || jumps <= 0 {
		return 0, fmt.Errorf("no route from %d to %d", fromStation, toStation)
	}

	// Batch into chunks under 20k m3.
	type batch struct {
		items  []item
		volume float64
		value  float64
	}
	var batches []batch
	cur := batch{}
	for _, it := range items {
		itVol := it.Volume * float64(it.Quantity)
		itVal := it.Value * float64(it.Quantity)

		if cur.volume+itVol > evedb.CourierMaxVolumeM3 && len(cur.items) > 0 {
			batches = append(batches, cur)
			cur = batch{}
		}
		// A single item over 20k m3 is skipped.
		if itVol > evedb.CourierMaxVolumeM3 {
			continue
		}
		cur.items = append(cur.items, it)
		cur.volume += itVol
		cur.value += itVal
	}
	if len(cur.items) > 0 {
		batches = append(batches, cur)
	}

	corpID, _ := evedb.GetCharCorpID(ctx, m.db, b.CharID)

	posted := 0
	for _, bat := range batches {
		if bat.value < warehouseMinValue {
			continue
		}

		reward := rewardForCourier(jumps, bat.volume, bat.value)
		collateral := bat.value * courierCollateralMul
		totalCost := reward + evedb.CourierBasicFee

		// Wallet check: an early out only. The debit below is the real test.
		var balance float64
		m.db.QueryRowContext(ctx,
			`SELECT balance FROM character_ WHERE characterID = ?`,
			b.CharID).Scan(&balance)
		if spendable(balance) < totalCost {
			m.logBot(b.CharID, logInfo, "courier",
				"cannot afford to post courier (need %.0f, spendable %.0f) — skipping remaining batches",
				totalCost, spendable(balance))
			break
		}

		// Post the contract in a transaction.
		tx, err := m.db.BeginTx(ctx, nil)
		if err != nil {
			return posted, fmt.Errorf("begin tx: %w", err)
		}

		// Debit wallet: relative and conditional on the floor, so a payment landing since the read
		// above is kept and a spend landing since cannot overdraw it.
		ok, newBalance, err := chargeSpendableTx(ctx, tx, b.CharID, totalCost)
		if err != nil {
			tx.Rollback()
			return posted, err
		}
		if !ok {
			tx.Rollback()
			m.logBot(b.CharID, logInfo, "courier",
				"cannot afford to post courier (need %.0f, the wallet moved since it was read) — skipping remaining batches",
				totalCost)
			break
		}

		// Journal.
		tx.ExecContext(ctx, `
			INSERT INTO market_journal
			  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
			   accountID, amount, balance, reason, accountKey)
			VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
			b.CharID, server.Win32TimeNow(), evedb.CourierRefType, b.CharID, b.CharID,
			-totalCost, newBalance,
			evedb.JournalReason("Courier contract created — reward + listing fee"))

		// Create escrow container.
		containerID, err := evedb.CreateCourierEscrowContainer(ctx, tx, fromStation)
		if err != nil {
			tx.Rollback()
			return posted, err
		}

		// Escrow items.
		for _, it := range bat.items {
			if err := evedb.EscrowCourierItem(ctx, tx, b.CharID, fromStation,
				containerID, it.ItemID, it.Quantity); err != nil {
				rlogf("rolebots: courier escrow item %d: %v — skipping", it.ItemID, err)
			}
		}

		// Insert mission.
		missionID, err := evedb.InsertCourierMission(ctx, tx, evedb.CourierCreateParams{
			IssuerID:            b.CharID,
			IssuerCorpID:        corpID,
			PickupStationID:     fromStation,
			DropStationID:       toStation,
			Reward:              reward,
			Collateral:          collateral,
			ListingFee:          evedb.CourierBasicFee,
			Volume:              bat.volume,
			Description:         "Bot hauling contract",
			DaysToExpire:        courierDefaultExpiry,
			ContainerID:         containerID,
			IssuerIsBot:         true,
			PickupSolarSystemID: fromSysID,
			PickupRegionID:      fromRegionID,
			DropSolarSystemID:   toSysID,
			DropRegionID:        toRegionID,
		})
		if err != nil {
			tx.Rollback()
			return posted, err
		}

		if err := tx.Commit(); err != nil {
			return posted, fmt.Errorf("commit: %w", err)
		}

		m.logBot(b.CharID, logInfo, "courier",
			"posted contract %d: %.0f m³, %.0f ISK value, %.0f ISK reward, %.0f ISK collateral, %d jumps from %s to %s",
			missionID, bat.volume, bat.value, reward, collateral, jumps,
			m.stationLabel(ctx, fromStation), m.stationLabel(ctx, toStation))
		posted++
	}

	if posted > 0 {
		m.logBot(b.CharID, logInfo, "courier",
			"posted %d courier contract(s) for goods at %s",
			posted, m.stationLabel(ctx, fromStation))
	}
	return posted, nil
}

// ── config helpers ──────────────────────────────────────────────────────────

func courierRelocationEnabled(ctx context.Context, db *sql.DB) bool {
	if vs, ok, _ := evedb.GetServerSetting(ctx, db, "courier.relocationEnabled"); ok {
		return vs == "1" || vs == "true"
	}
	return true // default on
}

// ── outsourcing: who posts, who carries ─────────────────────────────────────

const (
	// courierOutsourceFloorJumps is the closest a pilot that will not haul posts from.
	courierOutsourceFloorJumps = 2
	// courierOutsourceMaxOpenDefault caps one pilot's open (posted or in transit) contracts.
	courierOutsourceMaxOpenDefault = 3
)

// outsourceMinJumps returns the trip length from which this pilot hands its cargo to a
// hauler, and whether it ever does. A pilot whose hauling appetite is its strongest carries
// its own (post=false). Others post from base times haul/lead jumps. Pure.
func outsourceMinJumps(tn Tuning, base int) (int, bool) {
	lead := math.Max(math.Max(tn.MiningAppetite, tn.TradeAppetite),
		math.Max(tn.SupplyDrive*tn.IndustryAppetite, tn.ViolenceAppetite))
	if tn.HaulAppetite > lead || lead <= 0 {
		return 0, false
	}
	j := int(math.Round(float64(base) * tn.HaulAppetite / lead))
	if j < courierOutsourceFloorJumps {
		j = courierOutsourceFloorJumps
	}
	if j > base {
		j = base
	}
	return j, true
}

// openCouriersIssued counts the contracts this pilot has out: posted or in transit.
func (m *Mod) openCouriersIssued(ctx context.Context, charID int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM couriermissions
		 WHERE issuerID = ? AND state IN (?, ?)`,
		charID, evedb.CourierStatePosted, evedb.CourierStateAccepted).Scan(&n)
	return n
}

func courierOutsourceEnabled(ctx context.Context, db *sql.DB) bool {
	if vs, ok, _ := evedb.GetServerSetting(ctx, db, "courier.outsourceEnabled"); ok {
		return vs == "1" || vs == "true"
	}
	return true // default on
}

func courierOutsourceMaxOpen(ctx context.Context, db *sql.DB) int {
	if vs, ok, _ := evedb.GetServerSetting(ctx, db, "courier.outsourceMaxOpen"); ok {
		if v, err := strconv.Atoi(vs); err == nil && v > 0 {
			return v
		}
	}
	return courierOutsourceMaxOpenDefault
}

func courierWarehouseEnabled(ctx context.Context, db *sql.DB) bool {
	if vs, ok, _ := evedb.GetServerSetting(ctx, db, "courier.warehouseEnabled"); ok {
		return vs == "1" || vs == "true"
	}
	return true // default on
}
