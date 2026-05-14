package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Tool name constants. Use these instead of string literals to avoid typos
// and enable compile-time checking when filtering or comparing tool names.
const (
	ToolNameAskUser   = "AskUserQuestion"
	ToolNameSubAgent  = "SubAgent"
	ToolNameBash      = "Bash"
	ToolNameRead      = "ReadFile"
	ToolNameWrite     = "WriteFile"
	ToolNameEdit      = "EditFile"
	ToolNameGlob      = "Glob"
	ToolNameGrep      = "Grep"
	ToolNameWebSearch = "WebSearch"
	ToolNameWebFetch  = "WebFetch"
	ToolNameSkillsList  = "SkillsList"
	ToolNameSkillView   = "SkillView"
	ToolNameSkillCreate = "SkillCreate"
)

// PropertySchema defines a single property in the schema
type PropertySchema struct {
	Type        string
	Description string
	Items       any // JSON Schema for array element type
}

// Tool is the interface that all tools must implement
type Tool interface {
	Name() string
	Description() string
	Properties() map[string]PropertySchema
	Required() []string
	Parallel() bool
	ExecuteContext(ctx context.Context, args string) (string, error)
}

// ConfirmationTool is an optional interface for tools that require
// user confirmation before execution. The tool should return a diff
// preview via GetDiff() when NeedsConfirmation() returns true.
type ConfirmationTool interface {
	NeedsConfirmation() bool
	GetDiff(ctx context.Context, args string) (string, error)
}

// SubagentIDCarrier is an optional interface for tools whose execution
// produces a sub-agent ID that callers may want to track.
type SubagentIDCarrier interface {
	LastSubagentID() string
}

type ToolResultStatus int

const (
	ToolResultSuccess ToolResultStatus = iota
	ToolResultPendingConfirm
	ToolResultNeedUserInput
	ToolResultError
)

type ToolResult struct {
	Status     ToolResultStatus
	Output     string
	Err        error
	Name       string
	Args       string
	Diff       string
	Questions  []Question
	SubagentID string        // SubAgent shortID, for linking to subagent/<id>.jsonl
	Duration   time.Duration // Wall-clock duration of tool execution
}

// Schema defines the JSON schema for a tool
type Schema struct {
	Name        string
	Description string
	Parameters  ParametersSchema
}

// ParametersSchema defines the JSON schema for tool parameters
type ParametersSchema struct {
	Type       string
	Properties map[string]PropertySchema
	Required   []string
}

// ToSchema converts a Tool to its Schema representation
func ToSchema(t Tool) Schema {
	return Schema{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: ParametersSchema{
			Type:       "object",
			Properties: t.Properties(),
			Required:   t.Required(),
		},
	}
}

// Registry maintains a collection of available tools
type Registry struct {
	tools map[string]Tool
}

// NewRegistry creates a new tool registry
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Register registers a tool with the registry
func (r *Registry) Register(tool Tool) {
	r.tools[tool.Name()] = tool
}

// Unregister removes a tool from the registry by name.
// Returns true if the tool was removed, false if it wasn't registered.
func (r *Registry) Unregister(name string) bool {
	_, ok := r.tools[name]
	delete(r.tools, name)
	return ok
}

// GetTool returns the tool with the given name, or nil if not found.
func (r *Registry) GetTool(name string) Tool {
	return r.tools[name]
}

// GetToolNames returns all registered tool names in deterministic order
// (built-in first alphabetically, then MCP tools alphabetically).
func (r *Registry) GetToolNames() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		iMCP := strings.HasPrefix(names[i], "mcp__")
		jMCP := strings.HasPrefix(names[j], "mcp__")
		if iMCP != jMCP {
			return !iMCP
		}
		return names[i] < names[j]
	})
	return names
}

// Invoke calls a tool with the given arguments and context.
func (r *Registry) Invoke(ctx context.Context, name string, args string) ToolResult {
	tool, ok := r.tools[name]
	if !ok {
		return ToolResult{Status: ToolResultError, Err: &UnknownToolError{name}}
	}

	if err := validateArgs(tool, args); err != nil {
		return ToolResult{Status: ToolResultError, Err: err}
	}

	if ct, ok := tool.(ConfirmationTool); ok && ct.NeedsConfirmation() {
		diff, err := ct.GetDiff(ctx, args)
		if err != nil {
			return ToolResult{Status: ToolResultError, Err: err}
		}
		return ToolResult{Status: ToolResultPendingConfirm, Name: name, Args: args, Diff: diff}
	}

	startTime := time.Now()
	result, err := tool.ExecuteContext(ctx, args)
	resultDuration := time.Since(startTime)
	if askErr, ok := err.(*AskUserQuestionError); ok {
		return ToolResult{Status: ToolResultNeedUserInput, Name: askErr.ToolName, Args: askErr.Args, Questions: askErr.Questions, Duration: resultDuration}
	}
	if err != nil {
		return ToolResult{Status: ToolResultError, Err: err, Duration: resultDuration}
	}

	tr := ToolResult{Status: ToolResultSuccess, Output: result, Duration: resultDuration}
	if carrier, ok := tool.(SubagentIDCarrier); ok {
		tr.SubagentID = carrier.LastSubagentID()
	}
	return tr
}

// IsParallel returns whether the named tool supports parallel execution.
// Returns false for unknown tools (conservative default).
func (r *Registry) IsParallel(name string) bool {
	tool, ok := r.tools[name]
	if !ok {
		return false
	}
	return tool.Parallel()
}

// ExecuteConfirmed executes a tool that was previously pending confirmation, with context
func (r *Registry) ExecuteConfirmed(ctx context.Context, name string, args string) (string, error) {
	tool, ok := r.tools[name]
	if !ok {
		return "", &UnknownToolError{name}
	}

	return tool.ExecuteContext(ctx, args)
}

// validateArgs checks if the arguments match the tool's schema
func validateArgs(tool Tool, args string) error {
	if args == "" {
		args = "{}"
	}

	var argMap map[string]any
	if err := json.Unmarshal([]byte(args), &argMap); err != nil {
		return &InvalidArgsError{Name: tool.Name(), Err: err}
	}

	// Check required fields
	for _, field := range tool.Required() {
		if _, ok := argMap[field]; !ok {
			return &MissingArgError{Name: tool.Name(), Arg: field}
		}
	}

	return nil
}

// GetSchemas returns all tool schemas in deterministic order:
// built-in tools first (alphabetically by name), then MCP tools (alphabetically).
// Deterministic ordering is critical for LLM prompt caching — both OpenAI and
// Anthropic cache the tools prefix, and any change in order invalidates the cache.
func (r *Registry) GetSchemas() []Schema {
	schemas := make([]Schema, 0, len(r.tools))
	for _, t := range r.tools {
		schemas = append(schemas, ToSchema(t))
	}
	sort.Slice(schemas, func(i, j int) bool {
		iMCP := strings.HasPrefix(schemas[i].Name, "mcp__")
		jMCP := strings.HasPrefix(schemas[j].Name, "mcp__")
		if iMCP != jMCP {
			return !iMCP // built-in tools before MCP tools
		}
		return schemas[i].Name < schemas[j].Name
	})
	return schemas
}

// UnknownToolError is returned when a tool is not found
type UnknownToolError struct {
	Name string
}

func (e *UnknownToolError) Error() string {
	return "unknown tool: " + e.Name
}

// InvalidArgsError is returned when arguments are invalid JSON
type InvalidArgsError struct {
	Name string
	Err  error
}

func (e *InvalidArgsError) Error() string {
	return "invalid arguments for tool " + e.Name + ": " + e.Err.Error()
}

// MissingArgError is returned when a required argument is missing
type MissingArgError struct {
	Name string
	Arg  string
}

func (e *MissingArgError) Error() string {
	return "missing required argument '" + e.Arg + "' for tool " + e.Name
}

// AskUserQuestionError indicates a tool is an AskUserQuestion tool that needs user input
type AskUserQuestionError struct {
	ToolName string
	Args     string
	Questions []Question
}

func (e *AskUserQuestionError) Error() string {
	return "tool requires user input"
}