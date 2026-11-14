package rolebots

// Mail suppression for synthetic pilots.
//
// A rolebot never opens an inbox, but the server mails it like a person: war
// notices, bills to CEOs, corporation applications. On a large roster that is
// thousands of evemail rows a week. The core exposes db.MailboxSuppressed, a
// nil-by-default hook on the funnel every mail insert uses; installed here, a
// mail addressed to a bot is never written. Killmails are their own rows
// (db/killboarddb.go), so the killboard is unaffected.

import (
	"context"
	"database/sql"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// The roster changes rarely and this sits on every mail the server sends, so it
// is cached. The cache stays authoritative when the query fails: serving "not a
// bot" for the whole roster would flood bot inboxes.
const botMailboxTTL = 60 * time.Second

var (
	botMailboxMu     sync.RWMutex
	botMailboxSet    map[int64]struct{}
	botMailboxAt     time.Time
	botMailboxEverOK bool
)

// installMailSuppression points the core's mail gate at this roster. Called once
// from Init. There is no uninstall: the roster a stopped module leaves behind is
// still synthetic.
func (m *Mod) installMailSuppression() {
	// The corporation desk's link signer (corpdesk.go) reads its secret through the
	// module; wired here because this runs at every Init.
	deskMod = m
	evedb.MailboxSuppressed = m.isSynthetic
	// The R&D patent lottery (db/rndlottery.go) scales a synthetic pilot's tickets.
	// It shares the mail gate's predicate and cache, so one invalidation clears both.
	evedb.SyntheticPilot = m.isSynthetic
}

// isBotMailbox is the mail gate's name for isSynthetic.
func (m *Mod) isBotMailbox(charID int64) bool { return m.isSynthetic(charID) }

// isSynthetic answers "is this pilot a rolebot rather than a person"; every
// bot-vs-human decision comes here. A pilot is synthetic with a rolebot_characters
// row (enabled or not) or on a generated RoleBot<N> account, which catches an
// orphan whose roster insert failed (botgen.go). accountID > 0 is not a bot test.
func (m *Mod) isSynthetic(charID int64) bool {
	if charID == 0 || m.db == nil {
		return false
	}

	botMailboxMu.RLock()
	fresh := botMailboxEverOK && clock.Now().Sub(botMailboxAt) < botMailboxTTL
	if fresh {
		_, ok := botMailboxSet[charID]
		botMailboxMu.RUnlock()
		return ok
	}
	botMailboxMu.RUnlock()

	botMailboxMu.Lock()
	defer botMailboxMu.Unlock()
	// Another goroutine may have refreshed the cache while this one waited for the
	// write lock.
	if botMailboxEverOK && clock.Now().Sub(botMailboxAt) < botMailboxTTL {
		_, ok := botMailboxSet[charID]
		return ok
	}

	set, err := loadBotMailboxes(m.db)
	if err != nil {
		rlogf("rolebots: mail suppression roster reload: %v", err)
		if botMailboxEverOK {
			// Keep serving the last known roster. botMailboxAt is not stamped, so the next
			// call retries instead of keeping a stale answer for a full TTL.
			_, ok := botMailboxSet[charID]
			return ok
		}
		return false
	}
	botMailboxSet, botMailboxAt, botMailboxEverOK = set, clock.Now(), true
	_, ok := set[charID]
	return ok
}

// invalidateBotMailboxes drops the cached roster so the next mail re-reads it.
// Called wherever the roster changes (assignment, removal, generation, purge), so
// a new bot gets no mail in its first minute.
func invalidateBotMailboxes() {
	botMailboxMu.Lock()
	botMailboxAt = time.Time{}
	botMailboxMu.Unlock()
}

func loadBotMailboxes(db *sql.DB) (map[int64]struct{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	set := make(map[int64]struct{}, 4096)
	if err := scanIDSet(ctx, db, set, `SELECT characterID FROM rolebot_characters`); err != nil {
		return nil, err
	}
	// The generated-account half is best effort: the roster is the authority, and a
	// schema without the account table (the harness scratch schema) must not fail it.
	if err := scanIDSet(ctx, db, set, `
		SELECT c.characterID
		  FROM character_ c
		  JOIN account a ON a.accountID = c.accountID
		 WHERE LOWER(a.accountName) REGEXP '^rolebot[0-9]+$'`); err != nil && !isMissingTable(err) {
		rlogf("rolebots: synthetic set, generated-account half: %v", err)
	}
	return set, nil
}

func scanIDSet(ctx context.Context, db *sql.DB, set map[int64]struct{}, q string) error {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		set[id] = struct{}{}
	}
	return rows.Err()
}

// logOrphanBots reports generated pilots that have no roster row: they are
// synthetic by isSynthetic's account half, but nothing drives them. One line
// at boot so the operator can re-roster or purge them.
func (m *Mod) logOrphanBots(ctx context.Context) {
	var n int64
	err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM character_ c
		  JOIN account a ON a.accountID = c.accountID
		  LEFT JOIN rolebot_characters b ON b.characterID = c.characterID
		 WHERE LOWER(a.accountName) REGEXP '^rolebot[0-9]+$' AND b.characterID IS NULL`).Scan(&n)
	if err != nil {
		if !isMissingTable(err) {
			rlogf("rolebots: orphan generated-pilot count: %v", err)
		}
		return
	}
	if n > 0 {
		rlogf("rolebots: %d generated pilots on RoleBot accounts have no roster row; treated as synthetic (re-roster or purge them)", n)
	}
}

// settingMailCleared marks the one-shot backlog clear as done, so it runs on
// the boot that first carries the gate and never again.
const settingMailCleared = "rolebots.mailCleared"

// clearMailBacklog deletes the mail that piled up in bot inboxes before the
// suppression gate. One-shot: the gate stops new mail, so a second pass would only
// cost a full scan of evemail at boot. Never fatal.
func (m *Mod) clearMailBacklog(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingMailCleared); err != nil {
		rlogf("rolebots: read mail-cleared marker: %v (skipping backlog clear)", err)
		return
	} else if ok && v == "1" {
		return
	}

	set, err := loadBotMailboxes(m.db)
	if err != nil {
		rlogf("rolebots: mail backlog roster read: %v", err)
		return
	}
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	if len(ids) > 0 {
		n, err := evedb.DeleteMailboxes(ctx, m.db, ids)
		if err != nil {
			rlogf("rolebots: clear mail backlog for %d pilots: %v", len(ids), err)
			return
		}
		if n > 0 {
			rlogf("rolebots: cleared %d backlog mail rows across %d pilots", n, len(ids))
		}
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingMailCleared, "1"); err != nil {
		rlogf("rolebots: mark mail backlog cleared: %v", err)
	}
}
