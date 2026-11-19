package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gurimoon/dogma"
	"gurimoon/gang"
)

// The Boosts tab: /rolebots/gangs/boosts.
//
// A read-only view of the core's gang bonus board (gang.Boosts, written by cmd/gurimoon/gangboost.go):
// which bonus each live gang gets, from whom, in which system, and what every member wears. It covers
// every gang on the node, since the bonus rules ignore who is flying and a boost that fails to reach a
// player is what the page exists to show. Nothing here writes: the board is recomputed every few seconds.
// It sits on the Gangs strip because it answers the same question as the Fleets tab.

const gangTabs = `{{define "rb_gangtabs"}}
<div class="rb-subtabs">
  <a href="/rolebots/gangs"{{if eq .Tab "fleets"}} class="on"{{end}}>Fleets</a>
  <a href="/rolebots/gangs/boosts"{{if eq .Tab "boosts"}} class="on"{{end}}>Boosts</a>
</div>
{{end}}`

const tmplGangBoosts = gangTabs + `{{define "content"}}` + castTabsStyle + `
<style>
.gb-note { font-size:12px;color:#6b7280 }
.gb-pos { color:#6ee7b7 }
.gb-neg { color:#93c5fd }
.gb-pill { display:inline-block;padding:1px 8px;border-radius:10px;font-size:11px;background:#382020;color:#9ca3af }
.gb-pill.link { background:#1e2a4a;color:#93c5fd }
.gb-pill.skill { background:#0f2a1f;color:#6ee7b7 }
.gb-tbl td { padding-top:3px; padding-bottom:3px; font-size:13px }
</style>

<h2>Gangs</h2>
{{template "rb_gangtabs" .}}

<div class="card">
  <h3>Gang Bonuses <span class="gb-note">({{len .Gangs}} gang{{if ne (len .Gangs) 1}}s{{end}}, resolved {{.Resolved}})</span></h3>
  <p class="gb-note">
    Each bonus comes from the member with the best value and reaches every member
    <strong>in space in the same system</strong>. Bonuses of one kind never stack. Docked and offline members give and receive nothing.
  </p>
</div>

{{range .Gangs}}
<div class="card">
  <h3>Gang {{.GangID}} <span class="gb-note">&middot; {{.InSpace}} of {{len .Members}} in space &middot; resolved {{.Age}}</span></h3>
  {{if .Systems}}
    {{range .Systems}}
    <h4 style="margin:10px 0 4px">{{.Name}}</h4>
    <table class="gb-tbl" style="max-width:820px">
      <tr><th>Bonus</th><th>Kind</th><th>Stat</th><th>Strength</th><th>From</th></tr>
      {{range .Winners}}
      <tr>
        <td>{{.Source}}</td>
        <td>{{if .IsLink}}<span class="gb-pill link">link</span>{{else}}<span class="gb-pill skill">skill</span>{{end}}</td>
        <td>{{.Stat}}</td>
        <td class="{{if .Up}}gb-pos{{else}}gb-neg{{end}}">{{.Pct}}</td>
        <td>{{.From}}</td>
      </tr>
      {{end}}
    </table>
    {{end}}
  {{else}}
    <p class="gb-note">Nobody in this gang is projecting a bonus{{if eq .InSpace 0}} &mdash; nobody is in space{{end}}.</p>
  {{end}}
  <h4 style="margin:12px 0 4px">Members</h4>
  <table class="gb-tbl" style="max-width:820px">
    <tr><th>Pilot</th><th>Where</th><th>Wearing</th></tr>
    {{range .Members}}
    <tr>
      <td>{{.Name}}</td>
      <td>{{if .System}}{{.System}}{{else}}<span class="gb-note">docked / offline</span>{{end}}</td>
      <td style="font-size:12px">{{if .Wearing}}{{.Wearing}}{{else}}<span class="gb-note">&mdash;</span>{{end}}</td>
    </tr>
    {{end}}
  </table>
</div>
{{else}}
<div class="card"><p>No gangs are flying.</p></div>
{{end}}
{{end}}`

type gangBoostWinnerRow struct {
	Source string // skill or link name
	IsLink bool
	Stat   string
	Pct    string
	Up     bool
	From   string
}

type gangBoostSystemRow struct {
	Name    string
	Winners []gangBoostWinnerRow
}

type gangBoostMemberRow struct {
	Name    string
	System  string
	Wearing string
}

type gangBoostGangRow struct {
	GangID  int64
	Age     string
	InSpace int
	Systems []gangBoostSystemRow
	Members []gangBoostMemberRow
}

func (m *Mod) handleGangBoosts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	views := gang.Boosts.Snapshot()

	// Collect every ID the page names, then resolve them in three queries.
	var charIDs, typeIDs, sysIDs []int64
	for _, v := range views {
		for sysID, ws := range v.Winners {
			sysIDs = append(sysIDs, sysID)
			for _, c := range ws {
				charIDs = append(charIDs, c.SourceCharID)
				if _, id := boostChannel(c.Channel); id != 0 {
					typeIDs = append(typeIDs, id)
				}
			}
		}
		for _, mp := range v.Members {
			charIDs = append(charIDs, mp.CharID)
			if mp.SolarSystemID != 0 {
				sysIDs = append(sysIDs, mp.SolarSystemID)
			}
		}
	}
	charNames := boostNames(ctx, m.db, `SELECT characterID, characterName FROM character_ WHERE characterID IN (%s)`, charIDs)
	typeNames := boostNames(ctx, m.db, `SELECT typeID, typeName FROM invTypes WHERE typeID IN (%s)`, typeIDs)
	sysNames := boostNames(ctx, m.db, `SELECT solarSystemID, solarSystemName FROM mapSolarSystems WHERE solarSystemID IN (%s)`, sysIDs)
	name := func(names map[int64]string, id int64) string {
		if n := names[id]; n != "" {
			return n
		}
		return strconv.FormatInt(id, 10)
	}

	now := time.Now()
	var latest time.Time
	rows := make([]gangBoostGangRow, 0, len(views))
	for _, v := range views {
		if v.At.After(latest) {
			latest = v.At
		}
		row := gangBoostGangRow{GangID: v.GangID, Age: boostAge(now.Sub(v.At))}

		sids := make([]int64, 0, len(v.Winners))
		for sid := range v.Winners {
			sids = append(sids, sid)
		}
		sort.Slice(sids, func(i, j int) bool { return name(sysNames, sids[i]) < name(sysNames, sids[j]) })
		for _, sid := range sids {
			sr := gangBoostSystemRow{Name: name(sysNames, sid)}
			for _, c := range v.Winners[sid] {
				kind, id := boostChannel(c.Channel)
				sr.Winners = append(sr.Winners, gangBoostWinnerRow{
					Source: name(typeNames, id),
					IsLink: kind == "link",
					Stat:   c.Stat.String(),
					Pct:    fmt.Sprintf("%+.1f%%", c.Pct),
					Up:     c.Pct > 0,
					From:   name(charNames, c.SourceCharID),
				})
			}
			row.Systems = append(row.Systems, sr)
		}

		for _, mp := range v.Members {
			mr := gangBoostMemberRow{Name: name(charNames, mp.CharID)}
			if mp.SolarSystemID != 0 {
				row.InSpace++
				mr.System = name(sysNames, mp.SolarSystemID)
				mr.Wearing = boostWearing(mp.Boost)
			}
			row.Members = append(row.Members, mr)
		}
		sort.SliceStable(row.Members, func(i, j int) bool {
			if (row.Members[i].System == "") != (row.Members[j].System == "") {
				return row.Members[i].System != "" // in space first
			}
			return row.Members[i].Name < row.Members[j].Name
		})
		rows = append(rows, row)
	}

	resolved := "never"
	if !latest.IsZero() {
		resolved = boostAge(now.Sub(latest))
	}
	espRender(w, "rolebots_gang_boosts.html", map[string]any{
		"Page":     "rolebot-gangs",
		"Tab":      "boosts",
		"Gangs":    rows,
		"Resolved": resolved,
	})
}

// boostChannel splits "skill:3349" / "link:20409" into its kind and typeID.
func boostChannel(ch string) (string, int64) {
	kind, id, ok := strings.Cut(ch, ":")
	if !ok {
		return ch, 0
	}
	n, _ := strconv.ParseInt(id, 10, 64)
	return kind, n
}

// boostWearing renders a member's applied multipliers, e.g.
// "max velocity +10.0%, armor resonance −12.0%".
func boostWearing(b dogma.GangBoost) string {
	var parts []string
	for s := dogma.GangStat(0); s < dogma.NumGangStats; s++ {
		if m := b.Mult(s); m != 1 {
			parts = append(parts, fmt.Sprintf("%s %+.1f%%", s, (m-1)*100))
		}
	}
	return strings.Join(parts, ", ")
}

func boostAge(d time.Duration) string {
	switch {
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	default:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
}

// boostNames resolves an ID list to names with one query. Failures return
// what was found; the page falls back to bare IDs.
func boostNames(ctx context.Context, db *sql.DB, query string, ids []int64) map[int64]string {
	out := map[int64]string{}
	if db == nil || len(ids) == 0 {
		return out
	}
	seen := map[int64]bool{}
	var args []any
	for _, id := range ids {
		if id != 0 && !seen[id] {
			seen[id] = true
			args = append(args, id)
		}
	}
	if len(args) == 0 {
		return out
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	rows, err := db.QueryContext(ctx, fmt.Sprintf(query, ph), args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n string
		if rows.Scan(&id, &n) == nil {
			out[id] = n
		}
	}
	return out
}
