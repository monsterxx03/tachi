package tools

import "encoding/json"

// PropertySchema defines a single property in the schema
type PropertySchema struct {
	Type        string
	Description string
}

// Tool is the interface that all tools must implement
type Tool interface {
	Name() string
	Description() string
	Properties() map[string]PropertySchema
	Required() []string
	Parallel() bool
	Execute(args string) (string, error)
}

// ConfirmationTool is an optional interface for tools that require
// user confirmation before execution. The tool should return a diff
// preview via GetDiff() when NeedsConfirmation() returns true.
type ConfirmationTool interface {
	NeedsConfirmation() bool
	GetDiff(args string) (string, error)
}

// ToolPendingError indicates a tool requires user confirmation before execution
type ToolPendingError struct {
	Name string
	Args string
	Diff string
}

func (e *ToolPendingError) Error() string {
	return "tool requires confirmation"
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

// Invoke calls a tool with the given arguments
func (r *Registry) Invoke(name string, args string) (string, error) {
	return r.InvokeWithTool(name, args, nil)
}

// InvokeWithTool calls a tool with the given arguments and returns the tool instance.
// If confirmCh is provided and the tool requires confirmation, it returns a ToolPendingError.
func (r *Registry) InvokeWithTool(name string, args string, confirmCh chan<- bool) (string, error) {
	tool, ok := r.tools[name]
	if !ok {
		return "", &UnknownToolError{name}
	}

	if err := validateArgs(tool, args); err != nil {
		return "", err
	}

	// Check if tool requires confirmation
	if ct, ok := tool.(ConfirmationTool); ok && ct.NeedsConfirmation() {
		diff, err := ct.GetDiff(args)
		if err != nil {
			return "", err
		}
		return "", &ToolPendingError{Name: name, Args: args, Diff: diff}
	}

	return tool.Execute(args)
}

// ExecuteConfirmed executes a tool that was previously pending confirmation
func (r *Registry) ExecuteConfirmed(name string, args string) (string, error) {
	tool, ok := r.tools[name]
	if !ok {
		return "", &UnknownToolError{name}
	}
	return tool.Execute(args)
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

// GetSchemas returns all tool schemas
func (r *Registry) GetSchemas() []Schema {
	schemas := make([]Schema, 0, len(r.tools))
	for _, t := range r.tools {
		schemas = append(schemas, ToSchema(t))
	}
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
