package config

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Reading and writing ONE leaf of the in-memory config by the dot path SavePartial already
// speaks.
//
// Why a path API instead of more Set* methods: the desktop's settings table declares one
// string per field (`desktop/settings.go`), and forty hand-written accessors would be
// forgotten one by one. SavePartial owns the path syntax for the FILE side; this file owns
// the same syntax for the STRUCT side, so "providers.0.model" means one thing everywhere and
// an error message can name the key the caller actually typed.
//
// The two sides are deliberately not one implementation: the file is edited as YAML text
// (preserving comments and style), the struct is walked by reflection. What they share is the
// path grammar — and it is a grammar, not a convention: this file's rule for a segment is
// exactly yamlPath's (a numeric segment indexes a sequence; everything else names a key).

// ReadPath returns the value the config holds at key.
//
// Its callers are the settings schema (which shows the value) and SaveSettings (which
// snapshots it so a failed save can be rolled back), so two details are about what a READER
// sees rather than about the struct:
//
//   - a nil POINTER reads as the field's `default:` tag, parsed as the pointer's own type.
//     That is the value the config's accessors report (Hooks.IsEnabled, Oneoff.IsEnabled …),
//     and a form showing an empty box for "unset, therefore true" would misstate what is in
//     effect.
//   - a slice or map that is absent reads as its zero value (nil), not as an error: "no
//     servers configured" is a fact about the config, not a broken path.
//
// A path that does not exist — a misspelled segment, an index past the end, a key the map
// does not hold — is an error, because the caller asked for a specific leaf: reporting nil
// would look like "configured to nothing".
func ReadPath(cfg *Config, key string) (any, error) {
	leaf, err := resolvePath(cfg, key)
	if err != nil {
		return nil, err
	}
	if leaf.value.Kind() == reflect.Pointer {
		if leaf.value.IsNil() {
			if d, ok := leaf.defaultValue(); ok {
				return d, nil
			}
			return nil, nil // unset, and the field declares no default
		}
		return leaf.value.Elem().Interface(), nil
	}
	return leaf.value.Interface(), nil
}

// SetPath writes value into the in-memory config at key, doing exactly the half a runtime
// change needs before anything is told about it.
//
// Two rules, both of them the file side's rules, because memory and config.yaml must not be
// able to disagree about what was saved:
//
//   - the path must ALREADY EXIST (SavePartial only updates keys the file has; a path that
//     resolves to nothing here would be written to memory and then refused by the file, which
//     is the "already applied but not saved" state the design forbids);
//   - the value must fit the field's type, and a mismatch is an ERROR rather than a
//     conversion. The one conversion that IS accepted is the JSON shape a bound method
//     receives — numbers arrive as float64 and lists as []any — because that is the shape the
//     settings page sends; anything else is a bug worth hearing about.
//
// A pointer field takes the bare value and gets a fresh pointer, so a caller never has to
// spell out `&v`.
func SetPath(cfg *Config, key string, value any) error {
	leaf, err := resolvePath(cfg, key)
	if err != nil {
		return err
	}
	return assignValue(leaf, key, value)
}

// ValidateUpdates reports whether SavePartial would accept these values — the same type table
// the write uses, without touching the file. SaveSettings runs it BEFORE the config object is
// mutated, so a value the file would refuse cannot reach memory first (design §六's ordering:
// validate, then apply, then write).
//
// It says nothing about the FIELD types: those are SetPath's business, and a mismatch there
// is caught by ApplyUpdates' rollback.
func ValidateUpdates(updates map[string]any) error {
	for _, key := range sortedKeys(updates) {
		if _, err := renderScalar(updates[key]); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

// LeafValue is an opaque handle on one leaf's current value, for putting it back later.
type LeafValue struct {
	key   string
	field reflect.Value // the leaf itself, as resolved
	held  reflect.Value // a copy of what it held when the snapshot was taken
}

// Key names the path this handle covers, for an error message.
func (l LeafValue) Key() string { return l.key }

// SnapshotLeaves resolves several paths and copies what they currently hold, so a caller
// about to change them can put them back EXACTLY.
//
// ReadPath alone is not enough for that job, and the difference is the reason this exists: it
// substitutes a `default:` tag for an UNSET pointer, so restoring through it would turn
// "unset, therefore true" into an explicit `true` — a config change nobody asked for, written
// by the failure path.
func SnapshotLeaves(cfg *Config, keys []string) ([]LeafValue, error) {
	out := make([]LeafValue, 0, len(keys))
	for _, key := range keys {
		leaf, err := resolvePath(cfg, key)
		if err != nil {
			return nil, err
		}
		held := reflect.New(leaf.value.Type()).Elem()
		held.Set(leaf.value)
		out = append(out, LeafValue{key: key, field: leaf.value, held: held})
	}
	return out, nil
}

// RestoreLeaves puts a snapshot back. Best effort by construction: a leaf that could not be
// written could not have been changed either, so there is nothing to undo for it, and the one
// case that is skipped (a struct value inside a map) has no writer in the first place.
func RestoreLeaves(snap []LeafValue) {
	for _, s := range snap {
		if s.field.CanSet() {
			s.field.Set(s.held)
		}
	}
}

// ApplyUpdates writes several leaves in one pass, and puts every leaf it already wrote back if
// any of them fails — all or none, because a half-applied batch is a state no reader can
// reason about and the caller cannot describe to the user.
//
// It is the in-memory half of a save: SaveSettings calls it under the config lock, then tells
// the running parts, and only then writes the file.
func ApplyUpdates(cfg *Config, updates map[string]any) error {
	keys := sortedKeys(updates)
	snap, err := SnapshotLeaves(cfg, keys)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := SetPath(cfg, key, updates[key]); err != nil {
			RestoreLeaves(snap)
			return err
		}
	}
	return nil
}

// leafRef is a resolved path: the value itself, plus the struct field the LAST segment named
// (zero when the path ended on a slice element or a map value), which is where a default tag
// would live.
type leafRef struct {
	value reflect.Value
	field reflect.StructField
}

// defaultValue returns the field's `default:` tag parsed as the pointer's element type.
func (l leafRef) defaultValue() (any, bool) {
	if l.field.Type != l.value.Type() {
		return nil, false
	}
	raw := l.field.Tag.Get("default")
	if raw == "" {
		return nil, false
	}
	switch l.value.Type().Elem().Kind() {
	case reflect.Bool:
		if b, err := strconv.ParseBool(raw); err == nil {
			return b, true
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return int(n), true
		}
	case reflect.Float32, reflect.Float64:
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return f, true
		}
	case reflect.String:
		return raw, true
	}
	// A default the field's own type cannot hold (a size like "10mb" on a string field would
	// be fine, but "10mb" on an int is not) is not worth guessing at: report "no default" and
	// let the caller show the unset value.
	return nil, false
}

// resolvePath walks cfg to the value key names. Segments are split on "." and mean what
// yamlPath means by them: a numeric segment indexes a sequence, anything else names a struct
// field (by its yaml tag) or a map key.
//
// The struct field the LAST segment named travels back with the value, because that is where
// a `default:` tag lives (see defaultValue). A path that ended on a slice element or a map
// value carries none: the tag belongs to the field, not to the thing inside it.
func resolvePath(cfg *Config, key string) (leafRef, error) {
	if cfg == nil {
		return leafRef{}, errors.New("no config")
	}
	segs, err := pathSegments(key)
	if err != nil {
		return leafRef{}, err
	}
	cur := reflect.ValueOf(cfg)
	var last reflect.StructField
	for i, seg := range segs {
		for cur.Kind() == reflect.Pointer {
			if cur.IsNil() {
				return leafRef{}, fmt.Errorf("%s: 路径停在未设置的指针上", joinSegs(segs[:i]))
			}
			cur = cur.Elem()
		}
		switch cur.Kind() {
		case reflect.Struct:
			f, field, err := structFieldByYAMLName(cur, seg)
			if err != nil {
				return leafRef{}, fmt.Errorf("%s: %w", joinSegs(segs[:i+1]), err)
			}
			cur, last = f, field
		case reflect.Slice, reflect.Array:
			idx, err := strconv.Atoi(seg)
			if err != nil {
				return leafRef{}, fmt.Errorf("%s: 序列要下标，%q 不是数字", joinSegs(segs[:i+1]), seg)
			}
			if idx < 0 || idx >= cur.Len() {
				return leafRef{}, fmt.Errorf("%s: 下标越界（共 %d 项）", joinSegs(segs[:i+1]), cur.Len())
			}
			cur, last = cur.Index(idx), reflect.StructField{}
		case reflect.Map:
			kt := cur.Type().Key()
			if kt.Kind() != reflect.String {
				return leafRef{}, fmt.Errorf("%s: 只支持字符串键的 map", joinSegs(segs[:i+1]))
			}
			mv := cur.MapIndex(reflect.ValueOf(seg).Convert(kt))
			if !mv.IsValid() {
				return leafRef{}, fmt.Errorf("%s: map 里没有这个键", joinSegs(segs[:i+1]))
			}
			cur, last = mv, reflect.StructField{}
		default:
			return leafRef{}, fmt.Errorf("%s: 已经到底了，不能再往下走 %q", joinSegs(segs[:i]), seg)
		}
	}
	return leafRef{value: cur, field: last}, nil
}

// pathSegments splits a dot path, refusing an empty segment ("lsp..enabled" is a typo, not a
// request) and a leading/trailing dot.
func pathSegments(key string) ([]string, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("empty path")
	}
	segs := strings.Split(key, ".")
	for _, s := range segs {
		if s == "" {
			return nil, fmt.Errorf("invalid path %q: empty segment", key)
		}
	}
	return segs, nil
}

func joinSegs(segs []string) string { return strings.Join(segs, ".") }

// yamlName is the name a field is addressed by: its yaml tag, or the lowercased field name
// when it has none (what the loader itself would use).
func yamlName(f reflect.StructField) string {
	if tag, ok := f.Tag.Lookup("yaml"); ok {
		if name := strings.Split(tag, ",")[0]; name != "" {
			return name
		}
	}
	return strings.ToLower(f.Name)
}

// structFieldByYAMLName finds the field a segment names. `yaml:"-"` fields are not
// addressable by path — they are not keys of the file either (MCPServers is loaded from
// mcp.json), and a settings page that could write one would write something no reader of
// config.yaml would ever see.
func structFieldByYAMLName(v reflect.Value, name string) (reflect.Value, reflect.StructField, error) {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		if tag, ok := f.Tag.Lookup("yaml"); ok && strings.Split(tag, ",")[0] == "-" {
			continue
		}
		if yamlName(f) == name {
			return v.Field(i), f, nil
		}
	}
	return reflect.Value{}, reflect.StructField{}, fmt.Errorf("没有 %q 这个字段", name)
}

// assignValue writes src into the resolved leaf, accepting the shapes a bound method actually
// receives (JSON: float64 for numbers, []any for lists) and refusing everything else.
func assignValue(leaf leafRef, key string, src any) error {
	if src == nil {
		return fmt.Errorf("%s: nil 不是可保存的值（清空请用空字符串）", key)
	}
	dst := leaf.value
	if dst.Kind() == reflect.Pointer {
		if dst.Type().Elem().Kind() == reflect.Struct || dst.Type().Elem().Kind() == reflect.Slice {
			return fmt.Errorf("%s: %s 不是可以按路径写的叶子", key, dst.Type())
		}
		if dst.IsNil() {
			if !dst.CanSet() {
				return fmt.Errorf("%s: 这个位置写不进去（只读）", key)
			}
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		dst = dst.Elem()
	}
	if !dst.CanSet() {
		// A map value COPY, or a field inside one: reachable for reading, not for writing.
		// Note what this does NOT cover: `map[string][]T` — the slice header is copied but its
		// backing array is shared, so an element inside it IS settable and the write lands in
		// the caller's config (hooks.events is exactly that shape). Said out loud instead of
		// silently doing nothing.
		return fmt.Errorf("%s: 这个位置写不进去（map 值里的结构体/只读值）", key)
	}

	sv := reflect.ValueOf(src)
	switch dst.Kind() {
	case reflect.String:
		if sv.Kind() != reflect.String {
			return typeMismatch(key, dst, src)
		}
		dst.SetString(sv.String())
	case reflect.Bool:
		if sv.Kind() != reflect.Bool {
			return typeMismatch(key, dst, src)
		}
		dst.SetBool(sv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := asInt64(sv)
		if err != nil || dst.OverflowInt(n) {
			return typeMismatch(key, dst, src)
		}
		dst.SetInt(n)
	case reflect.Float32, reflect.Float64:
		f, err := asFloat64(sv)
		if err != nil {
			return typeMismatch(key, dst, src)
		}
		dst.SetFloat(f)
	case reflect.Slice:
		if dst.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("%s: %s 不是可以按路径写的叶子", key, dst.Type())
		}
		items, err := asStringSlice(sv)
		if err != nil {
			return typeMismatch(key, dst, src)
		}
		dst.Set(reflect.ValueOf(items))
	default:
		return fmt.Errorf("%s: %s 不是可以按路径写的叶子", key, dst.Type())
	}
	return nil
}

// asInt64 accepts the integer kinds and the float64 a JSON number arrives as, but only when
// it IS an integer: 1.5 saved into an int field would land in config.yaml as `1.5` and the
// next load would fail, so it is refused here instead.
func asInt64(v reflect.Value) (int64, error) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), nil
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if f != float64(int64(f)) {
			return 0, errors.New("not an integer")
		}
		return int64(f), nil
	}
	return 0, errors.New("not a number")
}

func asFloat64(v reflect.Value) (float64, error) {
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		return v.Float(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(v.Int()), nil
	}
	return 0, errors.New("not a number")
}

// asStringSlice accepts a []string and the []any a JSON list arrives as, requiring every
// element to be a string (a list holding a number is a form bug, not a value to coerce).
func asStringSlice(v reflect.Value) ([]string, error) {
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]string, 0, v.Len())
		for i := range v.Len() {
			el := v.Index(i)
			if el.Kind() == reflect.Interface {
				el = el.Elem()
			}
			if !el.IsValid() || el.Kind() != reflect.String {
				return nil, errors.New("element is not a string")
			}
			out = append(out, el.String())
		}
		return out, nil
	}
	return nil, errors.New("not a list")
}

func typeMismatch(key string, dst reflect.Value, src any) error {
	return fmt.Errorf("%s: 需要 %s，收到 %T", key, dst.Type(), src)
}
