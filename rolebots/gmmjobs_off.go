//go:build !gmmordu

package rolebots

// Stubs for builds without the gmmordu tag. The freelance board is the module
// modules/GMMordu-mod and gmmjobs.go compiles only with the tag, so here no job
// pulls on a planner.

import "context"

func (m *Mod) startGMMJobs(ctx context.Context) {}

func gmmMinePull(charID, sysID int64) float64 { return 0 }

func gmmMinePullMax(charID int64) float64 { return 0 }

func (m *Mod) gmmCombatSite(ctx context.Context, b *botRun, u *universe, weigh func(sysID int64) (ok, engage bool)) int64 {
	return 0
}
