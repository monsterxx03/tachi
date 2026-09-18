package config

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/creasty/defaults"
)

// The STRUCTURAL half of a config save: the list items the settings page added and the ones it
// dropped, applied to the in-memory config.
//
// It exists because "a value changed" and "an item appeared" are different edits, and the order
// between them is what makes the second addressable:
//
//  1. APPENDS come first. A new element's values are ordinary leaf updates like any other, but
//     they can only resolve once the element EXISTS — an update path cannot reach an element the
//     config does not have. Appending first is therefore not a preference; it is what makes
//     "add a provider and fill in its model" one batch.
//  2. UPDATES name elements that exist, BY INDEX — so they must land before anything shifts the
//     indexes.
//  3. REMOVALS come last, for exactly that reason.
//
// Both halves of a save speak this shape — the memory side here and the file side, which is now
// a single marshal of the same object (see Save) — so a caller cannot describe one without the
// other.

// ListEdits is one save's structural half, as a value: what to change, what to add, what to drop.
//
// The zero value is "nothing" (IsEmpty), so a caller with only leaf updates passes no appends and
// no removes and nothing structural happens.
type ListEdits struct {
	// Updates are leaf values, by full path (`providers.0.model`).
	Updates map[string]any
	// Appends names lists that each gain ONE element at their end (`providers`). A list named
	// twice gains two, which is what two presses of ＋ mean. The element is built from its type's
	// own defaults — a `web_search` provider starts at `type: exa`, so a new entry is a working
	// starting point rather than a field of zeroes — and its values arrive as ordinary updates,
	// which is why the order below matters and why nothing has to carry them.
	Appends []string
	// Removes are elements to drop, by their own path (`providers.2`).
	Removes []string
}

// IsEmpty reports whether this edit changes anything at all.
func (e ListEdits) IsEmpty() bool {
	return len(e.Updates) == 0 && len(e.Appends) == 0 && len(e.Removes) == 0
}

// ApplyListEdits performs the edit on the in-memory config, in the order documented above.
//
// All or nothing is the CALLER's job, and the pairing is like SaveSettings': snapshot first (see
// SnapshotListEdits), apply, and restore if the file write that follows fails. It is not done here
// because the failure that matters is the one AFTER this call — the file — and undoing inside
// would leave the caller holding a rollback it has already spent.
func ApplyListEdits(cfg *Config, edits ListEdits) error {
	if edits.IsEmpty() {
		return nil
	}
	// 1. Appends, each element starting from its type's defaults.
	for _, listPath := range edits.Appends {
		if err := appendListElement(cfg, listPath); err != nil {
			return err
		}
	}
	// 2. Updates on the elements that were already there.
	if len(edits.Updates) > 0 {
		if err := ApplyUpdates(cfg, edits.Updates); err != nil {
			return err
		}
	}
	// 3. Removals last: they shift the indexes, so nothing may address an element after them.
	for _, elementPath := range edits.Removes {
		if _, err := RemoveListItem(cfg, elementPath); err != nil {
			return err
		}
	}
	return nil
}

// appendListElement adds one element to the end of the list at listPath.
func appendListElement(cfg *Config, listPath string) error {
	slice, err := listField(cfg, listPath)
	if err != nil {
		return err
	}
	elem, err := newListElement(listPath)
	if err != nil {
		return err
	}
	slice.Set(reflect.Append(slice, elem))
	return nil
}

// newListElement builds one element of the list at listPath: the type's own defaults (a
// `web_search` provider starts at `type: exa`, so a new entry is a working starting point rather
// than a field of zeroes).
//
// It is a standalone value, not yet in any slice, so a failure leaves the config untouched.
func newListElement(listPath string) (reflect.Value, error) {
	elemType, err := listElemType(listPath)
	if err != nil {
		return reflect.Value{}, err
	}
	elem := reflect.New(elemType).Elem()
	if err := defaults.Set(elem.Addr().Interface()); err != nil {
		return reflect.Value{}, fmt.Errorf("%s: 默认值: %w", listPath, err)
	}
	return elem, nil
}

// resolveElementPath walks a tail (`spec.proxy`) inside ONE element, which is a standalone value
// rather than a *Config: an element is resolved against itself, since a staged one is in no list.
//
// The grammar is resolvePath's (a segment names a yaml field, a pointer is followed once set).
// ElementFields is its caller: describing a leaf of an element that does not exist yet is the one
// question a *Config cannot answer.
func resolveElementPath(elem reflect.Value, tail string) (leafRef, error) {
	segs, err := pathSegments(tail)
	if err != nil {
		return leafRef{}, err
	}
	cur := elem
	var last reflect.StructField
	for i, seg := range segs {
		for cur.Kind() == reflect.Pointer {
			if cur.IsNil() {
				if !cur.CanSet() {
					return leafRef{}, fmt.Errorf("%s: 路径停在未设置的指针上", joinSegs(segs[:i]))
				}
				cur.Set(reflect.New(cur.Type().Elem()))
			}
			cur = cur.Elem()
		}
		if cur.Kind() != reflect.Struct {
			return leafRef{}, fmt.Errorf("%s: 已经到底了，不能再往下走 %q", joinSegs(segs[:i]), seg)
		}
		f, field, err := structFieldByYAMLName(cur, seg)
		if err != nil {
			return leafRef{}, fmt.Errorf("%s: %w", joinSegs(segs[:i+1]), err)
		}
		cur, last = f, field
	}
	return leafRef{value: cur, field: last}, nil
}

// ElementField is one leaf of a list element, described without needing an element to exist: what a
// form needs to draw a field for an item that has just been staged (see ElementFields).
type ElementField struct {
	// Tail is the leaf's path INSIDE the element (`name`, `spec.proxy`) — the shape a caller can
	// address against an element whose index it names itself.
	Tail string
	// Kind, Optional and IsSet are LeafInfo's, read off the element's declared type.
	Kind     string
	Optional bool
	IsSet    bool
	// Value is what the element holds for this leaf before anyone edits it: the type's own default
	// (a `web_search` provider starts at `type: exa`), or nil for a leaf with neither a default nor
	// a value. It is what the form SHOWS, not a value the config has.
	Value any
}

// ElementFields describes every leaf an element of the list at listPath has, in the config's own
// declaration order.
//
// It exists for the one thing a form cannot ask the config about: an element that is STAGED but not
// written yet. Every other field a form draws comes from a value (PathsFor walks the live config),
// and a staged element has no value anywhere — which is the whole reason the settings page's list
// items used to have to be written to the file before they could be edited.
//
// The shape is the element TYPE's, and the values are a scaffold's: one element built with the
// type's defaults applied and no caller values, which is exactly what a new item will hold.
func ElementFields(listPath string) ([]ElementField, error) {
	elemType, err := listElemType(listPath)
	if err != nil {
		return nil, err
	}
	scaffold, err := newListElement(listPath)
	if err != nil {
		return nil, err
	}
	var paths []string
	walkYAMLType(elemType, "", &paths)
	out := make([]ElementField, 0, len(paths))
	for _, tail := range paths {
		// A `*` segment is a NESTED collection (`spec.pricing.bands.*`): not a leaf a form writes
		// as one value, and empty on a new element — which is exactly what PathsFor reports for an
		// existing element whose nested list is empty, so skipping them keeps the two in step.
		if strings.Contains(tail, ".*") || tail == "*" {
			continue
		}
		leaf, err := resolveElementPath(scaffold, tail)
		if err != nil {
			return nil, err
		}
		info := describeLeafRef(leaf)
		out = append(out, ElementField{
			Tail: tail, Kind: info.Kind, Optional: info.Optional, IsSet: info.IsSet,
			Value: readLeafRef(leaf),
		})
	}
	return out, nil
}

// listElemType resolves a list path to the STRUCT type of its elements, walking the config's
// DECLARED shape (reflect.TypeOf(Config{})) rather than a value.
//
// A type rather than a slice because a new element is built before it is anywhere: the question
// "what does an item of this list look like" has to be answerable for a list that is empty, which
// is exactly when someone wants the first item.
func listElemType(listPath string) (reflect.Type, error) {
	t := reflect.TypeOf(Config{})
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for _, seg := range strings.Split(listPath, ".") {
		if t.Kind() != reflect.Struct {
			return nil, fmt.Errorf("%s: 路径停在 %s 上，不是块", listPath, t.Kind())
		}
		var found bool
		for i := range t.NumField() {
			f := t.Field(i)
			if f.PkgPath != "" || yamlName(f) != seg {
				continue
			}
			t, found = f.Type, true
			break
		}
		if !found {
			return nil, fmt.Errorf("%s: 没有 %q 这个字段", listPath, seg)
		}
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
	}
	if t.Kind() != reflect.Slice {
		return nil, fmt.Errorf("%s: 不是列表", listPath)
	}
	elem := t.Elem()
	for elem.Kind() == reflect.Pointer {
		elem = elem.Elem()
	}
	if elem.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%s: 不是「条目」式的列表（元素得是一组字段）", listPath)
	}
	return elem, nil
}

// ── the rollback half ───────────────────────────────────────────────────────

// ListEditSnapshot is what puts a config object back after an edit it must not keep — the memory
// half of a save whose FILE write failed.
//
// It is a snapshot rather than a reload of the file on purpose: reloading restores the values the
// file holds, but it would also clobber the few things this process owns in memory and never
// writes there, which is a bigger change than the one being undone.
type ListEditSnapshot struct {
	// appends are the affected lists' lengths BEFORE the edit: putting a list back is truncating it
	// to the length it had. Per list rather than one number, because one save can grow two.
	appends []listLength
	// leaves is the leaf half — SnapshotLeaves' own handles, which know how to put a leaf back
	// EXACTLY, including an unset pointer that a re-read through ReadPath could not restore.
	leaves []LeafValue
	// removes is what each removed element held, so it can go back at its own index.
	removes []removedElement
}

type listLength struct {
	list string
	len  int
}

type removedElement struct {
	path string
	held any
}

// SnapshotListEdits records what it takes to undo this edit. It must be taken BEFORE the edit is
// applied, and by the same goroutine that applies it.
func SnapshotListEdits(cfg *Config, edits ListEdits) (*ListEditSnapshot, error) {
	snap := &ListEditSnapshot{}
	seen := make(map[string]bool, len(edits.Appends))
	for _, listPath := range edits.Appends {
		if seen[listPath] {
			continue
		}
		seen[listPath] = true
		slice, err := listField(cfg, listPath)
		if err != nil {
			return nil, err
		}
		snap.appends = append(snap.appends, listLength{list: listPath, len: slice.Len()})
	}
	if len(edits.Updates) > 0 {
		// A leaf the edit will CREATE — one inside an element this same batch appends — is skipped:
		// it does not resolve yet, and it does not need recording, because putting the list back to
		// its old length takes the whole element (and every leaf on it) with it. That is the one
		// case where a path a caller sent is legitimately absent, and the appends below are why.
		var resolvable []string
		for _, key := range sortedKeys(edits.Updates) {
			if _, err := resolvePath(cfg, key); err == nil {
				resolvable = append(resolvable, key)
			}
		}
		leaf, err := SnapshotLeaves(cfg, resolvable)
		if err != nil {
			return nil, err
		}
		snap.leaves = leaf
	}
	for _, path := range edits.Removes {
		held, err := heldElement(cfg, path)
		if err != nil {
			return nil, err
		}
		snap.removes = append(snap.removes, removedElement{path: path, held: held})
	}
	return snap, nil
}

// heldElement is what the element at elementPath currently holds — the value RemoveListItem
// reports and RestoreListItem takes back.
func heldElement(cfg *Config, elementPath string) (any, error) {
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
	return slice.Index(index).Interface(), nil
}

// RestoreListEdits puts the config object back the way SnapshotListEdits found it.
//
// The ORDER is what makes it exact, and it is not the reverse of ApplyListEdits' by accident:
//
//  1. The removals go back first, at their own indices (which are only correct while every earlier
//     element is still there — it is, nothing between the snapshot and here removed one).
//  2. The appends are truncated next, which is exact while the elements are still the appended
//     ones — they are, an append's element is only ever filled in by ordinary updates on it.
//  3. The LEAVES are restored last, and by RE-RESOLVING their paths rather than through the
//     handles the snapshot took. That is not a shortcut: `RemoveListItem` rebuilds a slice
//     (`reflect.MakeSlice`), so from that moment the old handles point at a backing array the
//     config no longer reads. Applying them in place would write the restored values into memory
//     nobody can see — the failure this ordering exists for. Re-resolving costs a walk and gives
//     back exactly what was held, including an UNSET pointer, because the snapshot stores the
//     field value itself rather than ReadPath's default-substituted answer.
func RestoreListEdits(cfg *Config, snap *ListEditSnapshot) {
	if snap == nil {
		return
	}
	for _, r := range snap.removes {
		_ = RestoreListItem(cfg, r.path, r.held)
	}
	for _, l := range snap.appends {
		slice, err := listField(cfg, l.list)
		if err != nil {
			continue
		}
		if slice.Len() > l.len {
			slice.Set(slice.Slice(0, l.len))
		}
	}
	for _, leaf := range snap.leaves {
		ref, err := resolvePath(cfg, leaf.key)
		if err != nil || !ref.value.CanSet() {
			continue
		}
		ref.value.Set(leaf.held)
	}
}
