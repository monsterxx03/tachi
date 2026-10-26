//go:build integration

package headless_test

// 审批卡在无头宿主上到底能不能用：这是「手机是唯一前端」最要紧的一条 —— 没有人坐在那台机器前，
// 唯一能按下「允许 / 拒绝」的地方就是手机。桌面端的卡片有冒烟套件看着；这条路只有这里看着。
//
// 两个用例走的是同一个 ask（一条命中 `permissions.bash.ask` 的命令），差别只在回答：
//   - 允许 → 命令真的执行，模型拿到结果继续说了下一句；
//   - 拒绝 → 这一轮中止，模型**没有被再问第二次**（那正是「拒绝」与「跳过并假装成功」的区别）。
//
// 第三个断言藏在第一个用例里：停车场（`d.asks`）是快照的一部分，所以手机切走再回来，卡片还在 ——
// 一条只活在 WebSocket 流里的 ask 会在锁屏之后消失，而用户没法回答一个看不见的问题。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/monsterxx03/tachi/itest/mockllm"
)

// askEchoConfig makes exactly one shape of command need approval, so the test's script (not the
// environment) decides when a card appears. No `jev:` block is written: the judgment gate only runs
// when that service is configured, and this test is about the card, not about a probability.
const askEchoConfig = "permissions:\n  bash:\n    ask:\n      - \"echo *\"\n"

func TestPermissionCardCanBeAllowedOverTheWire(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)
	// The command has a FILE side effect: "the model said it ran" is not evidence, the file is.
	marker := filepath.Join(t.TempDir(), "allowed.txt")
	cmd := `echo hello > ` + marker
	mock.Script(
		mockllm.Step{Reply: mockllm.Stream(
			mockllm.ToolCallStart("call-perm-allow", "Bash", `{"command":"`+cmd+`"}`),
			mockllm.Finish("tool_calls"),
			mockllm.Done(),
		)},
		mockllm.Step{Reply: mockllm.Stream(
			mockllm.Text("命令跑完了。"),
			mockllm.Finish("stop"),
			mockllm.Done(),
		)},
	)

	sb := newSandboxWith(t, mock.URL(), askEchoConfig)
	h := startHost(t, sb)
	id := h.createSession(t, sb.home)

	conn := h.dialWS(t)
	writeJSON(t, conn, map[string]string{"type": "open", "sessionId": id})
	waitSnapshot(t, conn)

	if status, body := h.do(t, "POST", "/v1/sessions/"+id+"/messages", `{"text":"跑个命令","mode":"send"}`); status != 200 {
		t.Fatalf("send: %d %s", status, body)
	}

	ask := waitAsk(t, conn)
	toolID, _ := ask["toolId"].(string)
	if toolID == "" {
		t.Fatalf("ask carries no toolId: %v", ask)
	}
	if ask["kind"] != "permission" {
		t.Fatalf("ask kind = %v, want permission", ask["kind"])
	}
	if ask["toolName"] != "Bash" {
		t.Fatalf("ask toolName = %v, want Bash", ask["toolName"])
	}
	if preview, _ := ask["preview"].(string); !strings.Contains(preview, "echo hello") {
		t.Fatalf("ask preview does not show the command: %q", preview)
	}

	// The parked card is part of the snapshot, so the phone that reopens the conversation still sees
	// it. A fresh subscription is the same thing the app does after being backgrounded.
	fresh := h.dialWS(t)
	writeJSON(t, fresh, map[string]string{"type": "open", "sessionId": id})
	if snap := waitSnapshot(t, fresh); !snapshotHasPendingAsk(snap, toolID) {
		t.Fatalf("snapshot does not carry the parked permission: %v", snap)
	}

	if status, body := h.do(t, "POST", "/v1/sessions/"+id+"/permissions/"+toolID, `{"decision":"allow_once"}`); status != 200 {
		t.Fatalf("answer permission: %d %s", status, body)
	}

	var text strings.Builder
	for {
		frame := readFrame(t, conn)
		if frame["type"] == "text" {
			if s, ok := frame["text"].(string); ok {
				text.WriteString(s)
			}
		}
		if frame["type"] == "idle" {
			break
		}
		if frame["type"] == "error" {
			t.Fatalf("turn failed after allowing: %v", frame["message"])
		}
	}
	if !strings.Contains(text.String(), "命令跑完了") {
		t.Fatalf("the turn did not continue after the answer; text = %q", text.String())
	}
	if b, err := os.ReadFile(marker); err != nil || !strings.Contains(string(b), "hello") {
		t.Fatalf("the allowed command did not run (marker %s): %v %q", marker, err, b)
	}
	// Two model calls: the tool-call round, then the round that reads the result. One would mean the
	// command never ran; three would mean something asked the model again.
	if n := mock.RequestCount(); n != 2 {
		t.Fatalf("model calls = %d, want 2 (tool round + follow-up)", n)
	}
}

func TestPermissionCardCanBeDeniedOverTheWire(t *testing.T) {
	mock := mockllm.NewServer()
	t.Cleanup(mock.Close)

	// Same shape as the allow case: a file the command would create. "The model said it was blocked"
	// is not evidence; an absent file is.
	marker := filepath.Join(t.TempDir(), "denied.txt")
	cmd := `echo denied > ` + marker
	mock.Script(
		mockllm.Step{Reply: mockllm.Stream(
			mockllm.ToolCallStart("call-perm-deny", "Bash", `{"command":"`+cmd+`"}`),
			mockllm.Finish("tool_calls"),
			mockllm.Done(),
		)},
		// A denial is answered as "not approved" WITH NO ERROR (desktop/permission.go), so the tool
		// result tells the model the command did not run and the turn continues — the model gets one
		// more round to say so. This step is that round; a host that skipped it would stop at one call.
		mockllm.Step{Reply: mockllm.Stream(mockllm.Text("好的，不执行。"), mockllm.Finish("stop"), mockllm.Done())},
	)

	sb := newSandboxWith(t, mock.URL(), askEchoConfig)
	h := startHost(t, sb)
	id := h.createSession(t, sb.home)

	conn := h.dialWS(t)
	writeJSON(t, conn, map[string]string{"type": "open", "sessionId": id})
	waitSnapshot(t, conn)

	if status, body := h.do(t, "POST", "/v1/sessions/"+id+"/messages", `{"text":"跑个命令","mode":"send"}`); status != 200 {
		t.Fatalf("send: %d %s", status, body)
	}

	ask := waitAsk(t, conn)
	toolID, _ := ask["toolId"].(string)
	if toolID == "" {
		t.Fatalf("ask carries no toolId: %v", ask)
	}

	if status, body := h.do(t, "POST", "/v1/sessions/"+id+"/permissions/"+toolID, `{"decision":"deny"}`); status != 200 {
		t.Fatalf("answer permission: %d %s", status, body)
	}

	// The phone must see a conclusion, not a hang: the turn ends normally (a denial is not a
	// failure), and the explanation travels to the model rather than to the screen.
	for {
		frame := readFrame(t, conn)
		switch frame["type"] {
		case "error":
			t.Fatalf("a denied turn must not be reported as a failure: %v", frame["message"])
		case "idle":
			goto done
		}
	}
done:
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("the denied command ran anyway (marker %s exists)", marker)
	}
	if n := mock.RequestCount(); n != 2 {
		t.Fatalf("model calls = %d, want 2 (the refusal is reported back to the model)", n)
	}
}

// waitAsk skips the state/list frames a turn emits before it parks, and fails if the turn ends
// instead of parking on the user.
func waitAsk(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	for i := 0; i < 50; i++ {
		frame := readFrame(t, conn)
		switch frame["type"] {
		case "ask":
			ask, _ := frame["ask"].(map[string]any)
			if ask == nil {
				t.Fatalf("ask frame carries no payload: %v", frame)
			}
			return ask
		case "idle":
			t.Fatalf("turn ended without ever parking on the user")
		}
	}
	t.Fatal("no ask frame")
	return nil
}

// snapshotHasPendingAsk looks inside the frame's snapshot payload for a parked permission.
func snapshotHasPendingAsk(frame map[string]any, toolID string) bool {
	snap, _ := frame["snapshot"].(map[string]any)
	asks, _ := snap["asks"].([]any)
	for _, a := range asks {
		ask, _ := a.(map[string]any)
		if ask["toolId"] == toolID {
			return true
		}
	}
	return false
}
