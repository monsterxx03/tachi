package lsp

// LSP protocol types — minimal subset needed by LSPTool.
// All position values are 0-based as per LSP spec; LSPTool converts
// from 1-based (user-facing) to 0-based before calling these types.

// Position represents a zero-based line and character position.
type Position struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

// Range defines a region in a document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location represents a location in a document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// LocationLink represents a link from a source location to a target location.
type LocationLink struct {
	OriginSelectionRange *Range `json:"originSelectionRange,omitempty"`
	TargetURI            string `json:"targetUri"`
	TargetRange          Range  `json:"targetRange"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}

// Hover represents hover information for a symbol.
type Hover struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

// MarkupContent represents a markup content (markdown or plaintext).
type MarkupContent struct {
	Kind  string `json:"kind"`  // "plaintext" | "markdown"
	Value string `json:"value"`
}

// MarkedString is a string with optional language identifier.
type MarkedString struct {
	Language string `json:"language"`
	Value    string `json:"value"`
}

// DocumentSymbol represents a symbol in a document with hierarchical children.
type DocumentSymbol struct {
	Name           string            `json:"name"`
	Detail         string            `json:"detail,omitempty"`
	Kind           SymbolKind        `json:"kind"`
	Tags           []SymbolTag       `json:"tags,omitempty"`
	Range          Range             `json:"range"`
	SelectionRange Range             `json:"selectionRange"`
	Children       []DocumentSymbol  `json:"children,omitempty"`
}

// SymbolInformation represents a symbol in the workspace (flat structure).
type SymbolInformation struct {
	Name          string     `json:"name"`
	Kind          SymbolKind `json:"kind"`
	Tags          []SymbolTag `json:"tags,omitempty"`
	Location      Location   `json:"location"`
	ContainerName string     `json:"containerName,omitempty"`
}

// SymbolKind is the kind of a symbol.
type SymbolKind uint32

const (
	SKFile          SymbolKind = 1
	SKModule        SymbolKind = 2
	SKNamespace     SymbolKind = 3
	SKPackage       SymbolKind = 4
	SKClass         SymbolKind = 5
	SKMethod        SymbolKind = 6
	SKProperty      SymbolKind = 7
	SKField         SymbolKind = 8
	SKConstructor   SymbolKind = 9
	SKEnum          SymbolKind = 10
	SKInterface     SymbolKind = 11
	SKFunction      SymbolKind = 12
	SKVariable      SymbolKind = 13
	SKConstant      SymbolKind = 14
	SKString        SymbolKind = 15
	SKNumber        SymbolKind = 16
	SKBoolean       SymbolKind = 17
	SKArray         SymbolKind = 18
	SKObject        SymbolKind = 19
	SKKey           SymbolKind = 20
	SKNull          SymbolKind = 21
	SKEnumMember    SymbolKind = 22
	SKStruct        SymbolKind = 23
	SKEvent         SymbolKind = 24
	SKOperator      SymbolKind = 25
	SKTypeParameter SymbolKind = 26
)

// SymbolTag represents extra modifiers on a symbol.
type SymbolTag uint32

const (
	SymbolTagDeprecated    SymbolTag = 1
	SymbolTagUnnecessary   SymbolTag = 2
)

// CallHierarchyItem represents a call hierarchy entry.
type CallHierarchyItem struct {
	Name           string     `json:"name"`
	Kind           SymbolKind `json:"kind"`
	Tags           []SymbolTag `json:"tags,omitempty"`
	Detail         string     `json:"detail,omitempty"`
	URI            string     `json:"uri"`
	Range          Range      `json:"range"`
	SelectionRange Range      `json:"selectionRange"`
}

// CallHierarchyIncomingCall represents a call to the target.
type CallHierarchyIncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

// CallHierarchyOutgoingCall represents a call from the target.
type CallHierarchyOutgoingCall struct {
	To         CallHierarchyItem `json:"to"`
	FromRanges []Range           `json:"fromRanges"`
}

// Diagnostic represents a diagnostic (e.g. error, warning) from an LSP server.
type Diagnostic struct {
	Range    Range             `json:"range"`
	Severity DiagnosticSeverity `json:"severity,omitempty"`
	Code     any               `json:"code,omitempty"`
	Source   string            `json:"source,omitempty"`
	Message  string            `json:"message"`
	Tags     []DiagnosticTag   `json:"tags,omitempty"`
}

// DiagnosticSeverity defines the severity levels for diagnostics.
type DiagnosticSeverity uint32

const (
	SeverityError       DiagnosticSeverity = 1
	SeverityWarning     DiagnosticSeverity = 2
	SeverityInformation DiagnosticSeverity = 3
	SeverityHint        DiagnosticSeverity = 4
)

// DiagnosticTag represents extra diagnostic tags.
type DiagnosticTag uint32

const (
	DiagnosticTagUnnecessary DiagnosticTag = 1
	DiagnosticTagDeprecated  DiagnosticTag = 2
)

// PublishDiagnosticsParams is sent from server to client.
type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// ServerCapabilities represents capabilities the server supports.
type ServerCapabilities struct {
	TextDocumentSync   any              `json:"textDocumentSync,omitempty"`
	HoverProvider      any              `json:"hoverProvider,omitempty"`
	DefinitionProvider any              `json:"definitionProvider,omitempty"`
	ReferencesProvider any              `json:"referencesProvider,omitempty"`
	DocumentSymbolProvider any          `json:"documentSymbolProvider,omitempty"`
	WorkspaceSymbolProvider any         `json:"workspaceSymbolProvider,omitempty"`
	ImplementationProvider any          `json:"implementationProvider,omitempty"`
	CallHierarchyProvider any           `json:"callHierarchyProvider,omitempty"`
}

// InitializeParams sent from client to server during initialization.
type InitializeParams struct {
	ProcessID             int                    `json:"processId"`
	ClientInfo            *ClientInfo            `json:"clientInfo,omitempty"`
	RootURI               string                 `json:"rootUri"`
	Capabilities          map[string]any         `json:"capabilities"`
	InitializationOptions map[string]any         `json:"initializationOptions,omitempty"`
	Trace                 string                 `json:"trace,omitempty"`
	WorkspaceFolders      []WorkspaceFolder      `json:"workspaceFolders,omitempty"`
}

// ClientInfo identifies the client.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// WorkspaceFolder represents a workspace folder.
type WorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// InitializeResult is the server's response to initialize.
type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
	ServerInfo   *ServerInfo        `json:"serverInfo,omitempty"`
}

// ServerInfo identifies the server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// SymbolKindString returns a human-readable name for a SymbolKind.
func SymbolKindString(kind SymbolKind) string {
	switch kind {
	case SKFile:
		return "File"
	case SKModule:
		return "Module"
	case SKNamespace:
		return "Namespace"
	case SKPackage:
		return "Package"
	case SKClass:
		return "Class"
	case SKMethod:
		return "Method"
	case SKProperty:
		return "Property"
	case SKField:
		return "Field"
	case SKConstructor:
		return "Constructor"
	case SKEnum:
		return "Enum"
	case SKInterface:
		return "Interface"
	case SKFunction:
		return "Function"
	case SKVariable:
		return "Variable"
	case SKConstant:
		return "Constant"
	case SKString:
		return "String"
	case SKNumber:
		return "Number"
	case SKBoolean:
		return "Boolean"
	case SKArray:
		return "Array"
	case SKObject:
		return "Object"
	case SKKey:
		return "Key"
	case SKNull:
		return "Null"
	case SKEnumMember:
		return "EnumMember"
	case SKStruct:
		return "Struct"
	case SKEvent:
		return "Event"
	case SKOperator:
		return "Operator"
	case SKTypeParameter:
		return "TypeParameter"
	default:
		return "Unknown"
	}
}
