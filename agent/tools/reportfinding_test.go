package tools

import (
	"context"
	"strings"
	"testing"
)

// TestReportFindingTool covers the contract the review prompt now depends on: a
// finding must be locatable (path + 1-based line), classifiable (severity) and
// checkable (text), and anything else is rejected with something the model can act on.
func TestReportFindingTool(t *testing.T) {
	tool := ReportFindingTool{}
	ctx := context.Background()

	tests := []struct {
		name     string
		args     string
		wantErr  string
		wantText string
	}{
		{
			name:     "a complete finding is acknowledged with its location",
			args:     `{"path":"src/main.go","line":42,"severity":"bug","category":"Correctness","text":"nil map write","suggestion":"init the map"}`,
			wantText: "src/main.go:42 [bug] nil map write",
		},
		{
			name:     "a range is shown as a range",
			args:     `{"path":"a.go","line":3,"end_line":9,"severity":"warn","text":"too long"}`,
			wantText: "a.go:3-9 [warn] too long",
		},
		{
			name:     "only the first line is echoed back",
			args:     `{"path":"a.go","line":1,"severity":"info","text":"first line\nsecond line"}`,
			wantText: "a.go:1 [info] first line",
		},
		{
			name:    "path is required",
			args:    `{"line":1,"severity":"bug","text":"x"}`,
			wantErr: "path is required",
		},
		{
			name:    "line is 1-based",
			args:    `{"path":"a.go","line":0,"severity":"bug","text":"x"}`,
			wantErr: "line is required and must be 1-based",
		},
		{
			name:    "text is required",
			args:    `{"path":"a.go","line":1,"severity":"bug"}`,
			wantErr: "text is required",
		},
		{
			name:    "severity must be one of the three",
			args:    `{"path":"a.go","line":1,"severity":"critical","text":"x"}`,
			wantErr: "severity",
		},
		{
			name:     "severity is case-insensitive",
			args:     `{"path":"a.go","line":1,"severity":"BUG","text":"x"}`,
			wantText: "[bug]",
		},
		{
			name:    "a reversed range is rejected",
			args:    `{"path":"a.go","line":9,"end_line":3,"severity":"bug","text":"x"}`,
			wantErr: "end_line",
		},
		{
			name:    "unparsable args",
			args:    `{"path":`,
			wantErr: "invalid arguments",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tool.ExecuteContext(ctx, tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(got, tt.wantText) {
				t.Errorf("acknowledgement = %q, want it to contain %q", got, tt.wantText)
			}
		})
	}
}

// TestReportFindingToolIsDeclarative: recording a finding must not touch anything —
// the tool call IS the record, and the review's report file is written separately.
func TestReportFindingToolIsDeclarative(t *testing.T) {
	tool := ReportFindingTool{}
	if tool.IsDestructive() {
		t.Error("recording a finding is not a destructive act")
	}
	if tool.Parallel() {
		t.Error("findings should be recorded in order, not concurrently")
	}
	if tool.Name() != ToolNameReportFinding {
		t.Errorf("Name() = %q", tool.Name())
	}
	// The schema is the instruction: the three severities the UI understands must be
	// constrained at the API level, not merely described.
	if got := tool.Properties()["severity"].Enum; len(got) != 3 {
		t.Errorf("severity enum = %v, want three values", got)
	}
}
