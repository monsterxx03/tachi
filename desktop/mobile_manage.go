package main

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/cron"
	"github.com/monsterxx03/tachi/session"
)

// 手机改会话列表的那一半：置顶、新建、列出项目。
//
// 这里每一条都遵守协议的不变量 7 —— **手机不改变桌面正在显示的会话**：新建只写记录，不碰
// activeID、不建 agent、不发状态事件。手机建好的对话由 `sessions_changed` 让两块屏各自重读
// 列表；手机点开它时，第一条消息才走 loadSessionForRemote 把 agent 建起来。

// mobileSetPinned pins or unpins a conversation from the phone. It goes through the SAME
// SetSessionPinned the sidebar uses, so the two screens can never disagree about where a row
// sits, and the event it broadcasts brings every other screen in step.
func (d *desktopApp) mobileSetPinned(id string, pinned bool) (int, string) {
	if d.sessionRecord(id) == nil {
		return http.StatusNotFound, "no_session"
	}
	if res := (&AgentService{desk: d}).SetSessionPinned(id, pinned); res != "ok" {
		return http.StatusInternalServerError, "pin_failed"
	}
	return 0, ""
}

// mobileCreateSession creates a conversation from the phone.
//
// projectID names a project (its roots become the session's snapshot, exactly as the window's
// NewSession binds it). Without one, workingDir is the workspace — the phone has no file
// picker, so "~" and "~/x" are expanded on THIS machine, and an empty value means
// ~/workspace. The directory is created when missing: the phone cannot browse for another one,
// and "没有这个目录" is not something the reader could act on.
//
// It deliberately does not build an agent. Building one costs provider setup, tool registration
// and an MCP root resolution, all for a conversation nobody has said anything in yet — and it
// would run against whatever the window is doing. The first send prepares it.
func (d *desktopApp) mobileCreateSession(projectID, workingDir string) (mobileSessionSummary, int, string) {
	if d.sm == nil {
		return mobileSessionSummary{}, http.StatusInternalServerError, "no_session_manager"
	}
	pname := ""
	if cfg := d.config(); cfg != nil {
		pname = cfg.DefaultProviderName()
	}
	wd := ""
	boundID := ""
	var additional []string
	if strings.TrimSpace(projectID) != "" {
		p, primary, ok := d.projects.drives(projectID)
		if !ok {
			// The picker offered it a moment ago; a project that vanished (or whose directory
			// went away) is a real refusal, not something to silently turn into a loose session.
			return mobileSessionSummary{}, http.StatusConflict, "no_project"
		}
		wd = primary
		boundID = p.ID
		additional = append([]string(nil), p.AdditionalDirs...)
	} else {
		raw := strings.TrimSpace(workingDir)
		if raw == "" {
			raw = "~/workspace"
		}
		abs, err := expandRootPath(raw)
		if err != nil {
			return mobileSessionSummary{}, http.StatusBadRequest, "bad_working_dir"
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return mobileSessionSummary{}, http.StatusConflict, "bad_working_dir"
		}
		wd = abs
	}

	sm := d.newSessionManager()
	if sm == nil {
		return mobileSessionSummary{}, http.StatusInternalServerError, "no_session_manager"
	}
	sess, err := sm.New(pname, wd)
	if err != nil {
		return mobileSessionSummary{}, http.StatusInternalServerError, "create_failed"
	}
	if boundID != "" {
		sess.ProjectID = boundID
		sess.AdditionalDirs = additional
		if err := sm.UpdateMeta(sess); err != nil {
			// The session exists; its binding did not stick. Report it rather than hand back a
			// row that claims a project it is not bound to.
			return mobileSessionSummary{}, http.StatusInternalServerError, "create_failed"
		}
	}
	d.emitSessionsChanged(sess.ID)
	return d.mobileSummary(sess, d.busySessions()), 0, ""
}

// mobileProject is the phone's project picker row. The primary directory rides along so the
// caller can show what the session will actually run in.
type mobileProject struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Primary  string `json:"primary"`
	Runnable bool   `json:"runnable"`
}

// mobileProjects lists the projects a new conversation can be created in. A project whose
// primary directory is gone is still listed (it is the user's own entry) but marked unrunnable,
// which is what the picker needs to disable it instead of letting the tap fail.
func (d *desktopApp) mobileProjects() []mobileProject {
	all := d.projects.list()
	out := make([]mobileProject, 0, len(all))
	for _, p := range all {
		_, primary, ok := d.projects.drives(p.ID)
		out = append(out, mobileProject{ID: p.ID, Name: p.Name, Primary: primary, Runnable: ok})
	}
	return out
}

// mobileSessionByID resolves one session record for the phone or answers no_session. Shared by
// the writers above and by the panels, so the refusal is spelled once.
func (d *desktopApp) mobileSessionByID(id string) (*session.Session, int, string) {
	if strings.TrimSpace(id) == "" {
		return nil, http.StatusNotFound, "no_session"
	}
	ss := d.sessionRecord(id)
	if ss == nil {
		return nil, http.StatusNotFound, "no_session"
	}
	return ss, 0, ""
}

// ---------------------------------------------------------------------------
// 会话面板：MCP / 可委派的 agent / 定时任务
// ---------------------------------------------------------------------------

// mobileMCPPanel is one MCP server as the phone lists it: the counts, not the tool contracts.
// A phone screen cannot use a JSON schema; what it can answer is "是否连上、载入了几个工具".
type mobileMCPPanel struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Connected bool   `json:"connected"`
	NeedsAuth bool   `json:"needsAuth"`
	Loaded    int    `json:"loaded"`
	Tools     int    `json:"tools"`
}

// mobileAgentPanel is the peer board, minus the parts only the window can act on (authorizing an
// MCP server, opening a card, inserting a reference).
type mobileAgentPanel struct {
	Peers      []mobileAgentRow `json:"peers"`
	CheckedAt  string           `json:"checkedAt"`
	AnyChecked bool             `json:"anyChecked"`
}

type mobileAgentRow struct {
	Name   string   `json:"name"`
	Label  string   `json:"label"`
	URL    string   `json:"url"`
	Note   string   `json:"note"`
	Reach  string   `json:"reach"`
	Skills []string `json:"skills"`
	Err    string   `json:"err"`
}

// mobileCronRow is one scheduled job as the phone lists it.
type mobileCronRow struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ScheduleText  string `json:"scheduleText"`
	Status        string `json:"status"`
	TargetKind    string `json:"targetKind"`
	TargetTitle   string `json:"targetTitle"`
	NextRun       string `json:"nextRun"`
	LastRunStatus string `json:"lastRunStatus"`
	LastRunError  string `json:"lastRunError"`
	Running       bool   `json:"running"`
}

// mobilePanels is the whole panel screen in one answer: three lists that are all read-only.
type mobilePanels struct {
	MCP    []mobileMCPPanel `json:"mcp"`
	Agents mobileAgentPanel `json:"agents"`
	Cron   []mobileCronRow  `json:"cron"`
}

// mobileSessionPanels answers the three panels for ONE conversation. It is a read: no agent is
// built, no probe is forced beyond what the board already caches, and the desk's view is
// untouched. The MCP half is the session's own loaded set (listMCPServersFor), because "this
// session can call these tools" is the question the panel exists to answer.
func (d *desktopApp) mobileSessionPanels(sessionID string) (*mobilePanels, int, string) {
	if _, status, code := d.mobileSessionByID(sessionID); code != "" {
		return nil, status, code
	}
	svc := &AgentService{desk: d}
	out := &mobilePanels{
		MCP:    make([]mobileMCPPanel, 0, 4),
		Cron:   make([]mobileCronRow, 0, 4),
		Agents: mobileAgentPanel{Peers: make([]mobileAgentRow, 0, 4)},
	}

	for _, s := range svc.listMCPServersFor(sessionID) {
		loaded := 0
		for _, t := range s.Tools {
			if t.Loaded {
				loaded++
			}
		}
		out.MCP = append(out.MCP, mobileMCPPanel{
			Name: s.Name, Type: s.Type, Connected: s.Connected, NeedsAuth: s.NeedsAuth,
			Loaded: loaded, Tools: len(s.Tools),
		})
	}

	board := svc.PeerBoard()
	out.Agents.CheckedAt = board.CheckedAt
	out.Agents.AnyChecked = board.AnyChecked
	for _, p := range board.Peers {
		row := mobileAgentRow{
			Name: p.Name, Label: p.Label, URL: p.URL, Note: p.Note, Reach: p.Reach, Err: p.Err,
			Skills: make([]string, 0, 4),
		}
		if p.Card != nil {
			for _, sk := range p.Card.Skills {
				row.Skills = append(row.Skills, sk.Name)
			}
		}
		out.Agents.Peers = append(out.Agents.Peers, row)
	}

	for _, j := range svc.ListCronJobs() {
		out.Cron = append(out.Cron, mobileCronRow{
			ID: j.ID, Name: j.Name, ScheduleText: j.ScheduleText, Status: j.Status,
			TargetKind: j.TargetKind, TargetTitle: j.TargetTitle,
			NextRun:       mobileStamp(j.NextRun),
			LastRunStatus: j.LastRunState, LastRunError: j.LastRunError, Running: j.Running,
		})
	}

	// A conversation a channel owns has scheduled jobs of ITS OWN — created from inside that
	// conversation, firing back into its thread. Before this the phone could not see them at all
	// (ListCronJobs deliberately excludes channel jobs), so a job you set up in discord was
	// invisible from the phone even while reading the very conversation it fires into.
	//
	// Read-only HERE: running one needs the channel runtime that owns the thread, and creating or
	// editing one belongs to that conversation's own commands — the same line the desktop panel
	// already draws for channel jobs.
	if ss, _, code := d.mobileSessionByID(sessionID); code == "" && ss.ThreadID != "" {
		// The SAME name resolution the row tag uses (record first, live mapping second), so a job and
		// the conversation it fires into can never name their channel differently.
		platform := d.mobileChannelName(ss)
		if platform == "" {
			platform = "渠道会话"
		}
		out.Cron = append(out.Cron, d.mobileChannelCronRows(ss.ThreadID, platform)...)
	}
	return out, 0, ""
}

// mobileChannelCronRows lists one channel thread's scheduled jobs for the phone's panel.
//
// An empty TargetType reads as "channel": every job written before the desktop could schedule is one
// of these (cron/job.go says so, and the channel's own scheduler reads it the same way).
func (d *desktopApp) mobileChannelCronRows(threadID, platform string) []mobileCronRow {
	jobs, err := d.cronJobRecords()
	if err != nil {
		return nil
	}
	var out []mobileCronRow
	for _, job := range jobs {
		if job.TargetThreadID != threadID {
			continue
		}
		if job.TargetType != "" && job.TargetType != cron.TargetTypeChannel {
			continue
		}
		row := mobileCronRow{
			ID:            job.ID,
			Name:          job.Name,
			ScheduleText:  cron.DescribeSchedule(job.Schedule),
			Status:        string(job.Status),
			TargetKind:    "channel",
			TargetTitle:   platform,
			LastRunStatus: job.LastRunStatus,
			LastRunError:  job.LastRunError,
		}
		if job.Status == cron.JobStatusActive {
			if next, nerr := cron.NextRun(job.Schedule, job.Timezone, time.Now()); nerr == nil {
				row.NextRun = mobileStamp(next)
			}
		}
		out = append(out, row)
	}
	return out
}

// mobileStamp renders a time for the wire, or "" when the job has never run / has no next run.
func mobileStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// 定时任务：运行一次 / 暂停 / 删除
// ---------------------------------------------------------------------------

// mobileCronResult maps a cron write's answer to the phone's (status, code). The three writers in
// cron.go answer with a Chinese sentence (it is what the window shows verbatim), so the mapping
// belongs here rather than in a second implementation: one refusal, one behaviour.
func mobileCronResult(res string) (int, string) {
	switch res {
	case "ok":
		return 0, ""
	case "任务不存在":
		return http.StatusNotFound, "no_job"
	case "定时任务已在设置里关闭":
		return http.StatusConflict, "cron_disabled"
	case "这个任务不属于 desktop":
		return http.StatusConflict, "not_desktop"
	}
	return http.StatusInternalServerError, "cron_failed"
}

// mobileCronRun runs one job now. The run itself is asynchronous: the phone gets "accepted", and
// the job's 运行中 state arrives through the cron_changed broadcast this path already emits.
func (d *desktopApp) mobileCronRun(id string) (int, string) {
	return mobileCronResult((&AgentService{desk: d}).RunCronJobNow(id))
}

// mobileCronPause pauses or resumes a job.
func (d *desktopApp) mobileCronPause(id string, paused bool) (int, string) {
	return mobileCronResult((&AgentService{desk: d}).SetCronJobPaused(id, paused))
}

// mobileCronDelete removes a job. Same writer as the window's 删除: the conversation it wrote into
// and its records stay where they are.
func (d *desktopApp) mobileCronDelete(id string) (int, string) {
	return mobileCronResult((&AgentService{desk: d}).DeleteCronJob(id))
}

// mobileRename sets a conversation's title from the phone and answers the protocol's (status, code).
//
// It goes through RenameSession — the window's own writer — so there is one place that decides what
// a rename means, and the sessions_changed it emits is how the OTHER screens hear about it. That
// broadcast is the whole point here: a row's title is its identity in the list, and a phone that
// renamed a conversation must not leave the window showing the old name (nor the reverse).
//
// A title is cosmetic, so a conversation another task owns can still be renamed — the same rule the
// sidebar follows. Only an EMPTY one is refused: a row with no name at all is worse than the old one.
func (d *desktopApp) mobileRename(id, title string) (int, string) {
	title = strings.TrimSpace(title)
	if title == "" {
		return http.StatusBadRequest, "bad_request"
	}
	if d.sessionRecord(id) == nil {
		return http.StatusNotFound, "no_session"
	}
	switch res := (&AgentService{desk: d}).RenameSession(id, title); res {
	case "ok":
		return 0, ""
	case "no session manager":
		return http.StatusInternalServerError, "rename_failed"
	default:
		return http.StatusNotFound, "no_session"
	}
}
