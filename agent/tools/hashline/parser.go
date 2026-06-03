package hashline

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Operation command patterns.
var (
	replaceRe      = regexp.MustCompile(`^replace\s+(\d+)\.\.(\d+):\s*$`)
	deleteRe       = regexp.MustCompile(`^delete\s+(\d+)(?:\.\.(\d+))?\s*$`)
	insertBeforeRe = regexp.MustCompile(`^insert\s+before\s+(\d+):\s*$`)
	insertAfterRe  = regexp.MustCompile(`^insert\s+after\s+(\d+):\s*$`)
	insertHeadRe   = regexp.MustCompile(`^insert\s+head:\s*$`)
	insertTailRe   = regexp.MustCompile(`^insert\s+tail:\s*$`)
	bodyLineRe     = regexp.MustCompile(`^\+(\s*.*)$`)
	contextLineRe  = regexp.MustCompile(`^(\d+):(.*)$`)
)

var headerRe = regexp.MustCompile(`^¶(.+)#([0-9a-f]{4,})\s*$`)

// Parse parses hashline operation-format text into a list of sections.
// cwd is used to resolve relative display paths to absolute paths.
func Parse(input string, cwd string) ([]Section, error) {
	if strings.TrimSpace(input) == "" {
		return nil, fmt.Errorf("%s", ErrEmptyInput)
	}

	rawSections := splitSections(normalizeLineEndings(input))
	if len(rawSections) == 0 {
		return nil, fmt.Errorf("%s", ErrEmptyInput)
	}

	sections := make([]Section, 0, len(rawSections))
	for i, block := range rawSections {
		section, err := parseSection(block, cwd)
		if err != nil {
			return nil, fmt.Errorf("section %d: %w", i+1, err)
		}
		sections = append(sections, section)
	}

	return sections, nil
}

// splitSections splits input into sections separated by blank lines.
func splitSections(input string) []string {
	lines := strings.Split(input, "\n")
	var sections []string
	var current []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" && len(current) > 0 {
			sections = append(sections, strings.Join(current, "\n"))
			current = nil
			continue
		}
		if trimmed != "" || len(current) > 0 {
			current = append(current, line)
		}
	}

	if len(current) > 0 {
		sections = append(sections, strings.Join(current, "\n"))
	}

	return sections
}

// parseSection parses a single section block.
// First line must be ¶path#tag, followed by optional numbered context lines
// and operation commands with body rows.
func parseSection(block, cwd string) (Section, error) {
	lines := strings.Split(block, "\n")
	if len(lines) == 0 {
		return Section{}, fmt.Errorf("empty section")
	}

	// First line must be the header
	headerMatch := headerRe.FindStringSubmatch(strings.TrimRight(lines[0], "\r"))
	if headerMatch == nil {
		return Section{}, ErrInvalidHeader(lines[0])
	}

	displayPath := headerMatch[1]
	tag := headerMatch[2]

	// Resolve display path to absolute path
	absPath := displayPath
	if !filepath.IsAbs(displayPath) {
		absPath = filepath.Join(cwd, displayPath)
	}
	absPath = filepath.Clean(absPath)

	// Separate context lines (N:content before first operation) from operation lines
	restLines := lines[1:]
	ctxLines, opLines := splitContextAndOps(restLines)

	// Parse context lines
	contextLines := make(map[int]string)
	for _, line := range ctxLines {
		if m := contextLineRe.FindStringSubmatch(line); m != nil {
			lineNum := parseInt(m[1])
			content := m[2]
			// Only record if the line number hasn't been seen yet
			if _, exists := contextLines[lineNum]; !exists {
				contextLines[lineNum] = content
			}
		}
	}

	// Parse remaining lines as operations
	ops, err := parseOperations(opLines)
	if err != nil {
		return Section{}, fmt.Errorf("section for %s: %w", displayPath, err)
	}

	if len(ops) == 0 {
		return Section{}, ErrSectionNoOps(displayPath)
	}

	return Section{
		Path:         absPath,
		Tag:          tag,
		Operations:   ops,
		ContextLines: contextLines,
	}, nil
}

// splitContextAndOps splits lines into context lines (before first operation command)
// and operation lines (from first operation onward).
func splitContextAndOps(lines []string) (contextLines, opLines []string) {
	opIndex := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if isOpCommandLine(trimmed) {
			opIndex = i
			break
		}
	}

	if opIndex == -1 {
		// No operation found — all lines are context
		return lines, nil
	}

	return lines[:opIndex], lines[opIndex:]
}

// isOpCommandLine checks if a trimmed line starts an operation command.
func isOpCommandLine(line string) bool {
	return replaceRe.MatchString(line) ||
		deleteRe.MatchString(line) ||
		insertBeforeRe.MatchString(line) ||
		insertAfterRe.MatchString(line) ||
		insertHeadRe.MatchString(line) ||
		insertTailRe.MatchString(line)
}

// parseOperations parses operation commands and body lines.
func parseOperations(lines []string) ([]Operation, error) {
	var ops []Operation
	var currentBody []string
	var pendingOp *Operation

	flushPendingOp := func() error {
		if pendingOp != nil {
			switch pendingOp.Type {
			case OpReplace:
				if len(currentBody) == 0 {
					return fmt.Errorf("%s", ErrEmptyReplace)
				}
			case OpDelete:
				if len(currentBody) > 0 {
					return fmt.Errorf("%s", ErrDeleteTakesNoBody)
				}
			case OpInsertBefore, OpInsertAfter, OpInsertHead, OpInsertTail:
				if len(currentBody) == 0 {
					return fmt.Errorf("%s", ErrEmptyInsert)
				}
			}
			pendingOp.Body = currentBody
			ops = append(ops, *pendingOp)
			pendingOp = nil
			currentBody = nil
		}
		return nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Try to match as an operation command
		if op, ok := parseOpCommand(line); ok {
			if err := flushPendingOp(); err != nil {
				return nil, err
			}
			pendingOp = &op
			continue
		}

		// Try to match as a body line
		if m := bodyLineRe.FindStringSubmatch(line); m != nil {
			if pendingOp == nil {
				return nil, fmt.Errorf("%s", ErrBodyWithoutOp)
			}
			content := m[1]
			currentBody = append(currentBody, content)
			continue
		}

		// Skip lines that don't match (comments, blank lines with text)
		continue
	}

	if err := flushPendingOp(); err != nil {
		return nil, err
	}

	return ops, nil
}

// parseOpCommand tries to parse line as an operation command.
// Returns (op, true) on success, (_, false) if no match.
func parseOpCommand(line string) (Operation, bool) {
	// replace N..M:
	if m := replaceRe.FindStringSubmatch(line); m != nil {
		return Operation{
			Type:  OpReplace,
			Start: parseInt(m[1]),
			End:   parseInt(m[2]),
		}, true
	}

	// delete N or delete N..M
	if m := deleteRe.FindStringSubmatch(line); m != nil {
		start := parseInt(m[1])
		end := start
		if m[2] != "" {
			end = parseInt(m[2])
		}
		return Operation{
			Type:  OpDelete,
			Start: start,
			End:   end,
		}, true
	}

	// insert before N:
	if m := insertBeforeRe.FindStringSubmatch(line); m != nil {
		return Operation{
			Type:  OpInsertBefore,
			Start: parseInt(m[1]),
		}, true
	}

	// insert after N:
	if m := insertAfterRe.FindStringSubmatch(line); m != nil {
		return Operation{
			Type:  OpInsertAfter,
			Start: parseInt(m[1]),
		}, true
	}

	// insert head:
	if insertHeadRe.MatchString(line) {
		return Operation{Type: OpInsertHead}, true
	}

	// insert tail:
	if insertTailRe.MatchString(line) {
		return Operation{Type: OpInsertTail}, true
	}

	return Operation{}, false
}

// parseInt converts a decimal string to int. Returns 0 on error.
// parseInt converts a decimal string to int using strconv.Atoi.
func parseInt(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// normalizeLineEndings converts CRLF and CR to LF.
func normalizeLineEndings(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}
