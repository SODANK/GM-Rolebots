package rolebots

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gurimoon/esp"
)

// /corp/management (and the Orders page's old addresses): the desk of a human CEO and the division
// heads, shown to nobody else. What a bot CEO decides for its members is decided here, division by
// division: each order is a rolebot_corp_commands row the order desk carries out (corpcommanddesk.go)
// through the same posters a bot CEO uses, so it reaches bots and capsuleers the way a bot CEO's
// would. The CEO has every division, the corporation's own settings, the appointments and the
// characters; a division head has the one division and cannot see past it (corpscope.go).
//
// The portal key (portalkey.go) signs a pilot in once; the page then carries a short-lived token
// signed for that pilot, corporation and key. Rank and scope are read from the database on every
// request. Forms are GET forms outside any table, with a preview before anything is filed, the shape
// the in-game browser renders.

const (
	managePath = "/corp/management"
	// ordersPath is where the letters and the other portal pages send a chair for its key.
	ordersPath = managePath

	manageTokenSec = 30 * 60
	manageNonceSec = 2 * manageTokenSec

	// The pages that are no division.
	mgCorp   = "corp"
	mgHeads  = "heads"
	mgPeople = "people"
)

// ── the session ─────────────────────────────────────────────────────────────

// manageToken signs a pilot in until exp. issuedAt is the portal key's, so a new key retires every
// token the old one opened. Pure.
func manageToken(key []byte, charID, corpID, issuedAt, exp int64) string {
	return strconv.FormatInt(exp, 10) + "-" + signDesk(key, "manage", charID, corpID, issuedAt, exp)
}

// manageTokenOK verifies a token. Pure.
func manageTokenOK(key []byte, token string, charID, corpID, issuedAt, now int64) bool {
	es, sig, ok := strings.Cut(token, "-")
	if !ok || issuedAt == 0 {
		return false
	}
	exp, err := strconv.ParseInt(es, 10, 64)
	if err != nil || exp <= now || exp > now+manageTokenSec {
		return false
	}
	return checkDesk(key, sig, "manage", charID, corpID, issuedAt, exp)
}

// portalKeyIssuedAt is when the pilot's key for this corporation was issued, 0 for none.
func (m *Mod) portalKeyIssuedAt(ctx context.Context, charID, corpID int64) int64 {
	var at int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT issuedAt FROM rolebot_portal_keys WHERE characterID = ? AND corporationID = ?`, charID, corpID).Scan(&at)
	return at
}

// manageNonces are the confirmations already given: a confirm is a GET, and reloading one must not
// give the order twice.
var manageNonces sync.Map // "charID|nonce" to unix time used

func init() {
	registerSweep("manageNonces", func() {
		now := clock.Unix()
		manageNonces.Range(func(k, v any) bool {
			if now-v.(int64) >= manageNonceSec {
				manageNonces.Delete(k)
			}
			return true
		})
	})
}

// useNonce takes a confirmation; false when it was taken before or is no confirmation.
func useNonce(charID int64, nonce string) bool {
	if len(nonce) != 16 {
		return false
	}
	_, used := manageNonces.LoadOrStore(fmt.Sprintf("%d|%s", charID, nonce), clock.Unix())
	return !used
}

func newNonce() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// ── the request ─────────────────────────────────────────────────────────────

// mgPage is one request of the Management page by somebody who may see it.
type mgPage struct {
	m      *Mod
	ctx    context.Context
	pg     igbPage
	q      url.Values
	charID int64
	corpID int64
	ceoID  int64
	scope  commandScope
	div    string // "", an arm's slug, or one of mgCorp, mgHeads, mgPeople
	token  string // "" while signed out
	botCEO bool
	// suggest is what a refused form might have meant: links that send it back filled in.
	suggest []string
	// previewing is set while a preview waits above to be confirmed.
	previewing bool
}

// arm is the division the page is on, "" for none.
func (p *mgPage) arm() corpArm {
	if validArm(corpArm(p.div)) {
		return corpArm(p.div)
	}
	return ""
}

// mgDivAllowed is the page a pilot gets for the one it asked for: a head is sent to its own
// division whatever the address says, and only the CEO has the pages that are no division. Pure.
func mgDivAllowed(s commandScope, div string) string {
	if a := corpArm(div); validArm(a) && s.Commands(a) {
		return div
	}
	if s.CEO {
		switch div {
		case mgCorp, mgHeads, mgPeople:
			return div
		}
		return ""
	}
	if ds := s.Divisions(); len(ds) > 0 {
		return string(ds[0])
	}
	return ""
}

func (p *mgPage) url(div string, kv ...string) string {
	v := url.Values{}
	v.Set("div", div)
	if p.token != "" {
		v.Set("s", p.token)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return esp.IGBHost() + managePath + "?" + v.Encode()
}

func (p *mgPage) link(div, label string, kv ...string) string {
	return `<a href="` + attrEsc(p.url(div, kv...)) + `">` + label + `</a>`
}

// open starts a form of this page. Never inside a table.
func (p *mgPage) open(form string) string {
	return `<form action="` + esp.IGBHost() + managePath + `" method="get">` +
		mgHidden("form", form) + mgHidden("div", p.div) + mgHidden("s", p.token)
}

func (p *mgPage) submit(label string) string {
	return `<br><input type="submit" value="` + attrEsc(label) + `"></form><br>`
}

func mgHidden(name, value string) string {
	return `<input type="hidden" name="` + name + `" value="` + attrEsc(value) + `">`
}

// mgText is a labelled text field.
func mgText(label, name, value string, size int, after string) string {
	s := `<font size=3>` + label + ` </font><input type="text" name="` + name + `" size="` + strconv.Itoa(size) +
		`" value="` + attrEsc(value) + `">`
	if after != "" {
		s += `<font size=3> ` + after + `</font>`
	}
	return s + " "
}

// cur is what a form's field held when it was sent, so a refused form comes back filled in.
func (p *mgPage) cur(form, field, def string) string {
	if p.q.Get("form") == form && p.q.Has(field) {
		return p.q.Get(field)
	}
	if v := p.q.Get("pre_" + field); v != "" && p.q.Get("pre") == form {
		return v
	}
	return def
}

// handleIGBCorpManagement serves /corp/management.
func (m *Mod) handleIGBCorpManagement(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	say := func(html string) { esp.WriteCorpPortalPage(w, r, "management", html) }
	charID, corpID := m.portalViewer(ctx, r)
	if corpID == 0 || !m.seatedByPerson(ctx, charID) {
		say(igbF3("Open this page from your own terminal."))
		return
	}
	pg := m.igbPageFor(r)
	q := r.URL.Query()
	if !m.memberRankIn(ctx, corpID, charID).Director {
		say(igbF3("Management is for the CEO and the Directors of the corporation. What they have ordered is on the " +
			`<a href="` + esp.IGBHost() + `/corp/ops">Operations</a> page.`))
		return
	}
	_, bot := managedCEO(corpID)
	p := &mgPage{m: m, ctx: ctx, pg: pg, q: q, charID: charID, corpID: corpID, ceoID: m.corpCEOID(ctx, corpID), botCEO: bot,
		scope: m.commandScopeOf(ctx, corpID, charID)}
	p.div = mgDivAllowed(p.scope, q.Get("div"))

	var top strings.Builder
	if q.Get("key") == "request" {
		// The header is only a hint, so the letter is rate limited: nobody else can
		// keep retiring a chair's key.
		if claimJoinMail(charID, "portalkey") {
			top.WriteString(pg.ordersBanner(m.mailPortalKey(ctx, charID, corpID), false))
		} else {
			top.WriteString(pg.ordersBanner("Your key was mailed a few minutes ago. Look in your inbox.", true))
		}
	}
	if !p.scope.Officer() {
		say(top.String() + p.unassigned())
		return
	}
	// Signed in by the token the page carries, or by the key just pasted.
	issued := m.portalKeyIssuedAt(ctx, charID, corpID)
	now := clock.Unix()
	signedIn := manageTokenOK(deskKeyAny(), q.Get("s"), charID, corpID, issued, now)
	if key := strings.TrimSpace(q.Get("key")); !signedIn && key != "" && key != "request" {
		if err := m.verifyPortalKey(ctx, charID, corpID, key); err != nil {
			msg := err.Error()
			top.WriteString(pg.ordersBanner(strings.ToUpper(msg[:1])+msg[1:]+".", true))
		} else {
			signedIn = true
		}
	}
	if !signedIn {
		if q.Get("s") != "" {
			top.WriteString(pg.ordersBanner("You have been signed out. Paste your key to carry on.", true))
		}
		say(top.String() + p.signIn(issued != 0))
		return
	}
	p.token = manageToken(deskKeyAny(), charID, corpID, issued, now+manageTokenSec)
	if q.Get("form") != "" {
		if CorpOrders() {
			top.WriteString(p.act())
		} else {
			top.WriteString(pg.ordersBanner("Orders are not being taken at the moment.", true))
		}
	}
	say(p.body(top.String()))
}

func (p *mgPage) unassigned() string {
	who := "The CEO"
	if p.botCEO {
		who = "The CEO, " + esp.IGBText(p.m.charNameOr(p.ctx, p.ceoID)) + ","
	}
	var b strings.Builder
	p.masthead(&b, esp.CorpLogo(p.corpID, 48), p.pg.pal.Title, "Management", "You hold the Director role but no division. "+
		who+" puts Directors in charge of divisions; yours appears here once you have one.", p.charID, "")
	return b.String()
}

// refuse is a refused form: why, and what it might have meant.
func (p *mgPage) refuse(why string) string {
	s := p.pg.ordersBanner(why, true)
	if len(p.suggest) > 0 {
		s += igbF3("Did you mean:") + "<br>\n" + strings.Join(p.suggest, "<br>\n") + "<br><br>\n"
	}
	return s
}

// again is a link that opens the form being answered with one field changed.
func (p *mgPage) again(label, field, value string) string {
	form := p.q.Get("form")
	kv := []string{"o", form, "pre", form}
	for k := range p.q {
		switch k {
		case "form", "div", "s", "o", "confirm", "n", "key", "pre", field:
			continue
		}
		if !strings.HasPrefix(k, "pre_") && !strings.HasPrefix(k, "find_") {
			kv = append(kv, "pre_"+k, p.q.Get(k))
		}
	}
	return p.link(p.div, esp.IGBText(label), append(kv, "pre_"+field, value)...)
}

// signIn is the page before the key: who the viewer is, and the field.
func (p *mgPage) signIn(hasKey bool) string {
	var b strings.Builder
	p.masthead(&b, esp.CorpLogo(p.corpID, 48), p.pg.pal.Title, "Management", p.hat(), p.charID, "")
	b.WriteString("<br>\n")
	host := esp.IGBHost()
	if hasKey {
		b.WriteString(p.pg.dim3(`Paste your portal key to open your desk. Lost it? <a href="`+host+managePath+
			`?key=request">Request a new one</a>; the old one stops working.`) + "<br>\n")
	} else {
		b.WriteString(p.pg.dim3(`Your desk opens with a portal key, mailed to you by the corporation: `+
			`<a href="`+host+managePath+`?key=request">request your key</a>.`) + "<br>\n")
	}
	b.WriteString(`<form action="` + host + managePath + `" method="get">` + mgHidden("div", p.q.Get("div")) +
		mgText("Portal key:", "key", "", 22, "") + `<input type="submit" value="Open the desk"></form>`)
	return b.String()
}

// hat words what the viewer commands.
func (p *mgPage) hat() string {
	if p.scope.CEO {
		return "You are the <b>Chief Executive Officer</b>: every division answers to you."
	}
	var names []string
	for _, a := range p.scope.Divisions() {
		names = append(names, "<b>"+esp.IGBText(armLabel(a))+"</b>")
	}
	return "You head " + strings.Join(names, " and ") + ". The CEO may overrule you."
}

// ── the forms ───────────────────────────────────────────────────────────────

// mgForm is one kind of order: where its form appears, how it is drawn, and what a sent form means.
// Parse validates and answers the commands to file, the preview, or why not; it puts every field it
// used in hidden, which the confirmation carries.
type mgForm struct {
	Form  string
	Title string
	Short string                    // its tile's line
	Note  string                    // what it does, over the open form
	Icon  func(corpID int64) string // its tile's picture
	Arms  []corpArm                 // the divisions it belongs to; empty is the corporation's own settings
	// CEOOnly keeps a division's form from its head.
	CEOOnly bool
	Write   func(p *mgPage, b *strings.Builder, a corpArm)
	Parse   func(p *mgPage, a corpArm, hidden map[string]string) (cmds []corpCommand, preview, bad string)
}

// on reports whether a form belongs on a page. Pure.
func (f mgForm) on(a corpArm) bool {
	if a == "" {
		return len(f.Arms) == 0
	}
	for _, x := range f.Arms {
		if x == a {
			return true
		}
	}
	return false
}

// manageForms is every form, in the order a page draws them. Built in init: the forms reach the
// page that lists them.
var manageForms []mgForm

func init() {
	manageForms = append(manageForms, divisionForms()...)
	manageForms = append(manageForms, settingForms()...)
}

// manageFormFor finds the form a pilot may send from a page, nil for none: the form must belong to
// that page and the page to the pilot. Pure.
func manageFormFor(s commandScope, form string, a corpArm) *mgForm {
	for i := range manageForms {
		f := &manageForms[i]
		if f.Form == form && f.on(a) && s.Commands(a) && (s.CEO || !f.CEOOnly) {
			return f
		}
	}
	return nil
}

// act answers a sent form: the refusal, the preview, or that it is done.
func (p *mgPage) act() string {
	form := p.q.Get("form")
	if form == "cancel" {
		return p.cancel()
	}
	a := p.arm()
	var f *mgForm
	switch {
	case form == "head" && p.scope.CEO:
		f = &mgForm{Form: form, Title: "Appointment", Parse: parseHeadForm}
	case form == "people" && (p.scope.CEO || a != ""):
		f = &mgForm{Form: form, Title: "Assignment", Parse: parsePeopleForm}
	default:
		f = manageFormFor(p.scope, form, a)
	}
	if f == nil {
		return p.pg.ordersBanner("That is not an order you can give from here.", true)
	}
	if field := p.findField(); field != "" {
		if page, shown := p.findPage(field, form); shown {
			return page
		}
	}
	hidden := map[string]string{}
	cmds, preview, bad := f.Parse(p, a, hidden)
	if bad != "" {
		return p.refuse(bad)
	}
	for i := range cmds {
		c := &cmds[i]
		c.CorpID, c.PosterID, c.PosterCEO = p.corpID, p.charID, p.scope.CEO
		c.PosterHat = p.m.officerHat(p.ctx, p.corpID, p.charID)
		c.Arm = string(a)
		if f.Form == "people" && p.scope.CEO {
			c.Arm = ""
		}
		if c.ExpiresAt == 0 {
			c.ExpiresAt = clock.Unix() + 3600
		}
	}
	if why := p.m.overruled(p.ctx, p.scope, cmds); why != "" {
		return p.pg.ordersBanner(why, true)
	}
	if p.q.Get("confirm") != "1" {
		return p.confirm("Is this right?", preview, form, hidden, "Give the order")
	}
	if !useNonce(p.charID, p.q.Get("n")) {
		return p.pg.ordersBanner("That order has already been given.", true)
	}
	for _, c := range cmds {
		id, err := p.m.postCorpCommand(p.ctx, c)
		if err != nil {
			rlogf("rolebots: post order: %v", err)
			return p.pg.ordersBanner("The order could not be filed. Try again shortly.", true)
		}
		p.m.logBot(0, logInfo, "order", "%s (%s) posted %s #%d for corporation %d: %s",
			p.m.charNameOr(p.ctx, p.charID), c.PosterHat, c.Kind, id, p.corpID, c.Summary(p.m, p.ctx))
	}
	// The filed form is not drawn back filled in.
	p.q.Del("form")
	p.q.Del("o")
	return p.pg.ordersBanner("Order given. The desk takes it up within a minute.", false)
}

// confirm draws a preview with its confirmation form.
func (p *mgPage) confirm(title, preview, form string, hidden map[string]string, button string) string {
	p.previewing = true
	var b strings.Builder
	b.WriteString("<table width=100% bgcolor=" + igbShadeMuster + "><tr><td><font size=5 color=" + p.pg.pal.Amber + "><b>" + title +
		"</b></font><br>" + preview + "</td></tr></table>\n")
	b.WriteString(p.open(form))
	keys := make([]string, 0, len(hidden))
	for k := range hidden {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(mgHidden(k, hidden[k]))
	}
	b.WriteString(mgHidden("confirm", "1") + mgHidden("n", newNonce()))
	b.WriteString(`<input type="submit" value="` + attrEsc(button) + `"></form>`)
	// Back to the form as it was filled in, or away from it.
	if form != "cancel" {
		kv := []string{"o", form, "pre", form}
		for _, k := range keys {
			kv = append(kv, "pre_"+k, hidden[k])
		}
		b.WriteString(igbF3(p.link(p.div, "change it", kv...) + " &nbsp; "))
	}
	b.WriteString(igbF3(p.link(p.div, "never mind")) + "<br><br>\n")
	return b.String()
}

// overruled is why a division head may not give an order the CEO has spoken on, "" when it may.
func (m *Mod) overruled(ctx context.Context, s commandScope, cmds []corpCommand) string {
	if s.CEO || len(cmds) == 0 {
		return ""
	}
	for _, o := range m.listCorpCommands(ctx, cmds[0].CorpID, 100) {
		if !o.Open() || !commandOutranks(o, s) {
			continue
		}
		for _, c := range cmds {
			if sameCommandTarget(o, c) {
				return "The CEO has an order in force on that: " + o.Title(m, ctx) + " — " + o.Summary(m, ctx) + "."
			}
		}
	}
	return ""
}

// lockWithin takes a lock that comes free in time.
func lockWithin(mu *sync.Mutex, d time.Duration) bool {
	for end := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		if mu.TryLock() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

// cancel withdraws an order.
func (p *mgPage) cancel() string {
	id, _ := strconv.ParseInt(p.q.Get("id"), 10, 64)
	c, err := p.m.getCorpCommand(p.ctx, id)
	if err != nil || c == nil || c.CorpID != p.corpID || !mayCancelCommand(p.scope, *c) {
		return p.pg.ordersBanner("No such order of yours.", true)
	}
	if !c.Open() {
		return p.pg.ordersBanner("That order is already closed.", true)
	}
	if p.q.Get("confirm") != "1" {
		preview := igbF3("Withdraw <b>"+esp.IGBText(c.Title(p.m, p.ctx))+"</b> — "+esp.IGBText(c.Summary(p.m, p.ctx))+"?") + "<br>"
		if c.Kind == cmdKindFleet && c.RefID != 0 {
			preview += p.pg.dim3("The fleet it raised winds down and settles up.") + "<br>"
		}
		return p.confirm("Withdraw this order?", preview, "cancel", map[string]string{"id": strconv.FormatInt(id, 10)}, "Withdraw the order")
	}
	if !useNonce(p.charID, p.q.Get("n")) {
		return p.pg.ordersBanner("That order has already been withdrawn.", true)
	}
	// The desk takes orders up under this lock: an order is withdrawn before it is taken or after,
	// never while.
	lk := corpOpLock(p.corpID)
	if !lockWithin(lk, 5*time.Second) {
		return p.pg.ordersBanner("The desk is busy with the corporation's orders. Try again in a minute.", true)
	}
	defer lk.Unlock()
	if c, err = p.m.getCorpCommand(p.ctx, id); err != nil || c == nil || !c.Open() {
		return p.pg.ordersBanner("That order is already closed.", true)
	}
	if why := p.m.withdrawCommand(withOpLockHeld(p.ctx), *c, p.charID); why != "" {
		return p.pg.ordersBanner(why, true)
	}
	p.q.Del("form")
	return p.pg.ordersBanner("Order withdrawn.", false)
}
