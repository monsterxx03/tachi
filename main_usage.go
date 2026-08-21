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
	Unpriced int     // calls without an effective price at call time
	Credit   float64 // real credit (snapshots; pre-upgrade rows recomputed from current rate)
}

// usageSummary is the all-time ledger aggregate shown by `tachi usage`.
type usageSummary struct {
	TotalCost     float64
	TotalCalls    int
	UnpricedCalls int
	TotalCredit   float64
	Days          []usageDayStat // newest day first
}

// summarizeUsage aggregates raw ledger rows into an all-time summary: total
// cost + per-day breakdown (grouped by each row's local date, newest first).
// cfg supplies the current credit_rate for pre-upgrade rows without a credit
// snapshot (see llm.UsageRow.CreditValue); nil = rate 0.
func summarizeUsage(rows []llm.UsageRow, cfg *config.Config) usageSummary {
	dayMap := make(map[string]*usageDayStat)
	var sum usageSummary
	for i := range rows {
		row := &rows[i]
		cost := row.Cost()
		credit := row.CreditValue(llm.ResolveCreditRate(cfg, row.Provider, row.Model))
		sum.TotalCredit += credit
		date := row.TS.Format("2006-01-02")
		day := dayMap[date]
		if day == nil {
			day = &usageDayStat{Date: date}
			dayMap[date] = day
		}
		day.Cost += cost
		day.Credit += credit
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
	// Current config for recomputing pre-credit rows; a failed load only
	// means rate 0 for those rows, never a broken report.
	cfg, _ := config.Load()
	sum := summarizeUsage(rows, cfg)

	fmt.Println("Usage Report (all-time)")
	fmt.Println(strings.Repeat("─", 24))
	if sum.TotalCalls == 0 {
		fmt.Println("No usage data yet.")
		return nil
	}
	fmt.Printf("Total cost:  ¥%.4f\n", sum.TotalCost)
	if sum.TotalCredit != 0 {
		fmt.Printf("Total credit: %.2f\n", sum.TotalCredit)
	}
	fmt.Printf("Total calls: %d", sum.TotalCalls)
	if sum.UnpricedCalls > 0 {
		fmt.Printf(" (%d unpriced)", sum.UnpricedCalls)
	}
	fmt.Println()

	fmt.Println("\nPer-day:")
	fmt.Printf("%-12s  %12s  %10s  %6s\n", "DATE", "COST", "CREDIT", "CALLS")
	fmt.Println(strings.Repeat("─", 44))
	for _, day := range sum.Days {
		line := fmt.Sprintf("%-12s  %12s  %10s  %6d",
			day.Date,
			padLeft(fmt.Sprintf("¥%.4f", day.Cost), 12),
			padLeft(fmt.Sprintf("%.2f", day.Credit), 10),
			day.Calls)
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
