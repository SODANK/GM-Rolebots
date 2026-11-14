//go:build !scc

package rolebots

// Without the SCC module there is no exchange. Every hook the rest of the bots call returns
// "not listed" or does nothing, so share business runs through the treasury desk
// (corpshares.go): issuing, member buys, divesting, selling when short of ISK and dividends.

import "context"

// sccRow is the board row the desk paths read when a corporation is listed. Under the scc tag
// it is evedb.SCCBoardRow.
type sccRow struct {
	Ticker string
	Price  float64
}

func (m *Mod) startSCCBots(context.Context) {}

func (m *Mod) sccListed(context.Context, int64) (sccRow, bool) { return sccRow{}, false }

func (m *Mod) sccCEO(context.Context, *corpCEO, Charter) {}

func (m *Mod) sccBuyOwn(context.Context, *botRun, sccRow, float64) {}

func (m *Mod) sccSellHolding(context.Context, *botRun, sccRow, int64, bool) int64 { return 0 }

func (m *Mod) sccBotReview(context.Context, *botRun) {}
