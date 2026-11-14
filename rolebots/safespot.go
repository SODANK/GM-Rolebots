package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/server/gangsta"
)

// Safe spots. A pilot warps between two celestials and bookmarks its position
// mid-warp, once clear of the bubble it left: a point nobody can warp to without a
// copy, for staging a fleet, escaping a gank or sitting out a log-off timer. The
// embodied maker (makeSafeOnWarp) flies the leg; the abstract maker
// (makeSafeAbstract) computes the same point from celestial positions. Both
// write one rolebot_safespots row; shareSafe hands copies to the fleet, pickSafe
// reads the newest live row and safeTarget turns it into a warp target. The
// sweep burns a safe when a kill lands within safeBurnRadiusM or it exceeds
// safeMaxAgeSec. Callers decide when to make or use one.

const (
	// safeClearMargin is how far past the bubble edge a safe must sit, so a
	// ship parked on it is on no grid a celestial's visitors can see.
	safeClearMargin = 100_000.0
	// safeClearM is the clearance safeOnLeg needs from the leg's origin and
	// from every celestial.
	safeClearM = destiny.BubbleRadius + safeClearMargin
	// safeBurnRadiusM: a kill this close to a safe means somebody found it.
	safeBurnRadiusM = 100_000.0
	// safeMaxAgeSec retires a safe after a week whatever happened to it.
	safeMaxAgeSec = 7 * 86400
	// safePruneAfterSec deletes a burned row this long after it burned.
	safePruneAfterSec = 30 * 86400
	// safeSweepTicks is the sweep cadence (ticks are one second).
	safeSweepTicks = 300
	// safeTargetBand offsets a safe's synthetic ItemID so it cannot collide
	// with the negative dungeon-object IDs siteembodied.go uses.
	safeTargetBand = int64(1) << 40
	// safeWatchStep is how often the embodied maker checks the warp.
	safeWatchStep = 250 * time.Millisecond
	// safeWatchSlackSec is how long past the modelled warp the watcher waits
	// before it gives up on a warp that never landed.
	safeWatchSlackSec = 30.0
	// safeBookmarkTypeID is "Solar System", the typeID the core gives every
	// coordinate bookmark (bookmarksvc.go).
	safeBookmarkTypeID = 5
)

// Safe purposes, stored in rolebot_safespots.purpose.
const (
	safeStage  = "stage"
	safeEscape = "escape"
	safeLogoff = "logoff"
)

// dialSafeShareVouchers hands a human fleet member a bookmark voucher in cargo
// for each safe the fleet makes. Bot mates always get a bookmark copy.
var dialSafeShareVouchers = &pvpDial{key: "rolebots.safes.shareVouchers", def: 1, lo: 0, hi: 1, step: 1,
	label: "Give human fleet members a bookmark voucher for each fleet safe (1 = on)"}

// safeDials is every dial in this file, appended to pvpDials() for the loader.
func safeDials() []*pvpDial { return []*pvpDial{dialSafeShareVouchers} }

func safeVouchersOn() bool { return dialSafeShareVouchers.Get() >= 0.5 }

// safeSpot is one rolebot_safespots row. Memo is not stored in the row; it is
// the maker's bookmark caption, filled when the row was just made or read
// back by safeMemo.
type safeSpot struct {
	SafeID        int64
	CorpID        int64
	SystemID      int64
	X, Y, Z       float64
	BookmarkID    int64
	NearStationID int64
	Purpose       string
	CreatedBy     int64
	CreatedAt     int64
	UsedAt        int64
	BurnedAt      int64
	Memo          string
}

func (s safeSpot) pos() destiny.Vec3 { return destiny.Vec3{X: s.X, Y: s.Y, Z: s.Z} }

func validSafePurpose(p string) bool {
	return p == safeStage || p == safeEscape || p == safeLogoff
}

// ── the geometry ────────────────────────────────────────────────────────────

// safeOnLeg returns the first point on the line from to to that is at least
// clearM from the origin, every celestial and the destination; false when the leg
// is too short or celestials cover it. Pure. Each celestial blocks the stretch
// within clearM of it, and the walk steps past every blocked stretch.
func safeOnLeg(from, to destiny.Vec3, celestials []destiny.Vec3, clearM float64) (destiny.Vec3, bool) {
	leg := to.Sub(from)
	length := leg.Len()
	if length == 0 || clearM <= 0 {
		return destiny.Vec3{}, false
	}
	dir := leg.Scale(1 / length)

	type span struct{ lo, hi float64 }
	spans := make([]span, 0, len(celestials)+1)
	block := func(c destiny.Vec3) {
		rel := c.Sub(from)
		along := rel.Dot(dir)
		perpSq := rel.LenSq() - along*along
		if perpSq >= clearM*clearM {
			return
		}
		if perpSq < 0 {
			perpSq = 0
		}
		half := math.Sqrt(clearM*clearM - perpSq)
		spans = append(spans, span{along - half, along + half})
	}
	for _, c := range celestials {
		block(c)
	}
	block(to)
	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })

	// One metre past a span's edge, so rounding cannot leave the point a
	// hair inside clearM.
	const edge = 1.0
	t := clearM
	for moved := true; moved; {
		moved = false
		for _, s := range spans {
			if s.lo < t && t < s.hi {
				t = s.hi + edge
				moved = true
			}
		}
	}
	if t > length {
		return destiny.Vec3{}, false
	}
	return from.Add(dir.Scale(t)), true
}

// safeLeg is one candidate leg for the abstract maker: warp from FromID to
// the destination and the safe lands at Point.
type safeLeg struct {
	FromID int64
	Point  destiny.Vec3
}

// safeLegs lists every planet-to-toID leg that has a safe on it, longest leg
// first. Pure.
func safeLegs(cels []evedb.CelestialEntity, toID int64, clearM float64) []safeLeg {
	var to *evedb.CelestialEntity
	pts := make([]destiny.Vec3, 0, len(cels))
	for i := range cels {
		c := &cels[i]
		if c.ItemID == toID {
			to = c
		}
		pts = append(pts, destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z})
	}
	if to == nil {
		return nil
	}
	toPos := destiny.Vec3{X: to.X, Y: to.Y, Z: to.Z}
	type cand struct {
		leg safeLeg
		len float64
	}
	var cs []cand
	for _, c := range cels {
		if c.GroupID != 7 || c.ItemID == toID { // planets only
			continue
		}
		from := destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}
		if p, ok := safeOnLeg(from, toPos, pts, clearM); ok {
			cs = append(cs, cand{safeLeg{c.ItemID, p}, toPos.Sub(from).Len()})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].len > cs[j].len })
	out := make([]safeLeg, len(cs))
	for i, c := range cs {
		out[i] = c.leg
	}
	return out
}

// chooseSafeLeg picks the first leg whose point is at least clearM from every
// point in avoid, so a second safe never shares a grid with the first. Pure.
func chooseSafeLeg(legs []safeLeg, avoid []destiny.Vec3, clearM float64) (safeLeg, bool) {
	for _, l := range legs {
		ok := true
		for _, a := range avoid {
			if l.Point.Sub(a).LenSq() < clearM*clearM {
				ok = false
				break
			}
		}
		if ok {
			return l, true
		}
	}
	return safeLeg{}, false
}

// warpReachSec is the seconds into the tunnel at which a warp of legLen metres
// has covered d metres, on the dll's curve: speed 3 m/s rising at e^(3t) to
// vMax (Manager.WarpPhases), then cruise. The deceleration tail is ignored, so
// a point inside it reads a little early. Pure.
func warpReachSec(d, legLen float64) float64 {
	const base, rate = 3.0, 3.0
	vMax := destiny.WarpSpeedAUperSec * 149_597_870_700.0
	if c := 1.5 * legLen; c < vMax {
		vMax = c
	}
	if vMax < base*2 {
		vMax = base * 2
	}
	if d <= 0 {
		return 0
	}
	dAccel := (vMax - base) / rate
	if d <= dAccel {
		return math.Log(1+d*rate/base) / rate
	}
	return math.Log(vMax/base)/rate + (d-dAccel)/vMax
}

// ── the burn rule ───────────────────────────────────────────────────────────

// safeKill is one killmail position, for the burn rule.
type safeKill struct {
	SystemID int64
	Pos      destiny.Vec3
	At       int64
}

// safeBurnReason says why a safe is burned at now, or "" when it is still
// good: "aged" past safeMaxAgeSec, or "kill" for a kill within safeBurnRadiusM
// of it after it was made. Pure.
func safeBurnReason(s safeSpot, kills []safeKill, now int64) string {
	if now-s.CreatedAt > safeMaxAgeSec {
		return "aged"
	}
	p := s.pos()
	for _, k := range kills {
		if k.SystemID != s.SystemID || k.At < s.CreatedAt {
			continue
		}
		if k.Pos.Sub(p).LenSq() <= safeBurnRadiusM*safeBurnRadiusM {
			return "kill"
		}
	}
	return ""
}

// ── using a safe ────────────────────────────────────────────────────────────

// safeTarget is the warp target for a safe: a synthetic celestial with a
// negative ItemID and no radius. The warp reads only its position and radius, and
// currentDest readers treat an ID they cannot find as nothing in particular.
func safeTarget(s safeSpot) evedb.CelestialEntity {
	return evedb.CelestialEntity{
		ItemID: -(safeTargetBand + s.SafeID),
		X:      s.X, Y: s.Y, Z: s.Z,
		Name: s.Memo,
	}
}

// safeIDOfTarget is the safeID behind a safeTarget ItemID, or 0.
func safeIDOfTarget(itemID int64) int64 {
	if itemID > -safeTargetBand {
		return 0
	}
	return -itemID - safeTargetBand
}

// warpToSafe sends one projection to a safe and stamps the row used. Reports
// whether the warp started.
func (m *Mod) warpToSafe(eb *embodiedBot, s safeSpot, class warpClass) bool {
	if !m.startEmbodyWarpTo(eb, safeTarget(s), class) {
		return false
	}
	m.goSafeBot(eb.charID, "safe used", func() { m.markSafeUsed(s.SafeID) })
	return true
}

// fleetWarpToSafe sends a lead to a safe and pulls its idle fleet mates along
// (gangFollowWarp). Reports whether the lead's warp started.
func (m *Mod) fleetWarpToSafe(lead *embodiedBot, s safeSpot, class warpClass) bool {
	if !m.startEmbodyWarpAs(lead, safeTarget(s), true, class) {
		return false
	}
	m.goSafeBot(lead.charID, "safe used", func() { m.markSafeUsed(s.SafeID) })
	return true
}

func (m *Mod) markSafeUsed(safeID int64) {
	if m.db == nil || safeID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_safespots SET usedAt = ? WHERE safeID = ?`, clock.Unix(), safeID); err != nil {
		rlogf("rolebots: safe %d mark used: %v", safeID, err)
	}
}

// pickSafe returns the newest live safe a corporation holds in a system for a
// purpose. nearStationID 0 takes any; otherwise only a safe made for that
// station. False when there is none.
func (m *Mod) pickSafe(ctx context.Context, corpID, sysID int64, purpose string, nearStationID int64) (safeSpot, bool) {
	if m.db == nil || corpID == 0 || sysID == 0 {
		return safeSpot{}, false
	}
	q := `SELECT safeID, corpID, solarSystemID, x, y, z, bookmarkID, nearStationID, purpose,
	             createdBy, createdAt, usedAt, burnedAt
	        FROM rolebot_safespots
	       WHERE corpID = ? AND solarSystemID = ? AND purpose = ? AND burnedAt = 0 AND createdAt >= ?`
	args := []any{corpID, sysID, purpose, clock.Unix() - safeMaxAgeSec}
	if nearStationID != 0 {
		q += ` AND nearStationID = ?`
		args = append(args, nearStationID)
	}
	q += ` ORDER BY createdAt DESC, safeID DESC LIMIT 1`
	var s safeSpot
	err := m.db.QueryRowContext(ctx, q, args...).Scan(&s.SafeID, &s.CorpID, &s.SystemID,
		&s.X, &s.Y, &s.Z, &s.BookmarkID, &s.NearStationID, &s.Purpose,
		&s.CreatedBy, &s.CreatedAt, &s.UsedAt, &s.BurnedAt)
	if err != nil {
		if err != sql.ErrNoRows {
			rlogf("rolebots: pick safe corp %d sys %d: %v", corpID, sysID, err)
		}
		return safeSpot{}, false
	}
	s.Memo = m.safeMemo(ctx, s)
	return s, true
}

// liveSafePoints is every live safe a corporation holds in a system.
func (m *Mod) liveSafePoints(ctx context.Context, corpID, sysID int64) []destiny.Vec3 {
	rows, err := m.db.QueryContext(ctx, `
		SELECT x, y, z FROM rolebot_safespots
		 WHERE corpID = ? AND solarSystemID = ? AND burnedAt = 0 AND createdAt >= ?`,
		corpID, sysID, clock.Unix()-safeMaxAgeSec)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []destiny.Vec3
	for rows.Next() {
		var p destiny.Vec3
		if rows.Scan(&p.X, &p.Y, &p.Z) == nil {
			out = append(out, p)
		}
	}
	return out
}

// ── making a safe ───────────────────────────────────────────────────────────

// recordSafe writes the maker's bookmark and the row for a safe at s's
// position. s needs CorpID, SystemID, X/Y/Z, Purpose and CreatedBy; the rest
// is filled in and returned.
func (m *Mod) recordSafe(ctx context.Context, s safeSpot) (safeSpot, error) {
	if m.db == nil {
		return s, fmt.Errorf("no database")
	}
	if !validSafePurpose(s.Purpose) {
		return s, fmt.Errorf("bad safe purpose %q", s.Purpose)
	}
	s.CreatedAt = clock.Unix()
	s.Memo = m.newSafeMemo(ctx, s.CorpID, s.SystemID)
	bm, err := evedb.InsertBookmark(ctx, m.db, safeBookmark(s, s.CreatedBy))
	if err != nil {
		return s, err
	}
	s.BookmarkID = bm.BookmarkID
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_safespots
		  (corpID, solarSystemID, x, y, z, bookmarkID, nearStationID, purpose, createdBy, createdAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.CorpID, s.SystemID, s.X, s.Y, s.Z, s.BookmarkID, s.NearStationID, s.Purpose,
		s.CreatedBy, s.CreatedAt)
	if err != nil {
		_, _ = evedb.DeleteBookmarks(ctx, m.db, s.CreatedBy, []int64{s.BookmarkID})
		return s, fmt.Errorf("insert safe: %w", err)
	}
	s.SafeID, _ = res.LastInsertId()
	m.startSafeSweep()
	m.logBot(s.CreatedBy, logInfo, "safes", "made %s safe %q in system %d (safe %d)",
		s.Purpose, s.Memo, s.SystemID, s.SafeID)
	return s, nil
}

// safeBookmark is a coordinate bookmark of s owned by owner, in the shape the
// core's BookmarkLocation writes for a spot in space.
func safeBookmark(s safeSpot, owner int64) evedb.Bookmark {
	return evedb.Bookmark{
		OwnerID:    owner,
		TypeID:     safeBookmarkTypeID,
		Memo:       s.Memo,
		X:          s.X,
		Y:          s.Y,
		Z:          s.Z,
		LocationID: s.SystemID,
	}
}

// newSafeMemo is "<ticker> safe N", N counting the corporation's safes in the
// system including burned ones, so a caption is never reused there.
func (m *Mod) newSafeMemo(ctx context.Context, corpID, sysID int64) string {
	var tag string
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(tickerName, ''), corporationName, '')
		  FROM corporation WHERE corporationID = ?`, corpID).Scan(&tag)
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_safespots WHERE corpID = ? AND solarSystemID = ?`,
		corpID, sysID).Scan(&n)
	return strings.TrimSpace(fmt.Sprintf("%s safe %d", tag, n+1))
}

// safeMemo reads a safe's caption back from the maker's bookmark.
func (m *Mod) safeMemo(ctx context.Context, s safeSpot) string {
	if s.Memo != "" {
		return s.Memo
	}
	var memo string
	if m.db != nil && s.BookmarkID != 0 {
		_ = m.db.QueryRowContext(ctx, `SELECT memo FROM bookmarks WHERE bookmarkID = ?`,
			s.BookmarkID).Scan(&memo)
	}
	if memo == "" {
		memo = fmt.Sprintf("safe %d", s.SafeID)
	}
	return memo
}

// makeSafeAbstract makes a safe without flying it: the point a pilot would
// bookmark warping from a planet to toID, on a leg whose point is off the
// grid of every live safe the corporation already holds there. False when the
// system has no such leg.
func (m *Mod) makeSafeAbstract(ctx context.Context, corpID, makerCharID, sysID, toID int64,
	purpose string, nearStationID int64) (safeSpot, bool) {
	if m.db == nil {
		return safeSpot{}, false
	}
	cels, err := evedb.CachedSystemCelestials(ctx, m.db, sysID)
	if err != nil {
		rlogf("rolebots: safe celestials sys %d: %v", sysID, err)
		return safeSpot{}, false
	}
	leg, ok := chooseSafeLeg(safeLegs(cels, toID, safeClearM), m.liveSafePoints(ctx, corpID, sysID), safeClearM)
	if !ok {
		return safeSpot{}, false
	}
	s, err := m.recordSafe(ctx, safeSpot{
		CorpID: corpID, SystemID: sysID,
		X: leg.Point.X, Y: leg.Point.Y, Z: leg.Point.Z,
		NearStationID: nearStationID, Purpose: purpose, CreatedBy: makerCharID,
	})
	if err != nil {
		rlogf("rolebots: abstract safe sys %d: %v", sysID, err)
		return safeSpot{}, false
	}
	return s, true
}

// makeSafeOnWarp warps a projection to `to` and bookmarks a safe on the leg: the
// first point clear of the origin and every celestial. The caller places the pilot
// at the leg's origin; onMade gets the row once written. Reports whether the warp
// started. See docs/comment-notes-b16.md#safe-watch.
func (m *Mod) makeSafeOnWarp(eb *embodiedBot, to evedb.CelestialEntity, purpose string,
	nearStationID int64, class warpClass, onMade func(safeSpot)) bool {
	if eb == nil || eb.ent == nil || eb.ent.Manager == nil || !validSafePurpose(purpose) {
		return false
	}
	from := eb.ent.Pos()
	toPos := destiny.Vec3{X: to.X, Y: to.Y, Z: to.Z}
	pts := make([]destiny.Vec3, 0, len(eb.celestials))
	for _, c := range eb.celestials {
		pts = append(pts, destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z})
	}
	point, ok := safeOnLeg(from, toPos, pts, safeClearM)
	if !ok {
		return false
	}
	alignSec, tunnelSec, _ := eb.ent.Manager.WarpPhases(toPos)
	if alignSec < embodyQueueHold {
		alignSec = embodyQueueHold
	}
	along := point.Sub(from).Len()
	passAt := alignSec + math.Min(warpReachSec(along, toPos.Sub(from).Len()), tunnelSec)
	giveUp := alignSec + tunnelSec + safeWatchSlackSec

	if !m.startEmbodyWarpTo(eb, to, class) {
		return false
	}
	eb.mu.Lock()
	gen := eb.warpGen
	eb.mu.Unlock()

	charID, corpID, sysID := eb.charID, eb.corpID, eb.systemID
	m.goSafeBot(charID, "safe watch", func() {
		tk := time.NewTicker(safeWatchStep)
		defer tk.Stop()
		// Elapsed counts ticks, not the clock; a dropped tick under load
		// bookmarks later, still on the leg.
		elapsed := 0.0
		for range tk.C {
			elapsed += safeWatchStep.Seconds()
			eb.mu.Lock()
			gone := eb.dead || eb.warpGen != gen || eb.currentDest != to.ItemID
			warping := eb.warping
			eb.mu.Unlock()
			if gone {
				return
			}
			passed := warping && elapsed >= passAt
			if !warping {
				// Landed, or abandoned before the tunnel: only a ship that moved past
				// the point made it there.
				passed = eb.ent.Pos().Sub(from).Len() >= along
				if !passed {
					return
				}
			}
			if passed {
				break
			}
			if elapsed > giveUp {
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, err := m.recordSafe(ctx, safeSpot{
			CorpID: corpID, SystemID: sysID,
			X: point.X, Y: point.Y, Z: point.Z,
			NearStationID: nearStationID, Purpose: purpose, CreatedBy: charID,
		})
		if err != nil {
			m.logBot(charID, logError, "safes", "could not bookmark a safe: %v", err)
			return
		}
		if onMade != nil {
			onMade(s)
		}
	})
	return true
}

// ── sharing ─────────────────────────────────────────────────────────────────

// safeRecipients is everyone a lead shares a fleet safe with: bot mates of
// its module gang and of the real gang behind it (the fleet mirror, or the
// crew it serves as a conscript), and the human members of either.
func (m *Mod) safeRecipients(ctx context.Context, leadCharID, modGangID int64) (bots, humans []int64) {
	var realGangID int64
	if modGangID != 0 && m.db != nil {
		if members, err := gangMembers(ctx, m.db, modGangID); err == nil {
			for _, gm := range members {
				if gm.Driven {
					bots = append(bots, gm.CharacterID)
				} else {
					humans = append(humans, gm.CharacterID)
				}
			}
		}
		if fm := mirrorOf(modGangID); fm != nil {
			realGangID = fm.RealGangID
		}
	}
	if realGangID == 0 {
		if k := conscriptOf(leadCharID); k != nil {
			realGangID = k.GangID
		}
	}
	if realGangID != 0 {
		humans = append(humans, gangsta.HumanMembersOnline(realGangID)...)
		bots = append(bots, gangsta.BotMembers(realGangID)...)
	}
	return splitSafeRecipients(leadCharID, bots, humans, m.ownsCharacter)
}

// splitSafeRecipients dedupes the two lists, drops the lead, files every
// character the module owns as a bot whichever list it came in on, and drops
// a "bot" the module does not own. Pure.
func splitSafeRecipients(lead int64, bots, humans []int64, owned func(int64) bool) (outBots, outHumans []int64) {
	seen := map[int64]bool{lead: true, 0: true}
	for _, id := range bots {
		if !seen[id] && owned(id) {
			seen[id] = true
			outBots = append(outBots, id)
		}
	}
	for _, id := range humans {
		if seen[id] {
			continue
		}
		seen[id] = true
		if owned(id) {
			outBots = append(outBots, id)
		} else {
			outHumans = append(outHumans, id)
		}
	}
	return outBots, outHumans
}

// shareSafe gives each bot its own bookmark copy and, while the dial is on,
// each human a bookmark voucher in the cargo of the ship they are flying. A
// recipient who already holds the safe is skipped. Returns the counts given.
func (m *Mod) shareSafe(ctx context.Context, s safeSpot, bots, humans []int64) (nBots, nVouchers int) {
	if m.db == nil {
		return 0, 0
	}
	s.Memo = m.safeMemo(ctx, s)
	for _, id := range bots {
		if id == s.CreatedBy || m.holdsSafeBookmark(ctx, id, s) {
			continue
		}
		if _, err := evedb.InsertBookmark(ctx, m.db, safeBookmark(s, id)); err != nil {
			rlogf("rolebots: share safe %d to %d: %v", s.SafeID, id, err)
			continue
		}
		nBots++
	}
	if !safeVouchersOn() {
		return nBots, 0
	}
	for _, id := range humans {
		ship := boardedShip(ctx, m.db, id)
		if ship == 0 || m.holdsSafeVoucher(ctx, id, s) {
			continue
		}
		itemID, err := evedb.CreateBookmarkVoucher(ctx, m.db, safeBookmark(s, s.CreatedBy), id, ship, s.Memo)
		if err != nil {
			rlogf("rolebots: safe %d voucher to %d: %v", s.SafeID, id, err)
			continue
		}
		// CreateBookmarkVoucher files the item in the hangar flag; in a ship
		// that is not a hold the client shows, so move it to cargo.
		if _, err := m.db.ExecContext(ctx, `UPDATE entity SET flag = ? WHERE itemID = ?`,
			flagCargo, itemID); err != nil {
			rlogf("rolebots: safe %d voucher %d to cargo: %v", s.SafeID, itemID, err)
		}
		m.pushItemMoved(id, itemID)
		nVouchers++
	}
	if nBots+nVouchers > 0 {
		m.logBot(s.CreatedBy, logInfo, "safes", "shared %q with %d mate(s), %d voucher(s)",
			s.Memo, nBots, nVouchers)
	}
	return nBots, nVouchers
}

// shareSafeWithFleet shares a safe with the maker's fleet (safeRecipients).
func (m *Mod) shareSafeWithFleet(ctx context.Context, s safeSpot, modGangID int64) (int, int) {
	bots, humans := m.safeRecipients(ctx, s.CreatedBy, modGangID)
	return m.shareSafe(ctx, s, bots, humans)
}

func (m *Mod) holdsSafeBookmark(ctx context.Context, owner int64, s safeSpot) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM bookmarks
		 WHERE ownerID = ? AND locationID = ? AND x = ? AND y = ? AND z = ?`,
		owner, s.SystemID, s.X, s.Y, s.Z).Scan(&n)
	return n > 0
}

func (m *Mod) holdsSafeVoucher(ctx context.Context, owner int64, s safeSpot) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity
		 WHERE ownerID = ? AND typeID = ? AND x = ? AND y = ? AND z = ?`,
		owner, evedb.TypeBookmark, s.X, s.Y, s.Z).Scan(&n)
	return n > 0
}

// ── the sweep ───────────────────────────────────────────────────────────────

var (
	safeSweepOnce sync.Once
	safeSweepBusy atomic.Bool
)

// startSafeSweep registers the burn sweep the first time a safe is made, so a
// server that never makes one never runs it.
func (m *Mod) startSafeSweep() {
	safeSweepOnce.Do(func() {
		m.registerPeriodic("rolebots-safespots", safeSweepTicks, func(uint64) bool {
			if !Master() || !safeSweepBusy.CompareAndSwap(false, true) {
				return true
			}
			go func() {
				defer safeSweepBusy.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				m.sweepSafes(ctx)
			}()
			return true
		})
	})
}

// sweepSafes burns every live safe safeBurnReason condemns, removes the bot
// bookmarks that point at it, and deletes rows long burned.
func (m *Mod) sweepSafes(ctx context.Context) {
	if m.db == nil {
		return
	}
	now := clock.Unix()
	rows, err := m.db.QueryContext(ctx, `
		SELECT safeID, solarSystemID, x, y, z, createdAt
		  FROM rolebot_safespots WHERE burnedAt = 0`)
	if err != nil {
		rlogf("rolebots: safe sweep: %v", err)
		return
	}
	var live []safeSpot
	for rows.Next() {
		var s safeSpot
		if rows.Scan(&s.SafeID, &s.SystemID, &s.X, &s.Y, &s.Z, &s.CreatedAt) == nil {
			live = append(live, s)
		}
	}
	rows.Close()

	var kills []safeKill
	if len(live) > 0 {
		systems := map[int64]bool{}
		since := now
		for _, s := range live {
			systems[s.SystemID] = true
			if s.CreatedAt < since {
				since = s.CreatedAt
			}
		}
		args := []any{since}
		for id := range systems {
			args = append(args, id)
		}
		krows, err := m.db.QueryContext(ctx, `
			SELECT solarSystemID, posX, posY, posZ, killTime FROM killmails
			 WHERE killTime >= ? AND solarSystemID IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(systems)), ",")+`)`, args...)
		if err != nil {
			rlogf("rolebots: safe sweep kills: %v", err)
		} else {
			for krows.Next() {
				var k safeKill
				if krows.Scan(&k.SystemID, &k.Pos.X, &k.Pos.Y, &k.Pos.Z, &k.At) == nil {
					kills = append(kills, k)
				}
			}
			krows.Close()
		}
	}

	burned := 0
	for _, s := range live {
		if safeBurnReason(s, kills, now) == "" {
			continue
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE rolebot_safespots SET burnedAt = ? WHERE safeID = ? AND burnedAt = 0`, now, s.SafeID); err != nil {
			continue
		}
		burned++
		// Bot copies only: a human's bookmark is theirs to keep.
		_, _ = m.db.ExecContext(ctx, `
			DELETE b FROM bookmarks b
			  JOIN rolebot_characters rc ON rc.characterID = b.ownerID
			 WHERE b.locationID = ? AND b.x = ? AND b.y = ? AND b.z = ?`,
			s.SystemID, s.X, s.Y, s.Z)
	}
	_, _ = m.db.ExecContext(ctx,
		`DELETE FROM rolebot_safespots WHERE burnedAt > 0 AND burnedAt < ?`, now-safePruneAfterSec)
	if burned > 0 {
		rlogf("rolebots: safe sweep burned %d of %d live safe(s)", burned, len(live))
	}
}
