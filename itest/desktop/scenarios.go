package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/session"
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
	// config is a YAML block appended to the sandbox config.yaml (see sandbox.writeConfig).
	// Empty for scenarios that need no settings beyond the shared provider/mock wiring.
	config string
	// extraRoots are ADDITIONAL workspace roots for the session, each entry a directory the
	// runner creates under the sandbox holding that entry's files. They are seeded rather than
	// added in the UI because the UI's only way in is a native directory picker, which a driver
	// cannot click — and a session's root set is what the multi-root behaviour is about.
	//
	// From the work dir a scenario reaches one as "../<name>/…": a scripted bash command cannot
	// know the sandbox's absolute path, and it does not have to.
	extraRoots map[string]map[string]string
	// project seeds a desktop project (design §6.1) and BINDS the seeded session to it. Seeded
	// for the same reason extraRoots are: the project form can only pick directories through a
	// native picker. The seeded session's own WorkingDir stays the sandbox work dir, i.e. a
	// deliberately STALE snapshot — every path the scenario asserts on must come from the
	// project, and a fixture where the two agree would prove nothing. A second, UNBOUND session
	// is seeded alongside it so the sidebar really has both of its groups.
	project *projectSeed
	// seedMessages are written into the seeded session's transcript BEFORE the app launches, so
	// the FIRST render of that session comes off disk. A driver cannot produce a reload on its
	// own: switching sessions inside one process serves the in-memory copy, and a restart is not
	// something a driver can do — but a seeded transcript is exactly what a restart would find,
	// which is how at-file-reload stages its assertion.
	seedMessages []session.Message
	// seedSecond writes a SECOND session's transcript, one the app has never displayed. The app
	// opens the newest session, so this one's first render happens when a driver CLICKS its row —
	// the load-on-first-visit path (the 加载会话… placeholder), which a session that is merely
	// re-selected never takes: switching inside one process serves the in-memory copy.
	seedSecond []session.Message
	// after runs once the driver has reported: the Go-side assertions.
	after func(c *checkCtx)
}

// projectSeed describes the desktop-project fixture: the project's primary directory (created
// under the sandbox, holding files) plus additional roots named the way extraRoots are.
type projectSeed struct {
	name       string
	files      map[string]string
	extraRoots map[string]map[string]string
	// emptyName adds a SECOND project with no sessions at all — the state every project starts
	// in, and the one the sidebar used to hide (a create that "did nothing"). It is given an
	// older CreatedAt so it sorts after the member's project, which keeps the assertions that
	// read the FIRST header pointing at the same project.
	emptyName string
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

// rawSeen looks for a substring in the requests' RAW bodies. It is the only way to assert on an
// IMAGE part: the normalized view keeps text only (mockllm's normalize concatenates the text parts
// and drops the rest), so a pasted image is invisible to requestSeen by construction — which is
// exactly why the assertion has to reach for the bytes.
func (c *checkCtx) rawSeen(want string) (int, bool) {
	for i, r := range c.requests {
		if strings.Contains(string(r.RawBody), want) {
			return i + 1, true
		}
	}
	return 0, false
}

// projectRoot is the fixture's project primary directory: the tree a bound session must work
// in, and — after the driver deletes the project — the tree its snapshot is refreshed to.
func (c *checkCtx) projectRoot() string { return filepath.Join(c.dir, projectRootDir) }

// projectsFileHas reports whether the sandbox's projects.json still holds a project by that
// name. It reads the file the desktop writes, i.e. the state the next process would find.
func (c *checkCtx) projectsFileHas(name string) bool {
	body, err := os.ReadFile(filepath.Join(c.home, ".tachi", "projects.json"))
	if err != nil {
		return false
	}
	var tf struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(body, &tf); err != nil {
		return false
	}
	for _, p := range tf.Projects {
		if p.Name == name {
			return true
		}
	}
	return false
}

// sessionByTitle loads a seeded session's record from the sandbox store: the probes that check
// meta (a binding, a refreshed snapshot) must read the FILE, not anything the app reported.
func (c *checkCtx) sessionByTitle(title string) *session.Session {
	store, err := session.NewFileStore(filepath.Join(c.home, ".tachi", "session"))
	if err != nil {
		return nil
	}
	list, err := store.ListSessions()
	if err != nil {
		return nil
	}
	for _, s := range list {
		if s.Title == title {
			return s
		}
	}
	return nil
}

// requestAt returns request n's messages as one string (1-based, the same numbering
// requestSeen reports), for an assertion that has to hold of ONE request rather than of the
// run: a scenario whose fact also appears in every request's system prompt (a workspace root,
// say) cannot be pinned by搜 the whole transcript.
func (c *checkCtx) requestAt(n int) string {
	if n < 1 || n > len(c.requests) {
		return ""
	}
	var b strings.Builder
	for _, m := range c.requests[n-1].Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
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

// rewoundSidecars lists the files a session TRUNCATION preserved: the abandoned branch a rewind
// moves into <session>/rewound/*. A refused rewind must leave none — that is the filesystem half
// of "nothing moved", and it is what tells a refusal apart from a rewind that quietly happened.
func (c *checkCtx) rewoundSidecars() []string {
	matches, _ := filepath.Glob(filepath.Join(c.home, ".tachi", "session", "*", "rewound", "*"))
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
	// slowWritePause holds a WriteFile round before it runs, so a turn's file set stays
	// incomplete for a measurable stretch — oneoff-footer uses it to test that nothing acts
	// on the turn's changes before the turn is done.
	slowWritePause = 2 * time.Second
	// projectRulesMarker is one SENTENCE of the rules that are injected with a .tachi.md
	// (agent/systemreminder/project_reminder.go). It is asserted on at the LLM boundary,
	// which is the only place the whole contract can be proven to travel: the rules are
	// emitted by the reminder rather than written into the file, so a repo cannot be
	// expected to carry them. Rewording them means updating this marker — and that is the
	// point.
	projectRulesMarker = "Keep it true, in the same turn"

	// reportWritePause is how long the report round waits before writing its report. It exists
	// for the window it opens, not for realism: the driver has to reach the 报告 pane and read it
	// while the file is still missing, and a window too narrow to observe is a race the assertion
	// cannot prove anything about (see the oneoff-report scenario — its timing lines say how
	// narrow the window actually has to be). Widened, never trimmed.
	reportWritePause = 12 * time.Second
	// smokeReportMarker is a string nothing else in a smoke run produces, so finding it in the
	// pane proves the text came from the report FILE.
	smokeReportMarker = "SMOKE-REPORT-MARKER"

	// fileFindLine / fileFindHits: the file that file-find previews repeats ONE line, so both
	// halves can name the number of hits instead of settling for "some".
	fileFindLine = "一行笔记，用来被查找。"
	fileFindHits = 12
)

// smokeReportMarkdown is the report body the mock writes: the marker plus enough prose that the
// markdown renderer has something to lay out.
var smokeReportMarkdown = "# 评审报告\n\n" + smokeReportMarker + "\n\n这是评审写的报告正文，用来验证报告页。\n"

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

// textStreamPrompt is textStream with an explicit PROMPT size. Scenarios about the context meter
// need the mock to report a prompt that matches the conversation it was given: the meter is anchored
// on what the provider billed (llm.PromptTokens), so a fixed 1200-token report standing next to a
// 30k-token history is a state the app cannot reach in production — the scenario would be testing
// the mock's arithmetic instead of the app's. (The meter used to show the local estimate, which is
// why the old fixed numbers went unnoticed; see docs/agents/desktop.md.)
func textStreamPrompt(text string, cacheRead, prompt int) mockllm.ReplyFunc {
	return mockllm.Stream(
		mockllm.Text(text),
		mockllm.Finish("stop"),
		mockllm.UsageWithCache(prompt, 60, cacheRead, 20),
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
		// 插话（steer，不是"立即发送"）：回合进行中排队的那条，在**工具调用的间隙**被自动插入。
		// 它同时改变折叠的形状——`injectSteerVisual` 把这一轮切成两段（两条助手消息），每段按自己
		// 的 parts 折叠：于是"插话之前那段正文"成了它那一段的**结论**（恒显），而"插话之后那段的
		// 中间正文"照旧折起来。这一条正是"steer 会不会改变折叠行为"的答案，两个方向都要断。
		//
		{
			name: "steer-fold",
			files: map[string]string{
				"README.md": "# smoke\n\nthe steer-fold scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// 第 1 段：正文 + 一条慢命令。sleep 3 是给驱动的窗口——它必须在这条命令还在跑的时候
				// 把插话排进队列，插话才会走 steer 这条路（而不是回合结束后的自动补发）。
				{Reply: mockllm.Stream(
					mockllm.Text("第一段：先跑一条慢命令。"),
					mockllm.ToolCallStart("call_s1", "Bash", `{"command":"sleep 3 && echo steer-ok"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(1500, 40, 1400, 10),
					mockllm.Done(),
				)},
				// 第 2 段（插话之后）：正文 + 一条命令。这段的正文是"中间说明"，收尾结论另有一段。
				{Reply: mockllm.Stream(
					mockllm.Text("收到插话了：这是插话之后那一段的开头。"),
					mockllm.ToolCallStart("call_s2", "Bash", `{"command":"echo steer-after"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(1500, 40, 1400, 10),
					mockllm.Done(),
				)},
				{Reply: textStream("两段都跑完了。", 1200)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("三次调用（两段 + 收尾）", len(c.requests) == 3, requestCount(c.requests))
				// 插话在**第 2 次**请求里到达模型：requestSeen 返回第一处命中的序号，所以这条同时
				// 证明它没有出现在第 1 次（那才是"已经插进去了"）。
				at, steered := c.requestSeen("插队这条：先做这件")
				c.check("插话作为下一轮的 user 消息注入（第 2 次请求）", steered && at == 2,
					fmt.Sprintf("命中第 %d 次请求", at))
				_, ran := c.requestSeen("steer-ok")
				c.check("慢命令真的执行过", ran, "")
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
		// bash-diff: the turn footer, 「完整 diff」 and 「评审本轮改动」 must all describe what the
		// turn ACTUALLY changed — and this turn changes files only through BASH, which no tool
		// argument declares. All three used to read the tool calls: such a turn rendered no chip
		// at all, the panel had nothing to diff, and the reviewer was handed an empty scope.
		// They now read the turn's own two checkpoint trees, so the file list, the line counts
		// and the reviewer's scope are taken from the same diff. The Go half asserts the review
		// PROMPT (which files it names, and that it is pointed at the frozen pair, not at HEAD).
		{
			name: "bash-diff",
			files: map[string]string{
				"README.md": "# smoke\n\nthe bash-diff scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// Turn 1: one shell command writes both files.
				{Reply: bashStream("printf 'a\\nb\\nc\\n' > made.txt && printf 'x\\ny\\n' > other.txt", "call_b")},
				{Reply: textStream("两个文件写好了。", 800)},
				// The review the driver starts from the chip. Its finding is ON made.txt, a file
				// only a shell command created.
				{Reply: findingStream("made.txt", 2, "warn", "call_f1")},
				{Reply: textStream("评审完成：1 条意见。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("对话一轮 + 评审一轮共四次调用", len(c.requests) == 4, requestCount(c.requests))
				// The reviewer is told about the files the SHELL wrote — the scope comes from the
				// checkpoint's trees, so it contains what no tool call declared.
				for _, name := range []string{"made.txt", "other.txt"} {
					n, seen := c.requestSeen(name)
					c.check("评审 scope 里包含 shell 写出的 "+name, seen, fmt.Sprintf("出现在第 %d 个请求", n))
				}
				if n, seen := c.requestSeen("--git-dir="); seen {
					c.check("评审被告知读这一轮的两棵树，而不是 git diff HEAD", true, fmt.Sprintf("第 %d 个请求", n))
				} else {
					c.check("评审被告知读这一轮的两棵树，而不是 git diff HEAD", false, "prompt 里没有冻结 diff 命令")
				}
			},
		},
		//
		// frozen-panel: the OTHER half of reading the checkpoint — the review's own panel. A review
		// reads the turn's two trees, and its findings are shown against that same diff; when the
		// pane read the WORKING TREE instead, a review of changes that were committed (or deleted)
		// since rendered as an empty pane whose findings all looked like they named files the turn
		// never touched. Turn 2 here deletes the two files turn 1 wrote, so nothing is left in the
		// working tree: turn 1's 「完整 diff」, the review, and the pane must all still show them.
		{
			name: "frozen-panel",
			files: map[string]string{
				"README.md": "# smoke\n\nfrozen-panel scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// Turn 1: one shell command writes both files.
				{Reply: bashStream("printf 'a\\nb\\nc\\n' > made.txt && printf 'x\\ny\\n' > other.txt", "call_z1")},
				{Reply: textStream("两个文件写好了。", 800)},
				// Turn 2: a shell command deletes them — the working tree is clean from here on.
				{Reply: bashStream("rm -f made.txt other.txt", "call_z2")},
				{Reply: textStream("已经删掉了。", 900)},
				// The review of turn 1, started from its chip, and its re-run from the pane.
				{Reply: findingStream("made.txt", 2, "warn", "call_z3")},
				{Reply: textStream("评审完成：1 条意见。", 1000)},
				// TWO findings this time: the chip reports the backend's tally of ReportFinding
				// calls, so a different number is the driver's proof that a NEW run happened
				// (a reply that merely says "2 条" would prove nothing).
				{Reply: findingStream("other.txt", 1, "info", "call_z4")},
				{Reply: findingStream("made.txt", 3, "warn", "call_z5")},
				{Reply: textStream("评审完成：2 条意见。", 1100)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("两轮对话 + 评审两次共九次调用", len(c.requests) == 9, requestCount(c.requests))
				// The reviewer is pointed at the turn's OWN two trees, not at HEAD: it has to be
				// readable even though the files are gone, and both runs are told the same thing.
				for _, name := range []string{"made.txt", "other.txt"} {
					n, seen := c.requestSeen(name)
					c.check("评审 prompt 点名了已经不在磁盘上的 "+name, seen, fmt.Sprintf("出现在第 %d 个请求", n))
				}
				if n, seen := c.requestSeen("--git-dir="); seen {
					c.check("评审被告知读这一轮的两棵树", true, fmt.Sprintf("第 %d 个请求", n))
				} else {
					c.check("评审被告知读这一轮的两棵树", false, "prompt 里没有冻结 diff 命令")
				}
			},
		},
		//
		// multi-root: the session has an ADDITIONAL workspace root, and both it and the primary
		// hold a file with the SAME relative name. 「本轮改动」 has to say which tree each file
		// belongs to: the panel resolves a file's absolute path from its root (without it, the
		// additional root's file resolves against the primary and 预览/打开 shows the wrong
		// one, silently), and the review's scope groups the paths by root for the same reason.
		{
			name: "multi-root",
			files: map[string]string{
				"README.md": "# smoke\n\nmulti-root scenario's primary working directory\n",
				"notes.md":  "primary\n",
			},
			// Seeded, not added in the UI: the only way in is a native directory picker.
			extraRoots: map[string]map[string]string{
				"shared-lib": {"notes.md": "lib\n"},
			},
			steps: []mockllm.Step{
				// One shell command changes the same relative path in BOTH roots. The second is
				// reached as "../shared-lib/…": the scripted command cannot know the sandbox's
				// absolute path, and the roots are sibling directories by construction.
				{Reply: bashStream("printf 'main-change\\n' >> notes.md && printf 'lib-change\\n' >> ../shared-lib/notes.md", "call_mr1")},
				{Reply: textStream("两边都改好了。", 800)},
				{Reply: findingStream("notes.md", 2, "warn", "call_mr2")},
				{Reply: textStream("评审完成：1 条意见。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("一轮对话加一次评审，共四次调用", len(c.requests) == 4, requestCount(c.requests))
				// The reviewer is told WHICH tree each file is in: a flat list would leave two
				// "notes.md" entries meaning two different files.
				// Both facts have to hold of the SAME request — the review's. The roots are in
				// every request's system prompt, so searching the run for the path would pass
				// even with no grouping at all.
				groupedAt, _ := c.requestSeen("working directories of this session")
				c.check("评审 prompt 说明这些文件分属多个工作目录", groupedAt > 0, fmt.Sprintf("第 %d 个请求", groupedAt))
				shared := filepath.Join(c.dir, "shared-lib")
				scope := c.requestAt(groupedAt)
				c.check("分组里点到了 additional root 的路径", strings.Contains(scope, shared),
					fmt.Sprintf("request %d 里没有 %s", groupedAt, shared))
				c.check("分组里两个 root 的清单都在", strings.Contains(scope, "(primary)") && strings.Contains(scope, "shared-lib —"),
					truncate(scope, 200))
				if n, seen := c.requestSeen("--git-dir="); seen {
					c.check("评审被告知读这两棵树", true, fmt.Sprintf("第 %d 个请求", n))
				} else {
					c.check("评审被告知读这两棵树", false, "prompt 里没有冻结 diff 命令")
				}
				// The filesystem half: both roots really were changed (a scenario whose second
				// root never got written would fail every DOM assertion for the wrong reason).
				for _, p := range []string{
					filepath.Join(c.work, "notes.md"),
					filepath.Join(shared, "notes.md"),
				} {
					data, err := os.ReadFile(p)
					c.check("两个 root 的 notes.md 都被改过: "+p, err == nil && strings.Contains(string(data), "-change"),
						fmt.Sprintf("err=%v content=%q", err, truncate(string(data), 40)))
				}
			},
		},
		//
		//
		// Rewind: "回退到这里" on a user bubble must put the workspace AND the conversation
		// back to the start of that turn. The file the agent wrote is written through BASH
		// (not EditFile/WriteFile), which is the coverage no other agent's checkpoints
		// provide. The work dir is deliberately NOT a git repository: the snapshot store is
		// private to the session, so it has to work without one.
		{
			name: "rewind",
			files: map[string]string{
				"README.md": "# smoke\n\nthe rewind scenario's working directory\n",
				"keep.txt":  "original\n",
			},
			steps: []mockllm.Step{
				// Turn 1: one shell command that both creates and edits a file.
				{Reply: bashStream("echo made > made.txt && echo changed > keep.txt", "call_w")},
				{Reply: textStream("写好了。", 800)},
				// Turn 2: a plain reply, so there is a later turn for the rewind to undo
				// (and a second bubble for the "its own prompt goes back to the composer"
				// half). The FILE state it leaves behind is what turn 1's snapshot restores.
				{Reply: textStream("第二轮回复。", 800)},
				// Turn 3, sent after the first rewind: a reply with no tool call at all, so
				// this turn writes nothing — the shape whose rewind must work anyway and say
				// that the workspace needs no restoring.
				{Reply: textStream("第三轮回复。", 800)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("三轮共四次调用", len(c.requests) == 4, requestCount(c.requests))

				// The shell command's changes are the ones a rewind has to undo. These run
				// AFTER the read-only turn's own rewind too, so they also say that a turn with
				// nothing to restore leaves the workspace exactly as it found it.
				_, statErr := os.Stat(filepath.Join(c.work, "made.txt"))
				c.check("回退删掉了 shell 新建的文件", os.IsNotExist(statErr), statErrText(statErr))
				content, readErr := os.ReadFile(filepath.Join(c.work, "keep.txt"))
				c.check("回退还原了 shell 改过的文件", readErr == nil && string(content) == "original\n", string(content))
			},
		},
		//
		// 重启后加载一条带 @-file 的用户消息：气泡必须显示用户打的 `@path`，而不是被内联进去的
		// 整份文件正文。展开只该活在"发给模型的那份"里（session.Message.Content），记录里另留
		// 一份用户原文（DisplayContent），重建转写读后者。
		//
		// 断言必须打在**从磁盘渲染**的那一次上：进程内切会话用的是内存里的转写（气泡是前端自己
		// 追加的原文，本来就不会错），所以这条的转写由 fixture 预置 —— 那正是重启会看到的东西。
		// 会话里没有任何要发出去的消息，所以 mock 一个请求都不该收到。
		//
		{
			name: "at-file-reload",
			files: map[string]string{
				"README.md": "# smoke\n\nthe at-file-reload scenario's working directory\n",
			},
			seedMessages: []session.Message{
				{
					Type: session.MessageTypeUser,
					Content: "@README.md 看看开头\n\n--- BEGIN UNTRUSTED FILE CONTENT: README.md ---\n" +
						"# smoke\n\nthe at-file-reload scenario's working directory\n--- END UNTRUSTED FILE CONTENT: README.md ---\n",
					DisplayContent: "@README.md 看看开头",
				},
				{Type: session.MessageTypeAssistant, Content: "看过了。"},
			},
			after: func(c *checkCtx) {
				c.check("预置的会话没有被多余地跑起来", len(c.requests) == 0, requestCount(c.requests))
			},
		},
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

				// @-file 的两份文本：Content 是模型收到的那份（文件被内联进去），
				// DisplayContent 是用户打的原文。展开只该活在"发出去的那份"里 —— 会话记录
				// 若只有展开文本，重启/切回时气泡里就是整个文件正文（这条曾经就是这样）。
				found := 0
				var withDisplay, plain session.Message
				for _, f := range sessionFiles(c.home) {
					b, err := os.ReadFile(f)
					if err != nil {
						continue
					}
					for _, line := range strings.Split(string(b), "\n") {
						if strings.TrimSpace(line) == "" {
							continue
						}
						var m session.Message
						if json.Unmarshal([]byte(line), &m) != nil || m.Type != session.MessageTypeUser {
							continue
						}
						found++
						if m.DisplayContent != "" {
							withDisplay = m
						} else {
							plain = m
						}
					}
				}
				c.check("两个会话各留下一条 user 记录", found == 2, fmt.Sprintf("%d 条", found))
				c.check("被 @ 展开的那条留下了用户原文（DisplayContent）",
					withDisplay.DisplayContent == "@README.md 里的说明", withDisplay.DisplayContent)
				c.check("发出去的那份仍是展开文本（模型看到的东西不许变）",
					strings.Contains(withDisplay.Content, "the sessions scenario's working directory"),
					strutil.Truncate(withDisplay.Content, 120))
				c.check("普通那条不带 DisplayContent（不写重复的字段）", plain.DisplayContent == "",
					plain.DisplayContent)
			},
		},
		//
		// A turn in flight while the reader moves between sessions: the stop control belongs to
		// the session that is actually running, and a background turn's END has to reach that
		// session. Before the fix the backend pushed a state only while its session was the
		// displayed one and the frontend kept the one value it received as a global, so the red
		// ring followed the reader into an idle session and stayed there forever — and clicking it
		// did nothing, because Stop acts on the displayed session's turn.
		//
		{
			name: "session-running",
			files: map[string]string{
				"README.md": "# smoke\n\nthe session-running scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// The first turn is deliberately LONG (the pauses are the point): the driver does
				// its switching inside that window, and the turn also has to outlive them so the
				// END lands while another session is on screen.
				{Reply: mockllm.Stream(
					mockllm.Text("这一轮要跑一会儿："),
					mockllm.Pause(longTurn),
					mockllm.Text("趁着它还在跑，"),
					mockllm.Pause(longTurn),
					mockllm.Text("读者在别的会话之间来回切，"),
					mockllm.Pause(longTurn),
					mockllm.Text("这个回合结束后，"),
					mockllm.Pause(longTurn),
					mockllm.Text("长回合收尾标记：任何一个没在跑的会话都不该留着停止按钮。"),
					mockllm.Pause(longTurn),
					mockllm.Finish("stop"),
					mockllm.UsageWithCache(1200, 120, 900, 20),
					mockllm.Done(),
				)},
				// The second turn: a new session is created while it runs, so the new conversation
				// must be clean and the running one must keep its own marker.
				{Reply: mockllm.Stream(
					mockllm.Text("第二个回合。"),
					mockllm.Pause(longTurn),
					mockllm.Text("跑完了。"),
					mockllm.Finish("stop"),
					mockllm.UsageWithCache(1200, 60, 900, 20),
					mockllm.Done(),
				)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// The driver sends from the fixture session alone; a third request would mean a
				// session switch sent something by itself.
				c.check("只有 fixture 会话里的两条请求到达模型", len(c.requests) == 2, requestCount(c.requests))
				// fixture + the two sessions the driver created.
				c.check("切换会话不会凭空造出新会话", c.sessionCount() == 3, strconv.Itoa(c.sessionCount()))
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
				{Reply: textStreamPrompt(strings.Repeat("这是一段很长的历史内容，用来把上下文撑起来。", 1500), 600, 8000)},
				// …and a SECOND turn, because the estimate describes the PROMPT of the last call:
				// a reply only enters the measurement when the next call is made with it in the
				// history. Without this turn the big reply is never counted, and "before" would
				// read the same floor as "after".
				{Reply: textStreamPrompt("第二轮回复：确认。", 600, 20000)},
				// The /compact turn: the model has to produce the summary the child session is
				// built from (an empty reply makes the command refuse).
				{Reply: textStreamPrompt("历史摘要：用户要求查看工作目录；已确认只有一个 README.md。", 600, 22000)},
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

				// The driver right-clicks a turn in the PRE-compaction session and is refused: a
				// rewind there would move the workspace back before the summary while the
				// conversation continues in the child. The filesystem half of that refusal is
				// this — a truncation would have preserved the abandoned branch in rewound/.
				if sides := c.rewoundSidecars(); len(sides) != 0 {
					c.check("被拒的回退没有截断任何会话（没有 rewound 侧车）", false, strings.Join(sides, ", "))
				} else {
					c.check("被拒的回退没有截断任何会话（没有 rewound 侧车）", true, "")
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
				//
				// The SECOND write is parked for two seconds: the turn's file set is not final
				// until that write lands, and the driver's earlier version acted on a proxy that
				// is true after the FIRST one (see the driver). The pause makes that window big
				// enough to be a test rather than a race nobody can reproduce.
				{Reply: writeFileStream(0, "NOTES.md", strings.Repeat("一行笔记：这一行只是为了让 diff 足够长，面板必须滚动才看得到下面的意见。\n", 80), "call_w1")},
				{Reply: writeFileStream(slowWritePause, "OTHER.md", "another file\n", "call_w2")},
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
		// The 报告 pane over a report that did not exist when the pane was opened.
		//
		// A run's report PATH is recorded when the run STARTS — it is what the round's prompt
		// tells the model to write — so the tab is there from the first second while the file
		// itself only arrives at the END. A read taken in between used to be stored under the
		// run's own key, which is also what the code read as "already have it": the pane said
		// 「报告是空的」 for a report that was written a moment later, and never looked again
		// (「review 过后，点击报告页，是空的」). The pause below is what makes that window
		// testable rather than raced: without it the driver's read would happen after the write
		// and nothing would be under test.
		{
			name: "oneoff-report",
			files: map[string]string{
				"README.md": "# smoke\n\nthe oneoff-report scenario's working directory\n",
			},
			steps: []mockllm.Step{
				reportWriteStep(reportWritePause, smokeReportMarkdown, "call_rep1"),
				{Reply: textStream("评审完成，报告已写入。", 1100)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("评审这一轮调用了两次模型", len(c.requests) == 2, requestCount(c.requests))
				// "The prompt named the path" is pinned by the step's own Require, which fails the
				// run when it cannot find one — a round whose prompt lost the save instruction
				// would leave the mock with nowhere to write.

				// The mock wrote to the path it read out of the REQUEST, so the file existing at
				// the orchestrator's own path is the same fact the pane goes looking for.
				matches, _ := filepath.Glob(filepath.Join(c.work, ".tachi", "reviews", "*", "round-*.md"))
				c.check("报告按 prompt 给的确切路径落盘", len(matches) == 1, fileList(matches))
				if len(matches) == 1 {
					b, err := os.ReadFile(matches[0])
					c.check("报告里是模型写的那份正文", err == nil && strings.Contains(string(b), smokeReportMarker),
						errText(err))
				}
				// The read the driver did while the file was still missing left nothing behind on
				// disk: the pane's own state is the driver's business, asserted there.
			},
		},
		//
		// ⌘F in a previewed FILE — the third surface that hosts find, next to the diff and the
		// report. The file repeats one line so the driver can name the count, and it arrives as an
		// attachment because that is how a file gets a card, and the card is what has the ⤢ that
		// opens the document viewer.
		{
			name: "file-find",
			files: map[string]string{
				"LONG.md": "# smoke\n\n" + strings.Repeat(fileFindLine+"\n\n", fileFindHits),
			},
			steps: []mockllm.Step{
				{Reply: sendFileStream("LONG.md", "call_send")},
				{Reply: textStream("已经把 LONG.md 发给你了。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("一轮对话两次调用（送文件 + 收尾）", len(c.requests) == 2, requestCount(c.requests))
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
		//
		// mermaid-zoom: a diagram rendered in a reply, clicked open. The lightbox has to show it
		// BIGGER than the message did — that is the whole point of clicking it. It is measured in
		// three places (the inline svg, the same svg in the overlay, the stage) because "it opened
		// tiny" can come from either end: a fit that computed wrong, or a diagram that never got
		// its intrinsic size inside the overlay.
		{
			name: "mermaid-zoom",
			files: map[string]string{
				"README.md": "# smoke\n\nmermaid-zoom scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// Wide on purpose (six nodes in a row): a diagram that already fills the message
				// area is the case where "the overlay shows it smaller" is obvious.
				{Reply: textStream("先看一张图：\n\n```mermaid\ngraph LR\n  A[读取配置] --> B[校验]\n  B --> C[建索引]\n  C --> D[跑任务]\n  D --> E[写报告]\n  E --> F[通知]\n```\n\n图看完了。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("一轮对话调用了一次模型", len(c.requests) == 1, requestCount(c.requests))
			},
		},
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
		//
		// 权限确认（允许）: a bash command matches an `ask` rule, so the agent parks the turn
		// on the user instead of refusing the command. The driver clicks 允许一次; the half
		// that proves the command ACTUALLY RAN is here — the tool result (the content of the
		// file the command read) has to be in the next request's messages, and only a run
		// command produces it.
		//
		// perm-deny is the negative control that keeps this one honest: without it, a build
		// where the ask rule silently did not match would run the command unprompted and the
		// driver's click would land on nothing.
		//
		{
			name: "perm-allow",
			files: map[string]string{
				"README.md":        "# smoke\n\nthe perm-allow scenario's working directory\n",
				"perm-fixture.txt": "PERM-FIXTURE-CONTENT\n",
			},
			config: `
permissions:
  bash:
    ask:
      - "cat perm-fixture*"
`,
			steps: []mockllm.Step{
				{Reply: bashStream("cat perm-fixture.txt", "call_pa1")},
				{Reply: textStream("读过文件了：内容在上面。", 1500)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// The command's OUTPUT — not its arguments — is what proves it ran.
				_, ran := c.requestSeen("PERM-FIXTURE-CONTENT")
				c.check("允许后命令真的执行了（工具结果回喂给了模型）", ran, requestCount(c.requests))
			},
		},
		//
		// 权限确认（拒绝）: the same rule and the same command, but the driver clicks 拒绝.
		// The command must NOT run, and the model must be told the user refused it.
		//
		{
			name: "perm-deny",
			files: map[string]string{
				"README.md":        "# smoke\n\nthe perm-deny scenario's working directory\n",
				"perm-fixture.txt": "PERM-FIXTURE-CONTENT\n",
			},
			config: `
permissions:
  bash:
    ask:
      - "cat perm-fixture*"
`,
			steps: []mockllm.Step{
				{Reply: bashStream("cat perm-fixture.txt", "call_pd1")},
				{Reply: textStream("好，那条命令我不执行了。", 1500)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				_, ran := c.requestSeen("PERM-FIXTURE-CONTENT")
				c.check("拒绝后命令没有执行（工具结果里没有文件内容）", !ran, requestCount(c.requests))
				_, told := c.requestSeen("denied")
				c.check("模型被告知这条命令被拒绝", told, "")
			},
		},
		//
		// 项目上下文与它的规矩: the session's working directory carries a .tachi.md, so the
		// first message of the conversation gets it injected — TOGETHER with the standing
		// rules that ride with it (see agent/systemreminder/project_reminder.go). The rules
		// live in the reminder rather than in the file precisely so they apply to every repo,
		// which makes this the end-to-end proof that the contract travels with the content.
		//
		{
			name: "project-context",
			files: map[string]string{
				"README.md": "# smoke\n\nthe project-context scenario's working directory\n",
				".tachi.md": "# 冒烟项目约定\n\n- SMOKE-PROJECT-RULE: 只改 NOTES.md，不要碰别的文件\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("知道了：只改 NOTES.md。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))

				_, projectSeen := c.requestSeen("SMOKE-PROJECT-RULE")
				c.check("工作目录里的 .tachi.md 进了 prompt", projectSeen, requestCount(c.requests))

				// The marker is a SENTENCE of the rules, not a heading — the same way
				// oneoff-footer pins its prompt sections: a heading contains the section's
				// `##` prefix, so a heading-shaped marker would pass even if the section
				// were never appended.
				_, rulesSeen := c.requestSeen(projectRulesMarker)
				c.check("随 .tachi.md 一起注入的规矩也进了 prompt", rulesSeen, "")
				if !projectSeen || !rulesSeen {
					return
				}
				// And the rules come first: they are the contract for the file below them.
				for _, r := range c.requests {
					for _, m := range r.Messages {
						rules := strings.Index(m.Content, projectRulesMarker)
						content := strings.Index(m.Content, "SMOKE-PROJECT-RULE")
						if rules >= 0 && content >= 0 {
							c.check("规矩排在项目内容之前", rules < content,
								fmt.Sprintf("rules@%d content@%d", rules, content))
							return
						}
					}
				}
				c.check("规矩与项目内容在同一条提醒里", false, "两处标记没有出现在同一条消息里")
			},
		},
		//
		// 本会话全部允许: two DIFFERENT commands match the same ask rule. The first one asks,
		// the driver answers 「本会话全部允许」, and the second must run with no card at all —
		// which is the whole point of the choice: an agent's shell commands do not repeat
		// verbatim, so a per-command memory would ask again here. The second command
		// deliberately matches the SAME rule, so a build where the switch did nothing would
		// park on it and never reach the mock's third step.
		//
		{
			name: "perm-session",
			files: map[string]string{
				"README.md":          "# smoke\n\nthe perm-session scenario's working directory\n",
				"perm-fixture.txt":   "PERM-FIXTURE-CONTENT\n",
				"perm-fixture-2.txt": "PERM-FIXTURE-2-CONTENT\n",
			},
			config: `
permissions:
  bash:
    ask:
      - "cat perm-fixture*"
`,
			steps: []mockllm.Step{
				{Reply: bashStream("cat perm-fixture.txt", "call_s1")},
				{Reply: bashStream("cat perm-fixture-2.txt", "call_s2")},
				{Reply: textStream("两个文件都读完了。", 1500)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("三步都跑到了（第二条没有被挂住）", len(c.requests) == 3, requestCount(c.requests))
				for _, marker := range []string{"PERM-FIXTURE-CONTENT", "PERM-FIXTURE-2-CONTENT"} {
					_, ran := c.requestSeen(marker)
					c.check("命令真的执行了："+marker, ran, "")
				}
			},
		},
		//
		// 会话树里的 skill：desktop 的 store 按会话的工作目录建，所以工作目录下 .tachi/skills 的
		// 项目级 skill 会被发现，技能目录随第一条消息进入模型上下文。断言落在模型收到的请求上而不是
		// UI 上：一个 skill 被用掉之前，界面上没有任何东西能看出它存在 —— 而按进程 cwd 建 store
		// (这个场景要防的 bug)会去扫 "/.tachi/skills"，一条目录都发不出来。
		//
		{
			name: "skill-catalog",
			files: map[string]string{
				"README.md":                          "# smoke\n\nthe skill-catalog scenario's working directory\n",
				".tachi/skills/smoke-skill/SKILL.md": "---\nname: smoke-skill\ndescription: SMOKE-SKILL-MARKER from the scenario's own tree\n---\n\nbody: the marker is what the catalog reports.\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("收到。", 1200)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				at, catalogued := c.requestSeen("SMOKE-SKILL-MARKER")
				c.check("工作目录里的项目级 skill 进了模型上下文", catalogued,
					"命中第 "+strconv.Itoa(at)+" 条请求")
				c.check("技能目录随第一条请求到达", catalogued && at == 1, requestCount(c.requests))
			},
		},
		//
		// 过程折叠：一轮的 thinking / 工具卡 / 中间说明收成一行，结论正文恒显；失败不藏
		// （过程条染红 + 那一张卡外露），点开才铺开完整时序。断言分两态：折叠时只应看到
		// 失败那张卡，展开后两张都在。判定不看 UI 数量就下结论——Go 那半另证命令真跑了。
		//
		{
			name: "transcript-fold",
			files: map[string]string{
				"README.md": "# smoke\n\nthe transcript-fold scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// 第 1 轮：一段正文 + 一条成功命令。这段正文是"中间说明"——它会（也应该）被折进过程条。
				{Reply: mockllm.Stream(
					mockllm.Text("第一步：先跑一条命令。"),
					mockllm.ToolCallStart("call_f1", "Bash", `{"command":"echo transcript-fold-ok"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(1500, 40, 1400, 10),
					mockllm.Done(),
				)},
				// 第 2 轮：一段正文 + 一次读不到文件的失败调用。这里钉住那个容易看成 bug 的形状：
				// 失败卡外露，但它**同一轮的正文**（"上一轮的输出"）照样折进过程条——折叠规则与
				// 失败无关，不是"失败了就把正文藏了"。相对路径 → 落在沙箱工作目录里：这个"读不到"
				// 是 fixture 保证的，不是靠机器上恰好没有。
				{Reply: mockllm.Stream(
					mockllm.Text("第二步：读一个不存在的文件。"),
					mockllm.ToolCallStart("call_f2", "ReadFile", `{"path":"missing-file.txt"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(1500, 40, 1400, 10),
					mockllm.Done(),
				)},
				{Reply: textStream("两步都处理完了。", 1200)},
				// 对照轮（整轮无失败）：中间说明同样折起来。没有这一段，"折起来是不是因为失败"就
				// 只能靠读代码回答。
				{Reply: mockllm.Stream(
					mockllm.Text("对照轮第一步：先跑一条命令。"),
					mockllm.ToolCallStart("call_c1", "Bash", `{"command":"echo control-ok-1"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(1500, 40, 1400, 10),
					mockllm.Done(),
				)},
				{Reply: mockllm.Stream(
					mockllm.Text("对照轮第二步：再跑一条命令。"),
					mockllm.ToolCallStart("call_c2", "Bash", `{"command":"echo control-ok-2"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(1500, 40, 1400, 10),
					mockllm.Done(),
				)},
				{Reply: textStream("对照轮的收尾结论。", 1200)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("两轮共六次调用", len(c.requests) == 6, requestCount(c.requests))
				_, ran := c.requestSeen("transcript-fold-ok")
				c.check("成功那一步真的执行了", ran, "")
			},
		},
		//
		// 运行中的样子：工具还在跑时，过程条变成实时条（脉冲点 + 正在什么 + 第几步 + 用时），
		// 输入框上方有同一句活动信息，正在跑的那张卡外露在过程条之外；回合结束后回到摘要、
		// 活动行消失，而且（回复够长、内容超出一屏时）仍然贴在底部。
		//
		{
			name: "transcript-live",
			files: map[string]string{
				"README.md": "# smoke\n\nthe transcript-live scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// sleep 让这一步真的"在跑"几秒，driver 才有东西可看。
				{Reply: bashStream("sleep 4", "call_l1")},
				// 第二步之前故意停一下（模拟真实的首 token 延迟）：这一轮因此在"有工具在跑"与
				// "两次调用之间"之间来回一次 —— 过程条正是每次这样切换时会改高度，把整段可见
				// 内容上下顶。没有这一步，driver 只观察得到回合末尾那一次切换。
				{Reply: mockllm.Stream(
					mockllm.Pause(longTurn),
					mockllm.ToolCallStart("call_l2", "Bash", `{"command":"echo done"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(1500, 40, 1400, 10),
					mockllm.Done(),
				)},
				{Reply: textStream(strings.Repeat("睡完了。这一段足够长，用来把转写撑过一屏，", 120)+"好验证跟随底部。", 800)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("三步都跑到了", len(c.requests) == 3, requestCount(c.requests))
			},
		},
		//
		// 运行中的会话删不掉（desktop/agent_session.go 的 DeleteSession）。拒绝是后端定的，
		// 因为跑着的回合有自己的 goroutine，它的会话写入和 run map 写入都只认这个 id——在它
		// 底下删掉目录，转录会静默丢失，还可能留下一个只有 meta.json 的重建目录。
		//
		// 这里跑的是一轮足够长的回合，driver 在它活着的时候右键当前会话行：删除项必须是禁用
		// 的、点了不弹确认框；停掉这一轮之后，同一个会话又能正常删除。Go 侧只证明这一轮没有
		// 因为删除尝试而多跑（一个请求），即拒绝没有顺手重启什么。
		//
		{
			name: "delete-running",
			files: map[string]string{
				"README.md": "# smoke\n\nthe delete-running scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Text("这一轮要跑得久一点，"),
					mockllm.Pause(longTurn),
					mockllm.Text("好让 driver 有机会在它运行中右键会话行，"),
					mockllm.Pause(longTurn),
					mockllm.Text("试完删除再去按停止。"),
					mockllm.Pause(longTurn),
					mockllm.Pause(longTurn),
					mockllm.Pause(longTurn),
					mockllm.Pause(longTurn),
					mockllm.Finish("stop"),
					mockllm.UsageWithCache(1200, 120, 900, 20),
					mockllm.Done(),
				)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("删除被拒绝后没有多跑一轮", len(c.requests) == 1, requestCount(c.requests))
			},
		},
		//
		//
		// Projects: a session bound to a project works in the PROJECT's tree, tells the reader
		// who owns its workspace, and follows a project edit without being switched to. The
		// seeded session's own record names the sandbox work dir (a stale snapshot), so every
		// path below that is not that one proves resolution came from the project — and the
		// driver's rename/delete cover the refresh contract (§7.4) and the detach (§6.2) end to
		// end, both of which are reachable without a native picker.
		{
			name: "projects",
			files: map[string]string{
				"README.md": "# smoke\n\nthe projects scenario's STALE snapshot directory\n",
			},
			project: &projectSeed{
				name: "smoke-proj",
				files: map[string]string{
					"README.md": "# smoke\n\nthe projects scenario's project directory\n",
				},
				extraRoots: map[string]map[string]string{
					"proj-shared": {"notes.md": "lib\n"},
				},
				// A second project with no sessions: the state a project is created in, which the
				// sidebar used to hide (the reader saw nothing after pressing 保存 — "I made a
				// project and nothing happened"). It must render a header, say it has no sessions
				// yet, and offer the ＋ that makes the first one.
				emptyName: "smoke-empty",
			},
			steps: []mockllm.Step{
				{Reply: textStream("收到。", 400)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("只有一轮对话（driver 的改名与删除都不是对话）", len(c.requests) == 1, requestCount(c.requests))
				projectRoot := c.projectRoot()
				stale := c.work
				prompt := c.requestAt(1)
				// The prompt's own "Working directory" line: the project's, never the record's.
				c.check("prompt 说的是项目主目录", strings.Contains(prompt, projectRoot),
					fmt.Sprintf("prompt 里没有 %s", projectRoot))
				c.check("prompt 里不是会话记录里的旧快照", !strings.Contains(prompt, stale),
					"prompt 里出现了 "+stale)
				c.check("project 的附加目录进了 prompt", strings.Contains(prompt, filepath.Join(c.dir, "proj-shared")),
					fmt.Sprintf("prompt 里没有 %s", filepath.Join(c.dir, "proj-shared")))
				// The filesystem half of the detach: the project is gone, and the member kept its
				// workspace — by design that is the project's primary, i.e. the user does not move.
				c.check("projects.json 里已经没有这个项目", !c.projectsFileHas("smoke-proj"),
					"projects.json 仍然含有该项目")
				bound := c.sessionByTitle("冒烟会话")
				if bound == nil {
					c.check("会话 fixture 还在", false, "找不到「冒烟会话」")
					return
				}
				c.check("detach 清掉了 project_id", bound.ProjectID == "", "project_id="+bound.ProjectID)
				c.check("detach 把快照刷成了项目主目录", bound.WorkingDir == projectRoot,
					fmt.Sprintf("WorkingDir=%s want=%s", bound.WorkingDir, projectRoot))
			},
		},
		//
		//
		// switch-load: the FIRST visit to a session, and following a transcript that grows without a
		// message update. Every other switch driver re-selects a session the process already loaded
		// (the in-memory copy), so the load-on-first-visit path — the 加载会话… placeholder, which
		// arrives AFTER the switch's own pin — is the one path they all miss. The seeded transcript
		// ends in a LONG mermaid diagram, whose height arrives asynchronously on top.
		{
			name: "switch-load",
			files: map[string]string{
				"README.md": "# smoke\n\nswitch-load scenario's working directory\n",
			},
			seedSecond: []session.Message{
				{Type: session.MessageTypeUser, Content: "先讲一段"},
				{Type: session.MessageTypeAssistant, Content: "好。\n\n" + strings.Repeat("这一段只是把转写撑长一点。", 40)},
				{Type: session.MessageTypeUser, Content: "再补一段长的"},
				{Type: session.MessageTypeAssistant, Content: "补完了。\n\n" + strings.Repeat("最后这一段同样只是填充。", 20) +
					"\n\n这里是那张长图：\n\n```mermaid\n" + tallMermaid() + "\n```"},
			},
		},
		//
		//
		// paste-image: a screenshot pasted into the box must reach the model as an IMAGE. The driver
		// dispatches a real paste event carrying PNG bytes (a screenshot on the clipboard is bytes
		// with no path — which is why the composer stores it through a binding instead of inserting a
		// path), and the Go side asserts those very bytes arrived at the LLM boundary: that is the
		// only place that proves the whole route (@-reference → @-file expansion → multi-modal part)
		// rather than its first step. It also pins WHERE the file goes: inside the session directory,
		// never the workspace, where the user's own diff would show it.
		{
			name: "paste-image",
			files: map[string]string{
				"README.md": "# smoke\n\npaste-image scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("图看到了。", 800)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				_, seen := c.rawSeen("data:image/png;base64")
				c.check("请求里带了图片（多模态 part 到了线路上）", seen, "请求的原始 body 里没有 data:image/png;base64")
				_, exact := c.rawSeen(pasteImageB64[:48])
				c.check("模型收到的正是用户粘贴的那张图（base64 原样到达）", exact,
					"请求的原始 body 里没有粘贴图片的 base64 片段")
				// "pasted" is desktop/fileservice.go's pastedDirName — the desktop module is not a
				// dependency of this one, so the name is spelled here and asserted to exist.
				pasted, _ := filepath.Glob(filepath.Join(c.home, ".tachi", "session", "*", "pasted", "*.png"))
				c.check("粘贴的图存在会话目录的 pasted/ 下", len(pasted) == 1, fmt.Sprintf("%v", pasted))
				inWork, _ := filepath.Glob(filepath.Join(c.work, "*", "pasted"))
				c.check("粘贴的图没有落在用户工作区里", len(inWork) == 0, fmt.Sprintf("%v", inWork))
			},
		},
		//
		//
		// zoom-fit: the lightbox must never MAGNIFY. fit() scales a diagram into the window, and
		// doing that in both directions opened the same gesture at wildly different sizes depending
		// on a thing the reader cannot see — the diagram's own natural size ("sometimes it opens
		// huge"). Three sizes, so the rule is visible from both sides: one that fits with room to
		// spare (must open at 1:1), one wider than the window (shrunk), one taller (shrunk).
		{
			name: "zoom-fit",
			files: map[string]string{
				"README.md": "# smoke\n\nzoom-fit scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("三张图：\n\n极小：\n\n```mermaid\ngraph LR\n  A[甲] --> B[乙]\n```\n\n中等：\n\n```mermaid\ngraph LR\n  A[读取配置] --> B[校验]\n  B --> C[建索引]\n  C --> D[跑任务]\n  D --> E[写报告]\n  E --> F[通知]\n```\n\n很高：\n\n```mermaid\nflowchart TD\n  N0[节点 0] --> N1[节点 1]\n  N1[节点 1] --> N2[节点 2]\n  N2[节点 2] --> N3[节点 3]\n  N3[节点 3] --> N4[节点 4]\n  N4[节点 4] --> N5[节点 5]\n  N5[节点 5] --> N6[节点 6]\n  N6[节点 6] --> N7[节点 7]\n  N7[节点 7] --> N8[节点 8]\n  N8[节点 8] --> N9[节点 9]\n  N9[节点 9] --> N10[节点 10]\n  N10[节点 10] --> N11[节点 11]\n  N11[节点 11] --> N12[节点 12]\n```\n\n三张都在这里。", 900)},
			},
		},
		//
		//
		// sidebar-text: the sidebar's own type scale. Its group headers were 14px against 13.5px
		// rows — a hierarchy that held on paper and was invisible on screen ("项目字体太小了"), with a
		// scatter of 8–11px fragments around it (the disclosure caret among them, which rendered as a
		// dot). Pinned as INTENT, not as numbers: nothing in the sidebar under 12px, a group header
		// strictly larger than a session row, and every disclosure caret at the one shared size
		// (--caret-size) — retuning the design later is not a test failure.
		{
			name: "sidebar-text",
			files: map[string]string{
				"README.md": "# smoke\n\nsidebar-text scenario's working directory\n",
			},
			project: &projectSeed{
				name:      "sidebar-proj",
				files:     map[string]string{"README.md": "# smoke\n\nthe project's own directory\n"},
				emptyName: "sidebar-empty",
			},
		},
		//
		//
		// form-text: the 新建项目 / 编辑项目 dialog's type scale. Its labels and paths used to sit at
		// 10.5–11px — the compact roots popover's sizes, which the form inherits because it renders
		// the same rows — three steps below the 14px title and 13.5px field between them, so the
		// dialog read as a shrunken one. Its own width was also silently clamped: `.confirm-box`
		// caps at 380px, which beat the form's 520px. Pinned as a FLOOR (nothing under 12px) rather
		// than per-element sizes, so tuning the design later is not a test failure.
		{
			name: "form-text",
			files: map[string]string{
				"README.md": "# smoke\n\nform-text scenario's working directory\n",
			},
		},
	}
}

// pasteImageB64 is the 1x1 PNG the paste-image driver pastes, as the webview sends it (a data URL's
// payload). It is duplicated in drivers/paste-image.js — a driver cannot read a Go constant — so the
// two must stay identical: the Go side asserts THESE bytes reached the model, which is what makes
// the test about the paste rather than about "some image".
const pasteImageB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="

// tallMermaid is a flowchart tall enough to matter: a vertical chain of nodes, each about a
// line high, so the rendered figure is several hundred pixels tall.
func tallMermaid() string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	for i := 1; i < 20; i++ {
		fmt.Fprintf(&b, "  N%d[节点 %d] --> N%d[节点 %d]\n", i-1, i-1, i, i)
	}
	return b.String()
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
//
// pause > 0 holds the reply at a KNOWN point (this file not yet written), which is how a
// scenario tests a driver that acts on the turn's changes against a file set that is still
// growing — see oneoff-footer, where the second write parks the turn on purpose.
func writeFileStream(pause time.Duration, path, content, callID string) mockllm.ReplyFunc {
	chunks := []mockllm.Chunk{}
	if pause > 0 {
		chunks = append(chunks, mockllm.Pause(pause))
	}
	args := jsonArgs(map[string]string{"path": path, "content": content})
	chunks = append(chunks,
		mockllm.ToolCallStart(callID, "WriteFile", args),
		mockllm.Finish("tool_calls"),
		mockllm.UsageWithCache(1500, 40, 1400, 10),
		mockllm.Done(),
	)
	return mockllm.Stream(chunks...)
}

// reportWriteStep is one review round that saves its report. The path is only in the REQUEST —
// the orchestrator writes it into the prompt verbatim (commands.BuildReviewPrompt) — so the step
// reads it out of the messages it is handed: `Require` is the hook that sees the request, it runs
// immediately before the reply in the same handler, and the reply is BUILT there, so the WriteFile
// carries the path the prompt named instead of a guess.
func reportWriteStep(pause time.Duration, content, callID string) mockllm.Step {
	var path string
	return mockllm.Step{
		Require: func(req *mockllm.RecordedRequest) string {
			path = reportPathIn(req)
			if path == "" {
				return "评审 prompt 里没有报告路径：模型被告知往哪写，是这条断言的先决条件"
			}
			return ""
		},
		Reply: func(ctx context.Context, w http.ResponseWriter, p mockllm.Protocol) {
			chunks := []mockllm.Chunk{}
			if pause > 0 {
				chunks = append(chunks, mockllm.Pause(pause))
			}
			chunks = append(chunks,
				mockllm.ToolCallStart(callID, "WriteFile", jsonArgs(map[string]string{"path": path, "content": content})),
				mockllm.Finish("tool_calls"),
				mockllm.UsageWithCache(1500, 40, 1400, 10),
				mockllm.Done(),
			)
			mockllm.Stream(chunks...)(ctx, w, p)
		},
	}
}

// reportPathRe finds the report path in whichever template the round was built from. Both name
// the same orchestrator-owned path, and only in those two sentences:
//
//   - single round (ReviewUserPrompt): "Write the complete report to this exact path: <path>."
//   - multi round  (BuildReviewPrompt): "…保存报告到：<path>（编排器给出的确切路径…）"
//
// Newest message first: the prompt is the last user message, and an earlier round's path (in a
// multi-round chain's "previous reports" section) must not be mistaken for this one's.
var reportPathRe = regexp.MustCompile(`(?:Write the complete report to this exact path: |保存报告到：)([^\s（]+\.md)`)

func reportPathIn(req *mockllm.RecordedRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		if m := reportPathRe.FindStringSubmatch(req.Messages[i].Content); m != nil {
			return m[1]
		}
	}
	return ""
}

// sendFileStream is one SendFile call — what puts an attachment card on screen, and with it the
// card's ⤢ that opens a document viewer (the surface file-find searches).
func sendFileStream(path, callID string) mockllm.ReplyFunc {
	args := jsonArgs(map[string]string{"path": path})
	return mockllm.Stream(
		mockllm.ToolCallStart(callID, "SendFile", args),
		mockllm.Finish("tool_calls"),
		mockllm.UsageWithCache(900, 30, 860, 10),
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

// sessionFiles lists the sandbox's session transcripts (every session's messages.jsonl). The
// Go half reads these to assert what was WRITTEN, which the driver — which only sees the UI —
// cannot: the driver proves what a reloaded bubble shows, these prove why.
func sessionFiles(home string) []string {
	files, _ := filepath.Glob(filepath.Join(home, ".tachi", "session", "*", "messages.jsonl"))
	return files
}

func fileList(paths []string) string {
	return strings.Join(paths, ", ")
}

// statErrText describes a stat failure for a report line ("" when the file is gone,
// which is what the rewind case expects).
func statErrText(err error) string {
	if err == nil {
		return "文件仍存在"
	}
	return err.Error()
}
