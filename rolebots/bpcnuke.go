package rolebots

// The one-shot clear-out of old print listings: every open bot listing whose crate holds a blueprint is
// expired through the core (evedb.ExpireEscrow), which returns the print to its issuer at the pickup
// station. The researcher finds it in its hangar and the library sweep files it; a corporation's listing
// comes back through its broker and settlePrintSales reshelves it. The listing fee is not refunded. It
// never deletes: the item returns through the path a listing's own expiry takes, so nothing is lost or
// duplicated.

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// settingBPOEscrowNuked marks that the clear-out has run.
const settingBPOEscrowNuked = "rolebots.bpoEscrowNuked"

// migrateBPOEscrowNuke expires every open bot escrow listing that holds a
// blueprint, once. Returns how many it expired.
func (m *Mod) migrateBPOEscrowNuke(ctx context.Context) (int, error) {
	have, _, err := evedb.GetServerSetting(ctx, m.db, settingBPOEscrowNuked)
	if err != nil {
		return 0, fmt.Errorf("migrateBPOEscrowNuke: %w", err)
	}
	if have == "1" {
		return 0, nil
	}
	ids, err := int64Column(ctx, m, `
		SELECT DISTINCT e.escrowID
		  FROM escrows e
		  JOIN entity i ON i.locationID = e.crateContainerID
		  JOIN invTypes t ON t.typeID = i.typeID
		  JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 9
		 WHERE e.state = ? AND e.issuerIsBot = 1`, evedb.CourierStatePosted)
	if err != nil {
		return 0, fmt.Errorf("migrateBPOEscrowNuke scan: %w", err)
	}
	n := 0
	for _, id := range ids {
		if err := evedb.ExpireEscrow(ctx, m.db, id); err != nil {
			// A listing claimed between the scan and here is not ours to
			// expire; anything else is logged and skipped, never fatal.
			rlogf("rolebots: print listing clear-out %d: %v", id, err)
			continue
		}
		n++
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingBPOEscrowNuked, "1"); err != nil {
		return n, fmt.Errorf("migrateBPOEscrowNuke marker: %w", err)
	}
	if n > 0 {
		rlogf("rolebots: cleared %d bot blueprint listing(s) off escrow — the prints went home to their issuers' hangars for the copy economy", n)
	}
	return n, nil
}
