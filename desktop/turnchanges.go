package main

// What a turn changed, for the footer chips and the changes panel.
//
// Two sources, in this order:
//
//   - the CHECKPOINT: the turn's own two trees (where it started, where it ended — see
//     agent/checkpoint). Exact: a file a shell command wrote is in it, created and deleted
//     files are exact, and the answer does not move when the working tree does.
//   - the TOOL CALLS: what Edit/Write declared they were about to do. The fallback when
//     there are no checkpoints at all — the feature is off, git is missing, the tree is over
//     its guard, or the run is a one-off — and the only thing that ever existed before.
//
// A reader always learns WHICH one it got (`Source`). A number that looks the same while
// meaning something narrower is worse than a smaller number that says what it covers, and
// the tool-call numbers have always been narrower: they cannot see a shell command at all.

import (
	"context"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/checkpoint"
	"github.com/monsterxx03/tachi/pkg/linediff"
)

const (
	// changeSourceCheckpoint is the turn's own trees: exact, frozen, shell commands included.
	changeSourceCheckpoint = "checkpoint"
	// changeSourceTools is what the tool calls declared. Narrower by construction.
	changeSourceTools = "tools"
)

// TurnChangesVO is the footer chip's data for one turn.
type TurnChangesVO struct {
	Files   int `json:"files"`
	Added   int `json:"added"`
	Removed int `json:"removed"`
	// Source is changeSourceCheckpoint or changeSourceTools — the chip's tooltip says which,
	// because the difference is exactly "did this count the shell command or not".
	Source string `json:"source"`
	// Note, when set, is why the checkpoint numbers are missing (a guard tripped at the turn
	// end, git failed, ...). It travels with the fallback so the reader is not left guessing
	// whether the number is complete.
	Note string `json:"note,omitempty"`
}

// turnStamp is a turn's chip data, attached to the session records that BELONG to it, so a
// transcript loaded from disk shows the same numbers a live one did — without a second call
// and without the frontend having to map record indexes to turn numbers itself.
//
// It stays unexported on purpose: it never appears in a bound method's signature, so it has
// no business in the generated models the frontend imports.
type turnStamp struct {
	Turn    int
	Changes *TurnChangesVO
}

// sessionTurnStamps indexes a session's turns by the record that BEGINS them, for one page
// load: a single manifest read, no git. buildSessionMessages turns that into "the turn this
// record is part of" (see stampBefore).
func sessionTurnStamps(r *sessionRun) map[int]turnStamp {
	if r == nil || r.agent == nil {
		return nil
	}
	turns, err := r.agent.RewindTurns(context.Background())
	if err != nil || len(turns) == 0 {
		return nil
	}
	out := make(map[int]turnStamp, len(turns))
	for _, t := range turns {
		var changes *TurnChangesVO
		if t.Diff != nil {
			changes = turnChangesFromCheckpoint(t.Diff)
		}
		out[t.Records] = turnStamp{Turn: t.Turn, Changes: changes}
	}
	return out
}

// stampBefore returns the turn in force at a record index: the newest boundary at or before
// it. A page is the newest `limit` records, so a turn with more records than a page has its
// opening record on an earlier page — resolving the boundary exactly (map lookup only) would
// silently leave the card of a long turn without its numbers, which is the turn whose footer
// and diff panel most need them (a shell-heavy turn is both long and undeclared).
func stampBefore(stamps map[int]turnStamp, index int) turnStamp {
	bestAt := -1
	var best turnStamp
	for at, s := range stamps {
		if at <= index && at > bestAt {
			bestAt, best = at, s
		}
	}
	return best
}

// changeSummaryVO reads what a turn changed from its checkpoint, or nil when there is
// nothing to read (no checkpoint for it, it wrote nothing, or its end state was refused).
// Never an error: the caller falls back to the tool-call fragments.
func changeSummaryVO(ag *agent.AIAgent, turn int) *TurnChangesVO {
	if ag == nil || turn == 0 {
		return nil
	}
	diff := ag.TurnSummary(context.Background(), turn)
	if diff == nil {
		return nil
	}
	return turnChangesFromCheckpoint(diff)
}

// turnChangesFromCheckpoint maps the checkpoint's own summary into the binding's shape.
func turnChangesFromCheckpoint(d *checkpoint.TurnDiff) *TurnChangesVO {
	return &TurnChangesVO{
		Files:   d.Files,
		Added:   d.Added,
		Removed: d.Removed,
		Source:  changeSourceCheckpoint,
		Note:    d.Skipped,
	}
}

// GetTurnChanges is the changes panel's entry: the frozen diff of what a TURN changed, or —
// when the checkpoints have no pair for it — the working-tree diff of the paths the tool
// calls declared, which is both the pre-checkpoint behaviour and all a session without
// checkpoints has. TurnDiffVO.Source says which of the two came back.
//
// paths are the caller's last resort, not its input: with a pair of trees the scope comes
// from the trees themselves, which is what lets a file the shell wrote appear at all.
func (s *AgentService) GetTurnChanges(sessionID string, turn int, paths []string) TurnDiffVO {
	root, _ := s.desk.sessionRoots(sessionID)
	if ag, refuse := s.desk.agentOf(sessionID); refuse == "" && ag != nil && turn != 0 {
		if text, ok, err := ag.TurnDiff(context.Background(), turn); err == nil && ok {
			vo := TurnDiffVO{Root: root, Source: changeSourceCheckpoint}
			vo.Files = filesFromUnified(text)
			// A file the working tree no longer has where this turn left it — a later turn,
			// the user, or a commit — is marked rather than silently shown as if the panel
			// were the disk.
			if moved := ag.TurnChangedSince(context.Background(), turn); len(moved) > 0 {
				for i := range vo.Files {
					vo.Files[i].ChangedSince = moved[vo.Files[i].Path]
				}
			}
			truncateDiff(&vo)
			return vo
		}
	}

	vo := s.GetTurnDiff(sessionID, paths)
	vo.Source = changeSourceTools
	vo.Note = joinNotes("本轮改动来自工具调用（检查点不可用），与 git HEAD 对照", vo.Note)
	return vo
}

// filesFromUnified parses a unified diff into the panel's per-file shape. It is the same
// parser the working-tree path uses: both sides are unified diffs, the difference is only
// where they came from.
func filesFromUnified(text string) []FileDiffVO {
	parsed := linediff.ParseUnified(text)
	out := make([]FileDiffVO, 0, len(parsed))
	for _, fd := range parsed {
		out = append(out, FileDiffVO{
			Path: fd.Path, OldPath: fd.OldPath,
			Created: fd.Created, Deleted: fd.Deleted, Binary: fd.Binary,
			Hunks: fd.Hunks, Added: fd.Added, Removed: fd.Removed,
		})
	}
	return out
}

// pathsFromUnified lists the files a unified diff touches, in the order git printed them.
// It is how the review's file list is taken from the SAME diff the reviewer will read, so
// the two can never disagree about what is under review.
func pathsFromUnified(text string) []string {
	parsed := linediff.ParseUnified(text)
	out := make([]string, 0, len(parsed))
	for _, fd := range parsed {
		out = append(out, fd.Path)
	}
	return out
}

// truncateDiff caps what travels to the webview for one panel open, in place, and says so.
func truncateDiff(vo *TurnDiffVO) {
	total := countHunks(vo.Files)
	if total <= maxDiffHunks {
		return
	}
	trimmed := vo.Files[:0]
	kept := 0
	for _, f := range vo.Files {
		if kept >= maxDiffHunks {
			break
		}
		if kept+len(f.Hunks) > maxDiffHunks {
			f.Hunks = f.Hunks[:maxDiffHunks-kept]
			f.Added, f.Removed = linediff.Counts(f.Hunks)
		}
		kept += len(f.Hunks)
		trimmed = append(trimmed, f)
	}
	vo.Files = trimmed
	vo.Note = joinNotes(vo.Note, "改动很大，这里只显示了一部分")
}
