package rolebots

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// The CEO page's share register (corpshares.go): the price, the float and
// who owns the corporation.

// ceoShareView is the Shareholders card, formatted.
type ceoShareView struct {
	Issued, Treasury, Outstanding, Sellable int64
	NAV, Earnings, Book, Fair, Ask, Bid     string
	Premium, Yield, Control                 string
	Owed                                    string
	Holders                                 []ceoShareRow

	// Dividend policy and the override form's current values.
	CorpID                     int64
	PayoutRatio, PayoutEvery   string
	PayoutAuto, EveryAuto      bool
	PctVal, DaysVal            string
	LastDividend, NextDividend string
	EarnedSince                string
}

// ceoShareRow is one holder.
type ceoShareRow struct {
	HolderID     int64
	Name, Kind   string
	Link         string
	Shares       int64
	Pct          string
	In, Out, Val string
}

// ceoShareHoldersShown bounds the register on the page.
const ceoShareHoldersShown = 30

func (m *Mod) ceoShareView(ctx context.Context, c *corpCEO, ch Charter) *ceoShareView {
	corpID := c.CorpID
	q, err := m.corpShareQuote(ctx, corpID, ch)
	if err != nil || q.Issued <= 0 {
		return nil
	}
	v := &ceoShareView{
		Issued: q.Issued, Treasury: q.Treasury, Outstanding: q.Outstanding,
		Sellable: treasurySellable(q.Issued, q.Treasury, ch.ControlFloor),
		NAV:      iskString(q.NAV), Earnings: iskString(q.Earnings),
		Book: iskString(q.Book), Fair: iskString(q.Fair), Ask: iskString(q.Ask), Bid: iskString(q.Bid),
		Premium: fmt.Sprintf("%.0f%%", q.Premium*100),
		Yield:   fmt.Sprintf("%.1f%%", 100*m.shareYield(ctx, corpID, q)),
		Control: fmt.Sprintf("%.0f%%", 100*float64(evedb.ShareVoteWeight(ctx, m.db, c.CharID, corpID))/float64(q.Issued)),
	}
	pol := m.loadDividendPolicy(ctx, corpID, ch)
	v.CorpID = corpID
	v.PayoutRatio = fmt.Sprintf("%.0f%%", pol.Ratio*100)
	v.PayoutEvery = fmt.Sprintf("%.0f days", float64(pol.EverySec)/86400)
	v.PayoutAuto, v.EveryAuto = !pol.RatioSet, !pol.EverySet
	if pol.RatioSet {
		v.PctVal = fmt.Sprintf("%g", pol.Ratio*100)
	}
	if pol.EverySet {
		v.DaysVal = fmt.Sprintf("%g", float64(pol.EverySec)/86400)
	}
	since := pol.LastPaidAt
	if since == 0 {
		v.LastDividend = "never"
		since = clock.Unix() - pol.EverySec
	} else {
		v.LastDividend = fmtDuration(clock.Unix()-pol.LastPaidAt) + " ago"
	}
	v.EarnedSince = iskString(m.earnedSince(ctx, corpID, since))
	if due := pol.LastPaidAt + pol.EverySec - clock.Unix(); pol.LastPaidAt == 0 || due <= 0 {
		v.NextDividend = "at the next review"
	} else {
		v.NextDividend = "in " + fmtDuration(due)
	}
	var owed float64
	_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(owed), 0) FROM rolebot_share_redemptions
		WHERE corporationID = ? AND paidAt = 0`, corpID).Scan(&owed)
	if owed > 0 {
		v.Owed = iskString(owed)
	}

	holders, err := evedb.BeneficialShareHolders(ctx, m.db, corpID)
	if err != nil {
		return v
	}
	for i, h := range holders {
		if i >= ceoShareHoldersShown {
			break
		}
		row := ceoShareRow{HolderID: h.HolderID, Shares: h.Shares,
			Pct: fmt.Sprintf("%.1f%%", 100*float64(h.Shares)/float64(q.Issued))}
		if h.HolderID == corpID {
			row.Name, row.Kind = "Treasury", "treasury"
			v.Holders = append(v.Holders, row)
			continue
		}
		var name string
		var memberOf int64
		var bot int
		err := m.db.QueryRowContext(ctx, `
			SELECT c.characterName, COALESCE(c.corporationID, 0),
			       (SELECT COUNT(*) FROM rolebot_characters b WHERE b.characterID = c.characterID)
			  FROM character_ c WHERE c.characterID = ?`, h.HolderID).Scan(&name, &memberOf, &bot)
		switch {
		case err == nil:
			row.Name = name
			row.Link = fmt.Sprintf("/character/%d", h.HolderID)
			if bot > 0 {
				row.Link = fmt.Sprintf("/rolebot/%d", h.HolderID)
			}
			switch {
			case h.HolderID == c.CharID:
				row.Kind = "CEO"
			case memberOf == corpID:
				row.Kind = "member"
			default:
				row.Kind = "ex-member"
			}
		default:
			row.Name, row.Kind = corpNameOf(ctx, m.db, h.HolderID), "corporation"
			row.Link = fmt.Sprintf("/corporation/%d", h.HolderID)
		}
		var in, out float64
		_ = m.db.QueryRowContext(ctx, `SELECT contributed, paidOut FROM rolebot_corp_equity
			WHERE corporationID = ? AND characterID = ?`, corpID, h.HolderID).Scan(&in, &out)
		row.In, row.Out = iskString(in), iskString(out)
		row.Val = iskString(float64(h.Shares) * q.Bid)
		v.Holders = append(v.Holders, row)
	}
	return v
}

// tmplCEOShares is the Shareholders card.
const tmplCEOShares = `
{{with .Shares}}
<div class="card">
  <h3>Shareholders</h3>
  <div class="stats">
    <div class="stat"><div class="label">Ask</div><div class="value">{{.Ask}}</div></div>
    <div class="stat"><div class="label">Bid</div><div class="value">{{.Bid}}</div></div>
    <div class="stat"><div class="label">Book / share</div><div class="value">{{.Book}}</div></div>
    <div class="stat"><div class="label">Premium</div><div class="value">{{.Premium}}</div></div>
    <div class="stat"><div class="label">Trailing yield</div><div class="value">{{.Yield}}</div></div>
    <div class="stat"><div class="label">CEO votes</div><div class="value">{{.Control}}</div></div>
  </div>
  <p style="font-size:12px">{{.Issued}} issued &middot; {{.Treasury}} in the treasury ({{.Sellable}} for sale)
    &middot; {{.Outstanding}} outstanding &middot; NAV {{.NAV}} &middot; earning {{.Earnings}} a week
    &middot; fair value {{.Fair}}{{if .Owed}} &middot; <b>owes {{.Owed}} in buybacks</b>{{end}}</p>
  <table class="tbl" style="font-size:12px">
    <tr><th>Holder</th><th></th><th>Shares</th><th>%</th><th>Paid in</th><th>Paid back</th><th>Worth (bid)</th></tr>
    {{range .Holders}}<tr>
      <td>{{if .Link}}<a href="{{.Link}}">{{.Name}}</a>{{else}}{{.Name}}{{end}}</td><td>{{.Kind}}</td>
      <td>{{.Shares}}</td><td>{{.Pct}}</td>
      <td>{{if ne .Kind "treasury"}}{{.In}}{{end}}</td><td>{{if ne .Kind "treasury"}}{{.Out}}{{end}}</td>
      <td>{{if ne .Kind "treasury"}}{{.Val}}{{end}}</td></tr>{{end}}
  </table>
  <h4 style="margin-top:10px">Dividends</h4>
  <p style="font-size:12px">Pays <b>{{.PayoutRatio}}</b> of earnings{{if .PayoutAuto}} (charter){{end}}
    every <b>{{.PayoutEvery}}</b>{{if .EveryAuto}} (charter){{end}} &middot; last paid {{.LastDividend}}
    &middot; next {{.NextDividend}} &middot; earned since: {{.EarnedSince}}</p>
  <form method="POST" style="display:inline;font-size:12px">
    <input type="hidden" name="corpID" value="{{.CorpID}}">
    <input type="hidden" name="action" value="dividend">
    Payout <input name="payoutPct" value="{{.PctVal}}" size="4" placeholder="auto">% of earnings,
    every <input name="dividendDays" value="{{.DaysVal}}" size="4" placeholder="auto"> days
    <button type="submit">Save</button>
  </form>
  <form method="POST" style="display:inline;font-size:12px" onsubmit="return confirm('Pay a dividend now?')">
    <input type="hidden" name="corpID" value="{{.CorpID}}">
    <input type="hidden" name="action" value="paydividend">
    <button type="submit">Pay now</button>
  </form>
  <p style="font-size:12px;color:#6b7280">Blank = the CEO's charter decides. 0% stops dividends. A dividend is
  capped at half the free capital; under 250k it waits for the next review (Pay now skips the wait, not the cap).
  Holders see it in the wallet journal as a dividend payment.</p>
  <p style="font-size:12px;color:#6b7280">Members buy out of the treasury at the ask and sell back at the bid,
  out of a weekly buyback budget. Players buy with Give Money to the corporation, "shares" in the reason, and
  sell by giving their shares to the corporation. The CEO votes the treasury.</p>
</div>
{{end}}
`
