package tools

import "encoding/json"

// FileChange is the edit a single tool call performed, in the shape both consumers
// need — the ACP stream (a structured diff block for Zed) and the desktop UI (a
// rendered diff). OldText/NewText are the FRAGMENTS the call carried, not the whole
// file: an EditFile's old_string/new_string, a WriteFile's content.
//
// An empty OldText means "this call has no old text" (a create). That is the same
// distinction the ACP SDK draws with a nil OldText, so callers forwarding this to
// ToolDiffContent must omit the argument rather than pass "".
type FileChange struct {
	Path    string
	OldText string
	NewText string
	// ReplaceAll records that the call replaced every occurrence (EditFile's
	// replace_all). The UI labels such a diff: a fragment diff can show one block
	// against another, not paired occurrences.
	ReplaceAll bool
}

// FileChangeForTool derives the change of a tool call from its raw arguments.
//
// It answers "what did this call set out to change", never "did it succeed" — the
// caller decides whether to show it (a tool_call and its tool_result are separate
// records, and the failure is only known once the result arrives). A failed edit
// changes nothing, so the UI filters on success; deriving is still correct, and
// keeps this function free of execution state.
//
// ok=false for tools that do not change a file's text, for unparsable args, or when
// the arguments carry no usable text. The text rules mirror what the ACP stream has
// always done (agent/acp/stream.go): an edit that says nothing about either side is
// not a change, and WriteFile without content (an empty file) has nothing to show.
func FileChangeForTool(name, argsJSON string) (FileChange, bool) {
	if argsJSON == "" {
		return FileChange{}, false
	}

	switch name {
	case ToolNameEdit:
		var args struct {
			Path       string `json:"path"`
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return FileChange{}, false
		}
		if args.Path == "" || (args.OldString == "" && args.NewString == "") {
			return FileChange{}, false
		}
		return FileChange{
			Path:       args.Path,
			OldText:    args.OldString,
			NewText:    args.NewString,
			ReplaceAll: args.ReplaceAll,
		}, true

	case ToolNameWrite:
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return FileChange{}, false
		}
		if args.Path == "" || args.Content == "" {
			return FileChange{}, false
		}
		// WriteFile creates or replaces a file: there is no old text to compare against,
		// and reading the file to invent one would be both slow and wrong (the content
		// at execution time is not the content now).
		return FileChange{Path: args.Path, NewText: args.Content}, true
	}
	return FileChange{}, false
}
