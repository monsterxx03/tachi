package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/creasty/defaults"
)

const mcpConfigFileName = "mcp.json"

// mcpConfigFile is the JSON structure for a single MCP config file.
type mcpConfigFile struct {
	Servers []MCPServerConfig `json:"servers"`
}

// mcpServerWithOrigin tracks a server and whether it came from a profile file.
type mcpServerWithOrigin struct {
	Server  MCPServerConfig
	Profile string // empty = base file, non-empty = profile file
}

// mcpConfigFileEntry describes a config file to load and its profile origin.
type mcpConfigFileEntry struct {
	Path    string
	Profile string // empty for base, profile name for profile-specific files
}

// LoadMCPConfig loads MCP server configuration from JSON files.
//
// Resolution order (later files override same-named servers from earlier):
//  1. ~/.tachi/mcp.json                  (global base)
//  2. ~/.tachi/mcp.{profile}.json        (global profile, if profile set)
//  3. .tachi/mcp.json                    (project base, if workDir set)
//  4. .tachi/mcp.{profile}.json          (project profile, if profile set)
//
// Profile JSON files that don't exist are silently skipped.
// If no JSON files exist at all, returns nil, nil.
//
// Every server gets its Profile field stamped: servers from base files get
// empty string, servers from profile files get the activeProfile name.
func LoadMCPConfig(activeProfile string, workDir string) ([]MCPServerConfig, error) {
	entries := mcpConfigEntries(workDir, activeProfile)

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
			wrapped[i] = mcpServerWithOrigin{Server: srv, Profile: entry.Profile}
		}

		merged = mergeMCPServersWithOrigin(merged, wrapped)
		anyLoaded = true
	}

	if !anyLoaded {
		return nil, nil
	}

	return finalizeMCPServers(merged)
}

// mcpConfigEntries returns the ordered list of MCP JSON config files.
func mcpConfigEntries(workDir, profile string) []mcpConfigFileEntry {
	var entries []mcpConfigFileEntry

	globalDir := BaseDir()

	// 1. Global base
	entries = append(entries, mcpConfigFileEntry{
		Path:    filepath.Join(globalDir, mcpConfigFileName),
		Profile: "",
	})

	// 2. Global profile
	if profile != "" {
		entries = append(entries, mcpConfigFileEntry{
			Path:    filepath.Join(globalDir, mcpProfileFileName(profile)),
			Profile: profile,
		})
	}

	if workDir == "" {
		return entries
	}

	// 3. Project base
	entries = append(entries, mcpConfigFileEntry{
		Path:    filepath.Join(workDir, ".tachi", mcpConfigFileName),
		Profile: "",
	})

	// 4. Project profile
	if profile != "" {
		entries = append(entries, mcpConfigFileEntry{
			Path:    filepath.Join(workDir, ".tachi", mcpProfileFileName(profile)),
			Profile: profile,
		})
	}

	return entries
}

// mcpProfileFileName returns the profile-specific filename.
func mcpProfileFileName(profile string) string {
	return fmt.Sprintf("mcp.%s.json", profile)
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
	seen := make(map[string]bool, len(merged))

	for _, item := range merged {
		srv := item.Server
		srv.Profile = item.Profile

		if srv.Name == "" {
			return nil, fmt.Errorf("mcp server has no name (profile=%q)", item.Profile)
		}
		if seen[srv.Name] {
			return nil, fmt.Errorf("duplicate mcp server name %q (profile=%q)", srv.Name, item.Profile)
		}
		seen[srv.Name] = true

		if srv.Timeout == 0 {
			srv.Timeout = Duration(10 * time.Second)
		}

		if err := defaults.Set(&srv); err != nil {
			return nil, fmt.Errorf("server %q defaults: %w", srv.Name, err)
		}

		result = append(result, srv)
	}

	return result, nil
}
