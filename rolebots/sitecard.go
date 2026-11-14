package rolebots

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/npc"
	"gurimoon/rates"
)

// The site card: one dungeon template read into the vocabulary the belt hunt
// already judges with.
//
// A template says nothing about difficulty; the card is the sum, computed once
// per dungeon from the rows alone (dunRooms / dunObjects / dunTriggers /
// dunObjectLoot). Every rat becomes a ratThreat (the DPS/EHP pair
// combat_abstract.go reads), every wave a spawnThreat, and the belt hunt's gate
// judges it: wouldEngage(threatMargin(power, card.Worst)). A template also has
// held waves (sitewave.go), so the card records the field each wave lands on,
// and structures with authored loot, which a bot shoots down and scoops.

// siteStruct is one destructible structure of the room: a target that cannot
// shoot back (sentries are rats, below), worth whatever its authored loot is.
type siteStruct struct {
	ObjectID int64
	TypeID   int64
	GroupID  int64
	Name     string
	EHP      float64
	Loot     []evedb.DunObjectLootRow
	LootEV   float64
	// Invulnerable structures take no damage (dunObjects.invulnerable) and
	// are listed only so the board can show them; nothing targets one.
	Invulnerable bool
}

// siteWave is one spawn group of a room: wave 0 spawns with the room, N > 0
// waits for a trigger.
type siteWave struct {
	Wave int64
	// Rats as template combatants (SiteObjectID = dunObjects.objectID, Role "").
	// Sentry structures are in here too: they shoot, so the duel must fight
	// them, and they are told apart by IsSentry on the card's struct list.
	Rats    []*ratThreat
	Structs []siteStruct
	// Concurrent is what the field holds when this wave lands: the wave alone
	// for a roomCleared release, the wave plus the room's wave 0 for a
	// roomEnter or npcKilled release. This is what Worst is taken over.
	Concurrent spawnThreat
	// ReleasedBy names the event that lets the wave out ("" for wave 0,
	// "unreachable" for a wave no trigger releases).
	ReleasedBy string
}

// siteGate is the room's acceleration gate: where it stands in the room's
// own frame and which way it throws a ship. Dir is the core's rule
// (main.go spawnDungeonObjects): outward from the room anchor.
type siteGate struct {
	ObjectID int64
	Local    destiny.Vec3
	Dir      destiny.Vec3
}

type siteRoom struct {
	RoomID int64
	Index  int
	// Anchor is the frame origin the core uses for gate aim: the beacon's
	// local position, else the centroid of the room's objects.
	Anchor      destiny.Vec3
	BeaconLocal destiny.Vec3
	HasBeacon   bool
	Objects     []evedb.ObjectRow
	Triggers    []evedb.TriggerRow
	Waves       []siteWave // Waves[0] is wave 0; the rest ascend
	Gate        *siteGate
}

func (r *siteRoom) wave(n int64) *siteWave {
	for i := range r.Waves {
		if r.Waves[i].Wave == n {
			return &r.Waves[i]
		}
	}
	return nil
}

type siteCard struct {
	DungeonVID int64
	Version    int64
	Hash       uint64 // of (objectID, typeID, spawnWave); an ESP edit re-grades the card
	Name       string
	FactionID  int64
	CorpID     int64
	Rooms      []siteRoom

	Rats           int
	Structures     int
	LootStructures int
	Bounty         float64 // Σ rat bounty at the server rate
	LootEV         float64 // Σ rat table EV + Σ structure loot EV
	StructLootEV   float64

	// Worst is the heaviest concurrent field in the site and Heaviest its single
	// heaviest rat (the two numbers threatMargin reads). Full is every reachable
	// rat at once, the gate for a site that can hold a ship down.
	Worst    spawnThreat
	Heaviest *ratThreat
	Full     spawnThreat
	HasScram bool
	HasWeb   bool

	BuiltAt time.Time
}

// siteTypeInfo is what the pure builder needs to know about one type.
type siteTypeInfo struct {
	GroupID     int64
	CategoryID  int64
	Name        string
	MaxVelocity float64
	// StructureEHP is GetEntityHPByType folded by ehpOf, for a structure.
	StructureEHP float64
	Scram, Web   bool
	// Known is false when the type could not be read at all.
	Known bool
}

// siteRoomInput is one room's rows as the builder wants them.
type siteRoomInput struct {
	RoomID      int64
	Objects     []evedb.ObjectRow
	Triggers    []evedb.TriggerRow
	BeaconLocal destiny.Vec3
	HasBeacon   bool
}

// siteCardSources are the lookups the builder is handed, so a test can feed
// fixtures and the live loader can feed the database.
type siteCardSources struct {
	Rat       func(typeID int64) *ratThreat
	Info      func(typeID int64) siteTypeInfo
	Loot      func(objectID int64) []evedb.DunObjectLootRow
	Price     func(typeID int64) float64
	RatLootEV func(typeID, groupID int64) float64
}

// buildSiteCard is the pure half. Object classification mirrors the core's
// spawnMissionDungeonsForSystem: category 6/11 with a velocity is a rat; a
// destructible-structure group with hitpoints is a structure (a sentry group
// is a rat that stands still); everything else is scenery.
func buildSiteCard(vid, version int64, name string, factionID, corpID int64,
	rooms []siteRoomInput, src siteCardSources) *siteCard {

	card := &siteCard{DungeonVID: vid, Version: version, Name: name,
		FactionID: factionID, CorpID: corpID, BuiltAt: clock.Now()}
	h := fnv.New64a()
	for idx, in := range rooms {
		room := siteRoom{RoomID: in.RoomID, Index: idx, Objects: in.Objects, Triggers: in.Triggers,
			BeaconLocal: in.BeaconLocal, HasBeacon: in.HasBeacon}
		// The frame origin gates aim from (main.go spawnDungeonRoom).
		if in.HasBeacon {
			room.Anchor = in.BeaconLocal
		} else if len(in.Objects) > 0 {
			var c destiny.Vec3
			for _, o := range in.Objects {
				c.X += o.X
				c.Y += o.Y
				c.Z += o.Z
			}
			room.Anchor = c.Scale(1.0 / float64(len(in.Objects)))
		}
		waves := map[int64]*siteWave{}
		getWave := func(n int64) *siteWave {
			if w, ok := waves[n]; ok {
				return w
			}
			w := &siteWave{Wave: n}
			waves[n] = w
			return w
		}
		getWave(0)
		for _, o := range in.Objects {
			fmt.Fprintf(h, "%d:%d:%d;", o.ObjectID, o.TypeID, o.SpawnWave.Int64)
			info := src.Info(o.TypeID)
			if !info.Known {
				continue
			}
			if info.GroupID == destiny.GroupWarpGate {
				dir := destiny.Vec3{X: o.X - room.Anchor.X, Y: o.Y - room.Anchor.Y, Z: o.Z - room.Anchor.Z}
				if dir.Len() < 1.0 {
					dir = destiny.Vec3{X: 0, Y: 0, Z: 1}
				}
				room.Gate = &siteGate{ObjectID: o.ObjectID,
					Local: destiny.Vec3{X: o.X, Y: o.Y, Z: o.Z}, Dir: dir.Normalize()}
				continue
			}
			wave := int64(0)
			if o.SpawnWave.Valid && o.SpawnWave.Int64 > 0 {
				wave = o.SpawnWave.Int64
			}
			isStructure := npc.IsDestructibleStructure(info.GroupID)
			isRat := (info.CategoryID == 6 || info.CategoryID == 11) && info.MaxVelocity > 0
			switch {
			case isRat || npc.IsSentryStructure(info.GroupID):
				if o.Invulnerable {
					continue // cannot be killed; never a target
				}
				base := src.Rat(o.TypeID)
				if base == nil {
					continue
				}
				r := *base
				r.SiteObjectID = o.ObjectID
				r.Role = ""
				r.BeltRatID, r.BeltID, r.RegionID = 0, 0, 0
				r.MaxEHP = r.EHP
				r.Dealt = 0
				r.FactionID, r.CorpID = factionID, corpID
				if o.NPCFactionID.Valid && o.NPCFactionID.Int64 > 0 {
					r.FactionID = o.NPCFactionID.Int64
				}
				if !isRat && npc.IsSentryStructure(info.GroupID) && info.StructureEHP > 0 {
					// A sentry's hitpoints come off the structure fold: no
					// shield tau, no resists worth the name, but what is there.
					r.EHP, r.MaxEHP = info.StructureEHP, info.StructureEHP
				}
				if r.EHP <= 0 {
					r.EHP, r.MaxEHP = 100, 100
				}
				w := getWave(wave)
				w.Rats = append(w.Rats, &r)
				card.Rats++
				card.Bounty += r.Bounty
				if src.RatLootEV != nil {
					card.LootEV += src.RatLootEV(o.TypeID, info.GroupID)
				}
				if info.Scram {
					card.HasScram = true
				}
				if info.Web {
					card.HasWeb = true
				}
			case isStructure && info.StructureEHP > 0:
				s := siteStruct{ObjectID: o.ObjectID, TypeID: o.TypeID, GroupID: info.GroupID,
					Name: info.Name, EHP: info.StructureEHP, Invulnerable: o.Invulnerable}
				if o.BallName.Valid && o.BallName.String != "" {
					s.Name = o.BallName.String
				}
				if src.Loot != nil {
					s.Loot = src.Loot(o.ObjectID)
				}
				for _, l := range s.Loot {
					avg := float64(l.MinQty+l.MaxQty) / 2
					if avg <= 0 {
						avg = 1
					}
					price := 0.0
					if src.Price != nil {
						price = src.Price(l.ItemTypeID)
					}
					s.LootEV += l.DropChance * avg * price
				}
				w := getWave(wave)
				w.Structs = append(w.Structs, s)
				card.Structures++
				if len(s.Loot) > 0 && !s.Invulnerable {
					card.LootStructures++
					card.StructLootEV += s.LootEV
				}
			}
		}
		// Order the waves: 0 first, then ascending.
		for _, w := range waves {
			room.Waves = append(room.Waves, *w)
		}
		sort.Slice(room.Waves, func(i, j int) bool { return room.Waves[i].Wave < room.Waves[j].Wave })
		// What each wave meets when it lands.
		w0 := room.wave(0)
		for i := range room.Waves {
			w := &room.Waves[i]
			if w.Wave == 0 {
				w.Concurrent = spawnThreatOf(w.Rats)
				continue
			}
			t := waveReleasedBy(&room, w.Wave)
			if t == nil {
				// No trigger names it: an auto wave, out once the room is quiet
				// (evedb.NextAutoWave), so it lands on an empty room.
				w.ReleasedBy = evedb.AutoWaveEvent
				w.Concurrent = spawnThreatOf(w.Rats)
				continue
			}
			w.ReleasedBy = t.Event
			field := append([]*ratThreat(nil), w.Rats...)
			switch t.Event {
			case "roomEnter", "npcKilled", "npcDamaged":
				if w0 != nil {
					field = append(field, w0.Rats...)
				}
			}
			w.Concurrent = spawnThreatOf(field)
		}
		card.Rooms = append(card.Rooms, room)
	}
	card.Hash = h.Sum64()
	card.LootEV += card.StructLootEV

	// Worst: the concurrent field with the most to chew through, measured the
	// way seqDamageTaken bills it (DPS × EHP). Every wave is reachable: one no
	// trigger names is an auto wave.
	var reachable []*ratThreat
	for ri := range card.Rooms {
		room := &card.Rooms[ri]
		for wi := range room.Waves {
			w := &room.Waves[wi]
			reachable = append(reachable, w.Rats...)
			if siteFieldWeight(w.Concurrent) > siteFieldWeight(card.Worst) {
				card.Worst = w.Concurrent
			}
		}
	}
	card.Full = spawnThreatOf(reachable)
	for _, r := range reachable {
		if card.Heaviest == nil || r.DPS > card.Heaviest.DPS ||
			(r.DPS == card.Heaviest.DPS && r.EHP > card.Heaviest.EHP) {
			card.Heaviest = r
		}
	}
	return card
}

// siteFieldWeight orders concurrent fields: DPS × EHP is the product
// seqDamageTaken's closed form bills a side for, and the heaviest single rat
// breaks a tie so a lone battleship outranks a cloud of frigates with the same
// product.
func siteFieldWeight(t spawnThreat) float64 {
	return t.DPS*t.EHP + t.HeaviestDPS*t.HeaviestEHP*0.01
}

// siteMargin is the one gate. A site with a point in it is judged on the
// whole of it: a ship that cannot leave once pointed has to be able to kill
// everything that will ever be on the field.
func siteMargin(dps, ehp float64, card *siteCard) float64 {
	return siteMarginRep(dps, ehp, 0, card)
}

// siteMarginRep is siteMargin for a side that repairs the hull under fire
// (focusRep): rep is EHP/s on that hull.
func siteMarginRep(dps, ehp, rep float64, card *siteCard) float64 {
	if card == nil {
		return 0
	}
	field := card.Worst
	if card.HasScram {
		field = card.Full
	}
	return threatMarginRep(dps, ehp, rep, field)
}

// siteRequiredPower is the board's reading of a card: the (DPS, EHP) a lone
// hull needs for the margin to pass, solved from threatMargin's two terms with
// the pair held at the ratio of the worst field. It is a label, not a gate.
func siteRequiredPower(card *siteCard, margin float64) (dps, ehp float64) {
	if card == nil || margin <= 0 {
		return 0, 0
	}
	field := card.Worst
	if card.HasScram {
		field = card.Full
	}
	if field.DPS <= 0 || field.EHP <= 0 {
		return 0, 0
	}
	// Start from an even trade and walk the pair up until the gate opens.
	dps, ehp = field.DPS, field.EHP
	for i := 0; i < 64; i++ {
		if threatMargin(dps, ehp, field) >= margin {
			return dps, ehp
		}
		dps *= 1.1
		ehp *= 1.1
	}
	return dps, ehp
}

// ── the live loader and its cache ───────────────────────────────────────────

const siteCardTTL = 10 * time.Minute

var (
	siteCardMu    sync.Mutex
	siteCardCache = map[int64]*siteCard{}
)

// siteCardFor reads (or returns the cached) card for a dungeon. It rebuilds
// when dunDungeons.version or the object hash moves, or after the TTL (the
// editor saves rooms without touching version).
func (m *Mod) siteCardFor(ctx context.Context, vid int64) (*siteCard, error) {
	var version int64
	var name string
	var factionID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(version, 0), COALESCE(dungeonVName, ''), COALESCE(factionID, 0)
		   FROM dunDungeons WHERE dungeonVID = ?`, vid).Scan(&version, &name, &factionID); err != nil {
		return nil, fmt.Errorf("site card dungeon %d: %w", vid, err)
	}
	siteCardMu.Lock()
	cached := siteCardCache[vid]
	siteCardMu.Unlock()
	if cached != nil && cached.Version == version && clock.Now().Sub(cached.BuiltAt) < siteCardTTL {
		return cached, nil
	}

	roomIDs, err := evedb.GetDungeonRoomIDs(ctx, m.db, vid)
	if err != nil {
		return nil, err
	}
	var rooms []siteRoomInput
	for _, rid := range roomIDs {
		in := siteRoomInput{RoomID: rid}
		if in.Objects, err = evedb.GetRoomObjects(ctx, m.db, rid); err != nil {
			return nil, err
		}
		if in.Triggers, err = evedb.GetRoomTriggers(ctx, m.db, rid); err != nil {
			return nil, err
		}
		if bx, by, bz, ok, berr := evedb.GetRoomBeacon(ctx, m.db, rid); berr == nil && ok {
			in.BeaconLocal = destiny.Vec3{X: bx, Y: by, Z: bz}
			in.HasBeacon = true
		}
		rooms = append(rooms, in)
	}
	corpID := int64(0)
	if factionID > 0 {
		corpID = evedb.GetFactionCorpID(ctx, m.db, factionID)
	}
	infoCache := map[int64]siteTypeInfo{}
	src := siteCardSources{
		Rat: func(typeID int64) *ratThreat {
			r, err := m.ratThreatFor(ctx, typeID)
			if err != nil {
				return nil
			}
			return r
		},
		Info: func(typeID int64) siteTypeInfo {
			if v, ok := infoCache[typeID]; ok {
				return v
			}
			v := m.siteTypeInfo(ctx, typeID)
			infoCache[typeID] = v
			return v
		},
		Loot: func(objectID int64) []evedb.DunObjectLootRow {
			rows, err := evedb.GetObjectLoot(ctx, m.db, objectID)
			if err != nil {
				return nil
			}
			return rows
		},
		Price:     func(typeID int64) float64 { return evedb.TypeValueOf(ctx, m.db, typeID).Value },
		RatLootEV: func(typeID, groupID int64) float64 { return m.ratLootEV(ctx, typeID, groupID) },
	}
	card := buildSiteCard(vid, version, name, factionID, corpID, rooms, src)
	if cached != nil && cached.Hash == card.Hash && cached.Version == version {
		// Nothing authored changed; keep the older object so pointers held
		// by a run in progress stay valid, but refresh its clock.
		cached.BuiltAt = clock.Now()
		return cached, nil
	}
	siteCardMu.Lock()
	siteCardCache[vid] = card
	siteCardMu.Unlock()
	return card, nil
}

// siteTypeInfo reads what the classifier needs about one type.
func (m *Mod) siteTypeInfo(ctx context.Context, typeID int64) siteTypeInfo {
	var info siteTypeInfo
	if err := m.db.QueryRowContext(ctx,
		`SELECT t.groupID, g.categoryID, t.typeName FROM invTypes t
		   JOIN invGroups g ON g.groupID = t.groupID WHERE t.typeID = ?`, typeID).
		Scan(&info.GroupID, &info.CategoryID, &info.Name); err != nil {
		return info
	}
	info.Known = true
	if attrs, err := evedb.TypeAttrs(ctx, m.db, typeID); err == nil {
		info.MaxVelocity = attrs[attrMaxVelocity]
		if v := attrs[508]; v > 0 { // entityCruiseSpeed, as GetNPCStats prefers it
			info.MaxVelocity = v
		}
	}
	if npc.IsDestructibleStructure(info.GroupID) {
		if hp, err := evedb.GetEntityHPByType(ctx, m.db, typeID); err == nil && hp != nil {
			if hp.StructureHP > 0 {
				info.StructureEHP = ehpOf(hp.ShieldHP, hp.ArmorHP, hp.StructureHP,
					hp.ShieldRes, hp.ArmorRes, hp.HullRes)
			}
		}
	}
	if ew, err := evedb.GetNPCEwarProfile(ctx, m.db, typeID); err == nil && ew != nil {
		for _, mod := range ew.Modules {
			switch mod.Kind {
			case "scram":
				info.Scram = true
			case "web":
				info.Web = true
			}
		}
	}
	return info
}

// ratLootEV is the expected ISK a kill of this type drops, for the board and
// the candidate ranking: the gate chance, the average haul size, and the
// weighted mean of the pool's entries. An estimate, not the roll.
func (m *Mod) ratLootEV(ctx context.Context, typeID, groupID int64) float64 {
	entries, err := evedb.GetLootDropsForNPC(ctx, m.db, typeID, groupID)
	if err != nil || len(entries) == 0 {
		return 0
	}
	var total, weighted float64
	for _, e := range entries {
		if e.RequireRole != "" && e.RequireRole != "normal" {
			continue
		}
		if e.DropChance <= 0 {
			continue
		}
		avg := float64(e.MinQty+e.MaxQty) / 2
		if avg <= 0 {
			avg = 1
		}
		weighted += e.DropChance * avg * evedb.TypeValueOf(ctx, m.db, e.ItemTypeID).Value
		total += e.DropChance
	}
	if total <= 0 {
		return 0
	}
	anyDrop, lucky, jackpot := evedb.LootRollChances()
	picks := 1 + lucky + 2*jackpot
	return anyDrop * rates.LootDrop * picks * weighted / total
}

// invalidateSiteCard drops one dungeon's card (an ESP edit, a test).
func invalidateSiteCard(vid int64) {
	siteCardMu.Lock()
	delete(siteCardCache, vid)
	siteCardMu.Unlock()
}
