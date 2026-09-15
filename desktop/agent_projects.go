package main

// AgentService methods for desktop projects — the read/write surface the sidebar and the
// workspace panel will use (design §6.1, §7). The store itself lives in projects.go.
//
// Writes answer with a string: "ok", or the reason the UI shows verbatim. That is the
// convention every other workspace API already uses (see roots.go), and the refresh is a
// separate LIST call — the frontend re-reads after a mutation, which is also what keeps a
// rename from needing a fan-out into the sidebar's rows (design §3.3, §7.4).

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/fileutil"
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
	d.emitWorkspaceChanged(id, "")
	return "ok"
}

// SetProjectRoots replaces a project's workspaces. Every member session follows on its
// NEXT read — no session meta is written here at all (design §2), which is what makes this
// a single file write however many sessions the project has.
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
	// Members whose agent is already alive have a skill store whose scan roots were fixed
	// when it was built (design §4.1): the tools, the prompt and @-references pick the new
	// directory up on their own, the skills would not. They are INVALIDATED rather than
	// re-pointed here — see invalidateMemberSkills.
	d.invalidateMemberSkills(id)
	// Every member's workspace just moved, and the frontend holds that state by value.
	d.emitWorkspaceChanged(id, "")
	return "ok"
}

// DeleteProject removes a project and DETACHES its members (design §6.2): each one keeps its
// workspace, becoming an ordinary session whose snapshot is refreshed to the project's roots
// as they are at this moment. Detaching rather than refusing or cascading is the point — after
// a project has collected dozens of conversations, "go move each one first" is a punishment,
// and deleting the conversations is data loss.
//
// Refused while any member is running a turn, like DeleteSession, because the detach rewrites
// session meta: a turn appending to that session's directory in parallel is the clobber the
// meta path exists to avoid. For the same reason the check and the writes share ONE critical
// section (d.mu) — a turn starting in between would be writing while we rewrite.
//
// The project entry goes last: if a member's write fails, the project is still there and the
// user can retry. (The other order would leave them detached with the container gone.)
func (s *AgentService) DeleteProject(id string) string {
	d := s.desk
	p, ok := d.projects.get(id)
	if !ok {
		return "项目不存在"
	}
	members := d.memberSessions(id)

	// The snapshot the members fall back to, taken BEFORE anything is written. A project that
	// cannot drive its sessions (its own primary is gone) has no roots worth copying: the
	// members are already on their last known-good snapshot, and overwriting that with a path
	// that does not exist would hand bash a cwd that is not there.
	refreshSnapshot := false
	snapPrimary := ""
	var snapAdditional []string
	if _, primary, ok := d.projects.drives(id); ok {
		refreshSnapshot = true
		snapPrimary = primary
		snapAdditional = append([]string(nil), p.AdditionalDirs...)
	}

	// The check and the writes share ONE critical section, inside detachMembers; the event is
	// sent after it AND after the table write, so a reader that reacts to it sees the finished
	// state (and the emit never runs with d.mu held).
	if reason := d.detachMembers(members, refreshSnapshot, snapPrimary, snapAdditional); reason != "" {
		return reason
	}
	if err := d.projects.remove(id); err != nil {
		return fmt.Sprintf("删除项目失败：%v", err)
	}
	d.emitWorkspaceChanged(id, "")
	return "ok"
}

// detachMembers unbinds every member session in ONE d.mu critical section and returns "" on
// success, or the refusal/error to show the user. Callers must NOT hold d.mu.
//
// refresh says whether the members' snapshot is rewritten to the project's roots (primary,
// additional); when false the record keeps the snapshot it already had — the caller decided the
// project has no roots worth copying (see DeleteProject).
func (d *desktopApp) detachMembers(members []*session.Session, refresh bool, primary string, additional []string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, sess := range members {
		if r := d.runs[sess.ID]; r != nil && r.running {
			return refuseDeleteProjectRunning
		}
	}
	for _, sess := range members {
		// updateSessionMetaLocked, not updateSessionMeta: we are inside the critical section
		// that makes the check above meaningful (see its doc).
		if err := d.updateSessionMetaLocked(sess.ID, func(cur *session.Session) {
			if refresh {
				cur.WorkingDir = primary
				cur.AdditionalDirs = append([]string(nil), additional...)
			}
			cur.ProjectID = ""
		}); err != nil {
			return fmt.Sprintf("删除项目失败：%v", err)
		}
		// A live member's agent must not keep serving the tree it was pointed at while the
		// project owned it (design §4.1/§6.2). The paths are normally identical, and the mark
		// is what keeps that from being silently assumed.
		if r := d.runs[sess.ID]; r != nil && r.agent != nil {
			r.skillsStale = true
		}
	}
	return ""
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
	if d.app == nil {
		return
	}
	payload := map[string]any{"projectId": projectID}
	if sessionID != "" {
		payload["sessionId"] = sessionID
	}
	d.app.Event.Emit("agent:workspace_changed", payload)
}

// invalidateMemberSkills marks every LIVE member session's agent as needing its skill store
// re-pointed, which happens at that session's next turn start (beginTurn).
//
// It deliberately does not call ReloadSkillsIn itself: this runs on the UI goroutine while
// member sessions may be mid-turn (sessions run in parallel), and the reload rewrites the
// agent's skill store and tool registry — a data race with the turn reading them. The
// session's own turn is the only place that can prove the agent is quiescent, and the
// store is only ever read during a turn, so nothing observes the stale one in between.
// Sessions without an agent are skipped on purpose: theirs will be built from the new
// directory, which is persisted by the time anyone reads it.
func (d *desktopApp) invalidateMemberSkills(projectID string) {
	for _, sess := range d.memberSessions(projectID) {
		d.markSkillsStale(sess.ID)
	}
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
