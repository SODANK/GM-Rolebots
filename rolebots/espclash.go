//go:build mindclash

package rolebots

// The Mind Clash tab: /rolebots/clash (docs/MIND_CLASH_BOTS.md section 11).
//
// Read-mostly: this week's cards with bot money against capsuleer money per card, how far the crowd's
// biases lean, the fans travelling to each venue, and the knobs. The sport itself (roster, schedule,
// venues, rules) belongs to the MindClash module, on ESP /mindclash.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

type clashTabFan struct {
	Name, Fight, Errand, ETA string
}

type clashTabCard struct {
	Card                     *evedb.MCCard
	League                   string
	Fans                     []clashTabFan
	Money                    evedb.MCToteMoney
	FavShare                 string
	Bell                     string
	Pool, BotPool, HumanPool string
}

const tmplClash = `{{define "content"}}
<h2>Mind Clash &mdash; the bot crowd</h2>
<p style="font-size:12px;color:#9ca3af;max-width:980px">
  The sport is the MindClash module's: its own roster fights, and the schedule, venues and rules are on <a href="/mindclash">ESP Mind Clash</a>.
  This page is the bot population's part in it: how the bot money sits on the tote, how far the crowd's biases lean, and who is
  flying to a venue to stand in the house. Bots bet through the circuit's API like any capsuleer.
</p>
{{if .Msg}}<div class="alert alert-success">{{.Msg}}</div>{{end}}
<div class="stats">
  <div class="stat"><div class="label">Bot bettors (7 days)</div><div class="value">{{.Bettors}}</div></div>
  <div class="stat"><div class="label">Staked (7 days)</div><div class="value">{{.Staked}}</div></div>
  <div class="stat"><div class="label">Lane</div><div class="value">{{if .On}}on{{else}}off{{end}}</div></div>
</div>
<div class="card">
  <h3>Knobs</h3>
  <form method="POST" action="/rolebots/clash" style="font-size:12px">
    <label><input type="checkbox" name="on" value="1" {{if .On}}checked{{end}}> lane on</label> &nbsp;
    Max fans per card <input type="number" name="maxFans" value="{{.MaxFans}}" style="width:60px"> &nbsp;
    Betting allowance &times; <input type="text" name="betMul" value="{{.BetMul}}" style="width:60px"> &nbsp;
    <button type="submit">Save</button>
  </form>
</div>
<div class="card">
  <h3>This week</h3>
  {{range .Cards}}
  <div style="border:1px solid #374151;padding:8px;margin-bottom:8px">
    <b>{{.League}}</b> &mdash; {{.Card.VenueName}} &mdash; first bell {{.Bell}} &mdash; <b>{{.Card.State}}</b>
    <span style="color:#9ca3af">&mdash; tote {{.Pool}}: bots {{.BotPool}} ({{.Money.BotBettors}} bettors), capsuleers {{.HumanPool}}; bot money on the rating favourite {{.FavShare}}</span>
    {{if .Fans}}
    <table style="font-size:12px;margin-top:4px"><tr><th>Fan</th><th>Backing</th><th>Errand</th><th>ETA / arrival</th></tr>
      {{range .Fans}}<tr><td>{{.Name}}</td><td>{{.Fight}}</td><td>{{.Errand}}</td><td>{{.ETA}}</td></tr>{{end}}
    </table>
    {{else}}<p style="color:#6b7280;font-size:12px">No fan is travelling to this card.</p>{{end}}
  </div>
  {{else}}<p style="color:#6b7280">No card is scheduled.</p>{{end}}
</div>
{{end}}`

func (m *Mod) handleClashTab(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		on := r.FormValue("on") == "1"
		clashOn.Store(on)
		_ = evedb.SetServerSetting(ctx, m.db, settingClashEnabled, map[bool]string{true: "1", false: "0"}[on])
		if n, err := strconv.Atoi(r.FormValue("maxFans")); err == nil && n >= 0 {
			clashMaxFans.Store(int64(n))
			_ = evedb.SetServerSetting(ctx, m.db, settingClashMaxFans, strconv.Itoa(n))
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("betMul")), 64); err == nil && f >= 0 {
			clashBetMulX.Store(int64(f * 1000))
			_ = evedb.SetServerSetting(ctx, m.db, settingClashBetMul, strconv.FormatFloat(f, 'f', -1, 64))
		}
		http.Redirect(w, r, "/rolebots/clash?msg="+url.QueryEscape("Saved."), http.StatusSeeOther)
		return
	}
	var bettors int
	var staked float64
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT characterID), COALESCE(SUM(staked), 0) FROM rolebot_clash_bets WHERE at >= ?`,
		clock.Unix()-7*86400).Scan(&bettors, &staked)
	data := map[string]any{"Page": "rolebot-clash", "Msg": r.URL.Query().Get("msg"), "On": clashOn.Load(),
		"MaxFans": clashMaxFans.Load(), "BetMul": strconv.FormatFloat(clashBetMul(), 'f', -1, 64),
		"Bettors": bettors, "Staked": iskString(staked)}

	// This week's cards and their fans.
	var cards []clashTabCard
	ups, _ := evedb.MCUpcomingCards(ctx, m.db)
	for _, c := range ups {
		tc := clashTabCard{Card: c, League: evedb.MCLeagueName(c.LeagueID), Money: evedb.MCCardMoney(ctx, m.db, c.ID),
			Bell: time.Unix(c.FirstBell, 0).UTC().Format("2006-01-02 15:04")}
		tc.Pool, tc.BotPool, tc.HumanPool = iskString(tc.Money.Pool), iskString(tc.Money.BotPool), iskString(tc.Money.HumanPool)
		venueSys, _ := evedb.GetSolarSystemForStation(ctx, m.db, c.VenueStationID)
		rows, err := m.db.QueryContext(ctx, `SELECT characterID, errandSlot, errandState FROM rolebot_clash WHERE errandCard = ? AND errandKind = 'fan'`, c.ID)
		if err == nil {
			for rows.Next() {
				var id, slot int64
				var state string
				if rows.Scan(&id, &slot, &state) != nil {
					continue
				}
				f := clashTabFan{Name: evedb.GetCharacterName(ctx, m.db, id), Errand: state}
				if s, err := evedb.MCGetSlot(ctx, m.db, slot); err == nil {
					f.Fight = evedb.MCNameOf(ctx, m.db, s.RedID) + " v " + evedb.MCNameOf(ctx, m.db, s.BlueID)
				}
				var sys int64
				_ = m.db.QueryRowContext(ctx, `SELECT solarSystemID FROM character_ WHERE characterID = ?`, id).Scan(&sys)
				if path, err := m.route(ctx, sys, venueSys, travelRules{}); err == nil {
					eta := time.Duration(len(path)*clashSecPerJump) * time.Second
					f.ETA = fmt.Sprintf("%d jumps, ~%s", len(path), eta.Round(time.Minute))
					if len(path) == 0 {
						f.ETA = "at the venue system"
					}
				}
				tc.Fans = append(tc.Fans, f)
			}
			rows.Close()
		}
		tc.FavShare = m.clashFavShare(ctx, c.ID)
		cards = append(cards, tc)
	}
	data["Cards"] = cards
	espRender(w, "rolebots_clash.html", data)
}

// clashFavShare is how much of the bots' Winner money backs the rating favourite: the favourite-longshot
// bias, made visible.
func (m *Mod) clashFavShare(ctx context.Context, cardID int64) string {
	mkts, err := evedb.MCCardMarkets(ctx, m.db, cardID)
	if err != nil {
		return "—"
	}
	fav, total := 0.0, 0.0
	for _, mk := range mkts {
		if mk.Kind != evedb.MCMktWinner {
			continue
		}
		s, err := evedb.MCGetSlot(ctx, m.db, mk.SlotID)
		if err != nil {
			continue
		}
		cs, _ := evedb.MCGetClashers(ctx, m.db, []int64{s.RedID, s.BlueID})
		red, blue := cs[s.RedID], cs[s.BlueID]
		if red == nil || blue == nil {
			continue
		}
		favSide := "red"
		if blue.Rating > red.Rating {
			favSide = "blue"
		}
		rows, err := m.db.QueryContext(ctx, `SELECT b.outcome, SUM(b.stake) FROM mcToteBets b JOIN rolebot_characters r ON r.characterID = b.bettorID
			WHERE b.marketID = ? GROUP BY b.outcome`, mk.ID)
		if err != nil {
			continue
		}
		for rows.Next() {
			var o string
			var v float64
			if rows.Scan(&o, &v) == nil {
				total += v
				if o == favSide {
					fav += v
				}
			}
		}
		rows.Close()
	}
	if total == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", 100*fav/total)
}

// clashESP adds the tab to the Rolebot sidebar section and registers its page.
// Built only with the mindclash tag; clash_off.go is the other half.
func (m *Mod) clashESP() {
	for i := range esp.ModuleNav {
		if esp.ModuleNav[i].Title == "Rolebot" {
			esp.ModuleNav[i].Links = append(esp.ModuleNav[i].Links,
				esp.ModuleNavLink{Label: "Mind Clash", Path: "/rolebots/clash", Page: "rolebot-clash"})
			break
		}
	}
	esp.ModulePages["rolebots_clash.html"] = tmplClash
}
