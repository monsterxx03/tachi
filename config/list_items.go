package config

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/creasty/defaults"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"

	"github.com/monsterxx03/tachi/pkg/fileutil"
)

// Adding and removing ITEMS of a config list: the structural edit the settings page makes that is
// not "change a value in place".
//
// SavePartial's contract is "update a key that exists, and add one — together with any missing
// BLOCK the path names — where the path says it goes" (see its doc comment). A list item is still
// its own entry point, because a path does NOT answer everything about it: a new element's other
// fields need a skeleton (the element type's), and there is exactly ONE place it can go (the end).
// That is the part this file owns; the block a list lives in is the same "the path says where it
// goes" as a key's, which is why `createListStructure` may create it and `ensureBlock`
// (save_partial.go) may create a key's.
//
// The element written is what the app's own `tachi` init writes for a fresh one: the element
// marshalled as a struct, so its key order is the struct's and a new block in a hand-written file
// looks like the ones already beside it. Fields the struct does not always write (`omitempty`
// ones, like a provider's `spec` contents) are left out — a later edit of such a leaf adds it,
// block and all, which SavePartial does.

// AppendListItem appends one element to the list at listPath, IN MEMORY, and reports the new
// element's path and the YAML literal for it.
//
// The two halves are split the way the rest of this package splits them: this one mutates the
// config object, AppendListItemTo writes the file, and the caller undoes this one if that fails.
func AppendListItem(cfg *Config, listPath string) (elementPath, literal string, err error) {
	slice, err := listField(cfg, listPath)
	if err != nil {
		return "", "", err
	}
	// The defaults are applied because this element is meant to be a WORKING starting point, not
	// a field of zeroes: `web_search.providers` declares `type: exa`, and a new entry that says so
	// is one the user only has to fill a key into.
	elem := reflect.New(slice.Type().Elem())
	if err := defaults.Set(elem.Interface()); err != nil {
		return "", "", fmt.Errorf("%s: 默认值: %w", listPath, err)
	}
	value := elem.Elem()
	slice.Set(reflect.Append(slice, value))
	elementPath = fmt.Sprintf("%s.%d", listPath, slice.Len()-1)

	lit, err := yaml.Marshal(value.Interface())
	if err != nil {
		slice.Set(slice.Slice(0, slice.Len()-1))
		return "", "", fmt.Errorf("%s: 渲染新条目: %w", elementPath, err)
	}
	return elementPath, string(lit), nil
}

// AppendListItemTo writes a new element at the END of the list at listPath in the file at path.
//
// It is the one-off entry point for the same operation ApplyConfigEditsTo performs (the settings
// page stages its adds into a save instead, see ListAppend): AppendListItem builds the element in
// memory and hands its literal here, and this writes that literal into the file.
//
// ⚠️ The element is NOT rebuilt from ListAppend's values here, and it cannot be: the two callers
// hold different things. AppendListItem has an element it already built (in memory, where a
// default-less field and a defaulted one are told apart) and this function has the literal for it;
// ApplyConfigEditsTo has the VALUES a form sent and no element at all. Both therefore go through
// the shared node-level helper (appendElementNodeToFile) and each verifies what it can: this one
// compares the re-read element to the literal it was given, ApplyConfigEditsTo compares each leaf
// it sent. Rebuilding here instead would be a second, subtly different rendering of the same
// element — the kind of drift that shows up as a new item whose fields the file does not have.
func AppendListItemTo(path, listPath, literal string) error {
	file, data, err := parseConfigFile(path)
	if err != nil {
		return err
	}
	if _, err := rootBlock(file); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if _, err := appendElementNodeToFile(file, ListAppend{List: listPath, Index: NoIndexClaim}, literal); err != nil {
		return err
	}
	out := []byte(file.String())

	// Verified the way an update is: re-parse what we are about to write and compare the element
	// we appended to the element we hold, as PARSED values on both sides so formatting cannot
	// make it a false failure. A silently failed append is the failure mode this guards (the
	// reason SavePartial re-reads at all).
	written, err := parseConfigBytes(out)
	if err != nil {
		return err
	}
	after, err := sequenceAt(written, listPath)
	if err != nil {
		return err
	}
	var got, want any
	if err := yaml.Unmarshal([]byte(after.Values[len(after.Values)-1].String()), &got); err != nil {
		return fmt.Errorf("append %s: re-read: %w", listPath, err)
	}
	if err := yaml.Unmarshal([]byte(literal), &want); err != nil {
		return fmt.Errorf("append %s: %w", listPath, err)
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("append %s: the item the file now holds (%v) is not the one written (%v)", listPath, got, want)
	}

	if string(data) == string(out) {
		return nil // nothing to write (an append that changed nothing cannot happen, but the rule is cheap)
	}
	return atomicWriteConfig(path, out)
}

// RemoveListItem removes the element at elementPath ("providers.2") from memory and returns what
// it held, which is what puts it back if the file write fails (RestoreListItem).
func RemoveListItem(cfg *Config, elementPath string) (held any, err error) {
	listPath, index, err := splitElementPath(elementPath)
	if err != nil {
		return nil, err
	}
	slice, err := listField(cfg, listPath)
	if err != nil {
		return nil, err
	}
	if index >= slice.Len() {
		return nil, fmt.Errorf("%s: 没有第 %d 项（共 %d 项）", listPath, index, slice.Len())
	}
	held = slice.Index(index).Interface()
	rest := reflect.MakeSlice(slice.Type(), 0, slice.Len()-1)
	rest = reflect.AppendSlice(rest, slice.Slice(0, index))
	rest = reflect.AppendSlice(rest, slice.Slice(index+1, slice.Len()))
	slice.Set(rest)
	return held, nil
}

// RestoreListItem puts a removed element back where it was: the undo of a file write that failed.
func RestoreListItem(cfg *Config, elementPath string, held any) error {
	listPath, index, err := splitElementPath(elementPath)
	if err != nil {
		return err
	}
	slice, err := listField(cfg, listPath)
	if err != nil {
		return err
	}
	if index > slice.Len() {
		return fmt.Errorf("%s: 放不回第 %d 项（现在只有 %d 项）", listPath, index, slice.Len())
	}
	back := reflect.MakeSlice(slice.Type(), 0, slice.Len()+1)
	back = reflect.AppendSlice(back, slice.Slice(0, index))
	back = reflect.Append(back, reflect.ValueOf(held))
	back = reflect.AppendSlice(back, slice.Slice(index, slice.Len()))
	slice.Set(back)
	return nil
}

// createListStructure builds the missing blocks of a list path in the file — `web_search` as a
// block, `providers` as a list inside it — and returns the sequence to append to.
//
// The list's block may not be in the file at all: a config that never configured search has no
// `web_search:` key, and "add the first one" is exactly when someone wants to. The path says where
// the block goes — the same no-guessing as ensureBlock's key (save_partial.go), which is the other
// half of this rule.
//
// Every node is built with a token whose COLUMN comes from the block it goes into (keyPosition),
// because that is what the emitter lays a mapping's lines out from; a node parsed out of a
// standalone snippet would carry positions pointing into its own buffer and print nonsense.
// Nothing here guesses: the caller named the path, and the path is the answer.
func createListStructure(file *ast.File, listPath string) (*ast.SequenceNode, error) {
	segs := strings.Split(listPath, ".")
	if len(segs) == 0 || segs[0] == "" {
		return nil, fmt.Errorf("%s: 不是一条路径", listPath)
	}
	root, err := rootBlock(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", listPath, err)
	}
	cur := root
	for i, seg := range segs {
		// The path may already exist partway down (web_search there, providers not).
		if p, err := yamlPath(strings.Join(segs[:i+1], ".")); err == nil {
			if node, err := p.FilterFile(file); err == nil {
				if i == len(segs)-1 {
					seq, ok := node.(*ast.SequenceNode)
					if !ok {
						return nil, fmt.Errorf("%s: 文件里这不是列表（是 %s）", listPath, node.Type())
					}
					return seq, nil
				}
				m, ok := node.(*ast.MappingNode)
				if !ok {
					return nil, fmt.Errorf("%s: 文件里的 %s 不是块（是 %s）",
						listPath, strings.Join(segs[:i+1], "."), node.Type())
				}
				cur = m
				continue
			}
		}
		pos, err := keyPosition(file, strings.Join(segs[:i+1], "."))
		if err != nil {
			return nil, err
		}
		if i == len(segs)-1 {
			// The start token is the encoder's own ("-"): the emitter reads a node's start
			// position from it, so a node built without one panics when the file is printed.
			seq := ast.Sequence(token.New("-", "-", pos), false)
			addKeyToBlock(cur, seg, seq, pos)
			return seq, nil
		}
		next := newBlockNode(pos)
		addKeyToBlock(cur, seg, next, pos)
		cur = next
	}
	return nil, fmt.Errorf("%s: 建不出这个列表", listPath)
}

// RemoveListItemFrom removes the element at elementPath from the file at path.
//
// The one-off entry point for the same operation ApplyConfigEditsTo performs (the settings page
// stages its removals into a save instead, see ListEdits.Removes); both do the work through the
// shared node-level helper (removeElementNode), which verifies by the SURVIVORS' identity.
func RemoveListItemFrom(path, elementPath string) error {
	file, data, err := parseConfigFile(path)
	if err != nil {
		return err
	}
	if err := removeElementNode(file, elementPath); err != nil {
		return err
	}
	out := []byte(file.String())
	if string(data) == string(out) {
		return nil
	}
	return atomicWriteConfig(path, out)
}

// ListLen reports how many elements the list at listPath holds. It is what a caller needs to say
// where a new element will land (the end), and therefore what a later write checks its own claim
// against — see ListAppend.Index.
func ListLen(cfg *Config, listPath string) (int, error) {
	slice, err := listField(cfg, listPath)
	if err != nil {
		return 0, err
	}
	return slice.Len(), nil
}

// ListItemLeaves reports the leaf values of one list element, as SavePartial's updates map is
// keyed: the element under its own path (`providers.2.name`). It is what a summary can name an
// item by, and what a caller checks references against before removing it.
func ListItemLeaves(cfg *Config, elementPath string) (map[string]any, error) {
	leaf, err := resolvePath(cfg, elementPath)
	if err != nil {
		return nil, err
	}
	var paths []string
	walkYAMLValue(leaf.value, "", &paths)
	out := make(map[string]any, len(paths))
	for _, rel := range paths {
		value, err := ReadPath(cfg, joinPath(elementPath, rel))
		if err != nil {
			return nil, err
		}
		out[rel] = value
	}
	return out, nil
}

// ListItemPaths reports every list the settings page may add items to: a SLICE OF STRUCTS reached
// through named fields only (`providers`, `web_search.providers`, `lsp.servers`).
//
// Two kinds of collection are deliberately not on it. A slice of scalars is ONE leaf to this
// package (the path API's own rule: `permissions.bash.allow` is edited as a list, and a form adds
// to it by typing a line, not by appending an item). A map is keyed by a NAME rather than by
// position (`hooks.events.pre_tool`), so "append at the end" has no meaning — the caller would
// have to invent a key, which is exactly the guessing this package refuses to do.
//
// The order is sorted, and the shape is the same vocabulary config.YAMLPaths uses for structure
// (`*` for a sequence element), except that a list path here names the list itself.
func ListItemPaths() []string {
	var out []string
	listPathsOfType(reflect.TypeOf(Config{}), "", &out)
	sort.Strings(out)
	return out
}

// listPathsOfType walks the config's declared shape, collecting the slices of structs.
func listPathsOfType(t reflect.Type, prefix string, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		for _, f := range yamlFields(t) {
			listPathsOfType(f.Type, joinPath(prefix, yamlName(f)), out)
		}
	case reflect.Slice, reflect.Array:
		elem := t.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() == reflect.Struct {
			*out = append(*out, prefix)
		}
	case reflect.Map:
		// Skipped on purpose: its items are keyed by name, not by position (see above).
	}
}

// ── shared plumbing ──────────────────────────────────────────────────────────

// listField resolves listPath to the slice field itself, refusing anything that is not a list of
// structs the caller can write to.
func listField(cfg *Config, listPath string) (reflect.Value, error) {
	leaf, err := resolvePath(cfg, listPath)
	if err != nil {
		return reflect.Value{}, err
	}
	v := leaf.value
	if v.Kind() != reflect.Slice {
		return reflect.Value{}, fmt.Errorf("%s: %s 不是列表，加不了条目", listPath, v.Type())
	}
	elem := v.Type().Elem()
	for elem.Kind() == reflect.Pointer {
		elem = elem.Elem()
	}
	if elem.Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("%s: %s 不是「条目」式的列表（元素得是一组字段）", listPath, v.Type())
	}
	if !v.CanSet() {
		return reflect.Value{}, fmt.Errorf("%s: 这个位置写不进去（只读）", listPath)
	}
	return v, nil
}

// SplitElementPath splits "providers.2" into the list's path and the index. The index must be the
// LAST segment: an element is addressed by its position and nothing else.
//
// Exported because a caller holding an element path it did not build itself has to take it apart the
// same way this package does — the settings page's staged items are paths (`providers.3`) that its
// own schema code has to resolve back to the list they belong to.
func SplitElementPath(elementPath string) (listPath string, index int, err error) {
	return splitElementPath(elementPath)
}

func splitElementPath(elementPath string) (listPath string, index int, err error) {
	at := strings.LastIndex(elementPath, ".")
	if at <= 0 || at == len(elementPath)-1 {
		return "", 0, fmt.Errorf("%s: 不是列表里的某一项（形如 providers.0）", elementPath)
	}
	index, err = strconv.Atoi(elementPath[at+1:])
	if err != nil || index < 0 {
		return "", 0, fmt.Errorf("%s: %q 不是下标", elementPath, elementPath[at+1:])
	}
	return elementPath[:at], index, nil
}

// parseConfigFile reads and parses config.yaml with its comments, which every edit here must
// preserve (the file is hand-written; see SavePartialTo).
func parseConfigFile(path string) (*ast.File, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	file, err := parseConfigBytes(data)
	if err != nil {
		return nil, nil, err
	}
	return file, data, nil
}

func parseConfigBytes(data []byte) (*ast.File, error) {
	file, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse config.yaml: %w", err)
	}
	return file, nil
}

// sequenceAt finds the sequence node at path, insisting it IS a sequence: a list path that names
// a block (or a scalar) is a caller bug, and appending to it would corrupt the file.
func sequenceAt(file *ast.File, path string) (*ast.SequenceNode, error) {
	p, err := yamlPath(path)
	if err != nil {
		return nil, err
	}
	node, err := p.FilterFile(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seq, ok := node.(*ast.SequenceNode)
	if !ok {
		return nil, fmt.Errorf("%s: 文件里这不是列表（是 %s）", path, node.Type())
	}
	return seq, nil
}

// renderedValues is a sequence's items as the emitter renders them, for comparing one parse of the
// file against another (both sides go through the same printer, so equal items are equal strings).
func renderedValues(seq *ast.SequenceNode) []string {
	out := make([]string, 0, len(seq.Values))
	for _, v := range seq.Values {
		out = append(out, strings.TrimSpace(v.String()))
	}
	return out
}

// atomicWriteConfig is SavePartial's write rule, unchanged: tmp + rename, and the mode the file
// already had (a user's 0600 must not become 0644).
func atomicWriteConfig(path string, out []byte) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return fileutil.AtomicWriteFile(path, out, 0o700, mode)
}
