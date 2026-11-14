package esp

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The in-game browser kit, exported for the rolebots Corporate Portal sections.
// It wraps esp package helpers so module pages use the same bands, icons, bars and
// tints as the other sites, and the same 2005 renderer rules (flat tables, no form
// in a cell, 96% widths). Overlay build only.

// IGBPalette is a site's colours, with three signal tints for security and outcome.
type IGBPalette struct {
	Page, Panel, RowA, RowB string
	Text, Link, Accent, Dim string
	Header, Up, Down, Title string
	// Green, Amber and Red mean empire, lowsec and nullsec; also live, mustering, lost.
	Green, Amber, Red string
	// Now shades the row the panel is about.
	Now string
}

func (p IGBPalette) site() igbSite {
	return igbSite{Pal: igbPalette{
		Page: p.Page, Panel: p.Panel, RowA: p.RowA, RowB: p.RowB,
		Text: p.Text, Link: p.Link, Accent: p.Accent, Dim: p.Dim,
		Header: p.Header, Up: p.Up, Down: p.Down, Title: p.Title,
	}}
}

func paletteOf(site igbSite) IGBPalette {
	s := site.Pal
	return IGBPalette{
		Page: s.Page, Panel: s.Panel, RowA: s.RowA, RowB: s.RowB,
		Text: s.Text, Link: s.Link, Accent: s.Accent, Dim: s.Dim,
		Header: s.Header, Up: s.Up, Down: s.Down, Title: s.Title,
		Green: cdGreen, Amber: cdHi, Red: cdRed, Now: cdNow,
	}
}

// CorpPortalPalette is the palette WriteCorpPortalPage uses for this request: the
// viewer's corporation accent if its logo colour is readable, else the portal's
// charcoal. Resolved through the database, never from the eve.corpID header.
func CorpPortalPalette(r *http.Request) IGBPalette {
	srv := corpPortalServer
	if srv == nil {
		return paletteOf(siteCorp)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	corp, npc := srv.viewerCorp(ctx, r)
	cancel()
	return paletteOf(corpPortalSite(corp, npc))
}

// Panel is one panel of a band: a title, an optional note, and rows of cells. Rows
// have equal cell counts; Align and Width are per column. Empty shows when there
// are no rows. Shades overrides row alternation (PanelGrid only).
type Panel struct {
	Title, Note string
	Align       []string
	Width       []string
	Rows        [][]string
	Empty       string
	Shades      []string
}

func (p Panel) band() igbBandPanel {
	return igbBandPanel{Title: p.Title, Note: p.Note, Align: p.Align, Width: p.Width,
		Rows: p.Rows, Empty: p.Empty, Shades: p.Shades}
}

// PanelBand lays panels side by side as one flat table, row i of every panel in one
// table row. A nested table keeps its own widths and draws over the neighbouring
// column.
func PanelBand(b *strings.Builder, pal IGBPalette, panels []Panel) {
	bands := make([]igbBandPanel, len(panels))
	for i, p := range panels {
		bands[i] = p.band()
	}
	writePanelBand(b, pal.site(), bands)
}

// PanelGrid is PanelBand for bands that share one column grid. Every panel needs
// the same column count and Width list.
func PanelGrid(b *strings.Builder, pal IGBPalette, bands [][]Panel) {
	grid := make([][]igbBandPanel, len(bands))
	for i, band := range bands {
		grid[i] = make([]igbBandPanel, len(band))
		for j, p := range band {
			grid[i][j] = p.band()
		}
	}
	writePanelGrid(b, pal.site(), grid)
}

// Section writes a section heading in the site's colours.
func Section(b *strings.Builder, pal IGBPalette, title string) { siteSection(b, pal.site(), title) }

// Portrait, ShipIcon and CorpLogo return client-drawn images built from the ID
// alone: a pilot's face from the render cache, an item or hull icon, and the corp
// paintshop composite.
func Portrait(charID int64, size int) string { return igbPortrait(charID, size) }
func ShipIcon(typeID int64, size int) string { return igbShipIcon(typeID, size) }
func CorpLogo(corpID int64, size int) string { return igbCorpLogo(corpID, size) }

// PortraitHint is Portrait with a hover line; the client copies an img alt onto
// the sprite hint (components.py Img.Load).
func PortraitHint(charID int64, size int, hint string) string {
	return withImgHint(igbPortrait(charID, size), hint)
}

// UIIcon returns one of the client's interface icons by sheet ID (for example
// "18_01"), drawn through uix.MapSprite.
func UIIcon(id string, size int, hint string) string {
	if id == "" {
		return ""
	}
	return withImgHint(`<img src="icon:`+htmlAttrEscape(id)+`" size=`+strconv.Itoa(size)+`>`, hint)
}

// withImgHint puts an alt attribute on a single <img> tag.
func withImgHint(img, hint string) string {
	if img == "" || hint == "" || !strings.HasSuffix(img, ">") {
		return img
	}
	return img[:len(img)-1] + ` alt="` + htmlAttrEscape(hint) + `">`
}

// PilotLink and CorpLink open the DED dossiers; an ID of 0 gives plain text.
func PilotLink(charID int64, name string) string { return igbPilotLink(charID, name) }
func CorpLink(corpID int64, name string) string  { return igbCorpLink(corpID, name) }

// Bar draws a proportion as repeated pipes in two colours, the only bar this
// renderer draws reliably. width is in characters.
func Bar(frac float64, width int, color string) string { return igbBar(frac, width, color) }

// SecColor tints a security status the way the client's overview does.
func SecColor(sec float64) string { return igbSecColor(sec) }

// YC formats a date such as "YC 128-10-03"; YCTime adds the clock (print it at size
// 3 or bold, since size 2 loses dots and colons). RelTime gives "4h 12m ago".
func YC(t time.Time) string      { return igbYC(t) }
func YCTime(t time.Time) string  { return igbYCTime(t) }
func RelTime(t time.Time) string { return igbRelTime(t) }

// ISK is "1,234,567.80 ISK"; ISKShort is "1.23m".
func ISK(v float64) string      { return igbISK(v) }
func ISKShort(v float64) string { return igbISKShort(v) }
