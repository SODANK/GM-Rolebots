package rolebots

import (
	"context"
	"fmt"
	"sort"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/dogma"
)

// Implants: the catalogue and what a pilot has in its head.
//
// An implant is tagged like a module (modtags.go): derived when the catalogue loads, from the
// type's effects and attributes, never stored, through the core classifier dogma.TagImplant so
// module and server read an implant alike. The tag says what the implant is for (a channel, a
// scope, a pirate set and its grade); implantwants.go turns that into what a pilot would pay.
//
// Implants are never on the NPC market. They arrive as loot, and a bot asks whether to plug or
// sell one (beltloot.go) and whether one for sale here is worth the price (implantupkeep.go).
// Both ask implantValue. The catalogue ignores the published flag, as some implants ship unpublished.

// implantType is one implant in the catalogue.
type implantType struct {
	TypeID        int64
	Name          string
	BasePrice     float64
	Published     bool
	MarketGroupID int64
	Tag           dogma.ImplantTag
}

// Slot is the implant's slot (attr 331).
func (t *implantType) Slot() int64 { return t.Tag.Slot }

// Label is the short "what it does" line for logs and the panel.
func (t *implantType) Label() string {
	tag := t.Tag
	switch {
	case tag.Set != dogma.ISNone:
		if tag.Omega {
			return fmt.Sprintf("%s set Omega (%s grade, x%.2f)", tag.Set, tag.Grade, tag.SetMult)
		}
		return fmt.Sprintf("%s set piece (%s grade): %+.0f%% %s", tag.Set, tag.Grade, tag.Pct(), tag.Channel())
	case tag.Mindlink:
		return "warfare mindlink"
	case len(tag.Effects) > 0:
		e := tag.Effects[0]
		s := fmt.Sprintf("%+.0f%% %s", e.Pct, e.Channel)
		if e.ReqSkill != 0 {
			s += " (" + turretFamilyName(e.ReqSkill) + ")"
		}
		if !e.Channel.Folded() {
			s += " [not applied here]"
		}
		return s
	case tag.HasAttrBonus():
		id, b := tag.AttrID()
		return fmt.Sprintf("+%.0f %s", b, learningAttrName(id))
	case tag.Dummy:
		return "no effect"
	}
	return ""
}

func turretFamilyName(skillID int64) string {
	switch skillID {
	case dogma.SkillSmallHybridTurret:
		return "small hybrid"
	case dogma.SkillMediumHybridTurret:
		return "medium hybrid"
	case dogma.SkillLargeHybridTurret:
		return "large hybrid"
	case dogma.SkillSmallProjectileTurret:
		return "small projectile"
	case dogma.SkillMediumProjectileTurret:
		return "medium projectile"
	case dogma.SkillLargeProjectileTurret:
		return "large projectile"
	case dogma.SkillSmallEnergyTurret:
		return "small energy"
	case dogma.SkillMediumEnergyTurret:
		return "medium energy"
	case dogma.SkillLargeEnergyTurret:
		return "large energy"
	case dogma.SkillShieldEmissionSystems:
		return "shield transporters"
	case dogma.SkillRemoteArmorRepair:
		return "armour projectors"
	case dogma.SkillEnergyEmissionSystems:
		return "energy transfers"
	}
	return fmt.Sprintf("skill %d", skillID)
}

func learningAttrName(attrID int64) string {
	switch attrID {
	case 164:
		return "Charisma"
	case 165:
		return "Intelligence"
	case 166:
		return "Memory"
	case 167:
		return "Perception"
	case 168:
		return "Willpower"
	}
	return fmt.Sprintf("attribute %d", attrID)
}

// The implant group. Category 20 also holds boosters (303), which this
// client cannot consume, so the module reads the group, not the category.
const groupCyberimplant = 300

var (
	implantCatOnce sync.Once
	implantCatByID map[int64]*implantType
	implantCatList []*implantType
	implantCatErr  error
)

// implantCatalogue is every cyberimplant type, tagged. Static data, loaded
// once per process; the first caller pays ~2 queries per type.
func (m *Mod) implantCatalogue(ctx context.Context) (map[int64]*implantType, []*implantType, error) {
	implantCatOnce.Do(func() {
		byID := map[int64]*implantType{}
		var list []*implantType
		rows, err := m.db.QueryContext(ctx, `
			SELECT typeID, typeName, basePrice, published, COALESCE(marketGroupID, 0)
			FROM invTypes WHERE groupID = ?`, groupCyberimplant)
		if err != nil {
			implantCatErr = fmt.Errorf("implantCatalogue: %w", err)
			return
		}
		for rows.Next() {
			it := &implantType{}
			var pub int
			if err := rows.Scan(&it.TypeID, &it.Name, &it.BasePrice, &pub, &it.MarketGroupID); err != nil {
				rows.Close()
				implantCatErr = fmt.Errorf("implantCatalogue scan: %w", err)
				return
			}
			it.Published = pub != 0
			byID[it.TypeID] = it
			list = append(list, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			implantCatErr = err
			return
		}
		for _, it := range list {
			tag, err := evedb.TagImplantType(ctx, m.db, it.TypeID)
			if err != nil {
				implantCatErr = fmt.Errorf("implantCatalogue tag %d: %w", it.TypeID, err)
				return
			}
			it.Tag = tag
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Tag.Slot != list[j].Tag.Slot {
				return list[i].Tag.Slot < list[j].Tag.Slot
			}
			return list[i].Name < list[j].Name
		})
		implantCatByID, implantCatList = byID, list
	})
	if implantCatErr != nil {
		// A failed load is retried by the next process, not the next call: sync.Once has fired.
		// The error is returned on every call; a restart fixes a missing table.
		return nil, nil, implantCatErr
	}
	return implantCatByID, implantCatList, nil
}

// isImplantType reports whether typeID is a cyberimplant, from the
// catalogue (so a DB miss reads as "no").
func (m *Mod) isImplantType(ctx context.Context, typeID int64) bool {
	byID, _, err := m.implantCatalogue(ctx)
	if err != nil {
		return false
	}
	_, ok := byID[typeID]
	return ok
}

// pluggedImplant is one implant in a pilot's head.
type pluggedImplant struct {
	ItemID int64
	Type   *implantType
}

// pluggedImplants reads what the pilot has in, keyed by slot. An implant
// whose type the catalogue does not know (impossible unless the data moved)
// is skipped rather than failing the read.
func (m *Mod) pluggedImplants(ctx context.Context, charID int64) (map[int64]pluggedImplant, error) {
	byID, _, err := m.implantCatalogue(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := evedb.GetCharacterImplants(ctx, m.db, charID)
	if err != nil {
		return nil, err
	}
	out := map[int64]pluggedImplant{}
	for _, r := range rows {
		it := byID[r.TypeID]
		if it == nil {
			continue
		}
		out[it.Slot()] = pluggedImplant{ItemID: r.ItemID, Type: it}
	}
	return out, nil
}
