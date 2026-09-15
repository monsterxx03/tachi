package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"
	"github.com/monsterxx03/tachi/agent/acpctx"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

const (
	leftSingleCurlyQuote  = '\u2018' // '
	rightSingleCurlyQuote = '\u2019' // '
	leftDoubleCurlyQuote  = '\u201C' // "
	rightDoubleCurlyQuote = '\u201D' // "
)

// EditTool performs exact string replacements in files.
//
// Edits are never gated behind interactive confirmation (design decision:
// the diff preview is delivered in the result, and channel/TUI flows treat
// edits as always-approved). Parallel() is therefore always true; per-path
// locks serialize edits to the same file while different files proceed
// concurrently.
type EditTool struct {
	acpMode   bool     // true = route writes through ACP writeTextFile
	fileLocks sync.Map // resolved path -> *sync.Mutex
}

// NewEditTool creates an EditTool.
func NewEditTool() *EditTool {
	return &EditTool{}
}

// SetACPMode enables ACP mode. In ACP mode ExecuteContext routes file writes
// through conn.WriteTextFile (Zed shows inline diff); without ACP it writes
// locally. Confirmation is never required either way.
func (t *EditTool) SetACPMode(v bool) { t.acpMode = v }

func (t *EditTool) Name() string { return ToolNameEdit }
func (t *EditTool) Description() string {
	return "Performs exact string replacements in files. Use for incremental edits — " +
		"do not use bash sed or the write tool for small changes. Read the file first so old_string " +
		"matches its current content; a failed match usually means the file changed — re-read before retrying. " +
		"Use replace_all to replace all occurrences. To create a new file, use an empty old_string."
}
func (t *EditTool) IsDestructive() bool { return true }
func (t *EditTool) Properties() map[string]PropertySchema {
	return map[string]PropertySchema{
		"path":        {Type: "string", Description: "The absolute path to the file to modify"},
		"old_string":  {Type: "string", Description: "The text to replace"},
		"new_string":  {Type: "string", Description: "The text to replace it with"},
		"replace_all": {Type: "boolean", Description: "Replace all occurrences of old_string (default false)"},
	}
}
func (t *EditTool) Required() []string      { return []string{"path", "old_string", "new_string"} }
func (t *EditTool) Parallel() bool          { return true }
func (t *EditTool) NeedsConfirmation() bool { return false }

// lockFile serializes edits to the same resolved path, returning an unlock
// func. Different paths proceed concurrently — the read-modify-write cycle
// per path is atomic.
//
// Symlink normalization: EvalSymlinks fails when the leaf doesn't exist yet
// (createNewFile path), so fall back to resolving the nearest existing
// ancestor and re-attaching the basename — two aliases of a to-be-created
// file must share a key. Hard-link aliases of the same inode are NOT
// serialized (known limitation, last-writer-wins).
func (t *EditTool) lockFile(p string) func() {
	real := p
	if r, err := filepath.EvalSymlinks(p); err == nil {
		real = r
	} else if dir, base := filepath.Split(p); dir != "" {
		if r, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
			real = filepath.Join(r, base)
		}
	}
	real = filepath.Clean(real)
	// The lock table grows with the number of distinct paths edited in a
	// session (accepted upper bound: hundreds to low thousands of entries).
	if mu, ok := t.fileLocks.Load(real); ok {
		m := mu.(*sync.Mutex)
		m.Lock()
		return m.Unlock
	}
	mu, _ := t.fileLocks.LoadOrStore(real, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// readFileChecked reads filePath enforcing the size limit; binary files are
// rejected so Edit never mangles non-text content. It also returns the
// file's permission bits so callers can preserve them on write.
func readFileChecked(filePath string) (string, os.FileMode, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return "", 0, fmt.Errorf("failed to stat file: %w", err)
	}
	if info.Size() > maxFileSize {
		return "", 0, ErrFileTooLarge(info.Size(), maxFileSize)
	}

	raw, err := os.ReadFile(filePath)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read file: %w", err)
	}

	if isBinaryFile(raw) {
		return "", 0, fmt.Errorf("cannot edit binary file: %s", filePath)
	}
	return string(raw), info.Mode().Perm(), nil
}

// GetDiff returns a diff preview for confirmation flows. Reserved for
// future confirmation-gated tools: EditTool itself never requires
// confirmation, so this is not called on the production path. Note it reads
// the file without holding the per-path lock — re-enabling confirmation
// must acquire lockFile first.
func (t *EditTool) GetDiff(ctx context.Context, args string) (string, error) {
	return t.getLegacyDiff(ctx, args)
}

// staleFileHint is appended to old_string-not-found errors: the most common
// cause is the model editing from stale context, so guide it to re-read.
const staleFileHint = "The file may have changed since your last read — use the read tool to reload it before retrying."

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

	filePath := ResolvePath(ctx, a.FilePath)

	if a.OldString == "" {
		return fmt.Sprintf("--- new file: %s\n+++ %s\n%s", filePath, filePath, a.NewString), nil
	}

	content, _, err := readFileChecked(filePath)
	if err != nil {
		return "", err
	}

	actualOld := findActualString(content, a.OldString)
	if actualOld == "" {
		return "", notFoundError(filePath, content, a.OldString)
	}

	return generateDiffSnippet(content, actualOld, a.NewString), nil
}

func (t *EditTool) ExecuteContext(ctx context.Context, args string) (string, error) {
	return t.executeLegacy(ctx, args)
}

func (t *EditTool) executeLegacy(ctx context.Context, args string) (string, error) {
	logger.FromContext(ctx).Info(ctx, fmt.Sprintf("ACP edit: executeLegacy called, acpMode=%v conn=%v", t.acpMode, acpctx.Conn(ctx) != nil))
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

	filePath := ResolvePath(ctx, a.FilePath)

	// Serialize edits to the same file (parallel tool calls may target it);
	// different files run concurrently.
	unlock := t.lockFile(filePath)
	defer unlock()

	// In ACP mode, route through ACP client for Zed inline diff + accept/reject.
	if t.acpMode {
		if conn := acpctx.Conn(ctx); conn != nil {
			sessionID := acpctx.SessionID(ctx)
			if a.OldString == "" {
				_, err := conn.WriteTextFile(ctx, acp.WriteTextFileRequest{
					SessionId: sessionID,
					Path:      filePath,
					Content:   a.NewString,
				})
				if err != nil {
					return "", fmt.Errorf("ACP writeTextFile failed: %w", err)
				}
				return fmt.Sprintf("Created new file via ACP %s (%d bytes)", filePath, len(a.NewString)), nil
			}
			resp, err := conn.ReadTextFile(ctx, acp.ReadTextFileRequest{
				SessionId: sessionID,
				Path:      filePath,
			})
			if err != nil {
				return "", fmt.Errorf("ACP readTextFile failed: %w", err)
			}
			actualOld := findActualString(resp.Content, a.OldString)
			if actualOld == "" {
				return "", notFoundError(filePath, resp.Content, a.OldString)
			}
			if !a.ReplaceAll && strings.Count(resp.Content, actualOld) > 1 {
				return "", fmt.Errorf("old_string matches multiple locations in %s (%s)", filePath, matchLines(resp.Content, actualOld))
			}
			var newContent string
			if a.ReplaceAll {
				newContent = strings.ReplaceAll(resp.Content, actualOld, a.NewString)
			} else {
				newContent = strings.Replace(resp.Content, actualOld, a.NewString, 1)
			}
			_, err = conn.WriteTextFile(ctx, acp.WriteTextFileRequest{
				SessionId: sessionID,
				Path:      filePath,
				Content:   newContent,
			})
			if err != nil {
				return "", fmt.Errorf("ACP writeTextFile failed: %w", err)
			}
			snippet := generateDiffSnippet(resp.Content, actualOld, a.NewString)
			return fmt.Sprintf("Successfully edited via ACP %s\n%s", filePath, snippet), nil
		}
	}

	if a.OldString == "" {
		return createNewFile(ctx, filePath, a.NewString)
	}

	return editExistingFile(ctx, filePath, a.OldString, a.NewString, a.ReplaceAll)
}

func createNewFile(ctx context.Context, filePath, content string) (string, error) {
	// Enforce path policy (used by worktree sandbox).
	if policy := GetPathPolicy(ctx); policy != nil {
		absPath, _ := filepath.Abs(filePath)
		if err := policy.CheckPath(absPath); err != nil {
			return "", err
		}
	}

	if fileutil.Exists(filePath) {
		return "", fmt.Errorf("file already exists: %s (use a non-empty old_string to edit it)", filePath)
	}

	if err := fileutil.AtomicWriteFileShared(filePath, []byte(content)); err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	return fmt.Sprintf("Created new file %s (%d bytes)", filePath, len(content)), nil
}

func editExistingFile(ctx context.Context, filePath, oldString, newString string, replaceAll bool) (string, error) {
	// Enforce path policy (used by worktree sandbox).
	if policy := GetPathPolicy(ctx); policy != nil {
		absPath, _ := filepath.Abs(filePath)
		if err := policy.CheckPath(absPath); err != nil {
			return "", err
		}
	}

	content, perm, err := readFileChecked(filePath)
	if err != nil {
		return "", err
	}

	actualOld := findActualString(content, oldString)
	if actualOld == "" {
		return "", notFoundError(filePath, content, oldString)
	}

	if !replaceAll {
		count := strings.Count(content, actualOld)
		if count > 1 {
			return "", fmt.Errorf("old_string matches %d locations in %s (%s). Provide a larger unique substring or set replace_all to true",
				count, filePath, matchLines(content, actualOld))
		}
	}

	var newContent string
	if replaceAll {
		newContent = strings.ReplaceAll(content, actualOld, newString)
	} else {
		newContent = strings.Replace(content, actualOld, newString, 1)
	}

	// Atomic write: parallel ReadFile calls in the same turn must never
	// observe a half-written file (they may see old content — fine).
	if err := fileutil.AtomicWriteFile(filePath, []byte(newContent), 0o755, perm); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	snippet := generateDiffSnippet(content, actualOld, newString)
	msg := ""
	if actualOld != oldString {
		// Tolerant match (quote normalization or trailing-whitespace fallback) hit a slightly
		// different range than the model wrote. The note goes FIRST, with the line it landed
		// on: a caveat at the tail of a diff is easy to skim past, and "did this hit the range
		// I meant" is the one thing to check here.
		msg += fmt.Sprintf("Note: old_string matched as %q (tolerant match, line %d) — verify the diff hit the intended range.\n",
			strutil.Truncate(actualOld, 200), findLineIndex(content, actualOld)+1)
	}
	return msg + fmt.Sprintf("Successfully edited %s\n%s", filePath, snippet), nil
}

// notFoundError reports a failed anchor with a diagnosis of the closest region in the file.
// A stale or approximate old_string is by far the most common failure this tool sees, and the
// difference is often one leading space or one re-wrapped line — telling the caller only to
// "reload the file" costs it a full read to find that out.
func notFoundError(filePath, content, oldString string) error {
	report := closestRegion(content, oldString)
	if report == "" {
		return fmt.Errorf("old_string not found in %s. Make sure it matches the file content exactly, including whitespace and indentation. %s", filePath, staleFileHint)
	}
	return fmt.Errorf("old_string not found in %s. Make sure it matches the file content exactly, including whitespace and indentation. %s\n%s",
		filePath, staleFileHint, report)
}

// closestRegion describes where the file comes closest to containing oldString: the window
// with the same line count scoring highest on line equality ignoring leading/trailing
// whitespace, that score, and the first line that differs (expected vs found, quoted so the
// whitespace is visible). Empty when no window shares even one line.
func closestRegion(content, oldString string) string {
	fileLines := strings.Split(content, "\n")
	want := strings.Split(strings.TrimRight(oldString, " \t\r\n"), "\n")
	if len(want) == 0 || len(want) > len(fileLines) {
		return ""
	}
	// Trim once per line: the scan below compares windows, so the same file line is looked at
	// many times and re-trimming it each time would be the only real cost here.
	trimmed := make([]string, len(fileLines))
	for i, l := range fileLines {
		trimmed[i] = strings.TrimSpace(l)
	}
	wantTrimmed := make([]string, len(want))
	for i, l := range want {
		wantTrimmed[i] = strings.TrimSpace(l)
	}

	bestAt, bestScore := -1, 0
	for i := 0; i+len(want) <= len(fileLines); i++ {
		score := 0
		for j := range wantTrimmed {
			if trimmed[i+j] == wantTrimmed[j] {
				score++
			}
		}
		if score > bestScore {
			bestAt, bestScore = i, score
		}
	}
	if bestAt < 0 {
		// No window shares even one line. In prose that usually means LINE WRAPPING: the
		// caller re-flowed the paragraph while the file kept its own line breaks, so no line
		// boundary lines up. A long literal prefix of the first line is what tells that apart
		// from "this text is nowhere in the file".
		return prefixProbe(content, want[0])
	}

	var b strings.Builder
	fmt.Fprintf(&b, "closest region: lines %d-%d, %d of %d lines equal ignoring leading/trailing whitespace",
		bestAt+1, bestAt+len(want), bestScore, len(want))

	// Which line differs, and how, is the whole point of the report. Two shapes matter: the
	// text differs (an edited or re-wrapped line), or only the whitespace does (the classic
	// "one extra leading space" miss) — and the second one still needs both forms printed, or
	// the caller cannot see WHICH way its whitespace is off.
	diff := -1
	for j := range wantTrimmed {
		if trimmed[bestAt+j] != wantTrimmed[j] {
			diff = j
			break
		}
	}
	if diff < 0 {
		for j := range want {
			if fileLines[bestAt+j] != want[j] {
				diff = j
				break
			}
		}
		fmt.Fprintf(&b, "\n  every line of old_string IS in that region — the difference is WHITESPACE only (indentation, or spaces at the end of a line).")
	}
	if diff >= 0 {
		fmt.Fprintf(&b, "\n  first difference at line %d:\n    expected: %q\n    found:    %q",
			bestAt+diff+1, strutil.Truncate(want[diff], 120), strutil.Truncate(fileLines[bestAt+diff], 120))
	}
	return b.String()
}

// prefixProbe reports where the longest shared prefix of a wanted line sits in the file, so a
// re-wrapped anchor reads as "right place, different line breaks" instead of "not found".
// The prefix is shortened in steps because the first line is usually where the wrapping starts
// to differ; below minProbeRunes a match is noise (a short string is everywhere).
func prefixProbe(content, wantLine string) string {
	const minProbeRunes = 24
	runes := []rune(strings.TrimSpace(wantLine))
	for n := len(runes); n >= minProbeRunes; n -= 4 {
		probe := string(runes[:n])
		idx := strings.Index(content, probe)
		if idx < 0 {
			continue
		}
		line := strings.Count(content[:idx], "\n") + 1
		found := content[idx:]
		if nl := strings.IndexByte(found, '\n'); nl >= 0 {
			found = found[:nl]
		}
		return fmt.Sprintf("  the first %d characters of old_string ARE in the file, at line %d — the difference starts right after them. In a wrapped document this is LINE BREAKS: your lines and the file's do not break at the same place.\n    file's line %d: %q",
			n, line, line, strutil.Truncate(found, 160))
	}
	return ""
}

// matchLines names where an ambiguous anchor hits (up to maxMatches of them, then the count in
// the message takes over), so a caller can lengthen the anchor instead of guessing which of
// the occurrences the tool meant.
func matchLines(content, sub string) string {
	const maxMatches = 5
	var lines []string
	off := 0
	for len(lines) < maxMatches {
		i := strings.Index(content[off:], sub)
		if i < 0 {
			break
		}
		lines = append(lines, strconv.Itoa(strings.Count(content[:off+i], "\n")+1))
		off += i + len(sub)
	}
	if len(lines) == 0 {
		return "locations unknown"
	}
	return "lines " + strings.Join(lines, ", ")
}

// findActualString finds the matching string in fileContent, with fallbacks:
//  1. exact match (after curly-quote normalization)
//  2. trailing-whitespace-insensitive match — the model often drops trailing
//     spaces/newlines when copying old_string from a read
//
// Returns the actual substring from fileContent so replacements preserve the
// file's real bytes.
func findActualString(fileContent, searchString string) string {
	if actual, ok := findNormalized(fileContent, searchString); ok {
		return actual
	}

	// Trailing-whitespace fallback: strip trailing whitespace from the search
	// string and retry. The replaced range excludes the whitespace, which
	// stays untouched in the file. An all-whitespace search string would
	// degenerate to an empty match — reject it explicitly.
	trimmed := strings.TrimRight(searchString, " \t\r\n")
	if trimmed != "" && trimmed != searchString {
		if actual, ok := findNormalized(fileContent, trimmed); ok {
			return actual
		}
	}
	return ""
}

// findNormalized matches search inside the curly-quote-normalized content and
// maps the match back to the original content's byte range.
func findNormalized(fileContent, search string) (string, bool) {
	normalizedSearch := normalizeQuotes(search)
	normalizedFile := normalizeQuotes(fileContent)

	normIdx := strings.Index(normalizedFile, normalizedSearch)
	if normIdx == -1 {
		return "", false
	}

	// Map byte offsets in normalizedFile back to the original fileContent.
	// Walk both strings rune-by-rune in a single pass to find both boundaries.
	origIdx, origEnd := mapNormalizedRange(fileContent, normalizedFile, normIdx, len(normalizedSearch))
	return fileContent[origIdx:origEnd], true
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
func generateDiffSnippet(oldContent, oldStr, newStr string) string {
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
