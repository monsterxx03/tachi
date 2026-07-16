package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/lsp"
	"github.com/monsterxx03/tachi/agent/wdctx"
)

// ToolNameLSPDiagnostics is the name exposed to the LLM.
const ToolNameLSPDiagnostics = "LSPDiagnostics"

const lspDiagnosticsDescription = `Get diagnostic information (errors, warnings, hints) from LSP servers.

If path is provided, returns diagnostics for that specific file.
If path is empty, returns a summary of all diagnostics across the workspace.

Use this to check for errors after editing code, or to understand what's wrong with a file.`

// LSPDiagnosticsTool implements tools.Tool for querying LSP diagnostics.
type LSPDiagnosticsTool struct {
	manager *lsp.LSPManager
}

// NewLSPDiagnosticsTool creates a new LSP diagnostics tool.
func NewLSPDiagnosticsTool(manager *lsp.LSPManager) *LSPDiagnosticsTool {
	return &LSPDiagnosticsTool{manager: manager}
}

func (t *LSPDiagnosticsTool) Name() string { return ToolNameLSPDiagnostics }

func (t *LSPDiagnosticsTool) Description() string { return lspDiagnosticsDescription }

func (t *LSPDiagnosticsTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"path": {
			Type:        "string",
			Description: "Optional path to a specific file. If empty, returns project-wide diagnostic summary.",
		},
	}
}

func (t *LSPDiagnosticsTool) Required() []string { return nil }

func (t *LSPDiagnosticsTool) Parallel() bool { return true }

func (t *LSPDiagnosticsTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var input struct {
		Path string `json:"path,omitempty"`
	}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return marshalDiagResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	if !t.manager.IsConfigured() {
		return marshalDiagResult("no LSP servers configured"), nil
	}

	wd := wdctx.Dir(ctx)

	if input.Path != "" {
		return t.fileDiagnostics(ctx, input.Path, wd)
	}
	return t.projectSummary(wd)
}

func (t *LSPDiagnosticsTool) fileDiagnostics(ctx context.Context, filePath, wd string) (string, error) {
	absPath := filePath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(wd, absPath)
	}
	absPath = filepath.Clean(absPath)
	uri := lsp.PathToURI(absPath)

	// Ensure file is opened on the server.
	_ = t.manager.SyncFile(ctx, absPath)

	server, err := t.manager.GetServer(ctx, absPath)
	if err != nil || server == nil {
		return marshalDiagResult(fmt.Sprintf("no LSP server for %s", filePath)), nil
	}

	// Wait briefly for fresh diagnostics after open.
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	server.WaitForDiagnostics(waitCtx, 2*time.Second)

	diags := server.GetFileDiagnostics(uri)
	if len(diags) == 0 {
		return marshalDiagResult(fmt.Sprintf("No diagnostics for %s", filePath)), nil
	}

	var lines []string
	relPath, _ := filepath.Rel(wd, absPath)
	for _, d := range diags {
		sev := severityString(d.Severity)
		line := d.Range.Start.Line + 1
		col := d.Range.Start.Character + 1
		lines = append(lines, fmt.Sprintf("%s: %s:%d:%d: %s", sev, relPath, line, col, d.Message))
	}

	errCount := countBySeverity(diags, lsp.SeverityError)
	warnCount := countBySeverity(diags, lsp.SeverityWarning)

	summary := fmt.Sprintf("Found %d diagnostics (%d errors, %d warnings):\n\n%s",
		len(diags), errCount, warnCount, strings.Join(lines, "\n"))
	return marshalDiagResult(summary), nil
}

func (t *LSPDiagnosticsTool) projectSummary(_ string) (string, error) {
	type serverSummary struct {
		Name   string
		Errors int
		Warns  int
		Total  int
	}
	var summaries []serverSummary

	for _, srv := range sortedLSPServers(t.manager.Servers()) {
		diags := srv.GetDiagnostics()
		total := 0
		var allDiags []lsp.Diagnostic
		for _, d := range diags {
			total += len(d)
			allDiags = append(allDiags, d...)
		}
		if total == 0 {
			continue
		}
		summaries = append(summaries, serverSummary{
			Name:   srv.Name(),
			Errors: countBySeverity(allDiags, lsp.SeverityError),
			Warns:  countBySeverity(allDiags, lsp.SeverityWarning),
			Total:  total,
		})
	}

	if len(summaries) == 0 {
		return marshalDiagResult("No diagnostics across any LSP server."), nil
	}

	var lines []string
	grandTotal := 0
	for _, s := range summaries {
		lines = append(lines, fmt.Sprintf("  %s: %d total (%d errors, %d warnings)", s.Name, s.Total, s.Errors, s.Warns))
		grandTotal += s.Total
	}

	result := fmt.Sprintf("Project diagnostic summary (%d total across %d server(s)):\n%s",
		grandTotal, len(summaries), strings.Join(lines, "\n"))
	return marshalDiagResult(result), nil
}

type diagOutput struct {
	Result string `json:"result"`
}

func marshalDiagResult(result string) string {
	b, _ := json.Marshal(diagOutput{Result: result})
	return string(b)
}

func severityString(sev lsp.DiagnosticSeverity) string {
	switch sev {
	case lsp.SeverityError:
		return "Error"
	case lsp.SeverityWarning:
		return "Warning"
	case lsp.SeverityInformation:
		return "Info"
	case lsp.SeverityHint:
		return "Hint"
	default:
		return "Unknown"
	}
}

func countBySeverity(diags []lsp.Diagnostic, sev lsp.DiagnosticSeverity) int {
	count := 0
	for _, d := range diags {
		if d.Severity == sev {
			count++
		}
	}
	return count
}

// Ensure LSPDiagnosticsTool implements Tool.
var _ Tool = (*LSPDiagnosticsTool)(nil)

// sortedLSPServers returns servers in alphabetical order by name.
func sortedLSPServers(servers map[string]*lsp.LSPServer) []*lsp.LSPServer {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]*lsp.LSPServer, len(names))
	for i, name := range names {
		result[i] = servers[name]
	}
	return result
}
