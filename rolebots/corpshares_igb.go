package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// /corp/shares (old address /ig/corpshares): the share desk as the "Shares" section of the
// core's Corporate Portal: what the viewer's corporation's stock costs, what the viewer
// holds, and how to trade it. Read-only: the eve.charID header is the client's hint, never
// authorization, so every trade goes through the client's own wallet and share dialogs
// (corpshares.go) and this page only explains them.

// handleIGBCorpShares serves /corp/shares and /ig/corpshares.
func (m *Mod) handleIGBCorpShares(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	charID := esp.IGBCharID(r)
	var corpID int64
	if charID != 0 {
		corpID, _ = evedb.GetCharCorpID(ctx, m.db, charID)
	}
	pg := m.igbPageFor(r)
	var b strings.Builder
	if c, _ := getCEO(ctx, m.db, corpID); c != nil && sharesEnabled() {
		m.igbShareDesk(ctx, &b, pg, c, charID)
	} else {
		b.WriteString(igbF3("Your corporation keeps no share desk. Corporations that sell stock to their members "+
			"publish its price and terms here.") + "<br><br>")
	}
	m.igbOtherHoldings(ctx, &b, charID, corpID)
	esp.WriteCorpPortalPage(w, r, "shares", b.String())
}

func (m *Mod) igbShareDesk(ctx context.Context, b *strings.Builder, pg igbPage, c *corpCEO, charID int64) {
	ch := ceoCharterOf(ctx, m, c)
	q, err := m.corpShareQuote(ctx, c.CorpID, ch)
	if err != nil || q.Issued <= 0 {
		b.WriteString(igbF3("The share register could not be read.") + "<br><br>")
		return
	}
	name := esp.IGBText(corpNameOf(ctx, m.db, c.CorpID))
	sellable := treasurySellable(q.Issued, q.Treasury, ch.ControlFloor)
	held := evedb.ShareHolding(ctx, m.db, charID, c.CorpID)

	// The figures.
	stats := []igbStat{
		{Label: "Price to buy", Value: esp.ISKShort(q.Ask), Note: "a share"},
		{Label: "Buyback", Value: esp.ISKShort(q.Bid), Note: "a share"},
		{Label: "Book value", Value: esp.ISKShort(q.Book), Note: "a share"},
		{Label: "For sale", Value: strconv.FormatInt(sellable, 10), Note: fmt.Sprintf("of %d issued", q.Issued)},
	}
	if held > 0 {
		stats = append(stats, igbStat{Label: "You hold", Value: strconv.FormatInt(held, 10),
			Note: fmt.Sprintf("%.1f%% &middot; %s", 100*float64(held)/float64(q.Issued), esp.ISKShort(float64(held)*q.Bid))})
	}
	pg.statStrip(b, stats)
	b.WriteString("<br>\n")

	// How to trade, beside who holds it.
	how := esp.Panel{Title: "How to trade", Align: []string{"", ""}, Width: []string{"60", ""}}
	how.Rows = [][]string{
		{pg.accent("Buy"), igbF3(fmt.Sprintf("Give Money to %s with the word <b>shares</b> in the reason. You get as many "+
			"shares as the money buys at the price above, and the rest is refunded. Members only; nobody but the "+
			"CEO may hold more than %.0f%% of the corporation.", name, m.shareMaxHolderPct(ctx)))},
		{pg.accent("Sell"), igbF3(fmt.Sprintf("In your wallet's Shares tab, give your shares to %s. You are paid the "+
			"buyback price out of this week's buyback budget; anything it cannot cover is owed to you and paid "+
			"as the budget allows.", name))},
	}
	holdersPanel := esp.Panel{Title: "Largest holders", Align: []string{"", "", "right", ""}, Width: []string{"30", "", "70", "130"},
		Empty: "The register is empty."}
	if holders, err := evedb.BeneficialShareHolders(ctx, m.db, c.CorpID); err == nil {
		for i, h := range holders {
			if i >= 10 {
				break
			}
			icon, label := esp.CorpLogo(c.CorpID, 24), igbF3("Treasury")
			if h.HolderID != c.CorpID {
				icon = esp.Portrait(h.HolderID, 24)
				label = igbF3(esp.PilotLink(h.HolderID, esp.IGBText(m.charNameOr(ctx, h.HolderID))))
			}
			frac := float64(h.Shares) / float64(q.Issued)
			shade := ""
			if h.HolderID == charID {
				shade = igbShadeMine
			}
			holdersPanel.Rows = append(holdersPanel.Rows, []string{icon, label,
				igbF3(strconv.FormatInt(h.Shares, 10)),
				esp.Bar(frac, 12, pg.pal.Accent) + " " + pg.dim(fmt.Sprintf("%.1f%%", 100*frac))})
			holdersPanel.Shades = append(holdersPanel.Shades, shade)
		}
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{how, holdersPanel}})
	b.WriteString("<br>\n")
}

// igbOtherHoldings lists the viewer's stakes in managed corporations it no longer belongs
// to: they still pay and can be sold back.
func (m *Mod) igbOtherHoldings(ctx context.Context, b *strings.Builder, charID, corpID int64) {
	if charID == 0 {
		return
	}
	var lines []string
	for _, h := range m.botHoldings(ctx, charID) {
		if h.corpID == corpID {
			continue
		}
		c, _ := getCEO(ctx, m.db, h.corpID)
		if c == nil {
			continue
		}
		q, err := m.corpShareQuote(ctx, h.corpID, ceoCharterOf(ctx, m, c))
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %d shares, buyback %s each",
			esp.IGBText(corpNameOf(ctx, m.db, h.corpID)), h.shares, iskString(q.Bid)))
	}
	if len(lines) > 0 {
		b.WriteString(igbF3("<b>Your other holdings</b><br>"+strings.Join(lines, "<br>")+
			"<br>Give them to the corporation that issued them to sell them back.") + "<br>")
	}
}
