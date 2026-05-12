package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/wdctx"
)

const (
	leftSingleCurlyQuote  = '\u2018' // '
	rightSingleCurlyQuote = '\u2019' // '
	leftDoubleCurlyQuote  = '\u201C' // "
	rightDoubleCurlyQuote = '\u201D' // "
)

// EditTool performs exact string replacements in files
type EditTool struct{}

func (t EditTool) Name() string { return ToolNameEdit }
func (t EditTool) Description() string {
	return "You must use your ReadFile tool at least once in the conversation before editing. " +
		"This tool will error if you attempt an edit without reading the file. " +
		"Performs exact string replacements in files. Specify old_string to find and new_string to replace with. " +
		"Use replace_all to replace all occurrences. To create a new file, use an empty old_string."
}
func (t EditTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"path":   {Type: "string", Description: "The absolute path to the file to modify"},
		"old_string":  {Type: "string", Description: "The text to replace"},
		"new_string":  {Type: "string", Description: "The text to replace it with"},
		"replace_all": {Type: "boolean", Description: "Replace all occurrences of old_string (default false)"},
	}
}
func (t EditTool) Required() []string      { return []string{"path", "old_string", "new_string"} }
func (t EditTool) Parallel() bool          { return false }
func (t EditTool) NeedsConfirmation() bool { return true }

func (t EditTool) GetDiff(ctx context.Context, args string) (string, error) {
	var a struct {
		FilePath   string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	filePath := resolveEditPath(ctx, a.FilePath)

	if a.OldString == "" {
		// Creating new file - no diff needed, just show the content
		return fmt.Sprintf("--- new file: %s\n+++ %s\n%s", filePath, filePath, a.NewString), nil
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}
	if info.Size() > maxFileSize {
		return "", ErrFileTooLarge(info.Size(), maxFileSize)
	}

	raw, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	if isBinaryFile(raw) {
		return "", fmt.Errorf("cannot edit binary file: %s", filePath)
	}

	content := string(raw)

	actualOld := findActualString(content, a.OldString)
	if actualOld == "" {
		return "", fmt.Errorf("old_string not found in %s", filePath)
	}

	var newContent string
	if a.ReplaceAll {
		newContent = strings.ReplaceAll(content, actualOld, a.NewString)
	} else {
		newContent = strings.Replace(content, actualOld, a.NewString, 1)
	}

	return generateDiffSnippet(content, newContent, actualOld, a.NewString), nil
}

func (t EditTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	var a struct {
		FilePath   string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if a.OldString == a.NewString {
		return "", fmt.Errorf("old_string and new_string are identical, no edit needed")
	}

	filePath := resolveEditPath(ctx, a.FilePath)

	if a.OldString == "" {
		return createNewFile(filePath, a.NewString)
	}

	return editExistingFile(filePath, a.OldString, a.NewString, a.ReplaceAll)
}

func createNewFile(filePath, content string) (string, error) {
	if _, err := os.Stat(filePath); err == nil {
		return "", fmt.Errorf("file already exists: %s (use a non-empty old_string to edit it)", filePath)
	}

	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	return fmt.Sprintf("Created new file %s (%d bytes)", filePath, len(content)), nil
}

func editExistingFile(filePath, oldString, newString string, replaceAll bool) (string, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}
	if info.Size() > maxFileSize {
		return "", ErrFileTooLarge(info.Size(), maxFileSize)
	}

	raw, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	if isBinaryFile(raw) {
		return "", fmt.Errorf("cannot edit binary file: %s", filePath)
	}

	content := string(raw)

	actualOld := findActualString(content, oldString)
	if actualOld == "" {
		return "", fmt.Errorf("old_string not found in %s. Make sure it matches the file content exactly, including whitespace and indentation", filePath)
	}

	if !replaceAll {
		count := strings.Count(content, actualOld)
		if count > 1 {
			return "", fmt.Errorf("old_string matches %d locations in %s. Provide a larger unique substring or set replace_all to true", count, filePath)
		}
	}

	var newContent string
	if replaceAll {
		newContent = strings.ReplaceAll(content, actualOld, newString)
	} else {
		newContent = strings.Replace(content, actualOld, newString, 1)
	}

	if err := os.WriteFile(filePath, []byte(newContent), info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	snippet := generateDiffSnippet(content, newContent, actualOld, newString)
	return fmt.Sprintf("Successfully edited %s\n%s", filePath, snippet), nil
}

// findActualString finds the matching string in fileContent, with curly quote normalization fallback.
func findActualString(fileContent, searchString string) string {
	if strings.Contains(fileContent, searchString) {
		return searchString
	}

	normalizedSearch := normalizeQuotes(searchString)
	normalizedFile := normalizeQuotes(fileContent)

	normIdx := strings.Index(normalizedFile, normalizedSearch)
	if normIdx == -1 {
		return ""
	}

	// Map byte offsets in normalizedFile back to the original fileContent.
	// Walk both strings rune-by-rune in a single pass to find both boundaries.
	origIdx, origEnd := mapNormalizedRange(fileContent, normalizedFile, normIdx, len(normalizedSearch))
	return fileContent[origIdx:origEnd]
}

// mapNormalizedRange converts a byte range [normStart, normStart+normLen) in the
// normalized string to the corresponding byte range in the original string by
// walking both strings rune-by-rune in a single pass.
func mapNormalizedRange(original, normalized string, normStart, normLen int) (int, int) {
	normPos := 0
	origPos := 0
	normEnd := normStart + normLen

	// Walk to normStart
	for normPos < normStart && origPos < len(original) {
		_, origRuneSize := utf8.DecodeRuneInString(original[origPos:])
		_, normRuneSize := utf8.DecodeRuneInString(normalized[normPos:])
		origPos += origRuneSize
		normPos += normRuneSize
	}
	origStart := origPos

	// Continue walking to normEnd
	for normPos < normEnd && origPos < len(original) {
		_, origRuneSize := utf8.DecodeRuneInString(original[origPos:])
		_, normRuneSize := utf8.DecodeRuneInString(normalized[normPos:])
		origPos += origRuneSize
		normPos += normRuneSize
	}
	return origStart, origPos
}

// curlyQuoteReplacer normalizes curly (smart) quotes to their straight ASCII equivalents.
// Allocated once at package level since the replacement pairs are constant.
var curlyQuoteReplacer = strings.NewReplacer(
	string(leftSingleCurlyQuote), "'",
	string(rightSingleCurlyQuote), "'",
	string(leftDoubleCurlyQuote), `"`,
	string(rightDoubleCurlyQuote), `"`,
)

// normalizeQuotes converts curly quotes to straight quotes.
func normalizeQuotes(s string) string {
	return curlyQuoteReplacer.Replace(s)
}

// generateDiffSnippet produces a unified diff with +/- markers for changes
func generateDiffSnippet(oldContent, newContent, oldStr, newStr string) string {
	const contextLines = 3

	oldLines := strings.Split(oldContent, "\n")

	editStart := max(findLineIndex(oldContent, oldStr), 0)
	snippetStart := max(editStart-contextLines, 0)

	oldStrLines := strings.Count(oldStr, "\n") + 1
	newStrLines := strings.Count(newStr, "\n") + 1

	beforeEnd := min(editStart+oldStrLines+contextLines, len(oldLines))

	// Count how many lines actually changed
	changedLines := strings.Split(oldStr, "\n")
	newChangedLines := strings.Split(newStr, "\n")

	var b strings.Builder
	// Unified diff header
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", editStart+1, oldStrLines, editStart+1, newStrLines)

	// Show context before
	for i := snippetStart; i < editStart; i++ {
		fmt.Fprintf(&b, " %d | %s\n", i+1, oldLines[i])
	}

	// Show old lines (deleted) - prefix with -
	for i, line := range changedLines {
		lineNum := editStart + i + 1
		fmt.Fprintf(&b, "-%d | %s\n", lineNum, line)
	}

	// Show new lines (added) - prefix with +
	for i, line := range newChangedLines {
		lineNum := editStart + i + 1
		fmt.Fprintf(&b, "+%d | %s\n", lineNum, line)
	}

	// Show context after
	for i := editStart + oldStrLines; i < beforeEnd; i++ {
		fmt.Fprintf(&b, " %d | %s\n", i+1, oldLines[i])
	}

	return b.String()
}

// findLineIndex returns the 0-indexed line number where substr first appears.
func findLineIndex(content, substr string) int {
	idx := strings.Index(content, substr)
	if idx < 0 {
		return -1
	}
	return strings.Count(content[:idx], "\n")
}

// resolveEditPath resolves a file path for the Edit tool, making relative paths
// relative to the context-provided working directory (for worktree isolation).
func resolveEditPath(ctx context.Context, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(wdctx.Dir(ctx), path)
}
