package main

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
)

// Plan tool: the desktop's read side. SavePlan writes one JSON document per plan per
// session (<title-slug>-<sessionID>.json under <project>/.tachi/plans), and this file
// turns that into the payload the footer's plan panel renders.
//
// The panel reads the FILE rather than reconstructing the plan from the transcript, for
// the same reason review findings are read from their own record: the file is the
// document (SavePlan overwrites it in place, so "the plan" is always one file), which
// means the panel shows the same thing after a restart with no new persistence — and it
// shows plans saved by a frontend that was not this one.

// PlanStepVO is one step of a plan as the panel renders it.
type PlanStepVO struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// PlanVO is the plan panel's payload: the newest plan saved for this session.
type PlanVO struct {
	Title   string       `json:"title"`
	Content string       `json:"content"`
	Steps   []PlanStepVO `json:"steps"`
	// Path is where the plan lives, so the panel can offer the same 预览/打开 the
	// rest of the app uses for files.
	Path string `json:"path,omitempty"`
	// UpdatedAt is the file's mtime (RFC3339): a plan is a document you revisit, and
	// "when was this last touched" is what tells you whether it still describes the
	// work in front of you.
	UpdatedAt string `json:"updatedAt,omitempty"`
	// Others counts the further plans this session has saved under other titles.
	Others int `json:"others,omitempty"`
	// Note explains an empty plan (none yet, unreadable) — an empty panel and a panel
	// with nothing to say are different states.
	Note string `json:"note,omitempty"`
}

// GetPlan returns this session's newest plan.
func (s *AgentService) GetPlan(sessionID string) PlanVO {
	if sessionID == "" {
		return PlanVO{Note: "没有会话"}
	}
	paths := s.desk.planFilesFor(sessionID)
	if len(paths) == 0 {
		return PlanVO{Note: "这个会话还没有计划"}
	}

	plan, err := tools.PlanFromFile(paths[0])
	if err != nil {
		return PlanVO{Path: paths[0], Note: "计划文件读不出来：" + err.Error()}
	}

	vo := PlanVO{
		Title:   plan.Title,
		Content: plan.Content,
		Path:    paths[0],
		Others:  len(paths) - 1,
	}
	if plan.Title == "" {
		vo.Title = filepath.Base(paths[0])
	}
	for _, st := range plan.Steps {
		vo.Steps = append(vo.Steps, PlanStepVO{Content: st.Content, Status: st.Status})
	}
	if info, err := os.Stat(paths[0]); err == nil {
		vo.UpdatedAt = info.ModTime().Format(time.RFC3339)
	}
	if len(vo.Steps) == 0 {
		vo.Note = "计划里没有步骤"
	}
	return vo
}

// planFilesFor lists this session's plan files, newest first.
//
// Every directory the session could have written a plan into is searched: its roots
// (SavePlan resolves the plan directory from the session's working directory) and the
// global fallback (<baseDir>/plans) it uses when the session has no workspace yet. The
// file name ends with the session ID, which is what keeps another session's plans — and
// the global directory's — out of the result.
func (d *desktopApp) planFilesFor(id string) []string {
	type candidate struct {
		path string
		mod  time.Time
	}
	var found []candidate
	seen := map[string]bool{}

	for _, dir := range d.planDirs(id) {
		matches, err := filepath.Glob(filepath.Join(dir, "*-"+id+".json"))
		if err != nil {
			continue // a malformed pattern (never for a session ID) skips that dir
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
			info, err := os.Stat(m)
			if err != nil {
				continue
			}
			found = append(found, candidate{path: m, mod: info.ModTime()})
		}
	}

	sort.Slice(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })
	paths := make([]string, 0, len(found))
	for _, c := range found {
		paths = append(paths, c.path)
	}
	return paths
}

// planDirs is where this session's plans may live, in search order.
func (d *desktopApp) planDirs(id string) []string {
	primary, additional := d.sessionRoots(id)
	roots := append([]string{primary}, additional...)

	var dirs []string
	for _, root := range roots {
		if root != "" {
			dirs = append(dirs, filepath.Join(root, ".tachi", "plans"))
		}
	}
	// SavePlan's fallback when the session has no working directory — also where a
	// plan saved before the user picked a workspace still lives.
	return append(dirs, filepath.Join(config.BaseDir(), "plans"))
}
