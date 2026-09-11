package main

// Review findings for the changes panel (P2b/P2c).
//
// The findings are NOT derived from the live transcript: a review runs as a one-off
// fork (agent.RunOneOffStream), so its messages are recorded in the session's
// oneoff/review-*.jsonl rather than in the conversation history — deliberately, since
// a review is a side run and must not become part of the main context. That file is
// therefore the record, and reading it is what makes findings available both right
// after a review and after a restart, with no new persistence.
//
// Design: docs/2026-09-11-desktop-diff-review-design.md §12.3

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/config"
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

// ReviewFindingsVO is the panel's findings payload.
type ReviewFindingsVO struct {
	Findings []FindingVO `json:"findings"`
	// Review is the review transcript they came from ("" when there has been none),
	// shown as a tooltip so the reader can tell which review these are.
	Review string `json:"review,omitempty"`
	// Note explains an empty list: no review yet, or one that reported nothing.
	Note string `json:"note,omitempty"`
}

// GetReviewFindings returns the findings of the most recent review in this session.
//
// Only the newest review is read: findings carry no review id, so listing every review
// ever run would keep showing problems the author has already fixed. "What did the last
// review say" is the scope a reader expects.
func (s *AgentService) GetReviewFindings(sessionID string) ReviewFindingsVO {
	if sessionID == "" {
		return ReviewFindingsVO{Note: "没有会话"}
	}
	dir, err := config.SessionDir()
	if err != nil {
		return ReviewFindingsVO{Note: "找不到会话目录"}
	}
	reviews, err := latestReviewTranscript(filepath.Join(dir, sessionID, "oneoff"))
	if err != nil || reviews == "" {
		return ReviewFindingsVO{Note: "还没有评审过这一轮的改动"}
	}

	findings, err := readReviewFindings(reviews)
	if err != nil {
		return ReviewFindingsVO{Note: "读取评审结果失败：" + err.Error()}
	}
	vo := ReviewFindingsVO{Findings: findings, Review: filepath.Base(reviews)}
	if len(findings) == 0 {
		vo.Note = "最近一次评审没有报告问题"
	}
	return vo
}

// latestReviewTranscript returns the newest review-*.jsonl under dir ("" when there is
// none). The name carries the timestamp (review-20060102-150405-abcd.jsonl), so the
// lexicographic maximum is the newest.
func latestReviewTranscript(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "review-") || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return "", nil
	}
	sort.Strings(names)
	return filepath.Join(dir, names[len(names)-1]), nil
}

// readReviewFindings collects the ReportFinding calls out of one review transcript.
//
// The file has the same shape as the session's messages.jsonl (one JSON message per
// line), which is why the parse is this short: the tool's arguments ARE the finding.
func readReviewFindings(path string) ([]FindingVO, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var out []FindingVO
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg struct {
			Type string          `json:"type"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue // a malformed line is not worth failing the whole panel over
		}
		if msg.Type != "tool_call" || msg.Name != tools.ToolNameReportFinding || len(msg.Args) == 0 {
			continue
		}
		// The recorder stores the raw arguments as a JSON STRING while a session message
		// carries the object itself — accept both shapes, or the reader silently finds
		// nothing in the very file it exists to read.
		raw := msg.Args
		if raw[0] == '"' {
			var unquoted string
			if err := json.Unmarshal(raw, &unquoted); err == nil {
				raw = json.RawMessage(unquoted)
			}
		}
		var p tools.ReportFindingParams
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		out = append(out, FindingVO{
			Path:       p.Path,
			Line:       p.Line,
			EndLine:    p.EndLine,
			Severity:   p.Severity,
			Category:   p.Category,
			Text:       p.Text,
			Suggestion: p.Suggestion,
		})
	}
	return out, nil
}
