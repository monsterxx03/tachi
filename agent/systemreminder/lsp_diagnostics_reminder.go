package systemreminder

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/monsterxx03/tachi/agent/lsp"
	"github.com/monsterxx03/tachi/agent/tools"
)

// LSPDiagnosticsProvider is the interface that LSPManager satisfies for the
// diagnostics reminder. It abstracts away the manager so we can unit-test
// the reminder without starting real LSP servers.
type LSPDiagnosticsProvider interface {
	Servers() map[string]*lsp.LSPServer
	IsConfigured() bool
}

// LSPDiagnosticsReminder injects an <lsp-diagnostics> block at tool-result
// boundaries when new diagnostics (errors/warnings/hints) are detected on
// any LSP server. It tracks the last diagnostic state per server with a
// content hash to avoid repeating the same diagnostics every iteration.
//
// Implements TaggedReminder so output gets its own tag independent of
// <system-reminder>.
type LSPDiagnosticsReminder struct {
	Provider LSPDiagnosticsProvider

	// lastHashes caches per-server diagnostic content hashes to detect
	// when diagnostics have changed since the last injection.
	lastHashes   map[string]string
	lastHashesMu sync.Mutex

	// MaxDiagnostics limits how many individual diagnostic lines are shown
	// in the reminder block. Defaults to 10 if <= 0.
	MaxDiagnostics int
}

// WrapperTag implements the TaggedReminder interface.
func (r *LSPDiagnosticsReminder) WrapperTag() string {
	return "lsp-diagnostics"
}

func (r *LSPDiagnosticsReminder) Generate(ctx context.Context, rctx Context) []string {
	if r.Provider == nil || !r.Provider.IsConfigured() {
		return nil
	}
	// Only fire at tool-result boundaries in the agent loop.
	if !rctx.IsToolResult {
		return nil
	}
	// Only fire after EditFile tool execution — that's when source
	// changes that LSP servers care about actually happen.
	if !slices.Contains(rctx.ToolNames, tools.ToolNameEdit) {
		return nil
	}

	servers := r.Provider.Servers()
	if len(servers) == 0 {
		return nil
	}

	// Collect diagnostics from all servers, grouped by server name.
	type serverDiags struct {
		name  string
		diags []fileDiagnostics
		hash  string
		errs  int
		warns int
		info  int
		total int
	}

	var active []serverDiags

	// Sort server names for stable output.
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		srv := servers[name]
		raw := srv.GetDiagnostics()
		if len(raw) == 0 {
			continue
		}

		sd := serverDiags{name: name}
		var hashParts []string

		// Sort file URIs for stable hashing.
		files := make([]string, 0, len(raw))
		for uri := range raw {
			files = append(files, uri)
		}
		sort.Strings(files)

		for _, uri := range files {
			diags := raw[uri]
			if len(diags) == 0 {
				continue
			}
			fd := fileDiagnostics{uri: uri}
			for _, d := range diags {
				fd.diags = append(fd.diags, d)
				hashParts = append(hashParts, fmt.Sprintf("%s|%d|%d|%s",
					uri, d.Severity, d.Range.Start.Line, d.Message))
				switch d.Severity {
				case lsp.SeverityError:
					sd.errs++
				case lsp.SeverityWarning:
					sd.warns++
				default:
					sd.info++
				}
				sd.total++
			}
			sd.diags = append(sd.diags, fd)
		}

		if sd.total == 0 {
			continue
		}

		h := sha256.New()
		for _, p := range hashParts {
			h.Write([]byte(p))
		}
		sd.hash = fmt.Sprintf("%x", h.Sum(nil))

		active = append(active, sd)
	}

	if len(active) == 0 {
		return nil
	}

	// Check for changes versus last injection.
	r.lastHashesMu.Lock()
	defer r.lastHashesMu.Unlock()

	if r.lastHashes == nil {
		r.lastHashes = make(map[string]string)
	}

	changed := false
	for i := range active {
		if last, ok := r.lastHashes[active[i].name]; !ok || last != active[i].hash {
			changed = true
			r.lastHashes[active[i].name] = active[i].hash
		}
	}
	if !changed {
		return nil // no new diagnostics since last time
	}

	// Build output lines.
	maxD := r.MaxDiagnostics
	if maxD <= 0 {
		maxD = 10
	}

	var lines []string

	for _, sd := range active {
		if sd.total == 0 {
			continue
		}

		var parts []string
		if sd.errs > 0 {
			parts = append(parts, fmt.Sprintf("%d errors", sd.errs))
		}
		if sd.warns > 0 {
			parts = append(parts, fmt.Sprintf("%d warnings", sd.warns))
		}
		if sd.info > 0 {
			parts = append(parts, fmt.Sprintf("%d other", sd.info))
		}
		lines = append(lines, fmt.Sprintf("%s: %d diagnostics (%s)",
			sd.name, sd.total, strings.Join(parts, ", ")))

		// Show individual diagnostics up to the limit.
		shown := 0
		for _, fd := range sd.diags {
			if shown >= maxD {
				break
			}
			for _, d := range fd.diags {
				if shown >= maxD {
					break
				}
				sev := severityAbbrev(d.Severity)
				line := d.Range.Start.Line + 1
				col := d.Range.Start.Character + 1
				// Truncate the message to one line.
				msg := d.Message
				if nl := strings.IndexByte(msg, '\n'); nl >= 0 {
					msg = msg[:nl]
				}
				// Use a shorter URI representation.
				shortURI := shortURI(fd.uri)

				lines = append(lines, fmt.Sprintf("  %s %s:%d:%d: %s",
					sev, shortURI, line, col, msg))
				shown++
			}
		}

		remaining := sd.total - shown
		if remaining > 0 {
			lines = append(lines, fmt.Sprintf("  ... and %d more diagnostics", remaining))
		}
	}

	if len(lines) == 0 {
		return nil
	}

	return lines
}

type fileDiagnostics struct {
	uri   string
	diags []lsp.Diagnostic
}

func severityAbbrev(sev lsp.DiagnosticSeverity) string {
	switch sev {
	case lsp.SeverityError:
		return "Error"
	case lsp.SeverityWarning:
		return "Warn"
	case lsp.SeverityInformation:
		return "Info"
	case lsp.SeverityHint:
		return "Hint"
	default:
		return "?"
	}
}

// shortURI strips the file:// prefix and, if that's not possible, returns
// the uri as-is. Prepends ./ for relative readability.
func shortURI(uri string) string {
	if strings.HasPrefix(uri, "file://") {
		return uri[len("file://"):]
	}
	return uri
}
