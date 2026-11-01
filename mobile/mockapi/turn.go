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
	// slow 把这一轮拉长到十来秒：手机端那条「运行中发送 → 排队 → 工具边界插入」的路
	// 需要一段真的还在跑的回合才验得到。
	slow bool
	// compact 让这一轮在中途搬进子会话：手机端「压缩后换轨」那条路（session_moved）
	// 需要一个真的换 id 的回合才验得到。
	compact bool
	// repeat 把同一个工具连出几次：手机端「跑的时候就把过程折成一行、步数实时往上走」
	// 需要一轮里有好几个工具才看得出来。
	repeat int
	// autoExpand 让这个工具调用走「用户自己点名」那条路（真实现是桌面的 /sh）：它的输出就是
	// 答案，手机上卡片要直接摊开。剧本里只有 /sh 那一条会置它。
	autoExpand bool
}

func pickScript(text string) scriptStep {
	switch {
	// /sh：真实现里这是本地执行（不过模型），输出就是一个「用户点名的工具调用」。mock 跑不了
	// shell，所以这里给一段固定输出 —— 手机端要看的是**卡片直接摊开**（autoExpand）这件事。
	case strings.HasPrefix(strings.TrimSpace(text), "/sh"):
		cmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/sh"))
		if cmd == "" {
			cmd = "ls"
		}
		return scriptStep{
			reply:      "跑完了，输出在上面那张卡里。",
			toolName:   "Bash",
			toolArgs:   cmd,
			toolOut:    "README.md\npackage.json\nsrc/\n… 4 项",
			autoExpand: true,
		}
	case strings.Contains(text, "slow"):
		return scriptStep{
			slow: true,
			reply: "这一轮故意跑得久一点，好让你在它还没结束的时候发消息。手机端应该把那句话放进待发送队列，" +
				"而不是立刻塞进正在流的回复里；等到这次工具调用结束，它才会作为插话出现在对话里。" +
				"如果中途按了「立即发送 · 打断」，这一轮会先停掉，队列里的那句再作为新的一轮发出来。" +
				"再多说几句，确保这段时间足够你操作。",
			toolName: "Bash",
			toolArgs: "sleep 3 && echo slow",
			toolOut:  "slow",
		}
	case strings.Contains(text, "压缩") || strings.Contains(strings.ToLower(text), "compact"):
		return scriptStep{
			slow:    true,
			compact: true,
			reply: "这段对话太长了，我先把它压缩一次再往下说。压缩会把历史写进一个新的子会话，" +
				"这一轮的尾巴也跟着过去 —— 手机上应该自己换到那段新会话，而不是停在压缩点等一个不会再来的一轮结束。",
			toolName: "Read",
			toolArgs: "internal/history.go",
			toolOut:  "… 共 640 行",
		}
	case strings.Contains(text, "多步") || strings.Contains(text, "multi"):
		return scriptStep{
			slow:     true,
			repeat:   3,
			reply:    "我分三步看：先读入口，再找引用，最后核对接口。这三步都跑完之后再看结论。",
			toolName: "Read",
			toolArgs: "src/auth/login.tsx",
			toolOut:  "… 共 128 行",
		}
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

	delay := 45 * time.Millisecond
	if step.slow {
		delay = 300 * time.Millisecond
	}

	// 吐字：中间插工具调用（默认一次，repeat 则均匀插几次）
	chunks := chunksOf(step.reply)
	repeat := step.repeat
	if repeat == 0 {
		repeat = 1
	}
	toolAfter := func(i int) int {
		for k := 1; k <= repeat; k++ {
			if i == len(chunks)*k/(repeat+1) {
				return k
			}
		}
		return 0
	}
	var spoken strings.Builder
	for i, ch := range chunks {
		if s.stopped(sess) {
			s.endTurn(sess, "interrupted", start)
			return
		}
		time.Sleep(delay)
		spoken.WriteString(ch)
		s.broadcast(id, Event{Type: "text", Text: ch})

		if k := toolAfter(i); k > 0 {
			toolID := "call-" + itoa(k)
			title := step.toolArgs + " #" + itoa(k)
			s.broadcast(id, Event{Type: "tool_start", ToolID: toolID, ToolName: step.toolName, Title: title, AutoExpand: step.autoExpand})
			s.broadcast(id, Event{Type: "tool_args", ToolID: toolID, Args: step.toolArgs})
			// 跑得久一点：手机端要能在「还在跑」的时候看到折叠行上的步数
			time.Sleep(700 * time.Millisecond)
			s.broadcast(id, Event{Type: "tool_result", ToolID: toolID, Result: step.toolOut})
			s.mu.Lock()
			sess.append(Message{Role: "tool_call", ToolName: step.toolName, ToolCallID: toolID, Title: title})
			sess.append(Message{Role: "tool_result", ToolName: step.toolName, ToolCallID: toolID, ToolResult: step.toolOut})
			s.mu.Unlock()
		}
	}

	s.mu.Lock()
	sess.append(Message{Role: "assistant", Content: spoken.String()})
	s.mu.Unlock()

	if step.compact {
		// 压缩：把这一轮搬进子会话。**换轨发生在一轮结束之后**（真实现就在 endTurn 里：
		// r.running 先落回 false，再 followCompaction），所以子会话此刻已经是空闲的 ——
		// 手机在这之后补发队列里的消息才不会被 turn_running 挡下。
		s.broadcast(id, Event{Type: "compact_start"})
		time.Sleep(600 * time.Millisecond)
		child := s.compactInto(sess)
		s.broadcast(id, Event{Type: "compact_done"})
		s.mu.Lock()
		child.Session.Running = false
		s.mu.Unlock()
		s.broadcast(id, Event{Type: "session_moved", From: id, To: child.Session.ID, End: "complete"})
		s.broadcast("", Event{Type: "sessions_changed"})
		sess = child
		id = child.Session.ID
	}

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

// compactInto 造出压缩后的子会话。真实现把摘要与最近几条记录写进新会话说，这里只留一条
// 摘要：手机要验的是「换轨之后画面里的东西来自子会话」，不是摘要的内容。
func (s *Server) compactInto(old *session) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := old.Session.ID + "-c"
	for s.byID[id] != nil {
		id += "c"
	}
	ns := &session{}
	ns.Snapshot = old.Snapshot
	ns.Session.ID = id
	ns.Session.UpdatedAt = "刚刚"
	ns.Messages = nil
	ns.nextIndex = 0
	ns.Asks = nil
	ns.Attachments = nil
	ns.Context = ContextInfo{Estimate: 2400, Window: old.Context.Window, Parts: []ContextPart{}}
	ns.append(Message{
		Role: "assistant",
		Content: "（压缩摘要）前面 " + itoa(old.nextIndex) + " 条消息的摘要。手机端在压缩结束后应该已经换到这段子会话上：" +
			"先看到这条摘要，再看到这一轮的尾巴。",
	})
	s.byID[id] = ns
	s.order = append([]string{id}, s.order...)
	return ns
}
