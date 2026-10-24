//go:build integration

package headless_test

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/monsterxx03/tachi/itest/mockllm"
)

// TestHeadlessTurnReachesThePhone drives one real turn through the phone protocol: create a
// conversation, subscribe, send, and collect what the host projects.
//
// It asserts the two halves the phone depends on separately — the LIVE stream (text arrives, the
// turn concludes) and the RECORD (a fresh subscription's snapshot carries the same reply). A host
// that only streamed, or only recorded, would pass a single-sided test and leave the other half
// looking like a dropped message.
func TestHeadlessTurnReachesThePhone(t *testing.T) {
	mock := newMock(t, "无头宿主跑通了：这是模型的第一轮回答。")
	sb := newSandbox(t, mock.URL())
	h := startHost(t, sb)

	id := h.createSession(t, sb.home)

	conn := h.dialWS(t)
	writeJSON(t, conn, map[string]string{"type": "open", "sessionId": id})
	waitSnapshot(t, conn)

	status, body := h.do(t, "POST", "/v1/sessions/"+id+"/messages", `{"text":"你好","mode":"send"}`)
	if status != 200 {
		t.Fatalf("send: %d %s", status, body)
	}

	var text strings.Builder
	var sawTurn, sawIdle bool
	for !sawIdle {
		frame := readFrame(t, conn)
		switch frame["type"] {
		case "text":
			if s, ok := frame["text"].(string); ok {
				text.WriteString(s)
			}
		case "turn":
			sawTurn = true
			if end := frame["end"]; end != "complete" {
				t.Fatalf("turn end = %v, want complete", end)
			}
		case "idle":
			sawIdle = true
			if end := frame["end"]; end != "complete" {
				t.Fatalf("idle end = %v, want complete", end)
			}
		case "error":
			t.Fatalf("turn error: %v", frame["message"])
		}
	}
	if !sawTurn {
		t.Fatal("no turn frame before idle")
	}
	if got := text.String(); !strings.Contains(got, "无头宿主跑通了") {
		t.Fatalf("streamed text = %q, want the mock's reply", got)
	}

	// The record is the authority: a NEW subscription (what the phone does when it reopens the
	// conversation) must show the reply even though this turn's stream is over.
	fresh := h.dialWS(t)
	writeJSON(t, fresh, map[string]string{"type": "open", "sessionId": id})
	snap := waitSnapshot(t, fresh)
	if !snapshotHasAssistantText(snap, "无头宿主跑通了") {
		t.Fatalf("snapshot does not carry the reply: %v", snap)
	}
}

// TestHeadlessRefusesRequestsWithoutTheToken pins the one guard the LAN protocol has. It is a
// surprising thing to lose in a refactor, because everything still WORKS — for anyone on the network.
func TestHeadlessRefusesRequestsWithoutTheToken(t *testing.T) {
	mock := newMock(t, "unused")
	sb := newSandbox(t, mock.URL())
	h := startHost(t, sb)

	resp, err := httpGet(h.url("/v1/sessions"))
	if err != nil {
		t.Fatalf("get sessions: %v", err)
	}
	if resp != 401 {
		t.Fatalf("unauthenticated /v1/sessions = %d, want 401", resp)
	}
	// /v1/ping is the exception the pairing screen needs: it answers without a token, or a phone
	// with a wrong one could never tell "wrong token" from "wrong address".
	if !h.ping() {
		t.Fatal("ping should answer without a token")
	}
}

// TestSecondHostOnTheSameHomeIsRefused pins the one-host rule (desktop/host_lock.go) from the
// outside: the second process exits non-zero and says why, instead of running a second agent over
// the same conversations.
func TestSecondHostOnTheSameHomeIsRefused(t *testing.T) {
	mock := newMock(t, "unused")
	sb := newSandbox(t, mock.URL())
	_ = startHost(t, sb) // holds the lock for the rest of the test

	cmd := exec.Command(headlessBinary(t))
	cmd.Env = append(os.Environ(), "HOME="+sb.home)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a second host on the same home exited 0; logs:\n%s", out)
	}
	if !strings.Contains(string(out), "同一份数据同时只能由一个进程跑") {
		t.Fatalf("refusal does not explain itself:\n%s", out)
	}
}

// --- helpers ---

func newMock(t *testing.T, reply string) *mockllm.Server {
	t.Helper()
	s := mockllm.NewServer()
	t.Cleanup(s.Close)
	s.Script(mockllm.Step{Reply: mockllm.Stream(mockllm.Text(reply))})
	return s
}

func httpGet(url string) (int, error) {
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func writeJSON(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func readFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	// Every frame this suite waits for ends the turn, so one budget covers the whole read. Without
	// a deadline a broken host would hang the run instead of failing it.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("decode frame %s: %v", data, err)
	}
	return frame
}

// waitSnapshot skips the session-list broadcasts a connection can carry (creating a conversation,
// for instance, notifies every connection) and returns the first snapshot.
func waitSnapshot(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	for i := 0; i < 20; i++ {
		frame := readFrame(t, conn)
		if frame["type"] == "snapshot" {
			return frame
		}
	}
	t.Fatal("no snapshot frame")
	return nil
}

// snapshotHasAssistantText looks inside the frame's snapshot payload — the wire frame is
// {"type":"snapshot","snapshot":{…}}, not the Snapshot itself.
func snapshotHasAssistantText(frame map[string]any, want string) bool {
	snap, _ := frame["snapshot"].(map[string]any)
	msgs, _ := snap["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		if msg["role"] != "assistant" {
			continue
		}
		if s, _ := msg["content"].(string); strings.Contains(s, want) {
			return true
		}
	}
	return false
}
