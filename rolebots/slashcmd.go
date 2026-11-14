package rolebots

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"gurimoon/server/slash"
)

// The module's in-game GM commands, reached through the slash.ModuleCommand seam.
// Every command answers as plain text in the GM's LOG window.
//
//   /rolebots promote [corpID]            : run the promotion pass, for one corporation or all
//   /rolebots ranks <corpID>              : print a corporation's chain of command
//   /rolebots conscripts                  : list pilots serving in player fleets
//   /rolebots release <charID>            : send a conscript back to its own rotation
//   /rolebots gank <corpID> [undock|gate] : raise a gank squad now; doctrine and roster still decide
//
// The seam is set once at Init and never torn down: the module cannot be unloaded.

func (m *Mod) registerSlashCommands() {
	slash.ModuleCommand = m.moduleSlashCommand
}

func (m *Mod) moduleSlashCommand(ctx context.Context, _ slash.ModuleCommandState,
	cmd string, args []string) (string, bool) {
	if cmd != "rolebots" {
		return "", false
	}
	if len(args) == 0 {
		return rolebotsSlashUsage, true
	}
	switch strings.ToLower(args[0]) {
	case "promote":
		if len(args) >= 2 {
			corpID, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Sprintf("rolebots promote: %q is not a corporation ID", args[1]), true
			}
			summary, err := m.promoteCorp(ctx, corpID)
			if err != nil {
				return fmt.Sprintf("rolebots promote: %v", err), true
			}
			return summary, true
		}
		summary, err := m.promotionPass(ctx)
		if err != nil {
			return fmt.Sprintf("rolebots promote: %v", err), true
		}
		return summary, true
	case "ranks":
		if len(args) < 2 {
			return "usage: /rolebots ranks <corpID>", true
		}
		corpID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Sprintf("rolebots ranks: %q is not a corporation ID", args[1]), true
		}
		ranks, err := m.listCorpRanks(ctx, corpID)
		if err != nil {
			return fmt.Sprintf("rolebots ranks: %v", err), true
		}
		if len(ranks) == 0 {
			return fmt.Sprintf("corporation %d has no seated fleet commanders.", corpID), true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Chain of command, corporation %d:\n", corpID)
		for _, r := range ranks {
			mark := ""
			if r.Manual {
				mark = " (appointed)"
			}
			fmt.Fprintf(&b, "  %d. %s — %s fleet commander, score %.0f%s\n",
				r.Seat, r.Name, r.Activity, r.Score, mark)
		}
		return strings.TrimRight(b.String(), "\n"), true
	case "conscripts":
		rows, err := m.conscriptRows(ctx)
		if err != nil {
			return fmt.Sprintf("rolebots conscripts: %v", err), true
		}
		if len(rows) == 0 {
			return "Nobody is serving in a player's fleet.", true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Pilots serving in player fleets (%d):\n", len(rows))
		for _, r := range rows {
			order := r.Order
			if order == "" {
				order = "standing by"
			}
			fmt.Fprintf(&b, "  %s (%d) — for %s, fleet %d, %s, %s\n",
				r.Name, r.CharID, r.BossName, r.GangID, order, r.Doing)
		}
		return strings.TrimRight(b.String(), "\n"), true
	case "release":
		if len(args) < 2 {
			return "usage: /rolebots release <charID>", true
		}
		charID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Sprintf("rolebots release: %q is not a character ID", args[1]), true
		}
		if conscriptOf(charID) == nil {
			return fmt.Sprintf("character %d is not serving in anybody's fleet.", charID), true
		}
		m.release(ctx, charID, "released by a GM")
		return fmt.Sprintf("character %d released — it goes back to its own rotation.", charID), true
	case "gank":
		if len(args) < 2 {
			return "usage: /rolebots gank <corpID> [undock|gate]", true
		}
		corpID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Sprintf("rolebots gank: %q is not a corporation ID", args[1]), true
		}
		site := ""
		if len(args) > 2 {
			site = strings.ToLower(args[2])
			if site != "undock" && site != "gate" {
				return "usage: /rolebots gank <corpID> [undock|gate]", true
			}
		}
		return m.forceGankFleet(ctx, corpID, site), true
	default:
		return rolebotsSlashUsage, true
	}
}

const rolebotsSlashUsage = "usage: /rolebots promote [corpID] | /rolebots ranks <corpID> | " +
	"/rolebots conscripts | /rolebots release <charID> | /rolebots gank <corpID> [undock|gate]"

// forceGankFleet raises a gank squad (gank_op.go) around one corporation's idle piracy
// commander, skipping the gank dial. The corporation must still hold the Predation doctrine
// and seat a free piracy commander; the formation's own refusal text is returned otherwise.
func (m *Mod) forceGankFleet(ctx context.Context, corpID int64, site string) string {
	if m.corpDoctrineFor(ctx, corpID) != DoctrinePredation {
		return fmt.Sprintf("corporation %d does not hold the Predation doctrine — a gank squad is a pirate's.", corpID)
	}
	ranks, err := m.listCorpRanks(ctx, corpID)
	if err != nil {
		return fmt.Sprintf("rolebots gank: %v", err)
	}
	var seat *fcSeat
	var busy []string
	for _, r := range ranks {
		if r.Activity != ActPiracy {
			continue
		}
		var inGang int
		_ = m.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM rolebot_gang_members WHERE characterID = ?`, r.CharID).Scan(&inGang)
		if inGang > 0 || popIsOffline(r.CharID) {
			busy = append(busy, r.Name)
			continue
		}
		seat = &fcSeat{corpID: corpID, charID: r.CharID, activity: ActPiracy, pack: true, gank: true, forced: true}
		break
	}
	if seat == nil {
		if len(busy) > 0 {
			return fmt.Sprintf("corporation %d: every piracy commander is already in a fleet or offline (%s).",
				corpID, strings.Join(busy, ", "))
		}
		return fmt.Sprintf("corporation %d seats no piracy commander — run /rolebots promote %d first.", corpID, corpID)
	}
	if site != "" {
		// The fleet's first site pick reads it (gank_op.go gankSite).
		gankSiteForce.Store(seat.charID, site)
	}
	ok, why, err := m.formFleetAround(ctx, *seat, nil)
	if err != nil {
		gankSiteForce.Delete(seat.charID)
		return fmt.Sprintf("rolebots gank: %v", err)
	}
	if !ok {
		gankSiteForce.Delete(seat.charID)
		return fmt.Sprintf("no gank squad for corporation %d — %s.", corpID, why)
	}
	return fmt.Sprintf("gank squad raised for corporation %d around %s (%d); watch rolebot_log tag gank and the Gangs page.",
		corpID, m.charNameOr(ctx, seat.charID), seat.charID)
}
