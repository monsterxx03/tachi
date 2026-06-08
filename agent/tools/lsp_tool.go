package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/monsterxx03/tachi/agent/lsp"
	"github.com/monsterxx03/tachi/agent/wdctx"
)

// LSPToolName is the name exposed to the LLM.
const LSPToolName = "LSP"

// LSPDescription is the tool description shown to the LLM.
// Key design: communicate WHY the LLM should use this instead of Grep/ReadFile.
const LSPDescription = `Language-aware code intelligence tool. More accurate than Grep for code navigation because it understands the language semantics (not just text matching).

When to use this tool:
- You need to jump to a symbol's definition or implementation
- You need to find all references/usages of a function, type, or variable
- You need type information, documentation, or signature details for a symbol
- You need an overview of all symbols in a file or workspace
- You need to understand call relationships between functions

Supported operations:
- goToDefinition: Find where a symbol is defined (more reliable than Grep for finding the correct definition)
- findReferences: Find all references to a symbol across the project (avoids Grep false positives from comments/strings)
- hover: Get documentation, type info, and signature for a symbol at a position
- documentSymbol: Get all symbols (functions, types, variables) defined in a file
- workspaceSymbol: Search for symbols by name across the entire workspace (like fuzzy-find for code)
- goToImplementation: Find implementations of an interface or abstract method
- prepareCallHierarchy / incomingCalls / outgoingCalls: Trace function call relationships

Parameters:
- operation (required): which LSP operation to perform
- filePath (required): the file containing the symbol
- line (optional, 1-based): required for goToDefinition/findReferences/hover/goToImplementation/callHierarchy
- character (optional, 1-based): required for goToDefinition/findReferences/hover/goToImplementation/callHierarchy
- query (optional): search query — only needed for workspaceSymbol operation`

// LSPTool implements the tools.Tool interface for LSP code intelligence.
type LSPTool struct {
	manager *lsp.LSPManager
}

// NewLSPTool creates a new LSP tool.
func NewLSPTool(manager *lsp.LSPManager) *LSPTool {
	return &LSPTool{manager: manager}
}

func (t *LSPTool) Name() string { return LSPToolName }

func (t *LSPTool) Description() string { return LSPDescription }

func (t *LSPTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
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
	return []string{"operation", "filePath"}
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
		return lspMarshalError("LSP", fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	// Resolve file path relative to working directory.
	wd := wdctx.Dir(ctx)
	absPath := input.FilePath
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(wd, absPath)
	}
	absPath = filepath.Clean(absPath)

	// Operations that do NOT need line/character: workspaceSymbol, documentSymbol.
	needsPosition := input.Operation != "workspaceSymbol" && input.Operation != "documentSymbol"
	if needsPosition {
		if input.Line <= 0 {
			return lspMarshalError(input.Operation, "line is required (1-based) for this operation"), nil
		}
		if input.Character < 0 {
			return lspMarshalError(input.Operation, "character is required (1-based) for this operation"), nil
		}
	}

	// Get or start the LSP server for this file type.
	server, err := t.manager.GetServer(ctx, absPath)
	if err != nil {
		return lspMarshalError(input.Operation, fmt.Sprintf("LSP server error: %v", err)), nil
	}
	if server == nil {
		ext := filepath.Ext(absPath)
		return lspMarshalError(input.Operation, fmt.Sprintf("No LSP server available for file type %s", ext)), nil
	}

	uri := lsp.PathToURI(absPath)

	// Sync file to LSP server (didOpen if not yet open).
	if err := t.manager.SyncFile(ctx, absPath); err != nil {
		return lspMarshalError(input.Operation, fmt.Sprintf("file sync error: %v", err)), nil
	}

	// Convert 1-based to 0-based.
	line := uint32(input.Line - 1)
	char := uint32(input.Character - 1)

	// Capability pre-checks.
	caps := server.Capabilities()
	if msg := checkCapability(input.Operation, caps); msg != "" {
		return lspMarshalError(input.Operation, msg), nil
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
		return lspMarshalError(input.Operation, fmt.Sprintf("unknown operation: %s", input.Operation)), nil
	}
}

func (t *LSPTool) goToDefinition(ctx context.Context, srv *lsp.LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var result json.RawMessage
	if err := srv.Call(ctx, "textDocument/definition", params, &result); err != nil {
		return lspMarshalResult("goToDefinition", err.Error(), absPath, 0, 0), nil
	}
	return formatRawLocations("goToDefinition", result, wd, absPath)
}

func (t *LSPTool) findReferences(ctx context.Context, srv *lsp.LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
		"context":      map[string]any{"includeDeclaration": true},
	}
	var locations []lsp.Location
	if err := srv.Call(ctx, "textDocument/references", params, &locations); err != nil {
		return lspMarshalResult("findReferences", err.Error(), absPath, 0, 0), nil
	}
	// Filter out gitignored files.
	if len(locations) > 0 && wd != "" {
		locations = lsp.FilterGitIgnored(locations, wd)
	}
	// Truncate if over limit.
	maxR := t.maxResults()
	truncated := len(locations) > maxR
	origCount := len(locations)
	if truncated {
		locations = locations[:maxR]
	}
	formatted := lsp.FormatFindReferences(locations, wd)
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
	return lspMarshalResult("findReferences", formatted, absPath, resultCount, fileCount), nil
}

func (t *LSPTool) hover(ctx context.Context, srv *lsp.LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var result lsp.Hover
	if err := srv.Call(ctx, "textDocument/hover", params, &result); err != nil {
		return lspMarshalResult("hover", err.Error(), absPath, 0, 0), nil
	}
	formatted := lsp.FormatHover(&result, wd)
	return lspMarshalResult("hover", formatted, absPath, 1, 1), nil
}

func (t *LSPTool) documentSymbol(ctx context.Context, srv *lsp.LSPServer, uri, absPath, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
	}
	var result json.RawMessage
	if err := srv.Call(ctx, "textDocument/documentSymbol", params, &result); err != nil {
		return lspMarshalResult("documentSymbol", err.Error(), absPath, 0, 0), nil
	}
	formatted := formatDocumentSymbolResult(result, wd)
	return lspMarshalResult("documentSymbol", formatted, absPath, 0, 1), nil
}

func (t *LSPTool) workspaceSymbol(ctx context.Context, srv *lsp.LSPServer, uri, query, wd string) (string, error) {
	params := map[string]any{
		"query": query,
	}
	var symbols []lsp.SymbolInformation
	if err := srv.Call(ctx, "workspace/symbol", params, &symbols); err != nil {
		return lspMarshalResult("workspaceSymbol", err.Error(), "", 0, 0), nil
	}
	// Truncate if over limit.
	maxR := t.maxResults()
	truncated := len(symbols) > maxR
	origCount := len(symbols)
	if truncated {
		symbols = symbols[:maxR]
	}
	formatted := lsp.FormatWorkspaceSymbol(symbols, wd)
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
	return lspMarshalResult("workspaceSymbol", formatted, "", resultCount, len(files)), nil
}

func (t *LSPTool) goToImplementation(ctx context.Context, srv *lsp.LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var result json.RawMessage
	if err := srv.Call(ctx, "textDocument/implementation", params, &result); err != nil {
		return lspMarshalResult("goToImplementation", err.Error(), absPath, 0, 0), nil
	}
	return formatRawLocations("goToImplementation", result, wd, absPath)
}

func (t *LSPTool) prepareCallHierarchy(ctx context.Context, srv *lsp.LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var items []lsp.CallHierarchyItem
	if err := srv.Call(ctx, "textDocument/prepareCallHierarchy", params, &items); err != nil {
		return lspMarshalResult("prepareCallHierarchy", err.Error(), absPath, 0, 0), nil
	}
	formatted := lsp.FormatPrepareCallHierarchy(items, wd)
	return lspMarshalResult("prepareCallHierarchy", formatted, absPath, len(items), countUniqueFiles(items)), nil
}

func (t *LSPTool) incomingCalls(ctx context.Context, srv *lsp.LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	// Two-step: prepareCallHierarchy → incomingCalls
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var items []lsp.CallHierarchyItem
	if err := srv.Call(ctx, "textDocument/prepareCallHierarchy", params, &items); err != nil || len(items) == 0 {
		if err != nil {
			return lspMarshalResult("incomingCalls", err.Error(), absPath, 0, 0), nil
		}
		return lspMarshalResult("incomingCalls", "No call hierarchy item found at this position.", absPath, 0, 0), nil
	}

	callParams := map[string]any{"item": items[0]}
	var calls []lsp.CallHierarchyIncomingCall
	if err := srv.Call(ctx, "callHierarchy/incomingCalls", callParams, &calls); err != nil {
		return lspMarshalResult("incomingCalls", err.Error(), absPath, 0, 0), nil
	}
	formatted := lsp.FormatIncomingCalls(calls, wd)
	return lspMarshalResult("incomingCalls", formatted, absPath, len(calls), countUniqueFilesFromItems(calls)), nil
}

func (t *LSPTool) outgoingCalls(ctx context.Context, srv *lsp.LSPServer, uri, absPath string, line, char uint32, wd string) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}
	var items []lsp.CallHierarchyItem
	if err := srv.Call(ctx, "textDocument/prepareCallHierarchy", params, &items); err != nil || len(items) == 0 {
		if err != nil {
			return lspMarshalResult("outgoingCalls", err.Error(), absPath, 0, 0), nil
		}
		return lspMarshalResult("outgoingCalls", "No call hierarchy item found at this position.", absPath, 0, 0), nil
	}

	callParams := map[string]any{"item": items[0]}
	var calls []lsp.CallHierarchyOutgoingCall
	if err := srv.Call(ctx, "callHierarchy/outgoingCalls", callParams, &calls); err != nil {
		return lspMarshalResult("outgoingCalls", err.Error(), absPath, 0, 0), nil
	}
	formatted := lsp.FormatOutgoingCalls(calls, wd)
	return lspMarshalResult("outgoingCalls", formatted, absPath, len(calls), countUniqueFilesFromOutgoingItems(calls)), nil
}

// --- helpers ---

type lspToolOutput struct {
	Operation   string `json:"operation"`
	Result      string `json:"result"`
	FilePath    string `json:"filePath"`
	ResultCount int    `json:"resultCount,omitempty"`
	FileCount   int    `json:"fileCount,omitempty"`
}

func lspMarshalResult(op, result, filePath string, resultCount, fileCount int) string {
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

func lspMarshalError(op, msg string) string {
	return lspMarshalResult(op, msg, "", 0, 0)
}

// formatRawLocations handles raw JSON responses for operations that return
// Location, Location[], LocationLink, or LocationLink[].
func formatRawLocations(op string, raw json.RawMessage, wd, absPath string) (string, error) {
	if raw == nil || string(raw) == "null" {
		return lspMarshalResult(op, lsp.FormatGoToDefinition(nil, wd), absPath, 0, 0), nil
	}

	// Try as LocationLink array first — it has more specific field names
	// (targetUri, targetRange) that won't accidentally match Location's (uri, range).
	// This avoids false positives from Go's json decoder silently accepting
	// unknown fields on mismatched types.
	var links []lsp.LocationLink
	if err := json.Unmarshal(raw, &links); err == nil && len(links) > 0 && links[0].TargetURI != "" {
		formatted := lsp.FormatGoToDefinition(links, wd)
		files := map[string]struct{}{}
		for _, link := range links {
			if link.TargetURI != "" {
				files[link.TargetURI] = struct{}{}
			}
		}
		return lspMarshalResult(op, formatted, absPath, len(links), len(files)), nil
	}

	// Try as single LocationLink.
	var link lsp.LocationLink
	if err := json.Unmarshal(raw, &link); err == nil && link.TargetURI != "" {
		formatted := lsp.FormatGoToDefinition(link, wd)
		return lspMarshalResult(op, formatted, absPath, 1, 1), nil
	}

	// Try as Location array.
	var locs []lsp.Location
	if err := json.Unmarshal(raw, &locs); err == nil {
		formatted := lsp.FormatGoToDefinition(locs, wd)
		resultCount := len(locs)
		files := map[string]struct{}{}
		for _, loc := range locs {
			if loc.URI != "" {
				files[loc.URI] = struct{}{}
			}
		}
		return lspMarshalResult(op, formatted, absPath, resultCount, len(files)), nil
	}

	// Try as single Location.
	var loc lsp.Location
	if err := json.Unmarshal(raw, &loc); err == nil {
		formatted := lsp.FormatGoToDefinition(loc, wd)
		return lspMarshalResult(op, formatted, absPath, 1, 1), nil
	}

	return lspMarshalResult(op, "No definition found.", absPath, 0, 0), nil
}

// formatDocumentSymbolResult handles both DocumentSymbol[] and SymbolInformation[] JSON.
func formatDocumentSymbolResult(raw json.RawMessage, wd string) string {
	if raw == nil || string(raw) == "null" {
		return lsp.FormatDocumentSymbol(nil, wd)
	}

	// Try DocumentSymbol[] first (hierarchical).
	var docSyms []lsp.DocumentSymbol
	if err := json.Unmarshal(raw, &docSyms); err == nil && len(docSyms) > 0 {
		return lsp.FormatDocumentSymbol(docSyms, wd)
	}

	// Try SymbolInformation[] (flat).
	var infoSyms []lsp.SymbolInformation
	if err := json.Unmarshal(raw, &infoSyms); err == nil {
		return lsp.FormatDocumentSymbol(infoSyms, wd)
	}

	return "No symbols found in document."
}

func countUniqueFiles(items []lsp.CallHierarchyItem) int {
	files := map[string]struct{}{}
	for _, item := range items {
		if item.URI != "" {
			files[item.URI] = struct{}{}
		}
	}
	return len(files)
}

func countUniqueFilesFromItems(calls []lsp.CallHierarchyIncomingCall) int {
	files := map[string]struct{}{}
	for _, call := range calls {
		if call.From.URI != "" {
			files[call.From.URI] = struct{}{}
		}
	}
	return len(files)
}

func countUniqueFilesFromOutgoingItems(calls []lsp.CallHierarchyOutgoingCall) int {
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
func checkCapability(op string, caps lsp.ServerCapabilities) string {
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
var _ Tool = (*LSPTool)(nil)
