package tui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/clipboard"
	"github.com/monsterxx03/tachi/session"
)

// pngBytes is a small valid PNG: the magic number is what a pasted image is recognised by, and the
// bytes are what the stored file is compared against.
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52}

// stubClipboard replaces the clipboard read for one test and counts the reads.
func stubClipboard(t *testing.T, snap clipboard.Snapshot, err error) *int {
	t.Helper()
	calls := 0
	prev := readClipboard
	t.Cleanup(func() { readClipboard = prev })
	readClipboard = func(context.Context) (clipboard.Snapshot, error) {
		calls++
		return snap, err
	}
	return &calls
}

// modelWithSession gives the model an agent that has a current session under a temp store, which is what
// a real TUI has after its first turn. It returns the store root the pasted file must land under.
func modelWithSession(t *testing.T) (*Model, string, string) {
	t.Helper()
	// Both the store and the pasted file resolve through config, so the base dir is what keeps this
	// test out of the real ~/.tachi.
	config.SetBaseDir(t.TempDir())
	root, err := config.SessionDir()
	if err != nil {
		t.Fatalf("session dir: %v", err)
	}
	provider, err := llm.NewProvider("openai", "sk", "", "test-model")
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	a := newTestAIAgent(t, provider, 0)
	store, err := session.NewFileStore(root)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	sm := session.NewManagerWithStore(store, nil)
	if _, err := sm.New("openai", "/tmp"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	a.SetSessionManager(sm)
	m := testModel()
	m.agent = a
	return m, sm.Current().ID, root
}

// pasteInto drives the whole gesture: the keypress, the background read it returns, and the message
// that comes back. Anything less would test the handler without the binding that reaches it.
func pasteInto(t *testing.T, m *Model) {
	t.Helper()
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+v returned no command, want the clipboard read")
	}
	m.Update(cmd())
}

// TestCtrlVAttachesTheClipboardImage is the whole point of the feature: a screenshot on the
// clipboard reaches the input as a stored file and the @-reference that attaches it — the route an
// image takes to the model, because a terminal's paste channel cannot carry one.
func TestCtrlVAttachesTheClipboardImage(t *testing.T) {
	m, sessionID, root := modelWithSession(t)
	stubClipboard(t, clipboard.Snapshot{Image: pngBytes, MediaType: "image/png"}, nil)

	pasteInto(t, m)

	value := m.input.textarea.Value()
	if !strings.HasPrefix(value, "@") || !strings.HasSuffix(value, ".png") {
		t.Fatalf("input = %q, want an @-reference to a .png", value)
	}
	// The file belongs to the conversation it was pasted in — beside the session's own records, not
	// in the workspace — which is the layout the desktop's paste uses too.
	path := strings.TrimPrefix(value, "@")
	if want := filepath.Join(root, sessionID, "pasted") + string(filepath.Separator); !strings.HasPrefix(path, want) {
		t.Errorf("pasted file %q is not under the session's directory (%q)", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the pasted file: %v", err)
	}
	if !bytes.Equal(got, pngBytes) {
		t.Errorf("stored %d bytes, want the %d that were pasted", len(got), len(pngBytes))
	}
}

// TestPasteClipboardImageWithoutASessionStillAttaches: a fresh TUI has no session until its first
// turn, and a paste made before that must still reach the model — the file is only the carrier for
// the @-reference, while the bytes themselves are recorded with the message.
func TestPasteClipboardImageWithoutASessionStillAttaches(t *testing.T) {
	m := testModel() // no agent, hence no session
	stubClipboard(t, clipboard.Snapshot{Image: pngBytes, MediaType: "image/png"}, nil)

	pasteInto(t, m)

	value := m.input.textarea.Value()
	if !strings.HasPrefix(value, "@") || !strings.HasSuffix(value, ".png") {
		t.Fatalf("input = %q, want an @-reference to a .png", value)
	}
	path := strings.TrimPrefix(value, "@")
	t.Cleanup(func() { os.Remove(path) })
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pasted file: %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, pngBytes) {
		t.Errorf("stored %d bytes, want the %d that were pasted", len(got), len(pngBytes))
	}
}

// TestPasteClipboardTextFollowsThePastePath: with text on the clipboard, ctrl+v is the paste the
// terminal would have done — the large-paste collapse included, which is the two paths agreeing.
func TestPasteClipboardTextFollowsThePastePath(t *testing.T) {
	m := testModel()
	stubClipboard(t, clipboard.Snapshot{Text: "a\nb\nc\nd\ne\nf"}, nil)

	pasteInto(t, m)

	if got := m.input.textarea.Value(); got != "[Pasted 6 lines]" {
		t.Errorf("input = %q, want the large paste collapsed", got)
	}

	// A short paste is inserted as it is.
	m = testModel()
	stubClipboard(t, clipboard.Snapshot{Text: "hello"}, nil)
	pasteInto(t, m)
	if got := m.input.textarea.Value(); got != "hello" {
		t.Errorf("input = %q, want %q", got, "hello")
	}
}

// TestPasteOfAnEmptyClipboardChangesNothing: pressing paste with nothing copied is not an error,
// and it must not put a line in the transcript for it.
func TestPasteOfAnEmptyClipboardChangesNothing(t *testing.T) {
	m := testModel()
	stubClipboard(t, clipboard.Snapshot{}, nil)

	pasteInto(t, m)

	if got := m.input.textarea.Value(); got != "" {
		t.Errorf("input = %q, want it untouched", got)
	}
	if len(m.chatview.items) != 0 {
		t.Errorf("transcript got %d items, want none", len(m.chatview.items))
	}
}

// TestPasteOfAnOversizedImageIsRefused: refused BEFORE it is stored, while the screenshot is still
// on the reader's clipboard — a file over the model's limit would sit in the session forever and
// never be seen by anyone.
func TestPasteOfAnOversizedImageIsRefused(t *testing.T) {
	m := testModel()
	stubClipboard(t, clipboard.Snapshot{
		Image:     bytes.Repeat([]byte{0x89}, int(llm.MaxImageSize)+1),
		MediaType: "image/png",
	}, nil)

	pasteInto(t, m)

	if got := m.input.textarea.Value(); got != "" {
		t.Errorf("input = %q, want nothing attached", got)
	}
	if len(m.chatview.items) != 1 {
		t.Fatalf("transcript got %d items, want one line saying why", len(m.chatview.items))
	}
	if !strings.Contains(m.chatview.items[0].msg.Content, "太大") {
		t.Errorf("notice = %q, want it to say the image is too large", m.chatview.items[0].msg.Content)
	}
}

// TestEmptyPasteMsgReadsTheClipboard: a terminal that reports a paste carrying nothing is telling us
// the clipboard had no text — the one case it cannot deliver an image for. Text pastes keep going
// through the ordinary path and must not read the clipboard at all.
func TestEmptyPasteMsgReadsTheClipboard(t *testing.T) {
	m := testModel()
	calls := stubClipboard(t, clipboard.Snapshot{Image: pngBytes, MediaType: "image/png"}, nil)

	_, cmd := m.Update(tea.PasteMsg{})
	if cmd == nil {
		t.Fatal("an empty paste returned no command, want the clipboard read")
	}
	m.Update(cmd())
	if *calls != 1 {
		t.Errorf("clipboard reads = %d, want 1", *calls)
	}
	if got := m.input.textarea.Value(); !strings.HasPrefix(got, "@") {
		t.Errorf("input = %q, want the pasted image's reference", got)
	}

	m = testModel()
	calls = stubClipboard(t, clipboard.Snapshot{Image: pngBytes, MediaType: "image/png"}, nil)
	m.Update(tea.PasteMsg{Content: "typed text"})
	if *calls != 0 {
		t.Errorf("clipboard reads = %d, want none for a text paste", *calls)
	}
	if got := m.input.textarea.Value(); got != "typed text" {
		t.Errorf("input = %q, want the pasted text", got)
	}
}
