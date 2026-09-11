package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ToolNameReportFinding is the review fork's structured-output tool: one call per
// finding, so the UI can attach it to a file and a line.
const ToolNameReportFinding = "ReportFinding"

// ReportFindingParams is one review finding. The field set mirrors what the review
// prompt has always asked for in prose (file, line range, severity, category,
// explanation, suggestion) — this makes it machine-readable instead of parsed.
type ReportFindingParams struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	EndLine    int    `json:"end_line,omitempty"`
	Severity   string `json:"severity"`
	Category   string `json:"category,omitempty"`
	Text       string `json:"text"`
	Suggestion string `json:"suggestion,omitempty"`
}

// ReportFindingTool records one review finding as a tool call.
//
// It exists so findings have a CONTRACT rather than a convention: the review prompt
// used to describe an output format ("File and line range, Severity, Category …") that
// only a human could read. As a tool call, each finding lands in the session record —
// which means the diff panel can show a review's findings again after a restart, with
// no new persistence and no parsing of free-form reports.
//
// It is registered ONLY on review forks (agent.ForkConfig.ForReview): the main agent
// has no business declaring findings about its own work, and the schema doubles as the
// instruction.
type ReportFindingTool struct{}

// Severities the panel understands. "warn" and "info" are the review prompt's ⚠️ and
// 💡; "bug" is its 🐛.
var reportFindingSeverities = []string{"bug", "warn", "info"}

func (t ReportFindingTool) Name() string { return ToolNameReportFinding }
func (t ReportFindingTool) Description() string {
	return "Record ONE review finding: a specific problem you found in the reviewed changes, " +
		"located by file and line. Call it once per finding (a review usually reports several). " +
		"Recording a finding does not change any code."
}
func (t ReportFindingTool) IsDestructive() bool { return false }
func (t ReportFindingTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"path": {
			Type:        "string",
			Description: "File the finding is about (the path as it appears in the diff)",
		},
		"line": {
			Type:        "integer",
			Description: "1-based line in that file — the line the finding is about",
		},
		"end_line": {
			Type:        "integer",
			Description: "Last line of the range, when the finding spans several lines",
		},
		"severity": {
			Type:        "string",
			Description: "bug = it is wrong or will break; warn = risky or questionable; info = suggestion",
			Enum:        reportFindingSeverities,
		},
		"category": {
			Type:        "string",
			Description: "Correctness | Quality | Efficiency | Security | Maintainability",
			Enum:        []string{"Correctness", "Quality", "Efficiency", "Security", "Maintainability"},
		},
		"text": {
			Type:        "string",
			Description: "What is wrong, with the reasoning that makes it checkable",
		},
		"suggestion": {
			Type:        "string",
			Description: "How to fix or improve it (optional)",
		},
	}
}
func (t ReportFindingTool) Required() []string { return []string{"path", "line", "severity", "text"} }
func (t ReportFindingTool) Parallel() bool     { return false }

// ExecuteContext validates the finding and acknowledges it. Nothing is written: the
// tool call itself is the record (the session stores its arguments), and the review's
// human-readable report is written separately with WriteFile.
func (t ReportFindingTool) ExecuteContext(_ context.Context, args string) (string, error) {
	var p ReportFindingParams
	if err := json.Unmarshal([]byte(args), &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	p.Path = strings.TrimSpace(p.Path)
	p.Severity = strings.ToLower(strings.TrimSpace(p.Severity))
	p.Text = strings.TrimSpace(p.Text)
	switch {
	case p.Path == "":
		return "", fmt.Errorf("path is required — name the file the finding is about")
	case p.Line < 1:
		return "", fmt.Errorf("line is required and must be 1-based (got %d)", p.Line)
	case p.Text == "":
		return "", fmt.Errorf("text is required — say what is wrong")
	}
	if !validReportSeverity(p.Severity) {
		return "", fmt.Errorf("severity %q is not one of %s", p.Severity, strings.Join(reportFindingSeverities, ", "))
	}
	if p.EndLine != 0 && p.EndLine < p.Line {
		return "", fmt.Errorf("end_line (%d) must not be before line (%d)", p.EndLine, p.Line)
	}

	where := fmt.Sprintf("%s:%d", p.Path, p.Line)
	if p.EndLine > p.Line {
		where = fmt.Sprintf("%s:%d-%d", p.Path, p.Line, p.EndLine)
	}
	return fmt.Sprintf("已记录 %s [%s] %s", where, p.Severity, firstLine(p.Text)), nil
}

func validReportSeverity(s string) bool {
	for _, want := range reportFindingSeverities {
		if s == want {
			return true
		}
	}
	return false
}

// firstLine keeps the acknowledgement short: the model does not need its own text read
// back to it, only confirmation that the finding landed.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
