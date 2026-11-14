package rolebots

// Killmails for rolebot deaths. The core files a killmail only for a death it saw on a live
// player connection (handlePlayerShipDestroyed and handlePlayerPodKilled need ds.entity.Conn),
// and a bot has none, so without this a player who kills a rolebot gets nothing on the board.
//
// No hook is needed: db.RecordKillmail takes plain values, no *destiny.Entity, so a module with
// its own death path files with the same call the core makes. The abstract tier, whose victims
// were never destiny entities, files through it with an empty attacker list. Recording is
// independent of mail suppression (mailsuppress.go): the board gets the kill, the mailbox nothing.

import (
	"context"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// fileKillmail records one rolebot death on the killboard. It runs off the caller's goroutine
// with its own context, because the death path holds the pilot's state and a slow write would
// stall the sim lane. Every failure is logged and swallowed: the pod, the wreck and the respawn
// do not depend on the board.
func (m *Mod) fileKillmail(charID int64, charName string, systemID, shipTypeID int64,
	x, y, z float64, isPod bool, hits []destiny.KillHit,
	dropped, destroyedItems []evedb.EntityRow) {

	if m.db == nil || charID == 0 {
		return
	}
	// Snapshot the affiliation now: the caller is about to move this pilot
	// between ships, and a corp change mid-teardown would misattribute the loss.
	corpID, allianceID := m.affiliationOf(charID)

	rec := evedb.KillmailRecord{
		Time:             time.Now(),
		SolarSystemID:    systemID,
		X:                x,
		Y:                y,
		Z:                z,
		VictimCharID:     charID,
		VictimName:       charName,
		VictimCorpID:     corpID,
		VictimAllianceID: allianceID,
		ShipTypeID:       shipTypeID,
		IsPod:            isPod,
		Source:           evedb.KillmailSourceModule,
		VictimIsBot:      true,
	}
	for _, h := range hits {
		rec.Attackers = append(rec.Attackers, evedb.KillmailAttacker{
			CharID:       h.CharID,
			Name:         h.Name,
			CorpID:       h.CorpID,
			AllianceID:   h.AllianceID,
			SecStatus:    h.SecStatus,
			ShipTypeID:   h.ShipTypeID,
			WeaponTypeID: h.WeaponTypeID,
			Damage:       h.Damage,
			IsNPC:        h.IsNPC,
			FinalBlow:    h.FinalBlow,
			Time:         h.Time,
		})
	}
	addItems := func(rows []evedb.EntityRow, wasDropped bool) {
		for _, it := range rows {
			rec.Items = append(rec.Items, evedb.KillmailItem{
				TypeID:    it.TypeID,
				Flag:      it.Flag,
				Quantity:  it.Quantity,
				Singleton: it.Singleton != 0,
				Dropped:   wasDropped,
			})
		}
	}
	addItems(dropped, true)
	addItems(destroyedItems, false)

	goSafe("killmail record", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		killID, err := evedb.RecordKillmail(ctx, m.db, rec)
		if err != nil {
			rlogf("rolebots: killmail for %d: %v", charID, err)
			return
		}
		// The relations ledger (corprelations_hooks.go): both death paths pass through this seam,
		// keyed on killID so the killboard catch-up cannot count it again.
		m.noteKillIncidents(ctx, killID, rec)
		// Said in corp chat, if a person is there (chatter.go).
		lossSlots := map[string]string{"system": m.nameOfSystem(ctx, systemID)}
		if isPod {
			lossSlots["ship"] = "pod"
		} else {
			lossSlots["ship"] = m.typeNameOr(ctx, shipTypeID)
		}
		for _, a := range rec.Attackers {
			if a.FinalBlow && !a.IsNPC && a.CharID != 0 && a.Name != "" {
				lossSlots["who"] = charLink(a.CharID, 0, a.Name)
			}
			if a.FinalBlow && a.IsNPC {
				if a.Name != "" && isStarbaseAttacker(a.IsNPC, a.CharID, a.CorpID, func(id int64) bool { return m.isPlayerCorpCached(ctx, id) }) {
					// A starbase gun: named like a pilot, never as rats.
					lossSlots["who"] = a.Name
					continue
				}
				// Only the rats lines can fill this; a pilot's kill never reads
				// as one.
				lossSlots["rats"] = "rats"
			}
		}
		m.chatterNoteBot(ctx, charID, "pers.loss", lossSlots, killID)
		// An envoy on the road (corpenvoy.go): its letter is lost with it.
		var attackerCorps []int64
		seen := map[int64]bool{}
		for _, a := range rec.Attackers {
			if !a.IsNPC && a.CorpID != 0 && a.CorpID != corpID && !seen[a.CorpID] {
				seen[a.CorpID] = true
				attackerCorps = append(attackerCorps, a.CorpID)
			}
		}
		var value float64
		_ = m.db.QueryRowContext(ctx, `SELECT totalValue FROM killmails WHERE killID = ?`, killID).Scan(&value)
		m.envoyLost(ctx, charID, corpID, killID, systemID, attackerCorps, value)
		// Another member corporation remarks on it in the alliance channel.
		m.chatterAllyLoss(ctx, charID, corpID, lossSlots["ship"], lossSlots["system"], value, isPod)
	})
}

// affiliationOf reads a pilot's corp and alliance. Zero for both on any error:
// an unattributed loss is a worse killmail, not a reason to skip one.
func (m *Mod) affiliationOf(charID int64) (corpID, allianceID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var alliance *int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT c.corporationID, co.allianceID
		  FROM character_ c
		  LEFT JOIN corporation co ON co.corporationID = c.corporationID
		 WHERE c.characterID = ?`, charID).Scan(&corpID, &alliance); err != nil {
		return 0, 0
	}
	if alliance != nil {
		allianceID = *alliance
	}
	return corpID, allianceID
}

// entityTypeID reads an item's typeID. Used on the death path to capture the
// hull's type before destroyShipAndContents removes the row.
func (m *Mod) entityTypeID(ctx context.Context, itemID int64) int64 {
	var typeID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ?`, itemID).Scan(&typeID); err != nil {
		return 0
	}
	return typeID
}
