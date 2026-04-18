package tools

import (
	"encoding/json"
	"fmt"
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
