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

// /rolebots/summit: the Summit desk (summit.go). It shows the dials, the queue of beats waiting with their heat,
// the stories and threads, the last posts, and why beats were not posted. "Post next now" runs one pass past the
// day and hour gates. This page is the only place a post is shown as a bot's; the board, thread pages and core
// /forum page carry no marker.

func (m *Mod) handleSummitBoard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	self := "/rolebots/summit"
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		switch r.FormValue("action") {
		case "settings":
			if err := m.saveSummitSettings(ctx, r); err != nil {
				redirect(w, r, self, "", err.Error())
				return
			}
			redirect(w, r, self, "settings saved", "")
		case "post":
			if !summitOn() {
				redirect(w, r, self, "", "the desk is switched off")
				return
			}
			if !summitBusy.CompareAndSwap(false, true) {
				redirect(w, r, self, "", "a pass is already running")
				return
			}
			res := m.summitPass(ctx, true)
			summitBusy.Store(false)
			redirect(w, r, self, res, "")
		case "sweep":
			m.summitSweeps(ctx)
			redirect(w, r, self, "sweeps run", "")
		default:
			redirect(w, r, self, "", "unknown action")
		}
		return
	}

	d := summitDialsNow()
	now := clock.Unix()

	type beatRow struct {
		Kind, Story, Due, Expires string
		Heat                      string
		Opens                     bool
	}
	var queue []beatRow
	if bs, err := m.summitWaitingBeats(ctx, now); err == nil {
		for _, b := range bs {
			due := "now"
			if b.NotBefore > now {
				due = "in " + summitAgo(b.NotBefore-now)
			}
			queue = append(queue, beatRow{Kind: b.Kind, Story: b.StoryKey, Due: due,
				Expires: "in " + summitAgo(b.ExpiresAt-now),
				Heat:    fmt.Sprintf("%.0f", summitEffectiveHeat(b.Heat, b.CreatedAt, now)), Opens: b.Opens})
		}
	}

	type storyRow struct {
		Key, Category, State, Last string
		ThreadID                   int64
		Posts                      int
	}
	var stories []storyRow
	if rows, err := m.db.QueryContext(ctx, `
		SELECT storyKey, category, state, threadID, posts, lastPostAt FROM rolebot_summit_stories
		 ORDER BY lastPostAt DESC LIMIT 60`); err == nil {
		for rows.Next() {
			var s storyRow
			var last int64
			if rows.Scan(&s.Key, &s.Category, &s.State, &s.ThreadID, &s.Posts, &last) == nil {
				s.Last = summitAgo(now-last) + " ago"
				stories = append(stories, s)
			}
		}
		rows.Close()
	}

	type postRow struct {
		Ago, Author, Kind, Story, Excerpt string
		ThreadID                          int64
		New                               bool
	}
	var posts []postRow
	if rows, err := m.db.QueryContext(ctx, `
		SELECT at, characterID, kind, storyKey, threadID, newThread, excerpt FROM rolebot_summit_posts
		 ORDER BY at DESC LIMIT 100`); err == nil {
		type raw struct {
			at, char, thread int64
			kind, story, ex  string
			nt               int
		}
		var rs []raw
		for rows.Next() {
			var x raw
			if rows.Scan(&x.at, &x.char, &x.kind, &x.story, &x.thread, &x.nt, &x.ex) == nil {
				rs = append(rs, x)
			}
		}
		rows.Close()
		for _, x := range rs {
			posts = append(posts, postRow{Ago: summitAgo(now-x.at) + " ago", Author: m.charNameOr(ctx, x.char),
				Kind: x.kind, Story: x.story, ThreadID: x.thread, New: x.nt != 0, Excerpt: x.ex})
		}
	}

	today := len(summitMarksSince(summitDayStart(clock.Now())))

	type kv struct {
		K string
		V int64
	}
	var drops []kv
	for k, v := range summitDropCounts() {
		drops = append(drops, kv{k, v})
	}
	sort.Slice(drops, func(i, j int) bool { return drops[i].K < drops[j].K })

	type shareRow struct {
		Cat   string
		Share int
	}
	var shares []shareRow
	for _, c := range summitCategories {
		shares = append(shares, shareRow{c, d.Share[c]})
	}
	lines := 0
	for _, ls := range summitBank {
		lines += len(ls)
	}

	espRender(w, "rolebots_summit.html", map[string]any{
		"Page":      "rolebot-chatter",
		"Tab":       "summit",
		"Msg":       r.URL.Query().Get("msg"),
		"Err":       r.URL.Query().Get("err"),
		"Master":    Master(),
		"D":         d,
		"Shares":    shares,
		"Today":     today,
		"Target":    fmt.Sprintf("%.0f", summitDayTarget(clock.Now(), d.PerDay)),
		"Idle":      summitIdle(now, d),
		"LastHuman": summitAgo(now-summitHumanAt.Load()) + " ago",
		"Queue":     queue,
		"Stories":   stories,
		"Posts":     posts,
		"Drops":     drops,
		"Lines":     lines,
		"ThreadCap": summitThreadPerDay,
	})
}

// summitWaitingBeats is every beat not yet posted or dropped, hottest first.
func (m *Mod) summitWaitingBeats(ctx context.Context, now int64) ([]summitBeatRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT beatID, storyKey, kind, heat, opens, notBefore, expiresAt, createdAt
		  FROM rolebot_summit_beats
		 WHERE postedAt = 0 AND dropped = 0 AND expiresAt > ?
		 ORDER BY heat DESC LIMIT 60`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []summitBeatRow
	for rows.Next() {
		var b summitBeatRow
		var opens int
		if rows.Scan(&b.ID, &b.StoryKey, &b.Kind, &b.Heat, &opens, &b.NotBefore, &b.ExpiresAt, &b.CreatedAt) == nil {
			b.Opens = opens != 0
			out = append(out, b)
		}
	}
	sortSummitBeats(out, now)
	return out, rows.Err()
}

func (m *Mod) saveSummitSettings(ctx context.Context, r *http.Request) error {
	dial := func(field string, def, min, max int) int {
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(field)))
		if err != nil || n < min {
			return def
		}
		if max > 0 && n > max {
			return max
		}
		return n
	}
	d := summitDialsNow()
	d.On = r.FormValue("on") == "on"
	d.NameCapsuleers = r.FormValue("nameCapsuleers") == "on"
	d.PerDay = dial("perDay", defaultSummitPerDay, 0, 1000)
	d.NewThreads = dial("newThreads", defaultSummitNewThreads, 0, 100)
	d.ThreadGapMin = dial("threadGapMin", defaultSummitThreadGapMin, 0, 1440)
	d.AuthorPerDay = dial("authorPerDay", defaultSummitAuthorPerDay, 1, 100)
	d.IdleDays = dial("idleDays", defaultSummitIdleDays, 0, 365)
	d.RepeatDays = dial("repeatDays", defaultSummitRepeatDays, 0, 365)
	share := map[string]int{}
	for _, c := range summitCategories {
		share[c] = dial("share_"+c, summitDefaultShare[c], 0, 100)
	}
	d.Share = share
	bit := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	kv := [][2]string{
		{settingSummitOn, bit(d.On)},
		{settingSummitNameCaps, bit(d.NameCapsuleers)},
		{settingSummitPerDay, strconv.Itoa(d.PerDay)},
		{settingSummitNewThreads, strconv.Itoa(d.NewThreads)},
		{settingSummitThreadGapMin, strconv.Itoa(d.ThreadGapMin)},
		{settingSummitAuthorPerDay, strconv.Itoa(d.AuthorPerDay)},
		{settingSummitIdleDays, strconv.Itoa(d.IdleDays)},
		{settingSummitRepeatDays, strconv.Itoa(d.RepeatDays)},
	}
	for _, c := range summitCategories {
		kv = append(kv, [2]string{settingSummitSharePrefix + c, strconv.Itoa(share[c])})
	}
	for _, p := range kv {
		if err := evedb.SetServerSetting(ctx, m.db, p[0], p[1]); err != nil {
			return err
		}
	}
	setSummitDials(d)
	return nil
}

// summitAgo is a duration in seconds as a short phrase.
func summitAgo(sec int64) string {
	switch {
	case sec < 0:
		sec = 0
		fallthrough
	case sec < 120:
		return fmt.Sprintf("%ds", sec)
	case sec < 7200:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 2*86400:
		return fmt.Sprintf("%dh", sec/3600)
	}
	return fmt.Sprintf("%dd", sec/86400)
}

const tmplSummit = chatterTabs + `{{define "content"}}` + castTabsStyle + `
<h2>Summit Desk</h2>
{{template "rb_chattabs" .}}
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}
{{if not .Master}}<div class="alert alert-error">The rolebot master switch is off; nothing is posted while it is.</div>{{end}}
{{if .Idle}}<div class="alert alert-error">Idle: no capsuleer has logged in for {{.D.IdleDays}} day(s) (last {{.LastHuman}}). The desk waits.</div>{{end}}
<div class="card">
  <h3>Switches &amp; dials</h3>
  <p style="font-size:12px;color:#6b7280">Bots post the drama of their world on the Intergalactic Summit forum: one thread per war, alliance,
  pact, recruitment ad or shop, and every later event a reply in it. Posts read as any capsuleer's; nothing on the board marks them.
  The desk posts at most one beat per 5&nbsp;min pass, the hottest the budget allows. {{.Lines}} lines loaded.</p>
  <p style="font-size:12px">Today: <b>{{.Today}}</b> posted, curve target by now <b>{{.Target}}</b> of {{.D.PerDay}}. Last capsuleer login {{.LastHuman}}.</p>
  <form method="POST">
    <input type="hidden" name="action" value="settings">
    <table class="tbl" style="font-size:12px">
      <tr><td><label><input type="checkbox" name="on" {{if .D.On}}checked{{end}}> <b>Desk on</b></label></td>
          <td>Off, nothing is filed or posted.</td></tr>
      <tr><td>Posts per day</td><td><input name="perDay" value="{{.D.PerDay}}" size="4"> &nbsp;(spread over the day, busiest in the evening, server time)</td></tr>
      <tr><td>New threads per day</td><td><input name="newThreads" value="{{.D.NewThreads}}" size="4"> &nbsp;(the rest are replies in existing threads)</td></tr>
      <tr><td>Minutes between posts in one thread</td><td><input name="threadGapMin" value="{{.D.ThreadGapMin}}" size="4"> &nbsp;(and at most {{.ThreadCap}} a day in one thread)</td></tr>
      <tr><td>Posts per author per day</td><td><input name="authorPerDay" value="{{.D.AuthorPerDay}}" size="4"></td></tr>
      <tr><td><label><input type="checkbox" name="nameCapsuleers" {{if .D.NameCapsuleers}}checked{{end}}> Name capsuleers</label></td>
          <td>A capsuleer on the killmail of a bot that dies in a bot war is named in that war's thread. Bots never reply to a person's post either way.</td></tr>
      <tr><td>Idle after (days)</td><td><input name="idleDays" value="{{.D.IdleDays}}" size="4"> &nbsp;(no capsuleer login for this long and the desk waits; 0 = never idle)</td></tr>
      <tr><td>Title and opener repeat block (days)</td><td><input name="repeatDays" value="{{.D.RepeatDays}}" size="4"></td></tr>
      <tr><th colspan="2" style="text-align:left;padding-top:10px">Category share of the day (%, 0 = off)</th></tr>
      {{range .Shares}}<tr><td>{{.Cat}}</td><td><input name="share_{{.Cat}}" value="{{.Share}}" size="4"></td></tr>{{end}}
    </table>
    <button type="submit">save</button>
  </form>
  <form method="POST" style="display:inline"><input type="hidden" name="action" value="post"><button type="submit">post next now</button></form>
  <form method="POST" style="display:inline"><input type="hidden" name="action" value="sweep"><button type="submit">run sweeps (tallies, recruitment)</button></form>
</div>
<div class="card">
  <h3>Waiting beats</h3>
  {{if .Queue}}<table class="tbl" style="font-size:12px">
    <tr><th>Heat</th><th>Kind</th><th>Story</th><th>Due</th><th>Expires</th></tr>
    {{range .Queue}}<tr><td>{{.Heat}}</td><td><code>{{.Kind}}</code>{{if .Opens}} (opens){{end}}</td><td>{{.Story}}</td><td>{{.Due}}</td><td>{{.Expires}}</td></tr>{{end}}
  </table>{{else}}<p>Nothing waiting.</p>{{end}}
</div>
<div class="card">
  <h3>Stories</h3>
  {{if .Stories}}<table class="tbl" style="font-size:12px">
    <tr><th>Story</th><th>Category</th><th>Thread</th><th>Posts</th><th>Last post</th><th>State</th></tr>
    {{range .Stories}}<tr><td>{{.Key}}</td><td>{{.Category}}</td><td><a href="/forum/thread/{{.ThreadID}}">#{{.ThreadID}}</a></td><td>{{.Posts}}</td><td>{{.Last}}</td><td>{{.State}}</td></tr>{{end}}
  </table>{{else}}<p>No stories yet.</p>{{end}}
</div>
<div class="card">
  <h3>Last posts</h3>
  {{if .Posts}}<table class="tbl" style="font-size:12px">
    <tr><th>When</th><th>Author</th><th>Kind</th><th>Thread</th><th>First line</th></tr>
    {{range .Posts}}<tr><td>{{.Ago}}</td><td>{{.Author}}</td><td><code>{{.Kind}}</code>{{if .New}} (new){{end}}</td><td><a href="/forum/thread/{{.ThreadID}}">#{{.ThreadID}}</a></td><td>{{.Excerpt}}</td></tr>{{end}}
  </table>{{else}}<p>Nothing posted yet.</p>{{end}}
</div>
<div class="card">
  <h3>Not posted, and why</h3>
  {{if .Drops}}<table class="tbl" style="font-size:12px">
    <tr><th>Gate</th><th>Count since boot</th></tr>
    {{range .Drops}}<tr><td><code>{{.K}}</code></td><td>{{.V}}</td></tr>{{end}}
  </table>
  <p style="font-size:12px;color:#6b7280">day/hour: budget; newthread: new-thread cap; threadgap/threadday: one thread's pace;
  author/twice: one author's pace; category: a category's share; idle: no capsuleer lately; author (dropped): no bot to write it;
  render: no line fits its facts; closed: the thread was locked or deleted; stale: expired waiting; killqueue: kill feed overflow.</p>
  {{else}}<p>Nothing refused yet.</p>{{end}}
</div>
{{end}}`
