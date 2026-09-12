package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/itest/mockllm"
)

// A scenario is one scripted conversation plus one driver: the mock decides what the
// model says, the driver does what a user would, and both sides assert.
//
// The two halves see different things on purpose. A driver can only prove what the UI
// SHOWS (transcript shape, chips, buttons); the mock and the filesystem prove what
// actually HAPPENED (which prompt the model got, which file was written). A regression
// usually shows up in exactly one of the two, so both are checked here.
type scenario struct {
	name string
	// What the model replies, request by request.
	steps []mockllm.Step
	// Working-directory fixtures the session's tools and @-references see.
	files map[string]string
	// gitInit gives the work dir a repository with one commit (dirty-tree scenarios).
	gitInit bool
	// after runs once the driver has reported: the Go-side assertions.
	after func(c *checkCtx)
}

// A probe is one Go-side assertion, appended to the same report the driver fills.
type checkCtx struct {
	scenario string
	dir      string
	home     string
	work     string
	requests []*mockllm.RecordedRequest
	mockErr  error
	lines    *[]Line
}

func (c *checkCtx) check(label string, ok bool, detail string) {
	*c.lines = append(*c.lines, Line{Label: "go: " + label, OK: ok, Detail: detail})
}

// requestSeen reports whether any request's messages contain want (the prompt-level
// assertion: "did the model actually get told this?").
func (c *checkCtx) requestSeen(want string) (int, bool) {
	for i, r := range c.requests {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, want) {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// planCopies counts the plan-reminder blocks in one request (the prompt-level measure of
// "how many times was the model told about the plan").
func planCopies(req *mockllm.RecordedRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += strings.Count(m.Content, "Active plan:")
	}
	return n
}

// planFiles returns the JSON plans written under the work dir.
func (c *checkCtx) planFiles() []string {
	matches, _ := filepath.Glob(filepath.Join(c.work, ".tachi", "plans", "*.json"))
	return matches
}

// oneOffFiles returns the side-channel (one-off) records this run left in the session
// store: <home>/.tachi/session/<id>/oneoff/*.jsonl. One record per run — a multi-round
// review writes one per round.
func (c *checkCtx) oneOffFiles() []string {
	matches, _ := filepath.Glob(filepath.Join(c.home, ".tachi", "session", "*", "oneoff", "*.jsonl"))
	return matches
}

// sessionCount is how many sessions the run left behind (each is one directory under the
// session store), so a scenario can tell "switched to mine and back" from "created extra".
func (c *checkCtx) sessionCount() int {
	entries, err := os.ReadDir(filepath.Join(c.home, ".tachi", "session"))
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}

// sessionMetas reads every session's meta.json as raw JSON (the scenario-level view of what the
// app wrote on disk — including fields the frontend binding does not carry, like the child link).
func (c *checkCtx) sessionMetas() []map[string]any {
	paths, _ := filepath.Glob(filepath.Join(c.home, ".tachi", "session", "*", "meta.json"))
	var out []map[string]any
	for _, p := range paths {
		var m map[string]any
		if err := readJSON(p, &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}

const (
	// A text turn long enough to still be running while the driver queues a message and
	// clicks 立即发送. The pauses are the point: they make the interruption window real.
	longTurn = 1500 * time.Millisecond
)

// textStream is one assistant message streamed in a few chunks, with token usage — the
// shape every scenario needs.
func textStream(text string, cacheRead int) mockllm.ReplyFunc {
	return mockllm.Stream(
		mockllm.Text(text),
		mockllm.Finish("stop"),
		mockllm.UsageWithCache(1200, 60, cacheRead, 20),
		mockllm.Done(),
	)
}

// bashStream makes the run do something: one tool call, then whatever comes next.
func bashStream(cmd, id string) mockllm.ReplyFunc {
	return mockllm.Stream(
		mockllm.ToolCallStart(id, "Bash", `{"command":"`+cmd+`"}`),
		mockllm.Finish("tool_calls"),
		mockllm.UsageWithCache(1500, 40, 1400, 10),
		mockllm.Done(),
	)
}

func scenarios() []scenario {
	return []scenario{
		//
		// The baseline: a message goes out, the reply lands, a tool call renders as a card,
		// and the usage numbers arrive. If this one fails, nothing else is worth running.
		//
		{
			name: "transcript",
			files: map[string]string{
				"README.md": "# smoke\n\nthe transcript scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// The model asks for a command, then answers with its output: two calls, so
				// the transcript gets both shapes (a tool card and a reply) in one turn.
				{Reply: bashStream("echo smoke-tool-ok", "call_t1")},
				{Reply: textStream("命令跑完了：smoke-tool-ok", 1700)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("两次请求都到达了模型", len(c.requests) == 2, requestCount(c.requests))
				_, toolResultSeen := c.requestSeen("smoke-tool-ok")
				c.check("工具结果回喂给了模型", toolResultSeen, "")
			},
		},
		//
		// 立即发送: interrupt a running turn with a queued message. The ordering bug that
		// shipped here marked the NEW placeholder as 已停止 and dropped the reply, so this
		// is asserted from both ends: the UI (one 已停止, the reply present) and the prompt
		// (the queued text reached the model as the next user message).
		//
		{
			name: "send-now",
			files: map[string]string{
				"README.md": "# smoke\n\nthe send-now scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Text("第一轮开始：先铺一段很长的输出，"),
					mockllm.Pause(longTurn),
					mockllm.Text("让这一轮持续得足够久，"),
					mockllm.Pause(longTurn),
					mockllm.Text("用户才有机会打断它。"),
					mockllm.Pause(longTurn),
					mockllm.Finish("stop"),
					mockllm.UsageWithCache(1200, 120, 900, 20),
					mockllm.Done(),
				)},
				{Reply: textStream("插队那条的回复：已经执行完毕。", 1400)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				_, queued := c.requestSeen("插队：先做这件")
				c.check("排队的那条作为下一轮 user 消息到达模型", queued, "")
			},
		},
		//
		// 用户气泡里的长链接：粘贴的 URL 没有空格可断，折行规则必须给到气泡本身。
		//
		{
			name: "bubble-wrap",
			files: map[string]string{
				"README.md": "# smoke\n\nthe bubble-wrap scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("链接收到了。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
			},
		},
		//
		// 上下文占用环（ContextMeter）：新会话跑完一轮之后它必须有数。
		//
		{
			name: "ctx-ring",
			files: map[string]string{
				"README.md": "# smoke\n\nthe ctx-ring scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: bashStream("sleep 4 && echo smoke-slow", "call_slow")},
				{Reply: textStream("慢工具跑完了。", 1200)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
			},
		},
		//
		// Session-scoped numbers: a brand-new session must not inherit the previous one's
		// cache ring or cost (that bug is in docs/agents/desktop.md's list), the sidebar row must pick
		// up the generated title from the session_title event, and creating one hands the
		// caret to the composer.
		//
		{
			name: "sessions",
			files: map[string]string{
				"README.md": "# smoke\n\nthe sessions scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("第一条的回复。", 900)},
				{Reply: textStream("新会话里的回复。", 1400)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("两个会话各跑了一轮", len(c.requests) == 2, requestCount(c.requests))
			},
		},
		//
		// The input box's height: the reader drags its top edge (or presses ↑ on the handle) and
		// the box grows — the conversation giving up the room, up to the window's share of it.
		// Nothing is persisted, so dragging back down must leave the stylesheet's height and no
		// inline style behind.
		//
		{
			name: "composer-height",
			files: map[string]string{
				"README.md": "# smoke\n\nthe composer-height scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// Never used: the driver only drags the box, and nothing here sends a message. It
				// exists so the mock is a well-formed script rather than an empty one.
				{Reply: textStream("不应该被调用。", 300)},
			},
			after: func(c *checkCtx) {
				c.check("拖动输入框高度不发任何请求", len(c.requests) == 0, requestCount(c.requests))
			},
		},
		//
		// Compaction, from the desktop's side: /compact runs the summarising turn, the
		// conversation moves into a child session, and the sidebar has to keep showing ONE row
		// for it (the child, with the pre-compaction session folded under it). The ring's
		// numbers are captured before and after, because "the context was summarised" has to be
		// visible in the meter that reports it.
		//
		{
			name: "compact",
			files: map[string]string{
				"README.md": "# smoke\n\nthe compact scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// A first turn big enough that summarising it MOVES the meter: the estimate is
				// dominated by the system prompt and tool schemas (a small turn is lost in the
				// rounding), so a scenario about "the context got smaller" needs a conversation
				// that was actually taking up room — which is the only reason anyone compacts.
				{Reply: textStream(strings.Repeat("这是一段很长的历史内容，用来把上下文撑起来。", 1500), 600)},
				// …and a SECOND turn, because the estimate describes the PROMPT of the last call:
				// a reply only enters the measurement when the next call is made with it in the
				// history. Without this turn the big reply is never counted, and "before" would
				// read the same floor as "after".
				{Reply: textStream("第二轮回复：确认。", 600)},
				// The /compact turn: the model has to produce the summary the child session is
				// built from (an empty reply makes the command refuse).
				{Reply: textStream("历史摘要：用户要求查看工作目录；已确认只有一个 README.md。", 600)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("两轮对话加一次压缩，共三次调用", len(c.requests) == 3, requestCount(c.requests))
				_, compacted := c.requestSeen("compress the above conversation")
				c.check("压缩 fork 收到了压缩指令", compacted, "")

				// The disk half of the same fact: the chain is linked BOTH ways, which is what
				// lets a fresh launch rebuild the folded shape (the frontend only sees
				// compacted_parent_id).
				metas := c.sessionMetas()
				c.check("压缩后有两个会话", len(metas) == 2, strconv.Itoa(len(metas)))
				var child, parent map[string]any
				for _, m := range metas {
					if m["compacted_parent_id"] != nil {
						child = m
					} else {
						parent = m
					}
				}
				c.check("子会话记下了它的父会话", child != nil && parent != nil,
					fmt.Sprintf("metas=%v", metas))
				if child != nil && parent != nil {
					c.check("父子互相指向对方",
						child["compacted_parent_id"] == parent["id"] && parent["compacted_child_id"] == child["id"],
						fmt.Sprintf("child.parent=%v parent.child=%v", child["compacted_parent_id"], parent["compacted_child_id"]))
				}
			},
		},
		//
		// The side-channel panel: a typed /review is a one-off fork, so it leaves a record
		// in the session's oneoff/ directory and the panel replays it from there. The
		// driver asserts what the panel SHOWS; these checks assert what actually landed on
		// disk and what the reviewer was told — the half no DOM inspection can see.
		//
		{
			name: "oneoff-panel",
			files: map[string]string{
				"README.md": "# smoke\n\nthe oneoff-panel scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// One review round: a tool call, then the reply. The tool call exists so the
				// replay has a card to render, not just prose.
				{Reply: bashStream("echo oneoff-smoke", "call_r1")},
				{Reply: textStream("评审完成：本次改动没有发现问题。", 1100)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("评审这一轮调用了两次模型", len(c.requests) == 2, requestCount(c.requests))

				records := c.oneOffFiles()
				c.check("会话目录里留下了这次评审的记录", len(records) == 1, fileList(records))
				if len(records) == 1 {
					// The header is what the panel's switcher is built from, so its shape is
					// part of the contract: kind, session id, and where the report goes.
					head, err := os.ReadFile(records[0])
					if err != nil {
						c.check("记录可读", false, err.Error())
					} else {
						var meta struct {
							Type      string            `json:"type"`
							Kind      string            `json:"kind"`
							SessionID string            `json:"session_id"`
							Extra     map[string]string `json:"extra"`
						}
						line := strings.SplitN(string(head), "\n", 2)[0]
						if err := json.Unmarshal([]byte(line), &meta); err != nil {
							c.check("记录首行是 meta", false, err.Error())
						} else {
							c.check("记录首行是 meta 且 kind=review", meta.Type == "meta" && meta.Kind == "review",
								meta.Type+"/"+meta.Kind)
							c.check("记录归属这个会话", meta.SessionID != "", meta.SessionID)
							c.check("记录带上了报告路径", meta.Extra["report"] != "", meta.Extra["report"])
						}
					}
				}

				// The reviewer must have been handed the review prompt — the panel replays
				// whatever the model got, so a prompt that never arrived would be invisible.
				_, prompted := c.requestSeen("Perform a thorough code review")
				c.check("评审 fork 收到了评审 prompt", prompted, "")

				// The panel's width is the reader's to drag, and it is a desktop preference: the
				// driver dragged it to the floor and then to the ceiling, and left the panel open.
				// The stored value must be the LAST drag — above the floor — because a file still
				// holding the floor would mean the second drag never reached the disk. What the
				// ceiling is (window, less sidebar and the conversation's minimum) is asserted in
				// the driver, as the fact it exists for. (Reading the preference BACK is the
				// second launch's job, which one smoke run cannot stage.)
				var ui struct {
					OneOffPanelOpen  bool `json:"oneOffPanelOpen"`
					OneOffPanelWidth int  `json:"oneOffPanelWidth"`
				}
				if err := readJSON(filepath.Join(c.home, ".tachi", "desktop_ui.json"), &ui); err != nil {
					c.check("桌面偏好文件可读", false, err.Error())
				} else {
					c.check("面板开关写进了 desktop_ui.json", ui.OneOffPanelOpen,
						"oneOffPanelOpen="+strconv.FormatBool(ui.OneOffPanelOpen))
					// 320 = the floor, mirrored by desktop/uitheme.go's oneOffPanelMinWidth and
					// oneoff.tsx's ONE_OFF_PANEL_MIN_WIDTH.
					c.check("拖出来的宽度写进了 desktop_ui.json", ui.OneOffPanelWidth > 320,
						"oneOffPanelWidth="+strconv.Itoa(ui.OneOffPanelWidth))
				}
			},
		},
		//
		// The OTHER entry into a review: the turn-level chip. It only exists on a turn that
		// changed something (hence the WriteFile), and the review it starts is scoped to that
		// turn's files (hence gitInit — a scoped review refuses when there is no baseline).
		// What the driver pins is the chip's state machine: 评审本轮改动 → 已评审 N 条 · 查看,
		// with the count coming from the backend's own tally of ReportFinding calls.
		//
		{
			name: "oneoff-footer",
			files: map[string]string{
				"README.md": "# smoke\n\nthe oneoff-footer scenario's working directory\n",
			},
			gitInit: true,
			steps: []mockllm.Step{
				// Turn 1: write TWO files, then say so. Two files, because the pane's fold rule
				// needs a file WITH a finding and one without in the same diff — the review
				// below reports on NOTES.md only. NOTES.md is LONG on purpose: the pane has to
				// SCROLL for the walk's home (the sticky bar) to be the thing under test — a
				// ten-line file would let a header-hosted ↑/↓ pass, because nothing ever scrolls
				// it off the top.
				{Reply: writeFileStream("NOTES.md", strings.Repeat("一行笔记：这一行只是为了让 diff 足够长，面板必须滚动才看得到下面的意见。\n", 80), "call_w1")},
				{Reply: writeFileStream("OTHER.md", "another file\n", "call_w2")},
				{Reply: textStream("已经写下 NOTES.md 和 OTHER.md。", 900)},
				// Review 1: one finding, then the reply.
				{Reply: findingStream("NOTES.md", 1, "warn", "call_f1")},
				{Reply: textStream("评审完成：1 条意见。", 1100)},
				// Review 2, re-run from the panel over the same files: TWO findings, so the
				// chip's count is what tells the two runs apart — and both are DEEP in the file,
				// so walking to the second one scrolls the pane the way a reader's would.
				{Reply: findingStream("NOTES.md", 40, "warn", "call_f2")},
				{Reply: findingStream("NOTES.md", 60, "info", "call_f3")},
				{Reply: textStream("再次评审完成：2 条意见。", 1100)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("一轮对话（两次写文件）加两轮评审，共八次调用", len(c.requests) == 8, requestCount(c.requests))
				// The review is scoped: its prompt must carry the scope section naming the
				// file the turn touched — that is what makes findings land in file
				// coordinates. Asserting on the path alone would pass on the FIRST turn's
				// prompt (the user's own words mention the file), which is why the marker
				// below is the thing to look for.
				_, scoped := c.requestSeen("## Scope (only these files)")
				c.check("评审 fork 的 prompt 带上了作用域", scoped, "")

				// The report's language: the prompt is mixed-language, so the model has to be told
				// explicitly — and the sandbox config says `language: zh`, which is where the value
				// has to come from. Asserted at the LLM boundary: this is the prompt the reviewer
				// actually received, not the one the UI thinks it sent.
				// The marker is the section's own SENTENCE, not its heading: the templates used to
				// carry an `### Output language` of their own, and a `###` heading contains the `##`
				// prefix — a heading-shaped marker would be found even if the section were never
				// appended, which is the regression this check exists to catch.
				_, langSection := c.requestSeen("Write the report AND every finding (its text and its suggestion) in zh")
				c.check("评审 prompt 带上了输出语言一节，且取自 config.language（sandbox 配的是 zh）", langSection, "")
				_, notLang := c.requestSeen(commands.DefaultReplyLanguage)
				c.check("配置了 language 时不会退回兜底语言", !notLang, "")

				records := c.oneOffFiles()
				// Two runs, two records: the re-run starts a NEW one-off (a review is not
				// resumable), which is also what makes the panel's switcher show both.
				c.check("两次评审各留一份记录", len(records) == 2, fileList(records))
				if len(records) == 2 {
					// P4's durable half: the record names the turn that asked for the review and
					// the files it was scoped to. Without them a restart loses the pairing (the
					// turn's chip could no longer say 已评审) and the diff pane has nothing to
					// show — the frontend's own memory of both dies with the window.
					head, err := os.ReadFile(records[0])
					if err != nil {
						c.check("记录可读", false, err.Error())
					} else {
						var meta struct {
							Extra map[string]string `json:"extra"`
						}
						line := strings.SplitN(string(head), "\n", 2)[0]
						if err := json.Unmarshal([]byte(line), &meta); err != nil {
							c.check("记录首行可解析", false, err.Error())
						} else {
							c.check("记录写下了被评审的那一轮", meta.Extra[agent.OneOffKeyReviewedMsg] != "",
								meta.Extra[agent.OneOffKeyReviewedMsg])
							c.check("记录写下了评审范围", meta.Extra[agent.OneOffKeyPaths] != "",
								meta.Extra[agent.OneOffKeyPaths])
						}
					}
				}

				// The panel's open/closed state is a desktop preference: the driver leaves the
				// panel open, so the file has to say so. (What it is restored FROM is only
				// exercised by a second launch, which one smoke run cannot do.) The WIDTH is the
				// oneoff-panel scenario's business — this driver never drags the handle.
				var ui struct {
					OneOffPanelOpen bool `json:"oneOffPanelOpen"`
				}
				if err := readJSON(filepath.Join(c.home, ".tachi", "desktop_ui.json"), &ui); err != nil {
					c.check("桌面偏好文件可读", false, err.Error())
				} else {
					c.check("面板开关写进了 desktop_ui.json", ui.OneOffPanelOpen,
						"oneOffPanelOpen="+strconv.FormatBool(ui.OneOffPanelOpen))
				}
			},
		},
		//
		// Switching away from a RUNNING session and back: the restored view must land at the
		// newest message and stay there while the stream keeps writing. The driver measures the
		// gap to the bottom on every frame across the switch, so the reported "先向上飘再跳到底"
		// is a number rather than an impression.
		//
		// The history deliberately contains a MERMAID DIAGRAM: it renders asynchronously (lazy
		// import + a debounced render), so on every switch back the diagram grows from its
		// placeholder into a real figure — content changing height AFTER the pin to the bottom,
		// which is what makes the view drift.
		//
		{
			name: "switch-scroll",
			files: map[string]string{
				"README.md": "# smoke\n\nthe switch-scroll scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("先看一张图：\n\n```mermaid\ngraph TD\n  A[开始] --> B{判断}\n  B --> C[结束]\n```\n\n图看完了。", 900)},
				// A turn that stays running while the driver goes away and comes back.
				{Reply: mockllm.Stream(
					mockllm.Text("第一段，先铺一点内容，"),
					mockllm.Pause(longTurn),
					mockllm.Text("第二段还在写，"),
					mockllm.Pause(longTurn),
					mockllm.Text("第三段，"),
					mockllm.Pause(longTurn),
					mockllm.Text("就这样结束。"),
					mockllm.Finish("stop"),
					mockllm.UsageWithCache(1200, 60, 900, 20),
					mockllm.Done(),
				)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("两轮对话各调用了一次模型", len(c.requests) == 2, requestCount(c.requests))
				// Switching away must not create a session on the backend beyond the two the
				// driver used: a spurious third would mean the restore path re-created one.
				c.check("没有多余的会话", c.sessionCount() == 2, strconv.Itoa(c.sessionCount()))
			},
		},
		//
		// Plan step write-back: the same plan_id twice, statuses advancing. The UI must show
		// the SAME document moving 0/3 → 1/3, and the disk must hold exactly one plan file —
		// two files would mean the model forked the plan instead of updating it.
		//
		{
			name: "plan-writeback",
			files: map[string]string{
				"README.md": "# smoke\n\nthe plan-writeback scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: planStream("plan-smoke", "pending", "pending", "pending", "call_p1")},
				{Reply: textStream("计划已记录：三步都没开始。", 1300)},
				{Reply: planStream("plan-smoke", "completed", "in_progress", "pending", "call_p2")},
				{Reply: textStream("第一步完成，第二步进行中。", 1700)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))

				plans := c.planFiles()
				c.check("磁盘上只有一份计划（同 plan_id 原地更新）", len(plans) == 1, fileList(plans))
				if len(plans) == 1 {
					var saved struct {
						Title  string `json:"title"`
						PlanID string `json:"plan_id"`
						Steps  []struct {
							Content string `json:"content"`
							Status  string `json:"status"`
						} `json:"steps"`
					}
					if err := readJSON(plans[0], &saved); err != nil {
						c.check("计划文件可解析", false, err.Error())
					} else {
						c.check("plan_id 保持稳定", saved.PlanID == "plan-smoke", saved.PlanID)
						statuses := make([]string, 0, len(saved.Steps))
						for _, s := range saved.Steps {
							statuses = append(statuses, s.Status)
						}
						c.check("步骤状态已回写", strings.Join(statuses, ",") == "completed,in_progress,pending",
							strings.Join(statuses, ","))
					}
				}

				// The reminder is the only channel that tells the model to update a plan, and
				// it must be in the prompt of the second turn (the first has no plan yet).
				_, reminded := c.requestSeen("plan_id: `plan-smoke`")
				c.check("第二轮 prompt 带上了 plan 提醒与 plan_id", reminded, "")

				// And the reminder must not repeat per iteration (the fix this suite pins):
				// the tool round appends no new block, so the plan copies in the prompt do
				// not grow from one request to the next inside a turn.
				if len(c.requests) >= 4 {
					before, after := planCopies(c.requests[2]), planCopies(c.requests[3])
					c.check("工具轮没有重复注入 plan 提醒", before == after,
						strconv.Itoa(before)+" → "+strconv.Itoa(after)+" 处 Active plan")
					last := c.requests[3].Messages[len(c.requests[3].Messages)-1]
					c.check("工具轮后没有再追加 reminder 消息", !strings.Contains(last.Content, "Active plan"),
						truncate(strings.ReplaceAll(last.Content, "\n", " / "), 60))
				}
			},
		},
	}
}

// planStream is one SavePlan tool call. The statuses are what the write-back moves.
func planStream(planID, s1, s2, s3, callID string) mockllm.ReplyFunc {
	args := `{"title":"冒烟计划","content":"# 冒烟计划\n\n三步。","plan_id":"` + planID + `","steps":[` +
		`{"content":"第一步","status":"` + s1 + `"},` +
		`{"content":"第二步","status":"` + s2 + `"},` +
		`{"content":"第三步","status":"` + s3 + `"}]}`
	return mockllm.Stream(
		mockllm.ToolCallStart(callID, "SavePlan", args),
		mockllm.Finish("tool_calls"),
		mockllm.UsageWithCache(1400, 50, 1300, 10),
		mockllm.Done(),
	)
}

// jsonArgs marshals a tool call's arguments.
//
// The other stream builders in this file hand-build their JSON, which is fine while the values
// hold no escapes — but a single real newline inside a JSON string makes the whole arguments
// object invalid, and the tool then fails on a fixture that looks correct in the source. So
// anything carrying free text goes through the encoder.
func jsonArgs(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("smoke fixture: " + err.Error())
	}
	return string(b)
}

// writeFileStream is one WriteFile tool call. It exists so a turn can be a turn that CHANGED
// something: the footer's review entry (and 「完整 diff」) only appear on such a turn.
func writeFileStream(path, content, callID string) mockllm.ReplyFunc {
	args := jsonArgs(map[string]string{"path": path, "content": content})
	return mockllm.Stream(
		mockllm.ToolCallStart(callID, "WriteFile", args),
		mockllm.Finish("tool_calls"),
		mockllm.UsageWithCache(1500, 40, 1400, 10),
		mockllm.Done(),
	)
}

// findingStream is one ReportFinding call — the review's structured output, and what the
// conversation's anchor counts (the backend tallies these calls, see emitOneOff).
func findingStream(path string, line int, severity, callID string) mockllm.ReplyFunc {
	args := jsonArgs(map[string]any{
		"path": path, "line": line, "severity": severity, "category": "Quality",
		"text": "这一行可以更清楚", "suggestion": "补一句注释说明",
	})
	return mockllm.Stream(
		mockllm.ToolCallStart(callID, "ReportFinding", args),
		mockllm.Finish("tool_calls"),
		mockllm.UsageWithCache(1200, 50, 1100, 10),
		mockllm.Done(),
	)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func requestCount(reqs []*mockllm.RecordedRequest) string {
	return strconv.Itoa(len(reqs)) + " 个请求"
}

func fileList(paths []string) string {
	return strings.Join(paths, ", ")
}

