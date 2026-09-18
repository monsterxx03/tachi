package config

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/creasty/defaults"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
)

// ONE save is ONE structural edit: the leaf values the form changed, the list items it added, and
// the list items it removed — applied in memory and written to config.yaml as a single operation.
//
// It exists because the settings page stopped being immediate about lists. Adding and removing used
// to be their own bound methods, each writing the file on the spot: an item had to BE in the file
// before any of its fields could be saved, so the structure had to move first. Staging them means
// a save can now be a mix — "add this provider AND fill in these values AND drop that one" — and
// three separate writes for one click would leave two windows where memory and the file disagree,
// which is the state this package refuses to be in.
//
// The ORDER is not a preference, it is what makes the edits address the right things:
//
//  1. APPENDS come first, and in the order the caller listed them. A new element's values are
//     written as part of the element itself: an update path cannot reach an element the file does
//     not have yet (that is `ensureBlock`'s own documented refusal), so an append carries its own
//     values rather than being followed by updates that name them.
//  2. UPDATES name elements that already existed, BY INDEX — so they must land before anything
//     shifts the indexes.
//  3. REMOVALS come last, for exactly that reason: a removal moves every later element up by one,
//     so a removal applied first would make every following index address the wrong item.
//
// ListEdits is the shape both halves take, which is what lets a caller do the memory half, the
// file half and the rollback of either without translating between three of them.

// NoIndexClaim says a ListAppend carries no expectation about where its element lands, for a
// caller that is not coordinating with a form (the one-off entry points). A claimed index is
// checked; an unclaimed one is not, because there is nothing to disagree with.
const NoIndexClaim = -1

// ListAppend is one element to add to a config list, at its END.
type ListAppend struct {
	// List is the list's own path (`web_search.providers`) — the same vocabulary
	// config.ListItemPaths writes.
	List string
	// Index is where the caller believes the element will land, or NoIndexClaim. It is CHECKED,
	// never trusted: the list grows at its end, so the answer is fixed by the list's current
	// length, and a caller whose count disagrees is working from a config that moved under it.
	// Refusing the batch is the only honest answer — the alternative is writing the item's values
	// into the wrong element, silently.
	Index int
	// Values are the element's own leaf values, keyed by the TAIL of the leaf inside the element
	// (`type`, `spec.proxy`): the shape a form can produce before the index is known. The index is
	// not part of the key because this struct carries it; a tail is what the caller knows without
	// it.
	Values map[string]any
}

// ListEdits is one save's structural half, as a value: what to change, what to add, what to drop.
//
// The zero value is "nothing" (IsEmpty), so a caller that has only leaf updates passes no appends
// and no removes and nothing structural happens.
type ListEdits struct {
	// Updates are leaf values on elements that already exist, by full path the way SavePartial
	// writes them (`providers.0.model`).
	Updates map[string]any
	// Appends are elements to add, each at the end of its own list.
	Appends []ListAppend
	// Removes are elements to drop, by their own path (`providers.2`).
	Removes []string
}

// IsEmpty reports whether this edit changes anything at all.
func (e ListEdits) IsEmpty() bool {
	return len(e.Updates) == 0 && len(e.Appends) == 0 && len(e.Removes) == 0
}

// Paths names every leaf this edit writes: the updates, plus the values each new element carries.
//
// It is the set a caller needs for what is per-LEAF rather than per-file — which settings rows must
// be pushed at the running process, and what a summary counts. A removal contributes nothing:
// dropping an element is a fact about the LIST, and the caller says so with the list's own path
// (there is no leaf left to name).
func (e ListEdits) Paths() []string {
	out := make([]string, 0, len(e.Updates))
	for key := range e.Updates {
		out = append(out, key)
	}
	for _, a := range e.Appends {
		for tail := range a.Values {
			out = append(out, elementLeafPath(a.List, a.Index, tail))
		}
	}
	return out
}

// elementLeafPath is one leaf inside an element: `providers.0.spec.proxy`.
func elementLeafPath(list string, index int, tail string) string {
	return fmt.Sprintf("%s.%d.%s", list, index, tail)
}

// updatesOutsideAppends is the updates that do NOT name a leaf inside an element being appended.
//
// Those travel WITH their element (ListAppend.Values): the element does not exist when an update is
// applied, so an update naming its leaf would either fail or have to CREATE the path — which is the
// guessing this package refuses. Both halves therefore work from this set, which is what keeps the
// memory and the file from disagreeing about what was written twice.
func (e ListEdits) updatesOutsideAppends() map[string]any {
	if len(e.Updates) == 0 {
		return nil
	}
	if len(e.Appends) == 0 {
		return e.Updates
	}
	out := make(map[string]any, len(e.Updates))
	for key, value := range e.Updates {
		if !e.insideAnAppend(key) {
			out[key] = value
		}
	}
	return out
}

// insideAnAppend reports whether a leaf path names a leaf inside one of this edit's new elements.
//
// It is the same PATH question `covers` answers for a settings row: this path starts with that
// element's path. An append that claims no index (NoIndexClaim) is skipped — with nothing to compare
// against, there is no prefix that means "this one", and a caller that makes no claim is not
// coordinating with a form anyway.
func (e ListEdits) insideAnAppend(key string) bool {
	for _, a := range e.Appends {
		if a.Index == NoIndexClaim {
			continue
		}
		if strings.HasPrefix(key, fmt.Sprintf("%s.%d.", a.List, a.Index)) {
			return true
		}
	}
	return false
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
	// 1. Appends, each with its own values.
	for _, a := range edits.Appends {
		if err := appendListElement(cfg, a); err != nil {
			return err
		}
	}
	// 2. Updates on the elements that were already there.
	if updates := edits.updatesOutsideAppends(); len(updates) > 0 {
		if err := ApplyUpdates(cfg, updates); err != nil {
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

// appendListElement adds one element to the end of its list, with the values the caller supplied,
// checking the index the caller claimed.
func appendListElement(cfg *Config, a ListAppend) error {
	slice, err := listField(cfg, a.List)
	if err != nil {
		return err
	}
	if a.Index != NoIndexClaim && a.Index != slice.Len() {
		return fmt.Errorf("%s: 以为新条目会落在第 %d 项，但列表现在有 %d 项"+
			"（页面上的列表和配置不一致，请重新打开设置页再改）", a.List, a.Index, slice.Len())
	}
	elem, err := newListElement(a.List, a.Values)
	if err != nil {
		return err
	}
	slice.Set(reflect.Append(slice, elem))
	return nil
}

// newListElement builds one element of the list at listPath: the type's own defaults first (a
// `web_search` provider starts at `type: exa`, so a new entry is a working starting point rather
// than a field of zeroes), then the caller's values on top.
//
// It is a standalone value, not yet in any slice, so a value the element's type refuses is
// reported without the config having been touched at all.
func newListElement(listPath string, values map[string]any) (reflect.Value, error) {
	elemType, err := listElemType(listPath)
	if err != nil {
		return reflect.Value{}, err
	}
	elem := reflect.New(elemType).Elem()
	if err := defaults.Set(elem.Addr().Interface()); err != nil {
		return reflect.Value{}, fmt.Errorf("%s: 默认值: %w", listPath, err)
	}
	for _, tail := range sortedKeys(values) {
		if err := setElementValue(elem, tail, values[tail]); err != nil {
			return reflect.Value{}, fmt.Errorf("%s: 新条目 %s: %w", listPath, tail, err)
		}
	}
	return elem, nil
}

// resolveElementPath walks a tail (`spec.proxy`) inside ONE element, which is a standalone value
// rather than a *Config: an element is resolved against itself, since a staged one is in no list.
//
// The grammar is resolvePath's (a segment names a yaml field, a pointer is followed once set) and
// so is the writer it feeds (assignValue), so a tail means here what it means everywhere else.
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

// setElementValue writes one leaf INSIDE an element, by the tail of its path (`spec.proxy`).
//
// An `Unset` value on an OPTIONAL pointer is a no-op, and that is the same fact stated twice rather
// than a special case: the leaves a form can say 自动 about are the optional ones, and those are
// exactly the fields whose yaml tag is `omitempty`. A freshly scaffolded element already holds nil
// for all of them, so "make this unset" and "leave the omitted field alone" are one state — and the
// element that reaches the file spells the field out only when it holds something.
//
// Anywhere else an `Unset` still goes to assignValue, which refuses it: the write that means "unset"
// has one gate (a pointer leaf), and this path must not be the second, quieter one.
func setElementValue(elem reflect.Value, tail string, value any) error {
	leaf, err := resolveElementPath(elem, tail)
	if err != nil {
		return err
	}
	if isUnset(value) && leaf.value.Kind() == reflect.Pointer {
		return nil
	}
	return assignValue(leaf, tail, value)
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
	scaffold, err := newListElement(listPath, nil)
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
// A type rather than a slice because a new element is built before it is anywhere: the file-side
// half has no *Config to resolve against at all (it edits text), and both halves must produce the
// same element for the file and the object to agree.
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
	for _, a := range edits.Appends {
		if seen[a.List] {
			continue
		}
		seen[a.List] = true
		slice, err := listField(cfg, a.List)
		if err != nil {
			return nil, err
		}
		snap.appends = append(snap.appends, listLength{list: a.List, len: slice.Len()})
	}
	if updates := edits.updatesOutsideAppends(); len(updates) > 0 {
		leaf, err := SnapshotLeaves(cfg, sortedKeys(updates))
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
//     ones — they are, an append's values are written as part of the element, never as an update
//     on it.
//  3. The LEAVES are restored last, and by RE-RESOLVING their paths rather than through the
//     handles the snapshot took. That is not a shortcut: `RemoveListItem` rebuilds a slice
//     (`reflect.MakeSlice`), so from that moment the old handles point at a backing array the
//     config no longer reads. Applying them in place would write the restored values into memory
//     nobody can see — the failure this test exists for. Re-resolving costs a walk and gives back
//     exactly what was held, including an UNSET pointer, because the snapshot stores the field
//     value itself rather than ReadPath's default-substituted answer.
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

// ApplyConfigEdits performs the edit on the config in effect — the write half of a save.
func ApplyConfigEdits(edits ListEdits) error {
	if edits.IsEmpty() {
		return nil
	}
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	return ApplyConfigEditsTo(path, edits)
}

// ── the file half ───────────────────────────────────────────────────────────

// ApplyConfigEditsTo performs the edit on config.yaml, in ONE write.
//
// One write is the point of this function. The three kinds used to be three separate
// read-modify-write passes, so a save that added an item and then failed to write a leaf left the
// item in the file with none of its values — a config nobody asked for, and one the memory half has
// no way to describe. Re-reading and re-parsing once makes the whole edit atomic in the same sense
// session metadata is: another process reading config.yaml sees the file before the edit or after
// it, never the middle.
func ApplyConfigEditsTo(path string, edits ListEdits) error {
	if edits.IsEmpty() {
		return nil
	}
	file, data, err := parseConfigFile(path)
	if err != nil {
		return err
	}
	// A file with no top-level block is refused up front, the way SavePartialTo refuses it:
	// goccy's ReplaceWithNode PANICS on a nil body rather than reporting the miss.
	if _, err := rootBlock(file); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	// 1. Appends, each element built from its type's defaults plus the caller's values. Where each
	// one LANDED is recorded rather than assumed: a caller that claimed no index (NoIndexClaim) has
	// one all the same, and it is the only thing that says which element to verify.
	landed := make([]int, 0, len(edits.Appends))
	for _, a := range edits.Appends {
		literal, err := elementLiteral(a)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		index, err := appendElementNodeToFile(file, a, literal)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		landed = append(landed, index)
	}

	// 2. Leaf updates on the elements that were already there. A key that names a leaf INSIDE a new
	// element is skipped: those values rode in with the element itself (see ListAppend.Values), and
	// applying them here would be a second write of the same leaf — one that also has to CREATE the
	// path, which is exactly the guessing this package refuses.
	if err := applyUpdatesToFile(file, edits.updatesOutsideAppends()); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	// The net under everything, and it has to happen HERE: goccy's ReplaceWithNode reports success
	// for a path it did not find while writing nothing, so nothing else can be trusted to have taken
	// effect — but the paths the caller sent are in the coordinates it was looking at, and step 3
	// shifts them. Verified any later, an update would be checked against whichever element moved
	// into its index (see TestApplyConfigEditsUpdatesBeforeRemoving).
	//
	// A re-parse rather than the in-memory nodes: an element inserted from a standalone literal
	// carries no indentation of its own (the emitter supplies it on the way out), so the only
	// stable comparison is against what the file will actually hold.
	if err := verifyUpdates([]byte(file.String()), edits.updatesOutsideAppends()); err != nil {
		return err
	}
	if err := verifyAppends([]byte(file.String()), edits.Appends, landed); err != nil {
		return err
	}

	// 3. Removals last (see ListEdits). Each is self-verifying, by the identity of the elements
	// that SURVIVE it.
	for _, elementPath := range edits.Removes {
		if err := removeElementNode(file, elementPath); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}

	out := []byte(file.String())
	// An edit that changed nothing must not rewrite the file: touching the mtime (or re-emitting
	// the user's bytes) for a save nobody changed anything in is rude at best.
	if string(data) == string(out) {
		return nil
	}
	return atomicWriteConfig(path, out)
}

// appendElementNodeToFile inserts one element at the END of its list in the parsed file, creating
// the list's blocks if the file has none (see createListStructure) and checking the index the
// caller claimed against what the file actually holds — the file half of ListAppend's check, and
// the same refusal: a caller working from a config that moved must be told, not guessed around.
//
// It returns the index the element LANDED at, which is the caller's claim when it made one and the
// list's own length when it did not — the only thing that says which element to verify afterwards.
//
// The node-level half of an append, shared by ApplyConfigEditsTo and AppendListItemTo so the two
// cannot drift (the token layout and the block creation are what goes wrong silently).
func appendElementNodeToFile(file *ast.File, a ListAppend, literal string) (int, error) {
	seq, err := sequenceAt(file, a.List)
	if err != nil {
		// The list's block may not be in the file at all: a config that never configured search
		// has no `web_search:` key, and "add the first one" is exactly when someone wants to. The
		// path says where the block goes, which is the same no-guessing as the item itself — the
		// reason an arbitrary new key is refused does not apply to a path the caller named.
		seq, err = createListStructure(file, a.List)
		if err != nil {
			return 0, err
		}
	}
	if a.Index != NoIndexClaim && len(seq.Values) != a.Index {
		return 0, fmt.Errorf("%s: 以为新条目会落在第 %d 项，但文件里有 %d 项"+
			"（页面上的列表和配置不一致，请重新打开设置页再改）", a.List, a.Index, len(seq.Values))
	}
	node, err := parseScalarNode(strings.TrimRight(literal, "\n"))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", a.List, err)
	}
	index := len(seq.Values)
	seq.Values = append(seq.Values, node)
	return index, nil
}

// elementLiteral renders one new element as the YAML text to insert: the element type's defaults,
// the caller's values on top, and this type's own field order — so a new block in a hand-written
// file looks like the ones already beside it.
func elementLiteral(a ListAppend) (string, error) {
	elem, err := newListElement(a.List, a.Values)
	if err != nil {
		return "", err
	}
	lit, err := yaml.Marshal(elem.Interface())
	if err != nil {
		return "", fmt.Errorf("%s: 渲染新条目: %w", a.List, err)
	}
	return string(lit), nil
}

// removeElementNode drops the element at elementPath from the parsed file, verified by the
// identity of the SURVIVORS: after the edit every remaining item must be the one that was beside
// it — the same item, in the same position. A removal that took the wrong one, or reflowed a
// neighbour, fails here.
func removeElementNode(file *ast.File, elementPath string) error {
	listPath, index, err := splitElementPath(elementPath)
	if err != nil {
		return err
	}
	// The baseline is a RE-PARSE of the document as printed, not the nodes in hand: an element that
	// came from a standalone literal carries no indentation of its own (the emitter supplies it on
	// the way out), so comparing an in-memory node against a re-parsed one reads as "a neighbour
	// changed" when nothing did.
	printed, err := parseConfigBytes([]byte(file.String()))
	if err != nil {
		return err
	}
	beforeSeq, err := sequenceAt(printed, listPath)
	if err != nil {
		return err
	}
	if index >= len(beforeSeq.Values) {
		return fmt.Errorf("%s: 第 %d 项不在文件里（共 %d 项）", listPath, index, len(beforeSeq.Values))
	}
	before := renderedValues(beforeSeq)

	seq, err := sequenceAt(file, listPath)
	if err != nil {
		return err
	}
	seq.Values = append(seq.Values[:index], seq.Values[index+1:]...)

	// Verified on a re-parse of THIS edit alone, before any other edit is laid on top: the check
	// needs the survivors' identity, and a later edit is free to change them.
	written, err := parseConfigBytes([]byte(file.String()))
	if err != nil {
		return err
	}
	after, err := sequenceAt(written, listPath)
	if err != nil {
		return err
	}
	kept := renderedValues(after)
	if len(kept) != len(before)-1 {
		return fmt.Errorf("remove %s: the file has %d items, expected %d", listPath, len(kept), len(before)-1)
	}
	for i, got := range kept {
		want := before[i]
		if i >= index {
			want = before[i+1]
		}
		if got != want {
			return fmt.Errorf("remove %s: item %d is %q after the edit, expected %q", listPath, i, got, want)
		}
	}
	return nil
}

// verifyAppends re-parses the bytes about to be written and confirms every new element really is
// where the caller claimed and really holds the values it sent.
//
// The values are compared by RENDERED FORM rather than by Go identity: the caller sends what a form
// produces (a float64 for a number, JSON strings) while the parse gives back what the YAML says (an
// int, a bool). Both sides go through renderScalar, so "4" and 4 are the same value here and a real
// difference still fails.
func verifyAppends(after []byte, appends []ListAppend, landed []int) error {
	if len(appends) == 0 {
		return nil
	}
	file, err := parseConfigBytes(after)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	for i, a := range appends {
		index := landed[i]
		seq, err := sequenceAt(file, a.List)
		if err != nil {
			return fmt.Errorf("verify %s: 改完之后文件里没有这个列表：%w", a.List, err)
		}
		if index >= len(seq.Values) {
			return fmt.Errorf("verify %s.%d: 新条目不在文件里（列表现在有 %d 项）",
				a.List, index, len(seq.Values))
		}
		var got map[string]any
		if err := yaml.Unmarshal([]byte(seq.Values[index].String()), &got); err != nil {
			return fmt.Errorf("verify %s.%d: 读回条目: %w", a.List, index, err)
		}
		for tail, want := range a.Values {
			held, err := readElementValue(got, tail)
			if err != nil {
				// An UNSET value with no key in the file is the RIGHT outcome, not a miss: the
				// optional leaves are the ones a form can say 自动 about, their yaml tag is
				// `omitempty`, and a nil pointer is therefore left out of the element altogether
				// (see setElementValue). Reading the file back gives nil for it, which is exactly
				// what was asked for.
				if isUnset(want) {
					continue
				}
				return fmt.Errorf("verify %s.%d.%s: %w", a.List, index, tail, err)
			}
			if isUnset(want) {
				continue
			}
			if !valueMatches(held, want) {
				return fmt.Errorf("verify %s.%d.%s: 文件里是 %v，写进去的是 %v（这次改动没有生效）",
					a.List, index, tail, held, want)
			}
		}
	}
	return nil
}

// readElementValue walks a tail (`spec.proxy`) inside a parsed element.
func readElementValue(m map[string]any, tail string) (any, error) {
	cur := any(m)
	for _, seg := range strings.Split(tail, ".") {
		asMap, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("读不到（%s 上面没有块）", seg)
		}
		next, ok := asMap[seg]
		if !ok {
			return nil, fmt.Errorf("读不到（文件里没有这个键）")
		}
		cur = next
	}
	return cur, nil
}

// valueMatches compares a value read back from the file against the one that was written.
func valueMatches(got, want any) bool {
	g, gErr := renderScalar(normalizeForCompare(got))
	w, wErr := renderScalar(normalizeForCompare(want))
	if gErr != nil || wErr != nil {
		return reflect.DeepEqual(got, want)
	}
	return g == w
}

// normalizeForCompare turns the parse's own types into the ones a form sends, so a YAML int and a
// JSON float64 are the same value.
func normalizeForCompare(v any) any {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	}
	return v
}
