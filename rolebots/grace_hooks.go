package rolebots

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"gurimoon/destiny"
)

// Where the grace ledger (grace_ledger.go) is fed: the two tiers' death sites, ransoms, robbery,
// aggression and favours from a bot's own leadership. Every write goes through noteGraceEventAsync,
// off whatever goroutine the event happened on.

// noteGraceEventAsync files one event on its own goroutine with its own context.
func (m *Mod) noteGraceEventAsync(botID, pilotID int64, kind graceKind, f fightFacts, systemID, shipTypeID int64) {
	if botID == 0 || pilotID == 0 || botID == pilotID {
		return
	}
	goSafe("grace event", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		m.noteGraceEvent(ctx, botID, pilotID, kind, f, systemID, shipTypeID)
	})
}

// noteGraceFavour: a member's own leadership paid for it or stood for it.
func (m *Mod) noteGraceFavour(memberID, leaderID int64) {
	m.noteGraceEventAsync(memberID, leaderID, gkFavour, fightFacts{}, 0, 0)
}

// ── the gf memo ─────────────────────────────────────────────────────────────

// graceGF remembers, for a minute, a kill the loser read as a fair one: (winner, loser) → unix.
var graceGF sync.Map

func init() {
	registerCharForget("graceGF", func(charID int64) {
		graceGF.Range(func(k, _ any) bool {
			if p := k.([2]int64); p[0] == charID || p[1] == charID {
				graceGF.Delete(k)
			}
			return true
		})
	})
}

func noteGoodFight(winner, loser int64) { graceGF.Store([2]int64{winner, loser}, clock.Unix()) }

// graceGoodFightBetween reports, once, whether the loser read this kill as a good fight.
func graceGoodFightBetween(winner, loser int64) bool {
	v, ok := graceGF.LoadAndDelete([2]int64{winner, loser})
	return ok && clock.Unix()-v.(int64) < 60
}

// ── deaths ──────────────────────────────────────────────────────────────────

type graceKiller struct {
	CharID int64
	Damage float64
}

// graceFileKill files one kill on the dead bot's ledger, one entry per killer weighted by damage
// (top five), and the gf memo when the kind is respect.
func (m *Mod) graceFileKill(ctx context.Context, victimID int64, killers []graceKiller, f fightFacts, systemID, shipTypeID int64) {
	if len(killers) == 0 || !m.isSynthetic(victimID) {
		return
	}
	traits, err := getTraits(ctx, m.db, victimID)
	if err != nil {
		return
	}
	tn := resolveTuning(traits)
	kind := graceKillKind(tn, f)
	sort.Slice(killers, func(i, j int) bool { return killers[i].Damage > killers[j].Damage })
	if len(killers) > 5 {
		killers = killers[:5]
	}
	total := 0.0
	for _, k := range killers {
		total += k.Damage
	}
	if kind == gkRespect {
		noteGoodFight(killers[0].CharID, victimID)
	}
	for _, k := range killers {
		share := 1.0
		if total > 0 {
			share = max(0.25, k.Damage/total)
		}
		fk := f
		fk.Share = share
		m.noteGraceEventAsync(victimID, k.CharID, kind, fk, systemID, shipTypeID)
		if kind == gkRespect {
			// A fair fight is respected from both sides; the winner's own grace scales it.
			m.noteGraceEventAsync(k.CharID, victimID, gkRespect, fk, systemID, shipTypeID)
		}
	}
}

// noteGraceKill is the abstract tier's ship loss (killBotShip).
func (m *Mod) noteGraceKill(ctx context.Context, victim *pvpCombatant, killers []*pvpCombatant) {
	if victim == nil {
		return
	}
	var ks []graceKiller
	for _, k := range killers {
		if k != nil && k.Dealt > 0 && k.CharID != victim.CharID {
			ks = append(ks, graceKiller{k.CharID, k.Dealt})
		}
	}
	f := fightFacts{
		VictimArmed:  victim.Armed,
		VictimFights: victim.Armed && victim.Role == RoleCombat,
		VictimAtWork: victim.Role != RoleCombat,
		Ransom:       victim.RansomAsked,
		VictimN:      1,
		KillerN:      len(ks),
	}
	m.graceFileKill(ctx, victim.CharID, ks, f, victim.SystemID, victim.ShipTypeID)
}

// noteGracePod is the abstract tier's pod loss, right after the ship.
func (m *Mod) noteGracePod(ctx context.Context, victim *pvpCombatant, killers []*pvpCombatant) {
	if victim == nil {
		return
	}
	var ks []graceKiller
	for _, k := range killers {
		if k != nil && k.Dealt > 0 && k.CharID != victim.CharID {
			ks = append(ks, graceKiller{k.CharID, k.Dealt})
		}
	}
	m.graceFileKill(ctx, victim.CharID, ks, fightFacts{Pod: true, VictimN: 1, KillerN: len(ks)}, victim.SystemID, capsuleTypeID)
}

// noteGraceLoss is the embodied tier's loss (handleShipLoss, handlePodLoss): the hit table says who.
// Only pilots enter the personal ledger. A starbase kill has no pilot to hold it against; its
// owner's corporation takes it on the corporation ledger (noteKillIncidents).
func (m *Mod) noteGraceLoss(ctx context.Context, eb *embodiedBot, hits []destiny.KillHit, pod bool) {
	if eb == nil || eb.ent == nil {
		return
	}
	byChar := map[int64]float64{}
	for _, h := range hits {
		if h.IsNPC || h.CharID <= 0 || h.CharID == eb.charID || h.Damage <= 0 {
			continue
		}
		byChar[h.CharID] += h.Damage
	}
	if len(byChar) == 0 {
		return
	}
	ks := make([]graceKiller, 0, len(byChar))
	for id, d := range byChar {
		ks = append(ks, graceKiller{id, d})
	}
	f := fightFacts{Pod: pod, VictimN: 1, KillerN: len(ks)}
	shipTypeID := eb.ent.Slim.TypeID
	if !pod {
		if l, err := m.loadCombatLoadout(ctx, eb.charID, eb.shipID, shipTypeID); err == nil {
			f.VictimArmed = l.armed()
		}
		f.VictimFights = postureFights(eb.posture)
		f.VictimAtWork = !eb.patrol && !eb.prey && !eb.engage
	}
	m.graceFileKill(ctx, eb.charID, ks, f, eb.systemID, shipTypeID)
}

// noteGraceWin is the embodied tier's kill of a pilot that shot back: the winner's respect.
func (m *Mod) noteGraceWin(eb *embodiedBot, victimCharID int64, pod bool) {
	if pod || eb == nil || eb.ent == nil || victimCharID == 0 {
		return
	}
	shotBack := false
	for _, h := range eb.ent.KillHitsSnapshot() {
		if h.CharID == victimCharID && h.Damage > 0 {
			shotBack = true
			break
		}
	}
	if !shotBack {
		return
	}
	f := fightFacts{VictimArmed: true, VictimFights: true, VictimN: 1, KillerN: 1}
	if fairFight(f) {
		m.noteGraceEventAsync(eb.charID, victimCharID, gkRespect, f, eb.systemID, 0)
	}
}

// ── the profile page ────────────────────────────────────────────────────────

type graceRowView struct {
	Name, Grudge, Regard, Band, Kind, Age string
}

// graceRows is a bot's ledger for its profile page, newest first.
func (m *Mod) graceRows(ctx context.Context, botID int64) []graceRowView {
	if m.db == nil {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT g.pilotID, COALESCE(c.characterName, ''), g.grudge, g.regard, g.band, g.lastKind, g.lastAt
		  FROM rolebot_grievances g LEFT JOIN character_ c ON c.characterID = g.pilotID
		 WHERE g.botID = ? ORDER BY g.lastAt DESC LIMIT 20`, botID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	now := clock.Unix()
	var out []graceRowView
	for rows.Next() {
		var pilot, lastAt int64
		var name, kind string
		var g, r float64
		var band int
		if rows.Scan(&pilot, &name, &g, &r, &band, &kind, &lastAt) != nil {
			continue
		}
		if name == "" {
			name = fmt.Sprintf("character %d", pilot)
		}
		label := graceBandLabel(band)
		if label == "" {
			label = "—"
		}
		age := "just now"
		if d := now - lastAt; d >= 86400 {
			age = fmt.Sprintf("%d d ago", d/86400)
		} else if d >= 3600 {
			age = fmt.Sprintf("%d h ago", d/3600)
		} else if d >= 60 {
			age = fmt.Sprintf("%d min ago", d/60)
		}
		out = append(out, graceRowView{Name: name, Grudge: fmt.Sprintf("%.1f", g), Regard: fmt.Sprintf("%.1f", r),
			Band: label, Kind: kind, Age: age})
	}
	return out
}
