package rolebots

// A gank reaching the politics.
//
// A suicide gank weighs more than the kill it also is (incGank, relBaseWeight)
// and scores its own war term outside the grievance cap (ceoWarMotive). It is the
// one motive for which a mercenary tender may open while the market is shut
// (rolebots.war.gankHires).
//
// The killmail cannot tell a gank, so both tiers log a called strike in
// rolebot_gank_log, and a kill of that mark within gankLogWindowSec is a gank. An
// NPC ganker's incident is filed pre-applied: shown, never folded.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

const (
	// gankLogWindowSec is how close to a logged call a kill must land to count as
	// that gank; a mark that died later died of something else.
	gankLogWindowSec = 120
	// gankIncidentDayCap is the most gank incidents one corporation files against
	// one ganker corporation per day. Past it a camp files ordinary kills, so the
	// ledger cannot be run to -10 in an afternoon.
	gankIncidentDayCap = 5
	// gankWarTermPerGank is what one gank is worth toward a war before the charter's
	// grievance weight; gankWarTermCap is Charter.WarAt's top (9), so three ganks
	// reach any CEO's line.
	gankWarTermPerGank = 3.0
	gankWarTermCap     = 9.0
)

// gankLogSchema is the call log both tiers write.
const gankLogSchema = `CREATE TABLE IF NOT EXISTS rolebot_gank_log (
	logID        BIGINT NOT NULL AUTO_INCREMENT,
	gangID       BIGINT NOT NULL,
	victimCharID BIGINT NOT NULL,
	systemID     BIGINT NOT NULL DEFAULT 0,
	at           BIGINT NOT NULL,
	isk          DOUBLE NOT NULL DEFAULT 0,
	PRIMARY KEY (logID),
	KEY idx_rbgank_victim (victimCharID, at),
	KEY idx_rbgank_at (at)
)`

// gankLogRetentionSec: a call row is kept a week; the ledger has its own copy
// of anything that mattered by then.
const gankLogRetentionSec = 7 * 86400

// noteGankCalled records a strike called on a mark, from either tier. Never
// fails its caller.
func (m *Mod) noteGankCalled(ctx context.Context, gangID, victimCharID, systemID int64, isk float64) {
	if m.db == nil || victimCharID == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.Background(), relHookTimeout)
	defer cancel()
	if _, err := m.db.ExecContext(wctx, `
		INSERT INTO rolebot_gank_log (gangID, victimCharID, systemID, at, isk) VALUES (?, ?, ?, ?, ?)`,
		gangID, victimCharID, systemID, clock.Unix(), isk); err != nil {
		rlogf("rolebots: gank log %d → %d: %v", gangID, victimCharID, err)
	}
}

// gankLogged reports whether a kill of this pilot at this time was a called
// gank: a log row inside the window either side of the kill.
func (m *Mod) gankLogged(ctx context.Context, victimCharID, killUnix int64) bool {
	if m.db == nil || victimCharID == 0 || killUnix == 0 {
		return false
	}
	var one int64
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM rolebot_gank_log
		 WHERE victimCharID = ? AND at BETWEEN ? AND ? LIMIT 1`,
		victimCharID, killUnix-gankLogWindowSec, killUnix+gankLogWindowSec).Scan(&one)
	return err == nil
}

// isGankKill is gankLogged for a killmail: the module's own record carries the
// victim and the time.
func (m *Mod) isGankKill(ctx context.Context, victimCharID int64, at time.Time) bool {
	return m.gankLogged(ctx, victimCharID, at.Unix())
}

// gankKindFor is the incident kind a kill files under: a gank when the log
// says so and the victim's corporation is under its daily cap against this
// ganker, else the ordinary kill (never a pod: a gank is the ship).
func (m *Mod) gankKindFor(ctx context.Context, victimCorp, gankerCorp int64, isPod bool) string {
	if isPod {
		return incPod
	}
	if m.gankIncidentsToday(ctx, victimCorp, gankerCorp) >= gankIncidentDayCap {
		return incKill
	}
	return incGank
}

// gankIncidentsToday counts the gank incidents already on victimCorp's ledger
// against gankerCorp since the start of the UTC day.
func (m *Mod) gankIncidentsToday(ctx context.Context, victimCorp, gankerCorp int64) int {
	if m.db == nil {
		return 0
	}
	day := clock.Unix() / 86400 * 86400
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_incidents
		 WHERE corporationID = ? AND toID = ? AND kind = ? AND side = ? AND at >= ?`,
		victimCorp, gankerCorp, incGank, relSideVictim, day).Scan(&n)
	return n
}

// noteGankIncidentNPC files a gank by an NPC-corporation ganker on the
// victim's ledger, pre-applied: visible on the Relations page and in the
// ledger's own counts, never folded toward a corporation nothing can stand
// against. Deduped on the kill like every other kill row.
func (m *Mod) noteGankIncidentNPC(ctx context.Context, victimCorp, gankerCorp int64, isk float64,
	systemID, killID int64, detail string) {

	if m.db == nil || !relLedgerOn() || !relationsEnabled() || victimCorp == 0 || gankerCorp == 0 {
		return
	}
	if _, managed := managedCEO(victimCorp); !managed {
		return
	}
	w := relBaseWeight(incGank, relSideVictim, isk, relParamsNow())
	if w == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.Background(), relHookTimeout)
	defer cancel()
	now := clock.Unix()
	if _, err := m.db.ExecContext(wctx, `
		INSERT IGNORE INTO rolebot_corp_incidents
			(corporationID, toID, kind, side, weight, isk, systemID, subjectID, dedupeKey, at, appliedAt, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		victimCorp, gankerCorp, incGank, relSideVictim, w, isk, systemID, killID,
		sql.NullString{String: fmt.Sprintf("kill:%d", killID), Valid: true}, now, now,
		truncate(detail, 255)); err != nil {
		rlogf("rolebots: relations gank incident corp %d → NPC %d: %v", victimCorp, gankerCorp, err)
	}
}

// relRecentGanks counts how many times each counterparty has ganked this
// corporation inside the window. It is a count, not a weight; the war term prices
// a gank.
func (m *Mod) relRecentGanks(ctx context.Context, corpID int64, sinceSec int64) map[int64]float64 {
	out := map[int64]float64{}
	if m.db == nil {
		return out
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT toID, COUNT(*) FROM rolebot_corp_incidents
		 WHERE corporationID = ? AND at >= ? AND kind = ? AND side = ?
		 GROUP BY toID`, corpID, clock.Unix()-sinceSec, incGank, relSideVictim)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n float64
		if rows.Scan(&id, &n) == nil {
			out[id] = n
		}
	}
	return out
}

// gankWarTerm is what being ganked is worth toward a war: per gank, through the
// charter's grievance weight but never under 1 (a mercenary CEO still answers a
// gank aimed at its business), capped at Charter.WarAt's top. Pure.
func gankWarTerm(ganked, grievanceWeight float64) float64 {
	if ganked <= 0 {
		return 0
	}
	return math.Min(ganked*gankWarTermPerGank*math.Max(grievanceWeight, 1), gankWarTermCap)
}

// mercOpenFor reports whether the mercenary market is open for a job of this
// motive: the market switch, or the gank-hires switch for a gank tender.
func mercOpenFor(motive string) bool {
	if mercMarketOn() {
		return true
	}
	return gankHiresOn() && motive == warMotiveGank
}

// mercContractOpen is mercOpenFor for a running contract, by its job's motive.
func (m *Mod) mercContractOpen(ctx context.Context, k mercContract) bool {
	if mercMarketOn() {
		return true
	}
	if !gankHiresOn() || k.JobID == 0 {
		return false
	}
	var motive string
	if err := m.db.QueryRowContext(ctx, `SELECT motive FROM rolebot_merc_jobs WHERE jobID = ?`, k.JobID).Scan(&motive); err != nil {
		return false
	}
	return motive == warMotiveGank
}

// sweepGankLog drops call rows older than the retention.
func (m *Mod) sweepGankLog(ctx context.Context) {
	if m.db == nil {
		return
	}
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_gank_log WHERE at < ?`, clock.Unix()-gankLogRetentionSec)
}
