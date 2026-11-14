package rolebots

// What a name on a module is worth.
// invTypes.basePrice is one number for every variant of a module line, because
// it is a manufacturing anchor (sql/module_prices.sql fills it the same way). It
// cannot tell a hunter what a faction drop is worth, so the market opinion of a
// name lives here. The grade is invmetatypes.metaGroupID, not attribute 633,
// which most faction and officer types lack; the attribute only places a type
// inside the tech-one band.
//
// The grade applies only to a price from basePrice: a live ask is already a
// market price, and grading it again compounds on each re-list (askbook.go).

import (
	"context"
	"database/sql"
	"math"
)

// The metaGroupID values this database ships. Groups 7 and up are hull
// classes, not module grades, and fall through to the tech-one band.
const (
	metaGroupTech1     = 1
	metaGroupTech2     = 2
	metaGroupStoryline = 3
	metaGroupFaction   = 4
	metaGroupOfficer   = 5
	metaGroupDeadspace = 6
)

const (
	// metaGradeCap bounds the ladder: past 400 times its metal an officer module's
	// ask is no price at all.
	metaGradeCap = 400.0

	// metaGradeNamedStep is what each rung of the named band adds over plain tech
	// one. Four rungs end a bit over 3x, so the best named module rivals tech two.
	metaGradeNamedStep = 0.55
	metaGradeNamedTop  = 4 // attribute 633 runs 1-4 inside the tech-one band

	// The bands above named. Tech two is buildable and competes on build cost;
	// storyline, faction, deadspace and officer gear is loot-only, so the step jumps.
	metaGradeTech2     = 4.0
	metaGradeStoryline = 8.0
	metaGradeFaction   = 15.0
	metaGradeDeadspace = 30.0
	metaGradeOfficer   = 50.0

	// Each officer meta level above the floor compounds (the ladder runs to meta
	// 16). Types with no attribute row sit on the floor.
	metaGradeOfficerFloor = 8
	metaGradeOfficerStep  = 1.6
)

// metaGradeMultiplier is what one grade is worth above the tech one line it
// shares a basePrice with. Pure, so the harness can pin the shape: monotone in
// both arguments, 1.0 for plain tech one, and capped.
func metaGradeMultiplier(metaGroupID, meta int) float64 {
	switch metaGroupID {
	case metaGroupTech2:
		return metaGradeTech2
	case metaGroupStoryline:
		return metaGradeStoryline
	case metaGroupFaction:
		return metaGradeFaction
	case metaGroupDeadspace:
		return metaGradeDeadspace
	case metaGroupOfficer:
		step := meta - metaGradeOfficerFloor
		if step < 0 {
			step = 0 // no attribute row: the band's floor, not tech one
		}
		return math.Min(metaGradeOfficer*math.Pow(metaGradeOfficerStep, float64(step)),
			metaGradeCap)
	}
	// Tech one, and anything whose group is not a module grade at all.
	if meta <= 0 {
		return 1.0
	}
	if meta > metaGradeNamedTop {
		meta = metaGradeNamedTop
	}
	return 1.0 + float64(meta)*metaGradeNamedStep
}

// typeGrade reads a type's meta group and meta level in one go: metaGroupID
// from invmetatypes, attribute 633 from dgmtypeattributes. Both are absent for
// plenty of types and both read as 0, which metaGradeMultiplier treats as
// plain tech one.
func (m *Mod) typeGrade(ctx context.Context, typeID int64) (metaGroupID, meta int) {
	var grp, lvl sql.NullFloat64
	if err := m.db.QueryRowContext(ctx, `
		SELECT (SELECT metaGroupID FROM invmetatypes WHERE typeID = ?),
		       (SELECT COALESCE(NULLIF(valueFloat, 0), CAST(valueInt AS DOUBLE), 0)
		          FROM dgmtypeattributes WHERE typeID = ? AND attributeID = ?)`,
		typeID, typeID, attrMetaLevel).Scan(&grp, &lvl); err != nil {
		return 0, 0
	}
	if grp.Valid && grp.Float64 > 0 {
		metaGroupID = int(grp.Float64)
	}
	if lvl.Valid && lvl.Float64 > 0 {
		meta = int(lvl.Float64)
	}
	return metaGroupID, meta
}
