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

// turnChangesFromCheckpoint maps the checkpoint's own summary into the binding's shape. A nil
// summary (the turn wrote nothing) maps to nil — the callers' "no numbers for this turn" case,
// rather than a VO of zeroes that would read as "it changed nothing".
func turnChangesFromCheckpoint(d *checkpoint.TurnDiff) *TurnChangesVO {
	if d == nil {
		return nil
	}
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
// from the trees themselves, which is what lets a file the shell wrote appear at all. They are
// also the reason the fallback stops rather than widening: an EMPTY list means this turn
// declared nothing (its changes came from a shell command), and GetTurnDiff reads an empty list
// as "the whole tree" — a different question, whose answer would be labelled 本轮改动.
func (s *AgentService) GetTurnChanges(sessionID string, turn int, paths []string) TurnDiffVO {
	root, additional := s.desk.sessionRoots(sessionID)
	labels := rootLabels(root, additional)
	if ag, refuse := s.desk.agentOf(sessionID); refuse == "" && ag != nil && turn != 0 {
		if diffs, ok, err := ag.TurnDiff(context.Background(), turn); err == nil && ok {
			vo := TurnDiffVO{Root: root, Source: changeSourceCheckpoint}
			// One root's text at a time, each labelled with its root: the paths inside are
			// relative to it, so a merged parse would hand the panel two roots' same-named
			// files as one file (and 打开文件 would resolve it against the primary root).
			for _, rd := range diffs {
				vo.Files = append(vo.Files, filesFromUnified(rd.Text, rd.Root, rootLabelFor(labels, rd.Root))...)
			}
			// A file the working tree no longer has where this turn left it — a later turn,
			// the user, or a commit — is marked rather than silently shown as if the panel
			// were the disk. Keyed by (root, path): the path alone names a different file in
			// each root.
			moved := map[string]bool{}
			for _, rc := range ag.TurnChangedSince(context.Background(), turn) {
				for _, p := range rc.Paths {
					moved[changedKey(rc.Root, p)] = true
				}
			}
			if len(moved) > 0 {
				for i := range vo.Files {
					vo.Files[i].ChangedSince = moved[changedKey(vo.Files[i].Root, vo.Files[i].Path)]
				}
			}
			truncateDiff(&vo)
			return vo
		}
		// No diff TEXT — but the checkpoint may still know the turn changed NOTHING (its two
		// trees are identical). That is an answer of its own, and it must not fall through to
		// the working tree, whose changes belong to some later turn under this turn's heading.
		if empty, ok := emptyPairAnswer(root, ag.TurnSummary(context.Background(), turn)); ok {
			return empty
		}
	}

	if len(paths) == 0 {
		// Nothing to fall back to, and the whole tree is not the answer: this turn declared no
		// files (a shell command wrote them), so "本轮改动" has nothing of its own to show.
		return TurnDiffVO{Root: root, Source: changeSourceTools,
			Note: "这一轮没有可显示的改动：检查点里没有它的记录，工具调用也没有声明过文件"}
	}

	vo := s.GetTurnDiff(sessionID, paths)
	vo.Source = changeSourceTools
	vo.Note = joinNotes("本轮改动来自工具调用（检查点不可用），与 git HEAD 对照", vo.Note)
	return vo
}

// changedKey identifies a file for the 「之后又改过」 lookup: the same relative path names a
// different file in each root, so the root is part of the key. NUL cannot appear in either.
func changedKey(root, path string) string { return root + "\x00" + path }

// emptyPairAnswer is what to show when the turn has no diff text but the checkpoint knows it
// changed nothing: the answer, and whether it applies. TurnSummary's three states are the whole
// rule — nil (no checkpoint for the turn, or it wrote nothing at all), a reason (its end state
// was refused) and zero files (the two trees are equal) — and only the last one is the statement
// 「本轮没有改动」.
func emptyPairAnswer(root string, sum *checkpoint.TurnDiff) (TurnDiffVO, bool) {
	if sum == nil || sum.Skipped != "" || sum.Files > 0 {
		return TurnDiffVO{}, false
	}
	return TurnDiffVO{Root: root, Source: changeSourceCheckpoint,
		Note: "这一轮没有改动：检查点里它前后的两棵树一致"}, true
}

// filesFromUnified parses a unified diff into the panel's per-file shape, labelling every file
// with the root its paths are relative to. It is the same parser the working-tree path uses:
// both sides are unified diffs, the difference is only where they came from.
//
// The root travels WITH each file rather than being remembered by the caller, because a diff
// is parsed one root at a time and the result is merged: a file that lost its root would be
// resolved against the primary root, which is exactly how two roots' same-named files used to
// be confused for one another.
func filesFromUnified(text, root, label string) []FileDiffVO {
	parsed := linediff.ParseUnified(text)
	out := make([]FileDiffVO, 0, len(parsed))
	for _, fd := range parsed {
		out = append(out, FileDiffVO{
			Path: fd.Path, OldPath: fd.OldPath,
			Created: fd.Created, Deleted: fd.Deleted, Binary: fd.Binary,
			Hunks: fd.Hunks, Added: fd.Added, Removed: fd.Removed,
			Root: root, RootLabel: label,
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
