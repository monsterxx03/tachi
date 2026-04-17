package tools

import (
	"encoding/json"
	"fmt"
	"os"
)

// Tool is a function that takes JSON arguments and returns a result or error
type Tool func(args string) (string, error)

// ParametersSchema defines the JSON schema for tool parameters
type ParametersSchema struct {
	Type       string
	Properties map[string]PropertySchema
	Required   []string
}

// PropertySchema defines a single property in the schema
type PropertySchema struct {
	Type        string
	Description string
}

// Schema defines the JSON schema for a tool
type Schema struct {
	Name        string
	Description string
	Parameters  ParametersSchema
}

// Registry maintains a collection of available tools
type Registry struct {
	tools   map[string]Tool
	schemas map[string]Schema
}

// NewRegistry creates a new tool registry
func NewRegistry() *Registry {
	return &Registry{
		tools:   make(map[string]Tool),
		schemas: make(map[string]Schema),
	}
}

// Register registers a tool with its schema
func (r *Registry) Register(name string, desc string, properties map[string]PropertySchema, required []string, tool Tool) {
	r.tools[name] = tool
	r.schemas[name] = Schema{
		Name:        name,
		Description: desc,
		Parameters: ParametersSchema{
			Type:       "object",
			Properties: properties,
			Required:   required,
		},
	}
}

// Invoke calls a tool with the given arguments
func (r *Registry) Invoke(name string, args string) (string, error) {
	tool, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}

	// Validate arguments against schema
	schema, hasSchema := r.schemas[name]
	if hasSchema {
		if err := r.validateArgs(schema, args); err != nil {
			return "", err
		}
	}

	return tool(args)
}

// validateArgs checks if the arguments match the schema
func (r *Registry) validateArgs(schema Schema, args string) error {
	if args == "" {
		args = "{}"
	}

	var argMap map[string]any
	if err := json.Unmarshal([]byte(args), &argMap); err != nil {
		return fmt.Errorf("invalid JSON arguments: %w", err)
	}

	// Check required fields
	for _, field := range schema.Parameters.Required {
		if _, ok := argMap[field]; !ok {
			return fmt.Errorf("missing required argument: %s", field)
		}
	}

	return nil
}

// GetSchemas returns all tool schemas
func (r *Registry) GetSchemas() []Schema {
	schemas := make([]Schema, 0, len(r.schemas))
	for _, s := range r.schemas {
		schemas = append(schemas, s)
	}
	return schemas
}

// ReadFile is the Read tool implementation
func ReadFile(args string) (string, error) {
	var argsMap struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &argsMap); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if isBlockedDevicePath(argsMap.Path) {
		return "", fmt.Errorf("cannot read from blocked device path: %s", argsMap.Path)
	}

	content, err := os.ReadFile(argsMap.Path)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	return string(content), nil
}

var blockedDevicePaths = map[string]bool{
	"/dev/zero":     true,
	"/dev/random":   true,
	"/dev/urandom":  true,
	"/dev/full":     true,
	"/dev/stdin":    true,
	"/dev/tty":      true,
	"/dev/console":  true,
	"/dev/stdout":   true,
	"/dev/stderr":   true,
	"/dev/fd/0":     true,
	"/dev/fd/1":     true,
	"/dev/fd/2":     true,
}

func isBlockedDevicePath(filePath string) bool {
	if blockedDevicePaths[filePath] {
		return true
	}
	// /proc/self/fd/0-2 and /proc/<pid>/fd/0-2 are Linux aliases for stdio
	if len(filePath) >= 11 && filePath[:6] == "/proc/" {
		// Check endsWith for /fd/0, /fd/1, /fd/2
		if len(filePath) >= 10 && (filePath[len(filePath)-5:] == "/fd/0" || filePath[len(filePath)-5:] == "/fd/1" || filePath[len(filePath)-5:] == "/fd/2") {
			return true
		}
	}
	return false
}

// WriteFile is the Write tool implementation
func WriteFile(args string) (string, error) {
	var argsMap struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &argsMap); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if err := os.WriteFile(argsMap.Path, []byte(argsMap.Content), 0644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return fmt.Sprintf("Successfully wrote to %s (%d bytes)", argsMap.Path, len(argsMap.Content)), nil
}
