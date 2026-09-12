package main

import (
	"context"
	"strings"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
)

// MCPToolVO describes one tool under an MCP server for the status-bar panel.
type MCPToolVO struct {
	Name        string `json:"name"`     // full name "mcp__server__tool"
	ToolName    string `json:"toolName"` // original MCP name (no prefix)
	Description string `json:"description,omitempty"`
	Loaded      bool   `json:"loaded"`
}

// MCPServerVO describes an MCP server and its tools for the status-bar panel.
type MCPServerVO struct {
	Name      string      `json:"name"`
	Type      string      `json:"type"`
	Connected bool        `json:"connected"`
	Tools     []MCPToolVO `json:"tools"`
}

// ListMCPServers returns all configured MCP servers with their tools and, for
// each tool, whether it is loaded (discovered/opt-in) in the CURRENT session.
// Only connected servers expose their tools (the pool); disabled ones show
// with no tools until connected.
func (s *AgentService) ListMCPServers() []MCPServerVO {
	d := s.desk
	if d.cfg == nil || d.mcp == nil {
		return nil
	}
	d.mu.Lock()
	sid := d.activeID
	d.mu.Unlock()
	set := d.mcp.SetFor(sid) // nil when sid is empty (no session context)

	byServer := make(map[string][]*mcp.DeferredTool)
	for _, t := range d.mcp.Pool().All() {
		byServer[t.ServerName] = append(byServer[t.ServerName], t)
	}

	out := make([]MCPServerVO, 0, len(d.cfg.MCPServers))
	for _, srv := range d.cfg.MCPServers {
		vo := MCPServerVO{Name: srv.Name, Type: string(srv.Type), Connected: d.mcp.IsConnected(srv.Name)}
		for _, t := range byServer[srv.Name] {
			loaded := set != nil && set.Contains(t.Name)
			vo.Tools = append(vo.Tools, MCPToolVO{
				Name:        t.Name,
				ToolName:    strings.TrimPrefix(t.Name, "mcp__"+t.ServerName+"__"),
				Description: t.Description,
				Loaded:      loaded,
			})
		}
		out = append(out, vo)
	}
	return out
}

// SetMCPServerEnabled connects (enabled=true) or disconnects (enabled=false) a
// configured MCP server at runtime. It does NOT persist to mcp.json — the
// change is runtime-only (restart reverts to config).
func (s *AgentService) SetMCPServerEnabled(name string, enabled bool) string {
	d := s.desk
	if d.cfg == nil || d.mcp == nil {
		return "mcp not configured"
	}
	var srv *config.MCPServerConfig
	for i := range d.cfg.MCPServers {
		if d.cfg.MCPServers[i].Name == name {
			srv = &d.cfg.MCPServers[i]
			break
		}
	}
	if srv == nil {
		return "server not found"
	}
	if enabled {
		tools, err := d.mcp.Reconnect(context.Background(), srv)
		if err != nil {
			return err.Error()
		}
		for _, t := range tools {
			hint := ""
			if srv.SearchHints != nil {
				hint = srv.SearchHints[t.ToolName()]
			}
			d.mcp.Pool().Add(mcp.NewDeferredToolFromMCPTool(t, hint))
		}
		return "ok"
	}
	if err := d.mcp.Disconnect(name); err != nil {
		return err.Error()
	}
	d.mcp.Pool().RemoveByServer(name)
	return "ok"
}

// SetMCPToolEnabled toggles a single MCP tool for the CURRENT session:
// enabled marks it discovered/opt-in (and registers it with the session's
// agent so it is immediately usable); disabled removes it (and unregisters it).
// Per-session only — no global persistence.
func (s *AgentService) SetMCPToolEnabled(name string, enabled bool) string {
	d := s.desk
	if d.cfg == nil || d.mcp == nil {
		return "mcp not configured"
	}
	d.mu.Lock()
	sid := d.activeID
	r := d.getRun(sid)
	d.mu.Unlock()
	if sid == "" {
		return "no current session"
	}
	set := d.mcp.SetFor(sid)
	if set == nil {
		return "no session context"
	}
	if enabled {
		set.Add(name)
		if r != nil && r.agent != nil {
			if dt := d.mcp.Pool().Get(name); dt != nil && dt.Tool != nil {
				r.agent.RegisterTool(dt.Tool)
			}
		}
		return "ok"
	}
	set.Remove(name)
	if r != nil && r.agent != nil {
		r.agent.UnregisterTool(name)
	}
	return "ok"
}

// ListMCPProfiles returns the active MCP profile and all available profiles
// (mcp.<name>.json in global/project scope).
func (s *AgentService) ListMCPProfiles() map[string]any {
	d := s.desk
	if d.cfg == nil {
		return nil
	}
	return map[string]any{
		"active":    d.cfg.ActiveMCPProfile,
		"available": config.ListMCPProfiles(config.FindProjectRoot()),
	}
}

// SetMCPProfile switches the active MCP profile at runtime (reverts on restart).
// It uses the current session's agent (whose manager is the shared one) and
// reconciles the connected servers via SwitchMCPProfile.
func (s *AgentService) SetMCPProfile(name string) string {
	d := s.desk
	if d.cfg == nil || d.mcp == nil {
		return "mcp not configured"
	}
	d.mu.Lock()
	sid := d.activeID
	r := d.getRun(sid)
	d.mu.Unlock()
	if sid == "" || r == nil || r.agent == nil {
		return "no current session agent"
	}
	if _, err := r.agent.SwitchMCPProfile(context.Background(), name, config.FindProjectRoot()); err != nil {
		return err.Error()
	}
	return "ok"
}
