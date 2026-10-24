package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/monsterxx03/tachi/agent/atfile"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/fileutil"
	"github.com/monsterxx03/tachi/pkg/strutil"
	"github.com/monsterxx03/tachi/session"
)

// 手机发文件的宿主编（桌面进程与无头进程共用这一份）。
//
// 这条路刻意只做一半的事：把字节放到**这段会话自己的目录**里，并告诉手机它存下的名字。把它
// 送进对话是另一件事 —— 发送时那串名字被解析成 @-引用，展开、图片多模态、记录都走既有的那条
// 路（atfile.Expand，见 desktop/agent_turn.go），所以这里没有第二套「文件怎么给模型看」。

// isImageUpload tells whether the model could be SHOWN this file, from the type the phone reported
// or, when it reported none (a document picker often does not), the name's own extension.
//
// The set is llm's, not a copy: an image the model cannot be handed is not an image here either —
// which is what makes a .heic a refusal rather than a file nobody can look at.
func isImageUpload(mimeType, name string) bool {
	if _, ok := llm.ImageExtForMediaType(mimeType); ok {
		return true
	}
	_, ok := llm.ImageMediaType(filepath.Ext(name))
	return ok
}

// mobileUpload stores one file the phone sent and answers what the sender has to cite when the
// message goes out: the stored NAME, its size, and what @-expansion will make of it.
//
// The answer is a name and not a path on purpose. A path in the phone's hands would be a way to
// name any file on this machine (the same hole the attachment DOWNLOAD endpoint closes by taking
// an id instead of a path), so the send request carries names too and this side joins them onto
// the session's own uploads directory.
func (d *desktopApp) mobileUpload(sessionID, name, mimeType string, data []byte) (mobileUploadVO, int, string) {
	ss := d.sessionRecord(sessionID)
	if ss == nil {
		return mobileUploadVO{}, http.StatusNotFound, "no_session"
	}
	if ss.Kind != "" {
		// Same rule as sending: a session some other task owns is not the phone's to write into.
		return mobileUploadVO{}, http.StatusConflict, "read_only"
	}
	if len(data) == 0 {
		return mobileUploadVO{}, http.StatusBadRequest, "bad_request"
	}
	limit := d.mobileAttachmentLimit()
	if int64(len(data)) > limit {
		return mobileUploadVO{}, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("文件太大（%s，上限 %s）", strutil.HumanBytes(int64(len(data))), strutil.HumanBytes(limit))
	}
	// An image has its own, much smaller ceiling: it reaches the model as a content part, and a
	// photo over that limit would be stored and then never seen by anyone. Refusing it NOW is what
	// lets the reader pick another one while they still remember doing it — the same rule the
	// composer's paste follows.
	if isImageUpload(mimeType, name) {
		if int64(len(data)) > llm.MaxImageSize {
			return mobileUploadVO{}, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("图片太大（%s，上限 %s）", strutil.HumanBytes(int64(len(data))), strutil.HumanBytes(llm.MaxImageSize))
		}
	} else if isUnsupportedImage(name) {
		return mobileUploadVO{}, http.StatusUnsupportedMediaType, "unsupported_image"
	}
	store := d.sessionStore()
	if store == nil {
		return mobileUploadVO{}, http.StatusInternalServerError, "store_failed"
	}
	path, err := session.SaveUploadedFile(store.BaseDir(), sessionID, name, data)
	if err != nil {
		if errors.Is(err, session.ErrNoUploadName) {
			// The client sent no usable name (or one of "." / ".."). Its own sentence: a phone
			// picker always has a name, so reaching this means the request was not one.
			return mobileUploadVO{}, http.StatusBadRequest, "bad_request"
		}
		return mobileUploadVO{}, http.StatusInternalServerError, "store_failed"
	}
	kind, size := atfile.Classify(path)
	return mobileUploadVO{Name: filepath.Base(path), Size: size, Kind: kindName(kind)}, 0, ""
}

// isUnsupportedImage reports a photo the model cannot be shown: HEIC/HEIF, which is what an iPhone
// camera produces unless something converted it. Saying so at upload time is the only moment the
// reader can act on it — stored, it becomes a binary the agent is merely told to read.
func isUnsupportedImage(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".heic", ".heif":
		return true
	}
	return false
}

// mobileUploadRefs resolves the names a send cited into the @-references that carry them into the
// conversation.
//
// A name is resolved INSIDE this session's uploads directory and nowhere else: the caller only had
// to hold the token, and a token must not become "read any file". A name that is not there is
// refused rather than skipped — a message that quietly loses its photo says nothing about it, and
// the reader would only find out by looking at the model's reply.
func (d *desktopApp) mobileUploadRefs(sessionID string, names []string) (refs []string, status int, code string) {
	if len(names) == 0 {
		return nil, 0, ""
	}
	store := d.sessionStore()
	if store == nil {
		return nil, http.StatusInternalServerError, "store_failed"
	}
	dir, err := session.UploadDir(store.BaseDir(), sessionID)
	if err != nil {
		return nil, http.StatusNotFound, "no_session"
	}
	for _, name := range names {
		// The name comes from the phone, so it is checked as untrusted before it is joined: one
		// path element, and one that exists in THIS session's uploads.
		if !fileutil.IsSinglePathElement(name) {
			return nil, http.StatusBadRequest, "bad_request"
		}
		path := filepath.Join(dir, name)
		st, statErr := os.Stat(path)
		if statErr != nil || !st.Mode().IsRegular() {
			return nil, http.StatusConflict, "no_such_upload"
		}
		refs = append(refs, string(atfile.FileRefMark)+path)
	}
	return refs, 0, ""
}

// mobileMessageWithFiles builds the two strings one message needs once files are attached: what the
// MODEL reads (the user's words plus the @-references, which @-expansion turns into file contents)
// and what the TRANSCRIPT shows (the same words plus one line per file, by name).
//
// The display rule is stated here because the server writes the record — both screens read the
// wording back from it — and it is mirrored in the phone's local echo (mobile/src/state/attach.ts)
// so the bubble does not change shape a frame after it appears.
func mobileMessageWithFiles(text string, refs, names []string) (full, shown string) {
	full, shown = strings.TrimSpace(text), strings.TrimSpace(text)
	if len(refs) == 0 {
		return full, shown
	}
	if full != "" {
		full += "\n\n"
	}
	full += strings.Join(refs, " ")
	var lines []string
	for _, n := range names {
		lines = append(lines, "📎 "+n)
	}
	if shown != "" {
		shown += "\n\n"
	}
	shown += strings.Join(lines, "\n")
	return full, shown
}
