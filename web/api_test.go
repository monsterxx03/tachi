package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
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

// listSessionsResponse mirrors the JSON shape of GET /api/sessions.
type listSessionsResponse struct {
	Sessions   []sessionsListItem `json:"sessions"`
	Total      int                `json:"total"`
	NextCursor string             `json:"next_cursor"`
}

// newListSessionsServer builds a Server over a temp dir with n sessions
// whose IDs are time-prefixed in ascending order (oldest first). The newest
// session gets a small message history (one user + one tool_call).
func newListSessionsServer(t *testing.T, n int) *Server {
	t.Helper()
	dir := t.TempDir()
	store, err := session.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		// ID prefix encodes the creation time; 2026-08-01 + i days.
		created := time.Date(2026, 8, 1+i, 9, 0, 0, 0, time.Local)
		id := fmt.Sprintf("%04d-%02d-%02d-%02d%02d%02d-%08x",
			created.Year(), created.Month(), created.Day(),
			created.Hour(), created.Minute(), created.Second(), i)
		ids = append(ids, id)
		sess := &session.Session{
			ID:        id,
			Title:     fmt.Sprintf("session-%d", i),
			CreatedAt: created,
			UpdatedAt: created.Add(time.Hour),
		}
		if err := store.CreateSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	// The newest session (last created) gets 1 user + 1 tool_call message.
	if n > 0 {
		for _, m := range []session.MessageType{session.MessageTypeUser, session.MessageTypeToolCall} {
			if err := store.AppendMessage(ids[n-1], &session.Message{Type: m, Timestamp: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &Server{Store: store, Usage: llm.NewUsageRecorder(filepath.Join(dir, "usage"))}
}

func listSessions(t *testing.T, s *Server, query string) listSessionsResponse {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/sessions"+query, nil)
	rr := httptest.NewRecorder()
	s.handleListSessions(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp listSessionsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestListSessionsPagination(t *testing.T) {
	s := newListSessionsServer(t, 5)

	// First page: newest 2 first, by creation time (ID) descending.
	p1 := listSessions(t, s, "?limit=2")
	if p1.Total != 5 {
		t.Fatalf("total = %d, want 5", p1.Total)
	}
	if len(p1.Sessions) != 2 {
		t.Fatalf("page size = %d, want 2", len(p1.Sessions))
	}
	if p1.Sessions[0].ID <= p1.Sessions[1].ID {
		t.Fatalf("page not sorted descending: %s before %s", p1.Sessions[0].ID, p1.Sessions[1].ID)
	}
	if p1.NextCursor != p1.Sessions[1].ID {
		t.Fatalf("next_cursor = %q, want last id %q", p1.NextCursor, p1.Sessions[1].ID)
	}
	// Newest session's stats: 1 user + 1 tool_call.
	if got := p1.Sessions[0].MessageCount; got != 2 {
		t.Fatalf("newest message_count = %d, want 2", got)
	}
	if got := p1.Sessions[0].ToolCalls; got != 1 {
		t.Fatalf("newest tool_calls = %d, want 1", got)
	}

	// Walk the rest with the cursor; pages must not overlap or skip.
	seen := map[string]bool{}
	for _, s := range p1.Sessions {
		seen[s.ID] = true
	}
	cursor := p1.NextCursor
	for pages := 1; cursor != "" && pages < 10; pages++ {
		page := listSessions(t, s, "?limit=2&cursor="+cursor)
		if len(page.Sessions) == 0 {
			t.Fatalf("non-empty next_cursor but empty page (cursor=%q)", cursor)
		}
		for _, item := range page.Sessions {
			if seen[item.ID] {
				t.Fatalf("duplicate session %s across pages", item.ID)
			}
			seen[item.ID] = true
		}
		cursor = page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("walked %d unique sessions, want 5", len(seen))
	}

	// Last page has no next_cursor.
	last := listSessions(t, s, "?limit=5")
	if last.NextCursor != "" {
		t.Fatalf("full-page fetch should have empty next_cursor, got %q", last.NextCursor)
	}
}

func TestListSessionsEmpty(t *testing.T) {
	s := newListSessionsServer(t, 0)
	resp := listSessions(t, s, "")
	if resp.Total != 0 || len(resp.Sessions) != 0 || resp.NextCursor != "" {
		t.Fatalf("empty list = %+v, want zero sessions/total/cursor", resp)
	}
}
