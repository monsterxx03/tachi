package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/monsterxx03/tachi/agent/tools/hashline"
	"github.com/monsterxx03/tachi/agent/wdctx"
)

const (
	leftSingleCurlyQuote  = '\u2018' // '
	rightSingleCurlyQuote = '\u2019' // '
	leftDoubleCurlyQuote  = '\u201C' // "
	rightDoubleCurlyQuote = '\u201D' // "
)

// EditTool performs exact string replacements in files.
type EditTool struct {
	hashlineMode  bool                     // true = hashline mode (parse input, use patcher)
	snapshotStore *hashline.SnapshotStore  // hashline snapshot store (nil when hashline disabled)
	fuzzyThreshold float64                 // fuzzy matching threshold for context line validation (0.0-1.0)
}

// NewEditTool creates a default (replace-mode) EditTool.
func NewEditTool() *EditTool {
	return &EditTool{}
}

// SetHashlineMode enables/disables hashline editing mode.
// store is the snapshot store used for tag verification.
// threshold is the fuzzy matching tolerance for context line validation (0.0-1.0), default 0.95.
func (t *EditTool) SetHashlineMode(enabled bool, store *hashline.SnapshotStore, threshold float64) {
	t.hashlineMode = enabled
	t.snapshotStore = store
	t.fuzzyThreshold = threshold
}

func (t *EditTool) Name() string { return ToolNameEdit }
func (t *EditTool) Description() string {
	if t.hashlineMode {
		return "Edit files using the hashline protocol. Each section starts with ¶PATH#TAG (from ReadFile output), followed by operation lines.\n" +
			"\n" +
			"<headers>\n" +
			"Every file section starts with `¶PATH#TAG`. `TAG` is the 4-hex snapshot tag from your latest ReadFile output, and is REQUIRED on every section — there is no hashless form. To create a new file, use the WriteFile tool; hashline only edits files that already exist.\n" +
			"</headers>\n" +
			"\n" +
			"<ops>\n" +
			"replace N..M:      replace original lines N..M with the body rows below.\n" +
			"delete N..M        delete original lines N..M. No body.\n" +
			"insert before N:   insert the body rows immediately before line N.\n" +
			"insert after N:    insert the body rows immediately after line N.\n" +
			"insert head:       insert the body rows at the very start of the file.\n" +
			"insert tail:       insert the body rows at the very end of the file.\n" +
			"Single line: `replace N..N:` / `delete N`. The range is the ORIGINAL lines you touch; body length is irrelevant (replacing 1 line with 10 is still `replace N..N:`).\n" +
			"</ops>\n" +
			"\n" +
			"<body-rows>\n" +
			"Body rows appear only under a `:` header. Every body row is:\n" +
			"  +TEXT     add a new literal line `TEXT`, verbatim (leading whitespace kept). `+` alone adds a blank line.\n" +
			"There is NO other body row kind. NEVER write `-old` or a bare/context line. To keep a line, leave it out of every range. To insert a literal line starting with `-` or `+`, prefix it: `+-text`, `++text`.\n" +
			"</body-rows>\n" +
			"\n" +
			"<rules>\n" +
			"- Line numbers come from ReadFile output (`LINE:TEXT`). Copy the `¶PATH#TAG` header; use the bare LINE numbers.\n" +
			"- Numbers refer to the ORIGINAL file and stay valid for the whole input — they do not shift as operations apply.\n" +
			"- Across calls they do NOT survive: each applied edit mints a fresh `#TAG` and renumbers the file, so the tag and line numbers you just used are dead. Anchor the next edit on the `¶PATH#TAG` and lines from the edit response (or re-read), never on pre-edit numbers.\n" +
			"- Keep every range as tight as the change: a range must cover ONLY lines whose content actually changes. Never widen it to swallow an unchanged signature, brace, or neighboring statement just to rewrite a few lines inside. (A range where every line genuinely changes is correctly long; tightness is about excluding unchanged lines, not about being short.)\n" +
			"- To change lines 2 and 5 while keeping 3-4, issue two hunks (`replace 2..2:` and `replace 5..5:`). Untouched lines are simply absent from every range.\n" +
			"- On a stale-tag rejection — or any result you cannot fully account for — STOP and re-read. Never stack more line-numbered edits onto output you have not re-grounded; that compounds corruption.\n" +
			"- NEVER use this tool to format code — reordering imports, re-indenting, aligning columns, or any mechanical restyling. That is the project formatter's job.\n" +
			"</rules>\n" +
			"\n" +
			"<example>\n" +
			"Original (what ReadFile returns):\n" +
			"¶greet.py#A1B2\n" +
			"1:def greet(name):\n" +
			"2:    msg = \"Hello, \" + name\n" +
			"3:    print(msg)\n" +
			"4:greet(\"world\")\n" +
			"\n" +
			"Insert a guard after line 1:\n" +
			"¶greet.py#A1B2\n" +
			"insert after 1:\n" +
			"+    if not name: name = \"stranger\"\n" +
			"\n" +
			"Replace line 2 with two lines:\n" +
			"¶greet.py#A1B2\n" +
			"replace 2..2:\n" +
			"+    greeting = \"Hi\"\n" +
			"+    msg = f\"{greeting}, {name}\"\n" +
			"\n" +
			"Delete line 3:\n" +
			"¶greet.py#A1B2\n" +
			"delete 3\n" +
			"\n" +
			"Add a header and trailer:\n" +
			"¶greet.py#A1B2\n" +
			"insert head:\n" +
			"+# generated header\n" +
			"insert tail:\n" +
			"+greet(\"everyone\")\n" +
			"</example>\n" +
			"\n" +
			"<anti-patterns>\n" +
			"# WRONG — empty replace to delete. RIGHT: delete 4\n" +
			"replace 4..4:\n" +
			"\n" +
			"# WRONG — range describes post-edit size. RIGHT: replace 1..1: (body length is irrelevant)\n" +
			"replace 1..2:\n" +
			"+def greet(name):\n" +
			"\n" +
			"# WRONG — `-` rows / bare context lines do not exist. The range deletes; the body is only the new content.\n" +
			"replace 3..3:\n" +
			"    msg = \"Hello, \" + name\n" +
			"-   print(msg)\n" +
			"+   return msg\n" +
			"# RIGHT\n" +
			"replace 3..3:\n" +
			"+   return msg\n" +
			"</anti-patterns>"
	}
	return "Performs exact string replacements in files. Specify old_string to find and new_string to replace with. " +
		"Use replace_all to replace all occurrences. To create a new file, use an empty old_string."
}
func (t *EditTool) Properties() map[string]PropertySchema {
	if t.hashlineMode {
		return map[string]PropertySchema{
			"input": {Type: "string", Description: "Hashline patch input. See tool description for syntax, rules, and examples.\n" +
				"Format:\n" +
				"¶path#TAG\n" +
				"replace N..M:\n" +
				"+body line\n" +
				"delete N\n" +
				"insert after N:\n" +
				"+new line" +
				""},
		}
	}
	return map[string]PropertySchema{
		"path":        {Type: "string", Description: "The absolute path to the file to modify"},
		"old_string":  {Type: "string", Description: "The text to replace"},
		"new_string":  {Type: "string", Description: "The text to replace it with"},
		"replace_all": {Type: "boolean", Description: "Replace all occurrences of old_string (default false)"},
	}
}
func (t *EditTool) Required() []string {
	if t.hashlineMode {
		return []string{"input"}
	}
	return []string{"path", "old_string", "new_string"}
}
func (t *EditTool) Parallel() bool          { return false }
func (t *EditTool) NeedsConfirmation() bool { return true }

func (t *EditTool) GetDiff(ctx context.Context, args string) (string, error) {
	if t.hashlineMode {
		return t.getHashlineDiff(ctx, args)
	}
	return t.getLegacyDiff(ctx, args)
}

func (t *EditTool) getLegacyDiff(ctx context.Context, args string) (string, error) {
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

func (t *EditTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	// Auto-detect mode: if input field is present with hashline content, use hashline
	var hasInput struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(args), &hasInput); err == nil && hasInput.Input != "" && t.hashlineMode {
		return t.executeHashline(ctx, hasInput.Input)
	}

	// Fall through to legacy mode
	return t.executeLegacy(ctx, args)
}

func (t *EditTool) executeHashline(ctx context.Context, input string) (string, error) {
	if t.snapshotStore == nil {
		return "", fmt.Errorf("hashline mode requires a snapshot store (read a file first)")
	}

	threshold := t.fuzzyThreshold
	if threshold <= 0 {
		threshold = hashline.SimilarityThresholdDefault
	}

	patcher := hashline.NewPatcherWithThreshold(hashline.OSFileSystem{}, t.snapshotStore, threshold)
	cwd := wdctx.Dir(ctx)

	results, err := patcher.Apply(input, cwd)
	if err != nil {
		return "", fmt.Errorf("hashline edit failed: %w", err)
	}

	var sb strings.Builder
	for _, r := range results {
		summary := fmt.Sprintf("Edited %s: %d changes, %d added, %d removed [snapshot: %s]",
			r.Path, r.Changes, r.Added, r.Removed, r.Tag)
		sb.WriteString(summary)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// getHashlineDiff produces a diff preview for hashline edits.
// Runs a full Prepare to catch content mismatches before the user confirms.
func (t *EditTool) getHashlineDiff(ctx context.Context, args string) (string, error) {
	var req struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(args), &req); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if req.Input == "" {
		return "", fmt.Errorf("input is required for hashline diff")
	}

	cwd := wdctx.Dir(ctx)

	if t.snapshotStore == nil {
		return "", fmt.Errorf("hashline diff requires a snapshot store (read a file first)")
	}

	threshold := t.fuzzyThreshold
	if threshold <= 0 {
		threshold = hashline.SimilarityThresholdDefault
	}

	patcher := hashline.NewPatcherWithThreshold(hashline.OSFileSystem{}, t.snapshotStore, threshold)
	sections, err := hashline.Parse(req.Input, cwd)
	if err != nil {
		return "", fmt.Errorf("parse hashline input for diff: %w", err)
	}

	var sb strings.Builder
	for _, sec := range sections {
		prepared, err := patcher.Prepare(sec)
		if err != nil {
			return "", fmt.Errorf("validate %s: %w", sec.Path, err)
		}

		oldLines := strings.Split(strings.TrimRight(prepared.OldContent, "\n"), "\n")

		fmt.Fprintf(&sb, "--- %s [tag: %s]\n+++ %s\n", sec.Path, sec.Tag, sec.Path)
		fmt.Fprintf(&sb, "@@ -%d +%d @@\n",
			len(oldLines), len(strings.Split(strings.TrimRight(prepared.NewContent, "\n"), "\n")))

		// Show per-operation line-level changes
		for _, op := range sec.Operations {
			switch op.Type {
			case hashline.OpReplace:
				for i := op.Start; i <= op.End && i-1 < len(oldLines); i++ {
					fmt.Fprintf(&sb, "-%s\n", oldLines[i-1])
				}
				for _, line := range op.Body {
					fmt.Fprintf(&sb, "+%s\n", line)
				}
			case hashline.OpDelete:
				for i := op.Start; i <= op.End && i-1 < len(oldLines); i++ {
					fmt.Fprintf(&sb, "-%s\n", oldLines[i-1])
				}
			case hashline.OpInsertBefore:
				for _, line := range op.Body {
					fmt.Fprintf(&sb, "+%s\n", line)
				}
				if op.Start-1 < len(oldLines) {
					fmt.Fprintf(&sb, " %s\n", oldLines[op.Start-1])
				}
			case hashline.OpInsertAfter:
				if op.Start-1 < len(oldLines) {
					fmt.Fprintf(&sb, " %s\n", oldLines[op.Start-1])
				}
				for _, line := range op.Body {
					fmt.Fprintf(&sb, "+%s\n", line)
				}
			case hashline.OpInsertHead:
				for _, line := range op.Body {
					fmt.Fprintf(&sb, "+%s\n", line)
				}
			case hashline.OpInsertTail:
				for _, line := range op.Body {
					fmt.Fprintf(&sb, "+%s\n", line)
				}
			}
		}
	}
	return sb.String(), nil
}

func (t *EditTool) executeLegacy(ctx context.Context, args string) (string, error) {
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
	before, _, ok := strings.Cut(content, substr)
	if !ok {
		return -1
	}
	return strings.Count(before, "\n")
}

// resolveEditPath resolves a file path for the Edit tool, making relative paths
// relative to the context-provided working directory (for worktree isolation).
func resolveEditPath(ctx context.Context, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(wdctx.Dir(ctx), path)
}
