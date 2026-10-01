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
	merged, err := loadMCPMerged(activeProfile)
	if err != nil || merged == nil {
		return nil, err
	}
	return finalizeMCPServers(merged)
}

// LoadMCPConfigRaw is LoadMCPConfig without the finalize step: the same merge, but every server
// is exactly what its file says.
//
// It exists for the EDITOR, not for the runtime. Defaults (the 10s timeout, `enabled: true`, the
// OAuth callback host) are applied by finalizeMCPServers, and a form that read the finalized
// value would write those defaults back into a hand-written file the moment a server was
// re-saved — turning "I opened this entry and pressed save" into a diff of settings the user
// never chose. Reading raw keeps "absent" absent.
func LoadMCPConfigRaw(activeProfile string) ([]MCPServerConfig, error) {
	merged, err := loadMCPMerged(activeProfile)
	if err != nil || merged == nil {
		return nil, err
	}
	out := make([]MCPServerConfig, 0, len(merged))
	for _, item := range merged {
		srv := item.Server
		srv.Profile = item.Profile
		srv.SourceFile = item.Path
		out = append(out, srv)
	}
	return out, nil
}

// loadMCPMerged reads the config files in order and merges them by server name, with each
// server's origin (which file, which profile) kept alongside it. nil when no file exists.
func loadMCPMerged(activeProfile string) ([]mcpServerWithOrigin, error) {
	var merged []mcpServerWithOrigin
	anyLoaded := false

	for _, entry := range mcpConfigEntries(activeProfile) {
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
	return merged, nil
}

// MCPConfigPath is the file a profile's servers live in: the base file for the empty profile,
// the profile's own file otherwise. It is the ONE mapping from a profile name to a path — the
// loader's entry list and every write must go through it, or the editor would write a server
// into a file the loader never reads.
func MCPConfigPath(profile string) string {
	if profile == "" {
		return filepath.Join(BaseDir(), mcpConfigFileName)
	}
	return filepath.Join(BaseDir(), mcpProfileFileName(profile))
}

// mcpConfigEntries returns the ordered list of MCP JSON config files: the global
// base, then the global profile file when a profile is named.
func mcpConfigEntries(profile string) []mcpConfigFileEntry {
	entries := []mcpConfigFileEntry{{Path: MCPConfigPath(""), Profile: ""}}
	if profile != "" {
		entries = append(entries, mcpConfigFileEntry{
			Path:    MCPConfigPath(profile),
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
	return writeMCPFile(path, out)
}

// writeMCPFile rewrites an mcp.json, keeping the permissions it already had (a config the user
// made readable only to themselves must not come back world-readable). A file this package
// CREATES gets 0600 rather than the usual 0644: `env` and `headers` carry tokens, and a brand-new
// file has no mode of its own to keep.
func writeMCPFile(path string, out []byte) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return fileutil.AtomicWriteFile(path, out, 0o755, mode)
}

// ErrServerNotInFile is the "this file does not define that server" case, which is a normal
// outcome (the name may live in another mcp.json) and worth telling apart from a parse error.
var ErrServerNotInFile = errors.New("server not defined in this file")

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
	return insertObjectField(data, objStart, objEnd, "enabled", lit), nil
}

// insertObjectField adds `"key": value` to the object at objStart..objEnd, on its own line at
// the object's own indentation (see objectFieldIndent). The caller must have checked that the
// key is not already there — this only inserts.
//
// `value` is raw JSON, so the caller decides the shape (a bool literal, an array, an object).
func insertObjectField(data []byte, objStart, objEnd int, key string, value []byte) []byte {
	indent := objectFieldIndent(data, objStart, objEnd)
	insert := make([]byte, 0, len(indent)+len(key)+len(value)+8)
	if objectHasFields(data, objStart) {
		insert = append(insert, ',')
	}
	insert = append(insert, '\n')
	insert = append(insert, indent...)
	insert = append(insert, '"')
	insert = append(insert, key...)
	insert = append(insert, `": `...)
	insert = append(insert, value...)

	// The closing brace, with any whitespace that precedes it, is where the new field goes.
	head := objEnd - 1
	for head > objStart && isJSONSpace(data[head-1]) {
		head--
	}
	// A value that spans lines ends on a line of its own — but the brace closing the object is
	// glued to whatever precedes it when the object was written on one line (`{}`, `{"a": 1}`),
	// and gluing a multi-line array to it (`  ]}`) is a shape no JSON writer produces. Such an
	// object had no layout of its own to keep, so the brace gets the one its fields have. A
	// single-line value leaves this alone, which is every caller that replaces a scalar.
	tail := ""
	if bytes.Contains(value, []byte("\n")) && !bytes.Contains(data[head:objEnd], []byte("\n")) {
		tail = "\n" + lineIndent(data, objStart)
	}

	out := make([]byte, 0, len(data)+len(insert)+len(tail))
	out = append(out, data[:head]...)
	out = append(out, insert...)
	out = append(out, tail...)
	return append(out, data[head:]...)
}

// --- adding, editing and removing whole entries -----------------------------
//
// The panel's toggle changes one VALUE; the settings editor changes entries, which is a bigger
// promise and is paid for the same way: the edit is placed by byte range, and everything it does
// not name stays exactly as the user wrote it. An entry that IS named is rendered by this
// package — a form re-states the whole server, so its formatting is ours to choose — but the
// file's own indentation is measured first and reused, so the result still reads like its
// neighbours rather than like a machine's output.

// UpsertServerInFile writes srv into the mcp.json at path: the entry of the same name is
// replaced, or a new one is appended to the "servers" array. A file (or a "servers" key) that
// does not exist yet is created.
//
// Every other byte of the file is left alone — other entries keep their formatting, their key
// order and any field this package does not model.
//
// A server with no name is refused HERE, at the only door a nameless entry can come through: the
// loader rejects such a file outright (finalizeMCPServers), and that failure reaches the process
// as a failed Bootstrap — no TUI, no desktop config — so it would take a hand edit to undo.
func UpsertServerInFile(path string, srv MCPServerConfig) error {
	if srv.Name == "" {
		return errors.New("mcp server has no name")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return writeMCPFile(path, newMCPFile(srv))
	}
	out, err := upsertServer(data, srv)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return writeMCPFile(path, out)
}

// DeleteServerInFile removes the entry named name from the mcp.json at path. The file stays
// (with an empty "servers" array) when that was its only server.
//
// Returns ErrServerNotInFile when this file does not define that server, which is a normal
// outcome: the name may live in the other file.
func DeleteServerInFile(path, name string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := removeServer(data, name)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return writeMCPFile(path, out)
}

// newMCPFile is a whole mcp.json holding one server: what a first save writes when the file is
// not there yet.
func newMCPFile(srv MCPServerConfig) []byte {
	data, err := json.MarshalIndent(mcpConfigFile{Servers: []MCPServerConfig{srv}}, "", "  ")
	if err != nil {
		return []byte("{\n  \"servers\": []\n}\n") // unreachable: MCPServerConfig always marshals
	}
	return append(data, '\n')
}

func upsertServer(data []byte, srv MCPServerConfig) ([]byte, error) {
	objStart, objEnd, err := findServerObject(data, srv.Name)
	// Either "the file has no entry with this name" or "the file has no servers array at all":
	// both are the append case, and the second one is a file this package is about to give a
	// servers key to (a hand-written mcp.json can be plain `{}`).
	if errors.Is(err, ErrServerNotInFile) || errors.Is(err, errNoServersArray) {
		return appendServer(data, srv)
	}
	if err != nil {
		return nil, err
	}
	// The entry's own layout decides the new one's: a file that lays its entries out across
	// lines keeps doing that, and a minified one stays on one line.
	pretty := bytes.Contains(data[objStart:objEnd], []byte("\n"))
	entry, err := renderServerEntry(srv, pretty, lineIndent(data, objStart), indentUnit(data, objStart, objEnd))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(data)-(objEnd-objStart)+len(entry))
	out = append(out, data[:objStart]...)
	out = append(out, entry...)
	return append(out, data[objEnd:]...), nil
}

// appendServer adds srv to the end of the "servers" array, creating the key when the file has
// none (a hand-written mcp.json can be `{"servers": []}` or, for a server added before this
// existed, plain `{}`).
func appendServer(data []byte, srv MCPServerConfig) ([]byte, error) {
	start, end, err := serversArrayRange(data)
	if errors.Is(err, errNoServersArray) {
		return addServersArray(data, srv)
	}
	if err != nil {
		return nil, err
	}

	elements, err := serverObjects(data, start, end)
	if err != nil {
		return nil, err
	}
	// The ARRAY's own layout decides where it has one: an array written one element per line
	// gets an entry across lines, and an array written on a single line gets a one-liner —
	// judged from the FILE it got this wrong for the ordinary `"servers": [{…}]` inside an
	// otherwise pretty file, and then also handed indentUnit a single-line object to measure
	// from (which has no step: it answered with the brace's column).
	unit := "  "
	pretty := bytes.Contains(data, []byte("\n"))
	if len(elements) > 0 {
		pretty = aloneOnLine(data, elements[0].start)
		if pretty {
			unit = indentUnit(data, elements[0].start, elements[0].end)
		}
	}

	if len(elements) == 0 {
		// An empty array is replaced whole: there is nothing inside it to preserve, and the
		// array's own closing bracket has to line up with the key it belongs to.
		var arr []byte
		if pretty {
			arr, err = json.MarshalIndent([]MCPServerConfig{srv}, lineIndent(data, start), unit)
		} else {
			arr, err = json.Marshal([]MCPServerConfig{srv})
		}
		if err != nil {
			return nil, err
		}
		out := make([]byte, 0, len(data)-(end-start)+len(arr))
		out = append(out, data[:start]...)
		out = append(out, arr...)
		return append(out, data[end:]...), nil
	}

	// Inserted at the END of the last element, so the whitespace (and the closing bracket) that
	// already follow it stay where they are.
	indent := lineIndent(data, elements[0].start)
	var entry []byte
	if pretty {
		body, merr := renderServerEntry(srv, true, indent, unit)
		if merr != nil {
			return nil, merr
		}
		entry = append([]byte{',', '\n'}, indent...)
		entry = append(entry, body...)
	} else {
		body, merr := json.Marshal(srv)
		if merr != nil {
			return nil, merr
		}
		entry = append([]byte{','}, body...)
	}

	at := elements[len(elements)-1].end
	out := make([]byte, 0, len(data)+len(entry))
	out = append(out, data[:at]...)
	out = append(out, entry...)
	return append(out, data[at:]...), nil
}

// addServersArray writes the "servers" key into a file that has none.
func addServersArray(data []byte, srv MCPServerConfig) ([]byte, error) {
	root := skipJSONSpace(data, 0)
	if root >= len(data) || data[root] != '{' {
		return nil, errors.New("not a JSON object")
	}
	end, err := jsonValueEnd(data, root)
	if err != nil {
		return nil, err
	}
	arr, err := json.MarshalIndent([]MCPServerConfig{srv}, objectFieldIndent(data, root, end), "  ")
	if err != nil {
		return nil, err
	}
	return insertObjectField(data, root, end, "servers", arr), nil
}

// removeServer deletes one entry, taking the comma that separated it with it.
func removeServer(data []byte, name string) ([]byte, error) {
	objStart, objEnd, err := findServerObject(data, name)
	if err != nil {
		return nil, err
	}
	start, end := objStart, objEnd
	if next := skipJSONSpace(data, objEnd); next < len(data) && data[next] == ',' {
		// The entry is not the last one: drop it and the comma AFTER it. When the entry had a
		// line to itself, its whole line goes — including the line break that ends it, or
		// deleting a middle entry leaves the blank line it was standing on.
		start, end = objStart, next+1
		if aloneOnLine(data, objStart) {
			start = lineStart(data, objStart)
			i := end
			for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\r') {
				i++
			}
			if i < len(data) && data[i] == '\n' {
				end = i + 1
			}
		}
		out := make([]byte, 0, len(data)-(end-start))
		out = append(out, data[:start]...)
		out = append(out, data[end:]...)
		return collapseEmptyServers(out)
	}
	if prev := lastNonSpace(data, objStart); prev >= 0 && data[prev] == ',' {
		// The last one: the comma BEFORE it is the one that has to go.
		start = prev
	}
	out := make([]byte, 0, len(data)-(end-start))
	out = append(out, data[:start]...)
	out = append(out, data[end:]...)
	return collapseEmptyServers(out)
}

// collapseEmptyServers turns an array that just lost its last element — `[\n\n  ]`, the
// whitespace of the entries that used to be there — back into the `[]` it should be.
func collapseEmptyServers(data []byte) ([]byte, error) {
	start, end, err := serversArrayRange(data)
	if err != nil {
		return data, nil // not our shape any more; leave the bytes as they are
	}
	if !isAllJSONSpace(data[start+1 : end-1]) {
		return data, nil
	}
	out := make([]byte, 0, len(data)-(end-start)+2)
	out = append(out, data[:start]...)
	out = append(out, '[', ']')
	return append(out, data[end:]...), nil
}

// renderServerEntry renders one server as the JSON object to place at an entry's position.
// `pretty` says the target lays its entries out across lines; `prefix` is the indentation the
// entry's own line carries (MarshalIndent repeats it on every line after the first, so the
// entry lines up with the one it replaces) and `unit` the file's indentation step.
func renderServerEntry(srv MCPServerConfig, pretty bool, prefix, unit string) ([]byte, error) {
	if !pretty {
		return json.Marshal(srv)
	}
	return json.MarshalIndent(srv, prefix, unit)
}

// indentUnit measures the file's indentation step from an object's opening line and its first
// field. Only an object that SPANS lines has a step to measure: objectFieldIndent falls back to
// the opening brace's COLUMN plus two for a one-line object, and that is not a step — answering
// with it indented the next entry by a column count (measured: a one-line entry at column 14
// made the new entry's fields come out fifteen columns too deep). Anything it cannot read
// confidently (mixed whitespace) falls back to two spaces, which is what a plain JSON writer
// produces.
func indentUnit(data []byte, objStart, objEnd int) string {
	if !bytes.Contains(data[objStart:objEnd], []byte("\n")) {
		return "  "
	}
	braceIndent := lineIndent(data, objStart)
	fieldIndent := objectFieldIndent(data, objStart, objEnd)
	if strings.HasPrefix(fieldIndent, braceIndent) && len(fieldIndent) > len(braceIndent) {
		if unit := fieldIndent[len(braceIndent):]; strings.Trim(unit, " \t") == "" {
			return unit
		}
	}
	return "  "
}

// lineStart returns the index just past the newline that opens the line pos sits on.
func lineStart(data []byte, pos int) int {
	return bytes.LastIndexByte(data[:pos], '\n') + 1
}

// lineIndent returns the whitespace the line pos sits on begins with — the column an entry
// placed there is aligned to. Empty for a line that does not start with whitespace (a minified
// file), which is exactly the "do not indent this" answer.
func lineIndent(data []byte, pos int) string {
	start := lineStart(data, pos)
	end := start
	for end < len(data) && (data[end] == ' ' || data[end] == '\t') {
		end++
	}
	return string(data[start:end])
}

// aloneOnLine reports whether only whitespace precedes pos on its line, i.e. deleting the value
// at pos can take the whole line with it.
func aloneOnLine(data []byte, pos int) bool {
	return strings.Trim(string(data[lineStart(data, pos):pos]), " \t") == ""
}

// lastNonSpace returns the index of the last non-whitespace byte before pos, or -1.
func lastNonSpace(data []byte, pos int) int {
	for i := pos - 1; i >= 0; i-- {
		if !isJSONSpace(data[i]) {
			return i
		}
	}
	return -1
}

// isAllJSONSpace reports whether b is empty or nothing but JSON whitespace.
func isAllJSONSpace(b []byte) bool {
	for _, c := range b {
		if !isJSONSpace(c) {
			return false
		}
	}
	return true
}

// errNoServersArray is the "this file has no servers key yet" case, which appendServer answers
// by writing one. It is deliberately not an error the caller sees.
var errNoServersArray = errors.New("no servers array")

// serversArrayRange returns the byte range of the "servers" array, brackets included.
func serversArrayRange(data []byte) (start, end int, err error) {
	root := skipJSONSpace(data, 0)
	if root >= len(data) || data[root] != '{' {
		return 0, 0, errors.New("not a JSON object")
	}
	vs, _, found, err := objectFieldValue(data, root, "servers")
	if err != nil {
		return 0, 0, err
	}
	if !found {
		return 0, 0, errNoServersArray
	}
	start = skipJSONSpace(data, vs)
	if start >= len(data) || data[start] != '[' {
		return 0, 0, errors.New(`"servers" is not an array`)
	}
	end, err = jsonValueEnd(data, start)
	if err != nil {
		return 0, 0, err
	}
	return start, end, nil
}

// serverSpan is one entry of the "servers" array: its byte range and its name.
type serverSpan struct {
	start, end int
	name       string
}

// serverObjects walks the "servers" array and returns every entry in it.
func serverObjects(data []byte, arrayStart, arrayEnd int) ([]serverSpan, error) {
	var out []serverSpan
	i := arrayStart + 1
	for {
		i = skipJSONSpace(data, i)
		if i >= arrayEnd {
			return nil, errors.New(`"servers" array is not closed`)
		}
		switch data[i] {
		case ']':
			return out, nil
		case ',':
			i++
		case '{':
			e, err := jsonValueEnd(data, i)
			if err != nil {
				return nil, err
			}
			span := serverSpan{start: i, end: e}
			if ns, ne, ok, err := objectFieldValue(data, i, "name"); err != nil {
				return nil, err
			} else if ok {
				name, err := strconv.Unquote(string(data[ns:ne]))
				if err != nil {
					return nil, fmt.Errorf("unreadable name: %w", err)
				}
				span.name = name
			}
			out = append(out, span)
			i = e
		default:
			return nil, errors.New(`"servers" holds something that is not an object`)
		}
	}
}

// findServerObject returns the byte range of the entry named name inside the file's
// "servers" array.
func findServerObject(data []byte, name string) (start, end int, err error) {
	arrayStart, arrayEnd, err := serversArrayRange(data)
	if err != nil {
		return 0, 0, err
	}
	spans, err := serverObjects(data, arrayStart, arrayEnd)
	if err != nil {
		return 0, 0, err
	}
	for _, span := range spans {
		if span.name == name {
			return span.start, span.end, nil
		}
	}
	return 0, 0, ErrServerNotInFile
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
