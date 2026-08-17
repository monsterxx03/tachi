package web

import (
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

func configWebAPIKey(key string) config.WebConfig {
	return config.WebConfig{APIKey: key}
}

func TestSummarizeUsageTotals(t *testing.T) {
	rows := []llm.UsageRow{
		{
			TS:                   time.Date(2026, 8, 17, 10, 0, 0, 0, time.Local),
			SessionID:            "s1",
			Kind:                 llm.UsageKindConversation,
			Model:                "deepseek-v4-flash",
			InputTokens:          1000,
			OutputTokens:         200,
			CacheReadInputTokens: 800,
			InputPrice:           1.5,
			OutputPrice:          4.5,
			CacheReadPrice:       0.05,
		},
		{
			TS:           time.Date(2026, 8, 17, 12, 0, 0, 0, time.Local),
			SessionID:    "s1",
			Kind:         llm.UsageKindCommit,
			Model:        "deepseek-v4-flash",
			InputTokens:  500,
			OutputTokens: 100,
			InputPrice:   0,
			OutputPrice:  0, // unpriced
		},
		{
			TS:           time.Date(2026, 8, 16, 9, 0, 0, 0, time.Local),
			Kind:         llm.UsageKindConversation,
			Model:        "claude-sonnet-4.5",
			InputTokens:  2000,
			OutputTokens: 0,
			InputPrice:   3.0,
			OutputPrice:  15.0,
		},
	}

	sum := summarizeUsage(rows)

	// Total cost: row1 (1000*1.5/1e6 + 200*4.5/1e6 + 800*0.05/1e6) = 0.0015+0.0009+0.00004
	// row3: 2000*3/1e6 = 0.006
	if sum.TotalCalls != 3 {
		t.Fatalf("TotalCalls = %d, want 3", sum.TotalCalls)
	}
	// sort/order of days: newest first → 08-17, 08-16
	if len(sum.Days) != 2 {
		t.Fatalf("len(days) = %d, want 2", len(sum.Days))
	}
	if sum.Days[0].Date != "2026-08-17" || sum.Days[1].Date != "2026-08-16" {
		t.Fatalf("day order wrong: %v", sum.Days)
	}
	// 08-17 has unpriced = 1
	if sum.Days[0].Unpriced != 1 {
		t.Fatalf("08-17 unpriced = %d, want 1", sum.Days[0].Unpriced)
	}
	// by_kind grouping
	if sum.ByKind[string(llm.UsageKindConversation)].Calls != 2 {
		t.Fatalf("conversation calls = %d, want 2", sum.ByKind[string(llm.UsageKindConversation)].Calls)
	}
	if got := sum.ByKind[string(llm.UsageKindCommit)].Cost; got != 0 {
		t.Fatalf("commit cost = %v, want 0 (unpriced)", got)
	}
	if _, ok := sum.ByKind[string(llm.UsageKindCommit)]; !ok {
		t.Fatal("commit kind should be present")
	}
	// by_model grouping (same model differs by day)
	if sum.ByModel["deepseek-v4-flash"].Calls != 2 {
		t.Fatalf("deepseek calls = %d, want 2", sum.ByModel["deepseek-v4-flash"].Calls)
	}
}

func TestSummarizeUsageEmpty(t *testing.T) {
	sum := summarizeUsage(nil)
	if sum.TotalCalls != 0 || sum.TotalCost != 0 {
		t.Fatalf("empty summary should be zero, got %+v", sum)
	}
	if sum.ByKind == nil || sum.ByModel == nil {
		t.Fatal("maps should be initialized (non-nil)")
	}
}

func TestAPIKeyMatches(t *testing.T) {
	s := &Server{Cfg: configWebAPIKey("s3cr3t")}
	if !s.apiKeyMatches("s3cr3t") {
		t.Fatal("exact key should match")
	}
	if s.apiKeyMatches("S3CR3T") || s.apiKeyMatches("s3cr3t ") || s.apiKeyMatches("") {
		t.Fatal("wrong key should not match")
	}
}

func TestAPIKeyDisabledWhenEmpty(t *testing.T) {
	s := &Server{Cfg: configWebAPIKey("")}
	if !s.apiKeyMatches("") {
		t.Fatal("empty configured key disables auth")
	}
}
