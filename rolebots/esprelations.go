package rolebots

// The Relations page: corporate politics, the wars, and what the roster has made of both.
//
// Corp-to-corp standings live in the core's crpStandings table. The only other writer of those rows
// is the in-game standing2.SetCorpStanding RPC (a director with the corporation window open), so this
// is the one ESP page that can write one. It is also the debugging surface for corprel.go: every
// behaviour it drives is downstream of one snapshot, and without a view of it "nothing is declared"
// cannot be told from "the snapshot failed to build" except by reading the log.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// relOwnerNames resolves a mixed set of corporation and alliance IDs in one
// pass. Same COALESCE the core's ListWars uses, and for the same reason: a war
// or a standing may name either kind of owner and the page must not care.
func (m *Mod) relOwnerNames(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	seen := map[int64]bool{}
	var uniq []int64
	for _, id := range ids {
		if id != 0 && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.id, COALESCE(c.corporationName, a.allianceName, '')
		  FROM (SELECT ? AS id`+strings.Repeat(" UNION ALL SELECT ?", len(uniq)-1)+`) o
		  LEFT JOIN corporation c ON c.corporationID = o.id
		  LEFT JOIN alliances a ON a.allianceID = o.id`, int64Args(uniq)...)
	if err != nil {
		// Names are decoration: every row still renders by ID, which is what the forms post.
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if rows.Scan(&id, &name) == nil && name != "" {
			out[id] = name
		}
	}
	return out
}

func relOwnerLabel(names map[int64]string, id int64) string {
	if n := names[id]; n != "" {
		return n
	}
	return fmt.Sprintf("#%d", id)
}

// warHostileDelayTicksLocal is the core's 24-hour window, restated here because
// the constant is unexported in db. If the two ever disagree the page lies
// about a countdown; it cannot make a bot shoot anything.
func warHostileDelayTicksLocal() uint64 { return uint64(24) * 3600 * 10000000 }

// relTicksAway renders a future FILETIME as a rough distance from now.
func relTicksAway(at, now uint64) string {
	if at <= now {
		return "now"
	}
	secs := (at - now) / 10000000
	switch {
	case secs < 90:
		return "in under a minute"
	case secs < 3600:
		return fmt.Sprintf("in %d min", secs/60)
	default:
		return fmt.Sprintf("in %.1f h", float64(secs)/3600)
	}
}

func (m *Mod) handleRelations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == http.MethodPost {
		m.handleRelationsPost(w, r, ctx)
		return
	}

	stats := relSnapshotStats()
	age := ""
	switch {
	case stats.At == 0:
		age = "never built"
	default:
		age = fmt.Sprintf("%ds ago", clock.Unix()-stats.At)
	}

	espRender(w, "rolebots_relations.html", map[string]any{
		"Page":          "rolebot-corps",
		"Tab":           "diplomacy",
		"Stats":         stats,
		"Ledger":        relLedgerOn(),
		"Letters":       relLettersOn(),
		"Envoys":        relEnvoysOn(),
		"LedgerDials":   relDialRows(),
		"GraceDials":    graceDialRows(),
		"Age":           age,
		"Stale":         stats.At != 0 && clock.Unix()-stats.At > relStaleCeilingSec,
		"StaleMins":     relStaleCeilingSec / 60,
		"Enabled":       relationsEnabled(),
		"Mutual":        relationsMutual(),
		"WarEverywhere": warEverywhere(),
		"CEODiplomacy":  ceoDiplomacyOn(),
		"EnemyCost":     fmt.Sprintf("%.1f", relEnemyCost()),
		"EnemyHop":      relEnemyHopJumps(),
		"RefreshSec":    relRefreshSec(),
		"WarOppMul":     fmt.Sprintf("%.2f", warOpportunityMul()),
		"CEOWars":       ceoWarsOn(),
		"MercMarket":    mercMarketOn(),
		"GankHires":     gankHiresOn(),
		"MutualWars":    mutualWarsOn(),
		"WarChestMax":   fmt.Sprintf("%.2f", warChestMaxFrac()),
		"MercWage":      fmt.Sprintf("%.0f", mercWagePerPilot()),
		"WarAvoidMul":   fmt.Sprintf("%.1f", warAvoidMul()),
		"WarMarkMul":    fmt.Sprintf("%.1f", warMarkMul()),
		"WarBook":       m.relWarBookRows(ctx),
		"MercJobs":      m.relMercJobRows(ctx),
		"MercContracts": m.relMercContractRows(ctx),
		"Msg":           r.URL.Query().Get("msg"),
		"Err":           r.URL.Query().Get("err"),
	})
}

func (m *Mod) handleRelationsPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	const self = "/rolebots/corps/diplomacy"

	switch r.FormValue("action") {
	case "switch":
		key := r.FormValue("key")
		on := r.FormValue("on") == "1"
		set := m.setRelationsSwitch
		switch {
		case isWarSwitch(key):
			set = m.setWarSwitch
		case isRelLedgerSwitch(key):
			set = m.setRelLedgerSwitch
		}
		if err := set(ctx, key, on); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		// A switch that changes what the snapshot means is rebuilt at once; the mutual dial is read at build
		// time, so the page would otherwise disagree with the switch just flipped.
		m.refreshRelations(ctx)
		state := "off"
		if on {
			state = "on"
		}
		redirect(w, r, self, key+" is now "+state, "")

	case "dial":
		key := r.FormValue("key")
		v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("value")), 64)
		if err != nil || (v < 0 && !isRelLedgerDial(key)) {
			redirect(w, r, self, "", "that is not a number")
			return
		}
		setNum := m.setRelationsNumber
		switch {
		case isWarDial(key):
			setNum = m.setWarNumber
		case isRelLedgerDial(key):
			setNum = m.setRelLedgerDial
		case isGraceDial(key):
			setNum = m.setGraceDial
		}
		if err := setNum(ctx, key, v); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		redirect(w, r, self, key+" saved", "")

	case "rebuild":
		m.refreshRelations(ctx)
		st := relSnapshotStats()
		redirect(w, r, self, fmt.Sprintf(
			"snapshot rebuilt: %d declared pair(s), %d war(s) (%d hostile), %d corporation(s) with enemies",
			st.Pairs, st.Wars, st.HostileNow, st.Viewers), "")

	default:
		redirect(w, r, self, "", "unknown action")
	}
}

// ── the war market (corpceo_war.go, merc.go) ────────────────────────────────

type relWarBookRow struct {
	WarID                       int64
	Corp, Target, Motive, Bills string
	Note                        string
}

type relMercJobRow struct {
	JobID                        int64
	Client, Kind, Target, Budget string
	Round                        int
	Status, Quotes               string
}

type relMercContractRow struct {
	ID                                 int64
	Client, Merc, Target, Weekly, Paid string
	Destroyed, Status, Note            string
}

// relWarBookRows lists every war a bot CEO is running. A missing table reads
// as none, so the page still renders on a schema that predates the war power.
func (m *Mod) relWarBookRows(ctx context.Context) []relWarBookRow {
	rows, err := m.db.QueryContext(ctx, `
		SELECT warID, corporationID, targetOwnerID, motive, billsPaid, reason
		  FROM rolebot_ceo_wars WHERE endedAt = 0 ORDER BY warID DESC LIMIT 100`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []relWarBookRow
	for rows.Next() {
		var r relWarBookRow
		var corp, target int64
		var bills float64
		if rows.Scan(&r.WarID, &corp, &target, &r.Motive, &bills, &r.Note) != nil {
			continue
		}
		r.Corp, r.Target, r.Bills = ownerNameOf(ctx, m.db, corp), ownerNameOf(ctx, m.db, target), iskString(bills)
		out = append(out, r)
	}
	return out
}

// relMercJobRows lists the tenders in flight and the last week's closed ones.
func (m *Mod) relMercJobRows(ctx context.Context) []relMercJobRow {
	jobs, err := m.mercJobsWhere(ctx, `status IN ('open', 'haggling') OR closedAt > ? ORDER BY jobID DESC LIMIT 50`,
		clock.Unix()-7*86400)
	if err != nil {
		return nil
	}
	var out []relMercJobRow
	for _, j := range jobs {
		r := relMercJobRow{JobID: j.JobID, Client: ownerNameOf(ctx, m.db, j.Client), Kind: j.Kind,
			Target: ownerNameOf(ctx, m.db, j.TargetOwner), Budget: iskString(j.Budget), Round: j.Round, Status: j.Status}
		if bids, err := m.mercBidsFor(ctx, j.JobID); err == nil {
			var parts []string
			for _, b := range bids {
				if b.Status == "declined" {
					parts = append(parts, fmt.Sprintf("%s declined (%s)", ownerNameOf(ctx, m.db, b.Merc), b.Why))
					continue
				}
				parts = append(parts, fmt.Sprintf("%s %s, now %s/wk (%s)", ownerNameOf(ctx, m.db, b.Merc),
					iskString(b.Ask), iskString(b.LastAsk), b.Status))
			}
			r.Quotes = strings.Join(parts, "; ")
		}
		out = append(out, r)
	}
	return out
}

// relMercContractRows lists the running contracts and the last fortnight's
// ended ones.
func (m *Mod) relMercContractRows(ctx context.Context) []relMercContractRow {
	ks, err := m.mercContractsWhere(ctx, `status = 'active' OR endedAt > ? ORDER BY contractID DESC LIMIT 50`,
		clock.Unix()-14*86400)
	if err != nil {
		return nil
	}
	var out []relMercContractRow
	for _, k := range ks {
		r := relMercContractRow{ID: k.ID, Client: ownerNameOf(ctx, m.db, k.Client), Merc: ownerNameOf(ctx, m.db, k.Merc),
			Target: ownerNameOf(ctx, m.db, k.TargetOwner), Weekly: iskString(k.Weekly), Paid: iskString(k.PaidTotal),
			Destroyed: iskString(k.Destroyed), Status: k.Status}
		_ = m.db.QueryRowContext(ctx, `SELECT endReason FROM rolebot_merc_contracts WHERE contractID = ?`, k.ID).Scan(&r.Note)
		out = append(out, r)
	}
	return out
}
