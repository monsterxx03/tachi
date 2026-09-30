package main

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/mcp"
	"github.com/monsterxx03/tachi/agent/skill"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// initAgent bootstraps the real tachi config. It does NOT construct the agent
// itself — agents are built lazily per session (see prepareSession) so multiple
// conversations run independently, mirroring channel's per-thread cachedAgent.
//
// d.sm is always created (even when bootstrap fails) so the sidebar can list and
// browse on-disk sessions; on failure d.cfg stays nil and turns fall back to the
// simulated path.
func (d *desktopApp) initAgent(ctx context.Context) error {
	boot, err := agent.Bootstrap(ctx)
	if err != nil {
		d.setConfig(nil)
		d.sm = d.newSessionManager()
		return err
	}
	cfg := boot.Config
	d.setConfig(cfg)
	d.sm = d.newSessionManager()
	// The system prompt is deliberately NOT built here: it is session-derived
	// (working directory, ID), so systemPromptFor resolves it per turn — and that
	// builder is where the Mermaid capability of this frontend is declared.

	// Build the shared MCP manager (when any servers are configured) and connect
	// in the background. It is shared across all per-session agents; the
	// manager's per-session discovered sets keep each session's tool loading
	// isolated (mirrors channel's initSharedMCP).
	if cfg.MCPEnabled() {
		d.mcp = mcp.NewManager(ctx, cfg, logger.New("desktop"))
		// Every connection this manager opens now declares MCP's `roots` capability and
		// answers it with this process's workspaces (see mcpRoots). Without it a roots-aware
		// server — the official chrome-devtools one, for instance — confines file access to
		// the OS temp dir, and a tool that writes into the project fails with "not within any
		// of the configured workspace roots".
		d.mcp.SetRootsFunc(d.mcpRoots)
		go d.populateSharedMCP(ctx, d.mcp)
	}
	return nil
}

// systemPromptCacheMax pins the built-prompt memo (see systemPromptFor). Keys are
// the exact build inputs, so the pin only matters for a long-lived process that
// walks through many sessions and directories; when it is reached the map is
// dropped wholesale and rebuilt on demand.
const systemPromptCacheMax = 64

// promptKey identifies a built system prompt. Every part is a build input, so a
// miss and a change are the same event. roots is the joined filtered root set
// (order-sensitive: reordering the list is a different prompt).
type promptKey struct {
	cwd   string
	id    string
	roots string
}

// systemPromptFor returns the system prompt to send for session id, built from
// the session's CURRENT working directory, additional roots and ID.
//
// The prompt cannot be a startup singleton the way a single-session TUI's is
// (there the process cwd IS the session directory, thanks to os.Chdir). One
// desktop process hosts several sessions, each pointing at its own tree, and a
// GUI's process cwd is not a working directory at all — macOS hands a
// Finder-launched app "/" — so a prompt built once would advertise "/" forever
// while the tools (wdctx) and @-file references already resolved against the
// session's directory. Resolving on demand keeps the advertised directory, the
// tool root and the @-file root the same thing.
//
// The cache is keyed on the resolved (working directory, session ID, additional
// roots) triple, which is what makes a directory or ROOT change take effect on the
// next turn with no invalidation hook: either is a new key, so it rebuilds. The
// build is not free — it probes git (shutil in agent.buildSystemPrompt) — hence the
// memo for the unchanged case. A root whose directory has since vanished is not a
// key input either: it is filtered out before the key is formed (see promptRoots),
// so a deleted volume cannot make the prompt advertise a path that is not there.
//
// Callers must NOT hold d.mu: the working directory is read through it, and the
// build is far too slow to run under it.
//
// The Mermaid capability is declared here because this frontend renders diagrams
// (and a zoomable overlay for them). A session with no workspace yet is passed
// through as an explicit state (WithoutWorkingDir), never as the process cwd.
func (d *desktopApp) systemPromptFor(id string) string {
	cfg := d.config()
	if cfg == nil {
		return ""
	}
	// A session that has not chosen a workspace says exactly that, and nothing is
	// substituted: the process cwd of a GUI app is "/" (launched from Finder), so
	// filling it in would advertise the filesystem root as the working directory —
	// and invite the model to use absolute paths there. agent.WithoutWorkingDir turns
	// the empty string into a statement instead of "go find a project root".
	cwd := d.sessionWorkDir(id)
	roots := d.promptRoots(id)
	opts := []agent.PromptOption{agent.WithFrontendCapabilities(agent.MermaidCapabilityPrompt)}
	if cwd == "" {
		opts = append(opts, agent.WithoutWorkingDir())
	}
	key := promptKey{cwd: cwd, id: id, roots: strings.Join(roots, "\x00")}

	d.promptMu.Lock()
	cached, ok := d.promptCache[key]
	d.promptMu.Unlock()
	if ok {
		return cached
	}

	prompt := agent.BuildSystemPromptWithRoots(cfg.Language, cwd, roots, id, cfg.ExtraSystemPrompt, opts...)

	d.promptMu.Lock()
	defer d.promptMu.Unlock()
	if d.promptCache == nil || len(d.promptCache) >= systemPromptCacheMax {
		d.promptCache = make(map[promptKey]string)
	}
	d.promptCache[key] = prompt
	return prompt
}

// invalidateSystemPrompts drops EVERY built prompt, so the next turn rebuilds it.
//
// It exists for the two config keys that are part of the prompt but NOT part of the cache
// key — `language` and `extra_system_prompt`: the key holds per-session facts
// (cwd / roots / session id), so without this a saved language is served the prompt
// that was built under the old one, until the cache happens to fill up and drop itself
// (the only invalidation there used to be).
//
// A whole-map flush rather than a per-session one, because the change is global: that is the
// same granularity the capacity flush already uses, and one rebuild per open session is
// cheap next to a turn. See desktop/settings.go — this is the runtime half of those rows.
func (d *desktopApp) invalidateSystemPrompts() {
	d.promptMu.Lock()
	defer d.promptMu.Unlock()
	d.promptCache = nil
}

// populateSharedMCP connects all configured MCP servers in the background and
// fills the manager's deferred pool / per-session discovered sets. Errors are
// logged; partial discovery is acceptable (mirrors channel's populateSharedMCP).
func (d *desktopApp) populateSharedMCP(ctx context.Context, mgr *mcp.Manager) {
	defer mgr.MarkInitDone()
	_, _, errs := mgr.PopulateFromConnect(ctx, d.config())
	for _, err := range errs {
		// A server that answers "OAuth authorization required" is not a load error to
		// shrug off: it is the one failure the user can FIX from the panel, so it is
		// recorded where the panel reads it (NeedsAuth → the 需要授权 badge and the
		// 授权 button). Without this the badge would only appear after a manual toggle,
		// i.e. after the user had already tried the thing that cannot work.
		if oauthErr, ok := errors.AsType[*mcp.OAuthRequiredError](err); ok {
			d.markMCPNeedsAuth(oauthErr.ServerName, true)
			log.Printf("desktop MCP: %s requires OAuth authorization", oauthErr.ServerName)
			continue
		}
		log.Printf("desktop MCP: load error: %v", err)
	}
	// The pool is filled only NOW — after every server has answered — while a connection is
	// registered as soon as its own handshake and tools/list succeed. So a panel opened during
	// those seconds renders "已连接 with no tools" from a read that was true when it was taken,
	// and nothing else would ever correct it. This tells an open panel to re-read; a panel
	// opened later re-reads on its own (see App.tsx).
	//
	// Before that: the servers just connected were read against the BOOTSTRAP tree (the process
	// root — "/" for a Finder-launched app), so if the user is already in a session, re-point the
	// set at that session's tree now. MarkInitDone is called explicitly first (the deferred one
	// below is the panic net; the call is idempotent) because SwitchMCPProfile refuses while
	// initialization is still in flight — and this reconcile drives exactly that.
	mgr.MarkInitDone()
	d.reloadMCPForActiveTree()
	d.emitMCPReady()
}

// emitMCPReady reports that the shared pool will not grow by itself any more for this
// startup. Informational: the frontend's only use is to re-read the server list.
func (d *desktopApp) emitMCPReady() {
	if d.app == nil {
		return
	}
	d.app.Event.Emit("agent:mcp_ready", map[string]any{})
}

// newSessionManager creates a fresh session manager honoring the configured
// cleanup cap. It is the per-run analog of channel's newSessionManager.
//
// Every manager the desktop builds is built on the ONE store sessionStore returns:
// the store memoizes the session list, and a turn's own metadata write (its
// AppendMessage → UpdateMeta) is exactly the write that has to drop that memo
// before the sidebar reads again. A manager with a store of its own would hold a
// snapshot nothing else could invalidate.
func (d *desktopApp) newSessionManager() *session.Manager {
	store := d.sessionStore()
	if store == nil {
		return nil
	}
	sm := session.NewManagerWithStore(store, nil)
	// NewManagerWithStore starts uncapped; reproduce what NewManager would have
	// given, then let the config override it.
	sm.SetMaxKeep(session.DefaultMaxKeep)
	if cfg := d.config(); cfg != nil {
		sm.SetMaxKeep(cfg.SessionCleanupMaxCount)
	}
	return sm
}

// sessionStore returns the single FileStore the desktop reads and writes sessions
// through, creating it on first use.
//
// It is lazy because the earliest callers are turns, not startup, and a failure
// here must not take the app down: nil means "no sessions", the same degradation
// a failed bootstrap leaves behind.
func (d *desktopApp) sessionStore() *session.FileStore {
	d.storeMu.Lock()
	defer d.storeMu.Unlock()
	if d.store != nil {
		return d.store
	}
	dir, err := config.SessionDir()
	if err != nil {
		return nil
	}
	store, err := session.NewFileStore(dir)
	if err != nil {
		return nil
	}
	d.store = store
	return d.store
}

// buildAgentForSession constructs an AIAgent wired to the given session manager,
// using the shared desktop config/system prompt and the SHARED MCP manager (so
// multiple sessions reuse one MCP connection layer; per-session tool loading is
// isolated by the manager's discovered sets). It returns the agent; the caller
// applies provider/thinking overrides.
//
// sessionID is needed for the permission handler: a bash `ask` rule parks the
// agent goroutine on the user, and the answer must reach THAT conversation (see
// permission.go).
func (d *desktopApp) buildAgentForSession(ctx context.Context, sessionID string, sm *session.Manager) (*agent.AIAgent, error) {
	// One snapshot for the agent's whole configuration. The pointer is NOT swapped
	// afterwards (see withConfig), so the agent keeps seeing settings the user
	// changes later — that is the contract, not a race.
	cfg := d.config()
	// No iteration budget: this frontend is INTERACTIVE, like the TUI, ACP and channel, and
	// none of those cap a turn (main.go: "TUI is interactive — no iteration budget cap").
	// `max_iterations` in config.yaml is the SINGLE-SHOT knob — main_agent.go's `tachi -p` and
	// the one-off commands are the only places that read it — so reading it here quietly capped
	// desktop turns at 50 and ended them mid-work for no visible reason. The user is watching
	// and can Stop; a runaway loop is their call, not the config's.
	a, _, err := agent.NewAIAgentWithConfig(ctx, agent.AgentConfig{
		Logger: logger.New("desktop"),
		// PermissionModeExternal: this frontend is a client that has a human in
		// front of it, exactly like ACP — so a bash policy `ask` is decided by the
		// user in the app (permission.go) instead of being refused as it was under
		// PermissionModeSkip, whose `ask` branch is written for unattended runs
		// (channel / subagent / `tachi -p`). `deny` rules behave the same in both.
		PermissionMode: agent.PermissionModeExternal,
		AskUserEnabled: true,         // the desktop renders question forms itself
		DisableMCP:     d.mcp == nil, // MCP enabled only when a shared manager exists
		// Skills are ON (DisableSkills is the knob for the NON-interactive modes),
		// with the store built from THIS SESSION's tree rather than initSkills'
		// process cwd: one desktop process hosts several sessions in different trees
		// and its own cwd is meaningless (macOS hands a Finder-launched app "/"), so
		// a cwd-built store would scan "/.tachi/skills" and miss every session's
		// project skills. Same failure shape as the reminders below, same fix.
		SkillStore: d.sessionSkillStore(sm),
		// System reminders stay ON. They are the only thing that tells the agent
		// about the project it is working in (.tachi.md, git state) and the only
		// thing that keeps an active plan's step statuses current.
		//
		// They used to be disabled here, because the contextual ones resolved their
		// directory from the PROCESS — and a desktop process hosts many sessions in
		// different trees while its own working directory is meaningless (macOS hands
		// a Finder-launched app "/"). That is fixed: every contextual reminder now
		// reads the turn's working directory from the context (see
		// systemreminder.workDir), so they describe the session, not the process.
		MCPManager: d.mcp, // shared manager (nil when no MCP configured)

		// The checkpoint's root set must be the one the tools actually worked in. In the
		// desktop the session RECORD holds a snapshot (a project owns the real roots), so
		// the agent resolves through the same exit the prompt and wdctx do — without this a
		// project edit would leave each turn's snapshot covering the old tree while the
		// tools wrote in the new one, and a rewind would then "restore" files nobody was
		// working on (design §14.3).
		RootsFunc: d.checkpointRootsFor,

		// Plan mode's structured output. The desktop renders it as the footer's plan
		// panel, which is exactly the "plan card UI" this flag gates registration on.
		PlanToolEnabled: true,
		FullConfig:      cfg,
		SystemConfig:    agent.SystemConfigFromConfig(cfg),
	})
	if err != nil {
		return nil, err
	}
	// The project half of the bash permission rules comes from
	// <project>/.tachi/permissions.yaml, and configure's default resolves that
	// project from the PROCESS cwd — "/" for a Finder-launched app, so the file
	// it looked for was /.tachi/permissions.yaml and a project's own deny/ask
	// rules were silently absent. Re-point it at THIS session's tree, through the
	// same resolver the skill store above uses, so the two can never disagree
	// about which tree the session is in.
	//
	// Also re-applied at the start of every turn whose tree moved
	// (beginTurn's stale flag), because a store's and a policy's inputs are fixed
	// when they are built: changing the folder has to re-point both.
	a.SetPermissionRoot(ctx, d.sessionPrimaryDir(sm))
	// The ask decision belongs to this session's user. Installed after construction
	// because the handler reaches back into the agent for 「本会话全部允许」 (which
	// flips the session-wide switch on this agent's own policy).
	a.SetPermissionHandler(d.permissionHandler(sessionID, a))
	if sm != nil {
		a.SetSessionManager(sm)
	}
	return a, nil
}

// sessionSkillStore builds the skill store for one session's tree, which is the
// desktop's answer to "which skills does THIS conversation have?".
//
// A store's scan roots are fixed when it is built, so this is the only place the
// desktop decides them: global skills always, plus the project-level
// .tachi/skills, .claude/skills and .cursor/skills of the session's git root.
// The root comes from the session's primary workspace root and never from the process
// cwd (see buildAgentForSession); a session that has not picked a folder yet gets
// the global scope alone, which is what skill.NewStore("") means.
//
// Additional roots are deliberately NOT scanned: they are extra places to read and
// write, not a second configuration home — the same line @-references and other
// agents' per-directory configs draw. Moving a session is handled by
// SetSessionWorkingDir (agent_session.go), which re-points the store in place.
func (d *desktopApp) sessionSkillStore(sm *session.Manager) *skill.Store {
	return skill.NewStore(config.FindProjectRootFrom(d.sessionPrimaryDir(sm)))
}

// checkpointRootsFor is the agent-side mirror of sessionRootsFrom, handed to the agent as
// AgentConfig.RootsFunc so a checkpoint covers exactly the tree the tools worked in
// (design §14.3). It returns the record's own roots for a session in no project (or one
// whose project is gone), in the same shape the agent's own default uses — including the
// empty primary — so a plain session's snapshots are byte-for-byte what they were.
//
// Called from the agent's goroutine, per turn, so it must stay a pure READ of sess:
//   - It takes no d.mu (only the project table's own lock), which is what makes that safe:
//     the lock order is d.mu → projects.mu and this path can never invert it.
//   - sess is the per-session manager's LIVE record, and the desktop mutates that same
//     struct IN PLACE from the UI goroutine (updateSessionMeta). Reading fields here is
//     therefore only as safe as the writers are: today they are SetSessionWorkingDir /
//     AddSessionRoots / RemoveSessionRoot, and ProjectID has no writer at all yet. Whoever
//     adds the bind/unbind API has to keep this in mind — a field written from the UI while
//     a turn resolves its roots here is a data race the compiler will not flag. The fix
//     belongs in the manager (hand out copies, or mutate under its lock), not here.
func (d *desktopApp) checkpointRootsFor(sess *session.Session) []string {
	if sess == nil {
		return nil
	}
	primary, additional := d.sessionRootsFrom(sess)
	roots := make([]string, 0, 1+len(additional))
	roots = append(roots, primary)
	return append(roots, additional...)
}

// sessionPrimaryDir is the primary workspace root of the session the given (per-session)
// manager has loaded, or "" when there is none yet.
//
// It reads the manager the caller passes rather than d.runs: an agent is built BEFORE
// its run is bound to the session manager (see prepareSession), so the run lookup would
// still be empty here. The directory itself comes from the same resolver the prompt, the
// turn's wdctx and the @-file search use (sessionRootsFrom), so the skill store cannot
// end up pointed at a tree the rest of the session has left.
func (d *desktopApp) sessionPrimaryDir(sm *session.Manager) string {
	if sm == nil {
		return ""
	}
	primary, _ := d.sessionRootsFrom(sm.Current())
	return primary
}

// applyThinking configures the given agent's thinking level from a session's
// stored value. "" / "default" means DON'T set it — follow the provider's own
// default; "none" disables thinking. Mirrors SetThinkingLevel's semantics.
func applyThinking(a *agent.AIAgent, level string) {
	switch level {
	case "none":
		f := false
		a.SetThinking(&f, "")
	case "", "default":
		a.SetThinking(nil, "")
	default:
		t := true
		a.SetThinking(&t, level)
	}
}

// teardownAgent closes every per-session agent and the shared MCP manager,
// ensuring lifecycle events (e.g. session_end) are dispatched before exit.
func (d *desktopApp) teardownAgent() {
	d.mu.Lock()
	runs := make([]*sessionRun, 0, len(d.runs))
	for _, r := range d.runs {
		runs = append(runs, r)
	}
	mcp := d.mcp
	d.mu.Unlock()
	for _, r := range runs {
		if r.agent != nil {
			r.agent.Close()
		}
	}
	if mcp != nil {
		mcp.Close()
	}
}
