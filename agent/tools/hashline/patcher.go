package hashline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileSystem defines the file I/O interface used by the Patcher.
type FileSystem interface {
	Read(path string) (string, error)
	Write(path string, content string) error
	Exists(path string) (bool, error)
}

// OSFileSystem is the real FileSystem implementation.
type OSFileSystem struct{}

func (OSFileSystem) Read(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return normalizeLineEndings(string(data)), nil
}

func (OSFileSystem) Write(path string, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func (OSFileSystem) Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// PreparedSection holds the result of the Prepare phase.
type PreparedSection struct {
	Path       string
	NewContent string
	OldContent string
	Changes    int // number of lines changed
	Added      int // number of lines added
	Removed    int // number of lines removed
}

// SectionResult reports the outcome of a committed edit.
type SectionResult struct {
	Path    string
	Tag     string
	Changes int
	Added   int
	Removed int
}

// Patcher applies hashline edits in a two-phase prepare/commit workflow.
// Patcher applies hashline edits in a two-phase prepare/commit workflow.
type Patcher struct {
	fs            FileSystem
	snapshots     *SnapshotStore
	FuzzyThreshold float64 // 0.0-1.0, line content fuzzy matching tolerance
}

// NewPatcher creates a new Patcher with the given file system and snapshot store.
// Uses SimilarityThresholdDefault (0.95) as the default fuzzy threshold.
func NewPatcher(fs FileSystem, snapshots *SnapshotStore) *Patcher {
	return &Patcher{
		fs:             fs,
		snapshots:      snapshots,
		FuzzyThreshold: SimilarityThresholdDefault,
	}
}

// NewPatcherWithThreshold creates a new Patcher with a custom fuzzy threshold.
func NewPatcherWithThreshold(fs FileSystem, snapshots *SnapshotStore, threshold float64) *Patcher {
	return &Patcher{
		fs:             fs,
		snapshots:      snapshots,
		FuzzyThreshold: threshold,
	}
}

// Prepare validates a section and computes the resulting file content
// without writing to disk. Returns a PreparedSection ready for Commit.
func (p *Patcher) Prepare(section Section) (*PreparedSection, error) {
	// Read current content
	content, err := p.fs.Read(section.Path)
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", section.Path, err)
	}

	// Verify hash tag
	if err := p.snapshots.Verify(section.Path, section.Tag, content); err != nil {
		return nil, err
	}

	lines := strings.Split(content, "\n")
	hasTrailingNewline := strings.HasSuffix(content, "\n")

	// Trim trailing empty element from split
	if hasTrailingNewline && len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	// Validate operations against file
	if err := validateOperations(section.Operations, len(lines)); err != nil {
		return nil, fmt.Errorf("%s: %w", section.Path, err)
	}

	// Validate context lines (optional) with fuzzy matching
	if err := p.validateContextLines(lines, section.ContextLines); err != nil {
		return nil, fmt.Errorf("%s: %w", section.Path, err)
	}

	// Apply all operations to build new content
	newLines, stats := applyOperations(section.Operations, lines)

	// Reconstruct with trailing newline
	newContent := strings.Join(newLines, "\n")
	if hasTrailingNewline {
		newContent += "\n"
	}

	return &PreparedSection{
		Path:       section.Path,
		NewContent: newContent,
		OldContent: content,
		Changes:    stats.changes,
		Added:      stats.added,
		Removed:    stats.removed,
	}, nil
}

// validateContextLines checks optional numbered context lines against actual file content
// using fuzzy matching with the Patcher's FuzzyThreshold.
// Returns nil if contextLines is empty (optional feature).
func (p *Patcher) validateContextLines(actualLines []string, contextLines map[int]string) error {
	if len(contextLines) == 0 {
		return nil // No context lines to validate — purely optional
	}

	// Always check context lines even if FuzzyThreshold is 0 (meaning exact-only mode)
	threshold := p.FuzzyThreshold
	if threshold <= 0 {
		threshold = 1.0 // No tolerance: exact match only
	}

	for lineNum, expectedContent := range contextLines {
		// 1-indexed to 0-indexed
		idx := lineNum - 1
		if idx < 0 || idx >= len(actualLines) {
			return ErrContextLineOutOfRange(lineNum, len(actualLines))
		}

		actualContent := actualLines[idx]
		if !fuzzyLineMatch(actualContent, expectedContent, threshold) {
			return ErrContextLineMismatch(lineNum, expectedContent, actualContent, threshold)
		}
	}

	return nil
}


// Commit writes a PreparedSection to disk and records the new snapshot.
func (p *Patcher) Commit(prepared *PreparedSection) (*SectionResult, error) {
	if err := p.fs.Write(prepared.Path, prepared.NewContent); err != nil {
		return nil, fmt.Errorf("write file %s: %w", prepared.Path, err)
	}

	tag := p.snapshots.Record(prepared.Path, prepared.NewContent)

	return &SectionResult{
		Path:    prepared.Path,
		Tag:     tag,
		Changes: prepared.Changes,
		Added:   prepared.Added,
		Removed: prepared.Removed,
	}, nil
}

// Apply parses hashline input, validates all sections, then commits them all.
func (p *Patcher) Apply(input, cwd string) ([]SectionResult, error) {
	sections, err := Parse(input, cwd)
	if err != nil {
		return nil, fmt.Errorf("parse input: %w", err)
	}

	prepared := make([]*PreparedSection, 0, len(sections))
	for _, section := range sections {
		ps, err := p.Prepare(section)
		if err != nil {
			return nil, fmt.Errorf("prepare %s: %w", section.Path, err)
		}
		prepared = append(prepared, ps)
	}

	results := make([]SectionResult, 0, len(prepared))
	for _, ps := range prepared {
		result, err := p.Commit(ps)
		if err != nil {
			return nil, fmt.Errorf("commit %s: %w", ps.Path, err)
		}
		results = append(results, *result)
	}

	return results, nil
}

// applyStats tracks the impact of operations.
type applyStats struct {
	changes int
	added   int
	removed int
}

// applyOperations applies all operations to the original lines and returns the result.
// All operations reference the original line numbers (1-indexed).
func applyOperations(ops []Operation, lines []string) ([]string, applyStats) {
	// Build operation maps keyed by original line number
	type lineOp struct {
		deleted      bool
		replacement  []string  // non-nil if replacing
		insertBefore []string
		insertAfter  []string
	}
	lineOps := make(map[int]*lineOp)

	var headLines, tailLines []string

	for _, op := range ops {
		switch op.Type {
		case OpDelete:
			for i := op.Start; i <= op.End; i++ {
				if lineOps[i] == nil {
					lineOps[i] = &lineOp{}
				}
				lineOps[i].deleted = true
			}
		case OpReplace:
			for i := op.Start; i <= op.End; i++ {
				if lineOps[i] == nil {
					lineOps[i] = &lineOp{}
				}
				lineOps[i].deleted = true
			}
			if lineOps[op.Start] == nil {
				lineOps[op.Start] = &lineOp{}
			}
			lineOps[op.Start].replacement = op.Body
		case OpInsertBefore:
			if lineOps[op.Start] == nil {
				lineOps[op.Start] = &lineOp{}
			}
			lineOps[op.Start].insertBefore = op.Body
		case OpInsertAfter:
			if lineOps[op.Start] == nil {
				lineOps[op.Start] = &lineOp{}
			}
			lineOps[op.Start].insertAfter = op.Body
		case OpInsertHead:
			headLines = op.Body
		case OpInsertTail:
			tailLines = op.Body
		}
	}

	// Build result by iterating through original lines
	var result []string
	result = append(result, headLines...)

	for i, line := range lines {
		lineNum := i + 1
		lo := lineOps[lineNum]

		// Insert before
		if lo != nil && lo.insertBefore != nil {
			result = append(result, lo.insertBefore...)
		}

		if lo != nil && lo.deleted {
			// Deleted line — don't output the original, but may have replacement
			if lo.replacement != nil {
				result = append(result, lo.replacement...)
			}
			continue
		}

		result = append(result, line)

		// Insert after
		if lo != nil && lo.insertAfter != nil {
			result = append(result, lo.insertAfter...)
		}
	}

	result = append(result, tailLines...)

	// Compute stats
	stats := applyStats{}
	for _, op := range ops {
		switch op.Type {
		case OpReplace:
			removed := op.End - op.Start + 1
			added := len(op.Body)
			stats.changes++
			stats.removed += removed
			stats.added += added
		case OpDelete:
			removed := op.End - op.Start + 1
			stats.removed += removed
		case OpInsertBefore, OpInsertAfter, OpInsertHead, OpInsertTail:
			stats.added += len(op.Body)
		}
	}

	return result, stats
}

// validateOperations checks that all operation line numbers are valid.
func validateOperations(ops []Operation, totalLines int) error {
	for _, op := range ops {
		switch op.Type {
		case OpReplace, OpDelete:
			if op.Start < 1 || op.Start > totalLines {
				return ErrLineOutOfRange(op.Start, totalLines)
			}
			if op.End < op.Start || op.End > totalLines {
				return ErrLineOutOfRange(op.End, totalLines)
			}
		case OpInsertBefore, OpInsertAfter:
			if op.Start < 1 || op.Start > totalLines {
				return ErrLineOutOfRange(op.Start, totalLines)
			}
		}
	}
	return nil
}
