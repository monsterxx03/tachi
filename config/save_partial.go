package config

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// SavePartial updates the given leaf values in config.yaml and leaves the rest of
// the file alone: comments (head, line and foot), key order, blank-line grouping,
// quoting style and the file's own indentation — including the "indentless"
// sequence style a hand-written file usually has (`servers:` followed by
// `- name: x` at the SAME indent).
//
// The file is hand-editable, so preserving it is not a nicety: a full
// Marshal/Unmarshal round trip rewrites all of it. Measured on a real 63-line
// config.yaml, editing ONE value through Save (yaml.Marshal of the whole struct)
// changed 70 lines — every comment gone, indentation re-flowed from 2 to 4
// spaces, sequences re-indented under their key. The same edit through this
// function changed 3 lines, one per value.
//
// CONTENT is preserved, not every byte: the run of spaces that right-aligns a
// trailing comment before its "#" is normalized (`x: 1     # note` → `x: 1 # note`)
// on EVERY line carrying one, whether or not it was touched. The comment itself
// survives intact. TestSavePartial_TrailingCommentPaddingIsNormalized pins this,
// so the promise above cannot quietly drift into a stronger claim than the code
// delivers.
//
// updates maps a dot path to the new leaf value: "language", "lsp.enabled",
// "providers.0.model" (numeric segments index sequences). Values are rendered as
// YAML scalars; a *bool / *int / *int64 / *float64 may be passed directly so a
// pointer field keeps its field type (see renderScalar).
//
// ⚠️ A path must NAME its blocks; the file does not have to spell them out. A key whose parent
// block is missing is added under a block this function creates (`spec:` that the file lacks gains
// its first key), because the caller named the path and the path says where the block goes — the
// same no-guessing `createListStructure` (list_items.go) already relies on for a list's blocks.
// What is still refused is what a path cannot answer, and none of it is guessed at:
//
//   - a SEQUENCE element: a list grows at its END, with a skeleton the element type decides, and
//     that is AppendListItem's own entry point (list_items.go) — not this one;
//   - a step through a node that is not a block (`language.foo`, `providers.type`): there is no
//     block to put the key in;
//   - a MAP key the file does not have (`hooks.events.pre_tool.…`): a map is keyed by name, so
//     writing one means inventing the name.
//
// The refusal is not "no error means success": goccy's ReplaceWithNode reports success for a
// missing path while writing nothing (verified), so this function re-reads the file and verifies
// each path holds the value it was given, and that check is the net under everything else.
//
// The write is atomic (tmp + rename) for the same reason session metadata is:
// another Tachi process may be reading config.yaml at startup, and an in-place
// rewrite can hand it a half-written file.
func SavePartial(updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	return SavePartialTo(path, updates)
}

// SavePartialTo is SavePartial for an explicit path (tests, and any caller that
// already resolved it).
func SavePartialTo(path string, updates map[string]any) error {
	return ApplyConfigEditsTo(path, ListEdits{Updates: updates})
}

// applyUpdatesToFile edits the parsed file in place: the per-key half of a file edit, WITHOUT the
// write, so a caller performing several kinds of edit can emit the result once (see
// ApplyConfigEditsTo — one save is one write).
//
// The write itself, the "changed nothing" check and the atomic rename live in the caller.
func applyUpdatesToFile(file *ast.File, updates map[string]any) error {
	// Sorted paths, so a file with several edits is written in a stable order and
	// a failure is reported for the same edit every time.
	for _, key := range sortedKeys(updates) {
		p, err := yamlPath(key)
		if err != nil {
			return fmt.Errorf("invalid path %q: %w", key, err)
		}
		lit, err := renderScalar(updates[key])
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		// The literal is PARSED rather than wrapped in ast.String: that is what
		// makes a list render as a list (`["a", "b"]` becomes a SequenceNode),
		// a number as a number and a bool as a bool, with one code path. Wrapping
		// everything as a string node silently wrote sequences as quoted text.
		node, err := parseScalarNode(lit)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if err := p.ReplaceWithNode(file, node); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
		// ReplaceWithNode reports success for a path the file does not have while writing
		// nothing, so existence is asked separately — and a path the file does not have yet is
		// added, block by block, as far as the path names blocks (see insertMissingKey).
		if _, err := p.FilterFile(file); err != nil {
			if err := insertMissingKey(file, key, lit); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	return nil
}

// insertMissingKey puts `key: <lit>` into the file, creating whatever MAPPING blocks the path
// names and the file does not have yet (see ensureBlock).
//
// It is the "adding a key" half of SavePartial's contract: the config OBJECT always has every
// struct field as a leaf while `omitempty` means the FILE may not spell one out — without this,
// editing such a leaf would fail with "not found in the file" and look like a broken form, which
// is exactly what a freshly added list item produces (its skeleton writes the fields the struct
// always writes and leaves the rest).
//
// The KEY is BUILT rather than parsed from a snippet, and its column comes from the block itself
// (see keyPosition): the emitter lays a mapping's lines out from each key token's column, and a
// node parsed out of a standalone snippet carries positions that point into its own buffer — the
// symptom is a key printed as garbage (`context_window` came out `ext_window`). The VALUE is
// parsed, which is fine and is what the update path does: its token's text is its own.
func insertMissingKey(file *ast.File, key, lit string) error {
	leaf, parentPath := key, ""
	if at := strings.LastIndex(key, "."); at > 0 {
		leaf, parentPath = key[at+1:], key[:at]
	}
	block, err := ensureBlock(file, parentPath)
	if err != nil {
		return err
	}
	pos, err := keyPosition(file, key)
	if err != nil {
		return err
	}
	value, err := parseScalarNode(lit)
	if err != nil {
		return err
	}
	addKeyToBlock(block, leaf, value, pos)
	return nil
}

// ensureBlock returns the block that blockPath names, creating — as empty blocks, one key at a
// time — every MAPPING between the document root and it that the file does not spell out. An
// empty blockPath is the document root itself, which is where a top-level key goes.
//
// Creating a block is the same no-guessing as adding a key to one: the CALLER named the path, and
// the path is where the block goes (the argument `createListStructure` in list_items.go already
// makes for a list's own blocks — a config that never configured search has no `web_search:` key
// either). What is refused is what a path cannot answer:
//
//   - a SEQUENCE element: nothing says what a new element's other fields hold, and a list is
//     grown at its END with a skeleton — AppendListItem's entry point, not this one;
//   - a step through a node that is not a block (a scalar or a sequence): there is no block to
//     put the key in;
//   - a MAP key the file does not have: a map is keyed by NAME, and inventing one is the guess
//     this package refuses (`hooks.events.pre_tool`).
func ensureBlock(file *ast.File, blockPath string) (*ast.MappingNode, error) {
	cur, err := rootBlock(file)
	if err != nil {
		return nil, err
	}
	if blockPath == "" {
		return cur, nil
	}
	var (
		segs   = strings.Split(blockPath, ".")
		inList bool // the step before was a list: it was poured through to reach an element
	)
	for i, seg := range segs {
		at := strings.Join(segs[:i+1], ".")
		node, err := filterNode(file, at)
		if err == nil {
			switch n := node.(type) {
			case *ast.MappingNode:
				cur, inList = n, false
			case *ast.SequenceNode:
				// A list is poured through only to reach ONE of its elements, by index: a
				// named segment under it would be a key on a list, which YAML does not have.
				if i+1 >= len(segs) || !isIndexSegment(segs[i+1]) {
					return nil, fmt.Errorf("%w: %q in the file is a list, and only its elements "+
						"can be reached through it (by index)", errNotInFile, at)
				}
				cur, inList = nil, true
			default:
				return nil, fmt.Errorf("%w: %q in the file is a %s, and only a block takes a new key",
					errNotInFile, at, node.Type())
			}
			continue
		}
		// Missing: a NAME becomes a block, an INDEX never becomes anything — nothing says what
		// a new element's other fields hold, and a list is grown at its END with a skeleton
		// (AppendListItem), not from here.
		if isIndexSegment(seg) {
			return nil, fmt.Errorf("%w: %q is a sequence element, and a new one is appended through "+
				"its own entry point (AppendListItem)", errNotInFile, at)
		}
		if inList {
			return nil, fmt.Errorf("%w: %q is a key on a list, which holds elements by index",
				errNotInFile, at)
		}
		pos, err := keyPosition(file, at)
		if err != nil {
			return nil, err
		}
		child := newBlockNode(pos)
		addKeyToBlock(cur, seg, child, pos)
		cur = child
	}
	return cur, nil
}

// isIndexSegment reports whether a path segment addresses a sequence element rather than a key.
func isIndexSegment(seg string) bool {
	_, err := strconv.Atoi(seg)
	return err == nil
}

// rootBlock is the document's top-level block: the one block that is never missing, and the one
// that is never created either — a file with no top level of its own (empty, or comments only)
// has nothing a key could go in, and turning THAT into a block would be inventing the file's
// whole shape, which is the one thing a path does not say.
func rootBlock(file *ast.File) (*ast.MappingNode, error) {
	if len(file.Docs) == 0 || file.Docs[0] == nil || file.Docs[0].Body == nil {
		return nil, fmt.Errorf("%w: the file's top level is empty, not a block", errNotInFile)
	}
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return nil, fmt.Errorf("%w: the file's top level is a %s, not a block",
			errNotInFile, file.Docs[0].Body.Type())
	}
	return root, nil
}

// filterNode resolves a dot path in the file, reporting the node the file holds there.
func filterNode(file *ast.File, key string) (ast.Node, error) {
	p, err := yamlPath(key)
	if err != nil {
		return nil, err
	}
	return p.FilterFile(file)
}

// addKeyToBlock appends `name: value` to a block's own keys — the ONE place a key enters a block,
// for both this file's insertions and list_items.go's structure creation (the token layout is the
// part that goes wrong silently, so it lives once).
func addKeyToBlock(block *ast.MappingNode, name string, value ast.Node, pos *token.Position) {
	// A block that gains a key stops being `{}`: an empty flow mapping keeps its braces on the
	// parent's line, so the child would be emitted inside them (`spec: {proxy: x}`).
	block.IsFlowStyle = false
	block.Values = append(block.Values, ast.MappingValue(nil, ast.String(token.New(name, name, pos)), value))
}

// newBlockNode builds the empty block a created one starts as. Its start token carries the column
// the emitter lays the block's lines out from; a node built without one panics when printed.
func newBlockNode(pos *token.Position) *ast.MappingNode {
	return ast.Mapping(token.New("", "", pos), false)
}

// keyPosition finds the column a new key of `key`'s parent block belongs at: the column of a key
// already in that block.
//
// An EMPTY block (a `spec: {}` — what a freshly appended list item holds, since the fields the
// struct always writes are the only ones a skeleton spells out, and what a block this file just
// CREATED starts as) has no key to copy, so the search walks UP to the nearest ancestor that has
// one and indents one step per level. Without this the new key lands at the block's own level,
// which parses as a sibling of the block.
func keyPosition(file *ast.File, key string) (*token.Position, error) {
	segs := strings.Split(key, ".")
	// One candidate per level up to the document root (prefix ""), which is where a TOP-LEVEL
	// key's neighbours are: the root's own keys.
	for drop := 1; drop <= len(segs); drop++ {
		prefix := strings.Join(segs[:len(segs)-drop], ".")
		var node ast.Node
		if prefix == "" {
			if len(file.Docs) == 0 || file.Docs[0].Body == nil {
				break
			}
			node = file.Docs[0].Body
		} else {
			p, err := yamlPath(prefix)
			if err != nil {
				break
			}
			found, err := p.FilterFile(file)
			if err != nil {
				continue // this level is not in the file either; the one above it may be
			}
			node = found
		}
		m, ok := node.(*ast.MappingNode)
		if !ok || len(m.Values) == 0 {
			continue
		}
		pos := *m.Values[len(m.Values)-1].Key.GetToken().Position
		pos.Column += 2 * (drop - 1)
		pos.Offset += 2 * (drop - 1)
		return &pos, nil
	}
	return nil, fmt.Errorf("%w, and no block above %q has a key to place one beside", errNotInFile, key)
}

// errNotInFile marks the "the file does not have this path" family, so the message a caller shows
// keeps saying which path and why it could not be created.
var errNotInFile = errors.New("not in the file")

// parseScalarNode turns a rendered YAML literal into the node to place in the
// file. Parsing (rather than constructing) is what lets one code path cover every
// shape a setting can have: a scalar stays a scalar with its own type, and a flow
// list stays a sequence.
func parseScalarNode(lit string) (ast.Node, error) {
	file, err := parser.ParseBytes([]byte(lit+"\n"), parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("render %q: %w", lit, err)
	}
	if len(file.Docs) == 0 || file.Docs[0].Body == nil {
		return nil, fmt.Errorf("render %q: produced no node", lit)
	}
	return file.Docs[0].Body, nil
}

// verifyUpdates re-parses the bytes about to be written and confirms every path
// holds the value that was requested, compared as its rendered YAML text.
func verifyUpdates(after []byte, updates map[string]any) error {
	file, err := parser.ParseBytes(after, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("verify: re-parse failed: %w", err)
	}
	for _, key := range sortedKeys(updates) {
		want, err := renderScalar(updates[key])
		if err != nil {
			return err
		}
		p, err := yamlPath(key)
		if err != nil {
			return err
		}
		node, err := p.FilterFile(file)
		if err != nil {
			// A path the file does not hold even after the edit: the insert above refused
			// (it returns before this), or it landed somewhere the path does not reach —
			// either way nothing is written.
			return fmt.Errorf("verify %s: not found in the file after the edit: %w", key, err)
		}
		// Compared as text, both normalized through the same parse+render path, so
		// a formatting difference between the requested literal and what the emitter
		// chose (quoting, flow spacing) does not read as a failed edit.
		gotNode, err := parseScalarNode(strings.TrimSpace(node.String()))
		if err != nil {
			return fmt.Errorf("verify %s: %w", key, err)
		}
		wantNode, err := parseScalarNode(want)
		if err != nil {
			return fmt.Errorf("verify %s: %w", key, err)
		}
		if gotNode.String() != wantNode.String() {
			return fmt.Errorf("verify %s: file holds %q, expected %q — the edit did not "+
				"take effect", key, node.String(), want)
		}
	}
	return nil
}

// renderScalar renders a setting value as the YAML text to place in the file.
//
// The pointer cases exist because the config's optional fields ARE pointers
// (*bool / *int / *int64 / *float64): a caller that dereferences one to pass it
// here would have to spell out its own formatting and could get it wrong for a
// type it did not expect. Passing the pointer through keeps the rendering here.
func renderScalar(v any) (string, error) {
	switch t := v.(type) {
	case Unset:
		// The one value that renders as a bare null: "not set", which is how these fields spell
		// 自动 (`vision: null`). Reaching here for a leaf that cannot be unset is assignValue's
		// to refuse — this function only knows the file's vocabulary, not the field's.
		return "null", nil
	case nil:
		return "", errors.New("nil is not a supported value (use config.Unset to clear an optional leaf, or an explicit empty string)")
	case string:
		return quoteYAMLString(t), nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case *bool:
		if t == nil {
			return "", errors.New("nil *bool")
		}
		return strconv.FormatBool(*t), nil
	case *int:
		if t == nil {
			return "", errors.New("nil *int")
		}
		return strconv.Itoa(*t), nil
	case *int64:
		if t == nil {
			return "", errors.New("nil *int64")
		}
		return strconv.FormatInt(*t, 10), nil
	case *float64:
		if t == nil {
			return "", errors.New("nil *float64")
		}
		return strconv.FormatFloat(*t, 'f', -1, 64), nil
	case []string:
		return renderStringList(t), nil
	case []any:
		// What a BOUND METHOD receives for a JS array: the settings page's payload arrives as
		// map[string]any, so its lists are []any and not []string. The elements must be strings
		// — the config has no list of numbers to render, and SetPath says the same thing about
		// the memory side.
		list := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return "", fmt.Errorf("unsupported list element type %T", e)
			}
			list = append(list, s)
		}
		return renderStringList(list), nil
	default:
		return "", fmt.Errorf("unsupported value type %T", v)
	}
}

// renderStringList renders a string slice in flow style, which is how the
// hand-written configs in the wild write short lists (`extensions: [".go"]`).
// goccy takes the literal as the node's replacement, so this is presentation the
// caller controls, not something the library decides.
func renderStringList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = quoteYAMLString(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// quoteYAMLString quotes a string only when the bare form would not round-trip:
// a value that would parse as another type (numbers, booleans, null), or one whose
// characters are meaningful mid-scalar (": ", " #", a leading indicator). Plain
// text stays plain, because quoting everything would rewrite the user's file style
// on every save.
func quoteYAMLString(s string) string {
	if s == "" {
		return `""`
	}
	if looksNonString(s) || strings.ContainsAny(s, ":#{}[],&*!|>'\"%@`\n\t") ||
		strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") ||
		strings.HasPrefix(s, "-") || strings.HasPrefix(s, "?") {
		return strconv.Quote(s)
	}
	return s
}

// looksNonString reports whether the bare text would be read back as a non-string
// YAML scalar (a number, bool or null), which is exactly when quoting is required.
func looksNonString(s string) bool {
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~", ".nan", ".inf", "-.inf":
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Path handling
// ---------------------------------------------------------------------------

// yamlPath translates the caller-facing dot path into goccy's path syntax.
//
// The caller's form is uniform — "providers.0.model" — because a dot path is what
// a settings schema declares and what reads naturally in an error message.
// goccy instead wants a numeric segment as a bracketed index
// ("providers[0].model"); a bare ".0." reaches it as a map lookup on the key "0",
// which fails with "expected node type is map or map value. but got Sequence".
//
// So the two forms are translated HERE, in one place, and every caller (and every
// error message) keeps saying "providers.0.model".
func yamlPath(key string) (*yaml.Path, error) {
	var b strings.Builder
	b.WriteString("$")
	for _, seg := range strings.Split(key, ".") {
		if seg == "" {
			return nil, fmt.Errorf("empty path segment in %q", key)
		}
		if _, err := strconv.Atoi(seg); err == nil {
			b.WriteString("[" + seg + "]")
			continue
		}
		b.WriteString("." + seg)
	}
	return yaml.PathString(b.String())
}

// sortedKeys returns the map keys in a stable order, so a multi-key save writes
// deterministically and reports the same first failure on every run.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
