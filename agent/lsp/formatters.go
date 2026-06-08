package lsp

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// formatGoToDefinition formats Location / LocationLink results.
func formatGoToDefinition(result any, cwd string) string {
	switch r := result.(type) {
	case nil:
		return "No definition found."
	case Location:
		return fmt.Sprintf("Defined in %s", formatLocation(r, cwd))
	case LocationLink:
		return fmt.Sprintf("Defined in %s", formatLocation(Location{URI: r.TargetURI, Range: r.TargetSelectionRange}, cwd))
	case []Location:
		if len(r) == 0 {
			return "No definition found."
		}
		var valid []Location
		for _, loc := range r {
			if loc.URI != "" {
				valid = append(valid, loc)
			}
		}
		if len(valid) == 0 {
			return "No definition found."
		}
		if len(valid) == 1 {
			return fmt.Sprintf("Defined in %s", formatLocation(valid[0], cwd))
		}
		var lines []string
		lines = append(lines, fmt.Sprintf("Found %d definitions:", len(valid)))
		for _, loc := range valid {
			lines = append(lines, fmt.Sprintf("  %s", formatLocation(loc, cwd)))
		}
		return strings.Join(lines, "\n")
	case []LocationLink:
		if len(r) == 0 {
			return "No definition found."
		}
		if len(r) == 1 {
			return fmt.Sprintf("Defined in %s", formatLocation(Location{URI: r[0].TargetURI, Range: r[0].TargetSelectionRange}, cwd))
		}
		var lines []string
		lines = append(lines, fmt.Sprintf("Found %d definitions:", len(r)))
		for _, link := range r {
			loc := Location{URI: link.TargetURI, Range: link.TargetSelectionRange}
			lines = append(lines, fmt.Sprintf("  %s", formatLocation(loc, cwd)))
		}
		return strings.Join(lines, "\n")
	}
	return "No definition found."
}

// formatFindReferences formats Location results grouped by file.
func formatFindReferences(result []Location, cwd string) string {
	if len(result) == 0 {
		return "No references found."
	}

	var valid []Location
	for _, loc := range result {
		if loc.URI != "" {
			valid = append(valid, loc)
		}
	}
	if len(valid) == 0 {
		return "No references found."
	}

	byFile := groupByFile(valid, cwd)
	var lines []string
	lines = append(lines, fmt.Sprintf("Found %d references across %d files:", len(valid), len(byFile)))

	for _, filePath := range sortedKeys(byFile) {
		locs := byFile[filePath]
		lines = append(lines, fmt.Sprintf("\n%s:", filePath))
		for _, loc := range locs {
			lines = append(lines, fmt.Sprintf("  Line %d:%d", loc.Range.Start.Line+1, loc.Range.Start.Character+1))
		}
	}
	return strings.Join(lines, "\n")
}

// formatHover formats hover results.
func formatHover(result *Hover, cwd string) string {
	if result == nil {
		return "No hover information available."
	}
	content := extractMarkupText(result.Contents)
	if result.Range != nil {
		line := result.Range.Start.Line + 1
		char := result.Range.Start.Character + 1
		return fmt.Sprintf("Hover info at %d:%d:\n\n%s", line, char, content)
	}
	return content
}

// formatDocumentSymbol formats hierarchical document symbols.
func formatDocumentSymbol(result any, cwd string) string {
	switch r := result.(type) {
	case nil:
		return "No symbols found in document."
	case []DocumentSymbol:
		if len(r) == 0 {
			return "No symbols found in document."
		}
		var lines []string
		lines = append(lines, "Document symbols:")
		for _, sym := range r {
			lines = append(lines, formatDocSymNode(sym, 0)...)
		}
		return strings.Join(lines, "\n")
	case []SymbolInformation:
		return formatWorkspaceSymbol(r, cwd)
	}
	return "No symbols found in document."
}

func formatDocSymNode(sym DocumentSymbol, indent int) []string {
	prefix := strings.Repeat("  ", indent)
	line := sym.Range.Start.Line + 1
	result := fmt.Sprintf("%s%s (%s) - Line %d", prefix, sym.Name, SymbolKindString(sym.Kind), line)
	if sym.Detail != "" {
		result += " " + sym.Detail
	}
	lines := []string{result}
	for _, child := range sym.Children {
		lines = append(lines, formatDocSymNode(child, indent+1)...)
	}
	return lines
}

// formatWorkspaceSymbol formats symbol information results.
func formatWorkspaceSymbol(result []SymbolInformation, cwd string) string {
	if len(result) == 0 {
		return "No symbols found in workspace."
	}
	var valid []SymbolInformation
	for _, sym := range result {
		if sym.Location.URI != "" {
			valid = append(valid, sym)
		}
	}
	if len(valid) == 0 {
		return "No symbols found in workspace."
	}

	byFile := groupSymbolsByFile(valid, cwd)
	var lines []string
	lines = append(lines, fmt.Sprintf("Found %d symbols in workspace:", len(valid)))
	for _, filePath := range sortedKeys(byFile) {
		syms := byFile[filePath]
		lines = append(lines, fmt.Sprintf("\n%s:", filePath))
		for _, sym := range syms {
			sline := sym.Location.Range.Start.Line + 1
			entry := fmt.Sprintf("  %s (%s) - Line %d", sym.Name, SymbolKindString(sym.Kind), sline)
			if sym.ContainerName != "" {
				entry += fmt.Sprintf(" in %s", sym.ContainerName)
			}
			lines = append(lines, entry)
		}
	}
	return strings.Join(lines, "\n")
}

// formatPrepareCallHierarchy formats call hierarchy items.
func formatPrepareCallHierarchy(result []CallHierarchyItem, cwd string) string {
	if len(result) == 0 {
		return "No call hierarchy item found at this position."
	}
	if len(result) == 1 {
		return fmt.Sprintf("Call hierarchy item: %s", formatCallItem(result[0], cwd))
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("Found %d call hierarchy items:", len(result)))
	for _, item := range result {
		lines = append(lines, fmt.Sprintf("  %s", formatCallItem(item, cwd)))
	}
	return strings.Join(lines, "\n")
}

// formatIncomingCalls formats incoming calls.
func formatIncomingCalls(result []CallHierarchyIncomingCall, cwd string) string {
	if len(result) == 0 {
		return "No incoming calls found (nothing calls this function)."
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("Found %d incoming calls:", len(result)))

	byFile := make(map[string][]CallHierarchyIncomingCall)
	for _, call := range result {
		uri := call.From.URI
		fp := formatFilePath(uri, cwd)
		byFile[fp] = append(byFile[fp], call)
	}

	for _, fp := range sortedKeys(byFile) {
		calls := byFile[fp]
		lines = append(lines, fmt.Sprintf("\n%s:", fp))
		for _, call := range calls {
			entry := fmt.Sprintf("  %s (%s) - Line %d", call.From.Name, SymbolKindString(call.From.Kind), call.From.Range.Start.Line+1)
			if len(call.FromRanges) > 0 {
				var sites []string
				for _, r := range call.FromRanges {
					sites = append(sites, fmt.Sprintf("%d:%d", r.Start.Line+1, r.Start.Character+1))
				}
				entry += fmt.Sprintf(" [calls at: %s]", strings.Join(sites, ", "))
			}
			lines = append(lines, entry)
		}
	}
	return strings.Join(lines, "\n")
}

// formatOutgoingCalls formats outgoing calls.
func formatOutgoingCalls(result []CallHierarchyOutgoingCall, cwd string) string {
	if len(result) == 0 {
		return "No outgoing calls found (this function calls nothing)."
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("Found %d outgoing calls:", len(result)))

	byFile := make(map[string][]CallHierarchyOutgoingCall)
	for _, call := range result {
		uri := call.To.URI
		fp := formatFilePath(uri, cwd)
		byFile[fp] = append(byFile[fp], call)
	}

	for _, fp := range sortedKeys(byFile) {
		calls := byFile[fp]
		lines = append(lines, fmt.Sprintf("\n%s:", fp))
		for _, call := range calls {
			entry := fmt.Sprintf("  %s (%s) - Line %d", call.To.Name, SymbolKindString(call.To.Kind), call.To.Range.Start.Line+1)
			if len(call.FromRanges) > 0 {
				var sites []string
				for _, r := range call.FromRanges {
					sites = append(sites, fmt.Sprintf("%d:%d", r.Start.Line+1, r.Start.Character+1))
				}
				entry += fmt.Sprintf(" [called from: %s]", strings.Join(sites, ", "))
			}
			lines = append(lines, entry)
		}
	}
	return strings.Join(lines, "\n")
}

// --- helpers ---

func formatLocation(loc Location, cwd string) string {
	fp := formatFilePath(loc.URI, cwd)
	return fmt.Sprintf("%s:%d:%d", fp, loc.Range.Start.Line+1, loc.Range.Start.Character+1)
}

func formatFilePath(uri, cwd string) string {
	fp := URItoPath(uri)
	if cwd != "" {
		if rel, err := filepath.Rel(cwd, fp); err == nil && len(rel) < len(fp) && !strings.HasPrefix(rel, "../") {
			return rel
		}
	}
	return fp
}

func groupByFile(locs []Location, cwd string) map[string][]Location {
	byFile := make(map[string][]Location)
	for _, loc := range locs {
		fp := formatFilePath(loc.URI, cwd)
		byFile[fp] = append(byFile[fp], loc)
	}
	return byFile
}

func groupSymbolsByFile(syms []SymbolInformation, cwd string) map[string][]SymbolInformation {
	byFile := make(map[string][]SymbolInformation)
	for _, sym := range syms {
		fp := formatFilePath(sym.Location.URI, cwd)
		byFile[fp] = append(byFile[fp], sym)
	}
	return byFile
}

func extractMarkupText(contents any) string {
	switch c := contents.(type) {
	case MarkupContent:
		return c.Value
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			switch v := item.(type) {
			case string:
				parts = append(parts, v)
			case map[string]any:
				if val, ok := v["value"]; ok {
					parts = append(parts, fmt.Sprintf("%v", val))
				}
			}
		}
		return strings.Join(parts, "\n\n")
	case map[string]any:
		if val, ok := c["value"]; ok {
			return fmt.Sprintf("%v", val)
		}
		return fmt.Sprintf("%v", c)
	}
	return fmt.Sprintf("%v", contents)
}

func formatCallItem(item CallHierarchyItem, cwd string) string {
	fp := formatFilePath(item.URI, cwd)
	line := item.Range.Start.Line + 1
	kind := SymbolKindString(item.Kind)
	s := fmt.Sprintf("%s (%s) - %s:%d", item.Name, kind, fp, line)
	if item.Detail != "" {
		s += fmt.Sprintf(" [%s]", item.Detail)
	}
	return s
}

func sortedKeys[K ~string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
