package rolebots

// chatterTabs is the sub-tab strip of the two Chatter pages (corp and Local chat, Summit Desk), with the
// same rules and look as castTabs (esptabs.go). It reads .Tab, not .Page: both pages render .Page
// "rolebot-chatter" so the one sidebar entry stays lit. The render test guards a missing define.
const chatterTabs = `{{define "rb_chattabs"}}
<div class="rb-subtabs">
  <a href="/rolebots/chatter"{{if eq .Tab "chatter"}} class="on"{{end}}>Corp &amp; Local Chat</a>
  <a href="/rolebots/summit"{{if eq .Tab "summit"}} class="on"{{end}}>Summit Desk</a>
</div>
{{end}}`
