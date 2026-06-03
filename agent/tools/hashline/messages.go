package hashline

import "fmt"

// LLM-facing error and warning messages. These are designed to give the LLM
// clear, actionable guidance so it can self-correct on the next turn.

// ErrEmptyReplace is returned when a replace hunk has no body rows.
const ErrEmptyReplace = "`replace N..M:` needs at least one `+TEXT` body row. To delete lines, use `delete N..M`."

// ErrDeleteTakesNoBody is returned when a delete hunk has body rows.
const ErrDeleteTakesNoBody = "`delete N..M` does not take body rows. Remove the body rows, or use `replace N..M:` if you intended to replace the lines."

// ErrEmptyInsert is returned when an insert hunk has no body rows.
const ErrEmptyInsert = "`insert` needs at least one `+TEXT` body row."

// ErrBodyWithoutOp is returned when +body rows appear without a preceding operation.
const ErrBodyWithoutOp = "Body rows (`+TEXT`) must follow an operation header (`replace`, `insert`). Each operation starts with a header line ending in `:`."

// ErrMinusRow is returned when a body row starts with `-`.
const ErrMinusRow = "`-` rows are not valid in hashline — the range already names the lines being changed. To insert a literal line starting with `-`, write `+-text`."

// ErrTagMismatch returns a tag mismatch error with the actual tag for recovery.
func ErrTagMismatch(path, expectedTag, currentTag string) error {
	return fmt.Errorf(
		"Snapshot tag mismatch for %s: expected %s, got %s — the file content changed since it was read. "+
			"Re-read the file with the ReadFile tool to get the current tag and line numbers before editing again.",
		path, expectedTag, currentTag,
	)
}

// ErrSnapshotRequired returns an error when no snapshot exists for the path.
func ErrSnapshotRequired(path string) error {
	return fmt.Errorf(
		"No snapshot recorded for %s — read the file first with the ReadFile tool to establish a snapshot tag.",
		path,
	)
}

// ErrStaleTag returns an error when a tag is not found for a path that has other
// recorded snapshots (i.e., the file was read before but the tag is outdated).
func ErrStaleTag(path, tag string) error {
	return fmt.Errorf(
		"Tag %q not found for %s — the file has been edited since this tag was issued. "+
			"Re-read the file with the ReadFile tool to get the current tag and line numbers.",
		tag, path,
	)
}

// ErrLineOutOfRange returns an error when an operation references a line beyond the file.
func ErrLineOutOfRange(line, totalLines int) error {
	return fmt.Errorf(
		"Line %d is beyond the end of the file (which has %d lines). Check that the line number is correct and the file hasn't been modified.",
		line, totalLines,
	)
}

// ErrSectionNoOps returns an error when a section header has no operations.
func ErrSectionNoOps(displayPath string) error {
	return fmt.Errorf(
		"Section for %s has no operations. Each section needs at least one operation (`replace`, `delete`, `insert`).",
		displayPath,
	)
}

// ErrEmptyInput is returned when the input is empty.
const ErrEmptyInput = "Hashline input is empty. Provide at least one section with a `¶PATH#TAG` header and an operation."

// ErrInvalidHeader is returned when the first line of a section is not a valid header.
func ErrInvalidHeader(line string) error {
	return fmt.Errorf(
		"Section must start with `¶PATH#TAG` (e.g., `¶file.go#a1b2`), got: %q. "+
			"Copy the header from the ReadFile output — it includes the snapshot tag that anchors your edits.",
		line,
	)
}

// ErrContextLineMismatch returns an error when a numbered context line doesn't
// fuzzy-match the actual file content.
func ErrContextLineMismatch(lineNum int, expected, actual string, threshold float64) error {
	return fmt.Errorf(
		"Context line %d doesn't match file content (similarity threshold: %.2f):\n"+
			"  expected: %q\n"+
			"  actual:   %q\n"+
			"Double-check that you're editing the right part of the file. "+
			"If the file has changed since you read it, re-read with the ReadFile tool to get updated line numbers.",
		lineNum, threshold, expected, actual,
	)
}

// ErrContextLineOutOfRange returns an error when a context line number is
// beyond the actual file length.
func ErrContextLineOutOfRange(lineNum, totalLines int) error {
	return fmt.Errorf(
		"Context line %d is beyond the file's %d lines. "+
			"The file may have changed since you read it. Re-read with the ReadFile tool.",
		lineNum, totalLines,
	)
}

// ErrOverlappingRange returns an error when two replace/delete operations
// target the same line.
func ErrOverlappingRange(line int) error {
	return fmt.Errorf(
		"Line %d is targeted by multiple replace/delete operations. "+
			"Each line can only be replaced or deleted once per edit. "+
			"Combine the overlapping operations into a single `replace` or `delete` range.",
		line,
	)
}
