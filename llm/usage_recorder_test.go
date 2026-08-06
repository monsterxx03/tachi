package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubRecordingProvider is a minimal Provider for recording tests.
type stubRecordingProvider struct {
	name         string
	providerName string // config name (Provider.ProviderName); "" = unknown
	model        string
	resp         *Response
	stream       []StreamEvent
	chatErr      error
}

func (s *stubRecordingProvider) Name() string         { return s.name }
func (s *stubRecordingProvider) ProviderName() string { return s.providerName }
func (s *stubRecordingProvider) Model() string        { return s.model }

func (s *stubRecordingProvider) CreateChat(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (*Response, error) {
	if s.chatErr != nil {
		return nil, s.chatErr
	}
	if s.resp == nil {
		return &Response{Content: "ok"}, nil
	}
	return s.resp, nil
}

func (s *stubRecordingProvider) CreateChatStream(ctx context.Context, messages []Message, tools []Tool, opts ChatOptions) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, len(s.stream)+1)
	for _, ev := range s.stream {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func TestRecordingProvider_CreateChat(t *testing.T) {
	dir := t.TempDir()
	rec := NewUsageRecorder(dir)
	inner := &stubRecordingProvider{
		name:         ProviderTypeOpenAI,
		providerName: "deepseek-v4-flash",
		model:        "deepseek-chat",
		resp: &Response{
			Content: "hi",
			Usage: &Usage{
				InputTokens:          1000,
				OutputTokens:         200,
				CacheReadInputTokens: 700, // OpenAI reports input INCLUDING cache-read
			},
		},
	}
	p := WrapRecordingProvider(inner, rec, func(provider Provider, model string) *ModelPrice {
		return &ModelPrice{InputPrice: 1.0, OutputPrice: 2.0, CacheReadInputPrice: 0.02}
	})

	ctx := WithUsageKind(context.Background(), UsageKindCommit)
	ctx = WithSessionID(ctx, "sess-1")
	resp, err := p.CreateChat(ctx, nil, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	if resp.Content != "hi" {
		t.Fatalf("passthrough broken: %q", resp.Content)
	}

	rows, err := rec.Rows("sess-1", time.Time{})
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Rows = %d, want 1", len(rows))
	}
	row := rows[0]
	// OpenAI-family normalization: input is cache-miss only.
	if row.InputTokens != 300 {
		t.Errorf("InputTokens = %d, want 300 (1000 - 700 cache-read)", row.InputTokens)
	}
	if row.CacheReadInputTokens != 700 {
		t.Errorf("CacheReadInputTokens = %d, want 700", row.CacheReadInputTokens)
	}
	if row.Kind != UsageKindCommit {
		t.Errorf("Kind = %q, want commit", row.Kind)
	}
	if row.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", row.SessionID)
	}
	if row.Provider != "deepseek-v4-flash" || row.Model != "deepseek-chat" {
		t.Errorf("provider/model = %s/%s, want config name deepseek-v4-flash", row.Provider, row.Model)
	}
	if row.InputPrice != 1.0 || row.OutputPrice != 2.0 || row.CacheReadPrice != 0.02 {
		t.Errorf("price snapshot wrong: %+v", row)
	}
	// Cost: 300×1 + 700×0.02 + 200×2 (per 1M) — cache hit billed ONCE.
	want := 300.0/1e6*1.0 + 700.0/1e6*0.02 + 200.0/1e6*2.0
	if got := row.Cost(); got != want {
		t.Errorf("Cost = %v, want %v", got, want)
	}
}

func TestRecordingProvider_CreateChat_AnthropicNoNormalize(t *testing.T) {
	dir := t.TempDir()
	rec := NewUsageRecorder(dir)
	inner := &stubRecordingProvider{
		name:  ProviderTypeAnthropic,
		model: "claude-x",
		resp: &Response{
			Usage: &Usage{
				InputTokens:              500, // Anthropic input excludes cache
				OutputTokens:             100,
				CacheReadInputTokens:     200,
				CacheCreationInputTokens: 50,
			},
		},
	}
	p := WrapRecordingProvider(inner, rec, nil) // nil price → unpriced row, tokens still tracked
	ctx := context.Background()
	if _, err := p.CreateChat(ctx, nil, nil, ChatOptions{}); err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	rows, err := rec.Rows("", time.Time{})
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Rows = %d, want 1", len(rows))
	}
	if rows[0].InputTokens != 500 {
		t.Errorf("Anthropic input must NOT be normalized: got %d, want 500", rows[0].InputTokens)
	}
	if !rows[0].Unpriced() {
		t.Error("nil price resolver → row must be unpriced")
	}
}

func TestRecordingProvider_CreateChatStream_PassthroughAndRecord(t *testing.T) {
	dir := t.TempDir()
	rec := NewUsageRecorder(dir)
	inner := &stubRecordingProvider{
		name:  ProviderTypeOpenAI,
		model: "deepseek-chat",
		stream: []StreamEvent{
			{Type: StreamEventTextDelta, TextDelta: "hel"},
			{Type: StreamEventTextDelta, TextDelta: "lo"},
			{Type: StreamEventDone, FinishReason: "stop", Usage: &Usage{InputTokens: 100, OutputTokens: 10}},
		},
	}
	p := WrapRecordingProvider(inner, rec, func(Provider, string) *ModelPrice { return &ModelPrice{InputPrice: 1, OutputPrice: 2} })
	ch, err := p.CreateChatStream(context.Background(), nil, nil, ChatOptions{})
	if err != nil {
		t.Fatalf("CreateChatStream: %v", err)
	}
	var got []StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 3 || got[0].TextDelta != "hel" || got[1].TextDelta != "lo" || got[2].Type != StreamEventDone {
		t.Fatalf("passthrough corrupted: %+v", got)
	}
	rows, _ := rec.Rows("", time.Time{})
	if len(rows) != 1 || rows[0].InputTokens != 100 {
		t.Fatalf("stream usage not recorded exactly once: %+v", rows)
	}
}

func TestRecordingProvider_ErrorsNotRecorded(t *testing.T) {
	dir := t.TempDir()
	rec := NewUsageRecorder(dir)
	inner := &stubRecordingProvider{chatErr: errors.New("boom")}
	p := WrapRecordingProvider(inner, rec, nil)
	_, err := p.CreateChat(context.Background(), nil, nil, ChatOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	rows, _ := rec.Rows("", time.Time{})
	if len(rows) != 0 {
		t.Fatalf("error must not be recorded: %+v", rows)
	}
}

func TestRecordingProvider_SubagentCompositeID(t *testing.T) {
	dir := t.TempDir()
	rec := NewUsageRecorder(dir)
	inner := &stubRecordingProvider{
		name: ProviderTypeOpenAI, model: "m",
		resp: &Response{Usage: &Usage{InputTokens: 10, OutputTokens: 1}},
	}
	p := WrapRecordingProvider(inner, rec, nil)

	// Composite "parent:short" → parent session.
	ctx := WithUsageKind(context.Background(), UsageKindSubagent)
	ctx = WithSessionID(ctx, "sess-9:ab12")
	if _, err := p.CreateChat(ctx, nil, nil, ChatOptions{}); err != nil {
		t.Fatal(err)
	}
	// Bare shortID (no parent) → global bucket (empty session_id).
	ctx2 := WithUsageKind(context.Background(), UsageKindSubagent)
	ctx2 = WithSessionID(ctx2, "cd34")
	if _, err := p.CreateChat(ctx2, nil, nil, ChatOptions{}); err != nil {
		t.Fatal(err)
	}

	parentRows, _ := rec.Rows("sess-9", time.Time{})
	if len(parentRows) != 1 {
		t.Fatalf("composite ID rows for parent = %d, want 1", len(parentRows))
	}
	globalRows, _ := rec.Rows("", time.Time{})
	if len(globalRows) != 1 || globalRows[0].SessionID != "" {
		t.Fatalf("bare shortID must fall to global: %+v", globalRows)
	}
	// The composite row must NOT appear under its own ID.
	if orphan, _ := rec.Rows("sess-9:ab12", time.Time{}); len(orphan) != 0 {
		t.Fatalf("composite ID leaked: %+v", orphan)
	}
}

func TestUsageRecorder_DayRotationAndScanLowerBound(t *testing.T) {
	dir := t.TempDir()
	rec := NewUsageRecorder(dir)

	rec.Record(UsageRow{TS: time.Date(2026, 8, 4, 23, 59, 59, 0, time.Local), SessionID: "s-a"})
	rec.Record(UsageRow{TS: time.Date(2026, 8, 5, 0, 0, 1, 0, time.Local), SessionID: "s-a"})
	rec.Record(UsageRow{TS: time.Date(2026, 8, 5, 12, 0, 0, 0, time.Local), SessionID: "s-b"})

	// Day split: two files.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("day files = %d, want 2 (rotation failed): %v", len(entries), entries)
	}

	// Lower bound = 2026-08-05 skips the 08-04 file.
	rows, err := rec.Rows("s-a", time.Date(2026, 8, 5, 0, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TS.Day() != 5 {
		t.Fatalf("scan lower bound failed: %+v", rows)
	}

	// No lower bound sees all s-a rows.
	all, _ := rec.Rows("s-a", time.Time{})
	if len(all) != 2 {
		t.Fatalf("full scan s-a = %d, want 2", len(all))
	}
}

func TestUsageRecorder_ConcurrentWritesNoTear(t *testing.T) {
	dir := t.TempDir()
	rec := NewUsageRecorder(dir)

	const goroutines = 8
	const perG = 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				sid := fmt.Sprintf("sess-%d", g)
				if err := rec.Record(UsageRow{TS: time.Now(), SessionID: sid, Kind: UsageKindConversation, InputTokens: int64(i)}); err != nil {
					t.Errorf("record: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// Every line must parse and the total count must be exact.
	data, err := os.ReadFile(filepath.Join(dir, time.Now().Format("2006-01-02")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != goroutines*perG {
		t.Fatalf("lines = %d, want %d", len(lines), goroutines*perG)
	}
	for _, line := range lines {
		var row UsageRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("torn line: %q (%v)", line, err)
		}
	}
}

func TestUsageRecorder_FilePerm0600(t *testing.T) {
	dir := t.TempDir()
	rec := NewUsageRecorder(dir)
	if err := rec.Record(UsageRow{TS: time.Now(), SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, time.Now().Format("2006-01-02")+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file perm = %o, want 600", perm)
	}
}

// TestWrapRecordingProvider_Idempotent: re-wrapping with the same recorder
// must return the same instance — repeated wrapping at different
// provider-creation points can never double-record a call.
func TestWrapRecordingProvider_Idempotent(t *testing.T) {
	rec := NewUsageRecorder(t.TempDir())
	inner := &stubRecordingProvider{name: ProviderTypeOpenAI, model: "m",
		resp: &Response{Usage: &Usage{InputTokens: 10, OutputTokens: 1}}}
	wrapped := WrapRecordingProvider(inner, rec, nil)
	if again := WrapRecordingProvider(wrapped, rec, nil); again != wrapped {
		t.Error("WrapRecordingProvider not idempotent for the same recorder")
	}
	// Different recorder → new wrapper (still safe: distinct ledgers).
	other := NewUsageRecorder(t.TempDir())
	if again := WrapRecordingProvider(wrapped, other, nil); again == wrapped {
		t.Error("different recorder should produce a new wrapper")
	}
}

// TestNormalizeUsageKind: the review-round-N drift folds back to review.
func TestNormalizeUsageKind(t *testing.T) {
	cases := []struct {
		in, want UsageKind
	}{
		{UsageKindReview, UsageKindReview},
		{"review-round-1", UsageKindReview},
		{"review-round-9", UsageKindReview},
		{UsageKindCommit, UsageKindCommit},
		{UsageKindConversation, UsageKindConversation},
	}
	for _, c := range cases {
		if got := normalizeUsageKind(c.in); got != c.want {
			t.Errorf("normalizeUsageKind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
