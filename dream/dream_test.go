package dream

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/session"
)

func TestGroupSessionsByDomain(t *testing.T) {
	tmpDir := t.TempDir()
	gitDir := filepath.Join(tmpDir, "myproject")
	os.MkdirAll(filepath.Join(gitDir, ".git"), 0755)

	sessions := []*session.Session{
		{ID: "s1", WorkingDir: gitDir},
		{ID: "s2", WorkingDir: gitDir},
		{ID: "s3", WorkingDir: "/tmp/no-git"},
		{ID: "s4", WorkingDir: ""},
	}

	groups := GroupSessionsByDomain(sessions)

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	var projectGroup, globalGroup *SessionGroup
	for i := range groups {
		switch groups[i].Domain {
		case "project":
			projectGroup = &groups[i]
		case "global":
			globalGroup = &groups[i]
		}
	}

	if projectGroup == nil {
		t.Fatal("missing project group")
	}
	if globalGroup == nil {
		t.Fatal("missing global group")
	}

	if len(projectGroup.Sessions) != 2 {
		t.Errorf("project sessions: got %d, want 2", len(projectGroup.Sessions))
	}
	if len(globalGroup.Sessions) != 2 {
		t.Errorf("global sessions: got %d, want 2", len(globalGroup.Sessions))
	}

	if projectGroup.Root != gitDir {
		t.Errorf("project root: got %q, want %q", projectGroup.Root, gitDir)
	}
	expectedMemRoot := filepath.Join(gitDir, ".tachi", "memory")
	if projectGroup.MemoryRoot != expectedMemRoot {
		t.Errorf("project memory root: got %q, want %q", projectGroup.MemoryRoot, expectedMemRoot)
	}
}

func TestFilterSkippedSessions(t *testing.T) {
	sessions := []*session.Session{
		{ID: "s1", SkipDream: false},
		{ID: "s2", SkipDream: true},
		{ID: "s3", SkipDream: false},
		{ID: "s4", SkipDream: true},
	}

	filtered := FilterSkippedSessions(sessions)
	if len(filtered) != 2 {
		t.Fatalf("expected 2, got %d", len(filtered))
	}
	if filtered[0].ID != "s1" || filtered[1].ID != "s3" {
		t.Errorf("expected [s1, s3], got [%s, %s]", filtered[0].ID, filtered[1].ID)
	}
}

func TestActiveSessionsSince(t *testing.T) {
	now := time.Now()
	sessions := []*session.Session{
		{ID: "s1", UpdatedAt: now.Add(-1 * time.Hour)},  // 1h ago
		{ID: "s2", UpdatedAt: now.Add(-25 * time.Hour)}, // 25h ago
		{ID: "s3", UpdatedAt: now.Add(-2 * time.Hour)},  // 2h ago
		{ID: "s4", UpdatedAt: now.Add(-48 * time.Hour)}, // 48h ago
	}

	tests := []struct {
		name  string
		since time.Time
		want  int
	}{
		{"zero time (first dream) → all", time.Time{}, 4},
		{"since 3h ago → 2 active", now.Add(-3 * time.Hour), 2},
		{"since 26h ago → 3 active", now.Add(-26 * time.Hour), 3},
		{"since 1min ago → 0 active", now.Add(-1 * time.Minute), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ActiveSessionsSince(tt.since, sessions)
			if len(got) != tt.want {
				t.Errorf("ActiveSessionsSince: got %d, want %d", len(got), tt.want)
			}
		})
	}
}

func TestActiveSessionsSince_LongLivedChannelSession(t *testing.T) {
	// Simulates a channel-mode scenario: one session created weeks ago,
	// but updated today. Should be picked up by dream.
	now := time.Now()
	sessions := []*session.Session{
		{
			ID:        "channel-thread-1",
			CreatedAt: now.Add(-30 * 24 * time.Hour), // created 30 days ago
			UpdatedAt: now.Add(-2 * time.Hour),       // last message 2h ago
		},
	}

	lastDream := now.Add(-24 * time.Hour) // last dream was 24h ago
	active := ActiveSessionsSince(lastDream, sessions)

	if len(active) != 1 {
		t.Errorf("expected 1 active session (long-lived channel thread), got %d", len(active))
	}
}

func TestAcquireReleaseLock(t *testing.T) {
	tmpDir := t.TempDir()

	if !AcquireLock(tmpDir) {
		t.Fatal("first acquire should succeed")
	}

	if AcquireLock(tmpDir) {
		t.Fatal("second acquire should fail")
	}

	ReleaseLock(tmpDir)

	if !AcquireLock(tmpDir) {
		t.Fatal("acquire after release should succeed")
	}
	ReleaseLock(tmpDir)
}

func TestAcquireLock_StaleLock(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "dream.lock")

	staleContent := "999999999:" + time.Now().Add(-10*time.Minute).Format(time.RFC3339)
	os.WriteFile(lockPath, []byte(staleContent), 0644)

	if !AcquireLock(tmpDir) {
		t.Fatal("should acquire stale lock")
	}
	ReleaseLock(tmpDir)
}

func TestState_LoadSave(t *testing.T) {
	tmpDir := t.TempDir()

	state := LoadState(tmpDir)
	if !state.LastDreamAt.IsZero() {
		t.Errorf("expected zero LastDreamAt, got %v", state.LastDreamAt)
	}

	now := time.Now().Truncate(time.Second)
	state = State{
		LastDreamAt:     now,
		SessionsDreamed: 5,
		FactsAdded:      10,
	}
	if err := SaveState(tmpDir, state); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded := LoadState(tmpDir)
	if !loaded.LastDreamAt.Equal(now) {
		t.Errorf("LastDreamAt: got %v, want %v", loaded.LastDreamAt, now)
	}
	if loaded.SessionsDreamed != 5 {
		t.Errorf("SessionsDreamed: got %d, want 5", loaded.SessionsDreamed)
	}
	if loaded.FactsAdded != 10 {
		t.Errorf("FactsAdded: got %d, want 10", loaded.FactsAdded)
	}
}

func TestFindGitRoot(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "repo")
	os.MkdirAll(filepath.Join(repoDir, ".git"), 0755)
	subDir := filepath.Join(repoDir, "sub", "dir")
	os.MkdirAll(subDir, 0755)

	tests := []struct {
		dir  string
		want string
	}{
		{repoDir, repoDir},
		{subDir, repoDir},
		{tmpDir, ""},
		{"", ""},
	}

	for _, tt := range tests {
		got := FindGitRoot(tt.dir)
		if got != tt.want {
			t.Errorf("FindGitRoot(%q) = %q, want %q", tt.dir, got, tt.want)
		}
	}
}

func TestEnsureMemoryDir(t *testing.T) {
	tmpDir := t.TempDir()
	memRoot := filepath.Join(tmpDir, "memory")

	if err := EnsureMemoryDir(memRoot); err != nil {
		t.Fatalf("EnsureMemoryDir: %v", err)
	}

	info, err := os.Stat(filepath.Join(memRoot, "topics"))
	if err != nil {
		t.Fatalf("topics dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("topics is not a directory")
	}
}

func TestOrchestrator_Run_SkipsWhenNoActiveSessions(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "repo")
	os.MkdirAll(filepath.Join(repoDir, ".git"), 0755)

	now := time.Now()
	sessions := []*session.Session{
		{ID: "s1", WorkingDir: repoDir, UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: "s2", WorkingDir: repoDir, UpdatedAt: now.Add(-2 * time.Hour)},
	}

	// Pretend we dreamed 1 hour ago. Sessions are all updated before that,
	// so no session has activity since last dream → Gate 1 blocks.
	memRoot := filepath.Join(repoDir, ".tachi", "memory")
	os.MkdirAll(memRoot, 0755)
	SaveState(memRoot, State{LastDreamAt: time.Now().Add(-1 * time.Hour)})

	o := NewOrchestrator(Config{})

	var called bool
	err := o.Run(t.Context(), sessions, func(ctx context.Context, p Plan) (State, error) {
		called = true
		return State{LastDreamAt: time.Now()}, nil
	})

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Error("runFn should not have been called (no active sessions)")
	}
}

func TestOrchestrator_Run_PassesGates(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "repo")
	os.MkdirAll(filepath.Join(repoDir, ".git"), 0755)

	now := time.Now()
	sessions := []*session.Session{
		{ID: "s5", WorkingDir: repoDir, UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "s4", WorkingDir: repoDir, UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "s3", WorkingDir: repoDir, UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: "s2", WorkingDir: repoDir, UpdatedAt: now.Add(-4 * time.Hour)},
		{ID: "s1", WorkingDir: repoDir, UpdatedAt: now.Add(-5 * time.Hour)},
	}

	o := NewOrchestrator(Config{})

	var calledDomain string
	var receivedActive int
	err := o.Run(t.Context(), sessions, func(ctx context.Context, p Plan) (State, error) {
		calledDomain = p.Group.Domain
		receivedActive = len(p.ActiveSessions)
		return State{
			LastDreamAt: time.Now(),
			FactsAdded:  7,
		}, nil
	})

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calledDomain != "project" {
		t.Errorf("expected domain 'project', got %q", calledDomain)
	}
	// All 5 are active (first dream, LastDreamAt is zero → all sessions qualify).
	if receivedActive != 5 {
		t.Errorf("expected 5 active sessions, got %d", receivedActive)
	}

	// Verify state was persisted.
	memRoot := filepath.Join(repoDir, ".tachi", "memory")
	state := LoadState(memRoot)
	if state.FactsAdded != 7 {
		t.Errorf("persisted FactsAdded: got %d, want 7", state.FactsAdded)
	}
}

func TestOrchestrator_Run_OnlyPicksActiveSessions(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "repo")
	os.MkdirAll(filepath.Join(repoDir, ".git"), 0755)

	// Simulate: last dream was 25h ago.
	memRoot := filepath.Join(repoDir, ".tachi", "memory")
	os.MkdirAll(memRoot, 0755)
	lastDreamTime := time.Now().Add(-25 * time.Hour)
	SaveState(memRoot, State{LastDreamAt: lastDreamTime})

	now := time.Now()
	sessions := []*session.Session{
		// These 2 have activity after last dream → active
		{ID: "s3", WorkingDir: repoDir, UpdatedAt: now.Add(-1 * time.Hour)},
		{ID: "s2", WorkingDir: repoDir, UpdatedAt: now.Add(-10 * time.Hour)},
		// This one was last updated before the dream → not active
		{ID: "s1", WorkingDir: repoDir, UpdatedAt: now.Add(-48 * time.Hour)},
	}

	o := NewOrchestrator(Config{})

	var receivedActive int
	var activeIDs []string
	err := o.Run(t.Context(), sessions, func(ctx context.Context, p Plan) (State, error) {
		receivedActive = len(p.ActiveSessions)
		for _, s := range p.ActiveSessions {
			activeIDs = append(activeIDs, s.ID)
		}
		return State{LastDreamAt: time.Now()}, nil
	})

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if receivedActive != 2 {
		t.Errorf("expected 2 active sessions, got %d", receivedActive)
	}
	// s1 (updated 48h ago, before last dream 25h ago) should NOT be included.
	for _, id := range activeIDs {
		if id == "s1" {
			t.Error("s1 should not be in active sessions (updated before last dream)")
		}
	}
}

// --- buildSessionSummaries tests ---

func TestFilterSessionMessages_Timestamps(t *testing.T) {
	now := time.Now()
	t1 := now.Add(-3 * time.Hour)
	t2 := now.Add(-2 * time.Hour)
	t3 := now.Add(-1 * time.Hour)

	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "q1", Timestamp: t1},
		{Type: session.MessageTypeAssistant, Content: "a1", Timestamp: t1.Add(time.Second)},
		{Type: session.MessageTypeUser, Content: "q2", Timestamp: t2},
		{Type: session.MessageTypeAssistant, Content: "a2", Timestamp: t2.Add(time.Second)},
		{Type: session.MessageTypeUser, Content: "q3", Timestamp: t3},
		{Type: session.MessageTypeAssistant, Content: "a3", Timestamp: t3.Add(time.Second)},
	}

	pairs := FilterSessionMessages(msgs)
	if len(pairs) != 3 {
		t.Fatalf("expected 3 pairs, got %d", len(pairs))
	}

	if !pairs[0].Timestamp.Equal(t1) {
		t.Errorf("pair 0 timestamp: got %v, want %v", pairs[0].Timestamp, t1)
	}
	if !pairs[1].Timestamp.Equal(t2) {
		t.Errorf("pair 1 timestamp: got %v, want %v", pairs[1].Timestamp, t2)
	}
	if !pairs[2].Timestamp.Equal(t3) {
		t.Errorf("pair 2 timestamp: got %v, want %v", pairs[2].Timestamp, t3)
	}
}

func TestBuildSessionSummaries_FirstDream(t *testing.T) {
	// First dream: lastDreamAt is zero → all pairs included.
	now := time.Now()
	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "q1", Timestamp: now.Add(-3 * time.Hour)},
		{Type: session.MessageTypeAssistant, Content: "a1", Timestamp: now.Add(-3 * time.Hour)},
		{Type: session.MessageTypeUser, Content: "q2", Timestamp: now.Add(-2 * time.Hour)},
		{Type: session.MessageTypeAssistant, Content: "a2", Timestamp: now.Add(-2 * time.Hour)},
		{Type: session.MessageTypeUser, Content: "q3", Timestamp: now.Add(-1 * time.Hour)},
		{Type: session.MessageTypeAssistant, Content: "a3", Timestamp: now.Add(-1 * time.Hour)},
	}

	sessions := []*session.Session{
		{ID: "s1", Title: "test session"},
	}

	loadFn := func(id string) ([]session.Message, error) {
		return msgs, nil
	}

	summaries := buildSessionSummaries(sessions, loadFn, time.Time{}, nil)
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
	if len(summaries[0].Messages) != 3 {
		t.Errorf("first dream should include all 3 pairs, got %d", len(summaries[0].Messages))
	}
}

func TestBuildSessionSummaries_WindowFiltering(t *testing.T) {
	// lastDreamAt = 1.5h ago. Pairs at 3h, 2.5h, 2h, 1h (new), 30min (new).
	// Expected: pairs at 2.5h, 2h (2 context), 1h, 30min (2 new) = 4 total.
	now := time.Now()
	t3h := now.Add(-3 * time.Hour)
	t2_5h := now.Add(-150 * time.Minute)
	t2h := now.Add(-2 * time.Hour)
	t1h := now.Add(-1 * time.Hour)
	t30m := now.Add(-30 * time.Minute)

	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "old1", Timestamp: t3h},
		{Type: session.MessageTypeAssistant, Content: "old1a", Timestamp: t3h},
		{Type: session.MessageTypeUser, Content: "ctx2", Timestamp: t2_5h},
		{Type: session.MessageTypeAssistant, Content: "ctx2a", Timestamp: t2_5h},
		{Type: session.MessageTypeUser, Content: "ctx1", Timestamp: t2h},
		{Type: session.MessageTypeAssistant, Content: "ctx1a", Timestamp: t2h},
		{Type: session.MessageTypeUser, Content: "new1", Timestamp: t1h},
		{Type: session.MessageTypeAssistant, Content: "new1a", Timestamp: t1h},
		{Type: session.MessageTypeUser, Content: "new2", Timestamp: t30m},
		{Type: session.MessageTypeAssistant, Content: "new2a", Timestamp: t30m},
	}

	sessions := []*session.Session{
		{ID: "s1"},
	}

	loadFn := func(id string) ([]session.Message, error) {
		return msgs, nil
	}

	lastDreamAt := now.Add(-90 * time.Minute) // 1.5h ago

	summaries := buildSessionSummaries(sessions, loadFn, lastDreamAt, nil)
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}

	pairs := summaries[0].Messages
	if len(pairs) != 4 {
		t.Fatalf("expected 4 pairs (2 context + 2 new), got %d", len(pairs))
	}

	// First two should be context pairs.
	if pairs[0].User != "ctx2" {
		t.Errorf("pair 0 user: got %q, want ctx2", pairs[0].User)
	}
	if pairs[1].User != "ctx1" {
		t.Errorf("pair 1 user: got %q, want ctx1", pairs[1].User)
	}
	// Then the new ones.
	if pairs[2].User != "new1" {
		t.Errorf("pair 2 user: got %q, want new1", pairs[2].User)
	}
	if pairs[3].User != "new2" {
		t.Errorf("pair 3 user: got %q, want new2", pairs[3].User)
	}
}

func TestBuildSessionSummaries_FewContextPairs(t *testing.T) {
	// Only 1 pair before the first new one → clamp context to 1 pair.
	now := time.Now()
	t3h := now.Add(-3 * time.Hour)
	t30m := now.Add(-30 * time.Minute)

	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "ctx1", Timestamp: t3h},
		{Type: session.MessageTypeAssistant, Content: "ctx1a", Timestamp: t3h},
		{Type: session.MessageTypeUser, Content: "new1", Timestamp: t30m},
		{Type: session.MessageTypeAssistant, Content: "new1a", Timestamp: t30m},
	}

	sessions := []*session.Session{
		{ID: "s1"},
	}

	loadFn := func(id string) ([]session.Message, error) {
		return msgs, nil
	}

	lastDreamAt := now.Add(-1 * time.Hour) // 1h ago

	summaries := buildSessionSummaries(sessions, loadFn, lastDreamAt, nil)
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}

	pairs := summaries[0].Messages
	if len(pairs) != 2 {
		t.Fatalf("expected 2 pairs (1 context + 1 new), got %d", len(pairs))
	}
	if pairs[0].User != "ctx1" {
		t.Errorf("pair 0 user: got %q, want ctx1", pairs[0].User)
	}
	if pairs[1].User != "new1" {
		t.Errorf("pair 1 user: got %q, want new1", pairs[1].User)
	}
}

func TestBuildSessionSummaries_NoContextPairs(t *testing.T) {
	// First pair is the first new one → 0 context pairs, start at index 0.
	now := time.Now()
	t30m := now.Add(-30 * time.Minute)

	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "new1", Timestamp: t30m},
		{Type: session.MessageTypeAssistant, Content: "new1a", Timestamp: t30m},
		{Type: session.MessageTypeUser, Content: "new2", Timestamp: now.Add(-10 * time.Minute)},
		{Type: session.MessageTypeAssistant, Content: "new2a", Timestamp: now.Add(-10 * time.Minute)},
	}

	sessions := []*session.Session{
		{ID: "s1"},
	}

	loadFn := func(id string) ([]session.Message, error) {
		return msgs, nil
	}

	lastDreamAt := now.Add(-1 * time.Hour) // 1h ago

	summaries := buildSessionSummaries(sessions, loadFn, lastDreamAt, nil)
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}

	pairs := summaries[0].Messages
	if len(pairs) != 2 {
		t.Fatalf("expected 2 pairs (all new, no context needed), got %d", len(pairs))
	}
}

func TestBuildSessionSummaries_AllBeforeLastDream(t *testing.T) {
	// All pairs are before lastDreamAt → session is skipped.
	now := time.Now()
	t3h := now.Add(-3 * time.Hour)
	t2h := now.Add(-2 * time.Hour)

	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "old1", Timestamp: t3h},
		{Type: session.MessageTypeAssistant, Content: "old1a", Timestamp: t3h},
		{Type: session.MessageTypeUser, Content: "old2", Timestamp: t2h},
		{Type: session.MessageTypeAssistant, Content: "old2a", Timestamp: t2h},
	}

	sessions := []*session.Session{
		{ID: "s1"},
	}

	loadFn := func(id string) ([]session.Message, error) {
		return msgs, nil
	}

	lastDreamAt := now.Add(-1 * time.Hour) // 1h ago

	summaries := buildSessionSummaries(sessions, loadFn, lastDreamAt, nil)
	if len(summaries) != 0 {
		t.Errorf("expected 0 summaries (all pairs before lastDreamAt), got %d", len(summaries))
	}
}

func TestBuildSessionSummaries_SkipsThinkingAndTools(t *testing.T) {
	// thinking/tool_call/tool_result messages are skipped; only user+assistant form pairs.
	now := time.Now()
	t1 := now.Add(-1 * time.Hour)

	msgs := []session.Message{
		{Type: session.MessageTypeUser, Content: "hello", Timestamp: t1},
		{Type: session.MessageTypeThinking, Content: "hmm...", Timestamp: t1},
		{Type: session.MessageTypeToolCall, Content: "tool", Timestamp: t1},
		{Type: session.MessageTypeToolResult, Content: "result", Timestamp: t1},
		{Type: session.MessageTypeAssistant, Content: "hi!", Timestamp: t1},
	}

	sessions := []*session.Session{
		{ID: "s1"},
	}

	loadFn := func(id string) ([]session.Message, error) {
		return msgs, nil
	}

	summaries := buildSessionSummaries(sessions, loadFn, time.Time{}, nil)
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
	if len(summaries[0].Messages) != 1 {
		t.Fatalf("expected 1 pair (only user+assistant), got %d", len(summaries[0].Messages))
	}
	if summaries[0].Messages[0].User != "hello" {
		t.Errorf("user content: got %q, want hello", summaries[0].Messages[0].User)
	}
}
