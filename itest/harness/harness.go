// Package harness builds the isolated environment shared by the integration
// test suites: a --home directory with a minimal config.yaml pointing the
// default provider at a mockllm server, a seeded work directory for tool
// side effects, and the real tachi binary for process-boundary layers
// (itest/run, itest/acp).
//
// Isolation invariants (docs/2026-07-31-tui-integration-test.md §六):
//   - --home is a t.TempDir() — config/session/logs/memory all isolated.
//   - The config fixture is rendered AFTER the mock server starts (its random
//     port is only known then); title_generation is always off so the script
//     only faces the main loop's streaming calls.
//   - No MCP servers, no web search key, no channel config → zero outbound
//     network besides the mock.
package harness

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/session"
)

// configYAML renders the minimal provider fixture from a template. The
// base_url differs per protocol: go-openai concatenates baseURL +
// "/chat/completions" (needs the /v1 suffix) while anthropic-sdk-go appends
// a trailing slash and resolves "v1/messages" against it (needs the bare
// host) — Server.BaseURL() already returns the correct value per protocol.
// Optional sections (spec.timeout, a second provider for /model switches,
// bash permission rules) render only when the corresponding option is set;
// herdr is always pinned off to keep itests hermetic.
func configYAML(o *options, baseURL string) (string, error) {
	var buf bytes.Buffer
	if err := configTmpl.Execute(&buf, configData{
		Protocol:      o.protocol.String(),
		BaseURL:       baseURL,
		Timeout:       o.timeout,
		HasTimeout:    o.timeout > 0,
		SecondModel:   o.secondModel,
		BashAllow:     o.bashAllow,
		BashAsk:       o.bashAsk,
		Permissions:   o.bashAllow || o.bashAsk,
		NodeSection:   o.nodeSection(),
		NodeEnabled:   o.nodeEnabled,
		NodeListen:    o.nodeListen,
		NodeName:      o.nodeName,
		NodeWorkspace: o.nodeWorkspace,
		NodeTools:     o.nodeTools,
		Peers:         o.peers,
	}); err != nil {
		return "", fmt.Errorf("harness: render config template: %w", err)
	}
	return buf.String(), nil
}

// configData feeds the config fixture template.
type configData struct {
	Protocol    string
	BaseURL     string
	Timeout     time.Duration
	HasTimeout  bool
	SecondModel string // "" = no second provider
	BashAllow   bool
	BashAsk     bool
	Permissions bool // BashAllow || BashAsk — render the permissions block
	// NodeSection switches the whole `node:` block on, and it is NOT the same as NodeEnabled:
	// a machine that serves nothing can still have a node.name — that is the label it sends a
	// peer with every call, which is what a peer's records say about who asked (node/config.go).
	// Rendering that alone needs a `node:` section with something other than `enabled: true`
	// under it, so the block is rendered when ANY of its keys has something to say.
	NodeSection bool
	NodeEnabled bool // render `enabled: true` in that block
	// NodeListen is the REMOTE A2A surface's address, and the only switch that decides whether
	// this node serves TLS: off loopback it encrypts and requires a client certificate
	// (node/options.go isLoopbackListen, node/a2a.go). A spec that exercises mTLS therefore has
	// to name a non-loopback address here.
	NodeListen string
	// NodeName is what this node calls itself. It is a LABEL and travels as one — in the Card
	// and in the header a peer files under `self_reported_name` — so it is also the only way to
	// assert, from the other machine's disk, which node asked for the work.
	NodeName string
	// NodeWorkspace / NodeTools are the remote-task half of that section: the directory
	// remote turns run in, and the tool allowlist they get.
	NodeWorkspace string
	NodeTools     []string
	// Peers are the nodes THIS machine may delegate work to (config's top-level `peers:`). They
	// are the sending side's directory, so unlike the node: section they are independent of
	// whether this machine serves anything itself.
	Peers []peerFixture
}

// peerFixture is one `peers:` entry: the alias the model uses and the address of that node's
// A2A interface.
type peerFixture struct {
	Name string
	URL  string
}

// configTmpl renders the config.yaml fixture. The {{- ... }} trims keep the
// YAML free of blank lines when optional sections are absent.
var configTmpl = template.Must(template.New("config").Parse(`provider: mock
providers:
  - name: mock
    type: {{.Protocol}}
    model: mock-model
    base_url: {{.BaseURL}}
    api_key: test-key
    spec:
      context_window: 128000
{{- if .HasTimeout}}
      timeout: {{.Timeout}}
{{- end}}
{{- if .SecondModel}}
  - name: mock2
    type: {{.Protocol}}
    model: {{.SecondModel}}
    base_url: {{.BaseURL}}
    api_key: test-key
    spec:
      context_window: 128000
{{- if .HasTimeout}}
      timeout: {{.Timeout}}
{{- end}}
{{- end}}
title_generation: false
language: zh
herdr:
  enabled: false
{{- if .Permissions}}
permissions:
  bash:
{{- if .BashAllow}}
    allow:
      - "*"
{{- end}}
{{- if .BashAsk}}
    ask:
      - "*"
{{- end}}
{{- end}}
{{- if .Peers}}
peers:
{{- range .Peers}}
  - name: {{.Name}}
    url: {{.URL}}
{{- end}}
{{- end}}
{{- if .NodeSection}}
node:
{{- if .NodeEnabled}}
  enabled: true
{{- end}}
{{- if .NodeListen}}
  listen: "{{.NodeListen}}"
{{- end}}
{{- if .NodeName}}
  name: "{{.NodeName}}"
{{- end}}
{{- if .NodeWorkspace}}
  workspace: {{.NodeWorkspace}}
{{- end}}
{{- if .NodeTools}}
  tools:
{{- range .NodeTools}}
    - {{.}}
{{- end}}
{{- end}}
{{- end}}
`))

// TB is the minimal testing surface harness needs. Satisfied by *testing.T
// and by ginkgo.GinkgoT() (which does not implement testing.TB due to its
// private method).
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	TempDir() string
}

// Option configures the harness home.
type Option func(*options)

type options struct {
	protocol      mockllm.Protocol
	bashAllow     bool
	bashAsk       bool          // permissions.bash.ask: ["*"] → 强制 Bash 走权限流
	timeout       time.Duration // spec.timeout injected into the provider fixture
	secondModel   string        // 第二个 provider 的 model（/model 切换场景）
	nodeEnabled   bool          // node.enabled: true — 节点服务开关（默认关闭）
	nodeListen    string        // node.listen — 远程 A2A 面的地址（loopback 之外就serve TLS）
	nodeName      string        // node.name — 这个节点对自己的称呼
	nodeWorkspace string        // node.workspace — 远程任务的工作目录
	nodeTools     []string      // node.tools — 远程任务的工具白名单
	peers         []peerFixture // peers: — 本机可以向哪些节点委派（发送方目录）
}

// nodeSection reports whether the fixture needs a `node:` block at all. See configData.
func (o *options) nodeSection() bool {
	return o.nodeEnabled || o.nodeListen != "" || o.nodeName != "" ||
		o.nodeWorkspace != "" || len(o.nodeTools) > 0
}

// WithProtocol selects the mock's wire protocol (default openai); the config
// fixture's provider type follows automatically.
func WithProtocol(p mockllm.Protocol) Option {
	return func(o *options) { o.protocol = p }
}

// WithBashAllow adds permissions.bash.allow: ["*"] so Bash tool calls never
// hit an ask rule (or the judgment gate — an allow rule is what switches that
// off, see docs/agents/permission.md). Default bash policy is allow already, but
// this is a defense-in-depth pin for scenarios that must execute Bash.
func WithBashAllow() Option {
	return func(o *options) { o.bashAllow = true }
}

// WithBashAsk adds permissions.bash.ask: ["*"] so EVERY Bash command requires
// interactive approval — with PermissionModeExternal (ACP) that approval goes
// through the client's RequestPermission callback. Scenarios exercising the
// editor permission flow pass this (the default empty policy executes Bash
// without asking).
func WithBashAsk() Option {
	return func(o *options) { o.bashAsk = true }
}

// WithSpecTimeout injects spec.timeout (per-request LLM timeout) into the
// config fixture. Zero (default) leaves the spec without a timeout.
func WithSpecTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithSecondProvider adds a second provider entry (same wire protocol and
// base_url, different model) to the fixture — the /model switch target for
// ACP SetSessionConfigOption scenarios.
func WithSecondProvider(model string) Option {
	return func(o *options) { o.secondModel = model }
}

// WithNodeEnabled switches the node service on in the fixture (node.enabled:
// true). The default is off, and the point of most node scenarios is what OFF
// means, so this is opt-in.
func WithNodeEnabled() Option {
	return func(o *options) { o.nodeEnabled = true }
}

// WithNodeListen switches the node service on and points its REMOTE A2A surface at addr.
//
// It enables the node as well: an address with no listener behind it is an option that changes
// nothing, and silently doing nothing is worse than not offering it.
//
// This is the ONLY switch that decides whether that surface serves TLS — the rule is the address,
// not a flag (node/options.go isLoopbackListen): loopback is plaintext because nothing can read it
// off a network, and anything else encrypts and asks for a client certificate. So a spec that
// wants to exercise the mTLS path has to come through here ("0.0.0.0:0" is how a test names
// "off this machine, kernel-chosen port").
func WithNodeListen(addr string) Option {
	return func(o *options) { o.nodeEnabled = true; o.nodeListen = addr }
}

// WithNodeName sets node.name: what this node calls itself.
//
// It deliberately does NOT switch the node service on, and that asymmetry with WithNodeListen is
// the point: a machine that serves nothing still has a name, because the name is also what it
// sends a PEER with every call — the label the peer's records show as who asked (node/auth.go,
// taskRecord.SelfReportedName). A spec whose subject is "what the receiving side recorded about
// the caller" configures exactly this and nothing else.
func WithNodeName(name string) Option {
	return func(o *options) { o.nodeName = name }
}

// WithNodeWorkspace switches the node service on and points its workspace at dir: the
// directory remote tasks run in. It enables the node as well — a workspace without a node
// would be an option that changes nothing, and silently doing nothing is worse than not
// offering it.
func WithNodeWorkspace(dir string) Option {
	return func(o *options) { o.nodeEnabled = true; o.nodeWorkspace = dir }
}

// WithPeers adds nodes this machine may delegate work to (config's top-level `peers:`). It
// deliberately does NOT switch the local node service on: delegating and serving are two
// different halves, and a spec that only sends should not accidentally host.
func WithPeers(name, url string) Option {
	return func(o *options) { o.peers = append(o.peers, peerFixture{Name: name, URL: url}) }
}

// WithNodeTools switches the node service on and gives remote tasks this tool allowlist
// (config's node.tools). An empty list enables the node with NO tools, which is what the
// empty list means at runtime.
func WithNodeTools(names ...string) Option {
	return func(o *options) { o.nodeEnabled = true; o.nodeTools = names }
}

// NewHome creates an isolated --home directory with a config.yaml wired to
// the given mock server. The mock must be started first (its port is baked
// into the fixture).
func NewHome(t TB, mock *mockllm.Server, opts ...Option) string {
	t.Helper()
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	home := t.TempDir()
	cfg, err := configYAML(o, mock.BaseURL())
	if err != nil {
		t.Fatalf("harness: render config fixture: %v", err)
	}
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("harness: write config fixture: %v", err)
	}
	return home
}

// SeedWorkDir creates a fresh working directory (no .tachi.md, no .tachi/)
// and seeds the given files so tool outputs are assertable (e.g. a README.md
// for `ls` to find). Returns the directory path.
func SeedWorkDir(t TB, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("harness: seed dir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("harness: seed file %s: %v", name, err)
		}
	}
	return dir
}

// Config loads the config fixture from a --home directory exactly like the
// real binary does (config.SetBaseDir + config.Load), so the in-process TUI
// layer exercises the same config path as -p/ACP (which go through the
// binary). SetBaseDir mutates a process-global — safe here because ginkgo
// parallel nodes are separate OS processes and specs run serially within a
// node: each spec sets its own home before loading, and nothing else reads
// the global concurrently.
func Config(t TB, home string) *config.Config {
	t.Helper()
	config.SetBaseDir(home)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("harness: load config from %s: %v", home, err)
	}
	return cfg
}

// NewAgent assembles a real AIAgent the way runTUI (main.go) does: the
// config's default provider is resolved inside NewAIAgentWithConfig (its
// base_url points at the mock), the permission mode is TUI (interactive
// tool-confirmation events), and the session manager is wired to the home's
// sessions dir so session files land under --home. A per-home UsageRecorder
// is injected so the process-global ledger singleton never leaks across
// specs (it is created lazily from the FIRST home it sees and kept forever).
//
// The caller owns the returned agent: Close it when done (the itest/tui
// driver does this in DeferCleanup).
func NewAgent(t TB, cfg *config.Config) *agent.AIAgent {
	t.Helper()
	ai, _, err := agent.NewAIAgentWithConfig(context.Background(), agent.AgentConfig{
		Logger:         logger.New("tui"),
		PermissionMode: agent.PermissionModeTUI,
		FullConfig:     cfg,
		SystemConfig:   agent.SystemConfigFromConfig(cfg),
		UsageRecorder:  llm.NewUsageRecorder(filepath.Join(config.BaseDir(), "usage")),
	})
	if err != nil {
		t.Fatalf("harness: NewAIAgentWithConfig: %v", err)
	}
	sm, err := session.NewManager(nil)
	if err != nil {
		t.Fatalf("harness: session manager: %v", err)
	}
	sm.SetMaxKeep(cfg.SessionCleanupMaxCount)
	sm.CleanupOldSessions()
	ai.SetSessionManager(sm)
	return ai
}

// BuildBinary compiles the real tachi binary and returns its path. Call from
// BeforeSuite. The build runs from the repository root (located relative to
// this package — itest/harness → ../..) because the itest packages are all
// behind the integration build tag.
//
// When the worktree is CLEAN the binary is cached under os.TempDir() keyed
// by the git HEAD hash: every ginkgo -p worker runs BeforeSuite, and sharing
// one build avoids re-linking N times (4 procs = 4 links per suite). A DIRTY
// worktree (uncommitted edits) has no stable identity to key on, so it falls
// back to per-call builds — always correct, just not shared.
func BuildBinary(t TB) string {
	t.Helper()
	root := filepath.Clean(filepath.Join("..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("harness: locate repo root (expected go.mod in %s): %v", root, err)
	}

	if bin, ok := sharedBinary(root); ok {
		return bin
	}

	bin := filepath.Join(t.TempDir(), "tachi")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("harness: build tachi binary: %v\n%s", err, out)
	}
	return bin
}

// sharedBinary returns the cached binary path for a CLEAN worktree (keyed by
// git HEAD), building it once under os.TempDir() if missing. ok=false on a
// dirty worktree or when git is unavailable — callers fall back to a per-call
// build. Concurrent workers (ginkgo -p) may race on the first build; each
// builds to a pid-unique temp and atomically renames, and since the sources
// are identical every winner is equivalent.
func sharedBinary(root string) (string, bool) {
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", false
	}
	status, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil || len(status) > 0 {
		return "", false // dirty worktree: no stable identity to share on
	}

	bin := filepath.Join(os.TempDir(), "tachi-itest-"+strings.TrimSpace(string(head)))
	if _, err := os.Stat(bin); err == nil {
		return bin, true
	}

	tmp := fmt.Sprintf("%s.%d.tmp", bin, os.Getpid())
	cmd := exec.Command("go", "build", "-o", tmp, ".")
	cmd.Dir = root
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return "", false // fall through to the per-call build, which surfaces the error
	}
	if err := os.Rename(tmp, bin); err != nil {
		_ = os.Remove(tmp)
		return bin, true // a concurrent worker won the race — reuse its build
	}
	return bin, true
}
