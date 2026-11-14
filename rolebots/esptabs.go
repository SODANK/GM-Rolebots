package rolebots

// castTabs is the sub-tab strip of the Bot Population pages, concatenated into each page's source
// because module pages parse independently (espcast_render_test.go guards a missing define). It reads
// .Tab, not .Page: every page renders .Page "rolebots" so the one sidebar entry stays lit.
const castTabs = `{{define "rb_casttabs"}}
<div class="rb-subtabs">
  <a href="/rolebots"{{if eq .Tab "cast"}} class="on"{{end}}>Main Cast</a>
  <a href="/rolebots/seed"{{if eq .Tab "gen"}} class="on"{{end}}>RoleBot Generation</a>
  <a href="/rolebots/generated"{{if eq .Tab "pop"}} class="on"{{end}}>Generated Characters</a>
  <a href="/rolebots/skills"{{if eq .Tab "skills"}} class="on"{{end}}>Skills</a>
  <a href="/rolebots/timezones"{{if eq .Tab "tz"}} class="on"{{end}}>Timezones</a>
</div>
{{end}}`

// castTabsStyle is its chrome, concatenated per page, with the same look as the trade strip's.
const castTabsStyle = `<style>
.rb-subtabs { display:flex; gap:4px; margin:0 0 12px; border-bottom:1px solid #2a1a1a;
              flex-wrap:wrap; }
.rb-subtabs a { padding:6px 13px; font-size:12px; color:#9ca3af; text-decoration:none;
                border:1px solid transparent; border-bottom:none;
                border-radius:5px 5px 0 0; margin-bottom:-1px; }
.rb-subtabs a:hover { color:#e6ded6; background:#180f0f; }
.rb-subtabs a.on { color:#f0e6dc; background:#140d0d; border-color:#2a1a1a;
                   border-bottom:1px solid #140d0d; }
</style>`
