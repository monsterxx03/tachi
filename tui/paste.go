package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/clipboard"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/session"
)

// readClipboard is pkg/clipboard's read, injectable so the paste path can be tested without a
// clipboard (the same seam agent's oneoff recorder uses for its session dir).
var readClipboard = clipboard.Read

// clipboardPasteMsg carries one read of the system clipboard back to Update.
//
// The read runs as a tea.Cmd — it shells out (pbpaste, osascript, wl-paste) and must not block the
// UI goroutine — and everything after it is the Model's, synchronously, like any other message.
type clipboardPasteMsg struct {
	snap clipboard.Snapshot
	err  error
}

// pasteClipboard reads the system clipboard in the background.
//
// The TUI reads it ITSELF because a terminal's paste channel carries text: with a screenshot on the
// clipboard there is nothing for the terminal to send, so Cmd+V produces no event at all (the
// terminal reports the paste as not performable) and the image can never arrive however the gesture
// is mapped. ctrl+v is the key that asks for this read, and it is also the key that makes plain
// text paste work in terminals that never bound ctrl+v themselves.
func (m *Model) pasteClipboard() tea.Cmd {
	return func() tea.Msg {
		snap, err := readClipboard(context.Background())
		return clipboardPasteMsg{snap: snap, err: err}
	}
}

// handleClipboardPaste turns one clipboard read into input: an image is stored and attached by
// reference, text goes through the ordinary paste path — the large-paste collapse and the
// dragged-file detection included, so ctrl+v behaves exactly like a terminal paste for text.
func (m *Model) handleClipboardPaste(msg clipboardPasteMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		// A clipboard this platform cannot read at all (no wl-paste/xclip) is worth a log line and
		// nothing more: the reader asked for a paste and there is nothing honest to show them.
		m.logger.Warn(context.Background(), "tui: could not read the clipboard", "err", msg.err)
		return m, nil
	}
	if len(msg.snap.Image) > 0 {
		m.attachClipboardImage(msg.snap)
		return m, nil
	}
	if msg.snap.Text == "" {
		// Paste with an empty clipboard: not an error, and not worth a line in the transcript.
		m.logger.Info(context.Background(), "tui: clipboard holds nothing to paste")
		return m, nil
	}

	oldHeight := m.input.Height()
	var cmd tea.Cmd
	m.input, cmd = m.input.handlePaste(msg.snap.Text)
	if m.input.Height() != oldHeight {
		m.layout()
	}
	return m, cmd
}

// attachClipboardImage stores the pasted image and inserts the @-reference that attaches it.
//
// The reference is the route an image takes to the model in this UI — the same one the desktop's
// paste and a dropped file take: @-file expansion turns it into a multi-modal content part at send
// time (atfile.Expand → WithPendingImages), and the reader keeps seeing what they sent.
func (m *Model) attachClipboardImage(snap clipboard.Snapshot) {
	// Refused BEFORE storing, like the desktop does it: a file over the model's limit would sit in
	// the session forever and never be seen by anyone, so the reader is better told now, while the
	// screenshot is still on their clipboard.
	if int64(len(snap.Image)) > llm.MaxImageSize {
		m.noticeText("粘贴的图片太大（" + strutil.HumanBytes(int64(len(snap.Image))) +
			"，上限 " + strutil.HumanBytes(llm.MaxImageSize) + "）")
		return
	}
	ext, ok := llm.ImageExtForMediaType(snap.MediaType)
	if !ok {
		m.noticeText("粘贴的不是支持的图片（支持 png / jpg / gif / webp）")
		return
	}
	// A fresh TUI has no session yet (the first turn creates it), and SavePastedImage handles that
	// by falling back to the temp directory — the bytes still reach the session with the message.
	path, err := session.SavePastedImage(m.currentSessionID(), ext, snap.Image)
	if err != nil {
		m.logger.Error(context.Background(), "tui: failed to store a pasted image", err)
		m.noticeText("保存粘贴的图片失败：" + err.Error())
		return
	}

	oldHeight := m.input.Height()
	m.input.InsertText("@" + path)
	if m.input.Height() != oldHeight {
		m.layout()
	}
	m.logger.Info(context.Background(), "tui: pasted an image", "path", path, "bytes", len(snap.Image))
}

// noticeText says something the paste could not do, in the transcript — where this UI already says
// things like it — because the input area has nowhere to put a line of its own.
func (m *Model) noticeText(text string) {
	m.chatview.AddMessage(chatMessage{Role: "assistant", Content: text})
}
