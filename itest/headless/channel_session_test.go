//go:build integration

package headless_test

// 手机对着一段 **channel 拥有的** 会话说话（desktop/channel_mirror.go）：
//
//   - 这一轮由那个渠道的运行时跑 —— 手机看到的是同一轮的流，写下的也是同一份记录；
//   - 没有渠道运行时的宿主明确拒绝（`channel_offline`），绝不退回自己跑一轮。
//
// 这两条就是「一会儿在 IM 里聊、一会儿在手机上接着聊」能成立的全部前提：写的人只有一个。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/session"
)

// seedChannelSession writes a conversation a channel thread owns, the way a channel's first message
// would have left it: meta.json carrying thread_id (the phone cannot set that through the API — it is
// what makes the conversation the channel's).
func seedChannelSession(t *testing.T, home, id, threadID string) {
	t.Helper()
	dir := filepath.Join(home, ".tachi", "session", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("seed session dir: %v", err)
	}
	now := time.Now()
	body, err := json.Marshal(&session.Session{
		ID: id, Title: "discord 里的对话", ThreadID: threadID,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("marshal seed session: %v", err)
	}
	writeFile(t, filepath.Join(dir, "meta.json"), string(body)+"\n")
	writeFile(t, filepath.Join(dir, "messages.jsonl"), "")
}

func TestMobileSendsIntoAChannelOwnedConversation(t *testing.T) {
	mock := newMock(t, "从渠道那边回答的。")
	sb := newSandboxWith(t, mock.URL(), "channel:\n  channels:\n    mock:\n      enabled: true\n")
	seedChannelSession(t, sb.home, "chan-1", "mock:thread-1")
	h := startHost(t, sb)
	// What the host said is the only way to tell "the turn never started" from "it started and the
	// phone was not told" — that difference is the whole point of this case.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("host logs:\n%s", h.logs.String())
		// The channel runtime logs to its own file, not to stdout — that is where "the turn never
		// started" and "it started and failed" differ.
		if b, err := os.ReadFile(filepath.Join(sb.home, ".tachi", "logs", "debug.log")); err == nil {
			t.Logf("channel log:\n%s", tail(string(b), 4000))
		}
	})

	conn := h.dialWS(t)
	writeJSON(t, conn, map[string]string{"type": "open", "sessionId": "chan-1"})
	waitSnapshot(t, conn)

	status, body := h.do(t, "POST", "/v1/sessions/chan-1/messages", `{"text":"在吗","mode":"send"}`)
	if status != 200 {
		t.Fatalf("send into a channel conversation: %d %s", status, body)
	}

	var text strings.Builder
	var sawTurn, sawIdle bool
	deadline := time.Now().Add(30 * time.Second)
	for !sawIdle && time.Now().Before(deadline) {
		frame := readFrame(t, conn)
		switch frame["type"] {
		case "text":
			if s, ok := frame["text"].(string); ok {
				text.WriteString(s)
			}
		case "turn":
			sawTurn = true
		case "idle":
			sawIdle = true
		case "error":
			t.Fatalf("turn error: %v", frame["message"])
		}
	}
	if !sawTurn {
		t.Fatal("no turn frame: the turn ran but the phone was never told it ended")
	}
	if !sawIdle {
		t.Fatal("no idle frame before the deadline — the phone's spinner would never stop")
	}
	if got := text.String(); !strings.Contains(got, "从渠道那边回答的") {
		t.Fatalf("streamed text = %q, want the mock's reply", got)
	}

	// And the RECORD is the same conversation: the channel runtime wrote it, the phone rereads it.
	fresh := h.dialWS(t)
	writeJSON(t, fresh, map[string]string{"type": "open", "sessionId": "chan-1"})
	snap := waitSnapshot(t, fresh)
	if !snapshotHasAssistantText(snap, "从渠道那边回答的") {
		t.Fatalf("the channel conversation did not record the reply: %v", snap)
	}
}

// TestMobileRefusesAChannelConversationWithoutTheChannel: a host that runs no channel must say so.
// Running the turn itself is exactly the failure this protects against — two histories, one session.
func TestMobileRefusesAChannelConversationWithoutTheChannel(t *testing.T) {
	mock := newMock(t, "不该跑到这里。")
	sb := newSandbox(t, mock.URL()) // mobile only: no channel enabled
	seedChannelSession(t, sb.home, "chan-2", "weixin:acct:user")
	h := startHost(t, sb)

	status, body := h.do(t, "POST", "/v1/sessions/chan-2/messages", `{"text":"在吗","mode":"send"}`)
	if status != 409 {
		t.Fatalf("send into a channel conversation with no channel = %d %s, want 409", status, body)
	}
	if !strings.Contains(body, "channel_offline") {
		t.Fatalf("refusal = %s, want the channel_offline reason", body)
	}
}

// tail returns the last n bytes of s, on a line boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	if i := strings.IndexByte(s[cut:], '\n'); i >= 0 {
		cut += i + 1
	}
	return s[cut:]
}
