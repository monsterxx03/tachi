package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/urfave/cli/v3"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// ── Usage report command ─────────────────────────────────────────────────────

// usageDayStat aggregates ledger cost and call count for one calendar day.
type usageDayStat struct {
	Date     string  // "2006-01-02"
	Cost     float64 // CNY
	Calls    int
	Unpriced int // calls without an effective price at call time
}

// usageSummary is the all-time ledger aggregate shown by `tachi usage`.
type usageSummary struct {
	TotalCost     float64
	TotalCalls    int
	UnpricedCalls int
	Days          []usageDayStat // newest day first
}

// summarizeUsage aggregates raw ledger rows into an all-time summary: total
// cost + per-day breakdown (grouped by each row's local date, newest first).
func summarizeUsage(rows []llm.UsageRow) usageSummary {
	dayMap := make(map[string]*usageDayStat)
	var sum usageSummary
	for i := range rows {
		row := &rows[i]
		cost := row.Cost()
		date := row.TS.Format("2006-01-02")
		day := dayMap[date]
		if day == nil {
			day = &usageDayStat{Date: date}
			dayMap[date] = day
		}
		day.Cost += cost
		day.Calls++
		if row.Unpriced() {
			day.Unpriced++
		}
		sum.TotalCost += cost
		sum.TotalCalls++
		if row.Unpriced() {
			sum.UnpricedCalls++
		}
	}
	for _, day := range dayMap {
		sum.Days = append(sum.Days, *day)
	}
	slices.SortFunc(sum.Days, func(a, b usageDayStat) int { return strings.Compare(b.Date, a.Date) })
	return sum
}

// runUsage implements `tachi usage`: aggregates every ledger row under
// <home>/usage/ and prints the all-time total plus a per-day breakdown.
func runUsage(ctx context.Context, cmd *cli.Command) error {
	rows, err := llm.NewUsageRecorder(config.UsageDir()).RowsAll(time.Time{})
	if err != nil {
		return fmt.Errorf("read usage ledger: %w", err)
	}
	sum := summarizeUsage(rows)

	fmt.Println("Usage Report (all-time)")
	fmt.Println(strings.Repeat("─", 24))
	if sum.TotalCalls == 0 {
		fmt.Println("No usage data yet.")
		return nil
	}
	fmt.Printf("Total cost:  ¥%.4f\n", sum.TotalCost)
	fmt.Printf("Total calls: %d", sum.TotalCalls)
	if sum.UnpricedCalls > 0 {
		fmt.Printf(" (%d unpriced)", sum.UnpricedCalls)
	}
	fmt.Println()

	fmt.Println("\nPer-day:")
	fmt.Printf("%-12s  %12s  %6s\n", "DATE", "COST", "CALLS")
	fmt.Println(strings.Repeat("─", 34))
	for _, day := range sum.Days {
		line := fmt.Sprintf("%-12s  %12s  %6d", day.Date, padLeft(fmt.Sprintf("¥%.4f", day.Cost), 12), day.Calls)
		if day.Unpriced > 0 {
			line += fmt.Sprintf("  (%d unpriced)", day.Unpriced)
		}
		fmt.Println(line)
	}
	return nil
}

// padLeft left-pads s with spaces to the given DISPLAY width (runewidth-aware,
// so multi-byte chars like ¥ don't misalign the column).
func padLeft(s string, width int) string {
	if d := width - runewidth.StringWidth(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}
