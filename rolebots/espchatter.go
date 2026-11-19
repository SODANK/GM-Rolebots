package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// /rolebots/chatter: the switches and dials, the kinds and their mutes, who is in which channel now, the last
// hundred lines said, and why lines were not said. A test line lets an operator check the pipe before enabling
// it for a corporation's members.

func (m *Mod) handleChatterBoard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	self := "/rolebots/chatter"
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		switch r.FormValue("action") {
		case "settings":
			m.handleChatterSettingsPost(ctx, w, r, self)
		case "kinds":
			var off []string
			for k := range chatterKinds {
				if r.FormValue("mute_"+k) == "on" {
					off = append(off, k)
				}
			}
			for k := range chatterExchangeKinds {
				if r.FormValue("mute_"+k) == "on" {
					off = append(off, k)
				}
			}
			sort.Strings(off)
			csv := strings.Join(off, ",")
			if err := evedb.SetServerSetting(ctx, m.db, settingChatterKindsOff, csv); err != nil {
				redirect(w, r, self, "", err.Error())
				return
			}
			setChatterKindsOff(csv)
			redirect(w, r, self, fmt.Sprintf("%d kind(s) muted", len(off)), "")
		case "test":
			corpID, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("corpID")), 10, 64)
			if corpID == 0 {
				corpID, _ = strconv.ParseInt(strings.TrimSpace(r.FormValue("corpPick")), 10, 64)
			}
			if corpID == 0 {
				redirect(w, r, self, "", "pick a corporation or an alliance")
				return
			}
			if err := m.chatterTest(ctx, corpID, strings.TrimSpace(r.FormValue("kind"))); err != nil {
				redirect(w, r, self, "", err.Error())
				return
			}
			redirect(w, r, self, "test line sent", "")
		default:
			redirect(w, r, self, "", "unknown action")
		}
		return
	}

	now := clock.Now()
	nowU := now.Unix()

	// Kinds, with what the ring says about each.
	said24 := map[string]int{}
	recent := chatterRecent()
	for _, s := range recent {
		if nowU-s.At < 86400 {
			said24[s.Kind]++
		}
	}
	type kindRow struct {
		Kind, Label, Class, Cooldown string
		Priority, Said24             int
		Muted, Wired                 bool
	}
	var kinds []kindRow
	for _, k := range chatterKindList() {
		cd := "—"
		if k.Cooldown > 0 {
			cd = k.Cooldown.String()
		}
		kinds = append(kinds, kindRow{Kind: k.Kind, Label: k.Label, Class: k.Class, Cooldown: cd,
			Priority: k.Priority, Said24: said24[k.Kind], Muted: chatterKindOff(k.Kind), Wired: k.Wired})
	}
	// The exchange families the alliance channel and the banter lane say.
	var xk []chatterExchangeKind
	for _, k := range chatterExchangeKinds {
		xk = append(xk, k)
	}
	sort.Slice(xk, func(i, j int) bool {
		if xk[i].Class != xk[j].Class {
			return xk[i].Class < xk[j].Class
		}
		return xk[i].Kind < xk[j].Kind
	})
	for _, k := range xk {
		cd := "—"
		if k.Cooldown > 0 {
			cd = k.Cooldown.String()
		}
		kinds = append(kinds, kindRow{Kind: k.Kind, Label: k.Label, Class: k.Class, Cooldown: cd,
			Said24: said24[k.Kind], Muted: chatterKindOff(k.Kind), Wired: true})
	}

	// Channels with a person in them.
	offers := chatterOfferRows(nowU)
	type audRow struct {
		CorpID                 int64
		Alliance               bool
		CorpName, Humans       string
		Bots, SaidHour, Queued int
		OfferSecs              int64
	}
	var auds []audRow
	for _, a := range chatterAudienceRows() {
		name := m.corpNameOf(ctx, a.CorpID)
		if a.Alliance {
			name = ownerNameOf(ctx, m.db, a.CorpID)
		}
		auds = append(auds, audRow{
			CorpID: a.CorpID, Alliance: a.Alliance, CorpName: name, Humans: strings.Join(a.Humans, ", "),
			Bots: a.Bots, SaidHour: chatterBook.saidLastHour(a.CorpID, now), Queued: chatterQ.depth(a.CorpID),
			OfferSecs: offers[a.CorpID],
		})
	}

	type recentRow struct {
		Ago, CorpName, Where, Speaker, Kind, Text string
	}
	var lines []recentRow
	corpNames := map[int64]string{}
	sysNames := map[int64]string{}
	for _, s := range recent {
		name, ok := corpNames[s.CorpID]
		if !ok {
			name = m.corpNameOf(ctx, s.CorpID)
			corpNames[s.CorpID] = name
		}
		where := "Corp"
		switch {
		case s.ChannelID >= 30_000_000 && s.ChannelID < 40_000_000:
			// Local: the channel is the solar system (chatter_voice.go).
			sys, ok := sysNames[s.ChannelID]
			if !ok {
				sys = m.nameOfSystem(ctx, s.ChannelID)
				sysNames[s.ChannelID] = sys
			}
			where = "Local " + sys
		case isRegionChannel(s.ChannelID):
			where = "Region"
		case s.ChannelID != 0 && s.ChannelID != s.CorpID:
			// Not the speaker's corporation, not Local, not a region: the
			// speaker's alliance (chatter_alliance.go).
			where = "Alliance"
		}
		lines = append(lines, recentRow{Ago: agoString(nowU - s.At), CorpName: name, Where: where, Speaker: s.Speaker, Kind: s.Kind, Text: s.Text})
	}

	type kv struct {
		K string
		V int64
	}
	var drops []kv
	for k, v := range chatterDrops() {
		drops = append(drops, kv{k, v})
	}
	sort.Slice(drops, func(i, j int) bool { return drops[i].K < drops[j].K })

	espRender(w, "rolebots_chatter.html", map[string]any{
		"Page":      "rolebot-chatter",
		"Tab":       "chatter",
		"Msg":      r.URL.Query().Get("msg"),
		"Err":       r.URL.Query().Get("err"),
		"On":        Chatter(),
		"Greet":     chatterGreetOn.Load(),
		"Replies":   chatterRepliesOn.Load(),
		"Farewell":  chatterFarewell.Load(),
		"PerHour":   chatterPerHour.Load(),
		"PilotGap":  chatterPilotGap.Load(),
		"MinGap":    chatterMinGap.Load(),
		"Reserve":   chatterGreetReserve,
		"Kinds":     kinds,
		"Audiences": auds,
		"Recent":    lines,
		"Drops":     drops,
		"Master":    Master(),
		"Mirror":    FleetMirror(),

		"Local":       chatterLocalOn.Load(),
		"Race":        chatterRaceOn.Load(),
		"CorpTalk":    chatterCorpOn.Load(),
		"Retorts":     chatterRetortsOn.Load(),
		"LocalPerMin": chatterLocalRate.Load(),
		"SpeakerGap":  chatterGapSec.Load(),
		"Bystander":   chatterBystander.Load(),
		"ReplyPct":    chatterReplyPct.Load(),
		"VoiceLines":  chatterVoiceLineCount(),

		"Market":          chatterMarketOn.Load(),
		"MarketRegion":    chatterMarketRegionOn.Load(),
		"MarketAdPct":     chatterMarketAdPct.Load(),
		"MarketRemarkPct": chatterMarketRemarkPct.Load(),
		"MarketLookMin":   chatterMarketLookMin.Load(),
		"MarketLocalGap":  int(marketLocalGap / time.Minute),
		"MarketRegionGap": int(marketRegionGap / time.Minute),
		"MarketAdMin":     iskWords(marketAdMinISK),
		"MarketGouge":     marketGougeRatio,
		"MarketBargain":   marketBargainRatio,

		"AllianceTalk":   chatterAllianceOn.Load(),
		"AlliancePerHr":  chatterAlliancePerHr.Load(),
		"AllianceGreet":  chatterAllianceGreetOn.Load(),
		"BanterLocal":    chatterBanterLocalOn.Load(),
		"BanterAlliance": chatterBanterAllyOn.Load(),
		"BanterMin":      chatterBanterMin.Load(),
		"RibPct":         chatterRibPct.Load(),
		"ShopPct":        chatterShopPct.Load(),
	})
}

// chatterVoiceLineCount is how many exchange templates loaded
// (chatter_lines_*.go).
func chatterVoiceLineCount() int {
	n := 0
	for _, l := range chatPools {
		n += len(l)
	}
	return n
}

// handleChatterSettingsPost saves the switches and dials and applies them.
func (m *Mod) handleChatterSettingsPost(ctx context.Context, w http.ResponseWriter, r *http.Request, self string) {
	bit := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	dial := func(field string, def, min int64) int64 {
		n, err := strconv.ParseInt(strings.TrimSpace(r.FormValue(field)), 10, 64)
		if err != nil || n < min {
			return def
		}
		return n
	}
	on := r.FormValue("on") == "on"
	greet := r.FormValue("greet") == "on"
	replies := r.FormValue("replies") == "on"
	farewell := r.FormValue("farewell") == "on"
	perHour := dial("perHour", defaultChatterPerHour, 0)
	pilotGap := dial("pilotGap", defaultChatterPilotGap, 0)
	minGap := dial("minGap", defaultChatterMinGap, 0)
	local := r.FormValue("local") == "on"
	race := r.FormValue("race") == "on"
	corp := r.FormValue("corp") == "on"
	retorts := r.FormValue("retorts") == "on"
	localRate := dial("localPerMin", defaultChatterLocalRate, 1)
	gap := dial("speakerGap", defaultChatterGapSec, 0)
	bystander := int64(clampPct(int(dial("bystander", defaultChatterBystander, 0))))
	reply := int64(clampPct(int(dial("replyPct", defaultChatterReply, 0))))
	market := r.FormValue("market") == "on"
	marketRegion := r.FormValue("marketRegion") == "on"
	adPct := int64(clampPct(int(dial("marketAdPct", defaultMarketAdPct, 0))))
	remarkPct := int64(clampPct(int(dial("marketRemarkPct", defaultMarketRemarkPct, 0))))
	lookMin := dial("marketLookMin", defaultMarketLookMin, 1)
	allianceTalk := r.FormValue("allianceTalk") == "on"
	alliancePerHr := dial("alliancePerHr", defaultChatterAlliancePerHr, 0)
	allianceGreet := r.FormValue("allianceGreet") == "on"
	banterLocal := r.FormValue("banterLocal") == "on"
	banterAlly := r.FormValue("banterAlliance") == "on"
	banterMin := dial("banterMin", defaultChatterBanterMin, 1)
	ribPct := int64(clampPct(int(dial("ribPct", defaultChatterRibPct, 0))))
	shopPct := int64(clampPct(int(dial("shopPct", defaultChatterShopPct, 0))))
	for _, kv := range [][2]string{
		{settingChatterAlliance, bit(allianceTalk)},
		{settingChatterAlliancePerHr, strconv.FormatInt(alliancePerHr, 10)},
		{settingChatterAllianceGreet, bit(allianceGreet)},
		{settingChatterBanterLocal, bit(banterLocal)},
		{settingChatterBanterAlly, bit(banterAlly)},
		{settingChatterBanterMin, strconv.FormatInt(banterMin, 10)},
		{settingChatterRibPct, strconv.FormatInt(ribPct, 10)},
		{settingChatterShopPct, strconv.FormatInt(shopPct, 10)},
		{settingChatterMarket, bit(market)},
		{settingChatterMarketRegion, bit(marketRegion)},
		{settingChatterMarketAdPct, strconv.FormatInt(adPct, 10)},
		{settingChatterMarketRemarkPct, strconv.FormatInt(remarkPct, 10)},
		{settingChatterMarketLookMin, strconv.FormatInt(lookMin, 10)},
		{settingChatterOn, bit(on)},
		{settingChatterGreet, bit(greet)},
		{settingChatterReplies, bit(replies)},
		{settingChatterFarewell, bit(farewell)},
		{settingChatterPerHour, strconv.FormatInt(perHour, 10)},
		{settingChatterPilotGap, strconv.FormatInt(pilotGap, 10)},
		{settingChatterMinGap, strconv.FormatInt(minGap, 10)},
		{settingChatterLocal, bit(local)},
		{settingChatterRace, bit(race)},
		{settingChatterCorp, bit(corp)},
		{settingChatterRetorts, bit(retorts)},
		{settingChatterLocalRate, strconv.FormatInt(localRate, 10)},
		{settingChatterGap, strconv.FormatInt(gap, 10)},
		{settingChatterBystander, strconv.FormatInt(bystander, 10)},
		{settingChatterReply, strconv.FormatInt(reply, 10)},
	} {
		if err := evedb.SetServerSetting(ctx, m.db, kv[0], kv[1]); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
	}
	setChatterOn(on)
	setChatterGreet(greet)
	setChatterReplies(replies)
	setChatterFarewell(farewell)
	setChatterPerHour(int(perHour))
	setChatterPilotGap(int(pilotGap))
	setChatterMinGap(int(minGap))
	setChatterLocal(local)
	setChatterRace(race)
	setChatterCorp(corp)
	setChatterRetorts(retorts)
	setChatterLocalRate(int(localRate))
	setChatterGap(int(gap))
	setChatterBystander(int(bystander))
	setChatterReply(int(reply))
	setChatterMarket(market)
	setChatterMarketRegion(marketRegion)
	setChatterMarketAdPct(int(adPct))
	setChatterMarketRemarkPct(int(remarkPct))
	setChatterMarketLookMin(int(lookMin))
	setChatterAlliance(allianceTalk)
	setChatterAlliancePerHr(int(alliancePerHr))
	setChatterAllianceGreet(allianceGreet)
	setChatterBanterLocal(banterLocal)
	setChatterBanterAlly(banterAlly)
	setChatterBanterMin(int(banterMin))
	setChatterRibPct(int(ribPct))
	setChatterShopPct(int(shopPct))
	redirect(w, r, self, "chatter settings saved", "")
}

const tmplChatter = chatterTabs + `{{define "content"}}` + castTabsStyle + `
<h2>Chatter</h2>
{{template "rb_chattabs" .}}
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}
{{if not .Master}}<div class="alert alert-error">The rolebot master switch is off; nothing is said while it is.</div>{{end}}
<div class="card">
  <h3>Switches &amp; dials</h3>
  <p style="font-size:12px;color:#6b7280">Bots speak only in channels a <b>person</b> is connected to.</p>
  <form method="POST">
    <input type="hidden" name="action" value="settings">
    <table class="tbl" style="font-size:12px">
      <tr><td><label><input type="checkbox" name="on" {{if .On}}checked{{end}}> <b>Chatter on</b></label></td>
          <td>Master switch. Off, nothing is queued or said.</td></tr>
      <tr><td><label><input type="checkbox" name="greet" {{if .Greet}}checked{{end}}> Greet a person who appears</label></td>
          <td>The CEO, or the most leaderly bot, greets them once per 6&nbsp;h.</td></tr>
      <tr><td><label><input type="checkbox" name="replies" {{if .Replies}}checked{{end}}> Reply to people</label></td>
          <td>Answers o7, hi, gf and thanks, and bots addressed by name. An <i>x</i> after a fleet announce sends an invite.</td></tr>
      <tr><td><label><input type="checkbox" name="farewell" {{if .Farewell}}checked{{end}}> o7 when a person leaves</label></td>
          <td>Off by default.</td></tr>
      <tr><td>Lines per channel per hour</td><td><input name="perHour" value="{{.PerHour}}" size="4"> &nbsp;(events leave the last {{.Reserve}} for greetings and replies)</td></tr>
      <tr><td>Seconds between any two lines in a channel</td><td><input name="minGap" value="{{.MinGap}}" size="4"> &nbsp;(direct replies wait a third of this)</td></tr>
      <tr><td>Seconds between one pilot's own lines</td><td><input name="pilotGap" value="{{.PilotGap}}" size="4"></td></tr>
      <tr><th colspan="2" style="text-align:left;padding-top:10px">Exchanges &mdash; bots answering each other ({{.VoiceLines}} lines loaded)</th></tr>
      <tr><td><label><input type="checkbox" name="local" {{if .Local}}checked{{end}}> Speak in Local</label></td>
          <td>Bots comment on locks and kills, and victims and bystanders answer. Only where a person is in Local.</td></tr>
      <tr><td><label><input type="checkbox" name="race" {{if .Race}}checked{{end}}> Race insults</label></td>
          <td>Xenophobes insult other races. Off, they only say they dislike a people.</td></tr>
      <tr><td><label><input type="checkbox" name="corp" {{if .CorpTalk}}checked{{end}}> Conversations in corp chat</label></td>
          <td>Directors, members and corpmates talk about fleet orders, calls for help, losses and work-board claims. Uses the hourly budget above.</td></tr>
      <tr><td><label><input type="checkbox" name="retorts" {{if .Retorts}}checked{{end}}> Answer players in Local</label></td>
          <td>A bot replies when a person answers it, links it or names it. One reply per 2&nbsp;min.</td></tr>
      <tr><td>Local lines per minute per system</td><td><input name="localPerMin" value="{{.LocalPerMin}}" size="4"></td></tr>
      <tr><td>Seconds between one pilot's own lines in Local</td><td><input name="speakerGap" value="{{.SpeakerGap}}" size="4"> &nbsp;(conversation lines skip it)</td></tr>
      <tr><td>Bystander remarks (%)</td><td><input name="bystander" value="{{.Bystander}}" size="4"></td></tr>
      <tr><td>Victim replies (%)</td><td><input name="replyPct" value="{{.ReplyPct}}" size="4"></td></tr>
      <tr><th colspan="2" style="text-align:left;padding-top:10px">Market talk &mdash; Local and region channels</th></tr>
      <tr><td><label><input type="checkbox" name="market" {{if .Market}}checked{{end}}> Talk about the market</label></td>
          <td>Bots announce listings, remark on gouged prices and market spikes, and answer people who reply. Needs &ldquo;Speak in Local&rdquo;.</td></tr>
      <tr><td><label><input type="checkbox" name="marketRegion" {{if .MarketRegion}}checked{{end}}> Region channel too</label></td>
          <td>If nobody is in Local but a person is in the region channel, the line goes there (one per {{.MarketRegionGap}}&nbsp;min per region; Local one per {{.MarketLocalGap}}&nbsp;min).</td></tr>
      <tr><td>Listings announced (%)</td><td><input name="marketAdPct" value="{{.MarketAdPct}}" size="4"> &nbsp;(never under {{.MarketAdMin}}; never ore or materials)</td></tr>
      <tr><td>Purchases remarked on (%)</td><td><input name="marketRemarkPct" value="{{.MarketRemarkPct}}" size="4"> &nbsp;(only past {{.MarketGouge}}&times; or under {{.MarketBargain}}&times; the two-week regional average)</td></tr>
      <tr><td>Minutes between market looks per channel</td><td><input name="marketLookMin" value="{{.MarketLookMin}}" size="4"></td></tr>
      <tr><th colspan="2" style="text-align:left;padding-top:10px">Alliance channel &mdash; member corporations talking to each other</th></tr>
      <tr><td><label><input type="checkbox" name="allianceTalk" {{if .AllianceTalk}}checked{{end}}> Speak in alliance channels</label></td>
          <td>Calls for help, reinforced towers and alliance politics (new members, votes, bills, wars, systems). Allies are never insulted.</td></tr>
      <tr><td>Alliance lines per channel per hour</td><td><input name="alliancePerHr" value="{{.AlliancePerHr}}" size="4"> &nbsp;(shared by all member corporations)</td></tr>
      <tr><td><label><input type="checkbox" name="allianceGreet" {{if .AllianceGreet}}checked{{end}}> Greet in the alliance channel</label></td>
          <td>The executor's CEO or most leaderly bot, unless already greeted in corp chat within the hour.</td></tr>
      <tr><td>Remarks on another member's loss or kill (%)</td><td><input name="ribPct" value="{{.RibPct}}" size="4"> &nbsp;(condolences past 50M, teasing under)</td></tr>
      <tr><td>Shop talk across the alliance (%)</td><td><input name="shopPct" value="{{.ShopPct}}" size="4"> &nbsp;(listings mentioned to the alliance)</td></tr>
      <tr><th colspan="2" style="text-align:left;padding-top:10px">Banter &mdash; pilots of different peoples passing the time</th></tr>
      <tr><td><label><input type="checkbox" name="banterLocal" {{if .BanterLocal}}checked{{end}}> In Local</label></td>
          <td>Bots of different races chat where a person is in Local. Needs &ldquo;Speak in Local&rdquo;.</td></tr>
      <tr><td><label><input type="checkbox" name="banterAlliance" {{if .BanterAlliance}}checked{{end}}> In alliance channels</label></td>
          <td>The same between member corporations. Never insulting.</td></tr>
      <tr><td>Minutes between banter per channel</td><td><input name="banterMin" value="{{.BanterMin}}" size="4"> &nbsp;(&plusmn;30%)</td></tr>
    </table>
    <button type="submit">save</button>
  </form>
</div>
<div class="card">
  <h3>People in channel now</h3>
  {{if .Audiences}}
  <table class="tbl" style="font-size:12px">
    <tr><th>Channel</th><th>Bots present</th><th>People</th><th>Said this hour</th><th>Queued</th><th>Open fleet offer</th></tr>
    {{range .Audiences}}<tr>
      <td>{{if .Alliance}}<b>Alliance</b> {{.CorpName}}{{else}}<a href="/rolebots/corp/{{.CorpID}}">{{.CorpName}}</a>{{end}} <span style="color:#6b7280">{{.CorpID}}</span></td>
      <td>{{.Bots}}</td><td>{{.Humans}}</td><td>{{.SaidHour}}</td><td>{{.Queued}}</td>
      <td>{{if .OfferSecs}}{{.OfferSecs}}&nbsp;s left{{else}}&mdash;{{end}}</td></tr>{{end}}
  </table>
  {{else}}<p>No person is connected to a channel a bot sits in.</p>{{end}}
  <form method="POST" style="margin-top:8px">
    <input type="hidden" name="action" value="test">
    Say a test line as a bot of
    {{if .Audiences}}<select name="corpPick"><option value="">&mdash; a channel with a person in it &mdash;</option>{{range .Audiences}}<option value="{{.CorpID}}">{{.CorpName}}</option>{{end}}</select> or{{end}}
    corporation or alliance ID <input name="corpID" size="10">
    &nbsp;kind <select name="kind"><option value="">(plain test line)</option>{{range .Kinds}}<option value="{{.Kind}}">{{.Label}}</option>{{end}}</select>
    <button type="submit">say it</button>
    <span style="font-size:12px;color:#6b7280">Ignores the budget. Slots render as [slot].</span>
  </form>
</div>
<div class="card">
  <h3>Kinds</h3>
  <form method="POST">
    <input type="hidden" name="action" value="kinds">
    <table class="tbl" style="font-size:12px">
      <tr><th>Mute</th><th>Kind</th><th>What</th><th>Class</th><th>Priority</th><th>Cooldown per channel</th><th>Said 24h</th><th></th></tr>
      {{range .Kinds}}<tr{{if not .Wired}} style="color:#6b7280"{{end}}>
        <td><input type="checkbox" name="mute_{{.Kind}}" {{if .Muted}}checked{{end}}></td>
        <td><code>{{.Kind}}</code></td><td>{{.Label}}</td><td>{{.Class}}</td><td>{{.Priority}}</td><td>{{.Cooldown}}</td><td>{{.Said24}}</td>
        <td>{{if not .Wired}}not wired yet{{end}}</td></tr>{{end}}
    </table>
    <button type="submit">save mutes</button>
  </form>
  {{if not .Mirror}}<p style="font-size:12px;color:#eab308">Fleet seats (Gangs page) are off, so a fleet announce carries no "x up" offer.</p>{{end}}
</div>
<div class="card">
  <h3>Last {{len .Recent}} lines</h3>
  {{if .Recent}}
  <table class="tbl" style="font-size:12px">
    <tr><th>When</th><th>Where</th><th>Corporation</th><th>Speaker</th><th>Kind</th><th>Line</th></tr>
    {{range .Recent}}<tr><td>{{.Ago}}</td><td>{{.Where}}</td><td>{{.CorpName}}</td><td>{{.Speaker}}</td><td><code>{{.Kind}}</code></td><td>{{.Text}}</td></tr>{{end}}
  </table>
  {{else}}<p>Nothing said since the server started.</p>{{end}}
</div>
<div class="card">
  <h3>Not said, by reason</h3>
  {{if .Drops}}
  <table class="tbl" style="font-size:12px"><tr><th>Reason</th><th>Count</th></tr>
    {{range .Drops}}<tr><td><code>{{.K}}</code></td><td>{{.V}}</td></tr>{{end}}
  </table>
  <p style="font-size:12px;color:#6b7280"><code>off</code> switch off &middot; <code>replay</code> during a catch-up &middot; <code>noaudience</code> no person in that channel &middot;
  <code>muted</code> kind muted &middot; <code>queue.full</code>/<code>stale</code> backlog &middot; <code>speaker</code> pilot left the channel &middot;
  <code>hour</code>/<code>mingap</code>/<code>pilotgap</code>/<code>kind</code>/<code>repeat</code> budget limits &middot; <code>render</code> no wording fit &middot; <code>replygap</code> one person asking too fast &middot;
  <code>local</code> Local rate limit.</p>
  {{else}}<p>Nothing refused yet.</p>{{end}}
</div>
{{end}}`
