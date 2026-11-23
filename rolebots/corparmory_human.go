package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/modapi"
	"gurimoon/server/gangsta"
)

// Kits for capsuleers (corparmory.go).
//
// A human draws a kit; it is never pushed one. On the Corporate Portal's Operations page a
// member aboard one of the corporation's fleets (per the fleet registry, which a header cannot
// forge) at merit rank Specialist or above is offered the seat's kit, handed over outright
// with a letter naming the seat, station and due time. A human outside Security holds no take on
// the Armory, so the kit is returned by parking it, empty, at the station it was drawn from. A kit sold,
// contracted away, left behind, or not back by the due date plus grace is stolen: the
// corporation sets the thief hostile, puts the kit's value on their head and bars them. A
// stripped return costs a month off the shelf and the merit.

const (
	settingArmoryHumanRank   = "rolebots.fleet.armoryHumanRank"
	settingArmoryHumanDueH   = "rolebots.fleet.armoryHumanDueHours"
	settingArmoryTheftGraceH = "rolebots.fleet.armoryTheftGraceHours"
	settingArmoryTheftStand  = "rolebots.fleet.armoryTheftStanding"
	settingArmoryTheftBounty = "rolebots.fleet.armoryTheftBountyMul"

	kitLostGraceSec   = 3600
	kitShortFrac      = 0.10 // a return missing more than this share of its value is short
	kitShortBarSec    = 30 * 86400
	kitShortMeritMul  = 1.0 // merit points docked per 1M ISK short
	kitTheftMinBounty = 1_000_000
)

var (
	armoryHumanRankV   atomic.Int64
	armoryHumanDueHV   atomic.Int64
	armoryTheftGraceHV atomic.Int64
	armoryTheftStandV  atomic.Int64 // standing ×100
	armoryTheftBountyV atomic.Int64 // multiplier ×100
)

func init() {
	armoryHumanRankV.Store(2) // Specialist (corpmerit.go)
	armoryHumanDueHV.Store(48)
	armoryTheftGraceHV.Store(72)
	armoryTheftStandV.Store(-1000)
	armoryTheftBountyV.Store(100)
}

func (m *Mod) loadArmoryHumanSettings(ctx context.Context) {
	read := func(key string, v *atomic.Int64, scale float64, lo, hi float64) {
		s, ok, err := evedb.GetServerSetting(ctx, m.db, key)
		if err != nil || !ok {
			return
		}
		var f float64
		if _, err := fmt.Sscan(s, &f); err != nil || f < lo || f > hi {
			return
		}
		v.Store(int64(f * scale))
	}
	read(settingArmoryHumanRank, &armoryHumanRankV, 1, 0, 10)
	read(settingArmoryHumanDueH, &armoryHumanDueHV, 1, 1, 24*30)
	read(settingArmoryTheftGraceH, &armoryTheftGraceHV, 1, 0, 24*30)
	read(settingArmoryTheftStand, &armoryTheftStandV, 100, -10, 0)
	read(settingArmoryTheftBounty, &armoryTheftBountyV, 100, 0, 10)
}

// kitBarred reports whether a pilot is barred from a corporation's shelf.
func (m *Mod) kitBarred(ctx context.Context, corpID, charID int64) (bool, string) {
	var reason string
	var until int64
	err := m.db.QueryRowContext(ctx, `
		SELECT reason, untilAt FROM rolebot_corp_barred WHERE corporationID = ? AND characterID = ?`,
		corpID, charID).Scan(&reason, &until)
	if err != nil {
		return false, ""
	}
	if until != 0 && until < clock.Unix() {
		return false, ""
	}
	return true, reason
}

// humanKitOffer is the Operations page's kit line for a fleet: the kit a viewer aboard it
// could draw, or why not. kit is nil with an empty why when there is nothing to say.
func (m *Mod) humanKitOffer(ctx context.Context, g *Gang, humanID int64) (*corpKit, string) {
	if g == nil || humanID == 0 || m.isSynthetic(humanID) {
		return nil, ""
	}
	fm := mirrorOf(g.GangID)
	if fm == nil || fm.RealGangID == 0 || gangsta.GangOf(humanID) != fm.RealGangID {
		return nil, ""
	}
	if m.charCorpID(ctx, humanID) != g.CorpID {
		return nil, ""
	}
	if _, managed := managedCEO(g.CorpID); !managed {
		return nil, ""
	}
	if is := m.openIssueFor(ctx, humanID); is != nil {
		return nil, ""
	}
	if barred, why := m.kitBarred(ctx, g.CorpID, humanID); barred {
		if why == "theft" {
			return nil, "The quartermaster will not issue you anything again."
		}
		return nil, "The quartermaster is not issuing to you this month — your last kit came back stripped."
	}
	need := int(armoryHumanRankV.Load())
	if rank := m.meritRankOf(ctx, g.CorpID, humanID); rank < need {
		return nil, fmt.Sprintf("The Armory issues kits from rank %s up.", meritRankName(need))
	}
	role := m.riderRoleOf(humanID, g)
	spec := seatSpec{}
	if fleetAttributesOre(g.Activity) && isHaulerRole(role) {
		spec = haulerSeatSpec
	} else if g.DoctrineKind != "" {
		if d := m.fleetDoctrineFor(ctx, g.CorpID, g.DoctrineKind); d != nil {
			spec = d.specFor(role)
		}
	}
	kits, err := m.corpKits(ctx, g.CorpID, "status = ? AND shipItemID <> 0", kitReady)
	if err != nil {
		return nil, ""
	}
	kind := kitKindFor(spec)
	for i := range kits {
		if kitMatches(kits[i], kind, spec) && m.canFlyKit(ctx, humanID, kits[i]) {
			return &kits[i], ""
		}
	}
	if len(kits) == 0 {
		return nil, ""
	}
	return nil, "Nothing on the shelf you have the skills to fly for this seat."
}

// riderRoleOf is the seat a human rides in, from their roster row; the op's default when the
// row says nothing.
func (m *Mod) riderRoleOf(humanID int64, g *Gang) string {
	var role string
	_ = m.db.QueryRow(`SELECT role FROM rolebot_gang_members WHERE gangID = ? AND characterID = ?`,
		g.GangID, humanID).Scan(&role)
	if role == "" {
		role = defaultRiderRole(g.Activity)
	}
	return role
}

// drawHumanKit issues the offered kit. The answer is a sentence for the page.
func (m *Mod) drawHumanKit(ctx context.Context, g *Gang, humanID int64) string {
	k, why := m.humanKitOffer(ctx, g, humanID)
	if k == nil {
		if why == "" {
			why = "There is no kit for you on this fleet."
		}
		return why
	}
	due := g.ExpiresAt + armoryHumanDueHV.Load()*3600
	if err := m.issueKit(ctx, *k, humanID, g.GangID, m.riderRoleOf(humanID, g), true, due); err != nil {
		if errors.Is(err, errKitGone) {
			return "Somebody drew that kit a moment ago."
		}
		rlogf("rolebots: human kit %d to %d: %v", k.KitID, humanID, err)
		return "The quartermaster could not issue it just now."
	}
	m.pushItemMoved(humanID, k.ShipItemID)
	hull := m.typeNameOr(ctx, k.HullTypeID)
	st := m.stationLabel(ctx, k.StationID)
	ceoID, _ := managedCEO(g.CorpID)
	from := ceoID
	if c, _ := getCEO(ctx, m.db, g.CorpID); c != nil {
		from, _ = m.letterFrom(ctx, c, mailFromSecurity)
	}
	m.mailPilot(ctx, humanID, from, "Armory issue: "+hull,
		fmt.Sprintf("<b>%s</b> has been issued to you from the %s for <b>%s</b>.<br><br>"+
			"Seat: %s<br>Collect it from your hangar at %s.<br>Due back: %s<br><br>"+
			"<b>To return it:</b> after the operation, dock at %s, board another ship or your capsule, "+
			"and leave the %s in your hangar there with its fittings aboard. The quartermaster collects it.<br><br>"+
			"A kit that is sold, traded, taken out of the corporation or not returned is theft, "+
			"and the corporation will treat it as such.",
			hull, kitLabel(k.Kind), g.Name, seatRoleLabel(m.riderRoleOf(humanID, g)), st,
			ycDate(time.Unix(due, 0))+" "+time.Unix(due, 0).UTC().Format("15:04"), st, hull))
	if ceoID != 0 {
		m.ceoLog(ctx, &corpCEO{CorpID: g.CorpID, CharID: ceoID}, powerProduction, "kit.issue", humanID, k.Value,
			"issued a %s from the %s to %s for %q", hull, kitLabel(k.Kind), m.charNameOr(ctx, humanID), g.Name)
	}
	return ""
}

// pushItemMoved tells an online pilot an item changed hands (the core's OnItemChange via
// modapi), so the hangar shows the kit without a relog.
func (m *Mod) pushItemMoved(charID, itemID int64) {
	if h := modapi.Corp().ItemChanged; h != nil {
		h(charID, itemID, 0, 0, 0)
	}
}

// ---- the watch ----

// kitWatch is what the quartermaster makes of one human's kit. Pure.
type kitWatch struct {
	ShipExists     bool
	Owner          int64
	Location, Flag int64
	Occupied       bool // a pilot is inside it
	HolderCorp     int64
	KilledRecently bool // a killmail of this hull and pilot within the grace
	OpOver         bool // the holder's fleet is gone or it has left it
}

const (
	watchHold   = ""
	watchLost   = "lost"
	watchStolen = "stolen"
	watchReturn = "return"
	watchWarn   = "warn"
)

func humanKitVerdict(is corpIssue, w kitWatch, now, graceSec int64) string {
	switch {
	case !w.ShipExists && w.KilledRecently:
		return watchLost
	case !w.ShipExists:
		return watchStolen
	case w.Owner == is.CorpID && w.Location != is.StationID:
		return watchReturn // already back in an office (an operator's hand)
	case w.Owner != is.CharID:
		return watchStolen // sold, contracted, given away
	case w.HolderCorp != is.CorpID:
		return watchStolen // left the corporation holding it
	case w.OpOver && w.Location == is.StationID && w.Flag == 4 && !w.Occupied:
		return watchReturn
	case now > is.DueAt+graceSec:
		return watchStolen
	case now > is.DueAt && is.WarnedAt == 0:
		return watchWarn
	}
	return watchHold
}

// armoryWatchHumans works every human kit of one corporation.
func (m *Mod) armoryWatchHumans(ctx context.Context, c *corpCEO) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+issueCols+` FROM rolebot_corp_issue
		WHERE corporationID = ? AND human = 1 AND status = ?`, c.CorpID, issueIssued)
	if err != nil {
		return
	}
	var list []corpIssue
	for rows.Next() {
		if is, err := scanIssue(rows); err == nil {
			list = append(list, is)
		}
	}
	rows.Close()
	now := clock.Unix()
	grace := armoryTheftGraceHV.Load() * 3600
	for i := range list {
		is := &list[i]
		w := m.watchKit(ctx, is)
		switch humanKitVerdict(*is, w, now, grace) {
		case watchLost:
			m.closeIssue(ctx, is, issueLost, "destroyed in the field")
			m.ceoLog(ctx, c, powerProduction, "kit.lost", is.HullTypeID, is.Value,
				"lost an %s %s flown by %s", kitLabel(is.Kind), m.typeNameOr(ctx, is.HullTypeID), m.charNameOr(ctx, is.CharID))
		case watchStolen:
			m.armoryTheft(ctx, c, is)
		case watchReturn:
			m.collectHumanKit(ctx, c, is)
		case watchWarn:
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_issue SET warnedAt = ? WHERE issueID = ?`, now, is.IssueID)
			from, _ := m.letterFrom(ctx, c, mailFromSecurity)
			m.mailPilot(ctx, is.CharID, from, "Armory: your kit is overdue",
				fmt.Sprintf("The %s issued to you is overdue.<br><br>Park it in your hangar at %s, "+
					"with its fittings aboard, within %d hours. After that it is counted stolen.",
					m.typeNameOr(ctx, is.HullTypeID), m.stationLabel(ctx, is.StationID), grace/3600))
		}
	}
}

func (m *Mod) watchKit(ctx context.Context, is *corpIssue) kitWatch {
	var w kitWatch
	err := m.db.QueryRowContext(ctx, `SELECT ownerID, locationID, flag FROM entity WHERE itemID = ?`,
		is.ShipItemID).Scan(&w.Owner, &w.Location, &w.Flag)
	w.ShipExists = err == nil
	if !w.ShipExists {
		var n int
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM killmails WHERE victimCharID = ? AND shipTypeID = ? AND killTime >= ?`,
			is.CharID, is.HullTypeID, is.IssuedAt-kitLostGraceSec).Scan(&n)
		w.KilledRecently = n > 0
	} else {
		var n int
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM entity e JOIN character_ ch ON ch.characterID = e.itemID
			 WHERE e.locationID = ?`, is.ShipItemID).Scan(&n)
		w.Occupied = n > 0
	}
	w.HolderCorp = m.charCorpID(ctx, is.CharID)
	g, _ := getGang(ctx, m.db, is.GangID)
	w.OpOver = g == nil || !g.Enabled || gangsta.GangOf(is.CharID) == 0 || clock.Unix() > g.ExpiresAt
	return w
}

// collectHumanKit takes a parked kit back: the hull and fittings to the corporation, the
// pilot's cargo to the pilot's hangar. A return short of what it went out with costs a month
// off the shelf and merit.
func (m *Mod) collectHumanKit(ctx context.Context, c *corpCEO, is *corpIssue) {
	flag, ok := m.corpDivFlag(ctx, is.CorpID, kitPurpose(is.Kind))
	officeID, _, err := evedb.GetCorpOfficeAt(ctx, m.db, is.CorpID, is.StationID)
	if !ok || err != nil || officeID == 0 || officeID == is.StationID {
		return // no shelf to put it on; it waits, still the pilot's, still due
	}
	// What the pilot keeps: anything in the hold.
	if _, err := m.db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 4 WHERE locationID = ? AND flag = 5 AND ownerID = ?`,
		is.StationID, is.ShipItemID, is.CharID); err != nil {
		return
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE entity SET ownerID = ? WHERE locationID = ?`, is.CorpID, is.ShipItemID); err != nil {
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE entity SET ownerID = ?, locationID = ?, flag = ? WHERE itemID = ?`,
		is.CorpID, officeID, flag, is.ShipItemID); err != nil {
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rolebot_corp_issue WHERE issueID = ?`, is.IssueID); err != nil {
		return
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE rolebot_corp_kit SET status = ? WHERE kitID = ?`, kitPlanned, is.KitID); err != nil {
		return
	}
	if tx.Commit() != nil {
		return
	}
	m.pushItemMoved(is.CharID, is.ShipItemID)
	hull := m.typeNameOr(ctx, is.HullTypeID)
	short := 0.0
	if k := m.kitByID(ctx, is.KitID); k != nil && k.Fit != nil {
		if missing, err := m.shipMissing(ctx, is.ShipItemID, k.Fit); err == nil {
			for _, it := range missing {
				if it.Dest == "module" || it.Dest == "drone" {
					short += evedb.TypeValueOf(ctx, m.db, it.TypeID).Value * float64(it.Qty)
				}
			}
		}
	}
	if is.Value > 0 && short > is.Value*kitShortFrac {
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_barred (corporationID, characterID, reason, value, issueID, barredAt, untilAt)
			VALUES (?, ?, 'short', ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE reason = IF(reason = 'theft', reason, 'short'),
			    value = VALUES(value), barredAt = VALUES(barredAt),
			    untilAt = IF(reason = 'theft', 0, VALUES(untilAt))`,
			is.CorpID, is.CharID, short, is.IssueID, clock.Unix(), clock.Unix()+kitShortBarSec)
		if meritOn() {
			m.addMerit(ctx, is.CorpID, is.CharID, -short/1e6*kitShortMeritMul,
				fmt.Sprintf("brought the Armory's %s back %s short", hull, iskShort(short)))
		}
		from, _ := m.letterFrom(ctx, c, mailFromSecurity)
		m.mailPilot(ctx, is.CharID, from, "Armory: kit returned short",
			fmt.Sprintf("The %s came back missing about %s of its fittings.<br><br>"+
				"The quartermaster will not issue to you for thirty days.", hull, iskShort(short)))
		m.ceoLog(ctx, c, powerProduction, "kit.short", is.CharID, short,
			"took back a %s from %s, %s short", hull, m.charNameOr(ctx, is.CharID), iskShort(short))
		return
	}
	m.ceoLog(ctx, c, powerProduction, "kit.back", is.CharID, is.Value,
		"took back the %s it lent %s", hull, m.charNameOr(ctx, is.CharID))
}

// armoryTheft is the corporation's answer to a kit that did not come back.
func (m *Mod) armoryTheft(ctx context.Context, c *corpCEO, is *corpIssue) {
	m.closeIssue(ctx, is, issueStolen, "not returned")
	hull := m.typeNameOr(ctx, is.HullTypeID)
	m.punishTheft(ctx, c, theftCase{
		CharID: is.CharID, Value: is.Value, IssueID: is.IssueID,
		Deed:    "stole a " + hull + " from the Armory",
		Subject: "Armory: theft",
		Letter:  fmt.Sprintf("The %s issued to you was not returned. The corporation counts it stolen.", hull),
		Log:     fmt.Sprintf("counts the %s lent to %s stolen", hull, m.charNameOr(ctx, is.CharID)),
	})
}

// theftCase is one theft from the corporation, as punishTheft answers it.
type theftCase struct {
	CharID  int64
	Value   float64
	IssueID int64  // the kit issue or sign-out it was, 0 for none
	Deed    string // the standing's reason: "stole a Rifter from the Armory"
	Subject string // the letter's subject
	Letter  string // the letter's first paragraph
	Log     string // the CEO log line, before what was done about it
}

// punishTheft is the corporation's answer to theft: the thief is barred from the Armory (and so
// loses its take on the war chest, humanGuardedTake) and from rejoining, set hostile, and has the
// value put on its head.
func (m *Mod) punishTheft(ctx context.Context, c *corpCEO, t theftCase) {
	name := m.charNameOr(ctx, t.CharID)
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_barred (corporationID, characterID, reason, value, issueID, barredAt, untilAt)
		VALUES (?, ?, 'theft', ?, ?, ?, 0)
		ON DUPLICATE KEY UPDATE reason = 'theft', value = value + VALUES(value), issueID = VALUES(issueID),
		    barredAt = VALUES(barredAt), untilAt = 0`,
		c.CorpID, t.CharID, t.Value, t.IssueID, clock.Unix())
	standing := float64(armoryTheftStandV.Load()) / 100
	if err := evedb.SetCorpStanding(ctx, m.db, c.CorpID, t.CharID, standing, 0, t.Deed); err != nil {
		rlogf("rolebots: theft standing corp=%d char=%d: %v", c.CorpID, t.CharID, err)
	}
	bounty := 0.0
	if mul := float64(armoryTheftBountyV.Load()) / 100; mul > 0 {
		price := t.Value * mul
		if price < kitTheftMinBounty {
			price = kitTheftMinBounty
		}
		if m.corpFilesBounty(ctx, c.CorpID, &pvpCombatant{CharID: t.CharID, Name: name}, price) {
			bounty = price
		}
	}
	from, _ := m.letterFrom(ctx, c, mailFromSecurity)
	m.mailPilot(ctx, t.CharID, from, t.Subject,
		fmt.Sprintf("%s<br><br>You are barred from its Armory and from rejoining it%s.",
			t.Letter, map[bool]string{true: fmt.Sprintf(", and %s has been placed on your head", iskShort(bounty))}[bounty > 0]))
	m.ceoLog(ctx, c, powerProduction, "kit.theft", t.CharID, t.Value,
		"%s — set them hostile%s and barred them",
		t.Log, map[bool]string{true: ", put " + iskShort(bounty) + " on their head"}[bounty > 0])
	// The bar takes its war-chest take back now, not at the next arm pass.
	if seat, ok := armOf(t.CharID, c.CorpID); ok && seat.Arm != "" {
		if row, err := m.readMemberRoleRow(ctx, t.CharID); err == nil {
			if err := m.setMemberRoles(ctx, t.CharID, row.Corprole); err != nil {
				rlogf("rolebots: theft roles %d: %v", t.CharID, err)
			}
		}
	}
}

// reconcileBotIssues settles bot loans that can no longer come home on their own: a pilot
// purged or switched off, or one long overdue.
func (m *Mod) reconcileBotIssues(ctx context.Context, c *corpCEO) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+issueCols+` FROM rolebot_corp_issue
		WHERE corporationID = ? AND human = 0 AND status = ? AND dueAt < ?`, c.CorpID, issueIssued, clock.Unix())
	if err != nil {
		return
	}
	var list []corpIssue
	for rows.Next() {
		if is, err := scanIssue(rows); err == nil {
			list = append(list, is)
		}
	}
	rows.Close()
	for i := range list {
		is := &list[i]
		var enabled sql.NullBool
		_ = m.db.QueryRowContext(ctx, `SELECT enabled FROM rolebot_characters WHERE characterID = ?`, is.CharID).Scan(&enabled)
		gone := !enabled.Valid || !enabled.Bool
		if !gone && clock.Unix() < is.DueAt+7*86400 {
			continue // it will bring it back on its next solo pass
		}
		w := m.watchKit(ctx, is)
		if w.ShipExists && !w.Occupied && w.Location == is.StationID {
			if flag, ok := m.corpDivFlag(ctx, is.CorpID, kitPurpose(is.Kind)); ok {
				if officeID, _, err := evedb.GetCorpOfficeAt(ctx, m.db, is.CorpID, is.StationID); err == nil && officeID != 0 && officeID != is.StationID {
					_, _ = m.db.ExecContext(ctx, `UPDATE entity SET locationID = ?, flag = ?, ownerID = ? WHERE itemID = ?`,
						officeID, flag, is.CorpID, is.ShipItemID)
					unloanKit(is.ShipItemID)
					_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_issue WHERE issueID = ?`, is.IssueID)
					_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_kit SET status = ? WHERE kitID = ?`, kitPlanned, is.KitID)
					continue
				}
			}
		}
		m.closeIssue(ctx, is, issueWritten, "its pilot never brought it back")
	}
}
