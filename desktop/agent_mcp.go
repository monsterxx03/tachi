package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
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
	needsAuth := d.mcpNeedsAuthSnapshot()
	for _, srv := range d.cfg.MCPServers {
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
		out = append(out, vo)
	}
	return out
}

// mcpServerByName returns the configured server with this name, or nil.
func (d *desktopApp) mcpServerByName(name string) *config.MCPServerConfig {
	for i := range d.cfg.MCPServers {
		if d.cfg.MCPServers[i].Name == name {
			return &d.cfg.MCPServers[i]
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
	for _, a := range d.liveAgents() {
		a.NotifyDeferredToolsAdded()
	}
	d.markMCPNeedsAuth(srv.Name, false)
	return "ok", nil
}

// SetMCPServerEnabled connects (enabled=true) or disconnects (enabled=false) a
// configured MCP server at runtime. It does NOT persist to mcp.json — the
// change is runtime-only (restart reverts to config).
func (s *AgentService) SetMCPServerEnabled(name string, enabled bool) string {
	d := s.desk
	if d.cfg == nil || d.mcp == nil {
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
		// as already loaded.
		for _, a := range d.liveAgents() {
			a.UnregisterMCPServer(name)
		}
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
func (d *desktopApp) persistMCPServerEnabled(srv *config.MCPServerConfig, enabled bool) string {
	if srv.SourceFile == "" {
		return ""
	}
	if err := config.SetServerEnabledInFile(srv.SourceFile, srv.Name, enabled); err != nil {
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
	if d.cfg == nil || d.mcp == nil {
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

// mcpProjectRoot returns the project directory whose mcp.json /
// mcp.<profile>.json apply to the ACTIVE session, or "" when there is none.
//
// MCP's project scope is per TREE (config.LoadMCPConfig reads
// <workDir>/.tachi/mcp.json), but this process hosts several sessions in
// different trees, so the answer has to come from the session rather than from
// config.FindProjectRoot — that one is the PROCESS cwd, which for a
// Finder-launched app is "/", i.e. /.tachi/mcp.json: a file that never exists.
// The effect was silent: project-scoped servers and profiles simply never
// appeared, with the global scope still working, so nothing looked broken.
//
// Resolved through sessionRoots (the same exit the prompt, the @-file search,
// the checkpoint roots and the skill store use) so no two of them can describe
// different trees.
func (d *desktopApp) mcpProjectRoot() string {
	return d.sessionPrimaryDir(d.runManager(d.currentID()))
}

// runManager returns a session's own manager, or nil when the session has no
// run yet. Read-only accessors like the MCP panel need the manager to resolve
// a tree; turns and agent construction already hold theirs.
func (d *desktopApp) runManager(id string) *session.Manager {
	r := d.runOf(id)
	if r == nil {
		return nil
	}
	return r.sm
}

// ListMCPProfiles returns the active MCP profile and all available profiles
// (mcp.<name>.json in global/project scope).
//
// The project scope is the ACTIVE SESSION's tree, not the process cwd — see
// mcpProjectRoot for why that distinction is the whole point of this call.
func (s *AgentService) ListMCPProfiles() map[string]any {
	d := s.desk
	if d.cfg == nil {
		return nil
	}
	return map[string]any{
		"active":    d.cfg.ActiveMCPProfile,
		"available": config.ListMCPProfiles(d.mcpProjectRoot()),
	}
}

// SetMCPProfile switches the active MCP profile at runtime (reverts on restart).
// It uses the current session's agent (whose manager is the shared one) and
// reconciles the connected servers via SwitchMCPProfile.
//
// The workDir is the active session's tree for the same reason as
// ListMCPProfiles: a profile switch has to read the project's own
// mcp.<profile>.json, and reading it from the process cwd found nothing.
// SwitchMCPProfile validates the name against the very directory it is given,
// so passing one root for the check and another for the load would let a name
// that exists only globally pass as a project profile.
func (s *AgentService) SetMCPProfile(name string) string {
	d := s.desk
	if d.cfg == nil || d.mcp == nil {
		return "mcp not configured"
	}
	a, refuse := d.agentOf(d.currentID())
	if refuse != "" {
		return refuse
	}
	if _, err := a.SwitchMCPProfile(context.Background(), name, d.mcpProjectRoot()); err != nil {
		return err.Error()
	}
	return "ok"
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
	if d.cfg == nil || d.mcp == nil {
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
	if d.cfg == nil || d.mcp == nil {
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
	for i := range d.cfg.MCPServers {
		other := &d.cfg.MCPServers[i]
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
	if d.app == nil {
		return
	}
	d.app.Event.Emit("agent:mcp_auth", ev)
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
