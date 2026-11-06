package main

// AgentService methods for desktop projects — the read/write surface the sidebar and the
// workspace panel will use (design §6.1, §7). The store itself lives in projects.go.
//
// Writes answer with a string: "ok", or the reason the UI shows verbatim. That is the
// convention every other workspace API already uses (see roots.go), and the refresh is a
// separate LIST call — the frontend re-reads after a mutation, which is also what keeps a
// rename from needing a fan-out into the sidebar's rows (design §3.3, §7.4).

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// ProjectVO is one project as the UI sees it. SessionCount and RootsUsable are computed
// per call so the sidebar's "(3)" and the panel's "目录不可用" never need a second round
// trip — and so a stale count is impossible (they are not stored anywhere).
type ProjectVO struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	WorkingDir     string          `json:"workingDir"`
	AdditionalDirs []SessionRootVO `json:"additionalDirs"`
	SessionCount   int             `json:"sessionCount"`
	// RootsUsable is false when the project's PRIMARY directory no longer validates on this
	// machine (it moved, was unmounted, or the file was hand-edited): its members then fall
	// back to their snapshots and become editable, and the UI says so instead of showing a
	// dead path as live (design §8.6). A vanished ADDITIONAL root does not set this — it is
	// reported per root below, exactly as it is for a session.
	RootsUsable bool      `json:"rootsUsable"`
	CreatedAt   time.Time `json:"createdAt"`
}

// ListProjects returns every project, newest first, for the sidebar's groups. An unknown
// or unreadable table yields an empty slice, never an error: the desktop must work
// without projects.
func (s *AgentService) ListProjects() []ProjectVO {
	d := s.desk
	projects := d.projects.list()
	counts := d.sessionCountsByProject()

	out := make([]ProjectVO, 0, len(projects))
	for _, p := range projects {
		out = append(out, projectVO(p, counts[p.ID]))
	}
	return out
}

// CreateProject adds a project. name may be empty, in which case it is derived from the
// primary directory's base name (with -2 / -3 while it collides).
func (s *AgentService) CreateProject(name, primary string, additional []string) string {
	d := s.desk
	absPrimary, absAdditional, err := usableProjectRoots(primary, additional)
	if err != nil {
		return err.Error()
	}
	if strings.TrimSpace(name) == "" {
		name = d.projects.projectNameFor(absPrimary)
	} else {
		name = strings.TrimSpace(name)
		// A typed name is checked too: the name is the sidebar's group label, and
		// projectNameFor's derived names only guarantee uniqueness among themselves.
		if d.projects.nameTaken(name, "") {
			return fmt.Sprintf("已存在同名项目「%s」", name)
		}
	}
	now := time.Now()
	p := project{
		ID:             session.GenerateID(),
		Name:           name,
		WorkingDir:     absPrimary,
		AdditionalDirs: absAdditional,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := d.projects.upsert(p); err != nil {
		return fmt.Sprintf("保存项目失败：%v", err)
	}
	// Deliberately NOT rememberWorkspace(primary): a project's directory is not the answer
	// to "where should the next PROJECT-LESS session start" (design §6.1).
	d.notifyMCPRootsChanged()
	d.emitWorkspaceChanged(p.ID, "")
	return "ok"
}

// RenameProject changes a project's name. Nothing else moves: sessions store the ID and
// the sidebar joins the name in, so one write relabels every row at once (design §3.3).
func (s *AgentService) RenameProject(id, name string) string {
	d := s.desk
	name = strings.TrimSpace(name)
	if name == "" {
		return "项目名不能为空"
	}
	p, ok := d.projects.get(id)
	if !ok {
		return "项目不存在"
	}
	if p.Name == name {
		return "ok"
	}
	if d.projects.nameTaken(name, id) {
		return fmt.Sprintf("已存在同名项目「%s」", name)
	}
	p.Name = name
	p.UpdatedAt = time.Now()
	if err := d.projects.upsert(p); err != nil {
		return fmt.Sprintf("保存项目失败：%v", err)
	}
	// The sidebar's rows are labelled by joining this name in, so they have to be re-read.
	d.notifyMCPRootsChanged()
	d.emitWorkspaceChanged(id, "")
	return "ok"
}

// SetProjectRoots replaces a project's workspaces. This moves NO existing session — a session
// owns its roots from the moment it is created (see copyProjectRoots) — so it is a single file
// write however many sessions the project has, and the next session created here starts in the
// new directory.
func (s *AgentService) SetProjectRoots(id, primary string, additional []string) string {
	d := s.desk
	p, ok := d.projects.get(id)
	if !ok {
		return "项目不存在"
	}
	primary, additional, err := usableProjectRoots(primary, additional)
	if err != nil {
		return err.Error()
	}
	p.WorkingDir = primary
	p.AdditionalDirs = additional
	p.UpdatedAt = time.Now()
	if err := d.projects.upsert(p); err != nil {
		return fmt.Sprintf("保存项目失败：%v", err)
	}
	// No member's workspace moved (that is the point of the model), so no live agent needs its
	// skill store re-pointed either — invalidateMemberSkills belongs to moving ONE session (see
	// SetSessionWorkingDir / CreateSessionWorktree), which is where it is called.
	d.notifyMCPRootsChanged()
	d.emitWorkspaceChanged(id, "")
	return "ok"
}

// DeleteProject removes a project and DETACHES its members: each one keeps the workspace it has
// (it owns it already), and only loses the group. Deleting the conversations instead would be data
// loss, and asking the user to move each one first would be a punishment — after a project has
// collected dozens of conversations, the removal of a heading is not a reason to touch any of them.
//
// Refused while any member is running a turn, like DeleteSession: the detach rewrites session meta,
// and a turn appending to that session's record in parallel is the clobber the meta path exists to
// avoid. The check and the writes share ONE critical section (d.mu).
//
// The project entry goes last: if a member's write fails, the project is still there and the user
// can retry.
func (s *AgentService) DeleteProject(id string) string {
	d := s.desk
	if _, ok := d.projects.get(id); !ok {
		return "项目不存在"
	}
	members := d.memberSessions(id)
	if reason := d.detachMembers(members); reason != "" {
		return reason
	}
	if err := d.projects.remove(id); err != nil {
		return fmt.Sprintf("删除项目失败：%v", err)
	}
	d.notifyMCPRootsChanged()
	d.emitWorkspaceChanged(id, "")
	return "ok"
}

// detachMembers unbinds every member session in ONE d.mu critical section and returns "" on
// success, or the refusal/error to show the user. Callers must NOT hold d.mu.
//
// Nothing about a member's workspace is written: its roots were copied in at creation and are its
// own, so detaching is exactly one field.
func (d *desktopApp) detachMembers(members []*session.Session) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, sess := range members {
		// ownerOfLocked, not d.runs[...]: a member that a compaction moved the conversation
		// INTO mid-turn is being written by that turn, so detaching it is refused the same way.
		if r := d.ownerOfLocked(sess.ID); r != nil && r.running {
			return refuseDeleteProjectRunning
		}
	}
	for _, sess := range members {
		if err := d.updateSessionMetaLocked(sess.ID, func(cur *session.Session) {
			cur.ProjectID = ""
		}); err != nil {
			return fmt.Sprintf("删除项目失败：%v", err)
		}
	}
	return ""
}

// copyProjectRoots gives a session created FROM a project the project's roots as its own. It is
// the only place a project's roots enter a session record, and it runs once per session: at
// creation (NewSession, the phone's create) or in the one-time upgrade migration below. Nothing
// re-copies them later — that is what makes an existing session immune to a project edit.
//
// Additional roots are expanded here the same way the primary was (projects.json is hand-editable,
// and a stored "~/" must not reach bash as a literal "~"); one that cannot be expanded is dropped,
// because a root that cannot be resolved is not a place the session can work in.
func copyProjectRoots(sess *session.Session, p project, primary string) {
	sess.ProjectID = p.ID
	sess.WorkingDir = primary
	sess.AdditionalDirs = make([]string, 0, len(p.AdditionalDirs))
	for _, dir := range p.AdditionalDirs {
		abs, err := expandRootPath(dir)
		if err != nil {
			continue
		}
		sess.AdditionalDirs = append(sess.AdditionalDirs, abs)
	}
	if len(sess.AdditionalDirs) == 0 {
		sess.AdditionalDirs = nil
	}
}

// rootsDiffer reports whether a session's own roots are no longer the project's current ones —
// the comparison behind the panel's 「已改动」 tag and the sidebar's dot.
func rootsDiffer(p project, primary string, sess *session.Session) bool {
	return filepath.Clean(primary) != filepath.Clean(sess.WorkingDir) ||
		!sameRoots(p.AdditionalDirs, sess.AdditionalDirs)
}

// workspaceChanged is rootsDiffer for a session that may not have a usable project at all: a
// project-less session, or one whose project is gone, has nothing to differ from.
func (d *desktopApp) workspaceChanged(sess *session.Session) bool {
	if sess == nil || sess.ProjectID == "" {
		return false
	}
	p, primary, ok := d.projects.drives(sess.ProjectID)
	if !ok {
		return false
	}
	return rootsDiffer(p, primary, sess)
}

// migrateProjectWorkspaces is the ONE fan-out write in this design, and it runs once per data dir:
// projects.json older than projectFileVersion describes sessions whose workspace lived on the
// project, so each member's record gets the project's CURRENT roots — the workspace it has been
// working in all along — and then the file is stamped so this never happens again.
//
// Without it, the new model would silently move every conversation created before its project's
// last directory change back to the directory it was created in.
//
// It runs at startup, before any turn can be in flight, and a failure on one session leaves that
// session on its old snapshot rather than aborting the rest.
func (d *desktopApp) migrateProjectWorkspaces() {
	if d.sm == nil || !d.projects.needsWorkspaceMigration() {
		return
	}
	list, err := d.sm.List()
	if err != nil {
		logger.New("desktop").Warn(context.Background(),
			"工作区迁移：读取会话列表失败，下次启动再试", "err", err)
		return
	}
	moved := 0
	for _, sess := range list {
		if sess.ProjectID == "" {
			continue
		}
		p, primary, ok := d.projects.drives(sess.ProjectID)
		if !ok || !rootsDiffer(p, primary, sess) {
			continue
		}
		id := sess.ID
		if err := d.updateSessionMeta(id, func(cur *session.Session) {
			copyProjectRoots(cur, p, primary)
		}); err != nil {
			logger.New("desktop").Warn(context.Background(),
				"工作区迁移：写入会话失败", "session", id, "err", err)
			continue
		}
		moved++
	}
	if err := d.projects.markWorkspacesMigrated(); err != nil {
		logger.New("desktop").Warn(context.Background(),
			"工作区迁移：标记 projects.json 版本失败，下次启动会重跑", "err", err)
		return
	}
	if moved > 0 {
		logger.New("desktop").Info(context.Background(),
			"工作区迁移完成：会话现在各自持有自己的工作区", "sessions", moved)
	}
}

// emitWorkspaceChanged tells the frontend that a project write moved the ground under some
// session, so it re-reads what it shows (design §7.4).
//
// Everything the frontend knows about a session's workspace — the composer's chip, the roots
// panel, the sidebar's groups — was PULLED at some earlier moment, while a project edit changes
// the BACKEND's resolution. Without this event an already-open panel keeps naming the old
// directory (and an old project name) until the user switches sessions, which is exactly the
// "it says it worked but nothing moved" report this contract exists to prevent.
//
// projectID is what changed; sessionID is set when the change is about ONE session (a detach),
// so a frontend can also act on a session it is not showing. Both are informational: the
// reader re-reads the session it IS showing plus the sidebar, whatever the payload says.
func (d *desktopApp) emitWorkspaceChanged(projectID, sessionID string) {
	if !d.hasUI() {
		return
	}
	payload := map[string]any{"projectId": projectID}
	if sessionID != "" {
		payload["sessionId"] = sessionID
	}
	d.emit("agent:workspace_changed", payload)
}

// memberSessions lists the sessions bound to a project, newest first.
func (d *desktopApp) memberSessions(projectID string) []*session.Session {
	if d.sm == nil || projectID == "" {
		return nil
	}
	sessions, err := d.sm.List()
	if err != nil {
		return nil
	}
	out := make([]*session.Session, 0, len(sessions))
	for _, sess := range sessions {
		if sess.ProjectID == projectID {
			out = append(out, sess)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// sessionCountsByProject counts the on-disk sessions per project ID, in one pass over the
// session list.
func (d *desktopApp) sessionCountsByProject() map[string]int {
	counts := make(map[string]int)
	if d.sm == nil {
		return counts
	}
	sessions, err := d.sm.List()
	if err != nil {
		return counts
	}
	for _, sess := range sessions {
		if sess.ProjectID != "" {
			counts[sess.ProjectID]++
		}
	}
	return counts
}

// projectVO renders a stored project for the frontend, with each additional root's current
// existence (the panel greys out a root that is gone, exactly like a session's).
func projectVO(p project, sessionCount int) ProjectVO {
	vo := ProjectVO{
		ID:             p.ID,
		Name:           p.Name,
		WorkingDir:     p.WorkingDir,
		AdditionalDirs: make([]SessionRootVO, 0, len(p.AdditionalDirs)),
		SessionCount:   sessionCount,
		CreatedAt:      p.CreatedAt,
	}
	for _, dir := range p.AdditionalDirs {
		vo.AdditionalDirs = append(vo.AdditionalDirs, SessionRootVO{Path: dir, Exists: fileutil.IsDir(dir)})
	}
	// The same predicate the resolver and the guards use (projectTable.drives), so the panel
	// never greys out a project that is in fact driving sessions.
	_, err := resolveProjectPrimary(p.WorkingDir)
	vo.RootsUsable = err == nil
	return vo
}
