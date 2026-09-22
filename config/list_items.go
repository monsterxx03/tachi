package config

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// The lists a settings page may grow: which ones they are, where an element lands, what one
// element holds, and how an element comes back out.
//
// This is the MEMORY side of a list edit. A list item is its own entry point rather than "a leaf
// on a path", because a path does not answer everything about it: a new element's other fields
// need a skeleton (the element type's own defaults), and there is exactly ONE place it can go
// (the end). ListEdits is what a caller uses to say all of that at once (see config_edits.go);
// this file is the plumbing underneath it.

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

// ListLen reports how many elements the list at listPath holds. It is what tells a reader where a
// new element will land (the end), and therefore what lets a form name the item it is about to add
// (the settings page gets it as SettingListVO.Length, and the append checks it again at save time —
// see ListEdits.Appends).
func ListLen(cfg *Config, listPath string) (int, error) {
	slice, err := listField(cfg, listPath)
	if err != nil {
		return 0, err
	}
	return slice.Len(), nil
}

// ListItemLeaves reports the leaf values of one list element, keyed the way the path API keys
// everything: the element under its own path (`providers.2.name`). It is what a summary can name
// an item by, and what a caller checks references against before removing it.
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
// through named fields only (`providers`, `web_search.providers`, `hooks.events.pre_tool`).
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
// same way this package does — a form addresses its own unsaved item as a path (`providers.3`), and
// the save has to resolve it back to the list it belongs to.
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
