package tools

import (
	"context"
	"path/filepath"
	"strings"
)

// PathPolicy restricts WriteFile to only write within allowed directories.
// When set in context, WriteFile.ExecuteContext checks the resolved absolute
// path against AllowedWriteDirs before proceeding.
type PathPolicy struct {
	AllowedWriteDirs []string // directories where writes are permitted
}

type pathPolicyKey struct{}

// WithPathPolicy attaches a PathPolicy to the context.
func WithPathPolicy(ctx context.Context, policy *PathPolicy) context.Context {
	return context.WithValue(ctx, pathPolicyKey{}, policy)
}

// GetPathPolicy retrieves the PathPolicy from context, or nil if none set.
func GetPathPolicy(ctx context.Context) *PathPolicy {
	if v, ok := ctx.Value(pathPolicyKey{}).(*PathPolicy); ok {
		return v
	}
	return nil
}

// CheckPath validates that absPath is within one of the allowed directories.
// Returns nil if allowed, or an error describing the violation.
func (p *PathPolicy) CheckPath(absPath string) error {
	if p == nil {
		return nil
	}
	for _, dir := range p.AllowedWriteDirs {
		// Normalize both paths for comparison.
		cleanDir := filepath.Clean(dir) + string(filepath.Separator)
		cleanPath := filepath.Clean(absPath)
		if strings.HasPrefix(cleanPath, cleanDir) || cleanPath == filepath.Clean(dir) {
			return nil
		}
	}
	return &PathPolicyError{Path: absPath, AllowedDirs: p.AllowedWriteDirs}
}

// PathPolicyError is returned when a write is denied by PathPolicy.
type PathPolicyError struct {
	Path        string
	AllowedDirs []string
}

func (e *PathPolicyError) Error() string {
	return "write denied by path policy: " + e.Path + " is outside allowed directories"
}
