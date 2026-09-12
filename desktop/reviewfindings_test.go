package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// writeReviewTranscript writes one review transcript under a temp session dir and
// returns the session id. Nothing touches the real ~/.tachi: config.SetBaseDir points
// the session dir at a temp dir first.
func writeReviewTranscript(t *testing.T, name string, messages []map[string]any) string {
	t.Helper()
	sid := "review-test-session"
	base, err := config.SessionDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, sid, "oneoff")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	//nolint:errcheck // test file

	defer f.Close()
	for _, m := range messages {
		line, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	return sid
}

// TestGetReviewFindings reads findings back out of the review transcript: a review is a
// one-off run, so this file — not the conversation history — is where its findings live.
func TestGetReviewFindings(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	svc := &AgentService{desk: newTestApp()}

	sid := writeReviewTranscript(t, "review-20260912-010101-aaaa.jsonl", []map[string]any{
		{"type": "meta", "kind": "review"},
		{"type": "user", "content": "## Context to gather..."},
		{"type": "tool_call", "name": "ReportFinding", "args": map[string]any{
			"path": "main.go", "line": 17, "severity": "bug", "category": "Correctness",
			"text": "超时常量与注释不一致", "suggestion": "提取常量",
		}},
		// The shape the one-off recorder actually writes: args as a JSON STRING.
		{"type": "tool_call", "name": "ReportFinding", "args": `{"path":"CHANGELOG.md","line":3,"end_line":5,"severity":"warn","text":"缺影响面"}`},
		{"type": "tool_call", "name": "Bash", "args": map[string]any{"command": "git diff HEAD"}},
		{"type": "assistant", "content": "评审完成"},
	})

	vo := svc.GetReviewFindings(sid)
	if vo.Note != "" {
		t.Errorf("Note = %q, want none", vo.Note)
	}
	if len(vo.Findings) != 2 {
		t.Fatalf("findings = %+v, want two (the Bash call is not a finding)", vo.Findings)
	}
	first := vo.Findings[0]
	if first.Path != "main.go" || first.Line != 17 || first.Severity != "bug" || first.Suggestion != "提取常量" {
		t.Errorf("first finding = %+v", first)
	}
	if vo.Findings[1].EndLine != 5 {
		t.Errorf("range lost: %+v", vo.Findings[1])
	}
	if vo.Review != "review-20260912-010101-aaaa.jsonl" {
		t.Errorf("Review = %q, want the transcript name", vo.Review)
	}
}

// TestGetReviewFindingsOnlyTheNewest: an older review's findings must not outlive it —
// "what did the LAST review say" is the scope that avoids haunting the panel.
func TestGetReviewFindingsOnlyTheNewest(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	svc := &AgentService{desk: newTestApp()}

	sid := writeReviewTranscript(t, "review-20260912-010101-aaaa.jsonl", []map[string]any{
		{"type": "tool_call", "name": "ReportFinding", "args": map[string]any{"path": "old.go", "line": 1, "severity": "bug", "text": "旧评审"}},
	})
	writeReviewTranscript(t, "review-20260912-020202-bbbb.jsonl", []map[string]any{
		{"type": "tool_call", "name": "ReportFinding", "args": map[string]any{"path": "new.go", "line": 2, "severity": "info", "text": "新评审"}},
	})

	vo := svc.GetReviewFindings(sid)
	if len(vo.Findings) != 1 || vo.Findings[0].Text != "新评审" {
		t.Fatalf("findings = %+v, want only the newest review's", vo.Findings)
	}
}

// TestGetReviewFindingsNone: no review yet is a sentence, not an error.
func TestGetReviewFindingsNone(t *testing.T) {
	config.SetBaseDir(t.TempDir())
	svc := &AgentService{desk: newTestApp()}

	vo := svc.GetReviewFindings("never-reviewed")
	if len(vo.Findings) != 0 || vo.Note == "" {
		t.Errorf("vo = %+v, want an explanatory note", vo)
	}
}

// The report path is recorded in the meta header, because the report file and the record
// live in different trees and are otherwise related only by a timestamp in their names.
// With it the panel can hand the report to the reader — and can tell "found nothing" apart
// from "recorded nothing".
func TestGetReviewFindingsCarriesReport(t *testing.T) {
	base := t.TempDir()
	config.SetBaseDir(base)
	svc := &AgentService{desk: newTestApp()}

	report := filepath.Join(base, "reviews", "20260912-010101", "round-1-reviewer-mock.md")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("# 评审报告\n\n正文"), 0o644); err != nil {
		t.Fatal(err)
	}

	sid := writeReviewTranscript(t, "review-20260912-010101-aaaa.jsonl", []map[string]any{
		{"type": "meta", "kind": "review", "extra": map[string]string{"report": report}},
		{"type": "tool_call", "name": "ReportFinding", "args": map[string]any{
			"path": "main.go", "line": 3, "severity": "warn", "text": "有点问题"}},
	})

	vo := svc.GetReviewFindings(sid)
	if vo.Report != report {
		t.Errorf("Report = %q, want %q", vo.Report, report)
	}
	if vo.Note != "" {
		t.Errorf("Note = %q, want none when findings were recorded", vo.Note)
	}
}

// A review that wrote its report but called ReportFinding zero times is NOT the same fact
// as a review that found nothing: its opinions exist only in prose, so the panel must say
// that rather than report 没有问题.
func TestGetReviewFindingsReportWithoutFindings(t *testing.T) {
	base := t.TempDir()
	config.SetBaseDir(base)
	svc := &AgentService{desk: newTestApp()}

	report := filepath.Join(base, "reviews", "20260912-010101", "round-1-reviewer-mock.md")
	if err := os.MkdirAll(filepath.Dir(report), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("# 报告"), 0o644); err != nil {
		t.Fatal(err)
	}

	sid := writeReviewTranscript(t, "review-20260912-010101-aaaa.jsonl", []map[string]any{
		{"type": "meta", "kind": "review", "extra": map[string]string{"report": report}},
		{"type": "assistant", "content": "详见报告"},
	})

	vo := svc.GetReviewFindings(sid)
	if len(vo.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", vo.Findings)
	}
	if !strings.Contains(vo.Note, "结构化") {
		t.Errorf("Note = %q, want it to say no structured finding was recorded", vo.Note)
	}
	if vo.Report != report {
		t.Errorf("the report must still be offered: %q", vo.Report)
	}
}

// A recorded path with nothing at it (the round died before writing) is not evidence of a
// report: that case keeps the plain "found nothing" wording.
func TestGetReviewFindingsReportPathWithoutFile(t *testing.T) {
	base := t.TempDir()
	config.SetBaseDir(base)
	svc := &AgentService{desk: newTestApp()}

	missing := filepath.Join(base, "reviews", "never-written.md")
	sid := writeReviewTranscript(t, "review-20260912-010101-aaaa.jsonl", []map[string]any{
		{"type": "meta", "kind": "review", "extra": map[string]string{"report": missing}},
	})

	if vo := svc.GetReviewFindings(sid); !strings.Contains(vo.Note, "没有报告问题") {
		t.Errorf("Note = %q, want the plain 'found nothing' wording", vo.Note)
	}
}
