package main

// Tool-call diffs for the transcript: what a single EditFile / WriteFile call set out
// to change, in the shape the frontend renders.
//
// The derivation is the SAME one the ACP stream uses (agent/tools.FileChangeForTool),
// so desktop and Zed cannot disagree about what a call did; only the last step
// differs — Zed gets acp.ToolDiffContent, the desktop gets hunks. Computing those in
// Go keeps the frontend a renderer and puts the diff arithmetic where it is
// unit-testable.
//
// Design: docs/2026-09-11-desktop-diff-review-design.md

import (
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/pkg/linediff"
)

// FileChangeVO carries one tool call's change to the frontend.
type FileChangeVO struct {
	Path string `json:"path"`
	// Hunks are render-ready lines (context/add/del) carrying fragment-relative line
	// numbers, which the UI deliberately does not display: a fragment has no position
	// in the file, and showing 1,2,3 would read as file coordinates (see the linediff
	// package comment for why they are computed anyway).
	Hunks []linediff.Hunk `json:"hunks"`
	// Added/Removed are FRAGMENT line counts, not git numstat.
	Added   int `json:"added"`
	Removed int `json:"removed"`
	// ReplaceAll marks a call that replaced every occurrence. A fragment diff can only
	// show one block against another, so the card labels it rather than implying the
	// changes were paired up.
	ReplaceAll bool `json:"replaceAll"`
}

// changeVO derives the change a tool call carries, or nil when it changes no file's
// text. It never fails: arguments it cannot parse are simply "nothing to show".
//
// It answers "what did this call set out to change", never whether it succeeded —
// when a tool_call is recorded the result has not happened yet. The frontend renders
// the diff only once the call is done and successful (docs §5.1).
func changeVO(name, argsJSON string) *FileChangeVO {
	fc, ok := tools.FileChangeForTool(name, argsJSON)
	if !ok {
		return nil
	}
	hunks := linediff.Fragments(fc.OldText, fc.NewText)
	added, removed := linediff.Counts(hunks)
	return &FileChangeVO{
		Path:       fc.Path,
		Hunks:      hunks,
		Added:      added,
		Removed:    removed,
		ReplaceAll: fc.ReplaceAll,
	}
}
