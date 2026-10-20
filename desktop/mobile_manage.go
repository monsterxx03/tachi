package main

import (
	"net/http"
	"os"
	"strings"

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
