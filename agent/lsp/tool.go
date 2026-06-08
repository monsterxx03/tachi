package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/agent/wdctx"
)

// ToolName is the name exposed to the LLM.
const ToolName = "LSP"

// Description is the tool description shown to the LLM.
const Description = `Interact with Language Server Protocol (LSP) servers for code intelligence.

Supported operations:
- goToDefinition: Find where a symbol is defined
- findReferences: Find all references to a symbol
- hover: Get hover information (documentation, type info) for a symbol
- documentSymbol: Get all symbols (functions, classes, variables) in a document
- workspaceSymbol: Search for symbols across the entire workspace
- goToImplementation: Find implementations of an interface or abstract method
- prepareCallHierarchy: Get call hierarchy item at a position
- incomingCalls: Find all functions/methods that call the function at a position
- outgoingCalls: Find all functions/methods called by the function at a position

All operations except workspaceSymbol require filePath, line (1-based), and character (1-based).
workspaceSymbol requires a query string and a filePath (any file in the workspace).`

// LSPTool implements the tools.Tool interface for LSP code intelligence.
type LSPTool struct {
	manager *LSPManager
}

// NewLSPTool creates a new LSP tool.
func NewLSPTool(manager *LSPManager) *LSPTool {
	return &LSPTool{manager: manager}
}

func (t *LSPTool) Name() string { return ToolName }

func (t *LSPTool) Description() string { return Description }

func (t *LSPTool) Properties() map[string]tools.PropertySchema {
	return map[string]tools.PropertySchema{
		"operation": {
			Type:        "string",
			Description: "The LSP operation to perform. Valid values: goToDefinition, findReferences, hover, documentSymbol, workspaceSymbol, goToImplementation, prepareCallHierarchy, incomingCalls, outgoingCalls",
		},
		"filePath": {
			Type:        "string",
			Description: "The absolute or relative path to the file",
		},
		"line": {
			Type:        "integer",
			Description: "The line number (1-based, as shown in editors)",
		},
		"character": {
			Type:        "integer",
			Description: "The character offset (1-based, as shown in editors)",
		},
		"query": {
			Type:        "string",
			Description: "Search query for workspaceSymbol (only needed for that operation)",
		},
	}
}

func (t *LSPTool) Required() []string {
	return []string{"operation", "filePath", "line", "character"}
}

func (t *LSPTool) Parallel() bool { return true }

func (t *LSPTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var input struct {
		Operation string `json:"operation"`
		FilePath  string `json:"filePath"`
		Line      int    `json:"line"`
		Character int    `json:"character"`
		Query     string `json:"query,omitempty"`
	}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return marshalError("LSP", fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	// Resolve file path relative to working directory.
	wd := wdctx.Dir(ctx)
	absPath := input.FilePath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(wd, absPath)
	}
	absPath = filepath.Clean(absPath)

	// Get or start the LSP server for this file type.
	server, err := t.manager.GetServer(ctx, absPath)
	if err != nil {
		return marshalError(input.Operation, fmt.Sprintf("LSP server error: %v", err)), nil
	}
	if server == nil {
		ext := filepath.Ext(absPath)
		return marshalError(input.Operation, fmt.Sprintf("No LSP server available for file type %s", ext)), nil
	}

	uri := pathToURI(absPath)

	// Sync file to LSP server (didOpen if not yet open).
	if err := t.manager.SyncFile(ctx, absPath); err != nil {
		return marshalError(input.Operation, fmt.Sprintf("file sync error: %v", err)), nil
	}

	// Convert 1-based to 0-based.
	line := uint32(input.Line - 1)
	char := uint32(input.Character - 1)

	// Capability pre-checks.
	caps := server.Capabilities()
	if msg := checkCapability(input.Operation, caps); msg != "" {
		return marshalError(input.Operation, msg), nil
	}

	// Execute the requested operation.
	switch input.Operation {
	case "goToDefinition":
		return t.goToDefinition(ctx, server, uri, absPath, line, char, wd)
	case "findReferences":
		return t.findReferences(ctx, server, uri, absPath, line, char, wd)
	case "hover":
		return t.hover(ctx, server, uri, absPath, line, char, wd)
	case "documentSymbol":
		return t.documentSymbol(ctx, server, uri, absPath, wd)
	case "workspaceSymbol":
		return t.workspaceSymbol(ctx, server, uri, input.Query, wd)
	case "goToImplementation":
		return t.goToImplementation(ctx, server, uri, absPath, line, char, wd)
	case "prepareCallHierarchy":
		return t.prepareCallHierarchy(ctx, server, uri, absPath, line, char, wd)
	case "incomingCalls":
		return t.incomingCalls(ctx, server, uri, absPath, line, char, wd)
	case "outgoingCalls":
		return t.outgoingCalls(ctx, server, uri, absPath, line, char, wd)
	default:
		return marshalError(input.Operation, fmt.Sprintf("unknown operation: %s", input.Operation)), nil
	}
}

func (t *LSPTool) goToDefinition(ctx context.Context, srv *LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var result json.RawMessage
	if err := srv.Call(ctx, "textDocument/definition", params, &result); err != nil {
		return marshalResult("goToDefinition", err.Error(), absPath, 0, 0), nil
	}
	return formatRawLocations("goToDefinition", result, wd, absPath)
}

func (t *LSPTool) findReferences(ctx context.Context, srv *LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
		"context":      map[string]any{"includeDeclaration": true},
	}
	var locations []Location
	if err := srv.Call(ctx, "textDocument/references", params, &locations); err != nil {
		return marshalResult("findReferences", err.Error(), absPath, 0, 0), nil
	}
	// Filter out gitignored files.
	if len(locations) > 0 && wd != "" {
		locations = filterGitIgnored(locations, wd)
	}
	// Truncate if over limit.
	maxR := t.maxResults()
	truncated := len(locations) > maxR
	origCount := len(locations)
	if truncated {
		locations = locations[:maxR]
	}
	formatted := formatFindReferences(locations, wd)
	if truncated {
		formatted += fmt.Sprintf("\n\n… and %d more results (truncated to %d)", origCount-maxR, maxR)
	}
	resultCount := 0
	fileCount := 0
	if len(locations) > 0 {
		resultCount = len(locations)
		files := map[string]struct{}{}
		for _, loc := range locations {
			if loc.URI != "" {
				files[loc.URI] = struct{}{}
			}
		}
		fileCount = len(files)
	}
	return marshalResult("findReferences", formatted, absPath, resultCount, fileCount), nil
}

func (t *LSPTool) hover(ctx context.Context, srv *LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var result Hover
	if err := srv.Call(ctx, "textDocument/hover", params, &result); err != nil {
		return marshalResult("hover", err.Error(), absPath, 0, 0), nil
	}
	formatted := formatHover(&result, wd)
	return marshalResult("hover", formatted, absPath, 1, 1), nil
}

func (t *LSPTool) documentSymbol(ctx context.Context, srv *LSPServer, uri, absPath, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
	}
	var result json.RawMessage
	if err := srv.Call(ctx, "textDocument/documentSymbol", params, &result); err != nil {
		return marshalResult("documentSymbol", err.Error(), absPath, 0, 0), nil
	}
	formatted := formatDocumentSymbolResult(result, wd)
	return marshalResult("documentSymbol", formatted, absPath, 0, 1), nil
}

func (t *LSPTool) workspaceSymbol(ctx context.Context, srv *LSPServer, uri, query, wd string) (string, error) {
	params := map[string]any{
		"query": query,
	}
	var symbols []SymbolInformation
	if err := srv.Call(ctx, "workspace/symbol", params, &symbols); err != nil {
		return marshalResult("workspaceSymbol", err.Error(), "", 0, 0), nil
	}
	// Truncate if over limit.
	maxR := t.maxResults()
	truncated := len(symbols) > maxR
	origCount := len(symbols)
	if truncated {
		symbols = symbols[:maxR]
	}
	formatted := formatWorkspaceSymbol(symbols, wd)
	if truncated {
		formatted += fmt.Sprintf("\n\n… and %d more symbols (truncated to %d)", origCount-maxR, maxR)
	}
	resultCount := len(symbols)
	files := map[string]struct{}{}
	for _, sym := range symbols {
		if sym.Location.URI != "" {
			files[sym.Location.URI] = struct{}{}
		}
	}
	return marshalResult("workspaceSymbol", formatted, "", resultCount, len(files)), nil
}

func (t *LSPTool) goToImplementation(ctx context.Context, srv *LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var result json.RawMessage
	if err := srv.Call(ctx, "textDocument/implementation", params, &result); err != nil {
		return marshalResult("goToImplementation", err.Error(), absPath, 0, 0), nil
	}
	return formatRawLocations("goToImplementation", result, wd, absPath)
}

func (t *LSPTool) prepareCallHierarchy(ctx context.Context, srv *LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var items []CallHierarchyItem
	if err := srv.Call(ctx, "textDocument/prepareCallHierarchy", params, &items); err != nil {
		return marshalResult("prepareCallHierarchy", err.Error(), absPath, 0, 0), nil
	}
	formatted := formatPrepareCallHierarchy(items, wd)
	return marshalResult("prepareCallHierarchy", formatted, absPath, len(items), countUniqueFiles(items)), nil
}

func (t *LSPTool) incomingCalls(ctx context.Context, srv *LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	// Two-step: prepareCallHierarchy → incomingCalls
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var items []CallHierarchyItem
	if err := srv.Call(ctx, "textDocument/prepareCallHierarchy", params, &items); err != nil || len(items) == 0 {
		if err != nil {
			return marshalResult("incomingCalls", err.Error(), absPath, 0, 0), nil
		}
		return marshalResult("incomingCalls", "No call hierarchy item found at this position.", absPath, 0, 0), nil
	}

	callParams := map[string]any{"item": items[0]}
	var calls []CallHierarchyIncomingCall
	if err := srv.Call(ctx, "callHierarchy/incomingCalls", callParams, &calls); err != nil {
		return marshalResult("incomingCalls", err.Error(), absPath, 0, 0), nil
	}
	formatted := formatIncomingCalls(calls, wd)
	return marshalResult("incomingCalls", formatted, absPath, len(calls), countUniqueFilesFromItems(calls)), nil
}

func (t *LSPTool) outgoingCalls(ctx context.Context, srv *LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var items []CallHierarchyItem
	if err := srv.Call(ctx, "textDocument/prepareCallHierarchy", params, &items); err != nil || len(items) == 0 {
		if err != nil {
			return marshalResult("outgoingCalls", err.Error(), absPath, 0, 0), nil
		}
		return marshalResult("outgoingCalls", "No call hierarchy item found at this position.", absPath, 0, 0), nil
	}

	callParams := map[string]any{"item": items[0]}
	var calls []CallHierarchyOutgoingCall
	if err := srv.Call(ctx, "callHierarchy/outgoingCalls", callParams, &calls); err != nil {
		return marshalResult("outgoingCalls", err.Error(), absPath, 0, 0), nil
	}
	formatted := formatOutgoingCalls(calls, wd)
	return marshalResult("outgoingCalls", formatted, absPath, len(calls), countUniqueFilesFromOutgoingItems(calls)), nil
}

// --- helpers ---

type lspToolOutput struct {
	Operation   string `json:"operation"`
	Result      string `json:"result"`
	FilePath    string `json:"filePath"`
	ResultCount int    `json:"resultCount,omitempty"`
	FileCount   int    `json:"fileCount,omitempty"`
}

func marshalResult(op, result, filePath string, resultCount, fileCount int) string {
	out := lspToolOutput{
		Operation:   op,
		Result:      result,
		FilePath:    filePath,
		ResultCount: resultCount,
		FileCount:   fileCount,
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func marshalError(op, msg string) string {
	return marshalResult(op, msg, "", 0, 0)
}

// formatRawLocations handles raw JSON responses for operations that return
// Location, Location[], LocationLink, or LocationLink[].
func formatRawLocations(op string, raw json.RawMessage, wd, absPath string) (string, error) {
	if raw == nil || string(raw) == "null" {
		return marshalResult(op, formatGoToDefinition(nil, wd), absPath, 0, 0), nil
	}

	// Try as LocationLink array first — it has more specific field names
	// (targetUri, targetRange) that won't accidentally match Location's (uri, range).
	// This avoids false positives from Go's json decoder silently accepting
	// unknown fields on mismatched types.
	var links []LocationLink
	if err := json.Unmarshal(raw, &links); err == nil && len(links) > 0 && links[0].TargetURI != "" {
		formatted := formatGoToDefinition(links, wd)
		files := map[string]struct{}{}
		for _, link := range links {
			if link.TargetURI != "" {
				files[link.TargetURI] = struct{}{}
			}
		}
		return marshalResult(op, formatted, absPath, len(links), len(files)), nil
	}

	// Try as single LocationLink.
	var link LocationLink
	if err := json.Unmarshal(raw, &link); err == nil && link.TargetURI != "" {
		formatted := formatGoToDefinition(link, wd)
		return marshalResult(op, formatted, absPath, 1, 1), nil
	}

	// Try as Location array.
	var locs []Location
	if err := json.Unmarshal(raw, &locs); err == nil {
		formatted := formatGoToDefinition(locs, wd)
		resultCount := len(locs)
		files := map[string]struct{}{}
		for _, loc := range locs {
			if loc.URI != "" {
				files[loc.URI] = struct{}{}
			}
		}
		return marshalResult(op, formatted, absPath, resultCount, len(files)), nil
	}

	// Try as single Location.
	var loc Location
	if err := json.Unmarshal(raw, &loc); err == nil {
		formatted := formatGoToDefinition(loc, wd)
		return marshalResult(op, formatted, absPath, 1, 1), nil
	}

	return marshalResult(op, "No definition found.", absPath, 0, 0), nil
}

// formatDocumentSymbolResult handles both DocumentSymbol[] and SymbolInformation[] JSON.
func formatDocumentSymbolResult(raw json.RawMessage, wd string) string {
	if raw == nil || string(raw) == "null" {
		return formatDocumentSymbol(nil, wd)
	}

	// Try DocumentSymbol[] first (hierarchical).
	var docSyms []DocumentSymbol
	if err := json.Unmarshal(raw, &docSyms); err == nil && len(docSyms) > 0 {
		return formatDocumentSymbol(docSyms, wd)
	}

	// Try SymbolInformation[] (flat).
	var infoSyms []SymbolInformation
	if err := json.Unmarshal(raw, &infoSyms); err == nil {
		return formatDocumentSymbol(infoSyms, wd)
	}

	return "No symbols found in document."
}

func countUniqueFiles(items []CallHierarchyItem) int {
	files := map[string]struct{}{}
	for _, item := range items {
		if item.URI != "" {
			files[item.URI] = struct{}{}
		}
	}
	return len(files)
}

func countUniqueFilesFromItems(calls []CallHierarchyIncomingCall) int {
	files := map[string]struct{}{}
	for _, call := range calls {
		if call.From.URI != "" {
			files[call.From.URI] = struct{}{}
		}
	}
	return len(files)
}

func countUniqueFilesFromOutgoingItems(calls []CallHierarchyOutgoingCall) int {
	files := map[string]struct{}{}
	for _, call := range calls {
		if call.To.URI != "" {
			files[call.To.URI] = struct{}{}
		}
	}
	return len(files)
}

// checkCapability verifies the server supports the requested operation.
// Returns an error message string, or empty string if supported.
func checkCapability(op string, caps ServerCapabilities) string {
	switch op {
	case "goToDefinition":
		if caps.DefinitionProvider == nil {
			return "goToDefinition is not supported by this LSP server"
		}
	case "findReferences":
		if caps.ReferencesProvider == nil {
			return "findReferences is not supported by this LSP server"
		}
	case "hover":
		if caps.HoverProvider == nil {
			return "hover is not supported by this LSP server"
		}
	case "documentSymbol":
		if caps.DocumentSymbolProvider == nil {
			return "documentSymbol is not supported by this LSP server"
		}
	case "workspaceSymbol":
		if caps.WorkspaceSymbolProvider == nil {
			return "workspaceSymbol is not supported by this LSP server"
		}
	case "goToImplementation":
		if caps.ImplementationProvider == nil {
			return "goToImplementation is not supported by this LSP server"
		}
	case "prepareCallHierarchy", "incomingCalls", "outgoingCalls":
		if caps.CallHierarchyProvider == nil {
			return "call hierarchy operations are not supported by this LSP server"
		}
	}
	return ""
}

// ExtractFilePath extracts a file path from a tool call's input JSON, if the
// tool modifies file content and should trigger an LSP file sync.
// Returns empty string if the tool doesn't operate on a specific file.
func ExtractFilePath(toolName, inputJSON string) string {
	if toolName == "" || inputJSON == "" {
		return ""
	}

	// Only sync for tools that read or write file content.
	switch toolName {
	case "ReadFile", "WriteFile", "EditFile", "SendFile":
	default:
		return ""
	}

	var parsed struct {
		Path     string `json:"path"`
		FilePath string `json:"filePath"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &parsed); err != nil {
		return ""
	}
	if parsed.FilePath != "" {
		return parsed.FilePath
	}
	return parsed.Path
}

// maxResults returns the configured max_results with a sensible default.
func (t *LSPTool) maxResults() int {
	return t.manager.MaxResults()
}

// Ensure LSPTool implements tools.Tool.
var _ tools.Tool = (*LSPTool)(nil)
