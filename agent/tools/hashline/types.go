package hashline

// OpType represents the type of a hashline edit operation.
type OpType int

const (
	OpReplace      OpType = iota // replace N..M: +body
	OpDelete                      // delete N..M
	OpInsertBefore                // insert before N: +body
	OpInsertAfter                 // insert after N: +body
	OpInsertHead                  // insert head: +body
	OpInsertTail                  // insert tail: +body
)

// String returns the operation type name for debugging.
func (t OpType) String() string {
	switch t {
	case OpReplace:
		return "replace"
	case OpDelete:
		return "delete"
	case OpInsertBefore:
		return "insert_before"
	case OpInsertAfter:
		return "insert_after"
	case OpInsertHead:
		return "insert_head"
	case OpInsertTail:
		return "insert_tail"
	default:
		return "unknown"
	}
}

// Operation represents a single edit operation within a section.
type Operation struct {
	Type  OpType
	Start int      // 1-indexed start line (for replace, delete, insert_before, insert_after)
	End   int      // 1-indexed end line (for replace, delete)
	Body  []string // content lines (for replace, insert operations)
}

// Section represents a single file edit section.
type Section struct {
	Path       string      // Absolute file path
	// ContextLines holds optional numbered lines from the LLM for content verification.
	// Key is 1-indexed line number, value is the expected line content.
	// If set, Prepare() will fuzzy-match these against the actual file content
	// using the Patcher's FuzzyThreshold. Empty/nil means no context verification.
	ContextLines map[int]string
	Tag        string      // 4-hex hash tag (e.g. "a1f0")
	Operations []Operation // Edit operations to apply
}
