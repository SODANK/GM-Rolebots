package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// The war power: a CEO taking a quarrel to CONCORD, paying for it, and giving
// it up. The core carries the war itself (db/wardb.go, warwrites.go); this power
// declares, funds and ends wars for a bot corporation. Every other player
// corporation is scored on a motive (ceoWarMotive); past Charter.WarAt the CEO
// either fights (declares itself) or hires a mercenary (merc.go). It pays and
// retracts only wars in its own book (rolebot_ceo_wars), and never lets a war
// bankrupt the corporation: fee plus runway must fit the war chest and every
// bill is paid only while the wallet stays above the reserve.
// See docs/comment-notes-b04.md#war-power.

const (
	warMotiveGrievance   = "grievance"
	warMotiveGank        = "gank" // they suicide-ganked ours in policed space (gankrel.go)
	warMotivePredation   = "predation"
	warMotiveCompetition = "competition"
	warMotiveAlly        = "ally"
	warMotiveDefence     = "defence"
	warMotiveContract    = "contract"

	warModeNone  = ""
	warModeFight = "fight"
	warModeHire  = "hire"

	warVerdictKeep    = "keep"
	warVerdictLapse   = "lapse"
	warVerdictRetract = "retract"

	warWeekSec = 7 * 86400
	// warHighsecLine is the policed-space line a predation war is bought to
	// cross (marketsec.go and the security bands use the same 0.45).
	warHighsecLine = 0.45

	// What each signal is worth toward a war. The scale is diplomacy's (+-10);
	// Charter.WarAt (3.5 to 9) is where the sum becomes a declaration.
	warEnemyWeight  = 5.0 // a declared enemy, held past the dwell
	warRivalWeight  = 2.0 // a declared rival, held past the dwell
	warGrievanceCap = 6.0 // members of ours they killed, capped
	warPreyShare    = 0.8 // × Charter.PreyWeight
	warCompeteShare = 1.5 // × Charter.CompeteWeight
	warAllyWeight   = 10  // × Charter.ReciprocityWarm (0.2–0.6)

	// warArmedFrac / warSoftFrac is the share of a corporation's members that
	// count as fighters, by whether it wears an armed tag.
	warArmedFrac = 0.8
	warSoftFrac  = 0.25
	// warPainFloorISK: losses under this never make a war bleeding; one frigate
	// lost against nothing killed is not a rout.
	warPainFloorISK = 20e6
	// warCoolFrac: a war whose motive has fallen under this share of
	// Charter.WarAt is over as far as the CEO is concerned.
	warCoolFrac = 0.5
)

// ── the pure half ───────────────────────────────────────────────────────────

// warMotiveIn is every signal about one possible enemy, from one CEO.
type warMotiveIn struct {
	Standing      float64 // what our corporation has declared toward them
	StandingHeld  bool    // ... and whether that has stood the CEO's dwell
	Grievance     float64 // decayed count of our members they killed
	Ganked        float64 // how many of ours they suicide-ganked in policed space (own term, outside the cap)
	SelfOutlaw    bool
	TargetSoft    bool // a soft trade and no armed tag
	TargetOutlaw  bool
	TargetHighsec bool // its home is in policed space
	Competes      bool // the same trade in the same region
	AllyAggressor bool // at war with one of our mutual allies, as the declarer
}

// ceoWarMotive is how much this CEO wants a war with this target, which kind
// of war it would be, and why. Pure.
func ceoWarMotive(in warMotiveIn, ch Charter) (float64, string, []ceoRelTerm) {
	if in.Standing > 0 {
		return 0, "", nil // no war on a corporation we have called a friend
	}
	var terms []ceoRelTerm
	byKind := map[string]float64{}
	add := func(kind string, v float64, label string) {
		if v <= 0 {
			return
		}
		terms = append(terms, ceoRelTerm{label, v})
		byKind[kind] += v
	}
	if in.StandingHeld {
		switch {
		case in.Standing <= ceoRelEnemy:
			add(warMotiveGrievance, warEnemyWeight, "a declared enemy")
		case in.Standing <= ceoRelRival:
			add(warMotiveGrievance, warRivalWeight, "a declared rival")
		}
	}
	if in.Grievance > 0 {
		add(warMotiveGrievance, math.Min(in.Grievance*ch.GrievanceWeight, warGrievanceCap),
			fmt.Sprintf("killed %s of ours lately", countWord(in.Grievance)))
	}
	if in.Ganked > 0 {
		// A gank is outside warGrievanceCap: a careful CEO's WarAt is 9, and a soft
		// outfit cannot answer a gank with its guns.
		add(warMotiveGank, gankWarTerm(in.Ganked, ch.GrievanceWeight),
			fmt.Sprintf("ganked %s of ours in highsec", countWord(in.Ganked)))
	}
	if in.SelfOutlaw && in.TargetSoft && !in.TargetOutlaw && in.TargetHighsec {
		add(warMotivePredation, ch.PreyWeight*warPreyShare, "soft, rich, and hiding in highsec")
	}
	if !in.SelfOutlaw && in.Competes {
		add(warMotiveCompetition, ch.CompeteWeight*warCompeteShare, "a competitor on our own ground")
	}
	if in.AllyAggressor {
		add(warMotiveAlly, ch.ReciprocityWarm*warAllyWeight, "at war with an ally of ours")
	}
	total, kind, best := 0.0, "", 0.0
	for _, k := range []string{warMotiveGrievance, warMotiveGank, warMotivePredation, warMotiveCompetition, warMotiveAlly} {
		v := byKind[k]
		total += v
		if v > best {
			kind, best = k, v
		}
	}
	sort.SliceStable(terms, func(i, j int) bool { return terms[i].V > terms[j].V })
	return total, kind, terms
}

// warStrength is a corporation's fighting weight: its members, the share of
// them that fight, and its killboard efficiency (eff in [0,1]; below 0 when
// it has no record, which reads as middling).
func warStrength(members int, armed bool, eff float64) float64 {
	if members <= 0 {
		return 0
	}
	frac := warSoftFrac
	if armed {
		frac = warArmedFrac
	}
	if eff < 0 {
		eff = 0.5
	}
	return float64(members) * frac * (0.5 + eff)
}

// ceoWarMode decides how a war of this kind is fought: by the corporation
// itself, by a hired mercenary, or not at all. Pure.
func ceoWarMode(kind string, ours, theirs float64, combat, hireOn bool, ch Charter) string {
	if combat && kind != warMotiveCompetition && ours > 0 && theirs <= ours*ch.WarMaxOdds {
		return warModeFight
	}
	if hireOn {
		return warModeHire
	}
	return warModeNone
}

// warBookIn is what a CEO knows about one of its running wars.
type warBookIn struct {
	Motive       float64 // what it thinks of the enemy now
	WeeksRun     float64
	Killed, Lost float64 // ISK, this war, from the killboard
	Spent        float64 // fee + bills paid
	Contract     bool    // a merc's contract war: the contract decides
	// Outmatched: the enemy's fighting weight (from hulls) is far past ours
	// (warOutmatched). Only retracts a war that has not yet hurt them.
	Outmatched bool
}

// ceoWarExhaustion is whether a CEO keeps paying for a war, lets its bill
// lapse, or retracts it. Pure.
func ceoWarExhaustion(in warBookIn, ch Charter) (string, string) {
	if in.Contract {
		return warVerdictKeep, "under contract"
	}
	if in.Lost > warPainFloorISK && in.Lost > math.Max(in.Killed, 1)*ch.WarPainRatio {
		return warVerdictRetract, fmt.Sprintf("losing %s against %s killed", iskString(in.Lost), iskString(in.Killed))
	}
	if in.Outmatched && in.Killed < warPainFloorISK {
		return warVerdictRetract, "outmatched, and it has not hurt them"
	}
	if in.Motive < ch.WarAt*warCoolFrac {
		return warVerdictRetract, "the quarrel has cooled"
	}
	if in.WeeksRun >= ch.WarPatienceWeeks && in.Killed < in.Spent {
		return warVerdictLapse, fmt.Sprintf("%.0f weeks in and it has not paid for itself", in.WeeksRun)
	}
	return warVerdictKeep, ""
}

// warFooting puts a directive on a war footing. fight moves `footing` of every
// share onto combat; otherwise the corporation hunkers down and that share of
// mining and hauling moves to work done from a station. Shares still sum to
// what they did. Pure.
func warFooting(dir map[string]float64, footing float64, fight bool) map[string]float64 {
	out := make(map[string]float64, len(dir)+1)
	for k, v := range dir {
		out[k] = v
	}
	if footing <= 0 || len(out) == 0 {
		return out
	}
	footing = math.Min(footing, 0.9)
	if fight {
		for k := range out {
			out[k] *= 1 - footing
		}
		out[ActCombat] += footing
		return out
	}
	moved := 0.0
	for _, k := range []string{ActMining, ActCourier} {
		if v := out[k]; v > 0 {
			d := v * footing
			out[k] = v - d
			moved += d
		}
	}
	if moved == 0 {
		return out
	}
	docked := out[ActIndustry] + out[ActTrade]
	if docked == 0 {
		out[ActIndustry] += moved
		return out
	}
	for _, k := range []string{ActIndustry, ActTrade} {
		if v := out[k]; v > 0 {
			out[k] = v + moved*v/docked
		}
	}
	return out
}

// warFootingSize is how far a war pulls: the charter's ceiling, scaled by how
// outmatched the corporation is (theirs ÷ ours), never below a third of it.
func warFootingSize(ch Charter, ours, theirs float64) float64 {
	r := 1.0
	if ours > 0 {
		r = theirs / ours
	}
	return ch.WarFootingMax * math.Max(1.0/3, math.Min(r, 1))
}

// ── the reads ───────────────────────────────────────────────────────────────

// warBookRow is one rolebot_ceo_wars row.
type warBookRow struct {
	WarID, CorpID, Target int64
	OwnerID               int64 // the alliance, for a war declared in its name; 0 = the corporation's own
	Motive                string
	ContractID            int64
	DeclaredAt            int64
	BillsPaid             float64
	Reason                string
}

// owner is the party the war is declared by on the wire.
func (r warBookRow) owner() int64 {
	if r.OwnerID != 0 {
		return r.OwnerID
	}
	return r.CorpID
}

func (m *Mod) ceoWarBook(ctx context.Context, corpID int64) ([]warBookRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT warID, corporationID, targetOwnerID, COALESCE(ownerID, 0), motive, contractID, declaredAt, billsPaid, reason
		  FROM rolebot_ceo_wars WHERE corporationID = ? AND endedAt = 0 ORDER BY warID`, corpID)
	if err != nil {
		return nil, fmt.Errorf("ceoWarBook: %w", err)
	}
	defer rows.Close()
	var out []warBookRow
	for rows.Next() {
		var r warBookRow
		if err := rows.Scan(&r.WarID, &r.CorpID, &r.Target, &r.OwnerID, &r.Motive, &r.ContractID,
			&r.DeclaredAt, &r.BillsPaid, &r.Reason); err != nil {
			return nil, fmt.Errorf("ceoWarBook scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (m *Mod) ceoWarBookAdd(ctx context.Context, warID, corpID, target int64, motive string, contractID int64, reason string) error {
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_ceo_wars (warID, corporationID, targetOwnerID, motive, contractID, declaredAt, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), endedAt = 0`,
		warID, corpID, target, motive, contractID, clock.Unix(), truncate(reason, 255))
	if err != nil {
		return fmt.Errorf("ceoWarBookAdd: %w", err)
	}
	return nil
}

func (m *Mod) ceoWarBookEnd(ctx context.Context, warID int64, reason string) {
	_, _ = m.db.ExecContext(ctx,
		`UPDATE rolebot_ceo_wars SET endedAt = ?, reason = ? WHERE warID = ? AND endedAt = 0`,
		clock.Unix(), truncate(reason, 255), warID)
	m.closeCommit(ctx, "war", warID)
}

// minKillSince is the first killID at or after a time, 0 when the board has
// nothing that recent. Killboard reads bound their attacker scan by it: the
// attackers table is keyed (corpID, killID) and killIDs rise with time (as in
// relKillboardCatchUp).
func (m *Mod) minKillSince(ctx context.Context, since int64) int64 {
	var id sql.NullInt64
	if err := m.db.QueryRowContext(ctx,
		`SELECT MIN(killID) FROM killmails WHERE killTime >= ?`, since).Scan(&id); err != nil || !id.Valid {
		return 0
	}
	return id.Int64
}

// corpKillEfficiency is ISK killed ÷ (killed + lost) over the diplomacy
// window, −1 when the corporation has no record there. A missing board is no
// record, never an error.
func (m *Mod) corpKillEfficiency(ctx context.Context, corpID, minKill, since int64) float64 {
	var killed, lost float64
	if minKill > 0 {
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(k.totalValue), 0)
			  FROM killmails k
			  JOIN (SELECT DISTINCT killID FROM killmail_attackers
			         WHERE corpID = ? AND killID >= ?) a ON a.killID = k.killID
			 WHERE k.victimCorpID <> ?`, corpID, minKill, corpID).Scan(&killed)
	}
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(totalValue), 0) FROM killmails
		 WHERE victimCorpID = ? AND killTime >= ?`, corpID, since).Scan(&lost)
	if killed+lost <= 0 {
		return -1
	}
	return killed / (killed + lost)
}

// warScore is what a war has cost and earned one side: ISK of the target it
// destroyed, and ISK it lost to the target, since a time. The target is an
// owner: a corporation or an alliance.
func (m *Mod) warScore(ctx context.Context, selfCorp, target, since int64) (killed, lost float64) {
	minKill := m.minKillSince(ctx, since)
	if minKill == 0 {
		return 0, 0
	}
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(k.totalValue), 0)
		  FROM killmails k
		  JOIN (SELECT DISTINCT killID FROM killmail_attackers
		         WHERE corpID = ? AND killID >= ?) a ON a.killID = k.killID
		 WHERE k.victimCorpID = ? OR k.victimAllianceID = ?`,
		selfCorp, minKill, target, target).Scan(&killed)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(k.totalValue), 0)
		  FROM killmails k
		 WHERE k.victimCorpID = ? AND k.killID >= ?
		   AND EXISTS (SELECT 1 FROM killmail_attackers a
		                WHERE a.killID = k.killID AND (a.corpID = ? OR a.allianceID = ?))`,
		selfCorp, minKill, target, target).Scan(&lost)
	return killed, lost
}

// commitRemaining is what is still unspent on one open commitment.
func (m *Mod) commitRemaining(ctx context.Context, kind string, refID int64) float64 {
	var v float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(GREATEST(amount - spent, 0), 0) FROM rolebot_corp_commit
		 WHERE refKind = ? AND refID = ? AND closedAt = 0`, kind, refID).Scan(&v)
	return v
}

// warChest is what one war or one hire may commit: the charter's share of free
// capital, under the operator's ceiling.
func (m *Mod) warChest(ctx context.Context, corpID int64, ch Charter) (chest, free float64, err error) {
	free, _, err = m.corpFreeCapital(ctx, corpID, ch)
	if err != nil {
		return 0, 0, err
	}
	return free * math.Min(ch.WarChestFrac, warChestMaxFrac()), free, nil
}

// canPay reports whether paying amount now keeps the wallet above the reserve
// and inside what is not already promised elsewhere. own is what the payment's
// own commitment still holds (money set aside for this).
func (m *Mod) canPay(ctx context.Context, corpID int64, ch Charter, amount, own float64) bool {
	free, _, err := m.corpFreeCapital(ctx, corpID, ch)
	if err != nil {
		return false
	}
	return amount <= free+own
}

// warActive is the war's live row when it has not finished.
func warActive(ctx context.Context, db *sql.DB, warID int64) (*evedb.War, bool) {
	w, err := evedb.GetWar(ctx, db, warID)
	if err != nil || w == nil || w.TimeFinished.Valid {
		return w, false
	}
	return w, true
}

// ── the power ───────────────────────────────────────────────────────────────

// ceoWar is one think's pass: settle what is running (the book, the bills, the
// merc contracts on both sides), then consider one new war or hire.
func (m *Mod) ceoWar(ctx context.Context, c *corpCEO, ch Charter) error {
	if err := m.ceoWarSettle(ctx, c, ch); err != nil {
		return err
	}
	// Mutuality before anything new: it also runs with the switches off, to
	// hand back the mutual wars this side agreed to (corpceo_warmutual.go).
	if err := m.ceoWarMutual(ctx, c, ch); err != nil {
		return err
	}
	if err := m.mercTick(ctx, c, ch); err != nil {
		return err
	}
	if !ceoWarsOn() || !relationsEnabled() {
		return nil
	}
	return m.ceoWarConsider(ctx, c, ch)
}

// ceoWarSettle walks the book: drops wars the core has finished, pays or
// lapses each bill owed, and retracts a war the CEO has had enough of.
func (m *Mod) ceoWarSettle(ctx context.Context, c *corpCEO, ch Charter) error {
	book, err := m.ceoWarBook(ctx, c.CorpID)
	if err != nil {
		return err
	}
	byWar := map[int64]*warBookRow{}
	now := clock.Unix()
	var cands map[int64]*warCand
	for i := range book {
		b := &book[i]
		w, live := warActive(ctx, m.db, b.WarID)
		if !live {
			why := "the war is over"
			if w == nil {
				why = "the war is gone"
			}
			m.ceoWarBookEnd(ctx, b.WarID, why)
			m.ceoLog(ctx, c, powerWar, "warover", b.Target, 0, "war on %s is over — %s",
				ownerNameOf(ctx, m.db, b.Target), why)
			continue
		}
		byWar[b.WarID] = b
		if b.ContractID != 0 || w.Retracted.Valid {
			continue
		}
		if !ceoWarsOn() {
			continue // bills lapse below; the switch is how an operator ends them
		}
		if cands == nil {
			if cands, err = m.ceoWarCandidates(ctx, c, ch); err != nil {
				return err
			}
		}
		motive := 0.0
		for _, x := range cands {
			if x.Owner == b.Target && x.Motive > motive {
				motive = x.Motive
			}
		}
		killed, lost := m.warScore(ctx, c.CorpID, b.Target, b.DeclaredAt)
		verdict, why := ceoWarExhaustion(warBookIn{
			Motive: motive, WeeksRun: float64(now-b.DeclaredAt) / warWeekSec,
			Killed: killed, Lost: lost,
			Spent:      evedb.WarDeclarationCost(ctx, m.db) + b.BillsPaid,
			Outmatched: m.warBookOutmatched(ctx, c, ch, cands, b.Target),
		}, ch)
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_ceo_wars SET lastScore = ? WHERE warID = ?`, motive, b.WarID)
		if verdict == warVerdictRetract {
			if err := m.retractWar(ctx, b.owner(), b.WarID); err != nil {
				m.logBot(c.CharID, logInfo, "ceo", "war: retract %d refused: %v", b.WarID, err)
				continue
			}
			m.ceoLog(ctx, c, powerWar, "retract", b.Target, 0, "retracted its war on %s — %s",
				ownerNameOf(ctx, m.db, b.Target), why)
		} else if verdict == warVerdictLapse {
			b.Reason = "lapse: " + why
		}
	}
	return m.ceoPayWarBills(ctx, c, ch, byWar)
}

// ceoPayWarBills pays every war bill the corporation owes for a war in its book
// that it still wants, only while the wallet stays above the reserve. Unpaid
// bills lapse in the core's sweep at the end of the grace.
func (m *Mod) ceoPayWarBills(ctx context.Context, c *corpCEO, ch Charter, byWar map[int64]*warBookRow) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT billID, amount, externalID2 FROM bills
		 WHERE debtorID = ? AND billTypeID = ? AND paid = 0`, c.CorpID, evedb.BillTypeWarBill)
	if err != nil {
		return nil // no bills table is no bills
	}
	type bill struct {
		id, warID int64
		amount    float64
	}
	var bills []bill
	for rows.Next() {
		var b bill
		if rows.Scan(&b.id, &b.amount, &b.warID) == nil {
			bills = append(bills, b)
		}
	}
	rows.Close()
	hook := modapi.Corp().PayBill
	for _, b := range bills {
		row := byWar[b.warID]
		if row == nil {
			continue // not our war to fund
		}
		keep, why := true, ""
		switch {
		case !ceoWarsOn() && row.ContractID == 0:
			keep, why = false, "wars are switched off"
		case row.ContractID != 0 && !m.mercContractLive(ctx, row.ContractID):
			keep, why = false, "the contract behind it is over"
		case strings.HasPrefix(row.Reason, "lapse: "):
			keep, why = false, strings.TrimPrefix(row.Reason, "lapse: ")
		}
		if keep && !m.canPay(ctx, c.CorpID, ch, b.amount, m.commitRemaining(ctx, "war", b.warID)) {
			keep, why = false, "the treasury cannot carry it"
		}
		if keep && hook == nil {
			keep, why = false, "the server has no bill hook"
		}
		if !keep {
			if m.lastWarBillNote(ctx, c.CorpID, b.id) {
				continue
			}
			m.ceoLog(ctx, c, powerWar, "lapse", b.id, b.amount, "left CONCORD's %s bill for the war on %s unpaid — %s",
				iskString(b.amount), ownerNameOf(ctx, m.db, row.Target), why)
			continue
		}
		if err := hook(b.id, c.CharID); err != nil {
			m.logBot(c.CharID, logInfo, "ceo", "war: pay bill %d: %v", b.id, err)
			continue
		}
		m.spendCommit(ctx, "war", b.warID, b.amount)
		_, _ = m.db.ExecContext(ctx,
			`UPDATE rolebot_ceo_wars SET billsPaid = billsPaid + ? WHERE warID = ?`, b.amount, b.warID)
		m.ceoLog(ctx, c, powerWar, "warbill", b.id, b.amount, "paid CONCORD %s to keep the war on %s going",
			iskString(b.amount), ownerNameOf(ctx, m.db, row.Target))
		if row.OwnerID != 0 {
			// An alliance war: the executor fronted it, the members share it.
			m.allianceWarBillPaid(ctx, c, b.warID, b.amount)
		}
	}
	return nil
}

// warBookOutmatched is whether the enemy a running war is against can field far
// more than this corporation's own wing, by hulls (corpceo_hulls.go). It uses the
// same odds and Director-of-Security advice as a new declaration, so a war is
// called off by the rule it was started under.
func (m *Mod) warBookOutmatched(ctx context.Context, c *corpCEO, ch Charter, cands map[int64]*warCand, target int64) bool {
	selfFacts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil || selfFacts == nil {
		return false
	}
	since := clock.Unix() - ceoRelWindowDays*86400
	minKill := m.minKillSince(ctx, since)
	ours := m.corpFightStrength(ctx, c.CorpID, selfFacts.Members, true, ceoCombatOriented(ch),
		m.corpKillEfficiency(ctx, c.CorpID, minKill, since))
	theirs := 0.0
	for _, x := range cands {
		if x.Owner != target {
			continue
		}
		theirs += m.corpFightStrength(ctx, x.Facts.CorpID, x.Facts.Members, false,
			x.Party.hasAny(ceoRelArmedTags), m.corpKillEfficiency(ctx, x.Facts.CorpID, minKill, since))
	}
	return warOutmatched(ours, theirs, ch.WarMaxOdds*m.securityOddsMul(ctx, c))
}

// lastWarBillNote reports whether the ledger already says this bill is being
// left to lapse, so an unpaid bill is one ledger line, not one per think.
func (m *Mod) lastWarBillNote(ctx context.Context, corpID, billID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_ceo_actions
		 WHERE corporationID = ? AND verb = 'lapse' AND subjectID = ?`, corpID, billID).Scan(&n)
	return n > 0
}

// retractWar retracts one war through the core.
func (m *Mod) retractWar(ctx context.Context, owner, warID int64) error {
	h := modapi.Corp().RetractWar
	if h == nil {
		return errors.New("the server has no retract hook")
	}
	if err := h(owner, warID); err != nil {
		return err
	}
	m.refreshRelations(ctx)
	return nil
}

// ── new wars ────────────────────────────────────────────────────────────────

// warCand is one possible enemy.
type warCand struct {
	Facts    CorpFacts
	Owner    int64 // what a war is declared on: the alliance when it has one
	Party    ceoRelParty
	In       warMotiveIn
	Motive   float64
	Kind     string
	Terms    []ceoRelTerm
	Strength float64
}

// ceoWarCandidates scores every other live player corporation.
func (m *Mod) ceoWarCandidates(ctx context.Context, c *corpCEO, ch Charter) (map[int64]*warCand, error) {
	selfFacts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return nil, err
	}
	corps, err := m.playerCorpFactsShared(ctx)
	if err != nil {
		return nil, err
	}
	var ids []int64
	var targets []CorpFacts
	for _, f := range corps {
		if f.CorpID == c.CorpID || f.Members == 0 {
			continue
		}
		if selfFacts.AllianceID != 0 && f.AllianceID == selfFacts.AllianceID {
			continue
		}
		targets = append(targets, f)
		ids = append(ids, f.CorpID)
	}
	peoples, err := m.ceoRelPeoples(ctx, append(ids, c.CorpID))
	if err != nil {
		return nil, err
	}
	// What each target flies, for soft: a failed read leaves the map empty and
	// falls back to the tags alone.
	hulls, _ := m.corpArmedHulls(ctx, ids, false)
	self := ceoRelPartyOf(*selfFacts, peoples[c.CorpID])
	// Grievance is the ledger's now (corprelations_hooks.go): what they have
	// done to us in the window, in kill units, violence only.
	griev := m.relRecentHostility(ctx, c.CorpID, ceoRelWindowDays*86400)
	ganks := m.relRecentGanks(ctx, c.CorpID, ceoRelWindowDays*86400)
	live, err := m.ceoRelLive(ctx, c.CorpID)
	if err != nil {
		return nil, err
	}
	rel, err := m.relRowsOf(ctx, c.CorpID)
	if err != nil {
		return nil, err
	}
	aggressors := m.allyAggressors(ctx, c.CorpID)
	now := clock.Unix()

	out := map[int64]*warCand{}
	for _, f := range targets {
		them := ceoRelPartyOf(f, peoples[f.CorpID])
		// A declared enemy counts once its tier has stood the dwell; a human director's
		// declaration (no ledger row, or a yielded one) counts at once.
		held := true
		if r, ok := rel[f.CorpID]; ok && !r.Yielded {
			held = r.Tier != 0 && now-r.TierAt >= ch.DwellSec
		}
		owner := f.CorpID
		if f.AllianceID != 0 {
			owner = f.AllianceID
		}
		competes := false
		if self.Region != "" && self.Region == them.Region {
			for _, t := range ceoRelCompeteTags {
				if self.has(t) && them.has(t) {
					competes = true
					break
				}
			}
		}
		soft := them.hasAny(ceoRelSoftTags) && !them.hasAny(ceoRelArmedTags) &&
			!hullsArmed(hulls[f.CorpID], f.Members)
		in := warMotiveIn{
			Standing:      live[f.CorpID],
			StandingHeld:  held,
			Grievance:     griev[f.CorpID],
			Ganked:        ganks[f.CorpID],
			SelfOutlaw:    self.outlaw(),
			TargetSoft:    soft,
			TargetOutlaw:  them.outlaw(),
			TargetHighsec: f.SystemID != 0 && m.systemSecurity(ctx, f.SystemID) >= warHighsecLine,
			Competes:      competes,
			AllyAggressor: aggressors[f.CorpID] || (f.AllianceID != 0 && aggressors[f.AllianceID]),
		}
		score, kind, terms := ceoWarMotive(in, ch)
		out[f.CorpID] = &warCand{Facts: f, Owner: owner, Party: them, In: in,
			Motive: score, Kind: kind, Terms: terms}
	}
	return out, nil
}

// allyAggressors is every owner that has declared a live war on one of this
// corporation's mutual allies.
func (m *Mod) allyAggressors(ctx context.Context, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	allies := relAlliesOf(corpID)
	if len(allies) == 0 {
		return out
	}
	set := map[int64]bool{}
	for _, a := range allies {
		set[a] = true
		if s := loadRelSnapshot(); s != nil && s.ally[a] != 0 {
			set[s.ally[a]] = true
		}
	}
	wars, err := evedb.ListWars(ctx, m.db)
	if err != nil {
		return out
	}
	for _, w := range wars {
		if w.Active && !w.TimeFinished.Valid && !w.Retracted.Valid && set[w.AgainstID] {
			out[w.DeclaredByID] = true
		}
	}
	return out
}

// ceoWarConsider takes at most one new step per dwell: a war declared, an
// offensive job posted, or a defensive job posted for a war it is losing.
func (m *Mod) ceoWarConsider(ctx context.Context, c *corpCEO, ch Charter) error {
	now := clock.Unix()
	for _, verb := range []string{"declare", "tender"} {
		if now-m.lastCEOActionAt(ctx, c.CorpID, verb) < ch.DwellSec {
			return nil
		}
	}
	selfFacts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	combat := ceoCombatOriented(ch)
	since := now - ceoRelWindowDays*86400
	minKill := m.minKillSince(ctx, since)
	// Ours is the wing it could send, by hulls; theirs (below) is every armed hull
	// they have. warStrength alone credits a combat tag with 80% of its members.
	ours := m.corpFightStrength(ctx, c.CorpID, selfFacts.Members, true, combat,
		m.corpKillEfficiency(ctx, c.CorpID, minKill, since))

	// Defence first: a war on this corporation that it cannot answer itself.
	if done, err := m.ceoWarDefend(ctx, c, ch, ours, combat, minKill, since); err != nil || done {
		return err
	}

	book, err := m.ceoWarBook(ctx, c.CorpID)
	if err != nil {
		return err
	}
	running := 0
	for _, b := range book {
		if b.ContractID == 0 {
			running++
		}
	}
	running += m.mercOpenAsClient(ctx, c.CorpID)
	if running >= ch.MaxWars {
		return nil
	}

	cands, err := m.ceoWarCandidates(ctx, c, ch)
	if err != nil {
		return err
	}
	busy := m.warBusyOwners(ctx, c.CorpID)
	var list []*warCand
	for _, x := range cands {
		if x.Motive >= ch.WarAt && !busy[x.Owner] && !busy[x.Facts.CorpID] {
			list = append(list, x)
		}
	}
	if len(list) == 0 {
		return nil
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Motive != list[j].Motive {
			return list[i].Motive > list[j].Motive
		}
		return list[i].Facts.CorpID < list[j].Facts.CorpID
	})
	x := list[0]
	x.Strength = m.corpFightStrength(ctx, x.Facts.CorpID, x.Facts.Members, false,
		x.Party.hasAny(ceoRelArmedTags), m.corpKillEfficiency(ctx, x.Facts.CorpID, minKill, since))
	why := ceoRelReason(x.Terms, 1)

	// The Director of Security's advice (corpstaff_dept.go): a violent head
	// talks the CEO into worse odds, a pacifist out of them.
	advised := ch
	advised.WarMaxOdds *= m.securityOddsMul(ctx, c)
	// A gank tender may go out with the market shut (rolebots.war.gankHires).
	switch ceoWarMode(x.Kind, ours, x.Strength, combat, mercOpenFor(x.Kind), advised) {
	case warModeFight:
		return m.ceoDeclareWar(ctx, c, ch, x, why)
	case warModeHire:
		return m.mercPostJob(ctx, c, ch, mercJobSpec{
			Kind: mercKindOffense, TargetOwner: x.Owner, TargetCorp: x.Facts.CorpID,
			Motive: x.Kind, MotiveScore: x.Motive, Why: why,
		})
	}
	return nil
}

// warBusyOwners is every owner this corporation already has business with: a
// live war either way, or an open merc job or contract against it.
func (m *Mod) warBusyOwners(ctx context.Context, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	var ally int64
	if s := loadRelSnapshot(); s != nil {
		ally = s.ally[corpID]
	}
	if wars, err := evedb.ListWars(ctx, m.db); err == nil {
		for _, w := range wars {
			if w.TimeFinished.Valid {
				continue
			}
			switch {
			case w.DeclaredByID == corpID || (ally != 0 && w.DeclaredByID == ally):
				out[w.AgainstID] = true
			case w.AgainstID == corpID || (ally != 0 && w.AgainstID == ally):
				out[w.DeclaredByID] = true
			}
		}
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT targetOwnerID, targetCorpID FROM rolebot_merc_jobs
		 WHERE clientCorpID = ? AND status IN ('open', 'haggling')
		UNION
		SELECT targetOwnerID, 0 FROM rolebot_merc_contracts
		 WHERE clientCorpID = ? AND status = 'active'`, corpID, corpID)
	if err == nil {
		for rows.Next() {
			var a, b int64
			if rows.Scan(&a, &b) == nil {
				out[a], out[b] = true, true
			}
		}
		rows.Close()
	}
	delete(out, 0)
	return out
}

// ceoDeclareWar declares a war the corporation will fight itself, only when the
// fee plus the runway of bills fits in the war chest.
func (m *Mod) ceoDeclareWar(ctx context.Context, c *corpCEO, ch Charter, x *warCand, why string) error {
	hook := modapi.Corp().DeclareWar
	if hook == nil {
		return nil
	}
	// An allied corporation cannot declare as itself (the core refuses it):
	// its grievance goes to the executor as a request (alliancewar.go).
	if aid := m.corpAllianceID(ctx, c.CorpID); aid != 0 {
		return m.fileAllianceWarRequest(ctx, c, aid, x, why)
	}
	fee := evedb.WarDeclarationCost(ctx, m.db)
	runway := ch.WarRunwayWeeks * evedb.WarWeeklyBillCost(ctx, m.db)
	chest, _, err := m.warChest(ctx, c.CorpID, ch)
	if err != nil {
		return err
	}
	if fee+runway > chest {
		return nil // cannot afford it without betting the corporation
	}
	if n, err := evedb.CountActiveDeclaredWars(ctx, m.db, c.CorpID); err != nil ||
		n >= evedb.WarMaxDeclarations(ctx, m.db) {
		return err
	}
	warID, err := hook(c.CorpID, c.CorpID, x.Owner)
	if err != nil {
		m.logBot(c.CharID, logInfo, "ceo", "war: declare on %s refused: %v", x.Facts.Name, err)
		return nil
	}
	if err := m.ceoWarBookAdd(ctx, warID, c.CorpID, x.Owner, x.Kind, 0, why); err != nil {
		return err
	}
	if err := m.openCommit(ctx, c.CorpID, "war", warID, runway); err != nil {
		return err
	}
	m.refreshRelations(ctx)
	m.ceoLog(ctx, c, powerWar, "declare", x.Owner, fee,
		"declared war on %s (%s) — %s; %s set aside for %.0f weeks of bills",
		ownerNameOf(ctx, m.db, x.Owner), x.Kind, why, iskString(runway), ch.WarRunwayWeeks)
	return nil
}

// ceoWarDefend posts a defensive job for the strongest war on this
// corporation that it cannot answer itself. Reports whether it acted.
func (m *Mod) ceoWarDefend(ctx context.Context, c *corpCEO, ch Charter, ours float64, combat bool, minKill, since int64) (bool, error) {
	if !mercMarketOn() {
		return false, nil
	}
	wars, err := evedb.ListWars(ctx, m.db)
	if err != nil {
		return false, nil
	}
	var ally int64
	if s := loadRelSnapshot(); s != nil {
		ally = s.ally[c.CorpID]
	}
	busy := m.warBusyJobs(ctx, c.CorpID)
	for _, w := range wars {
		if w.TimeFinished.Valid || w.Retracted.Valid || w.Mutual != 0 {
			continue
		}
		if w.AgainstID != c.CorpID && (ally == 0 || w.AgainstID != ally) {
			continue
		}
		if busy[w.WarID] {
			continue
		}
		aggCorp := w.DeclaredByID
		if exec := m.allianceExecutor(ctx, w.DeclaredByID); exec != 0 {
			aggCorp = exec
		}
		f, err := getAnyCorpFacts(ctx, m.db, aggCorp)
		if err != nil || f == nil {
			continue
		}
		theirs := m.corpFightStrength(ctx, aggCorp, f.Members, false, policyArmed(f.Policy),
			m.corpKillEfficiency(ctx, aggCorp, minKill, since))
		if combat && ours > 0 && theirs <= ours*ch.WarMaxOdds {
			continue // we can answer this one ourselves
		}
		return true, m.mercPostJob(ctx, c, ch, mercJobSpec{
			Kind: mercKindDefense, TargetOwner: w.DeclaredByID, TargetCorp: aggCorp, WarID: w.WarID,
			Motive: warMotiveDefence, MotiveScore: 2 * ch.WarAt,
			Why: fmt.Sprintf("%s declared war on us", ownerNameOf(ctx, m.db, w.DeclaredByID)),
		})
	}
	return false, nil
}

// warBusyJobs is every war this corporation already has a defensive job or
// contract running against.
func (m *Mod) warBusyJobs(ctx context.Context, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT warID FROM rolebot_merc_jobs
		 WHERE clientCorpID = ? AND kind = 'defense' AND status IN ('open', 'haggling', 'awarded')`, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// allianceExecutor is an alliance's executor corporation, 0 for anything that
// is not an alliance.
func (m *Mod) allianceExecutor(ctx context.Context, ownerID int64) int64 {
	var exec int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT executorCorpID FROM alliances WHERE allianceID = ?`, ownerID).Scan(&exec)
	return exec
}

// policyArmed reports whether a corporation wears an armed tag.
func policyArmed(p CorpPolicy) bool {
	for _, t := range ceoRelArmedTags {
		if p.HasTag(t) {
			return true
		}
	}
	return false
}

// ownerNameOf names a corporation or an alliance.
func ownerNameOf(ctx context.Context, db *sql.DB, ownerID int64) string {
	var name string
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(
			(SELECT corporationName FROM corporation WHERE corporationID = ?),
			(SELECT allianceName FROM alliances WHERE allianceID = ?), '')`,
		ownerID, ownerID).Scan(&name); err != nil || name == "" {
		return fmt.Sprintf("#%d", ownerID)
	}
	return name
}

// ── war footing (the directive's side) ──────────────────────────────────────

// ceoWarFooting is how far this corporation's directive leans toward war this
// think, and which way: fight (a fighting outfit, or a merc on contract) or
// hunker down (anyone else). Zero when it is at no war.
func (m *Mod) ceoWarFooting(ctx context.Context, c *corpCEO, ch Charter) (footing float64, fight bool, why string) {
	contracts := m.mercActiveAsMerc(ctx, c.CorpID)
	enemies := relWarEnemiesOf(c.CorpID)
	if len(enemies) == 0 && contracts == 0 {
		return 0, false, ""
	}
	fight = ceoCombatOriented(ch)
	if contracts > 0 {
		return ch.WarFootingMax, true, fmt.Sprintf("%d mercenary contract(s) to fight", contracts)
	}
	var ours, theirs float64
	var members int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM character_ WHERE corporationID = ? AND accountID > 0`,
		c.CorpID).Scan(&members)
	ours = warStrength(members, fight, -1)
	var enemyMembers int
	if len(enemies) > 0 {
		_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM character_ WHERE accountID > 0 AND corporationID IN (`+
			placeholders(len(enemies))+`)`, int64Args(enemies)...).Scan(&enemyMembers)
	}
	theirs = warStrength(enemyMembers, true, -1)
	return warFootingSize(ch, ours, theirs), fight, fmt.Sprintf("at war with %d corporation(s)", len(enemies))
}

// warHoldMul is what exposed work is worth to a pilot whose corporation's war
// enemy is in its current system.
const warHoldMul = 0.35

// warHoldTerm is the abstract tier's undock hold: a pilot of a corporation at
// war, in a system a war enemy works or lives in, prefers work done from a
// station. A multiplier on the draw, never a refusal.
func warHoldTerm(corpID, sysID int64, slug string) float64 {
	if slug != ActMining && slug != ActCourier {
		return 1
	}
	if !relWarEnemySystem(corpID, sysID) {
		return 1
	}
	return warHoldMul
}
