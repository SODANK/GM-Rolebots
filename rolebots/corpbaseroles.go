package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Department job roles held at Base.
//
// Every department member holds its arm's bundle (armRoles) and query and take on the arm's own
// divisions (armOwnRoles). A bot only takes work at its posted base (corpWorkStation), but a
// capsuleer with the roles in corprole can use them at any office. With rolebots.corp.baseRoles on
// and the core's location rule on (server_settings corp.locationRoles, db/locationroles.go), a
// capsuleer junior's or member's hangar bits and Factory Manager are granted as roles at Base
// instead. A department head holds its bundle at every office. Bots keep corprole, which the
// planner reads. The bits granted at Base are tracked in rolebot_corp_base_roles, the arm pass's
// grantMask rule: only what was added is taken away, and switching off takes them all back.

// armBaseScoped is what a Base-scoped member holds at its Base only: the hangar roles and Factory
// Manager. General roles (Trader, Security Officer, Config Starbase Equipment) hold everywhere.
const armBaseScoped = armIndustryRoles | armHangarRoles

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
	return want &^ armBaseScoped, want & armBaseScoped
}

// memberRoleRow is a member's own role row, without the titles or the core's
// filter GetCorporationMemberInfo folds in.
type memberRoleRow struct {
	Corprole, AtHQ, AtBase, AtOther uint64
	BaseID                          int64
}

func (m *Mod) readMemberRoleRow(ctx context.Context, charID int64) (memberRoleRow, error) {
	var r memberRoleRow
	err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(corprole, 0), COALESCE(rolesAtHQ, 0), COALESCE(rolesAtBase, 0),
		       COALESCE(rolesAtOther, 0), COALESCE(baseID, 0)
		  FROM chrCorporationRoles WHERE characterID = ?`, charID,
	).Scan(&r.Corprole, &r.AtHQ, &r.AtBase, &r.AtOther, &r.BaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	return r, err
}

// memberBaseID is a member's Base office station, 0 for none.
func (m *Mod) memberBaseID(ctx context.Context, charID int64) int64 {
	r, err := m.readMemberRoleRow(ctx, charID)
	if err != nil {
		return 0
	}
	return r.BaseID
}

// applyBaseRoles moves the role bits the arm pass holds for a member at its
// Base to want, adding and removing only its own. A bit the member holds
// corporation-wide stays in the Base column (setMemberRoles mirrors those).
func (m *Mod) applyBaseRoles(ctx context.Context, corpID, charID int64, want uint64) error {
	var prev uint64
	_ = m.db.QueryRowContext(ctx,
		`SELECT grantMask FROM rolebot_corp_base_roles WHERE characterID = ?`, charID).Scan(&prev)
	if prev == 0 && want == 0 {
		return nil
	}
	row, err := m.readMemberRoleRow(ctx, charID)
	if err != nil {
		return err
	}
	roles, grant := meritRoleDelta(row.AtBase, prev, want&armRoleMask)
	roles |= row.Corprole & corp.LocationalMask()
	roles = m.stripGuardedTake(ctx, corpID, charID, roles)
	if roles != row.AtBase {
		old, _ := evedb.GetCorporationMemberInfo(ctx, m.db, charID)
		v := int64(roles)
		if err := evedb.UpdateCorpMemberRow(ctx, m.db, charID, &evedb.CorpMemberUpdate{RolesAtBase: &v}); err != nil {
			return fmt.Errorf("base roles %d: %w", charID, err)
		}
		if h := modapi.Corp().MemberRolesChanged; h != nil {
			h(charID, old)
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

// mirrorLocationRoles carries the change in a member's corporation-wide hangar and
// container bits (oldGlobal to newGlobal) into its HQ, Base and Other columns,
// which are all the client's role window shows for those bits. A bit set or
// cleared by hand in one column and never held corporation-wide is left alone.
// Pure.
func mirrorLocationRoles(oldGlobal, newGlobal uint64, cols [3]uint64) [3]uint64 {
	loc := corp.LocationalMask()
	added := newGlobal &^ oldGlobal & loc
	removed := oldGlobal &^ newGlobal & loc
	for i := range cols {
		cols[i] = (cols[i] | added) &^ removed
	}
	return cols
}

// settingLocationMirrored marks that mirrorLocationBackfill has run.
const settingLocationMirrored = "rolebots.corp.locationRolesMirrored"

// mirrorLocationBackfill copies the hangar and container bits every member holds
// corporation-wide into its HQ, Base and Other columns, once: rows written before
// setMemberRoles mirrored them showed N in the client's role window for roles that
// worked at every office.
func (m *Mod) mirrorLocationBackfill(ctx context.Context) error {
	have, _, err := evedb.GetServerSetting(ctx, m.db, settingLocationMirrored)
	if err != nil {
		return fmt.Errorf("mirrorLocationBackfill: %w", err)
	}
	if have == "1" {
		return nil
	}
	loc := int64(corp.LocationalMask())
	res, err := m.db.ExecContext(ctx, `
		UPDATE chrCorporationRoles
		   SET rolesAtHQ = rolesAtHQ | (corprole & ?),
		       rolesAtBase = rolesAtBase | (corprole & ?),
		       rolesAtOther = rolesAtOther | (corprole & ?)
		 WHERE (corprole & ?) <> 0`, loc, loc, loc, loc)
	if err != nil {
		return fmt.Errorf("mirrorLocationBackfill: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		rlogf("rolebots: mirrored corporation-wide hangar roles into the location columns of %d member(s)", n)
	}
	return evedb.SetServerSetting(ctx, m.db, settingLocationMirrored, "1")
}
