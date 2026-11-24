package rolebots

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

// The corporate portal key: how a pilot proves, from the in-game browser, that the terminal
// is theirs. The browser's identity headers can be forged (esp/igbmodule.go), so a page that
// acts for a pilot needs a secret only that pilot holds.
// It twins the SCC brokerage key (db/sccaccount.go): the corporation mails the key to the
// pilot's own inbox, the pilot pastes it into the Management page, and the module keeps a hash,
// compares in constant time and locks the key after five wrong tries.
// The key proves the terminal, not the rank: every order re-reads the pilot's corporation
// and roles (isCorpOfficer), and a key stops working when its holder leaves the corporation
// it was issued in.

const (
	portalKeyAlphabet       = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I/O/0/1: read off a mail by eye
	portalKeyMaxFails       = 5
	portalKeyGroupSize      = 4
	portalKeyGroups         = 3
	settingPortalKeyLockSec = "rolebots.corp.portalKeyLockSec"
	defaultPortalKeyLockSec = 15 * 60
)

var portalKeyLockSec atomic.Int64

func (m *Mod) loadPortalKeySettings(ctx context.Context) {
	portalKeyLockSec.Store(defaultPortalKeyLockSec)
	if v, ok, err := getServerSettingInt(ctx, m, settingPortalKeyLockSec); err == nil && ok && v > 0 {
		portalKeyLockSec.Store(v)
	}
}

var (
	errPortalNoKey     = errors.New("you have no portal key yet — request one and it is mailed to you")
	errPortalBadKey    = errors.New("that key is not right")
	errPortalKeyLocked = errors.New("too many wrong keys: your key is locked for a while")
	errPortalKeyCorp   = errors.New("that key was issued in another corporation — request a new one")
)

// newPortalKey draws CORP-XXXX-XXXX-XXXX.
func newPortalKey() (string, error) {
	buf := make([]byte, portalKeyGroups*portalKeyGroupSize)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("CORP")
	for i, c := range buf {
		if i%portalKeyGroupSize == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(portalKeyAlphabet[int(c)%len(portalKeyAlphabet)])
	}
	return b.String(), nil
}

// normalizePortalKey forgives case, spaces, dashes and a missing prefix.
func normalizePortalKey(k string) string {
	k = strings.ToUpper(strings.Join(strings.Fields(k), ""))
	k = strings.ReplaceAll(k, "-", "")
	return strings.TrimPrefix(k, "CORP")
}

func hashPortalKey(k string) string {
	sum := sha256.Sum256([]byte("rolebot-portal:" + normalizePortalKey(k)))
	return hex.EncodeToString(sum[:])
}

// issuePortalKey issues (or re-keys) a pilot's portal key in their current corporation and
// returns it in clear, the only time it exists in clear. The caller mails it.
func (m *Mod) issuePortalKey(ctx context.Context, charID, corpID int64) (string, error) {
	if charID <= 0 || corpID <= 0 {
		return "", fmt.Errorf("no pilot")
	}
	key, err := newPortalKey()
	if err != nil {
		return "", err
	}
	_, err = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_portal_keys (characterID, corporationID, keyHash, issuedAt, failCount, failAt)
		VALUES (?, ?, ?, ?, 0, 0)
		ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), keyHash = VALUES(keyHash),
			issuedAt = VALUES(issuedAt), failCount = 0, failAt = 0`,
		charID, corpID, hashPortalKey(key), clock.Unix())
	if err != nil {
		return "", fmt.Errorf("issuePortalKey: %w", err)
	}
	return key, nil
}

// hasPortalKey reports whether a pilot holds a key in this corporation.
func (m *Mod) hasPortalKey(ctx context.Context, charID, corpID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_portal_keys WHERE characterID = ? AND corporationID = ?`, charID, corpID).Scan(&n)
	return n > 0
}

// verifyPortalKey checks a key against the pilot's record in this
// corporation. The sentences it returns are for the page.
func (m *Mod) verifyPortalKey(ctx context.Context, charID, corpID int64, key string) error {
	var hash string
	var issuedCorp int64
	var fails int
	var failAt int64
	err := m.db.QueryRowContext(ctx,
		`SELECT keyHash, corporationID, failCount, failAt FROM rolebot_portal_keys WHERE characterID = ?`, charID).
		Scan(&hash, &issuedCorp, &fails, &failAt)
	if errors.Is(err, sql.ErrNoRows) {
		return errPortalNoKey
	}
	if err != nil {
		return err
	}
	now := clock.Unix()
	lock := portalKeyLockSec.Load()
	if fails >= portalKeyMaxFails && now-failAt < lock {
		return errPortalKeyLocked
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashPortalKey(key))) == 1 && normalizePortalKey(key) != "" {
		if issuedCorp != corpID {
			return errPortalKeyCorp
		}
		_, _ = m.db.ExecContext(ctx,
			`UPDATE rolebot_portal_keys SET lastUsedAt = ?, failCount = 0 WHERE characterID = ?`, now, charID)
		return nil
	}
	if now-failAt >= lock {
		fails = 0
	}
	_, _ = m.db.ExecContext(ctx,
		`UPDATE rolebot_portal_keys SET failCount = ?, failAt = ? WHERE characterID = ?`, fails+1, now, charID)
	return errPortalBadKey
}

// mailPortalKey issues a key and mails it from the corporation. Returns the
// sentence for the page.
func (m *Mod) mailPortalKey(ctx context.Context, charID, corpID int64) string {
	key, err := m.issuePortalKey(ctx, charID, corpID)
	if err != nil {
		rlogf("rolebots: portal key for %d: %v", charID, err)
		return "The key could not be issued. Try again shortly."
	}
	corpName := corpNameOf(ctx, m.db, corpID)
	body := "Your corporate portal key for " + corpName + " is<br><br><b>" + key + "</b><br><br>" +
		"Paste it into the " + igbLink(managePath, "Management page") + " of your Corporate Portal to open your desk. " +
		"Anyone holding this key can give orders in your name, so keep the letter to yourself; " +
		"requesting a new key retires this one. The key is tied to this corporation and lapses if you leave it."
	if m.mailPilot(ctx, charID, corpID, "Your corporate portal key", body) == 0 {
		return "The key was issued but the letter could not be delivered."
	}
	return "Your key is on its way to your inbox."
}

// getServerSettingInt is a whole-number server setting.
func getServerSettingInt(ctx context.Context, m *Mod, key string) (int64, bool, error) {
	v, ok, err := evedb.GetServerSetting(ctx, m.db, key)
	if err != nil || !ok {
		return 0, ok, err
	}
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err != nil {
		return 0, false, nil
	}
	return n, true, nil
}
