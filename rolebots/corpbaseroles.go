package rolebots

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Department job roles held at Base.
//
// Industry and R&D hand every member Factory Manager and the library views (armIndustryRoles).
// A bot only takes work at its posted base (corpWorkStation), but a capsuleer with the roles in
// corprole can run the corporation's prints at any office. With rolebots.corp.baseRoles on and
// the core's location rule on (server_settings corp.locationRoles, db/locationroles.go), a
// capsuleer's bundle is granted as roles at Base instead. Bots keep corprole, which the planner
// reads. The bits granted at Base are tracked in rolebot_corp_base_roles, the arm pass's
// grantMask rule: only what was added is taken away, and switching off takes them all back.

// baseRolesActive is whether base roles are in force: the module's switch
// and the core's rule both.
func (m *Mod) baseRolesActive(ctx context.Context) bool {
	return baseRolesOn() && evedb.LocationRolesOn(ctx, m.db)
}

// splitBaseRoles parts an arm bundle into what stays in corprole and what is
// held at Base. Pure.
func splitBaseRoles(want uint64, atBase bool) (global, base uint64) {
	if !atBase {
		return want, 0
	}
	return want &^ armIndustryRoles, want & armIndustryRoles
}

// applyBaseRoles moves the role bits the arm pass holds for a member at its
// Base to want, adding and removing only its own.
func (m *Mod) applyBaseRoles(ctx context.Context, corpID, charID int64, want uint64) error {
	var prev uint64
	_ = m.db.QueryRowContext(ctx,
		`SELECT grantMask FROM rolebot_corp_base_roles WHERE characterID = ?`, charID).Scan(&prev)
	if prev == 0 && want == 0 {
		return nil
	}
	info, err := evedb.GetCorporationMemberInfo(ctx, m.db, charID)
	if err != nil || info == nil {
		return err
	}
	roles, grant := meritRoleDelta(info.RolesAtBase, prev, want&armRoleMask)
	roles = m.stripGuardedTake(ctx, corpID, charID, roles)
	if roles != info.RolesAtBase {
		v := int64(roles)
		if err := evedb.UpdateCorpMemberRow(ctx, m.db, charID, &evedb.CorpMemberUpdate{RolesAtBase: &v}); err != nil {
			return fmt.Errorf("base roles %d: %w", charID, err)
		}
		if h := modapi.Corp().MemberRolesChanged; h != nil {
			h(charID, info)
		}
	}
	if grant == 0 {
		_, err = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_base_roles WHERE characterID = ?`, charID)
		return err
	}
	_, err = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_base_roles (characterID, corporationID, grantMask) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), grantMask = VALUES(grantMask)`,
		charID, corpID, grant)
	return err
}
