package main

// Review findings: the shape `ReportFinding` produces, and the one place its recorded
// arguments are parsed.
//
// A review runs as a one-off fork (agent.RunOneOffStream), so its findings are NOT in the
// conversation history: they live in the run's own record under
// <SessionDir>/<id>/oneoff/, which is what makes them readable both right after a review
// and after a restart, with no extra persistence. desktop/oneoff.go collects them per RUN
// (the run the reader selected); this file owns the parse they share.
//
// Design: docs/2026-09-11-desktop-diff-review-design.md §12.3,
//         docs/2026-09-12-desktop-oneoff-panel-design.md §5.3

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/monsterxx03/tachi/agent/tools"
)

// FindingVO is one review finding as the panel renders it.
type FindingVO struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	EndLine    int    `json:"endLine,omitempty"`
	Severity   string `json:"severity"`
	Category   string `json:"category,omitempty"`
	Text       string `json:"text"`
	Suggestion string `json:"suggestion,omitempty"`
}

// findingFromToolCall parses one ReportFinding call's arguments into the panel's shape.
//
// The arguments reach a reader in one of two shapes: a session message carries the object
// itself, while a one-off record stores the raw JSON as a STRING. Both must be accepted — a
// reader that only takes the object silently finds nothing in the very file it exists to
// read. (That bug shipped once; see the note in readReviewFindings.)
func findingFromToolCall(args any) (FindingVO, bool) {
	raw, err := toolArgsJSON(args)
	if err != nil {
		return FindingVO{}, false
	}
	var p tools.ReportFindingParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return FindingVO{}, false
	}
	return FindingVO{
		Path: p.Path, Line: p.Line, EndLine: p.EndLine,
		Severity: p.Severity, Category: p.Category, Text: p.Text, Suggestion: p.Suggestion,
	}, true
}

// toolArgsJSON normalizes a recorded call's arguments to the JSON bytes of the OBJECT,
// whatever the writer stored: a plain string, a json.RawMessage, a decoded map, or a value
// that is itself a quoted JSON string.
func toolArgsJSON(args any) ([]byte, error) {
	var raw []byte
	switch v := args.(type) {
	case nil:
		return nil, errors.New("没有参数")
	case string:
		raw = []byte(v)
	case json.RawMessage:
		raw = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("空参数")
	}
	// The record's shape: arguments stored as a JSON string holding the object.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		raw = []byte(s)
	}
	return raw, nil
}

