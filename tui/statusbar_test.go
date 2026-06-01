package tui

import (
	"strings"
	"testing"

	cmds "github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/llm"
)

// ---- helpers ----

// sb returns a StatusBar with predictable defaults for testing.
func sb() StatusBar {
	return NewStatusBar("openai/gpt-4o", 128_000)
}

func withState(st state) func(*StatusBar) {
	return func(s *StatusBar) { s.state = st }
}

func withWidth(w int) func(*StatusBar) {
	return func(s *StatusBar) { s.width = w }
}

func withUsage(input, output int64) func(*StatusBar) {
	return func(s *StatusBar) {
		s.totalUsage = &llm.Usage{InputTokens: input, LastInputTokens: input, OutputTokens: output}
	}
}

func withSession(title, id string) func(*StatusBar) {
	return func(s *StatusBar) { s.sessionTitle = title; s.sessionID = id }
}

func withCopyMode(on bool) func(*StatusBar) {
	return func(s *StatusBar) { s.copyMode = on }
}

func withPending(n int) func(*StatusBar) {
	return func(s *StatusBar) { s.pendingCount = n }
}

func withProvider(info string) func(*StatusBar) {
	return func(s *StatusBar) { s.providerInfo = info }
}

func withContextWindow(cw int64) func(*StatusBar) {
	return func(s *StatusBar) { s.contextWindow = cw }
}

func makeStatusBar(opts ...func(*StatusBar)) StatusBar {
	s := sb()
	for _, o := range opts {
		o(&s)
	}
	return s
}

// ---- View basics ----

func TestStatusBar_View_Empty(t *testing.T) {
	s := makeStatusBar(withWidth(120))
	view := s.View()
	if !strings.Contains(view, "tachi") {
		t.Error("View should contain 'tachi'")
	}
	if !strings.Contains(view, "openai/gpt-4o") {
		t.Error("View should contain provider info")
	}
}

func TestStatusBar_View_ZeroWidth(t *testing.T) {
	s := makeStatusBar(withWidth(0))
	view := s.View()
	// Should not panic; statusBarStyle.Width(0) handles it gracefully.
	if view == "" {
		t.Skip("zero-width produces empty view — acceptable")
	}
}

// ---- State indicators ----

func TestStatusBar_View_IdleState(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle))
	view := s.View()
	if !strings.Contains(view, "●") {
		t.Error("idle state should show a dot indicator")
	}
}

func TestStatusBar_View_WaitingState(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateWaiting))
	view := s.View()
	// Waiting state shows the spinner (which renders something non-empty).
	if view == "" {
		t.Error("waiting state should render")
	}
}

func TestStatusBar_View_StreamingState(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateStreaming))
	view := s.View()
	if view == "" {
		t.Error("streaming state should render")
	}
}

func TestStatusBar_View_SelectingModelState(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateSelectingModel))
	view := s.View()
	// selectingModel uses the same dot as waiting.
	if !strings.Contains(view, "●") {
		t.Error("selecting model state should show dot indicator")
	}
}

func TestStatusBar_View_SelectingSessionState(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateSelectingSession))
	view := s.View()
	if !strings.Contains(view, "●") {
		t.Error("selecting session state should show dot indicator")
	}
}

func TestStatusBar_View_AwaitingConfirmationState(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateAwaitingConfirmation))
	view := s.View()
	if !strings.Contains(view, "●") {
		t.Error("awaiting confirmation state should show dot indicator")
	}
}

func TestStatusBar_View_AskUserQuestionState(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateAskUserQuestion))
	view := s.View()
	if !strings.Contains(view, "●") {
		t.Error("ask user question state should show dot indicator")
	}
}

// ---- Session info ----

func TestStatusBar_View_SessionInfo(t *testing.T) {
	s := makeStatusBar(
		withWidth(120),
		withSession("My test chat", "2026-05-09-143052-a1b2c3d4"),
	)
	view := s.View()
	if !strings.Contains(view, "My test chat") {
		t.Error("View should contain session title")
	}
	if !strings.Contains(view, "a1b2c3d4") {
		t.Error("View should contain session ID suffix")
	}
	// Should NOT contain the full session ID
	if strings.Contains(view, "2026-05-09-143052") {
		t.Error("View should only show the uuid suffix, not the full session ID")
	}
}

func TestStatusBar_View_SessionInfo_Untitled(t *testing.T) {
	s := makeStatusBar(
		withWidth(120),
		withSession("", "2026-05-09-143052-a1b2c3d4"),
	)
	view := s.View()
	if !strings.Contains(view, "(untitled)") {
		t.Error("View should show '(untitled)' when title is empty")
	}
}

func TestStatusBar_View_SessionInfo_ShortID(t *testing.T) {
	s := makeStatusBar(
		withWidth(120),
		withSession("chat", "abc123"),
	)
	view := s.View()
	// ID has no "-" separator, so the whole thing is used as suffix
	if !strings.Contains(view, "abc123") {
		t.Error("View should show the full short ID")
	}
}

func TestStatusBar_View_SessionInfo_NoID(t *testing.T) {
	s := makeStatusBar(withWidth(120))
	// sessionID is empty by default — no session info rendered
	view := s.View()
	if strings.Contains(view, "·") {
		t.Error("View should NOT show session separator when no session ID")
	}
}

// ---- Session info truncated in tight views ----

func TestStatusBar_View_SessionTruncatesInNarrowTerminal(t *testing.T) {
	s := makeStatusBar(
		withWidth(60),
		withSession("A very long session title that should truncate", "2026-05-09-143052-abc12345"),
		withState(stateIdle),
	)
	view := s.View()

	// Verify the title is in the view (truncated, not full)
	if strings.Contains(view, "A very long session") {
		// OK — title was rendered but should be truncated to at most maxSessionTitleLen runes
		full := "A very long session title that should truncate"
		if strings.Contains(view, full) {
			t.Error("long title should be truncated in view")
		}
	}
	// Verify session suffix appears
	if !strings.Contains(view, "abc12345") {
		t.Error("session ID suffix should appear")
	}
}

// ---- Title truncation ----

func TestStatusBar_TruncateTitle_Short(t *testing.T) {
	s := sb()
	got := s.truncateTitle("hello")
	if got != "hello" {
		t.Errorf("truncateTitle: got %q want %q", got, "hello")
	}
}

func TestStatusBar_TruncateTitle_ExactlyMax(t *testing.T) {
	s := sb()
	title := strings.Repeat("a", maxSessionTitleLen)
	got := s.truncateTitle(title)
	if got != title {
		t.Errorf("truncateTitle: exactly max should not truncate, got %q", got)
	}
}

func TestStatusBar_TruncateTitle_OverMax(t *testing.T) {
	s := sb()
	title := strings.Repeat("a", maxSessionTitleLen+5)
	got := s.truncateTitle(title)
	expected := strings.Repeat("a", maxSessionTitleLen-1) + "…"
	if got != expected {
		t.Errorf("truncateTitle: got %q want %q", got, expected)
	}
}

func TestStatusBar_TruncateTitle_Unicode(t *testing.T) {
	s := sb()
	title := "你好世界你好世界你好世界你好世界你好世界你好世界你好世界你好世界xx"
	got := s.truncateTitle(title)
	// Should be rune-aware, not break in the middle of a CJK character
	if len([]rune(got)) > maxSessionTitleLen {
		t.Errorf("truncateTitle: CJK title should be at most %d runes, got %d", maxSessionTitleLen, len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("truncateTitle: truncated title should end with '…'")
	}
}

// ---- Context window usage ----

func TestStatusBar_View_UsageNormal(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle), withUsage(10_000, 500))
	view := s.View()
	if !strings.Contains(view, "ctx:") {
		t.Error("View should show context usage when input tokens > 0")
	}
	if !strings.Contains(view, "10.0k") {
		t.Error("View should format 10_000 as '10.0k'")
	}
	if !strings.Contains(view, "128.0k") {
		t.Error("View should format 128_000 as '128.0k'")
	}
}

func TestStatusBar_View_UsageWarning(t *testing.T) {
	// 50% exactly should be warning
	s := makeStatusBar(withWidth(120), withState(stateIdle), withUsage(64_000, 500))
	view := s.View()
	if !strings.Contains(view, "64.0k") {
		t.Error("View should show 64.0k input tokens")
	}
	// 50% should use warning (yellow) style, not normal (green)
	if !strings.Contains(view, "50%") {
		t.Error("View should show 50%")
	}
}

func TestStatusBar_View_UsageHigh(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle), withUsage(110_000, 500))
	view := s.View()
	if !strings.Contains(view, "110.0k") {
		t.Error("View should show 110.0k input tokens")
	}
	// 86% should use high (red) style
	if !strings.Contains(view, "86%") {
		t.Error("View should show 86%")
	}
}

func TestStatusBar_View_UsageNearFull(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle), withUsage(127_950, 500))
	view := s.View()
	// >= 99.95% should show "~100%"
	if !strings.Contains(view, "~100%") {
		t.Errorf("usage at 99.95%%+ should show ~100%%: got %q", view)
	}
}

func TestStatusBar_View_NoUsageWhenZero(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle), withUsage(0, 0))
	view := s.View()
	if strings.Contains(view, "ctx:") {
		t.Error("View should NOT show context usage when input tokens are 0")
	}
}

func TestStatusBar_View_NoUsageWhenNil(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle))
	// totalUsage is nil by default
	view := s.View()
	if strings.Contains(view, "ctx:") {
		t.Error("View should NOT show context usage when usage is nil")
	}
}

func TestStatusBar_View_NoUsageWhenContextWindowZero(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle), withUsage(10_000, 500), withContextWindow(0))
	view := s.View()
	if strings.Contains(view, "ctx:") {
		t.Error("View should NOT show context usage when context window is 0")
	}
}

// ---- Token formatting ----

func TestFormatTokens_Zero(t *testing.T) {
	if got := cmds.FormatTokens(0); got != "0" {
		t.Errorf("FormatTokens(0) = %q, want %q", got, "0")
	}
}

func TestFormatTokens_Small(t *testing.T) {
	if got := cmds.FormatTokens(42); got != "42" {
		t.Errorf("FormatTokens(42) = %q, want %q", got, "42")
	}
}

func TestFormatTokens_Kilo(t *testing.T) {
	if got := cmds.FormatTokens(1_000); got != "1.0k" {
		t.Errorf("FormatTokens(1000) = %q, want %q", got, "1.0k")
	}
	if got := cmds.FormatTokens(2_500); got != "2.5k" {
		t.Errorf("FormatTokens(2500) = %q, want %q", got, "2.5k")
	}
	if got := cmds.FormatTokens(128_000); got != "128.0k" {
		t.Errorf("FormatTokens(128000) = %q, want %q", got, "128.0k")
	}
}

func TestFormatTokens_Million(t *testing.T) {
	if got := cmds.FormatTokens(1_000_000); got != "1.0M" {
		t.Errorf("FormatTokens(1000000) = %q, want %q", got, "1.0M")
	}
	if got := cmds.FormatTokens(2_500_000); got != "2.5M" {
		t.Errorf("FormatTokens(2500000) = %q, want %q", got, "2.5M")
	}
}

func TestFormatPercent_Normal(t *testing.T) {
	if got := formatPercent(0); got != "0%" {
		t.Errorf("formatPercent(0) = %q, want %q", got, "0%")
	}
	if got := formatPercent(42.7); got != "43%" {
		t.Errorf("formatPercent(42.7) = %q, want %q", got, "43%")
	}
}

func TestFormatPercent_NearFull(t *testing.T) {
	if got := formatPercent(99.95); got != "~100%" {
		t.Errorf("formatPercent(99.95) = %q, want %q", got, "~100%")
	}
	if got := formatPercent(99.999); got != "~100%" {
		t.Errorf("formatPercent(99.999) = %q, want %q", got, "~100%")
	}
}

// ---- Usage color style ----

func TestUsageColorStyle_Normal(t *testing.T) {
	s := usageColorStyle(30)
	// We can't easily compare lipgloss.Style structs, but we can call Render
	rendered := s.Render("test")
	if rendered == "" {
		t.Error("normal style should render")
	}
}

func TestUsageColorStyle_Warning(t *testing.T) {
	s := usageColorStyle(50)
	rendered := s.Render("test")
	if rendered == "" {
		t.Error("warning style should render")
	}
}

func TestUsageColorStyle_High(t *testing.T) {
	s := usageColorStyle(80)
	rendered := s.Render("test")
	if rendered == "" {
		t.Error("high style should render")
	}
}

// ---- Copy mode ----

func TestStatusBar_View_CopyMode(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle), withCopyMode(true))
	view := s.View()
	if !strings.Contains(view, "SELECT") {
		t.Error("View should show 'SELECT' when copy mode is on")
	}
}

func TestStatusBar_View_CopyModeOff(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle), withCopyMode(false))
	view := s.View()
	if strings.Contains(view, "SELECT") {
		t.Error("View should NOT show 'SELECT' when copy mode is off")
	}
}

// ---- Pending count ----

func TestStatusBar_View_PendingCount(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle), withPending(3))
	view := s.View()
	if !strings.Contains(view, "⏳ 3 pending") {
		t.Errorf("View should show pending count: got %q", view)
	}
}

func TestStatusBar_View_NoPending(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle), withPending(0))
	view := s.View()
	if strings.Contains(view, "pending") {
		t.Error("View should NOT show pending when count is 0")
	}
}

// ---- Provider info ----

func TestStatusBar_View_ProviderInfo(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateIdle), withProvider("anthropic/claude-sonnet-4-20250514"))
	view := s.View()
	if !strings.Contains(view, "anthropic/claude-sonnet-4-20250514") {
		t.Error("View should show provider info")
	}
}

func TestStatusBar_ProviderInfoGetter(t *testing.T) {
	s := makeStatusBar(withProvider("test/model"))
	if got := s.ProviderInfo(); got != "test/model" {
		t.Errorf("ProviderInfo() = %q, want %q", got, "test/model")
	}
}

// ---- Setters ----

func TestStatusBar_Setters(t *testing.T) {
	s := sb()

	s.SetWidth(42)
	if s.width != 42 {
		t.Errorf("SetWidth: want 42, got %d", s.width)
	}

	s.SetState(stateStreaming)
	if s.state != stateStreaming {
		t.Errorf("SetState: want stateStreaming, got %v", s.state)
	}

	u := &llm.Usage{InputTokens: 100, OutputTokens: 200}
	s.SetUsage(u)
	if s.totalUsage != u {
		t.Error("SetUsage should store the pointer")
	}

	s.SetCopyMode(true)
	if !s.copyMode {
		t.Error("SetCopyMode(true) should set copyMode")
	}

	s.SetProviderInfo("new/provider")
	if s.providerInfo != "new/provider" {
		t.Errorf("SetProviderInfo: got %q", s.providerInfo)
	}

	s.SetContextWindow(200_000)
	if s.contextWindow != 200_000 {
		t.Errorf("SetContextWindow: want 200000, got %d", s.contextWindow)
	}

	s.SetSessionInfo("title", "session-id")
	if s.sessionTitle != "title" || s.sessionID != "session-id" {
		t.Errorf("SetSessionInfo: got (%q, %q)", s.sessionTitle, s.sessionID)
	}

	s.SetPendingCount(5)
	if s.pendingCount != 5 {
		t.Errorf("SetPendingCount: want 5, got %d", s.pendingCount)
	}
}

// ---- Combined view scenarios ----

func TestStatusBar_View_FullRichState(t *testing.T) {
	// All optional elements active at once
	s := makeStatusBar(
		withWidth(120),
		withState(stateStreaming),
		withUsage(60_000, 3_000),
		withSession("Debugging session", "2026-05-09-143052-abc12345"),
		withPending(2),
		withProvider("openai/gpt-4o"),
	)
	view := s.View()

	if !strings.Contains(view, "tachi") {
		t.Error("should show app name")
	}
	if !strings.Contains(view, "Debugging session") {
		t.Error("should show session title")
	}
	if !strings.Contains(view, "abc12345") {
		t.Error("should show session ID suffix")
	}
	if !strings.Contains(view, "60.0k") {
		t.Error("should show input tokens")
	}
	if !strings.Contains(view, "⏳ 2 pending") {
		t.Error("should show pending count")
	}
	if !strings.Contains(view, "openai/gpt-4o") {
		t.Error("should show provider info")
	}
}

func TestStatusBar_View_UpdateSpinner(t *testing.T) {
	s := makeStatusBar(withWidth(120), withState(stateWaiting))
	// Simulate a spinner tick
	cmd := s.Tick()
	if cmd == nil {
		t.Error("Tick() should return a command when not idle")
	}
}

func TestStatusBar_Update(t *testing.T) {
	s := makeStatusBar(withState(stateWaiting))
	// Send a tick message — just verify it doesn't panic
	_ = s.Tick()
	// The returned command should work; we can't easily inspect the spinner state
	// but verifying no panic is valuable.
}

// ---- Edge cases ----

func TestStatusBar_View_VeryNarrowTerminal(t *testing.T) {
	s := makeStatusBar(
		withWidth(20),
		withState(stateIdle),
		withSession("Chat", "2026-05-09-143052-abc12345"),
		withUsage(10_000, 500),
	)
	view := s.View()
	// Should not panic, even if things get truncated
	if view != "" {
		// OK — lipgloss handles truncation
	}
}

func TestStatusBar_View_MillionTokens(t *testing.T) {
	s := makeStatusBar(
		withWidth(120),
		withState(stateIdle),
		withUsage(2_500_000, 50_000),
		withContextWindow(5_000_000),
	)
	view := s.View()
	if !strings.Contains(view, "2.5M") {
		t.Errorf("should format 2.5M input tokens: got %q", view)
	}
	if !strings.Contains(view, "5.0M") {
		t.Errorf("should format 5.0M context window: got %q", view)
	}
}

// ---- Cost display ----

func withCost(cost float64) func(*StatusBar) {
	return func(s *StatusBar) { s.sessionCost = cost }
}

func TestStatusBar_SetCost(t *testing.T) {
	s := makeStatusBar()
	s.SetCost(0.123)
	if s.sessionCost != 0.123 {
		t.Errorf("SetCost: want 0.123, got %v", s.sessionCost)
	}
}

func TestStatusBar_View_CostDisplay(t *testing.T) {
	s := makeStatusBar(
		withWidth(120),
		withState(stateIdle),
		withCost(0.123),
	)
	view := s.View()
	if !strings.Contains(view, "¥0.123") {
		t.Errorf("should show cost: got %q", view)
	}
}

func TestStatusBar_View_NoCostWhenZero(t *testing.T) {
	s := makeStatusBar(
		withWidth(120),
		withState(stateIdle),
		withCost(0),
	)
	view := s.View()
	if strings.Contains(view, "¥") {
		t.Errorf("should NOT show cost when 0: got %q", view)
	}
}

func TestFormatCostCNY(t *testing.T) {
	tests := []struct {
		cost float64
		want string
	}{
		{0, ""},
		{0.0001, "<¥0.001"},
		{0.001, "¥0.001"},
		{0.123, "¥0.123"},
		{1.0, "¥1.000"},
		{10.5, "¥10.500"},
		{100.0, "¥100.000"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatCostCNY(tt.cost)
			if got != tt.want {
				t.Errorf("formatCostCNY(%v) = %q, want %q", tt.cost, got, tt.want)
			}
		})
	}
}
