package main

// Additional workspace roots for a desktop session: the session's primary
// directory (Session.WorkingDir) plus N extra, fully writable roots.
//
// The semantics are ACP's — see agent.NormalizeAdditionalRoots and the prompt line
// BuildSystemPromptWithRoots emits: relative paths always resolve against the
// primary, additional roots are reached by absolute path. What the desktop adds is
// what an editor cannot be asked to do:
//
//   - PERSISTENCE. ACP is told the root set on every NewSession/LoadSession; the
//     desktop UI is the only input source, so the set lives in the session meta.
//   - VALIDATION, because there is a user to tell. Two rules go beyond the shared
//     syntax check, and both belong to this layer rather than to the shared one
//     (an editor handing ACP a spaced path is not doing anything wrong):
//     a root containing whitespace is rejected — @-references are
//     whitespace-delimited, so an absolute ref into such a root would be silently
//     truncated on send — and a root must exist and be a directory when added.
//   - STALENESS. A root can disappear later (an unmounted volume). It stays in the
//     session and is reported with exists=false; the prompt stops advertising it
//     and the @-file search skips it. Nothing is deleted behind the user's back.
//
// Design: docs/2026-09-11-desktop-multi-workspace-design.md

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/session"
)

// errNoSessionManager mirrors the message SetSessionWorkingDir has always returned
// when the app has no session manager at all (simulated mode).
var errNoSessionManager = errors.New("no session manager")

// SessionRootVO is one additional root as the UI sees it.
type SessionRootVO struct {
	Path string `json:"path"`
	// Exists is false when the directory has since been removed or unmounted: the
	// UI greys the entry out, and the prompt no longer advertises it.
	Exists bool `json:"exists"`
}

// SessionRootsVO is a session's full root set.
type SessionRootsVO struct {
	Primary    string          `json:"primary"`
	Additional []SessionRootVO `json:"additional"`
}

// GetSessionRoots returns the session's root set for the UI, with each additional
// root's current existence. It never fails: an unknown session yields the zero
// value, which the UI renders as "no roots yet".
func (s *AgentService) GetSessionRoots(id string) SessionRootsVO {
	primary, additional := s.desk.sessionRoots(id)
	vo := SessionRootsVO{Primary: primary, Additional: make([]SessionRootVO, 0, len(additional))}
	for _, dir := range additional {
		vo.Additional = append(vo.Additional, SessionRootVO{Path: dir, Exists: fileutil.IsDir(dir)})
	}
	return vo
}

// AddSessionRoots appends directories the user picked to the session's root set.
// Returns "ok", or a human-readable reason the UI shows instead (see the validation
// rules at the top of this file).
func (s *AgentService) AddSessionRoots(id string, dirs []string) string {
	if len(dirs) == 0 {
		return "没有选择目录"
	}
	primary, existing := s.desk.sessionRoots(id)
	if primary == "" {
		// Roots without a primary would leave relative paths resolving against the
		// process cwd, which is not a working directory at all. The UI keeps the
		// "pick a main directory first" state.
		return "请先设置主目录"
	}

	abs, err := validateRootPaths(dirs)
	if err != nil {
		return err.Error()
	}
	merged, err := agent.NormalizeAdditionalRoots(primary, append(existing, abs...))
	if err != nil {
		return err.Error()
	}
	if err := s.desk.updateSessionMeta(id, func(sess *session.Session) {
		sess.AdditionalDirs = merged
	}); err != nil {
		return err.Error()
	}
	return "ok"
}

// RemoveSessionRoot drops one additional root. The primary is not removable (the
// UI offers "change" instead), so a path equal to it is simply not found here.
func (s *AgentService) RemoveSessionRoot(id, dir string) string {
	abs, err := expandRootPath(dir)
	if err != nil {
		return err.Error()
	}
	_, existing := s.desk.sessionRoots(id)
	kept := make([]string, 0, len(existing))
	for _, root := range existing {
		if root != abs {
			kept = append(kept, root)
		}
	}
	if len(kept) == len(existing) {
		return "该目录不在附加目录中"
	}
	if err := s.desk.updateSessionMeta(id, func(sess *session.Session) {
		sess.AdditionalDirs = kept
	}); err != nil {
		return err.Error()
	}
	return "ok"
}

// sessionRoots reads a session's root set: from the per-session manager when the
// session is bound, otherwise from the stable manager — the same double path
// updateSessionMeta writes through, so a read never disagrees with a write. (The
// returned slice is a copy: callers must not mutate a session's own backing array.)
func (d *desktopApp) sessionRoots(id string) (primary string, additional []string) {
	d.mu.Lock()
	r := d.getRun(id)
	d.mu.Unlock()
	if r != nil && r.sm != nil {
		if cur := r.sm.Current(); cur != nil {
			return cur.WorkingDir, append([]string(nil), cur.AdditionalDirs...)
		}
	}
	if d.sm == nil {
		return "", nil
	}
	sess, err := d.sm.Load(id)
	if err != nil {
		return "", nil
	}
	return sess.WorkingDir, append([]string(nil), sess.AdditionalDirs...)
}

// promptRoots returns the additional roots to advertise in the system prompt: only
// the ones that still exist as directories. A stale root must not reach the model —
// it would be invited to use a path that is not there.
func (d *desktopApp) promptRoots(id string) []string {
	_, additional := d.sessionRoots(id)
	live := make([]string, 0, len(additional))
	for _, dir := range additional {
		if fileutil.IsDir(dir) {
			live = append(live, dir)
		}
	}
	return live
}

// updateSessionMeta applies mutate to a session's metadata and persists it. The
// read and the write happen on ONE snapshot (the per-session manager's current
// session when bound, else a freshly loaded copy), because UpdateMeta rewrites the
// whole meta.json — reading a stale copy and writing it back would clobber whatever
// happened in between (a title, the working directory).
func (d *desktopApp) updateSessionMeta(id string, mutate func(*session.Session)) error {
	d.mu.Lock()
	r := d.getRun(id)
	d.mu.Unlock()
	if r != nil && r.sm != nil {
		if cur := r.sm.Current(); cur != nil {
			mutate(cur)
			return r.sm.UpdateMeta(cur)
		}
	}
	if d.sm == nil {
		return errNoSessionManager
	}
	sess, err := d.sm.Load(id)
	if err != nil {
		return err
	}
	mutate(sess)
	return d.sm.UpdateMeta(sess)
}

// wideRootReason explains why a directory is too wide to be a workspace root, or
// returns "" when it is fine.
//
// The rule is deliberately narrow — exactly the filesystem root and the home
// directory, not a heuristic ("more than N files" would need the very walk this is
// meant to prevent). Both are allowed by the OS and useless as an agent workspace:
// the @-file index would cover the whole machine (or every dotfile the user owns),
// the picker would be noise, and relative paths would resolve against a directory
// that is not a project.
func wideRootReason(path string) string {
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) {
		return "不能把根目录 / 作为工作目录：@ 补全会索引整台机器。请选择具体的项目目录。"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && clean == filepath.Clean(home) {
		return "不能把家目录本身作为工作目录：@ 补全会索引整个 " + clean + "（含全部 dotfile 与无关目录）。请选择具体的项目目录。"
	}
	return ""
}

// defaultWorkspaceFor picks the directory a NEW session starts in: the one the user
// last chose (desktop_ui.json), else the most recently updated one from the session
// list. Wide roots are skipped in both — before this rule existed every session got
// $HOME, so inheriting blindly would just recreate the problem. "" means "none":
// the session starts without a workspace and the composer's chip asks for one.
func (d *desktopApp) defaultWorkspaceFor() string {
	if last := loadUIState().LastWorkspace; last != "" && fileutil.IsDir(last) && wideRootReason(last) == "" {
		return last
	}
	if d.sm == nil {
		return ""
	}
	sessions, err := d.sm.List()
	if err != nil {
		return ""
	}
	best := ""
	var bestAt time.Time
	for _, sess := range sessions {
		if sess.WorkingDir == "" || !fileutil.IsDir(sess.WorkingDir) || wideRootReason(sess.WorkingDir) != "" {
			continue
		}
		if sess.UpdatedAt.After(bestAt) {
			best, bestAt = sess.WorkingDir, sess.UpdatedAt
		}
	}
	return best
}

// rememberWorkspace records the user's explicit choice, so the next new session
// starts where the last one was pointed.
func rememberWorkspace(dir string) {
	st := loadUIState()
	if st.LastWorkspace == dir {
		return
	}
	st.LastWorkspace = dir
	saveUIState(st)
}

// validateRootPaths applies the desktop-only rules to the paths a user picked and
// returns them as the absolute paths that will be stored. Validation runs on the
// STORED form, so "/Users/x/My Projects/../lib" cannot slip a space past the check
// by being cleaned afterwards.
func validateRootPaths(dirs []string) ([]string, error) {
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		abs, err := expandRootPath(dir)
		if err != nil {
			return nil, err
		}
		if strings.ContainsAny(abs, " \t\n\r") {
			return nil, fmt.Errorf("%s 含空白字符：@ 引用按空白切分，无法表达这样的路径（请改用不含空白的目录）", abs)
		}
		if reason := wideRootReason(abs); reason != "" {
			return nil, errors.New(reason)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("%s 不存在或无法访问", abs)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s 不是目录", abs)
		}
		out = append(out, abs)
	}
	return out, nil
}

// expandRootPath turns a path into the absolute, cleaned form that gets stored:
// "~" and "~/x" expand through the home directory (the picker always returns
// absolute paths, but the API is also called with hand-written ones), everything
// else goes through filepath.Abs.
func expandRootPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("路径为空")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法定位用户主目录：%w", err)
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("无法解析路径 %s：%w", p, err)
	}
	return abs, nil
}
