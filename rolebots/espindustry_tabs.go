package rolebots

// industryTabs is the sub-tab strip of the three Industry pages (projects, ore, minerals), with the same
// rules and look as castTabs (esptabs.go). It reads .Tab, not .Page: the pages share one sidebar entry,
// so all render .Page "rolebot-industry". The render tests guard a missing define.
const industryTabs = `{{define "rb_indtabs"}}
<div class="rb-subtabs">
  <a href="/rolebots/industry"{{if eq .Tab "pipeline"}} class="on"{{end}}>Pipeline</a>
  <a href="/rolebots/ore"{{if eq .Tab "ore"}} class="on"{{end}}>Ore</a>
  <a href="/rolebots/minerals"{{if eq .Tab "minerals"}} class="on"{{end}}>Minerals</a>
</div>
{{end}}`

// industryTabsStyle is its chrome, likewise concatenated per page.
const industryTabsStyle = `<style>
.rb-subtabs { display:flex; gap:4px; margin:0 0 12px; border-bottom:1px solid #2a1a1a;
              flex-wrap:wrap; }
.rb-subtabs a { padding:6px 13px; font-size:12px; color:#9ca3af; text-decoration:none;
                border:1px solid transparent; border-bottom:none;
                border-radius:5px 5px 0 0; margin-bottom:-1px; }
.rb-subtabs a:hover { color:#e6ded6; background:#180f0f; }
.rb-subtabs a.on { color:#f0e6dc; background:#140d0d; border-color:#2a1a1a;
                   border-bottom:1px solid #140d0d; }
</style>`
