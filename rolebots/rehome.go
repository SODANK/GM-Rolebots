package rolebots

import (
	"context"
	"database/sql"
	"errors"
)

// The one-time address reconsider. Corp-home bookkeeping is in memory, so every
// boot re-looks each pilot on first sighting, spread over corpHomeWindowSec
// (corphome.go, planCorpAddresses). The first boot under new placement rules
// would relocate thousands of pilots at once, so those first looks spread over
// rehomeSpreadSec. The end of the window is stored, so a restart resumes it.

const (
	// rehomeSpreadMarker is the rolebot_traits marker row (characterID 0, the
	// one-shot migration convention, toil.go) whose value is the unix time the
	// spread ends.
	rehomeSpreadMarker = "_rehome1014"
	// rehomeSpreadSec is how long the first looks are spread over.
	rehomeSpreadSec = 6 * 3600
)

// corpHomeSpreadUntil, while in the future, widens the first-sighting window
// in corpHomeMove from corpHomeWindowSec to rehomeSpreadSec. Guarded by
// corpHomeMu, like the rest of the corp-home bookkeeping.
var corpHomeSpreadUntil int64

// startRehomeSpread arms the spread on the first boot, and re-arms what is left
// of it on a boot inside the window. Best-effort: a failure costs the spread,
// not the boot.
func (m *Mod) startRehomeSpread(ctx context.Context) {
	now := clock.Unix()
	var until int64
	err := m.db.QueryRowContext(ctx, `
		SELECT value FROM rolebot_traits WHERE characterID = 0 AND trait = ?`,
		rehomeSpreadMarker).Scan(&until)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		until = now + rehomeSpreadSec
		if _, err := m.db.ExecContext(ctx, `
			INSERT IGNORE INTO rolebot_traits (characterID, trait, value) VALUES (0, ?, ?)`,
			rehomeSpreadMarker, until); err != nil {
			rlogf("rolebots: rehome spread marker: %v", err)
			return
		}
		rlogf("rolebots: one-time address reconsider armed — first corp-home looks spread over %d h", rehomeSpreadSec/3600)
	case err != nil:
		rlogf("rolebots: rehome spread marker: %v", err)
		return
	case now >= until:
		return // done on an earlier boot
	}
	corpHomeMu.Lock()
	corpHomeSpreadUntil = until
	corpHomeMu.Unlock()
}

// corpHomeFirstLookAt is when a pilot seen for the first time looks: across
// the usual window, across the reconsider's wider one while it runs, or now
// during an operator's rush. Called under corpHomeMu.
func corpHomeFirstLookAt(charID, now int64) int64 {
	switch {
	case now < corpHomeRushUntil:
		return now
	case now < corpHomeSpreadUntil:
		return now + charID%rehomeSpreadSec
	}
	return now + charID%corpHomeWindowSec
}
