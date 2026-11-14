package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// The wallet spread panel on the Economy overview: how the roster's ISK is distributed across pilots.
// A distribution answers whether the economy works, which a list of the richest cannot. It reads live
// balances, so the overview's window picker does not change it.

// walletBand is one bucket of the wallet distribution.
type walletBand struct {
	Label string
	Count int
	Pct   float64
	Poor  bool // under the minimum: the band that cannot buy its way back to work
}

// walletBoard is the money summary the panel draws.
type walletBoard struct {
	Bands []walletBand
	// BelowFloor is the headline: pilots that cannot commit anything at all, which is what the wallet
	// floor (broke.go) defines as unable to work.
	BelowFloor int
	BelowPct   float64
	// Median summarises better than the mean here: a long right tail of rich pilots drags the mean up
	// and says nothing about the many holding almost nothing.
	Median float64
	Total  float64
	Floor  float64
}

func (w walletBoard) MedianISK() string { return iskShort(w.Median) }
func (w walletBoard) TotalISK() string  { return iskShort(w.Total) }
func (w walletBoard) FloorISK() string  { return iskShort(w.Floor) }
func (w walletBoard) BelowStr() string  { return fmt.Sprintf("%.1f", w.BelowPct) }

// buildWalletBoard derives the distribution from every rostered pilot's balance. The bands are fixed
// so the shape is comparable from one refresh to the next.
func buildWalletBoard(balances []float64) walletBoard {
	b := walletBoard{Floor: walletFloorISK}
	if len(balances) == 0 {
		return b
	}
	cuts := []struct {
		label string
		hi    float64
	}{
		{"under 100", 100},
		{"under 1k", 1_000},
		{"under 100k", 100_000},
		{"under the minimum", walletFloorISK},
		{"under 1M (starting balance)", 1_000_000},
		{"under 10M", 10_000_000},
		{"10M and over", math.MaxFloat64},
	}
	counts := make([]int, len(cuts))
	sorted := append([]float64(nil), balances...)
	for _, bal := range sorted {
		b.Total += bal
		if bal < walletFloorISK {
			b.BelowFloor++
		}
		for i, c := range cuts {
			if bal < c.hi {
				counts[i]++
				break
			}
		}
	}
	sort.Float64s(sorted)
	b.Median = sorted[len(sorted)/2]
	b.BelowPct = 100 * float64(b.BelowFloor) / float64(len(sorted))
	for i, c := range cuts {
		b.Bands = append(b.Bands, walletBand{
			Label: c.label,
			Count: counts[i],
			Pct:   100 * float64(counts[i]) / float64(len(sorted)),
			// Every band at or below the minimum is a pilot that cannot work.
			Poor: c.hi <= walletFloorISK,
		})
	}
	return b
}

// econWalletBalances reads every rostered pilot's live balance.
func (m *Mod) econWalletBalances(ctx context.Context) ([]float64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT COALESCE(c.balance, 0)
		  FROM rolebot_characters rb JOIN character_ c ON c.characterID = rb.characterID`)
	if err != nil {
		return nil, fmt.Errorf("wallet balances: %w", err)
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var bal float64
		if err := rows.Scan(&bal); err != nil {
			return nil, fmt.Errorf("wallet balances: %w", err)
		}
		out = append(out, bal)
	}
	return out, rows.Err()
}
