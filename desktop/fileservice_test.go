package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent/agentref"
	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/session"
)

// newTestApp builds a desktopApp with no runs: the @-file root then falls back
// to the process working directory, which each test pins with t.Chdir.
func newTestApp() *desktopApp {
	return &desktopApp{runs: make(map[string]*sessionRun), fileIndex: newFileIndex(), projects: &projectTable{}}
}

// newSessionManagerForTest returns a session manager rooted in a temp dir
// (config.SetBaseDir is a process global — it must never point at the real
// ~/.tachi) holding one current session bound to dir.
func newSessionManagerForTest(t *testing.T, dir string) *session.Manager {
	t.Helper()
	config.SetBaseDir(t.TempDir())
	sm, err := session.NewManager(nil)
	if err != nil {
		t.Fatalf("session manager: %v", err)
	}
	if _, err := sm.New("", dir); err != nil {
		t.Fatalf("new session: %v", err)
	}
	return sm
}

func writeTestFile(t *testing.T, dir, rel string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func requireRipgrep(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not installed")
	}
}

// TestSearchFilesRootsAtWorkingDir covers the empty-query case: the picker
// opens with the immediate entries of the session's working directory, which
// for a session without a bound agent is the process working directory.
func TestSearchFilesRootsAtWorkingDir(t *testing.T) {
	requireRipgrep(t)
	dir := t.TempDir()
	writeTestFile(t, dir, "readme.md")
	writeTestFile(t, dir, "src/main.go")
	t.Chdir(dir)

	svc := &AgentService{desk: newTestApp()}
	matches := svc.SearchFiles("session-without-run", "", 10)

	if len(matches) != 2 {
		t.Fatalf("expected 2 immediate entries, got %v", matches)
	}
	if matches[0].Path != "src" || !matches[0].IsDir {
		t.Errorf("expected directories first, got %+v", matches[0])
	}
	if matches[1].Path != "readme.md" || matches[1].IsDir {
		t.Errorf("expected readme.md second, got %+v", matches[1])
	}

	// A query searches the whole tree, not just the first level.
	fuzzy := svc.SearchFiles("session-without-run", "main", 10)
	if len(fuzzy) == 0 || fuzzy[0].Path != "src/main.go" {
		t.Errorf("expected src/main.go to match %q, got %+v", "main", fuzzy)
	}
}

// TestResolveDroppedPathsMapsReferences covers a file drop: paths inside the
// working directory become relative references, paths outside it stay absolute,
// and vanished paths are skipped.
func TestResolveDroppedPathsMapsReferences(t *testing.T) {
	root := t.TempDir()
	inside := writeTestFile(t, root, "notes/a.txt")
	t.Chdir(root)

	outside := writeTestFile(t, t.TempDir(), "pic.png")

	svc := &AgentService{desk: newTestApp()}
	got := svc.ResolveDroppedPaths("session-without-run", []string{
		inside,
		outside,
		filepath.Join(root, "gone.txt"),
	})

	if len(got) != 2 {
		t.Fatalf("expected the missing path to be skipped, got %+v", got)
	}
	if got[0].Ref != "@notes/a.txt" || got[0].Kind != "text" || got[0].IsDir {
		t.Errorf("wrong reference for an in-tree drop: %+v", got[0])
	}
	if want := "@" + filepath.ToSlash(outside); got[1].Ref != want {
		t.Errorf("expected an absolute reference %q, got %q", want, got[1].Ref)
	}
	if got[1].Kind != "image" {
		t.Errorf("expected the png to classify as an image, got %+v", got[1])
	}
}

// TestAtFileRootFollowsSessionWorkingDir pins the root used for BOTH the picker
// and @-reference expansion to the session's working directory, so what the
// popup offers is exactly what the agent will read.
func TestAtFileRootFollowsSessionWorkingDir(t *testing.T) {
	dir := t.TempDir()
	d := newTestApp()

	// No runs → process working directory fallback.
	if got, want := d.atFileRoot("unknown"), processCWD(); got != want {
		t.Errorf("expected the process cwd %q, got %q", want, got)
	}

	// A run bound to a session exposes that session's working directory.
	r := d.getRun("sid")
	r.sm = newSessionManagerForTest(t, dir)
	if got := d.atFileRoot("sid"); got != dir {
		t.Errorf("expected the session working dir %q, got %q", dir, got)
	}
}

// TestSavePastedImageStoresUnderTheSession pins the paste path end to end, as far as this layer can
// see it: the bytes land INSIDE the session's own directory (so a screenshot dies with the
// conversation it was sent in and never appears in the user's workspace, where their diff would
// show it), the answer is an @-reference — the same thing a dropped file gets — and it points at a
// file @-file expansion classifies as an image, which is how it becomes a multi-modal part for the
// model. Every refusal is a REASON rather than an empty answer: a paste that silently does nothing
// is the same class of bug as a message that silently goes nowhere.
func TestSavePastedImageStoresUnderTheSession(t *testing.T) {
	_, svc, sid, dir := newDeleteApp(t)

	// A real 1x1 PNG, base64 — the shape the webview sends (a data URL's payload).
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="

	got := svc.SavePastedImage(sid, "image/png", pngB64)
	if got.Error != "" {
		t.Fatalf("SavePastedImage: %s", got.Error)
	}
	path := strings.TrimPrefix(got.Ref, "@")
	if path == got.Ref {
		t.Fatalf("ref = %q, want an @-reference", got.Ref)
	}
	if want := filepath.Join(dir, "pasted") + string(filepath.Separator); !strings.HasPrefix(path, want) {
		t.Errorf("pasted file %q is not under the session's directory (%q)", path, want)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the pasted file: %v", err)
	}
	want, err := base64.StdEncoding.DecodeString(pngB64)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if !bytes.Equal(raw, want) {
		t.Errorf("stored %d bytes, want the %d that were pasted", len(raw), len(want))
	}
	if kind, _ := atfile.Classify(path); kind != atfile.KindImage {
		t.Errorf("atfile classifies the pasted file as %v, want an image — the expansion would not attach it", kind)
	}

	// A second paste must not overwrite the first: one file per paste, whatever the name.
	first := path
	if again := svc.SavePastedImage(sid, "image/png", pngB64); again.Error != "" || strings.TrimPrefix(again.Ref, "@") == first {
		t.Errorf("second paste = %+v, want its own file", again)
	}

	// Refusals, each naming what was wrong.
	for _, tc := range []struct{ name, session, media, data string }{
		{"not an image", sid, "application/pdf", pngB64},
		{"no session", "", "image/png", pngB64},
		{"undecodable", sid, "image/png", "not base64 at all!!"},
	} {
		if out := svc.SavePastedImage(tc.session, tc.media, tc.data); out.Error == "" {
			t.Errorf("%s: got %+v, want a reason", tc.name, out)
		}
	}
}

// useTestConfigBase points the config paths at a temp dir, so a test that reads (or writes) the
// peer records cannot touch the real ~/.tachi.
func useTestConfigBase(t *testing.T) {
	t.Helper()
	config.SetBaseDir(t.TempDir())
	t.Cleanup(func() { config.SetBaseDir("") })
}

// TestSearchAgentsDescribesEachNode pins what a #a: row says, because THREE frontends have to say
// the same thing about the same node: what the node is CALLED, the identity only THIS machine can
// vouch for — which is also what the row's reference and a tool argument carry — and what the last
// look found.
//
// Two of the three are refused when they are guessed. The identity comes from the TOFU record
// (written by a TLS handshake on an outbound call and nothing else), and a node this machine has
// never called has none — which is why such a node is not offered as a reference at all
// (agentref.Search), rather than being shown with the name the node reports about itself, which any
// key can claim. Reachability is a fact about a moment and nobody has looked yet, so the row says
// "检查中…" and never "offline".
func TestSearchAgentsDescribesEachNode(t *testing.T) {
	useTestConfigBase(t)
	cfg := config.DefaultConfig()
	cfg.Peers = []config.PeerConfig{
		{Name: "ugreen-nas", URL: "https://100.64.0.3:7080", Note: "家里的 NAS"},
		{Name: "hk-vps", URL: "https://100.64.0.9:7080"},
	}
	// Both machines have been reached, so both have an identity: the record is keyed by identity, and
	// a config line finds its own by the ADDRESS the pin was filed under.
	if err := fileutil.WriteFilePrivate(config.PeerIdentitiesPath(), []byte(`{
  "cd726b446c34386ece70d88fdd5fbf2a": {"alias": "ugreen-nas", "url": "https://100.64.0.3:7080", "first_seen": "2026-10-01T00:00:00Z"},
  "ab12cd34ab12cd34ab12cd34ab12cd34": {"alias": "hk-vps", "url": "https://100.64.0.9:7080", "first_seen": "2026-10-02T00:00:00Z"}
}`)); err != nil {
		t.Fatalf("write peer identities: %v", err)
	}

	app := newTestApp()
	app.setConfig(cfg)
	svc := &AgentService{desk: app}

	rows := svc.SearchAgents("", 0)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want one per nameable peer (a missing row reads as \"not configured\")", len(rows))
	}
	byName := map[string]AgentMatchVO{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	nas, ok := byName["ugreen-nas"]
	if !ok {
		t.Fatalf("no row for ugreen-nas: %+v", rows)
	}
	if nas.Ref != "#a:cd726b446c34386ece70d88fdd5fbf2a" {
		t.Errorf("ref = %q, want the identity the expansion reads back", nas.Ref)
	}
	if nas.NodeID != "cd726b446c34386ece70d88fdd5fbf2a" || nas.IDLabel != "cd726b44" {
		t.Errorf("identity = %q/%q, want the pinned id and its short form", nas.NodeID, nas.IDLabel)
	}
	if nas.Note != "家里的 NAS" {
		t.Errorf("note = %q, want the configured line", nas.Note)
	}
	if nas.Reach != agentref.ReachWord(atfile.AgentTarget{}) {
		t.Errorf("reach = %q, want %q — nobody has probed yet", nas.Reach, agentref.ReachWord(atfile.AgentTarget{}))
	}

	vps, ok := byName["hk-vps"]
	if !ok {
		t.Fatalf("no row for hk-vps: %+v", rows)
	}
	if vps.NodeID != "ab12cd34ab12cd34ab12cd34ab12cd34" || vps.IDLabel != "ab12cd34" {
		t.Errorf("identity = %q/%q, want the pinned id and its short form", vps.NodeID, vps.IDLabel)
	}
	if vps.Ref != "#a:ab12cd34ab12cd34ab12cd34ab12cd34" {
		t.Errorf("ref = %q, want the identity", vps.Ref)
	}

	// A query narrows by name, and the row's name is what gets searched.
	if narrowed := svc.SearchAgents("ugreen", 0); len(narrowed) != 1 || narrowed[0].Name != "ugreen-nas" {
		t.Errorf("SearchAgents(ugreen) = %+v, want just that node", narrowed)
	}
}

// TestPeerBoardShowsWhatThisMachineKnows pins the node board's content and its refresh: the alias
// and address this machine asked for, the identity pinned by the handshake, what the node answered
// — and a refresh that really asks again rather than re-reading its cache.
//
// The peer points at 127.0.0.1:1, where nothing listens: the failure is immediate (a refused
// connection, not a timeout), which keeps this a fast unit test instead of a fifteen-second one.
func TestPeerBoardShowsWhatThisMachineKnows(t *testing.T) {
	useTestConfigBase(t)
	cfg := config.DefaultConfig()
	cfg.Peers = []config.PeerConfig{{Name: "ugreen-nas", URL: "https://127.0.0.1:1", Note: "家里的 NAS"}}
	if err := fileutil.WriteFilePrivate(config.PeerIdentitiesPath(), []byte(`{
  "cd726b446c34386ece70d88fdd5fbf2a": {"alias": "ugreen-nas", "url": "https://127.0.0.1:1", "first_seen": "2026-10-01T00:00:00Z"}
}`)); err != nil {
		t.Fatalf("write peer identities: %v", err)
	}

	app := newTestApp()
	app.setConfig(cfg)
	svc := &AgentService{desk: app}

	board := svc.PeerBoard()
	if len(board.Peers) != 1 {
		t.Fatalf("board has %d rows, want one per configured peer: %+v", len(board.Peers), board)
	}
	row := board.Peers[0]
	if row.Name != "ugreen-nas" || row.Ref != "#a:cd726b446c34386ece70d88fdd5fbf2a" {
		t.Errorf("row = %+v, want what the node is called and the identity a tool argument takes", row)
	}
	if row.URL != "https://127.0.0.1:1" {
		t.Errorf("url = %q, want the address THIS machine dials (from the config)", row.URL)
	}
	if row.NodeID != "cd726b446c34386ece70d88fdd5fbf2a" || row.NodeIDLabel != "cd726b44" {
		t.Errorf("identity = %q/%q, want the pinned id and its short form", row.NodeID, row.NodeIDLabel)
	}
	if row.Note != "家里的 NAS" {
		t.Errorf("note = %q", row.Note)
	}

	// The refresh is a real round trip: nothing listens on that port, so the answer is "asked, and it
	// did not answer" — which is a different thing from "nobody has asked yet", and the row says so.
	refreshed := svc.RefreshPeerBoard()
	if len(refreshed.Peers) != 1 {
		t.Fatalf("refresh returned %d rows: %+v", len(refreshed.Peers), refreshed)
	}
	got := refreshed.Peers[0]
	if !got.Checked {
		t.Fatalf("after a refresh the node has been asked: %+v", got)
	}
	if got.Reachable || got.Card != nil {
		t.Errorf("nothing listens on that port: got %+v, want no card", got)
	}
	if got.Err == "" {
		t.Errorf("a node that did not answer has to say why: %+v", got)
	}
	if got.Reach != agentref.ReachWord(atfile.AgentTarget{Checked: true}) {
		t.Errorf("reach = %q, want the unreachable word", got.Reach)
	}
	if !refreshed.AnyChecked || refreshed.CheckedAt == "" {
		t.Errorf("the board must stamp when it looked: %+v", refreshed)
	}
}

// TestWarmPeersAsksTheNodesBeforeAnyoneLooks pins the startup warmup: a desktop that has just come
// up asks its configured nodes who they are, with nobody opening the picker or the board.
//
// The reason it matters is what reads a node's identity FIRST — the delegation reminder on the very
// first turn, and the rows of the picker someone is about to open — and that an identity only ever
// comes out of a handshake. Whether anyone has asked is a fact about a moment (Checked), which is
// what lets this test tell the warmup apart from a list that has been sitting there.
//
// The node points at 127.0.0.1:1, where nothing listens: refused at once, so the round finishes
// inside this test rather than timing out, and "asked, and it did not answer" is still an answer
// that was asked for (the same trick TestPeerBoardShowsWhatThisMachineKnows uses).
func TestWarmPeersAsksTheNodesBeforeAnyoneLooks(t *testing.T) {
	useTestConfigBase(t)
	cfg := config.DefaultConfig()
	cfg.Peers = []config.PeerConfig{{Name: "ugreen-nas", URL: "https://127.0.0.1:1"}}

	app := newTestApp()
	app.setConfig(cfg)

	before := app.peerList().Details()
	if len(before) != 1 || before[0].Checked {
		t.Fatalf("nobody has looked yet, so the list claims nothing: %+v", before)
	}

	app.warmPeers()

	// Asynchronous by construction — startup must not wait for a node that is down, slow, or gone —
	// so the answer lands in a later read of the same list.
	deadline := time.Now().Add(5 * time.Second)
	for {
		details := app.peerList().Details()
		if len(details) == 1 && details[0].Checked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the warmup has to ask every node on its own: %+v", details)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
