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

// PlanEntryVO is one row of the plan panel's list: enough to choose between a session's
// plans without loading them all. A session accumulates plans (one file per plan), so the
// list is the difference between "the newest plan" and "the plans".
type PlanEntryVO struct {
	Title     string `json:"title"`
	Path      string `json:"path"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// PlanVO is the plan panel's payload: the plan being shown, plus the session's list.
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
	// Plans lists every plan this session still has (newest first), the shown one
	// included — the UI needs to say "3 of 5" without a second round-trip.
	Plans []PlanEntryVO `json:"plans,omitempty"`
	// Note explains an empty plan (none yet, unreadable) — an empty panel and a panel
	// with nothing to say are different states.
	Note string `json:"note,omitempty"`
}

// GetPlan returns one of this session's plans — the newest when path is empty, otherwise
// the plan at path (which must be one of this session's own plan files).
//
// The list travels with the payload: a session can hold several plans, and the panel
// shows both the one you are reading and how many others are there.
func (s *AgentService) GetPlan(sessionID, path string) PlanVO {
	if sessionID == "" {
		return PlanVO{Note: "没有会话"}
	}
	paths := s.desk.planFilesFor(sessionID)
	if len(paths) == 0 {
		return PlanVO{Note: "这个会话还没有计划"}
	}

	chosen := paths[0]
	if path != "" {
		if !containsPath(paths, path) {
			return PlanVO{Plans: s.planList(paths), Note: "这份计划不属于这个会话"}
		}
		chosen = path
	}

	plan, err := tools.PlanFromFile(chosen)
	if err != nil {
		return PlanVO{Path: chosen, Plans: s.planList(paths), Note: "计划文件读不出来：" + err.Error()}
	}

	vo := PlanVO{
		Title:   plan.Title,
		Content: plan.Content,
		Path:    chosen,
		Plans:   s.planList(paths),
	}
	if plan.Title == "" {
		vo.Title = filepath.Base(chosen)
	}
	for _, st := range plan.Steps {
		vo.Steps = append(vo.Steps, PlanStepVO{Content: st.Content, Status: st.Status})
	}
	if info, err := os.Stat(chosen); err == nil {
		vo.UpdatedAt = info.ModTime().Format(time.RFC3339)
	}
	if len(vo.Steps) == 0 {
		vo.Note = "计划里没有步骤"
	}
	return vo
}

// DeletePlan removes one of this session's plan files and returns "ok".
//
// The path comes from the frontend, so it is validated against this session's own plan
// files first: a binding that deletes whatever it is handed is an arbitrary-file-delete
// primitive, and this one is reachable from a UI listing.
func (s *AgentService) DeletePlan(sessionID, path string) string {
	if sessionID == "" {
		return "没有会话"
	}
	if path == "" {
		return "没有指定要删除的计划"
	}
	if !containsPath(s.desk.planFilesFor(sessionID), path) {
		return "这份计划不属于这个会话，没有删除"
	}
	if err := os.Remove(path); err != nil {
		return "删除失败：" + err.Error()
	}
	return "ok"
}

// planList summarizes every plan of this session, newest first.
func (s *AgentService) planList(paths []string) []PlanEntryVO {
	entries := make([]PlanEntryVO, 0, len(paths))
	for _, p := range paths {
		e := PlanEntryVO{Path: p}
		if info, err := os.Stat(p); err == nil {
			e.UpdatedAt = info.ModTime().Format(time.RFC3339)
		}
		if plan, err := tools.PlanFromFile(p); err == nil {
			e.Title = plan.Title
			e.Total = len(plan.Steps)
			for _, st := range plan.Steps {
				if st.Status == "completed" {
					e.Done++
				}
			}
		}
		if e.Title == "" {
			e.Title = filepath.Base(p)
		}
		entries = append(entries, e)
	}
	return entries
}

func containsPath(paths []string, path string) bool {
	for _, p := range paths {
		if p == path {
			return true
		}
	}
	return false
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
