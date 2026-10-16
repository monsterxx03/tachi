package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/commands"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/mcpauth"
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
// smokeSourceNodeID is the caller identity the managed-session fixtures record: a fixed value, so
// the row's wording ("来自未知节点 <8 位>") is assertable.
const smokeSourceNodeID = "cd726b446c34386ece70d88fdd5fbf2a"

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
	// pricing is the mock provider's own price table, for the scenarios that make an assertion
	// about MONEY. The shared provider entry has no prices (an unpriced model bills nothing,
	// which keeps every other scenario's ¥/积分 figures at zero), so a scenario that needs a
	// real number supplies one here — and it cannot come in through `config`, because
	// `providers:` is already a key in the sandbox's own YAML and a second one would not load.
	pricing *config.PricingConfig
	// extraRoots are ADDITIONAL workspace roots for the session, each entry a directory the
	// runner creates under the sandbox holding that entry's files. They are seeded rather than
	// added in the UI because the UI's only way in is a native directory picker, which a driver
	// cannot click — and a session's root set is what the multi-root behaviour is about.
	//
	// From the work dir a scenario reaches one as "../<name>/…": a scripted bash command cannot
	// know the sandbox's absolute path, and it does not have to.
	extraRoots map[string]map[string]string
	// mcpOAuth points the sandbox's mcp.json at an OAuth-protected MCP server the RUNNER
	// starts (itest/mcpauth): the fixture binds a random loopback port, so the file is
	// written per run rather than checked in. The app is also told to replace its browser
	// with a headless fetch (TACHI_DEMO_BROWSER_FETCH, see desktop/demo.go), because a smoke
	// run must never open a window on the machine it is running on — and an authorization's
	// first act is opening one.
	//
	// The scenario's `after` reads the fixture (`c.mcp`) for what the client actually did:
	// registered for a client_id, asked for PKCE, exchanged the code for a token.
	mcpOAuth bool
	// mcpServersJSON is the sandbox's mcp.json written VERBATIM, for the scenarios that drive the
	// settings page's MCP server editor. Empty means the sandbox has no mcp.json at all — which is
	// a different scenario (no shared manager, so nothing to install into), not the default shape.
	//
	// Every server in it must be DISABLED (`"enabled": false`): a smoke run happens on a real
	// machine, and an enabled stdio server is a process the app really spawns.
	mcpServersJSON string
	// peers are the other Tachi nodes this machine may delegate to, written into the sandbox's
	// config.yaml. Structured rather than a YAML string in `config` because an ANSWERING peer's URL
	// only exists once the runner's fixture is up (see peerSeed), and because both halves of what the
	// Agents panel has to say — a card to draw, and a peer that never answered — belong to one list.
	peers []peerSeed
	// voiceText stands in for the microphone: the app is told to answer a finished recording
	// with this text instead of opening the mic and running the recognizer (TACHI_DEMO_VOICE_TEXT,
	// read in desktop/voice.go). It exists for the same reason browserFetch does — a run must not
	// touch the machine it is running on, and a real recording would raise a TCC prompt and capture
	// whatever is happening in the room. What the scenario asserts is the button's half of the
	// feature (start/stop, and where the text lands), which does not depend on where audio comes from.
	voiceText string
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
	// uiState seeds desktop_ui.json before the app launches (see sb.seedUIState). It exists for the one
	// shape a driver cannot produce: a RESTART. A driver can toggle a column and expand a directory, but
	// nothing it does makes the app read those preferences off disk — and "the tree came back the way I
	// left it, from the file" is a claim about exactly that read.
	uiState *uiStateSeed
	// orphanChain seeds a conversation that WAS compacted whose pre-compaction half is no longer in
	// the list — the state a deleted (or max_keep-swept) parent leaves behind, and the one shape a
	// driver cannot produce: a driver cannot write a session record, and deleting one is the other
	// half of the same fixture (see sandbox.seedOrphanChain). The row must still say the
	// conversation was compacted; it is the case that reads as "the chain is gone".
	orphanChain bool
	// seedLoose seeds this many project-less sessions (titled 批量会话 01…n, oldest first), for the
	// sidebar's ROW WINDOW: a window is only observable with a group longer than it renders, and no
	// driver can produce that one ⌘N at a time. They land in the 无项目 bucket, where the rest of a
	// fixture's project-less sessions already are.
	seedLoose int
	// sessionKind puts the seeded session under a SUBSYSTEM's ownership (session.Session.Kind;
	// today session.KindA2AExecution is the only kind) and makes it the session the app restores
	// on startup. Seeded rather than produced by a driver for the obvious reason — a driver cannot
	// write a session record — and the managed state has to be the FIRST render's, which is exactly
	// what a restart finds: a window opened onto a receiving node's execution record.
	sessionKind string
	// sessionSourceNodeID is the identity recorded as the one that OPENED the seeded session
	// (session.Session.SourceNodeID). Together with sessionKind it is the whole provenance of a
	// subsystem's conversation: who it came from, and that it is not the reader's.
	//
	// The sandbox's home has no peer-identities.json, so this id resolves to NOTHING — which is the
	// state the driver asserts, and the normal one on a machine that has never reached that peer
	// outbound. A resolved name comes from that file (node.PeerAliases, pinned by outbound contact)
	// and is covered where the matching lives, in the node's own tests.
	sessionSourceNodeID string
	// sessionHeldByTask takes a RUNNING execution's hold on the seeded session (a retention
	// record, session.ReasonTask), which is the state that refuses a delete and offers the owner
	// the escape hatch. Seeded the same way and for the same reason as sessionKind: it is a
	// persisted record, and what the window does with it has to be visible on the first render.
	sessionHeldByTask bool
	// after runs once the driver has reported: the Go-side assertions.
	after func(c *checkCtx)
}

// uiStateSeed is the desktop_ui.json fixture (seeded by sandbox.seedUIState) — the durable half of the
// desktop's layout preferences, which App reads once at mount.
type uiStateSeed struct {
	fileTreeOpen  bool
	fileTreeWidth int
	// treeDirs are directories under the work dir that were expanded when the app was last closed, and
	// treeFile is the file that was being read. Both go into the workspace memo under the key the
	// BACKEND would derive for this fixture (the work dir is the session's primary root, so "d:"+path —
	// see sandbox.fileTreeKeyForWork for why the spelling is repeated there).
	treeDirs []string
	treeFile string
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
	// mcp is the scenario's OAuth fixture, when it declared mcpOAuth: what the MCP client
	// actually did against it (registrations, authorization requests, token exchanges).
	mcp *mcpauth.Server
	// mcpConfigBefore is the sandbox's mcp.json as it was seeded, for asserting that a
	// panel toggle edited it by exactly one line.
	mcpConfigBefore string
	// peerCardRequests is how many Agent Card reads the scenario's peer fixtures saw (0 when the
	// scenario declared no answering peer). 「面板画的是缓存」 and 「面板问了那个节点」 look the same
	// on screen, so the count is the only place that difference shows.
	peerCardRequests int
	// opened is what the app asked the SYSTEM to open (one entry per launcher call, the argv
	// space-joined): a link clicked in the transcript, a 打开 button. Read from the sandbox's own
	// artifact, where TACHI_DEMO_OPEN_LOG points the app (desktop/demo.go) — a run must never really
	// launch a browser or Finder, so this is the only evidence the click reached the launcher.
	opened []string
}

func (c *checkCtx) check(label string, ok bool, detail string) {
	*c.lines = append(*c.lines, Line{Label: "go: " + label, OK: ok, Detail: detail})
}

// requestHasText reports whether ONE request's messages contain want. The scenario-level
// requestSeen asks "at any point"; tests about what a LATER turn was told need this one, since the
// earlier requests legitimately contain the text too.
func requestHasText(r *mockllm.RecordedRequest, want string) bool {
	for _, m := range r.Messages {
		if strings.Contains(m.Content, want) {
			return true
		}
	}
	return false
}

// lastUserText is the LAST user message of one request. "这一段和那一段在同一条消息里" has to be asked
// of this one: requestHasText only asks "did it appear somewhere", and the earlier turns' messages
// are in the same request — a batch sent as three separate messages looks exactly like one message
// to it.
func lastUserText(r *mockllm.RecordedRequest) string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "user" {
			return r.Messages[i].Content
		}
	}
	return ""
}

// requestTail is the last few messages of a request, for a failure's detail: "what did the model
// actually see" is the question both of these helpers exist for.
func requestTail(r *mockllm.RecordedRequest) string {
	var parts []string
	for _, m := range r.Messages {
		parts = append(parts, fmt.Sprintf("%s:%s", m.Role, strutil.Truncate(m.Content, 40)))
	}
	if len(parts) > 4 {
		parts = parts[len(parts)-4:]
	}
	return strings.Join(parts, " | ")
}

// orphanCallID is the tool call the orphan-tool-call fixture leaves UNANSWERED, and
// orphanAnsweredCallID is the one its result answers. Named rather than inlined because both the
// fixture and the assertion spell them, and a typo in either would make the assertion vacuous.
const (
	orphanCallID         = "call_orphan"
	orphanAnsweredCallID = "call_answered"
)

// requestHasToolCall reports whether ONE request advertises a tool call with that id — the question
// a rebuilt history has to answer about an unpaired call. It reads the normalized view, where a
// call's id survives (mockllm.ToolCall).
func requestHasToolCall(r *mockllm.RecordedRequest, id string) bool {
	for _, m := range r.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID == id {
				return true
			}
		}
	}
	return false
}

// requestHasToolResult reports whether ONE request carries the tool message answering that call.
func requestHasToolResult(r *mockllm.RecordedRequest, id string) bool {
	for _, m := range r.Messages {
		if m.Role == "tool" && m.ToolCallID == id {
			return true
		}
	}
	return false
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
// exactly why the assertion has to reach for the bytes (paste-image and image-reload both do).
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

	// selection-ask 的被选中文字与问题。driver 按这些字找那一段、找队列里的那一行，Go 这一半按
	// 它们断言模型收到的正文 —— 两边拼的是同一串字，所以它们只写一次。ChatLine1/2 是场景里 agent
	// 的一句回答（两段），会话全文那一半选中的就是它们；DefaultAsk 是那句默认问题，两半都拿它数
	// 「写了几次」。
	selectionAskLineA      = "这段文字是拿来被选中提问的。"
	selectionAskLineB      = "第二段也有它自己的问题。"
	selectionAskLineC      = "第三段用自己写的问题。"
	selectionAskLineD      = "第四段入队之后会被删掉。"
	selectionAskAskC       = "这一段是干什么的？"
	selectionAskAskC2      = "这一段到底是干什么的（改过）？"
	selectionAskChatLine1  = "就这一段的回答。"
	selectionAskChatLine2  = "旁边这句也能选。"
	selectionAskChatAsk    = "这句是什么意思？"
	selectionAskDefaultAsk = "解释一下？"
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

// mdWidgetsReply 是 md-widgets 那一场的回答：一段带语言的代码块（其中一行故意很长）、一张内联的
// SVG（data URI，不碰磁盘，必成）、一张指向不存在路径的图（要落到"图片无法加载"那一态），以及一块
// **画不出来的 mermaid**（标签里有个没引号的方括号，解析这一步就过不去）——正文里三种"控件"各自的
// 正常态与失败态都在这里。
//
// 长行是折行开关的判据：不折行时它必须横向溢出，开了折行之后不能 —— 那是读者的观感，`white-space`
// 只是手段。
func mdWidgetsReply() string {
	long := strings.Repeat("abcdefghij", 40)
	return "看这段代码：\n\n```go\nfunc main() { fmt.Println(\"" + long + "\") }\n```\n\n" +
		"再看两张图：\n\n" +
		"![dot](data:image/svg+xml;base64," + mdWidgetsDot + ")\n\n" +
		"![missing](nope/missing.png)\n\n" +
		"再画一张图（这张是坏的）：\n\n" +
		"```mermaid\nflowchart LR\n  A[到点] --> B[跑一轮]\n  B --> C[判定<br/>[SILENT] 保持]\n```\n\n" +
		"最后是一个没有空格的长串：`" + long + "`\n"
}

// mdWidgetsDot 是上面那张内联图：48x24 的实心小方块。写成 data URI 是有意的 —— 要钉的是"正文里的
// 图片能开浮层"，不是 /local 那条路由（那是附件那条路的事）。
const mdWidgetsDot = "PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSI0OCIgaGVpZ2h0PSIyNCI+PHJlY3Qgd2lkdGg9IjQ4IiBoZWlnaHQ9IjI0IiBmaWxsPSIjNDQ2MGUwIi8+PC9zdmc+"

// askStream scripts the agent asking the user something: one AskUserQuestion call carrying a
// question with two options and a free-text box. The turn parks on the answer, which is what
// makes the form's own keystrokes observable at all.
func askStream(id, question string) mockllm.ReplyFunc {
	return mockllm.Stream(
		mockllm.ToolCallStart(id, "AskUserQuestion",
			`{"questions":[{"question":"`+question+`","header":"确认","options":[{"label":"方案 A"},{"label":"方案 B"}]}]}`),
		mockllm.Finish("tool_calls"),
		mockllm.Usage(1500, 40),
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

// searchStream makes the run ask for an MCP tool. The call never reaches the tool: what the
// scenario is about is the agent's own gate in front of it (see perm-mcp-budget).
//
// promptTokens is what the provider REPORTS for the request, and that report is what the
// context meter — and therefore the cost estimate on the card — is anchored on (see
// llm.PromptTokens / convState.setPromptAnchor). A scenario that wants the card to appear
// therefore has to report a prompt big enough to be worth more than the budget; the small
// literal sizes every other scenario uses would ask for nothing.
func searchStream(id string, promptTokens int) mockllm.ReplyFunc {
	return mockllm.Stream(
		mockllm.ToolCallStart(id, "MCPSearchTools", `{"query":"echo from the fixture"}`),
		mockllm.Finish("tool_calls"),
		mockllm.UsageWithCache(promptTokens, 40, promptTokens/2, 0),
		mockllm.Done(),
	)
}

// ptrFloat is what every config price field needs: nil means "not set" (fall back to the
// built-in table), an explicit 0 means free.
func ptrFloat(v float64) *float64 { return &v }

// diffLayoutLine is one line of the diff-layout scenario's file: a tag the driver recognises,
// then enough filler that HALF of the pane cannot show the line whole. Every line needs it, not
// just one: the columns scroll together, and a short line's cell has nothing to scroll — a
// scrollLeft assigned to it is clamped straight back to 0, which reads exactly like "the mirror is
// broken" when it is only "this row cannot move".
func diffLayoutLine(tag string) string {
	return tag + " · " + strings.Repeat("填充，让半宽的格子装不下这一行。", 10)
}

// chatFindFixture seeds a long conversation for the ⌘F-over-the-transcript scenario. Three tokens,
// one per place a match can come from, and the counts they must produce are the whole test:
//
//	· CHATFIND-USER in the FIRST user message, which the app cannot show on open (the transcript is
//	  pinned to the bottom) — so finding it also proves the jump;
//	· CHATFIND-TOOL in a Bash call's ARGS and RESULT — two places a reader searching their
//	  conversation does NOT mean, and the reason the search is scoped (issue #35);
//	· CHATFIND-TOOL once more in the assistant's PROSE, the one hit that must be found.
//
// The filler turns exist so the transcript really scrolls: without them everything is on screen and
// "直接定位到消息" would be asserted against nothing.
func chatFindFixture() []session.Message {
	base := time.Now().Add(-time.Hour)
	n := 0
	stamp := func(m session.Message) session.Message {
		n++
		m.Timestamp = base.Add(time.Duration(n) * time.Second)
		return m
	}
	out := []session.Message{stamp(session.Message{Type: session.MessageTypeUser,
		Content: "第一条消息：这里有个词 CHATFIND-USER，它只出现在用户输入里。"})}
	out = append(out,
		stamp(session.Message{Type: session.MessageTypeAssistant, Iteration: 1}),
		stamp(session.Message{Type: session.MessageTypeToolCall, Name: "Bash", ToolCallID: "call_cf1",
			Args: map[string]any{"command": "echo CHATFIND-TOOL"}, Iteration: 1}),
		stamp(session.Message{Type: session.MessageTypeToolResult, Name: "Bash", ToolCallID: "call_cf1",
			Result: "CHATFIND-TOOL\n（这是工具结果，不是对话正文）", Iteration: 1}),
		stamp(session.Message{Type: session.MessageTypeAssistant, Iteration: 1,
			Content: "命令跑完了，输入里有 CHATFIND-TOOL 这个词。"}),
	)
	filler := strings.Repeat("这一段只是把转写撑长，好让查找真的需要滚动。", 24)
	for i := range 4 {
		out = append(out,
			stamp(session.Message{Type: session.MessageTypeUser, Content: fmt.Sprintf("第 %d 轮的问题", i+2)}),
			stamp(session.Message{Type: session.MessageTypeAssistant, Content: fmt.Sprintf("第 %d 轮的回答。\n\n%s", i+2, filler)}),
		)
	}
	return out
}

// longTurnPagesFixture seeds a transcript whose FIRST turn is longer than one history page
// (PAGE_SIZE raw records, see desktop/frontend/src/types.ts). The app loads the newest page
// first, so that turn arrives SLICED: its tail comes with the first page, and its head only when
// the reader scrolls to the top and the older page is prepended — the seam #29 is about.
//
// The two later turns are long PROSE on purpose: the transcript has to overflow before scrolling
// to the top can trigger a load at all (a folded turn is one line tall).
func longTurnPagesFixture() []session.Message {
	// Distinct, increasing stamps — the paging cursor is a TIMESTAMP ("older than this"), so
	// records that share one stamp leave LoadSessionMore with nothing older to return and the
	// transcript looks like it IS the whole history.
	base := time.Now().Add(-time.Hour)
	n := 0
	stamp := func(m session.Message) session.Message {
		n++
		m.Timestamp = base.Add(time.Duration(n) * time.Second)
		return m
	}
	out := []session.Message{stamp(session.Message{Type: session.MessageTypeUser, Content: "长回合开始"})}
	for i := range 40 {
		id := fmt.Sprintf("call_%d", i)
		out = append(out,
			stamp(session.Message{Type: session.MessageTypeAssistant, Iteration: i + 1}),
			stamp(session.Message{Type: session.MessageTypeToolCall, Name: "Bash", ToolCallID: id,
				Args: map[string]any{"command": fmt.Sprintf("echo %d", i)}, Iteration: i + 1}),
			stamp(session.Message{Type: session.MessageTypeToolResult, Name: "Bash", ToolCallID: id,
				Result: fmt.Sprintf("第 %d 步的结果", i), Iteration: i + 1}),
		)
	}
	out = append(out, stamp(session.Message{Type: session.MessageTypeAssistant, Content: "长回合结束。"}))
	filler := strings.Repeat("这一段只是把转写撑长一点，好让它真的可以滚动。", 30)
	for _, turn := range []string{"第二轮", "第三轮"} {
		out = append(out,
			stamp(session.Message{Type: session.MessageTypeUser, Content: turn}),
			stamp(session.Message{Type: session.MessageTypeAssistant, Content: turn + "的回复。\n\n" + filler}),
		)
	}
	return out
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
		// 消息区里的富文本控件：代码块的头栏（语言 / 复制 / 折行）与正文图片的放大。三样都是"看得见、
		// 点得动"的东西，单测看不见，所以各自钉一条最容易被改坏的：头栏必须真的在代码之上（不是像
		// 从前那样浮在第一行代码上）、折行开关必须真的改变换行方式（长行从溢出变成不溢出）、图片必须
		// 真的能开浮层。
		//
		// 第二轮是给"消息节奏"和"入场动画"用的：问题与它的回答是一段对话（挨得近），两轮之间是两段
		// 阅读（离得远），而只有最新那条消息会滑入 —— 换会话、翻历史、压缩后重载都是一次 mount 一屏
		// 消息，全都滑一遍就是闪。这两条都只能在"多轮"里量。
		//
		{
			name: "md-widgets",
			files: map[string]string{
				"README.md": "# smoke\n\nthe md-widgets scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream(mdWidgetsReply(), 900)},
				{Reply: textStream("收到。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("两轮共两次调用", len(c.requests) == 2, requestCount(c.requests))
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
		// 用户气泡里的换行：Shift+Enter 敲出的换行在气泡里必须还在。数据从来没错过（模型收到
		// 的就是带 \n 的原文，这里由 Go 侧那条请求断言钉住），丢的是渲染——气泡把消息当纯文本
		// 显示，`white-space: normal` 把每个换行折成了一个空格。
		//
		{
			name: "bubble-lines",
			files: map[string]string{
				"README.md": "# smoke\n\nthe bubble-lines scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("收到，两行都看到了。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				at, seen := c.requestSeen("第一行\n第二行")
				c.check("模型收到的是带换行的原文", seen, fmt.Sprintf("命中第 %d 次请求", at))
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
		// file-tree: 右列的文件树 + 内容列的文件视图（docs/2026-10-13-desktop-file-tree-design.md）。
		//
		// 这个场景覆盖的都是「一条绑定/一次渲染」之外的东西：树是从会话的工作区根长出来的（两个根、
		// 同名根退化成全路径、git 分支），一层是惰性列的（展开一个目录才要一层），忽略的文件照样显示
		// 只是灰掉，文件在**会话那一列**打开而树还在（这正是它不复用旁路面板宽度的原因），会话在文件
		// 视图盖着时保持挂载（草稿还在），以及 agent 写完文件后树自己更新。
		//
		// uiState fixture 是「重启形状」的那一半：展开态和正在看的文件都是从盘上读回来的，不是这次
		// 运行里点出来的；Go 那一半再把运行中被写回的 memo 读一遍。
		{
			name: "file-tree",
			// A repository with a committed tree: the root row carries its branch, and `dist/` is
			// genuinely ignored (so the row's grey mark is exercised by git, not by a fixture flag).
			gitInit: true,
			files: map[string]string{
				"README.md":              "# smoke\n\n顶层说明。\n",
				".gitignore":             "dist/\n*.log\n",
				"main.go":                "package main\n\nfunc main() { println(\"tree\") }\n",
				"pic.png":                smokePNG,
				"blob.bin":               "\x00\x01\x02binary\n",
				"docs/agents/desktop.md": "# 桌面手册\n\n文件视图要渲染的标题。\n",
				// 比阅读区高的一份文档：驱动靠它断言「这个盒子是滚动器」——一份短文件看不出
				// body 忘了 overflow:auto（内容不够高时，坏掉的盒子和好的盒子长得一模一样）。
				"long.md": longMarkdown(60),
				"dist/app.js":            "console.log(1)\n",
				"run.log":                "noise\n",
			},
			// Two roots whose BASE NAMES collide: the label a root is shown under then degrades to its
			// full path (rootLabels), and the frontend cannot derive that itself — it has to be the
			// backend's answer.
			extraRoots: map[string]map[string]string{
				"shared-lib":        {"notes.md": "lib\n"},
				"vendor/shared-lib": {"readme.txt": "vendor\n"},
			},
			uiState: &uiStateSeed{
				fileTreeOpen:  true,
				fileTreeWidth: 360,
				treeDirs:      []string{"docs/agents"},
				treeFile:      "docs/agents/desktop.md",
			},
			steps: []mockllm.Step{
				// The agent writes a NEW file into a directory the driver has expanded: the tree has to
				// grow a row for it on its own (the poll, or the end of the turn).
				{Reply: bashStream("printf 'from the agent\\n' > docs/agent-notes.md", "call_ft1")},
				{Reply: textStream("写好了。", 800)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("一次对话共两次调用", len(c.requests) == 2, requestCount(c.requests))

				// The file the agent wrote really is there (a DOM assertion about a row would otherwise
				// pass on a row that was already on screen).
				written := filepath.Join(c.work, "docs", "agent-notes.md")
				data, err := os.ReadFile(written)
				c.check("agent 真的写了新文件: docs/agent-notes.md", err == nil && strings.Contains(string(data), "from the agent"),
					fmt.Sprintf("err=%v", err))

				// The memo was written BACK: the expansion the driver produced (docs/dist…, whichever it
				// opened) and the file it was last reading are on disk under the key the backend derived
				// for this workspace. This is the "reopen the panel / next launch" half of the feature,
				// and the key is the part a frontend could get wrong on its own.
				var st struct {
					FileTreeOpen  bool `json:"fileTreeOpen"`
					FileTreeWidth int  `json:"fileTreeWidth"`
					FileTreeMemo  map[string]struct {
						Dirs []string `json:"dirs"`
						File string   `json:"file"`
					} `json:"fileTreeMemo"`
				}
				if err := readJSON(filepath.Join(c.home, ".tachi", "desktop_ui.json"), &st); err != nil {
					c.check("运行后的 desktop_ui.json 可读", false, err.Error())
					return
				}
				memo, ok := st.FileTreeMemo[fileTreeKeyForWork(c.work)]
				c.check("memo 落在后端推导的那个键上", ok, fmt.Sprintf("%v", keysOfMemo(st.FileTreeMemo)))
				if !ok {
					return
				}
				// 展开态里有运行中打开过的目录（docs 或它下面的 agents，取决于驱动点了哪一层）。
				expandedTarget := slices.Contains(memo.Dirs, filepath.Join(c.work, "docs", "agents")) ||
					slices.Contains(memo.Dirs, filepath.Join(c.work, "docs"))
				c.check("展开态被写回（docs/agents 在内）", expandedTarget, strings.Join(memo.Dirs, " | "))
				c.check("正在看的文件被写回", strings.HasPrefix(memo.File, c.work+string(filepath.Separator)), memo.File)
				c.check("宽度偏好仍是从 fixture 来的那个", st.FileTreeWidth == 360, fmt.Sprint(st.FileTreeWidth))
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
				// Turn 4, sent after the second rewind: a turn that writes a file through
				// BASH, so the conversation-only rewind that follows has a REAL change to
				// leave alone. "Nothing was touched" is only evidence when there was
				// something that would have been.
				{Reply: bashStream("echo conv-only > convonly.txt", "call_c")},
				{Reply: textStream("又写好了。", 800)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("四轮共六次调用", len(c.requests) == 6, requestCount(c.requests))

				// The shell command's changes are the ones a rewind has to undo. These run
				// AFTER the read-only turn's own rewind too, so they also say that a turn with
				// nothing to restore leaves the workspace exactly as it found it.
				_, statErr := os.Stat(filepath.Join(c.work, "made.txt"))
				c.check("回退删掉了 shell 新建的文件", os.IsNotExist(statErr), statErrText(statErr))
				content, readErr := os.ReadFile(filepath.Join(c.work, "keep.txt"))
				c.check("回退还原了 shell 改过的文件", readErr == nil && string(content) == "original\n", string(content))

				// …and the conversation-only rewind's file must be THE OPPOSITE: the last
				// turn wrote it through bash, and a rewind that kept the conversation only
				// has to leave it exactly where it was. The UI can only prove the card said
				// so; this is the half that proves the workspace really did not move.
				convonly, convErr := os.ReadFile(filepath.Join(c.work, "convonly.txt"))
				c.check("只回退对话没有动工作区（那一轮写的文件仍在）",
					convErr == nil && string(convonly) == "conv-only\n",
					fmt.Sprintf("err=%v content=%q", convErr, convonly))
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
		//
		// at-session: `#s:` 引用另一个会话。和 at-file-reload 是同一件事的两半——那个 pin 的
		// 是「气泡显示用户打的原文」，这个 pin 的是「另一个会话只以指针进上下文」：
		//
		//   驱动侧（drivers/at-session.js）：`#` 先给出两个命名空间候选、`#s:` 打开会话命名空间、
		//   Tab 插进来的是完整 id 的引用；
		//   Go 侧：模型收到的是那一行指针（id/标题/条数/transcript 路径），**不是**那个会话的正文。
		//   内联另一段对话会让本会话的上下文为一份没人要读的转写买单，所以最后一项查的是「没有正文」。
		//
		{
			name: "at-session",
			seedSecond: []session.Message{
				{Type: session.MessageTypeUser, Content: "另一个会话里问过的事"},
				{Type: session.MessageTypeAssistant, Content: "另一个会话里给出的答案。"},
			},
			steps: []mockllm.Step{
				{Reply: textStream("读过那个会话的转写了。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				_, pointer := c.requestSeen("[#s:")
				_, titled := c.requestSeen(`"另一个会话"`)
				c.check("模型拿到的是 #s: 指针而不是转写正文", pointer && titled, requestCount(c.requests))
				_, stored := c.requestSeen(".tachi/session/")
				_, transcript := c.requestSeen("messages.jsonl")
				c.check("指针带着会话存储里的 transcript 路径", stored && transcript, "")
				_, counted := c.requestSeen("— 2 messages")
				c.check("指针说得出那个会话有多少条记录", counted, "")
				_, inlined := c.requestSeen("另一个会话里给出的答案")
				c.check("没有把另一个会话的内容内联进消息", !inlined, "")
			},
		},
		//
		// at-agent: `#a:` 选一个节点。沙箱没配任何 peer，所以这里钉的是**路由与措辞**（drivers/
		// at-agent.js）：一个 '#' 给出两个命名空间候选、Tab 把它展开成 `#a:` 并把弹窗留在那个命名
		// 空间、空的节点列表说的是「没有可点名的节点」而不是退回去搜文件、不是引用的 '#' 不开弹窗。
		// 「行里显示什么」（本机怎么称呼它、pin 住的身份、上次探测的结果）由 Go 侧断，三个前端同一句话。
		//
		{
			name: "at-agent",
			after: func(c *checkCtx) {
				c.check("只看看选什么，不该产生任何模型请求", len(c.requests) == 0, requestCount(c.requests))
			},
		},
		//
		// agents-panel: 状态栏的 Agents 按钮与节点面板（drivers/agents-panel.js）。沙箱配了**两个**
		// peer，各代表面板要说的一半：
		//
		//   - 连不上的一个（127.0.0.1:1，立刻被拒，不是等超时）：行里三件事实各就各位 —— 本机配的
		//     别名、本机 pin 的身份（从没连过 → 「身份未记录」）、上次探测的结论；展开后说的是「它没有
		//     回答」，而不是编一个名字。刷新是一次真往返。
		//   - **会答话**的一个（runner 起 fixture 服务它的 Agent Card，见 peerfixture.go）：另一半只有
		//     答话才看得到 —— 自称、能力、入口，以及技能那张 LIST（三条，所以「第二行长什么样」也在
		//     断言里）。少了它，卡片上任何字段的排版都没人看：这正是一个每条都重复「技能」两个字的
		//     列表能留在树里的原因。
		//
		// 两个 peer 一份 `peers:`（peerSeed）—— 同一个 YAML 里第二个 `peers:` key 会顶掉第一个。
		{
			name: "agents-panel",
			peers: []peerSeed{
				{name: "smoke-nas", url: "https://127.0.0.1:1", note: "沙箱里没有这台机器"},
				{name: "smoke-peer", answers: true, note: "沙箱里的假节点"},
			},
			after: func(c *checkCtx) {
				c.check("只看面板，不该产生任何模型请求", len(c.requests) == 0, requestCount(c.requests))
				// 屏幕上那张卡片只能来自一次真的读取：面板画的是缓存还是刚问来的，DOM 上看不出来。
				c.check("面板向会答话的节点要过 Agent Card", c.peerCardRequests > 0,
					strconv.Itoa(c.peerCardRequests)+" 次卡片读取")
			},
		},
		// sidebar-window: 侧栏每个组只渲染「窗口」那么多行（App.tsx 的 rowWindow + components.tsx
		// 的 MoreRows），滚到尾部或点行尾再续。四件界面上看不出来的事：
		// (1) 截断只发生在长的那个组上，且画出来的行 + 行尾「还有 N 条」= 桶里真实的条数；
		// (2) 续窗口一次只续一段（滚到尾部会自己续，点也行），不是一次画完；
		// (3) **最关键**：⌘K 选中窗口够不到的旧会话时，窗口必须为它撑开、那行必须滚进视野 ——
		//     「把当前会话滚进视野」撞上一个还没渲染的行，就是找不到行、请求悄悄卡住；
		// (4) 整个过程不碰模型，也不增删任何会话（窗口是渲染层的事）。
		{
			name: "sidebar-window",
			files: map[string]string{
				"README.md": "# smoke\n\nthe sidebar-window scenario's working directory\n",
			},
			project: &projectSeed{
				name: "smoke-window",
				files: map[string]string{
					"README.md": "# smoke\n\nthe sidebar-window scenario's project directory\n",
				},
			},
			// 100 个无项目会话，titled 批量会话 01…100：桶因此有 101 条（加 fixture 自己那个
			// 无项目会话），而项目组只有 1 条 —— 窗口是每组各自的，短的那个不该被牵连。
			seedLoose: 100,
			after: func(c *checkCtx) {
				c.check("窗口化没有向模型发过任何请求", len(c.requests) == 0, requestCount(c.requests))
				c.check("窗口化没有建过也没有删过会话（磁盘上仍是 102 个）", len(c.sessionMetas()) == 102,
					fmt.Sprintf("%d 个", len(c.sessionMetas())))
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

				// 置顶：driver 在界面上验的是行序（老会话升到「无项目」组第一行、取消后回到
				// 第二行），这里验它落在了 meta.json 上 —— 界面上的顺序活不过重启，磁盘上的
				// pinned 才是下次启动时行序的来源。只数「恰好一个」，不硬编码是哪一条：置顶的
				// 那行标题是模型生成的。
				pinned := 0
				for _, m := range c.sessionMetas() {
					if p, _ := m["pinned"].(bool); p {
						pinned++
					}
				}
				c.check("置顶写进了 meta.json（恰好一个会话）", pinned == 1, fmt.Sprintf("%d 个", pinned))
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
		// composer-page-turn: ⌃U / ⌃D 在**输入框里**也要翻半页。手就在输入框里的时候，才是想回看上文最
		// 多的那一刻，而消息区那份处理器给不了这件事 —— 它按设计不拦落在文本框上的键，而且从框里按下的
		// 键根本不经过它。所以这对键由 composer 自己认，再交给同一个 scrollByHalfPage（设备的手势走的
		// 也是那一个）。
		//
		// 判据是转写的 scrollTop，所以种子转写必须**真的够长**（撑不住就没有"往上翻"这回事），而且开屏
		// 钉在底部、基线 > 0 —— 否则"翻上去了"是恒真的。driver 里还有一条反向断言：G / gg 是普通字母，
		// 必须在输入框里保持普通（放开这一层就等于把打字吃掉）。
		//
		// 合成事件能证明的到此为止：WebKit 在这两个组合上的光标动作有没有被 preventDefault 挡掉，
		// 是 driver 看不见的（不受信任的事件本来就不触发默认动作）。
		{
			name: "composer-page-turn",
			files: map[string]string{
				"README.md": "# smoke\n\nthe composer-page-turn scenario's working directory\n",
			},
			seedMessages: []session.Message{
				{Type: session.MessageTypeUser, Content: "先把转写撑长一点"},
				{Type: session.MessageTypeAssistant,
					Content: "好。\n\n" + strings.Repeat("这一段只是把转写撑长，好让翻半页真的翻得动。", 40)},
				{Type: session.MessageTypeUser, Content: "再来一段"},
				{Type: session.MessageTypeAssistant,
					Content: "补上了。\n\n" + strings.Repeat("再多几行，好让上面翻得动、下面也翻得回来。", 40)},
				{Type: session.MessageTypeUser, Content: "最后一段"},
				{Type: session.MessageTypeAssistant,
					Content: "完了。\n\n" + strings.Repeat("最后一段同样只是填充，够翻就行。", 40)},
			},
		},
		//
		// 桌面自己的语音输入（composer 上的麦克风按钮）。它和 badge 那条路共用**转写**那一半
		// （desktop/stt），不共用投递：设备没有键盘，所以它转出来的字直接成为一条消息；桌面有
		// 键盘，所以这里给的是草稿——填进输入框，由人改完再回车。这一条断的就是后半句：字落到框
		// 里、且**一个请求都没有发出去**（Go 侧在这里，UI 侧在 driver 里）。
		//
		// 麦克风被 TACHI_DEMO_VOICE_TEXT 替掉（见 scenario 的 voiceText 字段）：真开麦会在跑
		// 测试的**这台机器**上弹一次权限窗，并录进房间里正在发生的事。
		{
			name: "voice-input",
			files: map[string]string{
				"README.md": "# smoke\n\nthe voice-input scenario's working directory\n",
			},
			voiceText: "帮我看一下这个报错",
			steps: []mockllm.Step{
				// Never used: 语音转出来的字只进输入框。它存在只是为了让 mock 是一个完整的
				// 脚本，而不是空的。
				{Reply: textStream("不应该被调用。", 300)},
			},
			after: func(c *checkCtx) {
				c.check("语音转出来的字没有被发出去", len(c.requests) == 0, requestCount(c.requests))
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
		// compact-race: /compact on a conversation that is ALREADY over the threshold — the state
		// every real /compact is typed in, and the one where the loop used to compact the compact
		// run's OWN turn. The cost was a second child session: the summarising call's history had
		// been replaced by the loop's own summary (the instruction was gone from it), so the reply
		// was a continuation, and the frontend — which collects that reply as "the summary" —
		// compacted again, into a child whose summary was a fragment of conversation.
		{
			name: "compact-race",
			files: map[string]string{
				"README.md": "# smoke\n\nthe compact-race scenario's working directory\n",
			},
			// The threshold is set where the SECOND turn's billed prompt lands the estimate: under
			// it while turns 1-2 run, over it at the top of the /compact turn. That turn's estimate
			// is the previous call's real prompt scaled by how much the estimate has grown since
			// that call — so the long reply below is what carries it across, exactly as a grown
			// conversation does.
			config: "compact:\n  threshold: 0.2\n",
			steps: []mockllm.Step{
				{Reply: textStreamPrompt("第一轮回复：确认。", 600, 2000)},
				{Reply: textStreamPrompt(strings.Repeat("这是一段很长的历史内容，用来把上下文撑起来。", 1500), 600, 20000)},
				// The /compact turn's own call — the summary the child session is built from.
				{Reply: textStreamPrompt("历史摘要：两轮对话，只看了工作目录。", 600, 22000)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// 两轮对话 + 压缩那一次。四种请求数就是 bug：循环自己压了一次，压缩轮的调用又发了一次。
				c.check("一次 /compact 只发三次请求（两轮对话 + 压缩）",
					len(c.requests) == 3, requestCount(c.requests))

				// ONE child session. The bug left two: the loop compacted the run's own turn (a
				// child nobody asked for) and the frontend then compacted the continuation that
				// replaced the instruction (a grandchild summarising a fragment).
				metas := c.sessionMetas()
				var child map[string]any
				children := 0
				for _, m := range metas {
					if m["compacted_parent_id"] != nil {
						children++
						child = m
					}
				}
				c.check("压缩后只有两个会话（父 + 一个子）", len(metas) == 2, strconv.Itoa(len(metas)))
				c.check("只产出一个子会话", children == 1, fmt.Sprintf("子会话 %d 个", children))

				// …and the child carries the SUMMARY, not the text the model happened to answer
				// next. This is the assertion the scenario exists for: under the bug the child's
				// first message was the model's reply to a request that no longer carried the
				// compact instruction.
				if child == nil {
					return
				}
				id, _ := child["id"].(string)
				raw, err := os.ReadFile(filepath.Join(c.home, ".tachi", "session", id, "messages.jsonl"))
				if err != nil {
					c.check("读得出子会话的记录", false, err.Error())
					return
				}
				first := ""
				for _, line := range strings.Split(string(raw), "\n") {
					if strings.TrimSpace(line) == "" {
						continue
					}
					var m session.Message
					if json.Unmarshal([]byte(line), &m) != nil || m.Type != session.MessageTypeAssistant {
						continue
					}
					first = m.Content
					break
				}
				c.check("子会话的第一条是摘要（不是对话片段）",
					strings.Contains(first, "历史摘要") && strings.Contains(first, "两轮对话"),
					strutil.Truncate(first, 100))
			},
		},
		//
		// auto-compact: the compaction nobody typed. The agent loop swaps the conversation into
		// a child session ON ITS OWN, in the middle of a turn, and the sidebar has to fold the
		// chain the same way /compact does. It is a scenario of its own because the two paths
		// differ exactly where this one is hard: a typed command ends its own turn with the
		// switch already recorded, while auto-compaction moves the session UNDER a running turn
		// and only re-homes it when that turn ends — so the reader is looking at the new session
		// while the sidebar row still belongs to the old one. Nothing else in the suite triggers
		// auto-compaction at all, and the two ways it can fire are a scenario apart: this one
		// waits for a call and a tool result to push the estimate over the threshold (the real
		// shape — a conversation that grew), so the compaction lands between two iterations of a
		// live turn rather than before its first call.
		{
			name: "auto-compact",
			files: map[string]string{
				"README.md": "# smoke\n\nthe auto-compact scenario's working directory\n",
				// The tool output that moves the estimate: the floor of any prompt is a few percent
				// of the mock's 128k window, so a result of this size is what carries the turn over
				// the threshold — after a call has already been made with tools.
				"big.txt": strings.Repeat("auto-compact filler line abcdefghijklmnopqrstuvwxyz\n", 8000),
			},
			config: "compact:\n  threshold: 0.3\ntool_result:\n  max_chars: 400000\n",
			steps: []mockllm.Step{
				// Iteration 1: an ordinary call that asks for a tool, whose output is what pushes
				// the estimate over the threshold — so the compaction happens at the TOP OF THE
				// NEXT ITERATION, i.e. in the middle of a live turn, exactly as it does for a
				// reader whose conversation grew (the first call must therefore carry tools: see
				// the assertion below). Its usage is deliberately large: the reported context size
				// is anchored on what the provider billed, so a mock that always claims a
				// two-thousand-token prompt would keep the meter near the floor however much the
				// tool result adds.
				{Reply: mockllm.Stream(
					mockllm.ToolCallStart("call_big", "Bash", `{"command":"cat big.txt"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(40000, 60, 20000, 20),
					mockllm.Done(),
				)},
				// The summary the child session is built from. Auto-compaction asks for it through
				// the compact strategy's own CreateChat — a NON-streaming call (unlike the typed
				// /compact, which runs an ordinary streamed turn), so this step answers with a
				// plain JSON body.
				{Reply: mockllm.JSON("历史摘要：用户要求看一个文件，已经看过。", "")},
				// …and the reply the turn continues into, in the NEW session.
				{Reply: textStream("压缩之后我还能回答。", 600)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("工具调用、压缩、回复，共三次调用", len(c.requests) == 3, requestCount(c.requests))
				if len(c.requests) > 0 {
					c.check("第一次调用是带工具的普通调用（压缩发生在工具结果之后）",
						len(c.requests[0].Tools) > 0, strconv.Itoa(len(c.requests[0].Tools))+" 个工具")
				}
				_, compacted := c.requestSeen("compress the above conversation")
				c.check("自动压缩的 fork 收到了压缩指令", compacted, "")

				// The disk half: the chain is linked BOTH ways, which is what lets a fresh
				// launch rebuild the folded shape (the frontend only sees compacted_parent_id).
				metas := c.sessionMetas()
				c.check("自动压缩后有两个会话", len(metas) == 2, strconv.Itoa(len(metas)))
				var child, parent map[string]any
				for _, m := range metas {
					if m["compacted_parent_id"] != nil {
						child = m
					} else {
						parent = m
					}
				}
				c.check("子会话记下了它的父会话", child != nil && parent != nil, fmt.Sprintf("metas=%v", metas))
				if child != nil && parent != nil {
					c.check("父子互相指向对方",
						child["compacted_parent_id"] == parent["id"] && parent["compacted_child_id"] == child["id"],
						fmt.Sprintf("child.parent=%v parent.child=%v", child["compacted_parent_id"], parent["compacted_child_id"]))
				}
			},
		},
		//
		// compact-window: the stretch BETWEEN an auto-compaction and the end of the turn that
		// caused it. The conversation has already moved into the child session, while the run and
		// the turn's event stream are still keyed to the parent — so the child must not behave
		// like an ordinary idle session: what is sent there has to queue and go out AFTERWARDS,
		// never as a second turn appending to the same messages.jsonl beside the one still writing
		// (the report: two sessions spinning, one history with two turns' records interleaved).
		//
		// The window is held open with pauses in the child's reply — without them there is nothing
		// to send INTO and the scenario would measure the ordinary case. The reader's half is the
		// driver's (the conversation's own row carries the marker, the send button reads 排队, the
		// message lands in the pending queue); the Go half owns the fact the DOM cannot show — the
		// order the two requests were actually made in.
		{
			name: "compact-window",
			files: map[string]string{
				"README.md": "# smoke\n\nthe compact-window scenario's working directory\n",
				// What pushes the estimate over the threshold: the floor of any prompt is a few
				// percent of the mock's 128k window, so the tool result is what carries it there.
				"big.txt": strings.Repeat("compact-window filler line abcdefghijklmnopqrstuvwxyz\n", 8000),
			},
			config: "compact:\n  threshold: 0.3\ntool_result:\n  max_chars: 400000\n",
			steps: []mockllm.Step{
				// Iteration 1: an ordinary call that asks for a tool whose output crosses the
				// threshold, so the compaction lands at the top of the NEXT iteration — mid-turn,
				// the shape a conversation that grew actually takes.
				{Reply: mockllm.Stream(
					mockllm.ToolCallStart("call_big", "Bash", `{"command":"cat big.txt"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(40000, 60, 20000, 20),
					mockllm.Done(),
				)},
				// The summary the child is built from: auto-compaction asks through the compact
				// strategy's own CreateChat, a NON-streaming call, so this step is a plain JSON body.
				{Reply: mockllm.JSON("历史摘要：用户要求看一个文件，已经看过。", "")},
				// …and the turn continuing IN THE CHILD, held open long enough for the driver to do
				// its work inside the window.
				{Reply: mockllm.Stream(
					mockllm.Text("压缩之后我接着跑："),
					mockllm.Pause(longTurn),
					mockllm.Text("第一步，"),
					mockllm.Pause(longTurn),
					mockllm.Text("第二步，"),
					mockllm.Pause(longTurn),
					mockllm.Text("第三步，"),
					mockllm.Pause(longTurn),
					mockllm.Text("第四步，"),
					mockllm.Pause(longTurn),
					mockllm.Text("第五步，"),
					mockllm.Pause(longTurn),
					mockllm.Text("第六步，"),
					mockllm.Pause(longTurn),
					mockllm.Text("窗口收尾标记。"),
					mockllm.Finish("stop"),
					mockllm.UsageWithCache(1200, 120, 900, 20),
					mockllm.Done(),
				)},
				// The line the driver queued during the window. It reaches the model only once the
				// turn above has ended — which is exactly what the assertion below reads.
				{Reply: textStream("排队那条我收到了。", 600)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("工具调用、压缩、续跑、排队那条，共四次调用", len(c.requests) == 4, requestCount(c.requests))
				at, seen := c.requestSeen("窗口期这句先排着")
				c.check("窗口期排的那句最终发出去了", seen, "在 "+requestCount(c.requests)+" 里找「窗口期这句先排着」")
				if seen {
					// The whole point, and the one thing the DOM cannot say: that request has to
					// carry the reply the line was queued BEHIND. Sent from inside the window
					// instead, it would precede that reply — two turns writing one session file.
					c.check("它是在那一轮回复之后才发出去的（不是并发的第二个回合）",
						requestHasText(c.requests[at-1], "窗口收尾标记"), requestTail(c.requests[at-1]))
				}
				// The fixture, the child the compaction made, and the session the driver created to
				// get the child's row onto the screen (nothing refreshes the list mid-turn).
				c.check("会话数：fixture、子会话、驱动新建的那个", c.sessionCount() == 3, strconv.Itoa(c.sessionCount()))
			},
		},
		//
		// ask-form: the parked AskUserQuestion form and the keystroke that finishes it. The form is
		// inline in the transcript with option rows and a single-line free-text box; Enter in that
		// box submits (the box cannot hold a newline, so the key has no other job), while ⌘↩ keeps
		// working from anywhere. The assertion is on the BARE key: the driver types its own answer
		// and presses Enter once, and the Go half checks the answer arrived at the model — a form
		// that closed without answering would leave the same trace in the DOM.
		{
			name: "ask-form",
			files: map[string]string{
				"README.md": "# smoke\n\nthe ask-form scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: askStream("call_q1", "用哪个方案？")},
				// The turn only reaches this step if the answer came back through
				// AgentService.AnswerQuestion — the premise the driver's assertion rests on.
				{
					Require: func(req *mockllm.RecordedRequest) string {
						if !requestHasText(req, "两个都不选，按我的来") {
							return "自由输入框里的回答没有回到模型（tool result 里应当带着 answers）"
						}
						return ""
					},
					Reply: textStream("收到你的自由回答，继续。", 600),
				},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("提问一轮加回复一轮，共两次调用", len(c.requests) == 2, requestCount(c.requests))
				_, asked := c.requestSeen("用哪个方案")
				c.check("模型确实问了那个问题", asked, "")
				if n, seen := c.requestSeen("两个都不选，按我的来"); !seen {
					c.check("回答作为 tool result 到达模型", false, "前 "+strconv.Itoa(n)+" 个请求里都没有")
				} else {
					c.check("回答作为 tool result 到达模型", true, "出现在第 "+strconv.Itoa(n)+" 个请求")
				}
			},
		},
		//
		// chain-orphan: a compacted conversation whose pre-compaction half is gone from the list.
		// That is the state a deleted (or max_keep-swept) parent leaves behind, and it used to read
		// as a session that had never been compacted at all: the fold follows a link into the list,
		// and the link had nowhere to land. The child's own record carries the parent's TITLE, so
		// the row can still say what became of the other half — as a note, since there is nothing
		// left to unfold. The driver asserts the note; the Go half only proves the fixture really is
		// orphaned, which is the premise the whole scenario rests on.
		{
			name:        "chain-orphan",
			orphanChain: true,
			files: map[string]string{
				"README.md": "# smoke\n\nthe chain-orphan scenario's working directory\n",
			},
			after: func(c *checkCtx) {
				metas := c.sessionMetas()
				ids := map[string]bool{}
				for _, m := range metas {
					if id, _ := m["id"].(string); id != "" {
						ids[id] = true
					}
				}
				var linked, dangling string
				for _, m := range metas {
					if par, _ := m["compacted_parent_id"].(string); par != "" {
						linked = par
						if !ids[par] {
							dangling = par
						}
					}
				}
				c.check("会话记录里确实有一条压缩链接", linked != "", fmt.Sprintf("metas=%d", len(metas)))
				c.check("链接的父会话确实不在列表里（fixture 的前提）", dangling == linked && dangling != "",
					fmt.Sprintf("parent=%s 在列表里=%v", linked, ids[linked]))
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
		// review-all：「评审所有改动」——最新一轮的第二个评审入口，范围是**工作树 vs HEAD**，
		// 而不是某一轮的两个快照。
		//
		// 为什么单独一个场景：这个入口有两件别处钉不住的事。一是它只在**最新那一轮**上，而且当
		// 最新一轮自己什么都没改、工作树却还是脏的时候也必须还在（改动来自更早的轮次）——所以剧本
		// 是"写文件的轮次 + 只读的一问"，driver 两轮都看。二是它起的那次评审读的是另一份 diff
		// （工作树对 HEAD，不是 checkpoint 对），prompt 与面板标题都得跟着换，否则读者会把整树的
		// 意见读成"这一轮改的"。
		{
			name: "review-all",
			files: map[string]string{
				"README.md": "# smoke\n\nthe review-all scenario's working directory\n",
			},
			gitInit: true,
			steps: []mockllm.Step{
				// 第 1 轮：写一个文件（这一轮自己有改动）。
				{Reply: writeFileStream(0, "NOTES.md", "一行笔记：这一行是整树评审要看的改动。\n", "call_w1")},
				{Reply: textStream("已经写下 NOTES.md。", 900)},
				// 第 2 轮：只读的一问 —— 这一轮自己没有改动，工作树里的 NOTES.md 仍未提交。
				{Reply: textStream("NOTES.md 里是一行笔记。", 900)},
				// 整树评审：一条意见，然后收尾。
				{Reply: findingStream("NOTES.md", 1, "warn", "call_f1")},
				{Reply: textStream("评审完成：1 条意见。", 1100)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("两轮对话加一次整树评审，共五次调用", len(c.requests) == 5, requestCount(c.requests))
				// 作用域：整树那句必须在，而"本轮改的"那句一个字都不能有。两句话是互斥的，说错了
				// 评审审的就是另一件事 —— 而 prompt 是评审唯一读得到的东西。
				_, whole := c.requestSeen("EVERY uncommitted change in the working tree")
				c.check("评审 fork 的 prompt 说清了范围是整个工作树", whole, "")
				_, turnScoped := c.requestSeen("what the latest turn touched")
				c.check("整树评审的 prompt 没有把它说成某一轮改的", !turnScoped, "")
				_, listed := c.requestSeen("- NOTES.md")
				c.check("评审 prompt 里列出了未提交的那个文件", listed, "")
				// 记录：面板要靠这些字段（重启之后）才拿得到同一份 diff，而 whole_tree 是
				// "读工作树还是读 checkpoint" 的唯一依据。
				records := c.oneOffFiles()
				c.check("整树评审留了一份记录", len(records) == 1, fileList(records))
				if len(records) == 1 {
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
							c.check("记录写下了 whole_tree（面板据此读工作树）",
								meta.Extra[agent.OneOffKeyWholeTree] != "", "")
							c.check("记录写下了发起它的那一轮",
								meta.Extra[agent.OneOffKeyReviewedMsg] != "", meta.Extra[agent.OneOffKeyReviewedMsg])
							c.check("记录写下了文件清单",
								meta.Extra[agent.OneOffKeyPaths] != "", meta.Extra[agent.OneOffKeyPaths])
							c.check("整树评审没有记 turn（它不属于任何一轮）",
								meta.Extra[agent.OneOffKeyTurn] == "", meta.Extra[agent.OneOffKeyTurn])
						}
					}
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
		// ⌘F in a file the reader is looking at. The diff and the report are the other two
		// surfaces that host find (oneoff-footer / oneoff-report); this scenario covers the two
		// that show a FILE: the document viewer over an attachment card (how a file gets a card,
		// and the card is what has the ⤢), and then the SAME file in the content column's file
		// view — two hosts with the conversation still mounted underneath, which is the state the
		// ownership rule can get wrong. The file repeats one line so the driver can name the
		// count. The Go half is the usual two-call script.
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
		// 选中一段文字问当前会话：菜单浮出来的三个宿主各走一遍（附件卡片展开的预览、文件视图、
		// 会话全文），而 Go 这一半断言的是**模型到底收到什么** —— UI 上看起来都对、发出去的却
		// 少了来源行或拼错了地方，是这个场景存在的理由。
		//
		// 提问先收进「待提问」队列（回车入队，发送才出去），所以「批」这件事只能在这里断言：
		// 同一份文件只写一次来源，同一句问题只写一次（几段的原文并列在它下面），而删掉的那一段
		// 一个字节都不该出现 —— 分几条发出去在 requestHasText 眼里和拼成一条一模一样。
		//
		// 三条出路各覆盖一次：回车/「加入待提问」把段落攒进队列（卡片那一半），会话全文那一半
		// 走 Shift+回车那条捷径（真的当场发），文件视图那一半把四段收进队列、改掉一组的问题、
		// 删掉一段，再点发送。问答本身还覆盖「清空」、「关掉预览只带走它那份来源的项」、以及
		// 「会话里攒的不会被打开一个文件毁掉」。
		{
			name: "selection-ask",
			files: map[string]string{
				"NOTE.md": "# smoke\n\n" + selectionAskLineA + "\n\n" + selectionAskLineB + "\n\n" +
					selectionAskLineC + "\n\n" + selectionAskLineD + "\n",
			},
			steps: []mockllm.Step{
				{Reply: sendFileStream("NOTE.md", "call_send")},
				// 这一条回答里带着会话全文那一半要选的两句：它得在司机去选之前就已经在屏幕上。
				{Reply: textStream("已经把 NOTE.md 发给你了。\n\n"+selectionAskChatLine1+"\n\n"+selectionAskChatLine2, 900)},
				{Reply: textStream("对话里那一段的回答。", 900)},
				{Reply: textStream("一起问的那几条的回答。", 900)},
				{Reply: textStream("两句一起问的回答。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// 五个请求：第一条用户消息、它的工具续轮、一次直接发送、两次整队列发送。
				c.check("五次调用（送文件 + 一次直发 + 两次批量）", len(c.requests) == 5, requestCount(c.requests))
				if len(c.requests) < 5 {
					return
				}
				shortcut, batch := lastUserText(c.requests[2]), lastUserText(c.requests[3])
				chatBatch := lastUserText(c.requests[4])
				source := "文件：" + filepath.Join(c.work, "NOTE.md")
				c.check("Shift+回车那一条带着原文和读者写的问题，且不带文件来源",
					strings.Contains(shortcut, "“"+selectionAskChatLine1+"”") &&
						strings.Contains(shortcut, selectionAskChatAsk) &&
						!strings.Contains(shortcut, "文件："), strutil.Truncate(shortcut, 140))
				c.check("队列拼成的是同一条消息（三段的原文都在最后这一条里）",
					strings.Contains(batch, selectionAskLineA) && strings.Contains(batch, selectionAskLineB) &&
						strings.Contains(batch, selectionAskLineC), strutil.Truncate(batch, 240))
				c.check("同一份文件的来源只写了一次",
					strings.Count(batch, source) == 1, strconv.Itoa(strings.Count(batch, source))+" 次")
				c.check("同一句默认问题只写了一次（两段原文共用它）",
					strings.Count(batch, selectionAskDefaultAsk) == 1,
					strconv.Itoa(strings.Count(batch, selectionAskDefaultAsk))+" 次")
				c.check("原文被引号引起来（哪段是文件里的、哪句是读者问的分得开）",
					strings.Contains(batch, "“"+selectionAskLineA+"”") &&
						strings.Contains(batch, "“"+selectionAskLineB+"”"), strutil.Truncate(batch, 240))
				c.check("改过的问题算数（发出去的是改后的那句）",
					strings.Contains(batch, selectionAskAskC2) && !strings.Contains(batch, selectionAskAskC),
					strutil.Truncate(batch, 240))
				c.check("删掉的那一段一个字节都没发出去",
					!strings.Contains(batch, selectionAskLineD), strutil.Truncate(batch, 240))
				// 从对话里选中的那两段：没有文件可指，所以不该凭空多出一行来源。
				c.check("对话里选的两段都在同一条消息里",
					strings.Contains(chatBatch, "“"+selectionAskChatLine1+"”") &&
						strings.Contains(chatBatch, "“"+selectionAskChatLine2+"”"), strutil.Truncate(chatBatch, 160))
				c.check("对话里选的两段共用同一句默认问题",
					strings.Count(chatBatch, selectionAskDefaultAsk) == 1,
					strconv.Itoa(strings.Count(chatBatch, selectionAskDefaultAsk))+" 次")
				c.check("从对话里选的那一条没有文件来源那一行",
					!strings.Contains(chatBatch, "文件："), strutil.Truncate(chatBatch, 160))
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
		// parallel-tools: 一个回复里同名工具被调用多次时，每张卡都要落上**自己**的参数。
		//
		// 事件的到达顺序是"先全部的 start，后全部的 args"：start 由消费流的一侧发出（每见到一个
		// tool-call 块就发一次，那时还没有参数），args 由执行侧发出（`executeToolCallsParallel` 先
		// 把这一批的 args 一起发完，`Sequential` 也是先发再跑）。前端原本按**工具名 + 最新未完成**
		// 认卡，于是一批同名的调用里，每个 args 都盖在最新那张上：四张卡只有最后一张有参数与标题，
		// 前三张永远停在「执行中…」（issue #39）。修法是把 tool call 的 id 一路带过去（agent:tool
		// 载荷里加了 toolCallId），按 id 认卡。
		//
		// 这里用四个 ReadFile 而不是四个 SubAgent：同一个前端缺陷，不需要子 agent 那一整套 mock。
		{
			name: "parallel-tools",
			files: map[string]string{
				"README.md": "# smoke\n\nthe parallel-tools scenario's working directory\n",
				"a.md":      "A-FILE-CONTENT\n",
				"b.md":      "B-FILE-CONTENT\n",
				"c.md":      "C-FILE-CONTENT\n",
				"d.md":      "D-FILE-CONTENT\n",
			},
			steps: []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.ToolCallStart("call_r1", "ReadFile", `{"path":"a.md"}`),
					mockllm.ToolCallStart("call_r2", "ReadFile", `{"path":"b.md"}`),
					mockllm.ToolCallStart("call_r3", "ReadFile", `{"path":"c.md"}`),
					mockllm.ToolCallStart("call_r4", "ReadFile", `{"path":"d.md"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(1500, 60, 1400, 10),
					mockllm.Done(),
				)},
				{Reply: textStream("四份都读完了。", 1500)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// 四份文件的内容都回喂给了模型：这一批真的都执行了（而不是只跑了一张卡）。
				for _, name := range []string{"A-FILE", "B-FILE", "C-FILE", "D-FILE"} {
					_, seen := c.requestSeen(name)
					c.check("工具结果里有 "+name, seen, requestCount(c.requests))
				}
			},
		},
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
		// sidebar-wait: 一个会话在等你，侧栏要说出来 —— 而它所在的分组被折起来时，那句话只能
		// 落在表头上（行已经被折叠拿走了）。
		//
		// 标记不是前端自己判断的：Go 在 park 的那一刻把「等你做什么」写进这个会话的状态
		// （AgentState.Waiting，见 permission.go / agent_turn.go），前端只渲染。一次等待的
		// 两种形态都在这条里过一遍（bash 授权 / 模型提问），因为它们的措辞不同，而「标记在答复
		// 之后消失」是两半共用的同一条规矩：终结这次等待的事实写下下一个状态，标记随之清掉。
		//
		// 项目组是承重的 fixture：有项目才有「折叠一个组」这件事，而成员会话的工作目录由项目
		// 驱动，所以 perm-fixture.txt 种在项目目录里。
		{
			name: "sidebar-wait",
			files: map[string]string{
				"README.md": "# smoke\n\nthe sidebar-wait scenario's STALE snapshot directory\n",
			},
			project: &projectSeed{
				name: "smoke-wait",
				files: map[string]string{
					"perm-fixture.txt": "SIDEBAR-WAIT-FIXTURE-CONTENT\n",
				},
			},
			config: `
permissions:
  bash:
    ask:
      - "cat perm-fixture*"
`,
			steps: []mockllm.Step{
				{Reply: bashStream("cat perm-fixture.txt", "call_sw1")},
				// 两次 park 之间要有一次真的在跑、又什么都不问的工具调用：它是 driver 唯一能
				// 稳定观察到「标记已经没了」的窗口（一秒），而背靠背的两次等待之间那个间隔短到
				// 一次轮询就会错过。纯文本回复不行 —— 那会直接结束这一轮。
				{Reply: bashStream("sleep 1", "call_sw2")},
				{Reply: askStream("call_sw3", "还要继续吗？")},
				{Reply: textStream("都收到了。", 800)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// 两次等待都被真的答复过：命令跑了（输出回喂给模型），提问的答案也回去了。
				_, ran := c.requestSeen("SIDEBAR-WAIT-FIXTURE-CONTENT")
				c.check("答复授权后命令真的执行了", ran, requestCount(c.requests))
				_, answered := c.requestSeen("按你写的来")
				c.check("答复提问后答案真的回到了模型", answered, requestCount(c.requests))
			},
		},
		//
		// sidebar-run: 一个会话还在跑，而它所在的分组被折起来时，表头要说出来。
		//
		// 与 sidebar-wait 是同一条规矩的第二次应用（折叠把行拿走了，所以那句话只能落在表头上），
		// 但说的是另一件事：那一条是「有人在等你」，这一条是「有东西还在跑」——行上那只转圈的
		// 搬到了表头上，两者可以同时成立（一个会话完全可能在等你，同时另一轮在跑）。
		//
		// 这一轮用 mock 的 pause 撑着（长到 driver 能在窗口里折叠、断言、等它结束），回合真的跑完
		// 之后标记必须自己消失 —— 那是一个稳态，所以 driver 可以直接等它不在。
		{
			name: "sidebar-run",
			files: map[string]string{
				"README.md": "# smoke\n\nthe sidebar-run scenario's STALE snapshot directory\n",
			},
			project: &projectSeed{
				name: "smoke-run",
				files: map[string]string{
					"README.md": "# smoke\n\nthe sidebar-run scenario's project directory\n",
				},
			},
			steps: []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Text("这一轮要跑一会儿："),
					mockllm.Pause(longTurn),
					mockllm.Text("还在跑。"),
					mockllm.Pause(longTurn),
					mockllm.Text("跑完了。"),
					mockllm.Finish("stop"),
					mockllm.Usage(1200, 40),
					mockllm.Done(),
				)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("这一轮真的跑过（一次调用）", len(c.requests) == 1, requestCount(c.requests))
			},
		},
		//
		// sidebar-unseen: 一个回合在你看别处的时候跑完了，那一行要保持高亮 —— 直到你看它为止；而它
		// 所在的分组还折着时，那句话落在表头上。
		//
		// 这是「在跑」那条标记的延续：转圈在活干完的那一刻就没了，而「有事在那等你读」不会自己消失，
		// 所以状态得由前端留着（`agent:idle` 里 Go 判的 `current` 是「结束那一刻读者在不在看它」的
		// 权威答案）。清零点只有一个：这个会话成为屏幕上那一个 —— 行的点击、两个面板、删除交给读者的
		// 下一条，全都汇到 currentId 上。
		//
		// 一次新的、切换过去用的会话（无项目桶的 ＋）是 fixture 的一部分：不切走就没有「在别处」，
		// 而没有它，跑完的会话仍然是屏幕上那个，按定义就不该标。
		{
			name: "sidebar-unseen",
			files: map[string]string{
				"README.md": "# smoke\n\nthe sidebar-unseen scenario's STALE snapshot directory\n",
			},
			project: &projectSeed{
				name: "smoke-unseen",
				files: map[string]string{
					"README.md": "# smoke\n\nthe sidebar-unseen scenario's project directory\n",
				},
			},
			steps: []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Text("这一轮要跑一会儿："),
					mockllm.Pause(longTurn),
					mockllm.Text("还没看完呢。"),
					mockllm.Pause(longTurn),
					mockllm.Text("跑完了。"),
					mockllm.Finish("stop"),
					mockllm.Usage(1200, 40),
					mockllm.Done(),
				)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// 看那个会话本身是读内存里的那一段，不该再问模型一次。
				c.check("这一轮只问过模型一次（回看不再产生请求）", len(c.requests) == 1, requestCount(c.requests))
			},
		},
		//
		// 权限确认（文件工具）: the FILE rules' one ask path — a ReadFile that
		// matched an ask rule the frontend must show before anything is read.
		//
		// 与 perm-allow 的三点不同，正是这半边要单独有场景的理由：
		//   * 卡片内容不是命令 + 规则，而是「工具 + canonical 路径」+ 规则（agent 渲染的预览），
		//     而 kind 是 files —— 前端文案随之不同（permCopy.files）。
		//   * 规则是**路径** glob，模式匹配的是 canonical 绝对路径。这里用 "*/perm-fixture*"：
		//     config 写的是**全局**层，而全局层里的相对模式锚定在 $HOME 上（沙箱的 work 目录
		//     又没开 gitInit，于是它不是一棵树）—— 在沙箱里唯一稳定的写法是匹配"任何目录下的
		//     这个名字"。树锚定的相对模式由 agent/permission 的单测覆盖。
		//   * 拒绝只拒这一次调用、回合继续（不像 bash ask 会中止回合）——这一条由 Go 侧的
		//     after 断言：模型收到了工具结果，并且回合走到了下一条回复。
		{
			name: "perm-files",
			files: map[string]string{
				"README.md":        "# smoke\n\nthe perm-files scenario's working directory\n",
				"perm-fixture.txt": "PERM-FILE-FIXTURE-CONTENT\n",
			},
			config: `
permissions:
  files:
    read:
      ask:
        - "*/perm-fixture*"
`,
			steps: []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.ToolCallStart("call_pf1", "ReadFile", `{"path":"perm-fixture.txt"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(1500, 40, 1400, 10),
					mockllm.Done(),
				)},
				{Reply: textStream("读过那份文件了。", 1500)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// The file's CONTENT reaching the model is what proves the read
				// happened — an approved ask lets the call through.
				_, ran := c.requestSeen("PERM-FILE-FIXTURE-CONTENT")
				c.check("允许后读文件真的执行了（内容回喂给了模型）", ran, requestCount(c.requests))
			},
		},
		//
		// 权限确认（拒绝）: the same rule and the same command, but the driver clicks 拒绝.
		// The command must NOT run, and the model must be told the user refused it.
		//
		// perm-unparseable: 命令**解析不出来**时的确认卡，不能说自己"命中了某条 ask 规则"。
		//
		// 政策对解析不了的命令（末尾挂着 && 、引号没闭合这类）是保守地问一次 —— 它**根本没走到**任何
		// 规则；可卡片那句中文是写死的「命中 permissions.bash.ask 规则」，而 agent 的预览又把哨兵值
		// 当成规则名念出来，于是读者看到的是"一条我没写过的规则 + 一条看着像被截断的命令"（issue #40
		// 的截图正是这两件事叠在一起）。政策现在用 permission.AskNoRule 区分这两种来源，预览与卡片
		// 各说各的真话。
		//
		// config 里那条 ask 规则故意与命令**无关**：它只为让政策存在（没有规则时 agent 不做检查），
		// 于是"卡片没提它"本身就是断言的一部分。
		{
			name: "perm-unparseable",
			files: map[string]string{
				"README.md": "# smoke\n\nthe perm-unparseable scenario's working directory\n",
			},
			config: `
permissions:
  bash:
    ask:
      - "cat perm-fixture*"
`,
			steps: []mockllm.Step{
				{Reply: bashStream("ls -la | head && ", "call_pu1")},
				{Reply: textStream("好，那条命令我不跑了。", 1500)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// 「保守地问一次」仍然是一条真的询问：拒绝后命令没跑，模型被告知。
				_, ran := c.requestSeen("total ")
				c.check("拒绝后那条命令没有执行", !ran, requestCount(c.requests))
				_, told := c.requestSeen("denied")
				c.check("模型被告知这条命令被拒绝", told, "")
			},
		},
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
		// 权限确认（MCP 工具加载的花费）: 加载一个新工具会把整段 prompt 缓存作废，超过
		// mcp_tool_search.cache_budget 就先问一次。这张卡的问题是**钱**，不是危险 —— 所以它和
		// bash 那三张有三处不同，都要在这条场景里成立：拒绝**不中止回合**（模型被告知原因后接着
		// 干），拒绝**记在本会话**上（模型再调一次不再弹卡，而是直接收到同一个拒绝），以及
		// **无人值守模式不拦**（那是单测的事，进程里看不到区别）。
		//
		// 这个 fixture 有真实的池子：OAuth server 连上后池里有一个**未加载**的工具，一次搜索才
		// 可能改变工具列表（池里没有未发现的工具时，搜也搜不出新东西，缓存根本不会坏 —— 那时不
		// 该问）。这也是 mcpOAuth 唯一被两个场景共用的原因。
		//
		// min_tools_for_search 为什么是 **-1** 而不是 0：这个字段的默认标签是 5，而
		// creasty/defaults 把"显式写 0"当成"没写"（零值）再填回 5 —— 于是 0 在这个字段上写不出来。
		// 判据是 `len(pool) > 阈值`，-1 就是"永远不自动加载"，即这个场景要的那一件事。
		//
		// 价格必须由场景自己给（scenario.pricing）：沙箱那个 smoke-model 没有内置价格表，而估不出
		// 钱的卡不该弹 —— 这是产品行为，不是这里可以绕过的东西。
		{
			name:     "perm-mcp-budget",
			mcpOAuth: true,
			files: map[string]string{
				"README.md": "# smoke\n\nthe perm-mcp-budget scenario's working directory\n",
			},
			config: `
compact:
  auto: false
mcp_tool_search:
  min_tools_for_search: -1
  cache_budget: 1
`,
			// ¥100/M 输入、¥10/M 缓存命中 → 一次整段重读 90 元/百万 token。下面那两轮各报
			// 30000 个 prompt token，于是预估 ¥2.700 > 预算 ¥1.000（¥100/M 那套数字是编的，
			// 但算术是真的，卡片上印的就是这一套）。
			pricing: &config.PricingConfig{
				InputPrice:          ptrFloat(100),
				OutputPrice:         ptrFloat(100),
				CacheReadInputPrice: ptrFloat(10),
			},
			steps: []mockllm.Step{
				// 第一轮：模型去搜工具 → 卡片 → 用户点拒绝 → 模型被告知，继续干活。
				{Reply: searchStream("call_mb1", 30_000)},
				{Reply: textStream("那用现有工具继续。", 1500)},
				// 第二轮：同一个模型再搜一次 → 本会话已拒，直接收到同一个拒绝，不再弹卡。
				{Reply: searchStream("call_mb2", 30_000)},
				{Reply: textStream("明白了，不搜了。", 1500)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// 池里那个工具是「未加载」的：提醒只报名字，而这件事决定了这次搜索**可能**改变
				// 工具列表 —— 缓存失效的前提。
				_, announced := c.requestSeen("mcp__oauth-fixture__echo")
				c.check("未加载的 MCP 工具已被告知模型（池子非空）", announced, requestCount(c.requests))
				// 拒绝真的到了模型手里，而且带着"别再调"的指令：只回一句"被拒绝"会让它再试一次。
				_, refused := c.requestSeen("不要再调用 MCPSearchTools")
				c.check("拒绝写进了工具结果，且带上「不要再调用」", refused, "")
				// 第二次调用同样被拒 —— 这一次没有卡片，所以它必须**已经**被记在会话上。
				if at, again := c.requestSeen("用户拒绝了本次 MCP 工具加载"); again {
					c.check("会话级的拒绝在第二次调用时仍然生效", at >= 2, "第一次出现在第 "+strconv.Itoa(at)+" 条请求")
				} else {
					c.check("会话级的拒绝在第二次调用时仍然生效", false, "工具结果里没有拒绝")
				}
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
		// live-roll：跑着的那几行字是"滚动"着换状态的，不是瞬切。
		//
		// 为什么单独一个场景：状态切换最快的地方是助手气泡里的运行占位行（正在思考… ↔ 正在执行…），
		// 它每次 thinking/tool 转换都换一次，模型快起来就是一片闪；而"切换时有动画"这件事在别的场景
		// 里没人钉得住。这里让一轮里必然发生两次翻转（起工具→执行，工具完模型再说话→又回思考），
		// driver 逐次采样，每次变化都要求那一刻有动画在跑。
		//
		// 工具故意跑 3 秒：实时行要活着足够久，driver 才来得及等到它的标题（命令原文随 agent:tool
		// 的第二个事件到，比实时行本身晚），也才看得清它是个滚动盒而不是一闪而过的东西。
		{
			name: "live-roll",
			files: map[string]string{
				"README.md": "# smoke\n\nthe live-roll scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: mockllm.Stream(
					mockllm.Text("先跑一条命令。"),
					mockllm.Pause(longTurn),
					mockllm.ToolCallStart("call_r1", "Bash", `{"command":"sleep 3 && echo roll-ok"}`),
					mockllm.Finish("tool_calls"),
					mockllm.UsageWithCache(1500, 40, 1400, 10),
					mockllm.Done(),
				)},
				{Reply: textStream("两步都跑完了。", 800)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("工具结果回喂给了模型", len(c.requests) == 2, requestCount(c.requests))
				_, ran := c.requestSeen("roll-ok")
				c.check("那条命令真的跑了（它的输出回喂给了模型）", ran, "")
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
				// 置顶：driver 在项目组里置顶了这条成员，界面上验的是它升到该组第一行且没跨组；
				// 这里验它写对了记录 —— 置顶是会话自己的字段，不随项目走，detach 也必须原样保留
				// （detach 只改 project_id 和快照，不许把别的东西整条重写掉）。
				c.check("置顶挂在被置顶的那条会话上，且 detach 没弄丢它", bound.Pinned,
					fmt.Sprintf("pinned=%v", bound.Pinned))
			},
		},
		//
		//
		// project-palette: ⌘P 是进项目的那条键盘路径 —— 在「组」之间选，回车打开该组**最近用过**
		// 的会话，还没有会话就建第一个。四件断言都是界面上看不出来的：
		// (1) 打开的是「最近」而不是碰巧当前的那个 —— driver 先用 ⌘N 现造一个成员（它同时是
		// createdAt 与 updatedAt 最新的，两种「最新」在这里重合），再切回旧成员，然后 ⌘P 回车，
		// 断言 active 回到项目组的第一行；
		// (2) 空项目回车真的建出会话，且**绑在该项目上** —— 前端只看得见行数变了，绑定关系只有
		// 磁盘上的 meta.json 说得清（NewSession 在项目不能驱动时会静默建一个不属于任何项目的
		// 会话，这条就是防它的）；
		// (3) ⌘P 列的是「人落得进去的组」，所以无项目那个桶也在列表里（侧栏的最后一组）。它的成
		// 员数、回车落到哪个会话由 driver 在真机上验；这里钉的是它**没有凭空建** —— 那一行回车打
		// 开的是已有的无项目会话，磁盘上仍然只有一个不带 project_id 的会话；
		// (4) 整个过程不碰模型 —— 一次键盘导航不该花掉任何 token。
		{
			name: "project-palette",
			files: map[string]string{
				"README.md": "# smoke\n\nthe project-palette scenario's work directory\n",
			},
			project: &projectSeed{
				name: "smoke-pal",
				files: map[string]string{
					"README.md": "# smoke\n\nthe project-palette scenario's project directory\n",
				},
				// 一个空项目 = 唯一能走到「新建第一个会话」那条分支的状态。
				emptyName: "smoke-pal-empty",
			},
			after: func(c *checkCtx) {
				c.check("⌘P 的导航没有向模型发过任何请求", len(c.requests) == 0, requestCount(c.requests))
				// meta.json 里这个字段是 project_id（session.Session 的 json tag），不是绑定层那个
				// camelCase 的 projectId —— 前者才是磁盘上的事实。
				bound := 0
				for _, m := range c.sessionMetas() {
					if m["project_id"] == "smoke-project-empty" {
						bound++
					}
				}
				c.check("⌘P 在空项目里建的会话绑在该项目上（meta.json 的 project_id）", bound == 1,
					fmt.Sprintf("project_id=smoke-project-empty 的会话有 %d 个", bound))
				// 无项目那一行是「打开」不是「新建」：会话的 project_id 带 omitempty，所以无项目的
				// 会话在 meta.json 里根本没有这个键——两种写法都算。
				loose := 0
				for _, m := range c.sessionMetas() {
					if pid, ok := m["project_id"]; !ok || pid == "" {
						loose++
					}
				}
				c.check("无项目那一行打开的是已有的无项目会话（没有凭空建一个）", loose == 1,
					fmt.Sprintf("不带 project_id 的会话有 %d 个", loose))
				members := 0
				for _, m := range c.sessionMetas() {
					if m["project_id"] == "smoke-project" {
						members++
					}
				}
				c.check("有会话的项目里有两个成员（原成员 + ⌘N 的新成员）", members == 2,
					fmt.Sprintf("project_id=smoke-project 的会话有 %d 个", members))
			},
		},
		//
		// notify-open-session: 点一条通知要落到它说的那个会话。
		//
		// 通知本身（一次原生点击）驱动不了 —— 判据里有一半是焦点，而且它由系统投递 —— 所以这条
		// 驱动的是那次点击落到页面上的那一半：`ui:open_session` 由 Go 发出（desktop/notify.go 的
		// openSession），前端在 App.tsx 里接住它并走 clickSession。driver 用 Wails 交给页面的
		// 那个入口派发同一个事件，于是它钉住的是「前端真的照做了」：切到了那个会话，并且真的把它
		// 从磁盘读了出来（另一个会话的转写此前从没被显示过）。
		//
		// Go 那一半查的是「什么都没发生」：切换是一次读取，不该向模型发出任何请求。
		//
		{
			name: "notify-open-session",
			seedSecond: []session.Message{
				{Type: session.MessageTypeUser, Content: "另一个会话里的问题"},
				{Type: session.MessageTypeAssistant, Content: "另一个会话里的答案。"},
			},
			after: func(c *checkCtx) {
				c.check("切换到通知说的那个会话没有惊动模型", len(c.requests) == 0, requestCount(c.requests))
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
			},
		},
		//
		// session-palette: ⌘K 搜会话标题 —— ⌘P 的兄弟面板（同一个框，另一个面板）。它一个 token
		// 都不该花（导航不碰模型），也不该改任何会话记录 —— 这两条界面上看不出来，只有 Go 侧读得到；
		// 其余（列的是对话、按标题过滤、输入法组合不参与、回车落到哪、折叠的组要展开）由 driver 在
		// 真机上验，见 drivers/session-palette.js。
		//
		// fixture 的三个会话：一个项目成员（冒烟会话）+ 两个无项目的（无项目会话、另一个会话，
		// 后者带一段预置转写 —— 它同时把「从磁盘读一次」的那条路带上）。启动时打开的是最新的那个
		// （项目成员），所以它的组默认是展开的，driver 后面自己把它折起来。
		{
			name: "session-palette",
			files: map[string]string{
				"README.md": "# smoke\n\nthe session-palette scenario's working directory\n",
			},
			project: &projectSeed{
				name: "smoke-sesspal",
				files: map[string]string{
					"README.md": "# smoke\n\nthe session-palette scenario's project directory\n",
				},
			},
			seedSecond: []session.Message{
				{Type: session.MessageTypeUser, Content: "另一个会话里说过的话。"},
				{Type: session.MessageTypeAssistant, Content: "另一个会话里的回答。"},
			},
			after: func(c *checkCtx) {
				c.check("⌘K 全程没有向模型发过任何请求", len(c.requests) == 0, requestCount(c.requests))
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// 「一行一个对话」的另一半：⌘K 是只读的 —— 它没有建会话、没有删会话。三个 meta.json
				// 的标题就是磁盘上的事实（driver 只看得到行数）。
				metas := c.sessionMetas()
				c.check("⌘K 从没建过也没有删过会话（磁盘上仍是三个）", len(metas) == 3,
					fmt.Sprintf("%d 个", len(metas)))
				titles := map[string]bool{}
				bound := ""
				for _, m := range metas {
					if t, _ := m["title"].(string); t != "" {
						titles[t] = true
					}
					// meta.json 里是 project_id（session.Session 的 json tag），不是绑定层那个
					// camelCase 的 projectId —— 前者才是磁盘上的事实。
					if m["project_id"] == "smoke-project" {
						bound, _ = m["title"].(string)
					}
				}
				c.check("三个会话还是它们自己（标题一个都没变）",
					titles["冒烟会话"] && titles["无项目会话"] && titles["另一个会话"],
					fmt.Sprintf("%d 个标题", len(titles)))
				c.check("成员会话仍然绑在项目上（⌘K 没有动过绑定）", bound == "冒烟会话", bound)
			},
		},
		//
		// session-palette-busy: ⌘K 里「在跑 / 在等你」的会话排最前面 —— 打开这个浮层最常见的理由，
		// 正是「刚才停下来等我的那次对话叫什么来着」：读者打不出那个标题，而「最近更新」也不会把
		// 它顶上来（被 park 的那一轮自 park 之后就没再写过，中途跑完的对话反而更新）。
		//
		// fixture 让这条规则**可判**：一个停在权限授权上的会话（冒烟会话，启动时打开的那个），
		// 加上读者随后真的在里面发过一次消息、并且跑完的无项目会话（它因此成为最近更新的那个）。
		// 于是「⌘K 第一行是冒烟会话」只可能来自新规则 —— Go 那一半读磁盘上的 updated_at 把这句话
		// 的地基钉死：被 park 的确实更旧，否则 driver 那条断言在旧实现下也照样绿。
		{
			name: "session-palette-busy",
			files: map[string]string{
				"README.md": "# smoke\n\nthe session-palette-busy scenario's working directory\n",
			},
			project: &projectSeed{
				name: "smoke-busypal",
				files: map[string]string{
					"perm-fixture.txt": "PALETTE-BUSY-FIXTURE-CONTENT\n",
				},
			},
			config: `
permissions:
  bash:
    ask:
      - "cat perm-fixture*"
`,
			steps: []mockllm.Step{
				// 冒烟会话那一轮：一条命中 ask 规则的命令，于是这一轮停在读者身上（在等你）。
				{Reply: bashStream("cat perm-fixture.txt", "call_pb1")},
				// 读者切到无项目会话之后的那个回合：它跑完，于是那个会话成为最近更新的那一个。
				{Reply: textStream("桶里的回答。", 800)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("两个回合各一次调用（⌘K 自己一个请求都不花）", len(c.requests) == 2, requestCount(c.requests))
				parked, other := c.sessionByTitle("冒烟会话"), c.sessionByTitle("无项目会话")
				older, detail := false, "(磁盘上缺少 meta)"
				if parked != nil && other != nil {
					older = parked.UpdatedAt.Before(other.UpdatedAt)
					detail = parked.UpdatedAt.Format(time.RFC3339Nano) + " < " + other.UpdatedAt.Format(time.RFC3339Nano)
				}
				c.check("停在授权上的会话确实是较旧的那一个（所以它不是靠最近更新排第一的）", older, detail)
			},
		},
		//
		//
		// fork: 在一条 user 消息上右键「从这里分支」—— 把这一轮**之前**的对话拷成一个新会话，
		// 工作区、检查点与原会话一律不动。它与 rewind 是同一条切线上的两个方向，所以 fixture 也是
		// 那种形状（一轮用 bash 写文件，工作区是真的动过），只是要证明的东西相反。
		//
		// 三件事界面上看不见，只有 Go 侧读得到，所以这一半必须查：
		//   * 分支的 messages.jsonl 是原会话那一份的**逐字节前缀**（拷贝绝不能是重放 —— 重放会把
		//     Timestamp 改成现在，而桌面分页按时间戳严格比较，同一戳的一批记录会被整段跳过）；
		//   * 分支目录里**没有** checkpoints/（「不动检查点」就是它没有），而原会话有；
		//   * 工作区没动：第一轮用 bash 写的那个文件仍在（回退会把它删掉，分支不能）。
		{
			name: "fork",
			files: map[string]string{
				"README.md": "# smoke\n\nthe fork scenario's STALE snapshot directory\n",
			},
			project: &projectSeed{
				name: "smoke-fork",
				files: map[string]string{
					"README.md": "# smoke\n\nthe fork scenario's project directory\n",
				},
			},
			steps: []mockllm.Step{
				{Reply: bashStream("echo made > made.txt", "call_fk1")},
				{Reply: textStream("写好了。", 800)},
				// 第二轮是纯文本：分叉点选在它身上，于是「分支里不该有它」有东西可查。
				{Reply: textStream("第二轮回复。", 800)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("三个回合各一次调用（分支自己一个请求都不发）", len(c.requests) == 3, requestCount(c.requests))

				store, err := session.NewFileStore(filepath.Join(c.home, ".tachi", "session"))
				if err != nil {
					c.check("打开沙箱的会话存储", false, err.Error())
					return
				}
				src, fork := c.sessionByTitle("冒烟会话"), c.sessionByTitle("冒烟会话 · 分支")
				if src == nil || fork == nil {
					c.check("磁盘上有原会话与它的分支", false,
						fmt.Sprintf("src=%v fork=%v", src != nil, fork != nil))
					return
				}
				c.check("磁盘上多出一个会话（原会话 + 无项目桶 + 分支）", c.sessionCount() == 3,
					fmt.Sprintf("%d 个", c.sessionCount()))
				c.check("分支继承了工作区绑定（同一个项目）", fork.ProjectID == "smoke-project",
					"project_id="+fork.ProjectID)
				c.check("原会话的绑定没有被改写", src.ProjectID == "smoke-project", src.ProjectID)

				srcBytes, rerr := os.ReadFile(filepath.Join(store.BaseDir(), src.ID, "messages.jsonl"))
				forkBytes, ferr := os.ReadFile(filepath.Join(store.BaseDir(), fork.ID, "messages.jsonl"))
				c.check("分支的历史是原会话那一份的逐字节前缀（拷贝不是重放）",
					rerr == nil && ferr == nil && len(forkBytes) > 0 && bytes.HasPrefix(srcBytes, forkBytes),
					fmt.Sprintf("fork=%dB src=%dB", len(forkBytes), len(srcBytes)))

				forkMsgs, _ := store.LoadMessages(fork.ID)
				srcMsgs, _ := store.LoadMessages(src.ID)
				heldSecond := false
				for _, m := range forkMsgs {
					if strings.Contains(m.Content, "第二轮回复。") {
						heldSecond = true
					}
				}
				c.check("分支止于分叉点之前（第二条回答不在里面）", !heldSecond,
					fmt.Sprintf("分支 %d 条 / 原会话 %d 条", len(forkMsgs), len(srcMsgs)))
				c.check("原会话的记录一条没少", len(srcMsgs) > len(forkMsgs),
					fmt.Sprintf("原 %d 条 / 分支 %d 条", len(srcMsgs), len(forkMsgs)))

				_, fck := os.Stat(filepath.Join(store.BaseDir(), fork.ID, "checkpoints"))
				_, sck := os.Stat(filepath.Join(store.BaseDir(), src.ID, "checkpoints"))
				c.check("分支不拷检查点（原会话那边有，所以这条不是空着通过）",
					os.IsNotExist(fck) && sck == nil,
					fmt.Sprintf("fork err=%v src err=%v", fck, sck))

				_, werr := os.Stat(filepath.Join(c.projectRoot(), "made.txt"))
				c.check("分支没有动工作区（第一轮用 bash 写的文件仍在）", werr == nil, errText(werr))
			},
		},
		//
		//
		// stream-fail: 一轮 turn 的流在**工具跑完之后**断掉时，那一轮已经执行的步骤必须留在对话
		// 历史里（issue #38）。判据不是"界面还看得见那张卡"（那是页面自己的缓存），而是**出错之后的
		// 下一次请求里带着那条工具结果** —— 只有 Go 侧读得到请求，而模型能不能看见它才是这件事的
		// 全部意义：不然模型会把已经做过的事再做一遍。
		{
			name: "stream-fail",
			files: map[string]string{
				"README.md": "# smoke\n\nstream-fail scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// 1. 一轮正常的工具调用：它会被真正执行（mock 的 Bash 写不了盘，但结果里有这个字符串）
				{Reply: bashStream("echo smoke-step-ok", "call_sf1")},
				// 2. 同一个 turn 的下一次调用：流断在这里（不是 ctx 取消，是流本身坏了 —— 正是客户
				//    端超时/连接断掉的样子）。此时这一轮已经执行过一步。
				{Reply: mockllm.MalformedSSE()},
				// 3. 出错之后用户又发了一条：这一轮的请求必须带着上面那一步。
				{Reply: textStream("接着来。", 600)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("三次请求：工具调用 / 断掉的流 / 出错后的下一轮", len(c.requests) == 3, requestCount(c.requests))
				if len(c.requests) < 3 {
					return
				}
				// 前提：断开的那一次（第 2 个请求）里**已经**有那条工具结果 —— 说明这一步确实执行过，
				// 而且它当时就在历史里。没有这一条，下面那条断言会在"什么都没跑"的 fixture 上假绿。
				c.check("前提：断流那一次的请求里已经有这一步的结果",
					requestHasText(c.requests[1], "smoke-step-ok"), requestTail(c.requests[1]))
				// 正题：出错之后的下一轮仍然带着它。
				c.check("断流之后的那一轮把上一轮已执行的步骤带给了模型",
					requestHasText(c.requests[2], "smoke-step-ok"), requestTail(c.requests[2]))
			},
		},
		//
		// api-error-mid-turn: 同一个部位的另一半 —— 失败不是**流**断在半路，而是这次请求本身被拒（400：
		// 请求还没成流就错了）。它走的是 agent loop 里另一条退出路径（CreateChatStream 直接报错），而那条
		// 路径一度把 partial history 当 nil 交出去，理由是"调用根本没开流，没有半轮"—— 这话只对 turn 的
		// **第一次**调用成立。第 2 次调用被拒时，这一轮已经跑过工具，判据与 stream-fail 一字不差：
		// 出错之后的那一轮请求里必须带着那一步。
		{
			name: "api-error-mid-turn",
			files: map[string]string{
				"README.md": "# smoke\n\napi-error-mid-turn scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// 1. 一轮正常的工具调用：它会被真正执行。
				{Reply: bashStream("echo smoke-step-ok", "call_ae1")},
				// 2. 同一个 turn 的下一次调用被拒：HTTP 状态错误，流根本没建立（400 不可重试，
				//    所以脚本的步数是准的）。
				{Reply: mockllm.StatusError(400, "invalid request")},
				// 3. 出错之后用户又发了一条：这一轮的请求必须带着上面那一步。
				{Reply: textStream("接着来。", 600)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("三次请求：工具调用 / 被拒的请求 / 出错后的下一轮", len(c.requests) == 3, requestCount(c.requests))
				if len(c.requests) < 3 {
					return
				}
				// 前提同上：被拒的那一次请求里已经有那条工具结果。
				c.check("前提：被拒的那一次请求里已经有这一步的结果",
					requestHasText(c.requests[1], "smoke-step-ok"), requestTail(c.requests[1]))
				// 正题：出错之后的下一轮仍然带着它。
				c.check("请求被拒之后的那一轮把上一轮已执行的步骤带给了模型",
					requestHasText(c.requests[2], "smoke-step-ok"), requestTail(c.requests[2]))
			},
		},
		//
		//
		// chat-find: ⌘F 搜消息区（issue #35）。两件事只有真机才验得到 —— 搜索的**范围**（用户输入 +
		// 模型正文，不含工具的参数与结果）和**落点**（直接滚到那一条消息）。fixture 里三个 token 各就
		// 各位，数出来的条数就是断言本身。
		{
			name: "chat-find",
			files: map[string]string{
				"README.md": "# smoke\n\nchat-find scenario's working directory\n",
			},
			seedMessages: chatFindFixture(),
		},
		// turn-paging: 一轮 turn 跨了分页边界时，它必须还是**一张**卡片。
		//
		// 转写是分页载入的（PAGE_SIZE 条原始记录一页，「滚到顶再往前取」走另一个 binding），而
		// 每页都各自跑一遍 buildTurns —— 于是一轮跨越分页边界的长回合会变成**每页一张卡片**，
		// 每张都有自己的过程条、以及同一份「本轮改动」（issue #29 的截图正是如此：三张卡、
		// chips 相同）。buildTurns 只在 user 记录处开新卡（steer 也会在中间留一个 user 气泡），
		// 所以「助手贴着助手」只可能是分页缝 —— prependSessionPage 负责把它接回去。
		//
		// 这个 fixture 就是为那条缝造的：第一轮故意长过一页（40 组 assistant+tool_call+
		// tool_result = 121 条），后面两轮的正文很长，好让转写真的能滚、能触发上翻加载。
		{
			name: "turn-paging",
			files: map[string]string{
				"README.md": "# smoke\n\nthe turn-paging scenario's working directory\n",
			},
			seedSecond: longTurnPagesFixture(),
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
		// file-index: 索引里「当前是哪个文件」只有三个以上文件才试得出来 —— 点中间那一条时，上一条的
		// 底边正好落在跳转落点那条线的下方（见 diff.tsx 的 scrollMarginTop 规则），只按「谁的底边还在
		// 头栏下面」判就会亮上一条，正是读者报的那个症状。两个文件的 fixture 抓不到它：第二个就是最后
		// 一个，而「滚到底时最后一个胜出」的兜底会把答案修正回来，看起来是对的。所以这里第三个文件
		// 故意写得很长，好让**中间**那条够得着落点线 —— 只为让这条断言成立。
		{
			name: "file-index",
			files: map[string]string{
				"README.md": "# smoke\n\nthe file-index scenario's working directory\n",
			},
			gitInit: true,
			steps: []mockllm.Step{
				{Reply: writeFileStream(0, "NOTES.md", strings.Repeat("一行笔记：这一行只是为了让 diff 长到需要滚动。\n", 80), "call_x1")},
				{Reply: writeFileStream(0, "OTHER.md", "another file\n", "call_x2")},
				// 排序在最后、而且很长：中间那条（OTHER.md）的跳转落点才落在还能滚到的范围里。
				{Reply: writeFileStream(0, "ZED.md", strings.Repeat("最后一段：同样的填充，只为把可滚动范围撑开。\n", 40), "call_x3")},
				{Reply: textStream("三个文件都写好了。", 600)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("只有一轮对话（三次写文件 + 一次结论）", len(c.requests) == 4, requestCount(c.requests))
			},
		},
		//
		//
		// diff-layout: diff 的 上下 / 左右 两套布局（#34）。这个 fixture 的形状是为左右那一栏挑的：
		// 一段「删三行、加一行」的替换块，问出「谁跟谁并排」（one 对 ONE）与「多出来的一侧留不留空位」
		// （two/three 右边是空格子）；每一行都带同样的长填充，问出第三件 —— 两列各自横滚、同一列一起动
		// （短行没有可滚的余地，程序设 scrollLeft 会被钳回 0，看着就像同步坏了，见 diffLayoutLine）。
		{
			name: "diff-layout",
			files: map[string]string{
				"README.md": strings.Join([]string{
					diffLayoutLine("alpha"), diffLayoutLine("pad"),
					diffLayoutLine("one"), diffLayoutLine("two"), diffLayoutLine("three"), diffLayoutLine("omega"),
				}, "\n") + "\n",
			},
			gitInit: true,
			steps: []mockllm.Step{
				{Reply: writeFileStream(0, "README.md", strings.Join([]string{
					diffLayoutLine("alpha"), diffLayoutLine("pad"), diffLayoutLine("ONE"), diffLayoutLine("omega"),
				}, "\n")+"\n", "call_dl1")},
				{Reply: textStream("改好了。", 600)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("只有一轮对话（一次写文件 + 一次结论）", len(c.requests) == 2, requestCount(c.requests))
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
		// image-reload: 重启后重建的历史里，那条带图的消息必须还是带图的。图片 part 从前只活在内存里的
		// llm.Message 上，落盘的记录只剩一行占位文本，于是任何一次从磁盘重建（重启、--resume、切会话）
		// 都会把图丢掉：provider 的前缀缓存从会话里第一张图起整段失效，模型也再看不见那张图。
		//
		// 现场由 fixture 预置 —— 这正是"重启"的等价物（driver 无法真的重启 app，见 scenario.seedMessages）：
		// 会话记录里躺着一条带 image part 的用户消息，字节内联在记录上、走 store.AppendMessage 落盘，
		// 也就是真实写入的同一条路（图片会被搬进会话目录的 images/，记录只留引用）。driver 再发一句话，
		// 于是这一段重建出来的历史连同新消息一起发给模型。
		//
		// 断言打在**原始 body** 上：归一化视图只保留文本（mockllm 的 normalize 丢掉其余 part），
		// 而这里要证的恰恰是"线路上那张图的字节就是当初发出的那份"。
		{
			name: "image-reload",
			files: map[string]string{
				"README.md": "# smoke\n\nimage-reload scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("图还在。", 800)},
			},
			seedMessages: []session.Message{
				{
					Type:    session.MessageTypeUser,
					Content: "这张图是什么",
					Parts: []session.Part{
						{Type: session.PartTypeText, Text: "这张图是什么"},
						{Type: session.PartTypeImage, MediaType: "image/png", Data: pasteImageB64},
					},
				},
				{Type: session.MessageTypeAssistant, Content: "我看看。"},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("这一轮只有一次请求", len(c.requests) == 1, requestCount(c.requests))
				idx, seen := c.rawSeen("data:image/png;base64")
				c.check("重启后重建的历史里仍带着图片 part", seen && idx == 1,
					fmt.Sprintf("第 %d 次请求的原始 body 里没有 data:image/png;base64", idx))
				_, exact := c.rawSeen(pasteImageB64[:48])
				c.check("带上线的正是预置的那张图（base64 原样）", exact,
					"请求的原始 body 里没有预置图片的 base64 片段")
				// "images" is session/blobs.go's imagesDirName — the session package IS a dependency
				// here, but the blob directory is an implementation detail it does not export.
				blobs, _ := filepath.Glob(filepath.Join(c.home, ".tachi", "session", "*", "images", "*"))
				c.check("图片以 blob 落在会话目录的 images/ 下", len(blobs) == 1, fmt.Sprintf("%v", blobs))
				inline := 0
				for _, f := range sessionFiles(c.home) {
					b, err := os.ReadFile(f)
					if err != nil {
						continue
					}
					if strings.Contains(string(b), pasteImageB64) {
						inline++
					}
				}
				c.check("会话记录里没有内联的 base64（记录只留引用）", inline == 0,
					fmt.Sprintf("%d 个消息文件里有内联 base64", inline))
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
		// roots-branch: the workspace panel names the git branch of each directory it lists, and
		// nothing at all for one that is not a repository. Two checkouts of the same repository are
		// otherwise the same row — the branch is the only thing that tells them apart, and the value
		// itself is pinned by the desktop's own TestGetSessionRootsCarriesTheGitBranch (the driver can
		// read the DOM but not run git, so it asserts that a REAL name is rendered, on the right row).
		{
			name: "roots-branch",
			files: map[string]string{
				"README.md": "# smoke\n\nroots-branch scenario's working directory\n",
			},
			gitInit: true,
			// A root that is NOT a repository: it must come back with no branch, or the panel would
			// dress up "not a repo" as a branch name.
			extraRoots: map[string]map[string]string{
				"plain-lib": {"notes.md": "plain\n"},
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
		// sidebar-width: 会话列表的宽度是读者拖出来的，而且记得住。
		//
		// 三件事在这里：拖右边缘真的改宽度（拖拽期间不走 state —— 见 sidebar.tsx）、宽度被钳在
		// 「对话区至少 560px / 一半」这条线内、以及宽度写进了 desktop_ui.json（Go 侧 after 读回文件）。
		// 键盘（← →）与浮空/折起状态不在这里：前者是同一套 commit 的另一条入口，后者本来就没有宽度。
		{
			name: "sidebar-width",
			files: map[string]string{
				"README.md": "# smoke\n\nthe sidebar-width scenario's working directory\n",
			},
			after: func(c *checkCtx) {
				c.check("设置页/会话列表没有惊动模型", len(c.requests) == 0, requestCount(c.requests))
				raw, err := os.ReadFile(filepath.Join(c.home, ".tachi", "desktop_ui.json"))
				c.check("desktop_ui.json 可读", err == nil, errText(err))
				if err == nil {
					var st struct {
						SidebarWidth int `json:"sidebarWidth"`
					}
					if err := json.Unmarshal(raw, &st); err != nil {
						c.check("desktop_ui.json 可解析", false, errText(err))
					} else {
						c.check("拖出来的宽度落盘了（sidebarWidth）", st.SidebarWidth > 0 && st.SidebarWidth != 280,
							fmt.Sprintf("sidebarWidth=%d；%s", st.SidebarWidth, string(raw)))
					}
				}
			},
		},
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
		// titlebar: 窗口顶部的结构 —— 侧栏通高（窗口控件压在它顶部的空带上），工具栏归内容列，
		// 中间写着当前会话的名字。
		//
		// 让位分两个状态、各由一方出，且只出一份：展开时是侧栏顶部那条带（窗口控件压在侧栏上，
		// 竖着让），折叠时是工具栏的 padding-left（内容列滑到窗口最左，横着让）。让多少由平台
		// 决定（有控件的平台是那三个按钮的宽，没有的平台上窗口干脆无边框，那条带自己就是窗口
		// 的把手）。设置页则要求盖满窗口、左栏第一条控件同样让开。交接动画的中间帧断言不了：
		// smoke 的窗口被盖住，WebKit 在不做过渡的窗口里会把两个值直接落到终点（见 driver 的说明）。
		{
			name: "titlebar",
			files: map[string]string{
				"README.md": "# smoke\n\nthe titlebar scenario's working directory\n",
			},
		},
		//
		// font-size: 消息区的阅读字号（⌘= / ⌘- / ⌘0）。
		//
		// 一半在 driver 里（键真的改字号、标题按比例跟着、时间戳不动、localStorage 落值），另一半是
		// Go 侧读 desktop_ui.json —— 键与文件是两份存储，各读各的才叫都验过。
		//
		// driver 证明不了「原生菜单不再抢这三个键」：合成 KeyboardEvent 直接进页面，不经过菜单栏，
		// 那条守卫是 desktop/menu_test.go。
		{
			name: "font-size",
			files: map[string]string{
				"README.md": "# smoke\n\nfont-size scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// 正文 + 一个标题：标题要能按 em 跟着正文走（固定 px 的标题会在放大后沉到正文下面）。
				{Reply: textStream("字号标尺：正文一行。\n\n## 字号标尺小标题\n\n再来一行正文。", 1500)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("只有驱动发的那一条消息惊动了模型", len(c.requests) == 1, requestCount(c.requests))
				raw, err := os.ReadFile(filepath.Join(c.home, ".tachi", "desktop_ui.json"))
				c.check("desktop_ui.json 可读", err == nil, errText(err))
				if err == nil {
					var st struct {
						ReadingFontScale int `json:"readingFontScale"`
					}
					if err := json.Unmarshal(raw, &st); err != nil {
						c.check("desktop_ui.json 可解析", false, errText(err))
					} else {
						c.check("字号落盘了（readingFontScale=120）", st.ReadingFontScale == 120,
							fmt.Sprintf("readingFontScale=%d；%s", st.ReadingFontScale, string(raw)))
					}
				}
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
		//
		// mcp-auth：一个 OAuth2 保护的 HTTP MCP server，从「需要授权」走到「已连接 + 工具可见」。
		//
		// 这条链路横跨三个进程/协议：面板（前端）→ agent/mcp 的 OAuth 流程（DCR、PKCE、回调、
		// 换 token）→ 受保护的 MCP 端点，而授权本身是一次「打开浏览器」的动作。fixture
		// （itest/mcpauth）在 runner 里起，mcp.json 按它的随机端口写进沙箱；浏览器被
		// TACHI_DEMO_BROWSER_FETCH 换成 headless GET —— smoke 永远不该在别人的机器上弹出窗口。
		//
		// 它必须存在的原因：这条链路上没有一半在进程内可见。单测钉住 Go 的每一步，driver 钉住
		// 「后端说了、前端画了没有」—— 而这个功能之前的形状正是流程齐全却没人能触发。
		{
			name:     "mcp-auth",
			mcpOAuth: true,
			files: map[string]string{
				"README.md": "# smoke\n\nthe mcp-auth scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("收到。", 400)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				if c.mcp == nil {
					c.check("fixture 已启动", false, "mcp 为 nil —— 场景没声明 mcpOAuth")
					return
				}
				// 授权真的发生过，而不是"面板显示连上了"：客户端注册了 client_id、按 PKCE
				// 授权、并把 code 换成了 token。三样缺一，界面都不可能到那一步。
				regs := c.mcp.Registrations()
				c.check("动态客户端注册发生了", len(regs) == 1, strconv.Itoa(len(regs))+" 次注册")
				if len(regs) == 1 {
					c.check("注册带着回调地址（redirect_uri 不是空串）",
						len(regs[0].RedirectURIs) == 1 && regs[0].RedirectURIs[0] != "",
						regs[0].Raw)
				}
				auths := c.mcp.Authorizations()
				c.check("授权请求发起了", len(auths) == 1, strconv.Itoa(len(auths))+" 次授权")
				if len(auths) == 1 {
					c.check("授权用了 PKCE (S256)", auths[0].ChallengeMethod == "S256", auths[0].ChallengeMethod)
				}
				tokens := c.mcp.TokenRequestsDone()
				c.check("code 换到了 token", len(tokens) == 1, strconv.Itoa(len(tokens))+" 次换 token")
				if len(tokens) == 1 {
					c.check("换 token 带上了 code_verifier", tokens[0].CodeVerifier != "", "verifier 为空")
				}
				// 工具进了共享池，模型也必须被告知：reminder 一次会话只发一次，而这条 server
				// 是启动之后才连上的 —— 不通知就等于模型永远不知道它存在。driver 负责发一轮，
				// 这里在 LLM 边界上断言提示确实到了。
				at, announced := c.requestSeen("mcp__oauth-fixture__echo")
				c.check("授权后模型被告知新工具存在", announced, "命中第 "+strconv.Itoa(at)+" 条请求")
				c.check("提示只出现在授权之后的那一轮", announced && at == 1, requestCount(c.requests))
				c.check("只有一轮对话（driver 只发了授权之后那一条）", len(c.requests) == 1, requestCount(c.requests))

				// The token really landed under THIS sandbox's HOME, not the real ~/.tachi.
				dir := filepath.Join(c.home, ".tachi", "mcp_tokens")
				entries, _ := os.ReadDir(dir)
				c.check("token 落在沙箱 HOME 的 mcp_tokens 下", len(entries) > 0, dir)

				// 面板的「禁用」写回了 mcp.json —— 而且是它来自的那个文件、只动了那一个值。
				// driver 关掉 server 之后这里才读得到：文件是"结果"，不是"意图"。
				mcpJSON := filepath.Join(c.home, ".tachi", "mcp.json")
				raw, err := os.ReadFile(mcpJSON)
				c.check("mcp.json 可读", err == nil, errText(err))
				if err == nil {
					c.check("禁用写回了 enabled: false", strings.Contains(string(raw), `"enabled": false`), string(raw))
					c.check("写回没有动其它字段（fixture 的 timeout 与 name 还在）",
						strings.Contains(string(raw), `"timeout": "30s"`) && strings.Contains(string(raw), "oauth-fixture"),
						string(raw))
					// 逐字节证明"只加了一行"：把插入的那一行从结果里摘掉，必须还原成 fixture
					// 写下的原文。比"开头/结尾长什么样"硬 —— 它不依赖任何格式假设，而且是
					// 这份文件里唯一能区分"外科式编辑"与"整份重写"的判据。
					inserted := ",\n      \"enabled\": false"
					at := strings.Index(string(raw), inserted)
					c.check("写回 = 原文件 + 恰好一行 enabled（其余字节未动）",
						at > 0 && string(raw)[:at]+string(raw)[at+len(inserted):] == c.mcpConfigBefore,
						string(raw))
				}
			},
		},
		//
		//
		// settings: 设置页 —— 一个纯渲染器画出来的东西，以及一次保存真的走到了文件。
		//
		// 这一页把 GetSettingsSchema / settingsTable 的每一条声明变成控件，所以单测（Go）能钉住声明、
		// 却看不见"声明有没有到界面"：分组与顺序、配置列表折成的卡片叫什么、下拉里有哪些值。
		// 反过来说，driver 能看见界面、却看不见磁盘；所以**一次编辑的落点由 Go 侧读 config.yaml 断言**
		// （并且真的用 config.LoadFrom 装载一遍 —— 界面写出的文件必须是装载器认得的）。
		//
		// 两条最容易默默坏掉的路在这里：
		//   · 路径不当标签（`providers.0.model` 不该出现在表单里，卡片头说的是"哪个 provider"）；
		//   · 「自动」= 未设置，落到文件里是那个键**不出现**（可选叶子是 omitempty）—— 唯一一个
		//     "没有值"也有表达的状态（config.Unset；裸 nil 是被拒绝的，那才是"忘了解引用指针"
		//     的样子）。
		//
		// 还有一条只属于"保存之后"的路，它也不在 Go 侧：保存能改变的**不止这一页**，而配置列表的
		// 拷贝还有一个消费者在对话那一列（工具条上的模型选择器）。driver 读那个下拉自己 —— 一次
		// 保存加出来的 provider 必须当场出现在里面，而不是等到下次启动。
		{
			name: "settings",
			files: map[string]string{
				"README.md": "# smoke\n\nsettings scenario's working directory\n",
			},
			// title_provider 故意**不**写进 fixture：这条保存要证明的正是"文件里没有的键也能存下去"
			// —— 保存写的是**整份配置**（config.Save），所以文件里有没有这个键都无所谓，driver 把它
			// 改成 smoke，Go 侧再从文件里读回来。
			//
			// 嵌套块（provider 的 `spec:` 缺整块）那一路由单测钉住：共享的 provider fixture 必须
			// 带着 spec（mock 的 context_window 是别的场景要用的），从界面侧腾不出那个位置。
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				// 设置页不该惊动任何会话。
				c.check("设置页没有触发任何对话", len(c.requests) == 0, requestCount(c.requests))

				path := filepath.Join(c.home, ".tachi", "config.yaml")
				raw, err := os.ReadFile(path)
				c.check("config.yaml 可读", err == nil, errText(err))
				if err != nil {
					return
				}
				cfg, err := config.LoadFrom(path)
				c.check("界面写出的文件能被装载", err == nil, errText(err))
				if err != nil {
					return
				}
				c.check("保存写进了文件：title_provider = smoke", cfg.TitleProvider == "smoke",
					"title_provider="+cfg.TitleProvider)
				if len(cfg.Providers) == 0 {
					c.check("fixture 至少有一个 provider", false, "providers 为空")
					return
				}
				// 未设置的那一态：文件里**没有这个键**（可选叶子是 omitempty），装载回来是 nil
				// —— 两边都得对，否则界面上的「自动」与 Tachi 实际的行为就是两回事。
				// 这一条落在**新加的那个 provider** 上：它是界面造出来的条目，界面上把它的
				// spec.vision 选成「自动」再保存，那个键就该从文件里消失。
				c.check("「自动」把 vision 写成未设置（键不出现，回读为 nil）",
					cfg.Providers[0].Spec.Vision == nil, string(raw))
				c.check("文件里没有 vision 这个键",
					!strings.Contains(string(raw), "vision"), string(raw))

				// 列表的增删：这一页**不是立刻写盘**的那一套 —— ＋ 与删除都只是页面自己的待办，
				// 保存时和字段值一起写进同一份配置（见 config.ListEdits）。所以这里断言的是**保存之后**
				// 的文件：新条目在（带着从界面填进去的值），删掉的那一项不在，而那个全新列表也一起出现了
				// （沙箱配置里根本没有 web_search 这个键）。
				names := make([]string, 0, len(cfg.Providers))
				for _, p := range cfg.Providers {
					names = append(names, p.Name)
				}
				c.check("保存把界面上「＋」的条目写进了文件（带着填进去的值）",
					len(cfg.Providers) == 1 && names[0] == "added-by-smoke" && cfg.Providers[0].Model == "added-model",
					fmt.Sprintf("providers=%v", names))
				c.check("保存把界面上删掉的那一项也写进了文件（原来那个 provider 不在了）",
					!slices.Contains(names, "smoke"), fmt.Sprintf("providers=%v", names))
				c.check("文件里没有 web_search 块时，保存把它和条目一起建了出来",
					len(cfg.WebSearch.Providers) == 1 && cfg.WebSearch.Providers[0].Type == config.WebSearchProviderExa,
					fmt.Sprintf("web_search.providers=%+v", cfg.WebSearch.Providers))
				// 预览是只读的：driver 最后在设置页里**改过又没保存**就离开了（预览过、又改回
				// smoke、再收起、再返回），所以这里读到的必须还是上一次保存留下的样子。这一条
				// 只有 Go 侧看得见（页面看不见磁盘），而它抓的正是"预览顺手写了一次盘"这种错。
				c.check("预览（以及那之后没保存的改动）没有写盘：title_provider 还是上次保存的 smoke",
					cfg.TitleProvider == "smoke", "title_provider="+cfg.TitleProvider)
				// 同一件事的第二个证人：driver 最后把 language 改成 en 又改成 fr（两次都预览过），
				// 文件里必须还是 fixture 写下的 zh。页面上"预览过、改过、没保存"的那一路只要碰过
				// 磁盘，这两条里至少有一条会红。
				c.check("预览没有写盘：language 还是 fixture 的 zh",
					cfg.Language == "zh", "language="+cfg.Language)
				// 头一次保存已经把这个文件变成了本进程写出来的样子，所以 driver 后半段那次预览
				// 不该再报「会被整份重写」——而它前面那次（手写文件）必须报。两条都在 driver 里，
				// 这里补的是"文件确实已经被重写过一次"这个前提：注释没了。
				c.check("第一次保存之后，config.yaml 里已经没有手写注释（文件被整份重写过）",
					!strings.Contains(string(raw), "# vision:"), string(raw)[:200])
			},
		},
		//
		//
		// tray-new-session: 状态栏图标（菜单栏）的「新会话」。图标本身 driver 碰不到（Go 侧建的，
		// 不在 DOM 里），所以两边各补一半：driver 发那条 `ui:new_session` 事件并断言界面上真的多了
		// 一个会话，这里断言磁盘上真的多了一个、而且它和眼前那个一样**绑在项目上** —— NewSession
		// 在项目不能驱动时会静默建一个不属于任何项目的会话，只数行数看不出来，meta.json 的
		// project_id 才是判据。
		{
			name: "tray-new-session",
			files: map[string]string{
				"README.md": "# smoke\n\nthe tray-new-session scenario's working directory\n",
			},
			project: &projectSeed{
				name: "smoke-tray",
				files: map[string]string{
					"README.md": "# smoke\n\nthe tray-new-session scenario's project directory\n",
				},
			},
			after: func(c *checkCtx) {
				c.check("菜单栏那次新建没有向模型发过任何请求", len(c.requests) == 0, requestCount(c.requests))
				metas := c.sessionMetas()
				c.check("磁盘上多了一个会话（fixture 的两个 + 菜单栏建的）", len(metas) == 3,
					fmt.Sprintf("%d 个", len(metas)))
				bound := 0
				for _, m := range metas {
					// meta.json 里这个字段是 project_id（session.Session 的 json tag），不是绑定层
					// 那个 camelCase 的 projectId —— 前者才是磁盘上的事实。
					if m["project_id"] == "smoke-project" {
						bound++
					}
				}
				c.check("新会话和眼前那个一样绑在项目上（meta.json 的 project_id）", bound == 2,
					fmt.Sprintf("project_id=smoke-project 的会话有 %d 个", bound))
			},
		},
		//
		//
		// shortcut-sheet: ⌘? 那张快捷键表。键位一路加进来，表长到一屏放不下之后，唯一还要紧的
		// 布局事实是「行能滚、标题和关闭键不跟着滚走」—— 判据全是几何（框在窗口里、行区自己有
		// 滚动条、滚到底最后一行看得见而关闭键不动），driver 里量。Go 侧只断一件事：看表不该
		// 惊动模型（这条场景一个请求都不该有）。
		{
			name: "shortcut-sheet",
			files: map[string]string{
				"README.md": "# smoke\n\nthe shortcut-sheet scenario's working directory\n",
			},
			steps: []mockllm.Step{
				// 不会被调用：driver 只开表、滚表、关表，一个字都没发。它的存在只是让 mock
				// 是一份完整的脚本，而不是空脚本。
				{Reply: textStream("不应该被调用。", 300)},
			},
			after: func(c *checkCtx) {
				c.check("看快捷键表没有向模型发过任何请求", len(c.requests) == 0, requestCount(c.requests))
			},
		},
		//
		//
		// session-keys: 作用于「眼前那个会话」的键位 —— ⌘N / ⌘⇧N（新建）与 ⌘⇧⌫（删除）。前两个
		// 只差一个 shift，界面上的差别是新行落在哪个组，磁盘上的差别是 project_id —— 组是前端
		// 算的，绑没绑项目要读文件，所以两头都验。第三件事是「定位」：新建的行必须滚到眼前，
		// driver 先把列表撑过一屏再滚到底部，这一条才不是白给的。⌘⇧⌫ 只开确认框（Esc 取消什么
		// 都不少，Enter 才真删），所以 Go 侧看到的是删完之后的终态：⌘N 建的那个绑着项目的会话
		// 没了，只剩 fixture 那一个（见 drivers/session-keys.js）。
		{
			name: "session-keys",
			files: map[string]string{
				"README.md": "# smoke\n\nthe session-keys scenario's working directory\n",
			},
			project: &projectSeed{
				name: "smoke-keys",
				files: map[string]string{
					"README.md": "# smoke\n\nthe session-keys scenario's project directory\n",
				},
			},
			after: func(c *checkCtx) {
				// 建会话、删会话都不惊动模型：这条场景里没有一条消息，任何请求都说明键位自己发了东西。
				c.check("建会话/删会话都没有向模型发过任何请求", len(c.requests) == 0, requestCount(c.requests))
				metas := c.sessionMetas()
				bound := 0
				var newestAt time.Time
				newestBound := ""
				for _, m := range metas {
					// 磁盘上的字段名是 project_id（session.Session 的 json tag）；没绑项目时
					// omitempty 让它整个不出现。
					pid, _ := m["project_id"].(string)
					if pid == "smoke-project" {
						bound++
					}
					if ts, ok := m["created_at"].(string); ok {
						if t, err := time.Parse(time.RFC3339, ts); err == nil && t.After(newestAt) {
							newestAt, newestBound = t, pid
						}
					}
				}
				// 只剩 fixture 那个绑在项目上：driver 全程建的会话（侧栏 ＋ 与 ⌘⇧N）都不该绑，而
				// ⌘N 建的那个（唯一该绑的）被 ⌘⇧⌫ 删掉了 —— 数字还多一个，说明删除没落到磁盘上。
				c.check("⌘N 建的那个已被删掉、只剩 fixture 的一个绑在项目上",
					bound == 1, fmt.Sprintf("project_id=smoke-project 的会话有 %d 个（共 %d 个会话）", bound, len(metas)))
				// 最新的那个现在是 ⌘⇧N 建的（无项目）：⌘N 建的那个曾经是最新且绑着项目的，它若还
				// 在，这里就是 "smoke-project"。
				c.check("磁盘上最新的会话不是 ⌘N 建的那个（它被删了）", newestBound != "smoke-project",
					"最新会话的 project_id="+strconv.Quote(newestBound))
			},
		},
		//
		//
		// mcp-edit: 设置页里的 MCP server 编辑器 —— 这一页唯一一个**不是 config.yaml 叶子**的区块。
		//
		// 行为上和页面其余部分**不一样**，正是这一点值得端到端跑：它不经过 SaveSettings，每条自己的
		// 「保存」直接写 mcp.json 并立即装进运行中的进程。单测能钉住「写文件 + 装 config」，看不到的是
		// **声明有没有送到界面**：区块画没画在 MCP 分组里、列表是不是文件里那几条、来源标签写的是哪个
		// 文件、以及那句「这一块立即生效」在不在。
		//
		// 判据按 settings 那条的规矩分两边：driver 只看界面（新条目出现在列表里、来源标签对），Go 侧
		// 只读磁盘（fixture 那条逐字节没动、新条目在、这份文件是装载器认得的形状）。
		{
			name: "mcp-edit",
			files: map[string]string{
				"README.md": "# smoke\n\nmcp-edit scenario's working directory\n",
			},
			// DISABLED on purpose: a smoke run happens on a real machine, and an enabled stdio
			// server is a process the app really spawns (`/bin/echo` would not hurt, but the rule
			// is the rule). The editor still has a shared manager to install into, because the
			// config was non-empty at startup — which is the branch worth exercising.
			mcpServersJSON: mcpEditSeed,
			after: func(c *checkCtx) {
				c.check("设置页不该惊动任何会话：mock 没有收到请求", len(c.requests) == 0, requestCount(c.requests))

				raw, err := os.ReadFile(filepath.Join(c.home, ".tachi", "mcp.json"))
				c.check("mcp.json 可读", err == nil, errText(err))
				if err != nil {
					return
				}
				// 原有那条**逐字节没动**：编辑器只重写它改的那一条，文件其余部分是用户写的。
				c.check("fixture 里那条 server 逐字节没动", strings.Contains(string(raw), mcpEditSeedEntry),
					string(raw))
				// 而且写出来的必须是**装载器读得回来的**形状（不是"能被这个包恰好吃下"）。
				var parsed struct {
					Servers []config.MCPServerConfig `json:"servers"`
				}
				if err := json.Unmarshal(raw, &parsed); err != nil {
					c.check("mcp.json 能被解析成 config 的 server 列表", false, errText(err))
					return
				}
				c.check("mcp.json 能被解析成 config 的 server 列表", true, "")
				byName := map[string]config.MCPServerConfig{}
				for _, s := range parsed.Servers {
					byName[s.Name] = s
				}
				names := make([]string, 0, len(byName))
				for name := range byName {
					names = append(names, name)
				}
				slices.Sort(names)
				c.check("原有那条还在（没有被新条目顶掉）", len(byName) == 2, fileList(names))
				added, ok := byName["smoke-added"]
				c.check("界面上加的那条写进了文件", ok, fileList(names))
				if ok {
					c.check("带上了界面上填的 command", added.Command == "/bin/true", added.Command)
					// 界面上取消勾选必须落成 `enabled: false` —— 这个键**不出现**才是启用，所以「不启用」
					// 只有写下来才表达得出来（和面板开关写回文件的道理一样）。
					enabled := "<nil>（没写下来 = 启用）"
					if added.Enabled != nil {
						enabled = strconv.FormatBool(*added.Enabled)
					}
					c.check("界面上取消勾选落成了 enabled: false", added.Enabled != nil && !*added.Enabled, enabled)
				}
			},
		},
		//
		//
		// external-link: 消息区里的链接点下去必须走**系统浏览器**，而不是让 webview 自己导航走。
		//
		// webview 没有标签页、没有后退键（窗口就是 app 本身），所以旧行为是「点一下，整个界面被那个网页
		// 换掉，而且回不来」（issue #48）。修法是一条路：响应里的 <a> 由前端的 MdLink 接管
		// （preventDefault + AgentService.OpenURL），后端只放行 http/https/mailto。
		//
		// 断言分两半。driver 侧：链接是个带地址的真控制，点完应用还在原地 —— 真被导航走的话，那个页面
		// 整个换掉，driver 连下一行断言都跑不到。Go 侧：那次点击确实把**原样的地址**交给了系统启动器。
		// 启动器在脚本运行里被换成记录器（desktop/demo.go 的 TACHI_DEMO_OPEN_LOG），所以 smoke 绝不会
		// 在别人的机器上弹出浏览器窗口，而记下来的 argv 就是「本来会被打开的东西」。
		{
			name: "external-link",
			files: map[string]string{
				"README.md": "# smoke\n\nexternal-link scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("看这个：[示例]("+externalLinkURL+") 里面有详情。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("这一轮只有一次请求", len(c.requests) == 1, requestCount(c.requests))
				// 不是「文件里含有这个 URL」，而是「启动器收到的就是它」：多一个字少一个字都算错，
				// 而多出来的启动调用（比如顺手把整个会话目录也打开了）同样是失败。
				c.check("点链接把地址原样交给了系统启动器",
					len(c.opened) == 1 && c.opened[0] == externalLinkURL, fileList(c.opened))
			},
		},
		//
		// file-link: 回复里指向**本地文件**的链接点下去要开应用内的预览，而不是交给系统浏览器
		// （issue #49）。它是 external-link 的另一半：同一个手势、同一个组件，只是目标在磁盘上。
		//
		// 相对路径是这条链路的要害：回复里的链接和正文里的图片一样，相对**会话工作目录**解析
		// （lib.ts 的 linkTargetPath / toLocalAsset 是并列的两条），而 workDir 得真的传到链接
		// 渲染器上 —— 传不到时这一条会退化成「点了没反应」，测试正是钉住这一点。
		//
		// 「是不是文件」这个判断由后端的 stat 给出，不是前端照形状猜的：回复里确实存在
		// `[state](/concepts/state)` 这种站点绝对路径，和绝对文件路径长得一样。于是第二个链接
		// 只有路径不存在这一种差别：它按老行为落到 OpenURL，而后端只放行 http/https/mailto，
		// 所以 driver 断「没有浮层」，Go 侧断「一次启动器调用都没有」。
		{
			name: "file-link",
			files: map[string]string{
				"README.md": "# smoke\n\n" + fileLinkMarker + "\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("看这个文件：[README.md](./README.md)，还有 [没有这个](nope/missing.md)。", 900)},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("这一轮只有一次请求", len(c.requests) == 1, requestCount(c.requests))
				// 浮层那半在 driver 里断。这里断的是它**没有**要系统去开任何东西：本地文件链接
				// 一旦走回浏览器那条路，这里就会多出一条（哪怕后端最后拒绝了它，被记录下来的
				// 也只会是放行的 scheme，所以多出来的必然是真被开的东西）。
				c.check("点本地文件链接不碰系统启动器", len(c.opened) == 0, fileList(c.opened))
			},
		},
		//
		//
		// orphan-tool-call: 进程死在工具批次中间之后，重启加载出来的历史里会留下**只配对了一半**的
		// tool call —— assistant 一次发两条 call，落盘顺序是「先把 call 全记下，再逐个记 result」
		// （agent/tool_executor.go），所以 kill 在第二条跑完之前就只剩一条 result。
		//
		// provider 会直接拒掉这种请求（每个 tool_use 都要有 tool_result），而旧的全有全无式清理只看
		// 「这条之后有没有任意一条 tool」，有一条就整批留下 —— 于是孤儿 call 跟着会话一路走到最后，
		// 除非有人手改记录。desktop 只是最容易重启到这条路径的入口（record 从磁盘重建）。
		//
		// 现场由 fixture 预置（预置 = 重启的等价物）：一条 user + thinking + 两条 Bash call + 只有
		// 第一条 call 的 result。断言打在**模型实际收到的请求**上：孤儿 call 不在，配过对的那条还在
		// （少了后者，这条断言在「清理把手里的 call 全删了」时也会绿）。
		{
			name: "orphan-tool-call",
			files: map[string]string{
				"README.md": "# smoke\n\norphan-tool-call scenario's working directory\n",
			},
			steps: []mockllm.Step{
				{Reply: textStream("好，接着来。", 900)},
			},
			seedMessages: []session.Message{
				{Type: session.MessageTypeUser, Content: "两条都跑一下"},
				{Type: session.MessageTypeThinking, Content: "一起发两条命令", Signature: "sig-orphan"},
				{Type: session.MessageTypeToolCall, Name: "Bash", Args: `{"command": "git status"}`,
					ToolCallID: orphanAnsweredCallID},
				{Type: session.MessageTypeToolCall, Name: "Bash", Args: `{"command": "git diff"}`,
					ToolCallID: orphanCallID},
				{Type: session.MessageTypeToolResult, Name: "Bash", Result: "nothing to commit",
					ToolCallID: orphanAnsweredCallID},
			},
			after: func(c *checkCtx) {
				c.check("mock 脚本跑完且没有多余/缺失的请求", c.mockErr == nil, errText(c.mockErr))
				c.check("这一轮只有一次请求", len(c.requests) == 1, requestCount(c.requests))
				if len(c.requests) == 0 {
					return
				}
				req := c.requests[0]
				c.check("没有结果的 tool call 没有跟着上线", !requestHasToolCall(req, orphanCallID),
					requestTail(req))
				c.check("配过对的那条 call 还在（不是把两条一起删了）", requestHasToolCall(req, orphanAnsweredCallID),
					requestTail(req))
				c.check("它的 result 也还在", requestHasToolResult(req, orphanAnsweredCallID), requestTail(req))
			},
		},
		// managed-session: 受管会话在桌面上的样子 —— 侧栏行带来源标注，打开只能看，输入框换成一句原因。
		//
		// 这条 fixture 只能预置：kind 写在会话记录里，driver 写不了记录，而它必须是**启动时恢复的
		// 那一个**会话才说明问题 —— 重启后直接落在一条子系统的执行记录上，正是这个形状最常发生的
		// 时刻（节点的会话就在这个数据目录里）。另一半 ——「驱动它会被 agent 入口拒掉」——由 node/
		// 与 agent/ 的单测钉住；这里验的是桌面没有摆出一个点了必然失败的输入框。
		//
		// 断言几乎全在界面上；Go 侧只钉一件事：这一轮一次模型都没调过 —— 只读会话不该产生任何请求
		// （mock 有脚本就会有请求，所以脚本是空的：来了就是失败）。
		{
			name:                "managed-session",
			sessionKind:         session.KindA2AExecution,
			sessionSourceNodeID: smokeSourceNodeID,
			files: map[string]string{
				"README.md": "# smoke\n\nmanaged-session scenario's working directory\n",
			},
			seedMessages: []session.Message{
				{Type: session.MessageTypeUser, Content: "检查昨天的备份"},
				{Type: session.MessageTypeAssistant, Content: "发现两个目录未完成备份。"},
			},
			after: func(c *checkCtx) {
				c.check("只读会话没有产生任何模型请求", len(c.requests) == 0, requestCount(c.requests))
				c.check("mock 脚本没被消耗", c.mockErr == nil, errText(c.mockErr))
			},
		},
		// managed-session-busy: the same session WITH a running execution holding it. That hold is
		// what refuses a delete, and the window's answer to it is a button in the banner — this
		// scenario is the one that pins the button, its absence in the scenario above, and the
		// honest answer the backend gives when there is no node service to stop (the sandbox has
		// none). Clicking it here is therefore a REAL round trip that fails in a known way: a test
		// that only checked the button's presence would pass with the binding mis-wired.
		{
			name:                "managed-session-busy",
			sessionKind:         session.KindA2AExecution,
			sessionSourceNodeID: smokeSourceNodeID,
			sessionHeldByTask:   true,
			files: map[string]string{
				"README.md": "# smoke\n\nmanaged-session-busy scenario's working directory\n",
			},
			seedMessages: []session.Message{
				{Type: session.MessageTypeUser, Content: "检查昨天的备份"},
				{Type: session.MessageTypeAssistant, Content: "发现两个目录未完成备份。"},
			},
			after: func(c *checkCtx) {
				c.check("只读会话没有产生任何模型请求", len(c.requests) == 0, requestCount(c.requests))
			},
		},
	}
}

// mcpEditSeed is the mcp.json the mcp-edit scenario starts from. The server is DISABLED: a smoke
// run happens on a real machine, and an enabled stdio server is a process the app really spawns.
// smokePNG is a real 1×1 PNG on disk (decoded from the same bytes paste-imageBase64 carries): the
// file-tree scenario's image row has to be an image the webview can actually render, or 「点开图片」 would
// be asserting on a broken <img> — and a text file named .png is exactly that.
var smokePNG = func() string {
	raw, err := base64.StdEncoding.DecodeString(pasteImageB64)
	if err != nil {
		panic("smokePNG: " + err.Error())
	}
	return string(raw)
}()

// longMarkdown is a document tall enough to overflow the file view's reading area, for the scenario
// that asserts the box is a real SCROLLER (a short file cannot tell a working one from a broken one).
func longMarkdown(sections int) string {
	var b strings.Builder
	b.WriteString("# 长文档\n\n")
	for i := 1; i <= sections; i++ {
		fmt.Fprintf(&b, "## 第 %d 节\n\n这是第 %d 节的一段普通文字，用来把这份文档撑得比阅读区高。\n\n", i, i)
	}
	return b.String()
}

// keysOfMemo lists the workspaces a desktop_ui.json memo table holds, for a failure message that says
// what WAS there instead of only that the expected key was missing.
func keysOfMemo(memo map[string]struct {
	Dirs []string `json:"dirs"`
	File string   `json:"file"`
}) []string {
	out := make([]string, 0, len(memo))
	for key := range memo {
		out = append(out, key)
	}
	return out
}

const mcpEditSeed = `{
  "servers": [
    {
      "name": "echo-fixture",
      "type": "stdio",
      "command": "/bin/echo",
      "args": ["smoke"],
      "enabled": false
    }
  ]
}
`

// mcpEditSeedEntry is the part of that file an unrelated edit must leave alone, byte for byte —
// and a substring of mcpEditSeed by construction, so reformatting the fixture above makes the
// assertion below fail loudly instead of passing against a stale copy.
//
// It is the ENTRY rather than the whole file on purpose: appending a server writes a comma and a
// new entry into the array, so "the file is unchanged" is false by definition. What the file-format
// promise actually says is that the bytes of the entries nobody named come back untouched.
const mcpEditSeedEntry = `    {
      "name": "echo-fixture",
      "type": "stdio",
      "command": "/bin/echo",
      "args": ["smoke"],
      "enabled": false
    }`

// externalLinkURL is the address the external-link scenario's reply links to. The DRIVER spells it
// too (drivers/external-link.js reads it off the anchor) — a driver cannot read a Go constant — so
// the two must stay identical, exactly like pasteImageB64 below.
//
// It points at example.com because nothing ever fetches it: the app records the launcher call
// instead of performing it (TACHI_DEMO_OPEN_LOG), which is what keeps a smoke run from opening a
// browser window on the machine it runs on.
const externalLinkURL = "https://example.com/tachi-link-smoke"

// fileLinkMarker is a sentence inside the file-link scenario's README.md. The DRIVER spells it too
// (drivers/file-link.js) — a driver cannot read a Go constant — so the two must stay identical, the
// same convention as externalLinkURL and pasteImageB64 above.
//
// It exists to tell "the overlay opened" from "the overlay opened ON THIS FILE": an empty frame is
// also a .viewer-overlay, and an overlay showing yesterday's file would satisfy every other check.
const fileLinkMarker = "file-link smoke marker"

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
