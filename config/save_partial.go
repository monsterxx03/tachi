package config

import (
	"errors"
	"fmt"
	"os"
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
// ⚠️ Every path must ALREADY EXIST in the file, with ONE exception this function does handle:
// a key whose PARENT mapping exists is added (a `spec:` block that does not spell out
// `context_window` gains that line). There is nothing to guess there — the parent says where the
// key goes — which is what makes it different from creating structure, and structure is still
// refused: a missing parent, a sequence index past the end, a new top-level block, all error.
// (The one place structure IS created is a list item, and that has its own entry point with its
// own rules — see list_items.go.)
//
// The refusal is not "no error means success": goccy's ReplaceWithNode reports success for a
// missing path while writing nothing (verified), so this function re-reads the file and verifies
// each path holds the value it was given, and that check is what catches everything else.
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
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	// Parse with COMMENTS — without the option the parse drops them and the
	// round trip silently strips every comment in the user's file.
	file, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	// Sorted paths, so a file with several edits is written in a stable order and
	// a failure is reported for the same edit every time.
	for _, key := range sortedKeys(updates) {
		p, err := yamlPath(key)
		if err != nil {
			return fmt.Errorf("%s: invalid path %q: %w", path, key, err)
		}
		lit, err := renderScalar(updates[key])
		if err != nil {
			return fmt.Errorf("%s: %s: %w", path, key, err)
		}
		// The literal is PARSED rather than wrapped in ast.String: that is what
		// makes a list render as a list (`["a", "b"]` becomes a SequenceNode),
		// a number as a number and a bool as a bool, with one code path. Wrapping
		// everything as a string node silently wrote sequences as quoted text.
		node, err := parseScalarNode(lit)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", path, key, err)
		}
		if err := p.ReplaceWithNode(file, node); err != nil {
			return fmt.Errorf("%s: set %s: %w", path, key, err)
		}
		// ReplaceWithNode reports success for a path the file does not have while writing
		// nothing, so existence is asked separately — and a missing key whose parent IS there is
		// the one case this function adds rather than refuses (see the doc comment).
		if _, err := p.FilterFile(file); err != nil {
			if err := addMissingKey(file, key, lit); err != nil {
				return fmt.Errorf("%s: %s: %w", path, key, err)
			}
		}
	}

	out := []byte(file.String())
	// Verification before the write, because ReplaceWithNode is silent about a
	// path it did not find (see the doc comment). Reading it back is also the only
	// check that covers "the value we asked for is the value the file now holds"
	// for EVERY path at once, whatever the library did internally.
	if err := verifyUpdates(out, updates); err != nil {
		return err
	}

	// A save that changed nothing must not rewrite the file: the caller may be
	// re-saving a form it did not edit, and touching the mtime (or re-emitting the
	// user's bytes) for that is rude at best. Compared on the bytes we would write,
	// so this cannot hide a real edit.
	if string(data) == string(out) {
		return nil
	}

	// Atomic: a concurrent `tachi` starting up reads this file, and it must never
	// see a truncated one. Mode follows the existing file so a user's 0600 stays.
	return atomicWriteConfig(path, out)
}

// addMissingKey inserts `key: <lit>` into the file when the key's PARENT block already exists.
//
// It is the narrow half of "adding a key" — the other half (creating structure) stays refused,
// because its answer is not in the file: which block would a new top-level key go under? A key
// whose parent is right there has no such question. It is what makes a leaf representable that
// the CONFIG OBJECT always has and a hand-written file may not: every struct field is a leaf the
// settings form offers, while `omitempty` means the file may not spell out one — without this,
// editing it would fail with "not found in the file" and look like a broken form.
//
// The KEY is BUILT rather than parsed from a snippet, and its column comes from the block itself
// (see keyPosition): the emitter lays a mapping's lines out from each key token's column, and a
// node parsed out of a standalone snippet carries positions that point into its own buffer — the
// symptom is a key printed as garbage (`context_window` came out `ext_window`). The VALUE is
// parsed, which is fine and is what the update path does: its token's text is its own.
func addMissingKey(file *ast.File, key, lit string) error {
	at := strings.LastIndex(key, ".")
	if at <= 0 || at == len(key)-1 {
		return fmt.Errorf("%w, and %q is not a key under an existing block", errNotInFile, key)
	}
	parent, err := yamlPath(key[:at])
	if err != nil {
		return err
	}
	node, err := parent.FilterFile(file)
	if err != nil {
		return fmt.Errorf("%w, and its parent %q is not either", errNotInFile, key[:at])
	}
	mapping, ok := node.(*ast.MappingNode)
	if !ok {
		return fmt.Errorf("%w: %q is a %s, and only a block can take a new key",
			errNotInFile, key[:at], node.Type())
	}
	pos, err := keyPosition(file, key)
	if err != nil {
		return err
	}
	value, err := parseScalarNode(lit)
	if err != nil {
		return err
	}
	// A block that gains a key is no longer `{}`: an empty flow mapping has to become a real
	// block, or the child would be emitted inside the braces on the parent's line.
	mapping.IsFlowStyle = false
	leaf := key[at+1:]
	mapping.Values = append(mapping.Values, ast.MappingValue(nil, ast.String(token.New(leaf, leaf, pos)), value))
	return nil
}

// keyPosition finds the column a new key of `key`'s parent block belongs at: the column of a key
// already in that block.
//
// An EMPTY block (a `spec: {}` — what a freshly appended list item holds, since the fields the
// struct always writes are the only ones a skeleton spells out) has no key to copy, so the search
// walks UP to the nearest ancestor that has one and indents one step per level. Without this the
// new key lands at the block's own level, which parses as a sibling of the block.
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
			// A path that does not exist lands here: the caller asked to update a
			// key the file does not have (goccy's ReplaceWithNode reports success
			// for it while writing nothing, which is why this check exists).
			return fmt.Errorf("verify %s: not found in the file — SavePartial only updates "+
				"keys that already exist: %w", key, err)
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
