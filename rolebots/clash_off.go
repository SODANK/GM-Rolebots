//go:build !mindclash

package rolebots

// Mind Clash, absent. The sport lives in modules/MindClash-mod; a build without the
// mindclash tag gets these inert seams. A bot left mid-errand has no registered task for
// clash.travel or clash.attend, and sim.go advance logs and re-plans.

import (
	"context"
	"net/http"
)

func (m *Mod) startClash(ctx context.Context) {}

func (m *Mod) clashVenueDue(ctx context.Context, b *botRun) (bool, error) { return false, nil }

func clashErrandActive(charID int64) bool { return false }

func (m *Mod) clashESP() {}

func (m *Mod) handleClashTab(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/rolebots", http.StatusSeeOther)
}
