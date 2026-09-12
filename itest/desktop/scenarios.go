package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
		// Session-scoped numbers: a brand-new session must not inherit the previous one's
		// cache ring or cost (that bug is in .tachi.md's list), and the sidebar row must pick
		// up the generated title from the session_title event.
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

