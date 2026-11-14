package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// The real belts: the bridge between the abstract tier and the server's persistent asteroid field.
//
// A field is read, not ledgered. asteroidFields holds the system's seed (db/asteroiddb.go) and
// asteroidDepletion what was taken per rock. destiny.GenerateAsteroidsSeeded is pure over (seed, belt
// celestials, security, sovereignty), so an unloaded field is rebuilt exactly and depletion overlaid.
// A loaded system is read and written through the live SolarSystem, never the tables: its memory is
// the authority and a row written behind it is lost at the next flush (flushSystemAsteroids).
// This module never mints a seed: EnsureAsteroidSeed can reseed, and reseeding clears depletion. A
// system with no seed is unknown and the caller mines uncapped.

// oreLot is one ore type and how many units a mining leg produced. A leg yields a mix (pickVariant
// splits a family 70/20/10) of invTypes rows with their own prices and yields, so a trip carries a list.
type oreLot struct {
	TypeID int64 `json:"t"`
	Units  int64 `json:"u"`
}

// beltRock is one asteroid: its own variant type, its belt, what is left in
// it and what the seed put there.
type beltRock struct {
	EntityID uint32
	BeltID   int64
	TypeID   int64 // the rock's own type, variant included (see MinedOreType)
	Qty      int64
	MaxQty   int64
}

// beltField is one system's asteroid field as it stands right now.
type beltField struct {
	SysID int64
	Seed  int64
	// Live means these rocks came from a loaded SolarSystem, so writes go through it, not
	// asteroidDepletion. Intact totals are unknowable: the live scan cannot see mined-out rocks.
	Live    bool
	Rocks   []beltRock
	builtAt int64
}

// beltCel is one asteroid-belt celestial: the anchor of one field.
type beltCel struct {
	ItemID  int64
	TypeID  int64 // destiny.IceFieldTypeID makes the belt ice
	X, Y, Z float64
}

const (
	// beltFieldCacheSec is how long a reconstructed field is reused. A mining pass asks about one system
	// several times in a row and each take updates the cached copy. A loaded system is never served from here.
	beltFieldCacheSec = 120

	// beltLiveScanRange is how far from a belt celestial a rock counts as that belt's. Fields reach
	// 46 km and celestials sit millions of km apart. It equals the embodied miner's scan range.
	beltLiveScanRange = 100_000.0
)

var (
	beltFieldMu    sync.Mutex
	beltFieldCache = map[int64]*beltField{}

	// beltSysLocks serialises everything that reads or debits one system's field, sharded because the
	// work includes a database write. It keeps a shared belt finite when several pilots work it and
	// avoids a data race on r.Qty. An unloaded system has this process as its only writer.
	beltSysLocks [64]sync.Mutex
)

func beltSysLock(sysID int64) *sync.Mutex {
	i := sysID % int64(len(beltSysLocks))
	if i < 0 {
		i = -i
	}
	return &beltSysLocks[i]
}

// invalidateBeltField drops a system's cached field. Called after a write
// whose outcome the cached copy cannot be trusted to reflect.
func invalidateBeltField(sysID int64) {
	beltFieldMu.Lock()
	delete(beltFieldCache, sysID)
	beltFieldMu.Unlock()
}

// clone is the read-only copy handed to callers outside the system lock.
// Rocks are values, so one copy of the slice is the whole snapshot.
func (f *beltField) clone() *beltField {
	if f == nil {
		return nil
	}
	out := *f
	out.Rocks = append([]beltRock(nil), f.Rocks...)
	return &out
}

// beltCelestialsOf lists a system's asteroid belts sorted by itemID, the order the generator sees
// them (destiny.beltCelestials sorts the same way). A belt's position decides which block of entity
// IDs its rocks occupy, so another order rebuilds a different field and every depletion row points
// at the wrong rock.
func (m *Mod) beltCelestialsOf(ctx context.Context, sysID int64) ([]beltCel, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, typeID, COALESCE(x,0), COALESCE(y,0), COALESCE(z,0)
		  FROM mapDenormalize
		 WHERE solarSystemID = ? AND groupID = 9
		 ORDER BY itemID`, sysID)
	if err != nil {
		return nil, fmt.Errorf("belt celestials: %w", err)
	}
	defer rows.Close()
	var out []beltCel
	for rows.Next() {
		var c beltCel
		if err := rows.Scan(&c.ItemID, &c.TypeID, &c.X, &c.Y, &c.Z); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// beltFieldFor returns the system's field as it stands, or nil when it cannot be known (no belts,
// no seed row, no asteroid tables). A nil field is not an error: the caller mines uncapped.
func (m *Mod) beltFieldFor(ctx context.Context, sysID int64) (*beltField, error) {
	if sysID == 0 {
		return nil, nil
	}
	lk := beltSysLock(sysID)
	lk.Lock()
	defer lk.Unlock()
	f, err := m.beltFieldLocked(ctx, sysID)
	if err != nil || f == nil {
		return nil, err
	}
	// A copy: the cached field is canonical and beltTakeReal writes through it under this lock.
	return f.clone(), nil
}

// beltFieldLocked is beltFieldFor's body, returning the canonical field.
// The caller must hold the system's lock and must not retain the pointer.
func (m *Mod) beltFieldLocked(ctx context.Context, sysID int64) (*beltField, error) {
	// A loaded system is never cached: its rocks move under the module. The scan builds a fresh field per
	// call and the debit goes through DecrementAsteroidQty, which does its own locking.
	if f, err := m.liveBeltField(ctx, sysID); err != nil {
		return nil, err
	} else if f != nil {
		return f, nil
	}

	now := clock.Unix()
	beltFieldMu.Lock()
	f, ok := beltFieldCache[sysID]
	beltFieldMu.Unlock()
	if ok && now-f.builtAt < beltFieldCacheSec {
		return f, nil
	}

	f, err := m.seededBeltField(ctx, sysID)
	if err != nil || f == nil {
		return nil, err
	}
	beltFieldMu.Lock()
	beltFieldCache[sysID] = f
	beltFieldMu.Unlock()
	m.recordBeltFieldTotals(ctx, f)
	return f, nil
}

// liveBeltField reads the field out of a loaded SolarSystem, belt by belt, or nil when it is not
// loaded. It scans outward from each belt celestial with AsteroidsInRange, the thread-safe read, since
// the exported asteroid maps are guarded by a lock this package cannot take. Depleted rocks are omitted.
func (m *Mod) liveBeltField(ctx context.Context, sysID int64) (*beltField, error) {
	ss := m.liveSystem(sysID)
	if ss == nil {
		return nil, nil
	}
	cels, err := m.beltCelestialsOf(ctx, sysID)
	if err != nil {
		return nil, err
	}
	f := &beltField{SysID: sysID, Live: true, builtAt: clock.Unix()}
	seen := map[uint32]bool{}
	for _, c := range cels {
		pos := destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}
		for _, a := range ss.AsteroidsInRange(pos, beltLiveScanRange) {
			if seen[a.EntityID] {
				continue
			}
			seen[a.EntityID] = true
			f.Rocks = append(f.Rocks, beltRock{
				EntityID: a.EntityID, BeltID: c.ItemID,
				TypeID: a.TypeID, Qty: a.Qty, MaxQty: a.Qty,
			})
		}
	}
	return f, nil
}

// seededBeltField rebuilds an unloaded system's field from its stored seed and overlays the depletion
// rows. The generator runs against the stored security and sovereignty, which is what the field on
// disk was rolled under; on a mismatch the server reseeds.
func (m *Mod) seededBeltField(ctx context.Context, sysID int64) (*beltField, error) {
	cels, err := m.beltCelestialsOf(ctx, sysID)
	if err != nil {
		return nil, err
	}
	if len(cels) == 0 {
		return nil, nil // no belts here; nothing to know
	}

	var (
		seed      int64
		security  float64
		factionID int64
	)
	err = m.db.QueryRowContext(ctx, `
		SELECT seed, security, factionID FROM asteroidFields WHERE solarSystemID = ?`,
		sysID).Scan(&seed, &security, &factionID)
	if err == sql.ErrNoRows {
		// The galaxy is seeded at boot, so this system gained its first belt since and the next boot sweep
		// picks it up. Unknown, not an error, and never minted here.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("belt field seed: %w", err)
	}

	scratch := &destiny.SolarSystem{Security: security, FactionID: factionID}
	celBalls := make([]destiny.BallDesc, 0, len(cels))
	celSlims := make([]destiny.SlimItem, 0, len(cels))
	for _, c := range cels {
		celBalls = append(celBalls, destiny.BallDesc{
			EntityID: uint32(c.ItemID), Pos: destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}})
		celSlims = append(celSlims, destiny.SlimItem{
			BallID: c.ItemID, ItemID: c.ItemID, TypeID: c.TypeID, GroupID: 9})
	}
	scratch.GenerateAsteroidsSeeded(celBalls, celSlims, seed)

	qtys, err := evedb.LoadAsteroidDepletion(ctx, m.db, sysID)
	if err != nil {
		return nil, fmt.Errorf("belt field depletion: %w", err)
	}
	scratch.ApplyAsteroidDepletion(qtys)

	f := &beltField{SysID: sysID, Seed: seed, builtAt: clock.Unix()}
	// Nothing else holds this scratch system, so its maps are read directly. Mined-out rocks are kept:
	// fullness is measured against the intact total.
	for _, slim := range scratch.AsteroidSlims {
		id := uint32(slim.ItemID)
		f.Rocks = append(f.Rocks, beltRock{
			EntityID: id,
			BeltID:   scratch.AsteroidBelt[id],
			TypeID:   slim.TypeID,
			Qty:      scratch.AsteroidQty[id],
			MaxQty:   scratch.AsteroidMaxQty[id],
		})
	}
	return f, nil
}

// oreFamilyBases is every minable ore family's base typeID, the key BaseOreType folds a variant onto.
// Ice is absent: no fit in the module carries an ice harvester.
var oreFamilyBases = func() map[int64]string {
	out := map[int64]string{}
	for name, fam := range destiny.OreFamilies {
		out[fam.Base.TypeID] = name
	}
	return out
}()

// oreFamilyID resolves an ore family name to the typeID BaseOreType uses; oreFamilyBases is the same
// map read the other way. It reads destiny's table, not invTypes, because a variant typeID matches no rock.
func oreFamilyID(name string) int64 {
	if fam, ok := destiny.OreFamilies[name]; ok {
		return fam.Base.TypeID
	}
	return 0
}

// familyUnits totals what is left in a belt, per ore family. beltID 0 asks
// about the whole system.
func (f *beltField) familyUnits(beltID int64) map[int64]int64 {
	out := map[int64]int64{}
	if f == nil {
		return out
	}
	for _, r := range f.Rocks {
		if r.Qty <= 0 || (beltID != 0 && r.BeltID != beltID) {
			continue
		}
		base := destiny.BaseOreType(r.TypeID)
		if _, ok := oreFamilyBases[base]; !ok {
			continue // ice, or something the ore tables do not know
		}
		out[base] += r.Qty
	}
	return out
}

// beltM3 totals what each belt still holds of minable ore, in m3 because units are not comparable
// across ores (Veldspar 0.1 m3, Arkonor 16). Ice is left out as in familyUnits: it would send a
// miner to the one belt in reach that no fit can harvest.
func (f *beltField) beltM3() map[int64]float64 {
	out := map[int64]float64{}
	if f == nil {
		return out
	}
	for _, r := range f.Rocks {
		if r.Qty <= 0 {
			continue
		}
		if _, ok := oreFamilyBases[destiny.BaseOreType(r.TypeID)]; !ok {
			continue
		}
		out[r.BeltID] += float64(r.Qty) * destiny.UnitVolume(r.TypeID)
	}
	return out
}

// totalM3 is what the field holds now against what the seed put there; both are 0 for a live field.
// Ice is excluded from both sides: it is harvested by its own trip (ice.go), and counting it would
// put a floor under fullness.
func (f *beltField) totalM3() (left, full float64) {
	if f == nil || f.Live {
		return 0, 0
	}
	for _, r := range f.Rocks {
		if _, ok := oreFamilyBases[destiny.BaseOreType(r.TypeID)]; !ok {
			continue
		}
		v := destiny.UnitVolume(r.TypeID)
		if r.Qty > 0 {
			left += float64(r.Qty) * v
		}
		full += float64(r.MaxQty) * v
	}
	return left, full
}

// beltTakeReal debits the real rocks for one mining leg and reports what came out, variant by
// variant. Rocks are worked biggest first, which keeps the depletion table small. A short return is
// not an error; zero means the belt is worked out for this ore.
func (m *Mod) beltTakeReal(ctx context.Context, sysID, beltID, familyID, want int64) ([]oreLot, int64, error) {
	if sysID == 0 || beltID == 0 || familyID == 0 || want <= 0 {
		return nil, 0, nil
	}
	lk := beltSysLock(sysID)
	lk.Lock()
	defer lk.Unlock()

	f, err := m.beltFieldLocked(ctx, sysID)
	if err != nil {
		return nil, 0, err
	}
	if f == nil {
		// Field unknown: nothing to take. Answering want would mine uncapped, and the delivery step would
		// conjure a full hold against rocks nobody debited.
		return nil, 0, nil
	}

	idx := make([]int, 0, 16)
	for i := range f.Rocks {
		r := &f.Rocks[i]
		if r.BeltID != beltID || r.Qty <= 0 || destiny.BaseOreType(r.TypeID) != familyID {
			continue
		}
		idx = append(idx, i)
	}
	// Biggest first, entityID as the tiebreak so the order is stable across
	// two bots asking the same question in the same second.
	sort.Slice(idx, func(a, b int) bool {
		ra, rb := f.Rocks[idx[a]], f.Rocks[idx[b]]
		if ra.Qty != rb.Qty {
			return ra.Qty > rb.Qty
		}
		return ra.EntityID < rb.EntityID
	})

	ss := m.liveSystem(sysID)
	lots := map[int64]int64{}
	var writes []evedb.AsteroidDepletionRow
	var got int64
	remain := want

	for _, i := range idx {
		if remain <= 0 {
			break
		}
		r := &f.Rocks[i]
		take := r.Qty
		if take > remain {
			take = remain
		}
		if ss != nil {
			// Through the live system: it owns the field while loaded and marks the rock dirty for the core's flush.
			take = ss.DecrementAsteroidQty(r.EntityID, take)
			if take <= 0 {
				r.Qty = 0
				continue
			}
			if ss.GetAsteroidQty(r.EntityID) <= 0 {
				ss.MarkAsteroidDepleted(r.EntityID)
			}
		} else {
			writes = append(writes, evedb.AsteroidDepletionRow{
				EntityID: r.EntityID, BeltID: r.BeltID, Qty: r.Qty - take,
			})
		}
		r.Qty -= take
		remain -= take
		got += take
		lots[destiny.MinedOreType(r.TypeID)] += take
	}

	if len(writes) > 0 {
		if err := evedb.FlushAsteroidDepletion(ctx, m.db, sysID, writes); err != nil {
			// The ore is gone from the cached field but not the table; drop the cache so the next read is true.
			invalidateBeltField(sysID)
			return nil, 0, fmt.Errorf("belt depletion write: %w", err)
		}
	}
	if got > 0 && !f.Live {
		m.recordBeltFieldTotals(ctx, f)
	}

	out := make([]oreLot, 0, len(lots))
	for typeID, units := range lots {
		out = append(out, oreLot{TypeID: typeID, Units: units})
	}
	// Biggest lot first: it is the one the trip is named after in the log
	// and the one the sell station gets chosen for.
	sort.Slice(out, func(a, b int) bool {
		if out[a].Units != out[b].Units {
			return out[a].Units > out[b].Units
		}
		return out[a].TypeID < out[b].TypeID
	})
	return out, got, nil
}

// liveSystem is m.systems.Get with the nil-manager guard the harness needs:
// its Mod is built with no SystemManager at all.
func (m *Mod) liveSystem(sysID int64) *destiny.SolarSystem {
	if m.systems == nil {
		return nil
	}
	return m.systems.Get(sysID)
}
