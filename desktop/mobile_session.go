package main

import (
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// 手机读的那一半：列表、一页历史、一份快照、一份附件清单。
//
// 这里没有一条路径会改变桌面正在显示的会话（design 不变量 1）：它们只读记录与本进程的
// run 表，不碰 activeID，也不发状态事件。

// mobileNoSession 是协议里的 no_session（手机显示「这段对话已经不在了」）。
var errMobileNoSession = fmt.Errorf("no_session")

// mobilePageLimit is the default page size for the phone. It matches the mobile client's own
// default (/v1/sessions/{id}/page?limit=50) so a client that omits the parameter still gets what
// it asked for.
const mobilePageLimit = 50

// mobileSnapshotLimit is how much history one snapshot carries. Older messages are paged in
// over HTTP; the snapshot only needs enough to paint the screen.
const mobileSnapshotLimit = 100

// mobileSessions returns the list the phone shows: one row per CONVERSATION, which means a
// compaction chain folds to its leaf. Folding happens here, once — the phone never rebuilds a
// chain (a second definition of "which row is this conversation" is exactly what drifts).
func (d *desktopApp) mobileSessions() ([]mobileSessionSummary, error) {
	if d.sm == nil {
		return nil, fmt.Errorf("no session manager")
	}
	list, err := d.sm.List()
	if err != nil {
		return nil, err
	}
	// A session named as another's compacted parent is the OLD half of a conversation: the
	// child is the same conversation continued, and both would render as two identical rows.
	parents := make(map[string]bool, len(list))
	for _, ss := range list {
		if ss.CompactedParentID != "" {
			parents[ss.CompactedParentID] = true
		}
	}
	busy := d.busySessions()
	// 一次扫描算完整个列表：定时任务的条数是**那段对话**的属性（逻辑 id），不是某一页的
	crons := d.cronFactsByLogical()
	out := make([]mobileSessionSummary, 0, len(list))
	for _, ss := range list {
		if parents[ss.ID] {
			continue
		}
		out = append(out, d.mobileSummaryWith(ss, busy, crons))
	}
	// Pinned first, then newest — the same order the desktop sidebar gives a group, and the
	// order the protocol asks for.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out, nil
}

// mobileSummary renders one row. running / waiting / heldByTask all come from the SAME sources
// the sidebar reads (sessionRunning, waitingSentence, the retention record): a phone that
// disagreed with the window about "who is waiting" would be worse than no phone at all.
func (d *desktopApp) mobileSummary(ss *session.Session, busy map[string]bool) mobileSessionSummary {
	// 单行调用方（快照 / 翻页 / 新建）各扫一遍就够了；列表那条路走 mobileSummaryWith，只扫一次
	return d.mobileSummaryWith(ss, busy, d.cronFactsByLogical())
}

// mobileSummaryWith 是列表那份：`crons` 由调用方扫一次传进来（每行各扫一遍 crons.json 是
// 白花的）。
func (d *desktopApp) mobileSummaryWith(ss *session.Session, busy map[string]bool, crons map[string]cronFacts) mobileSessionSummary {
	running, _ := d.sessionRunning(ss.ID)
	waiting := ""
	if running {
		waiting = d.waitingSentence(ss.ID, d.stateFor(ss.ID))
	}
	project := ""
	if p, ok := d.projects.get(ss.ProjectID); ok {
		project = p.Name
	}
	// 这段对话的定时任务事实（同一次扫描，两屏共用）：条数、下次自己醒来的时刻
	cron := crons[ss.LogicalConversationID()]
	return mobileSessionSummary{
		ID:          ss.ID,
		Title:       ss.Title,
		ProjectName: project,
		UpdatedAt:   ss.UpdatedAt.Format(time.RFC3339),
		Running:     running,
		Waiting:     waiting,
		ReadOnly:    ss.Kind != "",
		HeldByTask:  busy[ss.ID],
		Pinned:      ss.Pinned,
		// A conversation an IM channel owns: the phone tags it, and names the channel when this
		// process can (see mobileSessionSummary.ChannelName).
		ChannelSession: ss.ThreadID != "",
		ChannelName:    d.mobileChannelName(ss),
		// 定时任务写进这段对话的条数，和它下一次自己醒来的时候（桌面侧栏那颗徽章的同一份事实）
		Cron:     cron.Count,
		CronNext: mobileCronNext(cron.Next),
	}
}

// mobileCronNext renders "when this conversation next wakes up on its own" for the wire. A zero
// time and an absent one are the SAME fact here — nothing is scheduled — so zero is "" and the
// phone draws no time rather than 0001-01-01.
func mobileCronNext(next time.Time) string {
	if next.IsZero() {
		return ""
	}
	return next.Format(time.RFC3339)
}

// mobileChannelName is the channel that owns a conversation, or "" when this process cannot say.
//
// The RECORD comes first: the channel writes its own name onto the conversation on the first turn it
// runs (channel/manager/turn_helpers.go), so the name survives a restart. The live mapping is the
// fallback — it is what a process that has seen this thread's messages this run knows — and "" is
// what both say when neither does, which is why the phone still has the flag to draw the tag with.
func (d *desktopApp) mobileChannelName(ss *session.Session) string {
	if ss == nil || ss.ThreadID == "" {
		return ""
	}
	if ss.ChannelName != "" {
		return ss.ChannelName
	}
	if turns := d.channelRuntime(); turns != nil {
		return turns.ThreadPlatform(ss.ThreadID)
	}
	return ""
}

// mobilePage returns up to `limit` records whose index is strictly LESS than `before`
// (newest-first window from the tail, delivered oldest→newest).
//
// The cursor is the record index, not a timestamp — two records written in the same instant
// would otherwise be skipped wholesale when they straddle a page boundary.
func (d *desktopApp) mobilePage(id string, before int, limit int) ([]mobileMessage, bool, *session.Session, error) {
	if d.sm == nil {
		return nil, false, nil, fmt.Errorf("no session manager")
	}
	ss := d.sessionRecord(id)
	if ss == nil {
		return nil, false, nil, errMobileNoSession
	}
	if limit <= 0 {
		limit = mobilePageLimit
	}
	raw, err := d.sm.LoadSessionMessages(id)
	if err != nil {
		return nil, false, nil, err
	}
	kept := make([]int, 0, len(raw))
	for i := range raw {
		if before > 0 && i >= before {
			continue
		}
		kept = append(kept, i)
	}
	hasMore := len(kept) > limit
	if hasMore {
		kept = kept[len(kept)-limit:]
	}
	return mobileMessages(raw, kept), hasMore, ss, nil
}

// mobileSnapshot is what `open` answers with: the record's tail plus everything the phone needs
// to paint the screen around it. It is a READ — no agent is built, no state is emitted, and the
// desktop's displayed session is untouched.
func (d *desktopApp) mobileSnapshot(id string) (*mobileSnapshot, error) {
	if d.sm == nil {
		return nil, fmt.Errorf("no session manager")
	}
	ss := d.sessionRecord(id)
	if ss == nil {
		return nil, errMobileNoSession
	}
	raw, err := d.sm.LoadSessionMessages(id)
	if err != nil {
		return nil, err
	}
	kept := make([]int, 0, len(raw))
	for i := range raw {
		kept = append(kept, i)
	}
	hasMore := len(kept) > mobileSnapshotLimit
	if hasMore {
		kept = kept[len(kept)-mobileSnapshotLimit:]
	}
	primary, additional := d.sessionRootsFrom(ss)
	if additional == nil {
		additional = []string{}
	}
	// Everything the context card shows, from ONE read: a live agent's own measurement when one
	// exists, the records on disk when it does not (a conversation the desktop never opened in
	// this process — issue #53's "context and cost are empty").
	est, tb, window := d.contextUsage(id)
	if est <= 0 {
		est = estimateContextFromRecords(raw, d.providerTypeFor(ss))
	}
	if window <= 0 {
		window = d.contextWindowFor(ss)
	}
	parts := mobileContextParts(contextParts(tb))
	// The session records the provider it runs on; "" means "the default", and the phone shows
	// a name, so the default is spelled out here rather than left as an empty chip.
	model := ss.ProviderName
	if model == "" {
		if cfg := d.config(); cfg != nil {
			model = cfg.Provider
		}
	}
	return &mobileSnapshot{
		Session:      d.mobileSummary(ss, d.busySessions()),
		Messages:     mobileMessages(raw, kept),
		HasMore:      hasMore,
		State:        d.mobileState(id),
		Asks:         d.mobileAsks(id),
		Model:        model,
		SkipApproval: ss.SkipApproval,
		NotifyOff:    ss.NotifyOff,
		Workspace:    mobileWorkspace{Primary: primary, Additional: additional},
		Usage:        d.mobileUsage(id),
		Context:      mobileContext{Estimate: est, Window: window, Parts: parts},
		Attachments:  d.mobileAttachments(ss, raw),
	}, nil
}

// mobileMessages converts the selected raw records into the wire shape, preserving the true
// in-turn ordering (assistant text / tool call / tool result interleaved) and folding a run of
// thinking records into the assistant message that follows them.
func mobileMessages(raw []session.Message, kept []int) []mobileMessage {
	out := make([]mobileMessage, 0, len(kept))
	var pendingThinking []string
	for _, i := range kept {
		rm := raw[i]
		index := i
		switch rm.Type {
		case session.MessageTypeUser:
			out = append(out, mobileMessage{
				Index: index, Role: "user", Content: rm.Content, DisplayContent: rm.DisplayContent,
				Iteration: rm.Iteration, Seq: rm.Seq, Timestamp: rm.Timestamp.Format(time.RFC3339),
			})
		case session.MessageTypeReminder:
			out = append(out, mobileMessage{
				Index: index, Role: "reminder", Content: rm.Content, Timestamp: rm.Timestamp.Format(time.RFC3339),
			})
		case session.MessageTypeThinking:
			pendingThinking = append(pendingThinking, rm.Content)
		case session.MessageTypeAssistant:
			m := mobileMessage{
				Index: index, Role: "assistant", Content: rm.Content,
				Iteration: rm.Iteration, Seq: rm.Seq, Timestamp: rm.Timestamp.Format(time.RFC3339),
			}
			if len(pendingThinking) > 0 {
				m.Thinking = strings.Join(pendingThinking, "\n")
				pendingThinking = nil
			}
			out = append(out, m)
		case session.MessageTypeToolCall:
			argsJSON := marshalArgs(rm.Args)
			out = append(out, mobileMessage{
				Index: index, Role: "tool_call", ToolName: rm.Name, ToolCallID: rm.ToolCallID,
				Args: argsJSON, Title: tools.ToolArgsSummary(rm.Name, argsJSON), AutoExpand: rm.AutoExpand,
				Iteration: rm.Iteration, Seq: rm.Seq, Timestamp: rm.Timestamp.Format(time.RFC3339),
			})
		case session.MessageTypeToolResult:
			out = append(out, mobileMessage{
				Index: index, Role: "tool_result", ToolName: rm.Name, ToolCallID: rm.ToolCallID,
				ToolResult: rm.Result, IsError: rm.IsError,
				Iteration: rm.Iteration, Seq: rm.Seq, Timestamp: rm.Timestamp.Format(time.RFC3339),
			})
		}
	}
	return out
}

// mobileState is one session's state, in the wire's shape. The waiting sentence comes from the
// same place the desktop's marker does (waitingSentence), never from a caller.
func (d *desktopApp) mobileState(id string) mobileState {
	st := d.stateFor(id)
	return mobileState{
		Status:  string(st.Status),
		Label:   st.Label,
		Detail:  st.Detail,
		Waiting: d.waitingSentence(id, st),
	}
}

// mobileAsks lists this session's pending waits. The card's content is the agent's own
// (preview / questions) — the phone renders it, it does not re-describe it.
func (d *desktopApp) mobileAsks(sessionID string) []mobileAsk {
	out := make([]mobileAsk, 0, 2)
	for _, a := range d.snapshotAsks() {
		if a.sessionID != sessionID {
			continue
		}
		out = append(out, mobileAskOf(a))
	}
	return out
}

// mobileUsage reads this session's cost from the usage LEDGER, which is what the desktop's
// status bar aggregates too (rebuildCostCredit). It deliberately does not need a live run: a
// conversation the desktop has not opened in this process still spent its money, and reporting
// 0 for it made the phone's usage card look broken (issue #53).
//
// Cost and credit are CNY / 积分, the units the ledger rows carry — the phone renders ¥.
//
// The answer is memoized for mobileUsageTTL. The scan is the most expensive part of opening a
// conversation (40–55 ms measured on a 2 500-record conversation: the ledger is one file per day
// and it is summed from the session's start), while everything else in the snapshot costs a few
// milliseconds — so tapping three conversations in a row meant three full scans for a number that
// had not moved. A turn's spend reaches the phone through its `usage` event, not through this, so
// a few seconds of staleness here cannot make the card disagree with a running turn.
func (d *desktopApp) mobileUsage(id string) mobileUsage {
	ss := d.sessionRecord(id)
	if ss == nil || d.sm == nil {
		return mobileUsage{}
	}
	if u, ok := d.usageCache.get(id); ok {
		return u
	}
	rec := agent.GlobalUsageRecorder()
	if rec == nil {
		return mobileUsage{}
	}
	rows, err := rec.Rows(id, ss.CreatedAt)
	if err != nil {
		return mobileUsage{}
	}
	u := mobileUsageFromRows(rows, d.config())
	d.usageCache.put(id, u)
	return u
}

// mobileUsageTTL is how long one session's cost is trusted. Short on purpose: it only has to
// cover "the reader is moving between conversations right now". A FAILED read is not cached —
// the next open should try again rather than keep reporting zero for five seconds.
const mobileUsageTTL = 5 * time.Second

// mobileUsageCache remembers one number per session. Zero value is usable; the map is created on
// the first write, so a desktopApp built in a test needs no constructor call for it.
type mobileUsageCache struct {
	mu  sync.Mutex
	at  map[string]time.Time
	val map[string]mobileUsage
}

func (c *mobileUsageCache) get(id string) (mobileUsage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.at[id]
	if !ok || time.Since(at) > mobileUsageTTL {
		return mobileUsage{}, false
	}
	return c.val[id], true
}

func (c *mobileUsageCache) put(id string, u mobileUsage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == nil {
		c.at = make(map[string]time.Time)
		c.val = make(map[string]mobileUsage)
	}
	// A bound, because a long-lived desktop walks through hundreds of conversations: dropping the
	// table wholesale is cheaper than tracking which entries are still interesting.
	const maxEntries = 512
	if len(c.at) >= maxEntries {
		c.at = make(map[string]time.Time, maxEntries)
		c.val = make(map[string]mobileUsage, maxEntries)
	}
	c.at[id] = time.Now()
	c.val[id] = u
}

// mobileUsageFromRows is the aggregation itself, without a recorder: the same sums the desktop's
// status bar takes (rebuildCostCredit), testable without touching a ledger directory.
func mobileUsageFromRows(rows []llm.UsageRow, cfg *config.Config) mobileUsage {
	var cost, credit float64
	var lastIn, lastCr int64
	have := false
	for _, row := range rows {
		cost += row.Cost()
		credit += row.CreditValue(llm.ResolveCreditRate(cfg, row.Provider, row.Model))
		lastIn, lastCr = row.InputTokens, row.CacheReadInputTokens
		have = true
	}
	u := mobileUsage{Cost: cost, Credit: credit}
	// Same "recent rate" as the desktop: the LAST call's cache reads over cache-miss + cache-read.
	// Writing new cache entries is excluded on purpose.
	if have && lastIn+lastCr > 0 {
		u.CacheHitRate = float64(lastCr) / float64(lastIn+lastCr)
	}
	return u
}

// mobileContextParts maps the desktop's breakdown onto the wire's {label, tokens}.
func mobileContextParts(parts []ContextPartVO) []mobileContextPart {
	out := make([]mobileContextPart, 0, len(parts))
	for _, p := range parts {
		out = append(out, mobileContextPart{Label: p.Label, Tokens: p.Tokens})
	}
	return out
}

// estimateContextFromRecords is estimateFromMessages WITHOUT a live run: the most recent call's
// recorded prompt size, falling back to the chars/4 estimate on records too old to carry counts.
// A session restored from disk has had nothing appended since that call, so it IS the context
// size right now.
func estimateContextFromRecords(msgs []session.Message, providerType string) int64 {
	var est, real int64
	for _, m := range msgs {
		u := m.Usage
		if u == nil || u.EstimatedInputTokens <= 0 {
			continue
		}
		est = u.EstimatedInputTokens
		real = llm.PromptTokens(&llm.Usage{
			InputTokens:              u.InputTokens,
			CacheReadInputTokens:     u.CacheReadInputTokens,
			CacheCreationInputTokens: u.CacheCreationInputTokens,
		}, providerType)
	}
	if real > 0 {
		return real
	}
	return est
}

// providerTypeFor names the provider TYPE (anthropic / openai / …) a session runs on. It decides
// how llm.PromptTokens reads a recorded call's counts, so "" (unknown) must be distinguishable
// from a real type — it changes the answer for OpenAI-family providers.
func (d *desktopApp) providerTypeFor(ss *session.Session) string {
	cfg := d.config()
	if cfg == nil {
		return ""
	}
	name := ""
	if ss != nil {
		name = ss.ProviderName
	}
	if p := cfg.ProviderConfig(name); p != nil {
		return string(p.Type)
	}
	return ""
}

// contextWindowFor resolves the model's window from CONFIG, for a session no live agent has
// been built for. Same rule as llm.Resolve: the provider's explicit override wins, then the
// built-in model table.
func (d *desktopApp) contextWindowFor(ss *session.Session) int64 {
	cfg := d.config()
	if cfg == nil {
		return 0
	}
	name := ""
	if ss != nil {
		name = ss.ProviderName
	}
	p := cfg.ProviderConfig(name)
	if p == nil {
		return 0
	}
	if p.Spec.ContextWindow != nil && *p.Spec.ContextWindow > 0 {
		return *p.Spec.ContextWindow
	}
	return llm.ModelContextWindow(p.Model)
}

// mobileAttachments lists the files THIS conversation handed over with SendFile. Nothing else
// is reachable from the phone: the download endpoint resolves an id against these same records,
// so "the token can read any file" is not a capability this interface ever had.
//
// The id IS the tool call's id: one call carries one path, and that id is already in the record,
// so the link survives a reload and a restart.
func (d *desktopApp) mobileAttachments(ss *session.Session, raw []session.Message) []mobileAttachment {
	max := d.mobileAttachmentLimit()
	out := make([]mobileAttachment, 0, 4)
	seen := make(map[string]bool, 4)
	for _, rm := range raw {
		if rm.Type != session.MessageTypeToolCall || rm.Name != tools.ToolNameSendFile || rm.ToolCallID == "" {
			continue
		}
		if seen[rm.ToolCallID] {
			continue
		}
		seen[rm.ToolCallID] = true
		path := sendFilePath(rm.Args)
		if path == "" {
			continue
		}
		abs := d.resolveAttachmentPath(ss, path)
		size := int64(0)
		downloadable := false
		if abs != "" {
			if st, err := os.Stat(abs); err == nil && st.Mode().IsRegular() {
				size = st.Size()
				downloadable = size <= max
			}
		}
		name := filepath.Base(path)
		out = append(out, mobileAttachment{
			ID:           rm.ToolCallID,
			Name:         name,
			MimeType:     mobileMimeType(name),
			Size:         size,
			Downloadable: downloadable,
		})
	}
	return out
}

// sendFilePath pulls the single argument SendFile takes out of the recorded call.
func sendFilePath(args any) string {
	switch v := args.(type) {
	case nil:
		return ""
	case string:
		var p struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(v), &p) != nil {
			return ""
		}
		return p.Path
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	var p struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(b, &p) != nil {
		return ""
	}
	return p.Path
}

// resolveAttachmentPath turns a recorded path into an absolute one, using the SAME roots this
// conversation runs its tools under (primary first, then the additional ones). "" means the file
// is not reachable through them.
func (d *desktopApp) resolveAttachmentPath(ss *session.Session, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	primary, additional := d.sessionRootsFrom(ss)
	roots := append([]string{primary}, additional...)
	for _, root := range roots {
		if root == "" {
			continue
		}
		candidate := filepath.Join(root, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if primary != "" {
		return filepath.Join(primary, path)
	}
	return ""
}

// mobileAttachmentLimit is the configured download bound, read live so a change applies to the
// next download rather than needing a restart.
func (d *desktopApp) mobileAttachmentLimit() int64 {
	cfg := d.config()
	if cfg == nil {
		return config.MobileConfig{}.MaxAttachmentBytes()
	}
	return cfg.Mobile.MaxAttachmentBytes()
}

// mobileMimeType names a file's type for the phone's renderer. It decides only how the phone
// SHOWS the file (inline image, markdown reader, plain text, "open it on the computer"), so a
// wrong guess is cosmetic — but a missing one would send every unknown file down the same path.
func mobileMimeType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".md", ".markdown":
		return "text/markdown"
	case ".txt", ".log":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		return "text/html"
	case ".pdf":
		return "application/pdf"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}
