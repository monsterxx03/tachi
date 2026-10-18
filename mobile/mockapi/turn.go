package main

import (
	"strings"
	"time"
)

// 一轮是怎么演的：用户消息回声 → 在跑 → 流式吐字（中间插一个工具调用）
// → 要么停下来等一句（权限或提问），要么结束。
//
// 剧本按用户说了什么挑，好让手机端把每条分支都走一遍：说「删」会引出权限卡，
// 带问号会引出提问卡，其余是普通的一轮。

type scriptStep struct {
	reply    string
	toolName string
	toolArgs string
	toolOut  string
	ask      any
}

func pickScript(text string) scriptStep {
	switch {
	case strings.Contains(text, "删") || strings.Contains(text, "rm "):
		return scriptStep{
			reply:    "先看一下要删的东西有多大，然后清理命令会命中 `permissions.bash.ask` 规则，需要你确认。",
			toolName: "Bash",
			toolArgs: "du -sh build && ls -lt build | head -24",
			toolOut:  "328M\tbuild\n… 6 个 7 天前的产物\n… 2 个今天的产物",
			ask: PermissionAsk{
				Kind: "permission", ToolID: nextToolID(), ToolName: "Bash", PermissionKind: "bash",
				Preview: "$ rm -rf build/electron-20261009-* build/web-20261010-*\n匹配规则 permissions.bash.ask ·「rm -rf*」",
			},
		}
	case strings.Contains(text, "吗") || strings.Contains(text, "？") || strings.Contains(text, "?"):
		return scriptStep{
			reply:    "两种做法都行，但影响面不一样，我先确认一下你想要的形态。",
			toolName: "Grep",
			toolArgs: `"login|signin" src/ --type tsx`,
			toolOut:  "src/auth/login.tsx\nsrc/auth/signup.tsx\nsrc/auth/oauth-callback.tsx\n… 5 个文件",
			ask: QuestionAsk{
				Kind: "question", ToolID: nextToolID(),
				Questions: []Question{{
					Question: "失败提示放在哪一层？",
					Options:  []string{"页面各自展示", "统一的 toast 组件", "全局错误边界"},
				}},
			},
		}
	default:
		return scriptStep{
			reply:    "好，我看过了。这一段先说结论，细节我放在下一句里。",
			toolName: "Read",
			toolArgs: "desktop/agent_turn.go",
			toolOut:  "func (d *desktopApp) startTurn(...) { … }\n… 共 214 行",
		}
	}
}

var toolSeq int

func nextToolID() string {
	toolSeq++
	return "tool-" + itoa(toolSeq)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// chunksOf 把一段话切成小块，模拟逐字流出来
func chunksOf(s string) []string {
	runes := []rune(s)
	const size = 6
	var out []string
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

func (s *Server) stopped(sess *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !sess.Session.Running
}

// runTurn 从用户发来一条消息开始演
func (s *Server) runTurn(sess *session, text, mode string) {
	id := sess.Session.ID
	start := time.Now()

	s.mu.Lock()
	sess.Session.Running = true
	sess.Session.Waiting = ""
	sess.Session.UpdatedAt = "刚刚"
	sess.State = SessionState{Status: "working", Label: "在跑", Detail: "正在生成"}
	running := sess.State
	sess.append(Message{Role: "user", Content: text, Timestamp: time.Now().Format(time.RFC3339)})
	s.mu.Unlock()

	// 另一块屏发的消息要告诉所有人（自己发的那条靠 origin 认出来）
	s.broadcast(id, Event{Type: "user_message", Text: text, DisplayText: text, Origin: "mobile", Steer: mode == "steer"})
	s.broadcast(id, Event{Type: "state", State: &running})
	s.broadcast(id, Event{Type: "tps", Tps: 51, LastTps: 51})

	step := pickScript(text)

	// 吐字：中间插一次工具调用
	chunks := chunksOf(step.reply)
	half := len(chunks) / 2
	var spoken strings.Builder
	for i, ch := range chunks {
		if s.stopped(sess) {
			s.endTurn(sess, "interrupted", start)
			return
		}
		time.Sleep(45 * time.Millisecond)
		spoken.WriteString(ch)
		s.broadcast(id, Event{Type: "text", Text: ch})

		if i == half {
			s.broadcast(id, Event{Type: "tool_start", ToolID: "call-1", ToolName: step.toolName, Title: step.toolArgs})
			s.broadcast(id, Event{Type: "tool_args", ToolID: "call-1", Args: step.toolArgs})
			time.Sleep(120 * time.Millisecond)
			s.broadcast(id, Event{Type: "tool_result", ToolID: "call-1", Result: step.toolOut})
			s.mu.Lock()
			sess.append(Message{Role: "tool_call", ToolName: step.toolName, ToolCallID: "call-1", Title: step.toolArgs})
			sess.append(Message{Role: "tool_result", ToolName: step.toolName, ToolCallID: "call-1", ToolResult: step.toolOut})
			s.mu.Unlock()
		}
	}

	s.mu.Lock()
	sess.append(Message{Role: "assistant", Content: spoken.String()})
	s.mu.Unlock()

	if step.ask != nil {
		// 停下来等一句：把待答项挂上，广播出去，这一轮就不再往下走了
		s.mu.Lock()
		sess.Asks = append(sess.Asks, step.ask)
		waiting := "等待你的回答"
		label := "等你回答"
		status := "waiting"
		if p, ok := step.ask.(PermissionAsk); ok {
			waiting, label = "等待你的授权", "等你授权"
			_ = p
		}
		sess.Session.Waiting = waiting
		sess.State = SessionState{Status: status, Label: label, Detail: "等你回一句", Waiting: waiting}
		st := sess.State
		s.mu.Unlock()

		s.broadcast(id, Event{Type: "ask", Ask: step.ask})
		s.broadcast(id, Event{Type: "state", State: &st})
		return
	}

	s.endTurn(sess, "complete", start)
}

// finishTurn 是用户答完之后的收尾：把这一轮跑完
func (s *Server) finishTurn(sess *session, note string) {
	id := sess.Session.ID
	start := time.Now()

	s.mu.Lock()
	sess.Session.Running = true
	sess.State = SessionState{Status: "working", Label: "在跑", Detail: "按你的意思继续"}
	st := sess.State
	s.mu.Unlock()
	s.broadcast(id, Event{Type: "state", State: &st})

	var spoken strings.Builder
	for _, ch := range chunksOf(note) {
		if s.stopped(sess) {
			s.endTurn(sess, "interrupted", start)
			return
		}
		time.Sleep(40 * time.Millisecond)
		spoken.WriteString(ch)
		s.broadcast(id, Event{Type: "text", Text: ch})
	}

	s.mu.Lock()
	sess.append(Message{Role: "assistant", Content: spoken.String()})
	s.mu.Unlock()
	s.endTurn(sess, "complete", start)
}

// endTurn 收尾：落状态、报时长、发 idle（手机据此补发排队中的消息）
func (s *Server) endTurn(sess *session, end string, start time.Time) {
	id := sess.Session.ID
	s.mu.Lock()
	sess.Session.Running = false
	sess.Session.Waiting = ""
	sess.State = SessionState{Status: "idle", Label: "空闲"}
	st := sess.State
	s.mu.Unlock()

	s.broadcast(id, Event{Type: "turn", End: end, DurationMs: time.Since(start).Milliseconds(), Iterations: 1})
	s.broadcast(id, Event{Type: "state", State: &st})
	s.broadcast(id, Event{Type: "idle", End: end})
}
