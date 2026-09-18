package config

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// The set of leaves a settings form can address, in the two shapes the two callers need.
//
// One rule decides where a path STOPS, and it is the same rule the path API's value table uses:
// a slice or map whose element is a scalar is ONE leaf (as a flow list, which is how a hand-written
// config usually writes one), while a collection of structs is a collection of leaves, one per
// element (`providers.0.model`). Getting this wrong is not cosmetic: a form offering
// `allowed_tools.0` would write a path no leaf ever answers to, and one offering `providers` as a
// single field would show a whole struct in a text box.

// YAMLPaths returns every leaf path config.Config can have, with "*" standing for a sequence
// element or a map key (both unbounded, and neither known without a value).
//
// It is the STRUCTURAL shape, which is what a coverage question needs: "does every key this
// file can hold have a disposition?" must be answerable for a config whose provider list is
// empty, and only the type can answer that. The desktop's settings guard test is its caller.
func YAMLPaths() []string {
	var out []string
	walkYAMLType(reflect.TypeOf(Config{}), "", &out)
	sort.Strings(out)
	return out
}

// PathsFor returns the leaf paths THIS config can be asked about, with every wildcard already
// expanded into a concrete key: `providers.0.model`, `lsp.servers.1.command`,
// `hooks.events.pre_tool.0.command`.
//
// It is the shape a form needs — an input box cannot be labelled "*" — and its callers are
// the desktop's settings schema (which enumerates the fields) and any test that wants to know
// which keys a given config exposes. Empty collections contribute nothing: with no LSP servers
// configured there is no `lsp.servers.N.*` to edit, and the block's own leaves (`lsp.enabled`)
// are still there.
func PathsFor(cfg *Config) []string {
	var out []string
	walkYAMLValue(reflect.ValueOf(cfg), "", &out)
	sort.Strings(out)
	return out
}

// walkYAMLType collects the structural paths of a type.
func walkYAMLType(t reflect.Type, prefix string, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		if isScalarCollection(t) {
			*out = append(*out, prefix) // one leaf: the whole list is the value
			return
		}
		walkYAMLType(t.Elem(), prefix+".*", out)
	case reflect.Struct:
		for _, f := range yamlFields(t) {
			walkYAMLType(f.Type, joinPath(prefix, yamlName(f)), out)
		}
	default:
		*out = append(*out, prefix)
	}
}

// walkYAMLValue is walkYAMLType against a live value: the same rules, except that a
// collection of structs is expanded into its ACTUAL elements (and an empty one yields
// nothing).
func walkYAMLValue(v reflect.Value, prefix string, out *[]string) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.Kind() == reflect.Interface {
			*out = append(*out, prefix)
			return
		}
		if v.IsNil() {
			// A nil pointer to a SCALAR is still a leaf — it is the case the form most needs
			// (unset, therefore the default), and ReadPath reports that default. A nil pointer
			// to a struct hides everything under it, so nothing is emitted.
			elem := v.Type().Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if elem.Kind() != reflect.Struct {
				*out = append(*out, prefix)
			}
			return
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		if isScalarCollection(v.Type()) {
			*out = append(*out, prefix)
			return
		}
		for i := range v.Len() {
			walkYAMLValue(v.Index(i), joinPath(prefix, strconv.Itoa(i)), out)
		}
	case reflect.Map:
		if isScalarCollection(v.Type()) {
			*out = append(*out, prefix)
			return
		}
		for _, k := range v.MapKeys() {
			walkYAMLValue(v.MapIndex(k), joinPath(prefix, k.String()), out)
		}
	case reflect.Struct:
		for _, f := range yamlFields(v.Type()) {
			walkYAMLValue(v.FieldByIndex(f.Index), joinPath(prefix, yamlName(f)), out)
		}
	default:
		*out = append(*out, prefix)
	}
}

// isScalarCollection reports whether a collection is one leaf rather than a list of them: its
// element (or a pointer to it) is a scalar, so the whole thing renders as one YAML value.
func isScalarCollection(t reflect.Type) bool {
	elem := t.Elem()
	for elem.Kind() == reflect.Pointer {
		elem = elem.Elem()
	}
	switch elem.Kind() {
	case reflect.Struct, reflect.Slice, reflect.Map, reflect.Array:
		return false
	}
	return true
}

// yamlFields returns the fields a path can name, in declaration order: exported, and not
// skipped by `yaml:"-"` (those are not keys of the file at all — MCPServers is loaded from
// mcp.json).
func yamlFields(t reflect.Type) []reflect.StructField {
	var out []reflect.StructField
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		if tag, ok := f.Tag.Lookup("yaml"); ok && strings.Split(tag, ",")[0] == "-" {
			continue
		}
		out = append(out, f)
	}
	return out
}

func joinPath(prefix, seg string) string {
	if prefix == "" {
		return seg
	}
	return prefix + "." + seg
}
