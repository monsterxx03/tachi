package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/creasty/defaults"
	"github.com/monsterxx03/tachi/pkg/container"
	"github.com/monsterxx03/tachi/pkg/fileutil"
)

const mcpConfigFileName = "mcp.json"

// DefaultMCPServerTimeout is the per-server connect timeout applied when a
// server's "timeout" field is unset (mcp.json, duration string like "30s").
// Also the fallback for operations that batch-connect servers (e.g. MCP
// profile switches derive their overall budget from per-server timeouts).
const DefaultMCPServerTimeout = Duration(10 * time.Second)

// mcpConfigFile is the JSON structure for a single MCP config file.
type mcpConfigFile struct {
	Servers []MCPServerConfig `json:"servers"`
}

// mcpServerWithOrigin tracks a server and whether it came from a profile file.
type mcpServerWithOrigin struct {
	Server  MCPServerConfig
	Profile string // empty = base file, non-empty = profile file
	// Path is the file this entry was read from: the merge below keeps only the WINNER of a
	// name clash, and the winner's file is the one a write-back must target.
	Path string
}

// mcpConfigFileEntry describes a config file to load and its profile origin.
type mcpConfigFileEntry struct {
	Path    string
	Profile string // empty for base, profile name for profile-specific files
}

// LoadMCPConfig loads MCP server configuration from the global JSON files.
//
// Resolution order (the profile file overrides same-named servers from the base):
//  1. ~/.tachi/mcp.json                  (base)
//  2. ~/.tachi/mcp.{profile}.json        (profile, if profile set)
//
// There is no project scope: <tree>/.tachi/mcp.json is NOT read. One process — the
// desktop — serves several sessions in different trees over ONE shared connection
// layer (one pool, one connection per server), so "which tree am I in" cannot decide
// a global set: the old project scope could only ever be resolved by picking one tree
// to win. A server set belongs to the machine.
//
// A profile JSON file that doesn't exist is silently skipped.
// If no JSON files exist at all, returns nil, nil.
//
// Every server gets its Profile field stamped: servers from the base file get
// empty string, servers from the profile file get the activeProfile name.
func LoadMCPConfig(activeProfile string) ([]MCPServerConfig, error) {
	entries := mcpConfigEntries(activeProfile)

	var merged []mcpServerWithOrigin
	anyLoaded := false

	for _, entry := range entries {
		servers, err := loadMCPJSONFile(entry.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("loading %s: %w", entry.Path, err)
		}

		wrapped := make([]mcpServerWithOrigin, len(servers))
		for i, srv := range servers {
			wrapped[i] = mcpServerWithOrigin{Server: srv, Profile: entry.Profile, Path: entry.Path}
		}

		merged = mergeMCPServersWithOrigin(merged, wrapped)
		anyLoaded = true
	}

	if !anyLoaded {
		return nil, nil
	}

	return finalizeMCPServers(merged)
}

// mcpConfigEntries returns the ordered list of MCP JSON config files: the global
// base, then the global profile file when a profile is named.
func mcpConfigEntries(profile string) []mcpConfigFileEntry {
	dir := BaseDir()
	entries := []mcpConfigFileEntry{
		{Path: filepath.Join(dir, mcpConfigFileName), Profile: ""},
	}
	if profile != "" {
		entries = append(entries, mcpConfigFileEntry{
			Path:    filepath.Join(dir, mcpProfileFileName(profile)),
			Profile: profile,
		})
	}
	return entries
}

// mcpProfileFileName returns the profile-specific filename.
func mcpProfileFileName(profile string) string {
	return fmt.Sprintf("mcp.%s.json", profile)
}

// mcpProfileGlob matches profile files (mcp.<name>.json) but not the base
// file (mcp.json — the "*" requires at least one character).
const mcpProfileGlob = "mcp.*.json"

// ListMCPProfiles returns the names of available MCP profiles: the <name> part
// of every mcp.<name>.json in the global base dir, sorted. Returns nil when no
// profile files exist.
func ListMCPProfiles() []string {
	matches, err := filepath.Glob(filepath.Join(BaseDir(), mcpProfileGlob))
	if err != nil {
		return nil // malformed pattern — impossible with the constant above
	}

	seen := container.NewSet[string]()
	names := make([]string, 0, len(matches))
	for _, path := range matches {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "mcp."), ".json")
		if name == "" || seen.Has(name) {
			continue
		}
		seen.Add(name)
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// loadMCPJSONFile reads and parses a single MCP JSON config file.
func loadMCPJSONFile(path string) ([]MCPServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg mcpConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}

	return cfg.Servers, nil
}

// mergeMCPServersWithOrigin merges override into base. Servers in override
// with the same name replace those in base, taking the override's Profile.
func mergeMCPServersWithOrigin(base, override []mcpServerWithOrigin) []mcpServerWithOrigin {
	if len(override) == 0 {
		return base
	}
	if len(base) == 0 {
		return override
	}

	indexByName := make(map[string]int, len(base))
	for i, item := range base {
		indexByName[item.Server.Name] = i
	}

	result := make([]mcpServerWithOrigin, len(base))
	copy(result, base)

	for _, item := range override {
		if idx, exists := indexByName[item.Server.Name]; exists {
			result[idx] = item
		} else {
			result = append(result, item)
			indexByName[item.Server.Name] = len(result) - 1
		}
	}

	return result
}

// finalizeMCPServers converts merged origin-tracked servers to the final
// config, validates names, applies defaults, and stamps the Profile field.
func finalizeMCPServers(merged []mcpServerWithOrigin) ([]MCPServerConfig, error) {
	result := make([]MCPServerConfig, 0, len(merged))
	seen := container.NewSet[string]()

	for _, item := range merged {
		srv := item.Server
		srv.Profile = item.Profile
		srv.SourceFile = item.Path

		if srv.Name == "" {
			return nil, fmt.Errorf("mcp server has no name (profile=%q)", item.Profile)
		}
		if seen.Has(srv.Name) {
			return nil, fmt.Errorf("duplicate mcp server name %q (profile=%q)", srv.Name, item.Profile)
		}
		seen.Add(srv.Name)

		if srv.Timeout == 0 {
			srv.Timeout = DefaultMCPServerTimeout
		}

		if err := defaults.Set(&srv); err != nil {
			return nil, fmt.Errorf("server %q defaults: %w", srv.Name, err)
		}

		result = append(result, srv)
	}

	return result, nil
}

// --- writing back into a user-authored mcp.json -----------------------------
//
// The toggle in the desktop's MCP panel is a change to a file the USER wrote, so the edit is
// surgical: only the one boolean's bytes are replaced. Re-marshaling the file would be far
// less code, and it would also reorder every key, reflow every array and drop any field this
// struct does not model — turning a click into a diff on someone's config. What the edit
// needs from JSON is "where does this value start and end", which is a lexical question, so
// it is answered lexically below (no decoder: the offsets a decoder reports are only as
// exact as its buffering, and a byte out of place here corrupts a config file).

// SetServerEnabledInFile sets one server's "enabled" flag in the mcp.json it came from
// (MCPServerConfig.SourceFile — the file that won, when several define the same name).
//
// A server already carrying "enabled" has its value replaced; one without the key gets it
// appended to its entry (the key's absence means "enabled" — the defaults tag — and writing
// the choice down is the point of this function). Everything else in the file is untouched,
// byte for byte.
func SetServerEnabledInFile(path, name string, enabled bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := replaceServerEnabled(data, name, enabled)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if st, statErr := os.Stat(path); statErr == nil {
		mode = st.Mode().Perm()
	}
	return fileutil.AtomicWriteFile(path, out, 0o755, mode)
}

// errServerNotInFile is the "this file does not define that server" case, which is a normal
// outcome (the name may live in another mcp.json) and worth telling apart from a parse error.
var errServerNotInFile = errors.New("server not defined in this file")

func replaceServerEnabled(data []byte, name string, enabled bool) ([]byte, error) {
	objStart, objEnd, err := findServerObject(data, name)
	if err != nil {
		return nil, err
	}
	valStart, valEnd, found, err := objectFieldValue(data, objStart, "enabled")
	if err != nil {
		return nil, err
	}
	lit := []byte(strconv.FormatBool(enabled))
	if found {
		out := make([]byte, 0, len(data)+len(lit))
		out = append(out, data[:valStart]...)
		out = append(out, lit...)
		return append(out, data[valEnd:]...), nil
	}
	// No key yet: append it to the entry, on its own line and at the entry's own indentation
	// (taken from the entry itself, so a file indented with tabs or four spaces keeps its
	// style instead of getting this function's taste).
	indent := objectFieldIndent(data, objStart, objEnd)
	insert := make([]byte, 0, len(indent)+16)
	if objectHasFields(data, objStart) {
		insert = append(insert, ',')
	}
	insert = append(insert, '\n')
	insert = append(insert, indent...)
	insert = append(insert, `"enabled": `...)
	insert = append(insert, lit...)

	brace := objEnd - 1 // data[objEnd-1] is this object's closing '}'
	head := brace
	for head > objStart && isJSONSpace(data[head-1]) {
		head--
	}
	out := make([]byte, 0, len(data)+len(insert))
	out = append(out, data[:head]...)
	out = append(out, insert...)
	out = append(out, data[head:]...)
	return out, nil
}

// findServerObject returns the byte range of the entry named name inside the file's
// "servers" array.
func findServerObject(data []byte, name string) (start, end int, err error) {
	root := skipJSONSpace(data, 0)
	if root >= len(data) || data[root] != '{' {
		return 0, 0, errors.New("not a JSON object")
	}
	vs, _, found, err := objectFieldValue(data, root, "servers")
	if err != nil {
		return 0, 0, err
	}
	if !found {
		return 0, 0, errors.New(`no "servers" array`)
	}
	i := skipJSONSpace(data, vs)
	if i >= len(data) || data[i] != '[' {
		return 0, 0, errors.New(`"servers" is not an array`)
	}
	i++
	for {
		i = skipJSONSpace(data, i)
		if i >= len(data) {
			return 0, 0, errors.New(`"servers" array is not closed`)
		}
		switch data[i] {
		case ']':
			return 0, 0, errServerNotInFile
		case ',':
			i++
			continue
		case '{':
			e, err := jsonValueEnd(data, i)
			if err != nil {
				return 0, 0, err
			}
			if ns, ne, ok, err := objectFieldValue(data, i, "name"); err != nil {
				return 0, 0, err
			} else if ok {
				got, err := strconv.Unquote(string(data[ns:ne]))
				if err != nil {
					return 0, 0, fmt.Errorf("unreadable name: %w", err)
				}
				if got == name {
					return i, e, nil
				}
			}
			i = e
		default:
			return 0, 0, errors.New(`"servers" holds something that is not an object`)
		}
	}
}

// objectFieldValue returns the byte range of the value of key in the object starting at
// objStart (which must be '{'). Only the object's OWN keys are considered.
func objectFieldValue(data []byte, objStart int, key string) (start, end int, found bool, err error) {
	i := objStart + 1
	for {
		i = skipJSONSpace(data, i)
		if i >= len(data) {
			return 0, 0, false, errors.New("object is not closed")
		}
		switch data[i] {
		case '}':
			return 0, 0, false, nil
		case ',':
			i++
			continue
		case '"':
			keyEnd, err := jsonValueEnd(data, i)
			if err != nil {
				return 0, 0, false, err
			}
			got, err := strconv.Unquote(string(data[i:keyEnd]))
			if err != nil {
				return 0, 0, false, fmt.Errorf("unreadable key: %w", err)
			}
			i = skipJSONSpace(data, keyEnd)
			if i >= len(data) || data[i] != ':' {
				return 0, 0, false, fmt.Errorf("key %s has no value", got)
			}
			valStart := skipJSONSpace(data, i+1)
			valEnd, err := jsonValueEnd(data, valStart)
			if err != nil {
				return 0, 0, false, err
			}
			if got == key {
				return valStart, valEnd, true, nil
			}
			i = valEnd
		default:
			return 0, 0, false, errors.New("object holds a field name that is not a string")
		}
	}
}

// objectHasFields reports whether the object has at least one key (so an appended field needs
// a separating comma).
func objectHasFields(data []byte, objStart int) bool {
	i := skipJSONSpace(data, objStart+1)
	return i < len(data) && data[i] == '"'
}

// objectFieldIndent returns the indentation the object's own fields use, measured from its
// first field line; a single-line object falls back to one level deeper than its opening brace.
func objectFieldIndent(data []byte, objStart, objEnd int) string {
	lineStart := bytes.LastIndexByte(data[:objStart], '\n')
	braceIndent := objStart - (lineStart + 1)
	if i := bytes.IndexByte(data[objStart:objEnd], '\n'); i >= 0 {
		j := objStart + i + 1
		k := j
		for k < objEnd && (data[k] == ' ' || data[k] == '\t') {
			k++
		}
		if k > j {
			return string(data[j:k])
		}
	}
	return strings.Repeat(" ", braceIndent+2)
}

// jsonValueEnd returns the index just past the JSON value that starts at i (which must be the
// value's first byte). It validates only as much structure as it needs to find the end.
func jsonValueEnd(data []byte, i int) (int, error) {
	if i >= len(data) {
		return 0, errors.New("unexpected end of file")
	}
	switch data[i] {
	case '{', '[':
		open, close := data[i], byte('}')
		if open == '[' {
			close = ']'
		}
		depth := 0
		for ; i < len(data); i++ {
			switch data[i] {
			case '"':
				end, err := jsonStringEnd(data, i)
				if err != nil {
					return 0, err
				}
				i = end - 1
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					return i + 1, nil
				}
			}
		}
		return 0, errors.New("unterminated container")
	case '"':
		return jsonStringEnd(data, i)
	default:
		end := i
		for end < len(data) && !isJSONSpace(data[end]) &&
			data[end] != ',' && data[end] != '}' && data[end] != ']' {
			end++
		}
		if end == i {
			return 0, fmt.Errorf("unreadable value at byte %d", i)
		}
		return end, nil
	}
}

// jsonStringEnd returns the index just past the closing quote of the string starting at i.
func jsonStringEnd(data []byte, i int) (int, error) {
	for i++; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
		case '"':
			return i + 1, nil
		}
	}
	return 0, errors.New("unterminated string")
}

func skipJSONSpace(data []byte, i int) int {
	for i < len(data) && isJSONSpace(data[i]) {
		i++
	}
	return i
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
