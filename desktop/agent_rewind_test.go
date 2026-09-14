package main

import (
	"testing"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/checkpoint"
)

// TestRewindPreviewVOCarriesEverythingTheCardShows pins the binding between the
// agent's preview and the confirmation card. The card can only show what the VO
// carries, so a field dropped in this one function goes missing in the UI with no
// compile error and no failing Go test anywhere else — which is exactly how the
// irreversible list (a git commit a rewind cannot undo) reached the card empty:
// it was computed, and then read from the wrong field.
//
// Written with the standard library like every other test in this module (testify
// is not a dependency here, and importing it for one test rewrites go.mod).
func TestRewindPreviewVOCarriesEverythingTheCardShows(t *testing.T) {
	commit := "git commit 在 /repo（HEAD a1b2c3d4 → e5f6a7b8）"
	vo := toRewindPreviewVO(agent.RewindPreview{
		Turn:           2,
		Target:         2,
		UserText:       "把导出改成流式",
		Records:        47,
		APIRecords:     12,
		Irreversible:   []string{commit},
		FilesUnchanged: true,
		Roots: []checkpoint.RootPreview{{
			Root:    "/repo",
			Added:   []string{"new.txt"},
			Changed: []string{"a.txt"},
			Deleted: []string{"gone.txt"},
			Stat:    "3 files changed",
		}},
	})

	if vo.Turn != 2 || vo.Target != 2 || vo.UserText != "把导出改成流式" {
		t.Errorf("turn/target/userText = %d/%d/%q", vo.Turn, vo.Target, vo.UserText)
	}
	if !vo.FilesUnchanged {
		t.Errorf("the benign workspace answer must reach the card: %+v", vo)
	}
	if len(vo.Irreversible) != 1 || vo.Irreversible[0] != commit {
		t.Errorf("irreversible = %v, want [%s] (the card renders this as 无法撤销)", vo.Irreversible, commit)
	}
	if vo.NoFiles != "" || vo.Blocked != "" {
		t.Errorf("noFiles/blocked must stay empty here: %q / %q", vo.NoFiles, vo.Blocked)
	}
	if len(vo.Roots) != 1 {
		t.Fatalf("roots = %+v, want one", vo.Roots)
	}
	r := vo.Roots[0]
	if r.Root != "/repo" || r.Stat != "3 files changed" {
		t.Errorf("root = %+v", r)
	}
	if len(r.Added) != 1 || r.Added[0] != "new.txt" {
		t.Errorf("added = %v — what the rewind would DELETE is what a reader must see", r.Added)
	}
	if len(r.Changed) != 1 || r.Changed[0] != "a.txt" {
		t.Errorf("changed = %v", r.Changed)
	}
	if len(r.Deleted) != 1 || r.Deleted[0] != "gone.txt" {
		t.Errorf("deleted = %v", r.Deleted)
	}

	// The other two outcomes of the workspace half travel as the same fields the
	// card switches on, so a refusal can never be rendered as a success.
	unknown := toRewindPreviewVO(agent.RewindPreview{Turn: 3, NoFiles: "没有安装 git"})
	if unknown.NoFiles != "没有安装 git" || len(unknown.Roots) != 0 {
		t.Errorf("an unrecorded file state must arrive as noFiles: %+v", unknown)
	}
	blocked := toRewindPreviewVO(agent.RewindPreview{Turn: 4, Blocked: "第 4 轮没有检查点（可能已被裁剪）"})
	if blocked.Blocked == "" {
		t.Errorf("a refusal must arrive as blocked: %+v", blocked)
	}
}
