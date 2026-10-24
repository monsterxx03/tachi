package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
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
	// NeedsAuth is true when this server answered a connection attempt with "OAuth
	// authorization required". It exists so the panel can offer the button AND say why
	// — a server that needs authorizing is otherwise indistinguishable from one that is
	// down, which is what made this feature unreachable: the failure had nowhere to
	// show up.
	NeedsAuth bool `json:"needsAuth"`
}

// ListMCPServers returns all configured MCP servers with their tools and, for
// each tool, whether it is loaded (discovered/opt-in) in the CURRENT session.
// Only connected servers expose their tools (the pool); disabled ones show
// with no tools until connected. A server's tools are ordered loaded-first
// (see the sort below), because the panel renders the order it is given.
func (s *AgentService) ListMCPServers() []MCPServerVO {
	d := s.desk
	d.mu.Lock()
	sid := d.activeID
	d.mu.Unlock()
	return s.listMCPServersFor(sid)
}

// listMCPServersFor is the same reading addressed by SESSION: the phone's MCP panel is about the
// conversation it is looking at, not about whichever one the window has open.
func (s *AgentService) listMCPServersFor(sessionID string) []MCPServerVO {
	d := s.desk
	if d.config() == nil || d.mcp == nil {
		return nil
	}
	set := d.mcp.SetFor(sessionID) // nil when the id is empty (no session context)

	byServer := make(map[string][]*mcp.DeferredTool)
	for _, t := range d.mcp.Pool().All() {
		byServer[t.ServerName] = append(byServer[t.ServerName], t)
	}

	cfg := d.config()
	out := make([]MCPServerVO, 0, len(cfg.MCPServers))
	needsAuth := d.mcpNeedsAuthSnapshot()
	for _, srv := range cfg.MCPServers {
		vo := MCPServerVO{
			Name:      srv.Name,
			Type:      string(srv.Type),
			Connected: d.mcp.IsConnected(srv.Name),
			NeedsAuth: needsAuth[srv.Name],
		}
		for _, t := range byServer[srv.Name] {
			loaded := set != nil && set.Contains(t.Name)
			vo.Tools = append(vo.Tools, MCPToolVO{
				Name:        t.Name,
				ToolName:    strings.TrimPrefix(t.Name, "mcp__"+t.ServerName+"__"),
				Description: t.Description,
				Loaded:      loaded,
			})
		}
		// Loaded first, and stable otherwise. The pool lists a server's tools alphabetically; under
		// tool search NOTHING is auto-loaded, so the loaded rows are the few this session can
		// actually call — the ones a reader opens the panel for — and they must not be scattered
		// through a list of everything the server offers. Stable, so the two halves keep the pool's
		// own order: toggling one tool must not reshuffle the rows around it.
		sort.SliceStable(vo.Tools, func(i, j int) bool { return vo.Tools[i].Loaded && !vo.Tools[j].Loaded })
		out = append(out, vo)
	}
	return out
}

// mcpServerByName returns the configured server with this name, or nil.
//
// The returned pointer ALIASES the live config's slice, and the caller may write
// through it (persistMCPServerEnabled updates Enabled in place so the panel and
// the next profile switch see what the file now says). That is the ONE place the
// cfgMu rules get bent, and it is tolerable only because it is a single bool on a
// single element: no reader can observe a torn value, and the field is not one any
// agent caches at construction. Anything larger belongs behind withConfig.
func (d *desktopApp) mcpServerByName(name string) *config.MCPServerConfig {
	cfg := d.config()
	if cfg == nil {
		return nil
	}
	for i := range cfg.MCPServers {
		if cfg.MCPServers[i].Name == name {
			return &cfg.MCPServers[i]
		}
	}
	return nil
}

// enableMCPServer connects a server and publishes what it offers: the tools go into the
// shared deferred pool and every live agent is told the pool grew. It returns the reason
// to show the user on failure, plus the raw error for a caller that can act on the KIND
// of failure ("OAuth authorization required" has a button; a timeout does not).
//
// It is the ONE enable path — the panel's toggle and the end of an OAuth authorization
// both come through here, because a server authorized in the panel must end up exactly as
// connected as one the user enabled by hand, and two code paths that both "connect a
// server" drift the moment one of them learns something the other does not.
//
// Adding to the shared pool is only half the job: the reminder that tells the model these
// tools exist fires at most once per session, so without the notify every mid-session
// enable is invisible and the tools are only findable if the model happens to search for
// them blind. Every live agent is notified, not just the one on screen: the pool and the
// connection are shared by all sessions, so enabling a server is a global fact and a
// session parked in the background would otherwise stay blind to it. Each agent's own
// discovered set still decides what it is told about, so nothing leaks between sessions.
func (d *desktopApp) enableMCPServer(srv *config.MCPServerConfig) (string, error) {
	tools, err := d.mcp.Reconnect(context.Background(), srv)
	if err != nil {
		if oauthErr, ok := errors.AsType[*mcp.OAuthRequiredError](err); ok {
			d.markMCPNeedsAuth(oauthErr.ServerName, true)
		}
		return err.Error(), err
	}
	for _, t := range tools {
		hint := ""
		if srv.SearchHints != nil {
			hint = srv.SearchHints[t.ToolName()]
		}
		d.mcp.Pool().Add(mcp.NewDeferredToolFromMCPTool(t, hint))
	}
	// The fan-out goes through applyToLiveAgents for the same reason the doc comment above
	// gives: this is a global fact, not the displayed session's. Its failures are dropped on
	// purpose — NotifyDeferredToolsAdded has no error to report (the pool is already filled
	// by the lines above; the notify only makes the reminder re-arm).
	d.applyToLiveAgents(func(a *agent.AIAgent) error {
		a.NotifyDeferredToolsAdded()
		return nil
	})
	d.markMCPNeedsAuth(srv.Name, false)
	return "ok", nil
}

// SetMCPServerEnabled connects (enabled=true) or disconnects (enabled=false) a
// configured MCP server at runtime. It does NOT persist to mcp.json — the
// change is runtime-only (restart reverts to config).
func (s *AgentService) SetMCPServerEnabled(name string, enabled bool) string {
	d := s.desk
	if d.config() == nil || d.mcp == nil {
		return "mcp not configured"
	}
	srv := d.mcpServerByName(name)
	if srv == nil {
		return "server not found"
	}
	if enabled {
		res, err := d.enableMCPServer(srv)
		// The toggle is a statement of intent about a FILE, so it is written down whatever
		// the connection did: a connect that failed (a server that needs authorizing, a
		// network that is down) is a reason to try again next launch, not a reason to forget
		// the user asked for it.
		note := d.persistMCPServerEnabled(srv, true)
		// "needs authorization" is not "the connect failed": the user's way out is a
		// button, not a retry. enableMCPServer has recorded it (the panel's NeedsAuth);
		// here it is said in the panel's own words, because the error's own text is
		// addressed to whichever frontend the reader happens to be in.
		if _, ok := errors.AsType[*mcp.OAuthRequiredError](err); ok {
			return withNote(fmt.Sprintf("「%s」需要 OAuth 授权：在面板里点「授权」", name), note)
		}
		return withNote(res, note)
	}
	discErr := d.mcp.Disconnect(name)
	if discErr == nil {
		d.mcp.Pool().RemoveByServer(name)
		// Mirror the cleanup TUI does on the same toggle: a tool the user had
		// explicitly enabled must leave the registry, or the model keeps calling a
		// tool whose server is gone. UnregisterMCPServer also drops the tools from
		// every session's discovered set, so a later re-enable does not report them
		// as already loaded. Failures are dropped: there is nothing here to fail.
		d.applyToLiveAgents(func(a *agent.AIAgent) error {
			a.UnregisterMCPServer(name)
			return nil
		})
	}
	note := d.persistMCPServerEnabled(srv, false)
	if discErr != nil {
		return withNote(discErr.Error(), note)
	}
	return withNote("ok", note)
}

// persistMCPServerEnabled writes a toggle into the mcp.json the server came from, and returns
// the reason it could not — or "" when there is nothing to report.
//
// It writes to `SourceFile`, the file the entry was READ from, and that is the whole point:
// a server in mcp.json and one in mcp.<profile>.json are different entries in different files.
// Writing both into the profile file would shadow the base entry (profile files override base
// ones) and lose the change the moment the user switches back to the default profile; writing
// both into the base file would be overridden and equally lost.
//
// The profile SWITCH itself is deliberately NOT persisted (no active_mcp_profile in
// config.yaml): which profile is on is a decision about this session, and a dropdown that
// rewrote the user's config file would make it the default for every future launch.
//
// A server with no SourceFile (a config assembled in code rather than read from disk) has no
// file to disagree with, so the change simply stays runtime-only — the behavior every caller
// had before this existed.
//
// mcpFileMu is held across the write because this is a read-modify-write of the whole file, and
// the settings editor does the same to the same file: two of them landing together would let the
// second one's read predate the first one's write (see the field's comment).
func (d *desktopApp) persistMCPServerEnabled(srv *config.MCPServerConfig, enabled bool) string {
	if srv.SourceFile == "" {
		return ""
	}
	d.mcpFileMu.Lock()
	err := config.SetServerEnabledInFile(srv.SourceFile, srv.Name, enabled)
	d.mcpFileMu.Unlock()
	if err != nil {
		return fmt.Sprintf("没能写回 %s：%v", srv.SourceFile, err)
	}
	// Keep the in-memory copy in step with the file, so the panel and the next profile
	// switch see the same thing the next launch will read.
	flag := enabled
	srv.Enabled = &flag
	return ""
}

// withNote folds a persistence warning into a binding's answer, which is a single string: the
// primary outcome first (it is what the user did), the save failure after it.
func withNote(res, note string) string {
	if note == "" {
		return res
	}
	if res == "ok" {
		return "已生效，但" + note
	}
	return res + "（" + note + "）"
}

// SetMCPToolEnabled toggles a single MCP tool for the CURRENT session:
// enabled marks it discovered/opt-in (and registers it with the session's
// agent so it is immediately usable); disabled removes it (and unregisters it).
// Per-session only — no global persistence.
func (s *AgentService) SetMCPToolEnabled(name string, enabled bool) string {
	d := s.desk
	if d.config() == nil || d.mcp == nil {
		return "mcp not configured"
	}
	sid := d.currentID()
	r := d.runOf(sid)
	if sid == "" {
		return refuseNoSession
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

// switchMCPProfileTo switches the shared MCP manager to the named profile: it loads that
// profile's config, reconciles the connections, and only THEN writes the two config fields —
// under cfgMu, and nowhere else.
//
// It is the desktop's own path for what AIAgent.SwitchMCPProfile does for the frontends that have
// no app-wide config lock (tui / acp / channel). The split matters twice over:
//
//   - the config has exactly ONE guarded writer (`withConfig`), while turns read MCPServers as
//     they start — SwitchMCPProfile writes both fields itself, unguarded;
//   - the write happens AFTER the connections moved, so the profile field never names a profile
//     whose servers failed to load. The old order wrote it first, and a failure had nothing to
//     roll back to (the value it captured as "previous" was already the new one).
//
// Unlike the settings rows, this is NOT deferred for a running turn: it is a shared-layer
// operation (one connection serves every session) and the user asked for it now — see
// deferred_push.go for the split.
func (d *desktopApp) switchMCPProfileTo(profile string) error {
	if d.config() == nil {
		return errors.New("no config")
	}
	a, note := d.appAgentForMCP()
	if a == nil {
		return errors.New(note)
	}
	if err := agent.ValidateMCPProfile(profile); err != nil {
		return err
	}
	_, err := d.installMCPConfig(a, profile)
	return err
}

// installMCPConfig loads profile's files, reconciles the live connections with what they say, and
// only THEN writes the two config fields — under cfgMu, and nowhere else.
//
// It is the one path that turns "what the files hold" into "what this process runs", shared by the
// profile switch and by the settings editor's save/delete (both of which change a FILE and owe the
// running process the same follow-up). Reconciling is diff-based, so calling it when nothing moved
// costs nothing and changes nothing.
func (d *desktopApp) installMCPConfig(a *agent.AIAgent, profile string) (agent.MCPSwitchResult, error) {
	cfg := d.config()
	if cfg == nil {
		return agent.MCPSwitchResult{}, errors.New("no config")
	}
	// The load is a pure read of the JSON files: it does not touch the config, so a file that
	// cannot even be read leaves everything as it was.
	next, err := config.LoadMCPConfig(profile)
	if err != nil {
		return agent.MCPSwitchResult{}, fmt.Errorf("loading profile %q: %w", profile, err)
	}
	result, final, err := a.ReconcileMCPServers(context.Background(), profile, cfg.MCPServers, next)
	if err != nil {
		return agent.MCPSwitchResult{}, err
	}
	d.withConfig(func(c *config.Config) {
		c.ActiveMCPProfile = profile
		c.MCPServers = final
	})
	logger.New("desktop").Info(context.Background(), "MCP: config installed",
		"profile", profile, "kept", len(result.Kept), "added", len(result.Added),
		"removed", len(result.Removed), "reconnected", len(result.Reconned),
		"errors", len(result.Errors))
	return result, nil
}

// ListMCPProfiles returns the active MCP profile and all available profiles
// (mcp.<name>.json in ~/.tachi).
func (s *AgentService) ListMCPProfiles() map[string]any {
	d := s.desk
	cfg := d.config()
	if cfg == nil {
		return nil
	}
	return map[string]any{
		"active":    cfg.ActiveMCPProfile,
		"available": config.ListMCPProfiles(),
	}
}

// SetMCPProfile switches the active MCP profile at runtime (reverts on restart).
// It uses the current session's agent (whose manager is the shared one) and
// reconciles the connected servers via SwitchMCPProfile.
func (s *AgentService) SetMCPProfile(name string) string {
	d := s.desk
	if d.config() == nil || d.mcp == nil {
		return "mcp not configured"
	}
	if _, refuse := d.agentOf(d.currentID()); refuse != "" {
		return refuse
	}
	// The desktop's own path: it writes the config under cfgMu and only after the connections
	// moved (see switchMCPProfileTo — the agent's SwitchMCPProfile writes config itself, which is
	// fine for tui/acp/channel and wrong here). The refusal above is what tells the user "no live
	// session" first; switchMCPProfileTo picks its agent through the same helper.
	if err := d.switchMCPProfileTo(name); err != nil {
		return err.Error()
	}
	return "ok"
}

// --- the settings editor's view of mcp.json --------------------------------
//
// The status-bar PANEL is a shortcut for the session's own concerns: toggle a server, authorize
// it, load one of its tools. The settings EDITOR is where the FILE is authored — add a server,
// change its command, drop it. They overlap on purpose; what they cannot overlap on is the write,
// which is why every edit here goes through mcpFileMu and is followed by the same install the
// profile switch runs.

// MCPServerEntryVO is one server as the editor handles it: the RAW contents of the file
// (config.LoadMCPConfigRaw — an absent field stays absent, so pressing save on an entry the user
// only looked at does not write this process's defaults into a file he wrote by hand) plus the
// profile whose file defines it.
type MCPServerEntryVO struct {
	Server config.MCPServerConfig `json:"server"`
	// Profile is "" for the base mcp.json, else the profile whose mcp.<profile>.json holds this
	// entry. An edit goes back to that same file.
	Profile string `json:"profile,omitempty"`
	// PreviousName is write-only, and only for a rename: the name this entry had before the edit,
	// so the old one can be removed rather than left behind as a SECOND definition of one server.
	PreviousName string `json:"previousName,omitempty"`
}

// ListMCPServerEntries returns every server the editor can edit: the merged view of the base file
// and the ACTIVE profile's file.
//
// The merged view is the only set a save can reach (it is what the loader builds, and what the
// next launch reads), so an entry the user cannot see here is not one he can edit — which is why
// nothing lists the inactive profiles' files.
func (s *AgentService) ListMCPServerEntries() []MCPServerEntryVO {
	d := s.desk
	cfg := d.config()
	if cfg == nil {
		return nil
	}
	servers, err := config.LoadMCPConfigRaw(cfg.ActiveMCPProfile)
	if err != nil {
		// A file this process cannot parse is exactly what the editor is for, but a list is the
		// wrong place to report it: the settings page has its own error surface, and an empty
		// list plus a log line keeps the rest of the page usable.
		logger.New("desktop").Error(context.Background(), "MCP: reading config files failed", err)
		return nil
	}
	out := make([]MCPServerEntryVO, 0, len(servers))
	for _, srv := range servers {
		out = append(out, MCPServerEntryVO{Server: srv, Profile: srv.Profile})
	}
	return out
}

// SaveMCPServerEntry writes entry into the file its Profile names ("" = the base mcp.json) and
// installs the result into the running process. It returns "" on success, or what to tell the
// user.
//
// The write is the one step that cannot be skipped and cannot be rolled back by what follows: a
// server that fails to CONNECT is reported, not undone — the file the user asked for is on disk
// either way, and undoing it would have to guess whether he meant the file or the connection.
func (s *AgentService) SaveMCPServerEntry(entry MCPServerEntryVO) string {
	d := s.desk
	if d.config() == nil {
		return "no config"
	}
	if err := validateMCPEntry(entry.Server); err != nil {
		return err.Error()
	}
	if err := validateMCPEntryProfile(entry.Profile); err != nil {
		return err.Error()
	}
	path := config.MCPConfigPath(entry.Profile)

	d.mcpFileMu.Lock()
	err := config.UpsertServerInFile(path, entry.Server)
	if err == nil && entry.PreviousName != "" && entry.PreviousName != entry.Server.Name {
		err = config.DeleteServerInFile(path, entry.PreviousName)
	}
	d.mcpFileMu.Unlock()
	if err != nil {
		return fmt.Sprintf("写入 %s 失败：%v", filepath.Base(path), err)
	}
	return withNote("ok", d.applySavedMCPConfig())
}

// DeleteMCPServerEntry removes name from the file its Profile names and installs the result.
//
// Deleting reveals, rather than deletes, when the other file also defines the name: the base file
// and the profile file are merged by name, so dropping the profile's copy brings the base's back.
// That is what the merge means, and the editor's list shows it on the next read.
func (s *AgentService) DeleteMCPServerEntry(name, profile string) string {
	d := s.desk
	if d.config() == nil {
		return "no config"
	}
	if name == "" {
		return "没有指定要删除的 server"
	}
	if err := validateMCPEntryProfile(profile); err != nil {
		return err.Error()
	}
	path := config.MCPConfigPath(profile)

	d.mcpFileMu.Lock()
	err := config.DeleteServerInFile(path, name)
	d.mcpFileMu.Unlock()
	if err != nil {
		if errors.Is(err, config.ErrServerNotInFile) {
			return fmt.Sprintf("%s 里没有 %q —— 它可能在另一个文件里", filepath.Base(path), name)
		}
		return fmt.Sprintf("写入 %s 失败：%v", filepath.Base(path), err)
	}
	return withNote("ok", d.applySavedMCPConfig())
}

// applySavedMCPConfig hands the files' new contents to the running process, and returns "" when
// that worked or the reason to show when it did not.
//
// The "no MCP layer yet" case is a real one and not a failure: the shared manager is built at
// startup only when the config was non-empty (desktop/agent_driver.go), so a FIRST server added
// here has nothing to connect to until the next launch. The file is still the thing that matters
// — it is what the next launch reads — so the save says so rather than refusing.
func (d *desktopApp) applySavedMCPConfig() string {
	a, note := d.appAgentForMCP()
	if a == nil {
		return fmt.Sprintf("%s（这个 server 已写入配置，重启后生效）", note)
	}
	profile := ""
	if cfg := d.config(); cfg != nil {
		profile = cfg.ActiveMCPProfile
	}
	result, err := d.installMCPConfig(a, profile)
	if err != nil {
		return fmt.Sprintf("已写入配置，但应用到运行中的连接失败：%v", err)
	}
	if len(result.Errors) > 0 {
		msgs := make([]string, 0, len(result.Errors))
		for _, e := range result.Errors {
			msgs = append(msgs, e.Error())
		}
		return "已写入配置，但有 server 没连上：" + strings.Join(msgs, "；")
	}
	return ""
}

// validateMCPEntry rejects what the editor can see before it reaches the file.
//
// The name is the one check that matters beyond politeness: the loader treats a nameless entry as
// a broken FILE (finalizeMCPServers errors → LoadMCPServers fails → Bootstrap fails), so writing
// one would leave an mcp.json nobody can start from. config.UpsertServerInFile asserts the same
// invariant at the door; this one exists to say it in the user's words.
func validateMCPEntry(srv config.MCPServerConfig) error {
	if strings.TrimSpace(srv.Name) == "" {
		return errors.New("server 名字不能为空")
	}
	switch srv.Type {
	case config.MCPTransportStdio:
		if strings.TrimSpace(srv.Command) == "" {
			return fmt.Errorf("stdio 类型要填 command（%q）", srv.Name)
		}
	case config.MCPTransportHTTP:
		if strings.TrimSpace(srv.URL) == "" {
			return fmt.Errorf("http 类型要填 url（%q）", srv.Name)
		}
	default:
		return fmt.Errorf("type 只能是 %q 或 %q", config.MCPTransportStdio, config.MCPTransportHTTP)
	}
	if srv.Timeout < 0 {
		return errors.New("timeout 不能是负数")
	}
	if srv.OAuth != nil && srv.Type != config.MCPTransportHTTP {
		return errors.New("oauth 只对 http 类型有效")
	}
	return nil
}

// validateMCPEntryProfile rejects a profile the loader would never read: "" (the base file) or a
// name that an mcp.<name>.json already provides. Both are resolved through config.MCPConfigPath,
// so the file written is by construction the file read.
func validateMCPEntryProfile(profile string) error {
	if profile == "" {
		return nil
	}
	if !slices.Contains(config.ListMCPProfiles(), profile) {
		existing := config.ListMCPProfiles()
		if len(existing) == 0 {
			return fmt.Errorf("没有 mcp.%s.json（当前一个 profile 文件都没有）", profile)
		}
		return fmt.Errorf("没有 mcp.%s.json（现有的 profile：%s）", profile, strings.Join(existing, ", "))
	}
	return nil
}

// --- MCP OAuth authorization ---
//
// An HTTP MCP server behind OAuth2 cannot be reached until a token exists, and the token
// only comes from a browser round trip the user has to consent to. Three things make that
// work in the desktop, and each is a place this can go silently wrong:
//
//   - the flow is STARTED here, from mcp.RunOAuthFlowWithProgress. A GUI needs the
//     authorization URL and the "now open it by hand" moment as DATA — the text
//     frontends' way of learning them is a sentence, and parsing prose for a URL is the
//     coupling the progress callback exists to remove.
//   - "this server needs authorizing" is REMEMBERED per server (`needsAuth`), because a
//     server whose token is missing looks exactly like a server that is down: without it
//     the panel has nothing to offer and the user has nothing to click.
//   - the way out is the SAME enable path an ordinary toggle takes (enableMCPServer), so
//     an authorized server is connected, pooled and announced identically.

// MCPAuthEvent is the payload of the "agent:mcp_auth" event: one step of an
// authorization, as the panel should render it.
//
// Phase is the string form of mcp.OAuthPhase for the phases the flow itself reports
// (browser / waiting / manual), plus the two the desktop owns: "starting" before anything
// is known, "ok" when the server is authorized AND connected, and "error" for everything
// that is nobody's instruction.
type MCPAuthEvent struct {
	Server  string `json:"server"`
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`
	AuthURL string `json:"authUrl,omitempty"`
}

// The desktop's own phases, alongside mcp.OAuthPhase's (see MCPAuthEvent).
const (
	mcpAuthPhaseStarting = "starting"
	mcpAuthPhaseOK       = "ok"
	mcpAuthPhaseError    = "error"
)

// mcpAuthTimeout bounds one authorization, start to finish. It is deliberately long: the
// waiting phases are spent on a human reading a consent screen, and the browser flow's own
// callback wait (five minutes) has to fit inside it.
const mcpAuthTimeout = 10 * time.Minute

// AuthorizeMCPServer starts the OAuth2 flow for an HTTP MCP server and returns "ok" (or
// the reason it will not start). The flow runs in the background and reports itself
// through agent:mcp_auth events: it needs a browser, and often a human, so there is no
// answer to wait for here.
func (s *AgentService) AuthorizeMCPServer(name string) string {
	d := s.desk
	if d.config() == nil || d.mcp == nil {
		return "mcp not configured"
	}
	srv := d.mcpServerByName(name)
	if srv == nil {
		return "server not found"
	}
	if srv.Type != config.MCPTransportHTTP {
		return fmt.Sprintf("只有 HTTP 类型的 MCP server 需要 OAuth 授权（「%s」是 stdio）", name)
	}
	if d.mcp.IsConnected(name) {
		return "已连接：无需授权（要换一个账号，先禁用它再授权）"
	}
	if !d.beginMCPAuth(name) {
		return "授权流程已经在这个 server 上进行"
	}
	d.emitMCPAuth(MCPAuthEvent{Server: name, Phase: mcpAuthPhaseStarting, Message: "正在启动授权流程…"})

	ctx, cancel := context.WithTimeout(context.Background(), mcpAuthTimeout)
	// The cancel is registered BEFORE the goroutine starts, so a paste that arrives while the
	// flow is still coming up cannot miss it: the two ways out finish the SAME exchange, and
	// letting both run would spend the authorization code twice.
	d.setMCPAuthCancel(name, cancel)

	go func() {
		defer d.endMCPAuth(name)
		// The context's own release: the flow may have returned early (a failure, or a
		// cancel from the paste path), and a 10-minute timer left running is a leak that
		// only shows up as a stuck goroutine in a profile.
		defer cancel()

		err := mcp.RunOAuthFlowWithProgress(ctx, srv, func(p mcp.OAuthProgress) {
			d.emitMCPAuth(MCPAuthEvent{
				Server:  name,
				Phase:   string(p.Phase),
				Message: p.Message,
				AuthURL: p.AuthURL,
			})
		})
		if err != nil {
			// A flow that ended by asking for the redirect URL has ALREADY said so (it
			// reported the manual phase with the URL). Repeating it as an error would
			// replace the instruction with a complaint — and the instruction is the part
			// the user can act on.
			if _, ok := errors.AsType[*mcp.OAuthRequiredError](err); !ok {
				d.emitMCPAuth(MCPAuthEvent{Server: name, Phase: mcpAuthPhaseError, Message: err.Error()})
			}
			return
		}
		// A token was obtained. Stopping here would leave the user authorized and still
		// disconnected — the flow's own entry points in the TUI reconnect for this reason.
		if res, _ := d.enableMCPServer(srv); res != "ok" {
			d.emitMCPAuth(MCPAuthEvent{
				Server: name, Phase: mcpAuthPhaseError,
				Message: "授权成功但连接失败：" + res,
			})
			return
		}
		d.reconnectMCPTokenSiblings(srv)
		d.emitMCPAuth(MCPAuthEvent{Server: name, Phase: mcpAuthPhaseOK, Message: "授权成功，已连接"})
	}()
	return "ok"
}

// CompleteMCPServerAuth finishes an authorization with the redirect URL the user pasted
// out of their browser's address bar, and returns "ok" or the reason it failed.
//
// This is the way through when the callback listener never receives the redirect — the
// port is taken or firewalled, the browser refuses to follow it, or the user is simply
// faster than the browser. It is why the flow persists its state and PKCE verifier before
// it starts waiting (see mcp.tryBrowserCallback).
func (s *AgentService) CompleteMCPServerAuth(name, redirectURL string) string {
	d := s.desk
	if d.config() == nil || d.mcp == nil {
		return "mcp not configured"
	}
	srv := d.mcpServerByName(name)
	if srv == nil {
		return "server not found"
	}
	if srv.Type != config.MCPTransportHTTP {
		return fmt.Sprintf("只有 HTTP 类型的 MCP server 支持 OAuth 授权（「%s」是 stdio）", name)
	}
	if strings.TrimSpace(redirectURL) == "" {
		return "请粘贴浏览器地址栏里的完整 URL"
	}
	// Any in-flight flow for this server is stopped first: both ways out exchange the
	// same authorization code, and the code is single-use.
	d.cancelMCPAuth(name)

	ctx, cancel := context.WithTimeout(context.Background(), mcpAuthTimeout)
	defer cancel()
	if err := mcp.CompleteManualAuth(ctx, srv, redirectURL); err != nil {
		return err.Error()
	}
	if res, err := d.enableMCPServer(srv); err != nil {
		if _, ok := errors.AsType[*mcp.OAuthRequiredError](err); ok {
			return fmt.Sprintf("已拿到 token，但「%s」仍要求授权：确认 token 是否对该 server 有效", name)
		}
		return res
	}
	d.reconnectMCPTokenSiblings(srv)
	d.emitMCPAuth(MCPAuthEvent{Server: name, Phase: mcpAuthPhaseOK, Message: "授权成功，已连接"})
	return "ok"
}

// reconnectMCPTokenSiblings connects the OTHER configured servers that share this one's
// token (same storage key = same host), because one authorization serves all of them.
//
// Failures are logged and dropped: this is tidying up after a success the user already
// got, and a sibling that stays down is a row the panel still shows with its own toggle.
// Servers that are disabled stay off — the token being shared is not consent to run them.
func (d *desktopApp) reconnectMCPTokenSiblings(srv *config.MCPServerConfig) {
	key := srv.TokenStorageName()
	cfg := d.config()
	if cfg == nil {
		return
	}
	for i := range cfg.MCPServers {
		other := &cfg.MCPServers[i]
		if other.Name == srv.Name || other.Type != config.MCPTransportHTTP {
			continue
		}
		if !other.IsEnabled() || d.mcp.IsConnected(other.Name) || other.TokenStorageName() != key {
			continue
		}
		if res, err := d.enableMCPServer(other); err != nil {
			logger.New("desktop").Warn(context.Background(), "MCP: sibling reconnect failed after authorization",
				"server", other.Name, "reason", res, "err", err)
		}
	}
}

// emitMCPAuth pushes one authorization step to the frontend. A nil app is the
// simulated/test case (see emitWorkspaceChanged for the same guard).
func (d *desktopApp) emitMCPAuth(ev MCPAuthEvent) {
	if !d.hasUI() {
		return
	}
	d.emit("agent:mcp_auth", ev)
}

// markMCPNeedsAuth records whether a server has asked for authorization and not been
// granted one, which is what the panel's NeedsAuth badge and 授权 button are drawn from.
func (d *desktopApp) markMCPNeedsAuth(name string, needed bool) {
	if name == "" {
		return
	}
	d.mcpAuthMu.Lock()
	defer d.mcpAuthMu.Unlock()
	if d.mcpAuthNeeded == nil {
		d.mcpAuthNeeded = make(map[string]bool)
	}
	if needed {
		d.mcpAuthNeeded[name] = true
		return
	}
	delete(d.mcpAuthNeeded, name)
}

// mcpNeedsAuthSnapshot returns a copy of the needs-auth set, so a caller (ListMCPServers)
// never reads the map while a connect attempt is writing it.
func (d *desktopApp) mcpNeedsAuthSnapshot() map[string]bool {
	d.mcpAuthMu.Lock()
	defer d.mcpAuthMu.Unlock()
	out := make(map[string]bool, len(d.mcpAuthNeeded))
	for name, needed := range d.mcpAuthNeeded {
		out[name] = needed
	}
	return out
}

// beginMCPAuth claims the authorization slot for a server, returning false when one is
// already running. Two flows for one server would race the same callback port and the same
// token file, and the user would see two browser tabs for one decision.
func (d *desktopApp) beginMCPAuth(name string) bool {
	d.mcpAuthMu.Lock()
	defer d.mcpAuthMu.Unlock()
	if d.mcpAuthInFlight == nil {
		d.mcpAuthInFlight = make(map[string]bool)
	}
	if d.mcpAuthInFlight[name] {
		return false
	}
	d.mcpAuthInFlight[name] = true
	return true
}

// setMCPAuthCancel hands the running flow's cancel func to the app, so a pasted redirect
// URL can stop it (see CompleteMCPServerAuth).
func (d *desktopApp) setMCPAuthCancel(name string, cancel context.CancelFunc) {
	d.mcpAuthMu.Lock()
	defer d.mcpAuthMu.Unlock()
	if d.mcpAuthCancels == nil {
		d.mcpAuthCancels = make(map[string]context.CancelFunc)
	}
	d.mcpAuthCancels[name] = cancel
}

// cancelMCPAuth stops an in-flight flow for this server, if there is one. The flow's own
// goroutine still owns the slot: it clears the state in endMCPAuth when it returns.
func (d *desktopApp) cancelMCPAuth(name string) {
	d.mcpAuthMu.Lock()
	cancel := d.mcpAuthCancels[name]
	delete(d.mcpAuthCancels, name)
	d.mcpAuthMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// endMCPAuth releases the authorization slot for a server.
func (d *desktopApp) endMCPAuth(name string) {
	d.mcpAuthMu.Lock()
	defer d.mcpAuthMu.Unlock()
	delete(d.mcpAuthInFlight, name)
	delete(d.mcpAuthCancels, name)
}

// MCPToolParamVO is one parameter of a tool's schema, flattened for display.
//
// Flattened rather than raw JSON on purpose: the panel's job is to answer "what does this
// tool want", and a raw inputSchema makes the reader reconstruct that from $refs and unions.
// What is lost is deliberate — nested object/array *details* collapse to their type (see
// ItemType), because a schema tree in a 380px popover is unreadable, and the full schema is
// one `MCPSearchTools` call away for anyone who needs it.
type MCPToolParamVO struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Required    bool     `json:"required"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Default     string   `json:"default,omitempty"`
	// ItemType is an array parameter's element type ("" when unknown).
	ItemType string `json:"itemType,omitempty"`
}

// MCPToolSchemaVO is one tool's whole contract — description and parameters — for the
// panel's tool detail.
type MCPToolSchemaVO struct {
	Name        string           `json:"name"`
	ToolName    string           `json:"toolName"`
	Server      string           `json:"server"`
	Description string           `json:"description"`
	Parameters  []MCPToolParamVO `json:"parameters"`
}

// GetMCPToolSchema returns one pool tool's contract for the panel, or the zero value when the
// name is not in the pool (an unknown tool is not an error here: the panel is showing a list
// it fetched earlier, and a name that has since left the pool just has nothing to show).
//
// It is a separate call rather than a field on MCPServerVO because the panel lists up to a
// hundred tools and fetches that list on every open (and after every authorization): the
// schemas would ride along every time for the one the user clicks.
func (s *AgentService) GetMCPToolSchema(name string) MCPToolSchemaVO {
	d := s.desk
	if d.mcp == nil || name == "" {
		return MCPToolSchemaVO{}
	}
	dt := d.mcp.Pool().Get(name)
	if dt == nil {
		return MCPToolSchemaVO{}
	}

	required := make(map[string]bool, len(dt.Schema.Parameters.Required))
	for _, r := range dt.Schema.Parameters.Required {
		required[r] = true
	}
	params := make([]MCPToolParamVO, 0, len(dt.Schema.Parameters.Properties))
	for pname, prop := range dt.Schema.Parameters.Properties {
		vo := MCPToolParamVO{
			Name:        pname,
			Type:        prop.Type,
			Required:    required[pname],
			Description: prop.Description,
			Enum:        prop.Enum,
			Default:     schemaValueText(prop.Default),
			ItemType:    schemaItemType(prop.Items),
		}
		if vo.Type == "" {
			// A $ref-only property stays typeless (see agent/mcp/tool.go's propertyType);
			// "any" is what that means to a reader, and an empty column reads as a bug.
			vo.Type = "any"
		}
		params = append(params, vo)
	}
	// Required first, then by name: the map above has no order, and a list that reshuffles
	// between two opens is worse than either order.
	sort.Slice(params, func(i, j int) bool {
		if params[i].Required != params[j].Required {
			return params[i].Required
		}
		return params[i].Name < params[j].Name
	})

	return MCPToolSchemaVO{
		Name:        dt.Name,
		ToolName:    strings.TrimPrefix(dt.Name, "mcp__"+dt.ServerName+"__"),
		Server:      dt.ServerName,
		Description: dt.Description,
		Parameters:  params,
	}
}

// schemaValueText renders a schema default for display: a string as itself, anything else as
// JSON (a default of `["a","b"]` should not print as Go's slice formatting).
func schemaValueText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

// schemaItemType digs the element type out of an array property's `items` node.
func schemaItemType(items any) string {
	m, ok := items.(map[string]any)
	if !ok {
		return ""
	}
	if t, ok := m["type"].(string); ok {
		return t
	}
	return ""
}

// notifyMCPRootsChanged tells the connected MCP servers that the process's workspace set moved.
//
// Called from the writers that move it — a project's roots, a session's folder or additional
// roots, and the moment a session's run appears (its tree joins the union mcpRoots answers) —
// because a roots-aware server asks ONCE and then trusts the answer until it is told otherwise
// (see Manager.NotifyRootsChanged for what a stale answer costs).
//
// Fire-and-forget on purpose: the caller is a UI gesture that has already succeeded, and a
// server that cannot be reached is not a reason to fail it. The context is detached for the same
// reason as the notification itself: it must not be cancelled with whatever request triggered it.
func (d *desktopApp) notifyMCPRootsChanged() {
	if d.mcp == nil {
		return
	}
	d.mcp.NotifyRootsChanged(context.Background())
}

// mcpRoots is what a roots-aware MCP server is told it may touch: every workspace this process
// currently has a session in, as file:// URIs.
//
// The answer is the process's, not one session's, and that follows from how the desktop shares
// MCP: ONE manager, one connection and one pool serve every session, so a server can only be
// given one roots list. Handing it the displayed session's tree alone would deny a background
// session's turn the moment the user switched away — the same failure this exists to fix, just
// narrower. The union is also the honest reading of "the directories this client is working
// in": every one of them is a directory the user opened here on purpose.
//
// The rules for turning that set into the answer itself (existing directories only, deduplicated,
// sorted) are mcp.RootsForDirs — shared with ACP, which answers for its single session.
func (d *desktopApp) mcpRoots() []mcp.Root {
	d.mu.Lock()
	ids := make([]string, 0, len(d.runs))
	for id := range d.runs {
		ids = append(ids, id)
	}
	d.mu.Unlock()

	dirs := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		primary, additional := d.sessionRootsFrom(d.sessionRecord(id))
		dirs = append(dirs, primary)
		dirs = append(dirs, additional...)
	}
	// The building rules (file:// URIs, existing directories only, deduplicated and sorted) live
	// in agent/mcp: ACP answers with the same shape for its one session, and two copies of that
	// rule would drift on exactly the cases that matter (a stale root, a reshuffled list).
	return mcp.RootsForDirs(dirs)
}
