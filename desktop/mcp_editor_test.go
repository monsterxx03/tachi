package main

// The settings editor's half of MCP: authoring mcp.json (add / edit / delete an entry) and handing
// the result to the running process. The fixtures keep every server DISABLED, so a save never
// spawns a server process — what is under test is the file and the config, not a connection.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

func readMCPFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(config.BaseDir(), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func entryNames(entries []MCPServerEntryVO) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Server.Name)
	}
	return out
}

// TestListMCPServerEntriesReadsRawAndSaysWhereEachCameFrom: the editor lists exactly what the
// loader would load (base + the ACTIVE profile), and hands back the file's own content — a field
// the file does not set stays absent, so pressing save on an untouched entry cannot write this
// process's defaults into a hand-written file.
func TestListMCPServerEntriesReadsRawAndSaysWhereEachCameFrom(t *testing.T) {
	d, svc, _ := mcpConfigFixture(t)
	writeMCPConfigFile(t, "mcp.prod.json", `{"servers":[{"name":"prod-server","type":"http","url":"https://p.example.com","enabled":false}]}`)
	d.withConfig(func(c *config.Config) { c.ActiveMCPProfile = "prod" })

	entries := svc.ListMCPServerEntries()
	if got := entryNames(entries); len(got) != 2 || got[0] != "base-server" || got[1] != "prod-server" {
		t.Fatalf("entries = %v, want base-server + prod-server", got)
	}
	if entries[0].Profile != "" {
		t.Errorf("base entry Profile = %q, want \"\"", entries[0].Profile)
	}
	if entries[1].Profile != "prod" {
		t.Errorf("profile entry Profile = %q, want prod", entries[1].Profile)
	}
	// Nothing filled in: the file has no timeout, so the editor must not see the 10s default.
	if entries[0].Server.Timeout != 0 {
		t.Errorf("Timeout = %v, want the absent value (0), not the default", entries[0].Server.Timeout)
	}
}

// TestSaveMCPServerEntryWritesTheFileAndInstalls: the file is the output, and the running config
// follows it — that is the whole contract of the editor.
func TestSaveMCPServerEntryWritesTheFileAndInstalls(t *testing.T) {
	d, svc, _ := mcpConfigFixture(t)

	off := false
	res := svc.SaveMCPServerEntry(MCPServerEntryVO{Server: config.MCPServerConfig{
		Name: "added", Type: config.MCPTransportStdio, Command: "/bin/added", Enabled: &off,
	}})
	if res != "ok" {
		t.Fatalf("SaveMCPServerEntry = %q, want ok", res)
	}

	file := readMCPFile(t, "mcp.json")
	if !strings.Contains(file, `"added"`) || !strings.Contains(file, `"/bin/added"`) {
		t.Errorf("mcp.json does not hold the new server:\n%s", file)
	}
	if !strings.Contains(file, `"base-server"`) {
		t.Errorf("the entry that was already there was lost:\n%s", file)
	}
	if got := serverNames(d.cfg.MCPServers); len(got) != 2 || got[0] != "base-server" || got[1] != "added" {
		t.Errorf("MCPServers = %v, want the new server installed", got)
	}
}

// TestSaveMCPServerEntryWritesIntoTheChosenProfile: "加到当前 profile" is a file choice, and the
// file it names is the one the loader reads for that profile.
func TestSaveMCPServerEntryWritesIntoTheChosenProfile(t *testing.T) {
	d, svc, _ := mcpConfigFixture(t)
	writeMCPConfigFile(t, "mcp.prod.json", `{"servers":[]}`)

	off := false
	res := svc.SaveMCPServerEntry(MCPServerEntryVO{
		Profile: "prod",
		Server: config.MCPServerConfig{
			Name: "prod-only", Type: config.MCPTransportHTTP, URL: "https://x.example.com", Enabled: &off,
		},
	})
	if res != "ok" {
		t.Fatalf("SaveMCPServerEntry = %q, want ok", res)
	}

	if got := readMCPFile(t, "mcp.prod.json"); !strings.Contains(got, `"prod-only"`) {
		t.Errorf("mcp.prod.json does not hold the server:\n%s", got)
	}
	if got := readMCPFile(t, "mcp.json"); strings.Contains(got, "prod-only") {
		t.Errorf("the base file was written too:\n%s", got)
	}
	// The ACTIVE profile is still the default, so the file just written is not the running set —
	// which is what "it only loads when you switch to that profile" means.
	if got := serverNames(d.cfg.MCPServers); len(got) != 1 || got[0] != "base-server" {
		t.Errorf("MCPServers = %v, want the active set untouched", got)
	}
}

// TestSaveMCPServerEntryRenamesWithoutLeavingTwo: the old name has to go, or the file defines one
// server twice — which the loader refuses outright, taking Bootstrap with it.
func TestSaveMCPServerEntryRenamesWithoutLeavingTwo(t *testing.T) {
	d, svc, _ := mcpConfigFixture(t)

	off := false
	res := svc.SaveMCPServerEntry(MCPServerEntryVO{
		PreviousName: "base-server",
		Server: config.MCPServerConfig{
			Name: "renamed", Type: config.MCPTransportStdio, Command: "true", Enabled: &off,
		},
	})
	if res != "ok" {
		t.Fatalf("SaveMCPServerEntry = %q, want ok", res)
	}

	file := readMCPFile(t, "mcp.json")
	if strings.Contains(file, "base-server") {
		t.Errorf("the old name is still defined:\n%s", file)
	}
	if !strings.Contains(file, `"renamed"`) {
		t.Errorf("the new name is missing:\n%s", file)
	}
	if got := serverNames(d.cfg.MCPServers); len(got) != 1 || got[0] != "renamed" {
		t.Errorf("MCPServers = %v, want just the renamed server", got)
	}
}

// TestSaveMCPServerEntryRefusesWhatCannotWork: every message is a thing the user can fix, and
// nothing is written. The nameless case is the one that matters beyond politeness — such an entry
// makes the FILE unloadable (Bootstrap fails), not just the server unusable.
func TestSaveMCPServerEntryRefusesWhatCannotWork(t *testing.T) {
	d, svc, _ := mcpConfigFixture(t)
	off := false
	cases := []struct {
		name  string
		entry MCPServerEntryVO
		want  string
	}{
		{"no name", MCPServerEntryVO{Server: config.MCPServerConfig{Type: config.MCPTransportStdio, Command: "x"}}, "名字"},
		{"stdio without command", MCPServerEntryVO{Server: config.MCPServerConfig{Name: "a", Type: config.MCPTransportStdio}}, "command"},
		{"http without url", MCPServerEntryVO{Server: config.MCPServerConfig{Name: "a", Type: config.MCPTransportHTTP}}, "url"},
		{"unknown type", MCPServerEntryVO{Server: config.MCPServerConfig{Name: "a", Type: "sse", URL: "x"}}, "type"},
		{"unknown profile", MCPServerEntryVO{Profile: "ghost", Server: config.MCPServerConfig{Name: "a", Type: config.MCPTransportStdio, Command: "x", Enabled: &off}}, "ghost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := svc.SaveMCPServerEntry(tc.entry)
			if !strings.Contains(res, tc.want) {
				t.Errorf("SaveMCPServerEntry = %q, want a message mentioning %q", res, tc.want)
			}
			if got := readMCPFile(t, "mcp.json"); strings.Contains(got, `"a"`) {
				t.Errorf("a refused save wrote the file anyway:\n%s", got)
			}
		})
	}
	if got := serverNames(d.cfg.MCPServers); len(got) != 0 {
		t.Errorf("MCPServers = %v, want nothing installed by a refused save", got)
	}
}

// TestDeleteMCPServerEntryRevealsTheBaseEntry: the two files are merged BY NAME, so deleting the
// profile's copy brings the base's back. That is what the merge means, and it is worth pinning —
// "I deleted it and it is still there" is the surprising outcome of a correct deletion.
func TestDeleteMCPServerEntryRevealsTheBaseEntry(t *testing.T) {
	d, svc, sid := mcpConfigFixture(t)
	writeMCPConfigFile(t, "mcp.prod.json",
		`{"servers":[{"name":"base-server","type":"http","url":"https://override.example.com","enabled":false}]}`)
	d.withConfig(func(c *config.Config) { c.ActiveMCPProfile = "prod" })
	if _, err := d.installMCPConfig(d.getRun(sid).agent, "prod"); err != nil {
		t.Fatalf("install: %v", err)
	}

	if res := svc.DeleteMCPServerEntry("base-server", "prod"); res != "ok" {
		t.Fatalf("DeleteMCPServerEntry = %q, want ok", res)
	}

	if got := readMCPFile(t, "mcp.prod.json"); strings.Contains(got, "override.example.com") {
		t.Errorf("the override is still there:\n%s", got)
	}
	// The base definition is what is left, and it is what the running set now holds.
	if got := serverNames(d.cfg.MCPServers); len(got) != 1 || got[0] != "base-server" {
		t.Errorf("MCPServers = %v, want the base entry revealed", got)
	}
}

// TestDeleteMCPServerEntryReportsAMissingServer: deleting from the file that does not define the
// name is a normal outcome (it may live in the other file) and says so instead of failing silently.
func TestDeleteMCPServerEntryReportsAMissingServer(t *testing.T) {
	_, svc, _ := mcpConfigFixture(t)

	res := svc.DeleteMCPServerEntry("ghost", "")
	if !strings.Contains(res, "ghost") {
		t.Errorf("DeleteMCPServerEntry = %q, want a message naming the server", res)
	}
	if got := readMCPFile(t, "mcp.json"); !strings.Contains(got, "base-server") {
		t.Errorf("a refused delete touched the file:\n%s", got)
	}
}
