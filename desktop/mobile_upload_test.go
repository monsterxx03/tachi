package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
)

// 手机发文件的宿主侧测试：字节落在**这段会话自己的目录**里，名字是发送方给的那个；发送时引用的
// 只能是这个目录里的名字（令牌不能被当成「读这台机器上任意文件」的能力）；气泡里读到的是文件名。

// mobileUploadFile posts one file the way a phone does: multipart, with the part's own filename
// and type. Nothing is base64'd, and nothing is read into the request twice.
func mobileUploadFile(t *testing.T, ts *httptest.Server, sessionID, token, name, mime string, data []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="file"; filename="` + name + `"`}
	if mime != "" {
		h["Content-Type"] = []string{mime}
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("multipart part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("multipart write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}
	req, err := http.NewRequest("POST", ts.URL+"/v1/sessions/"+sessionID+"/files", &buf)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// TestMobileUploadLandsInTheSession: an upload is stored beside the conversation it was sent in —
// deleting that conversation takes the file with it, and nothing is written into the user's
// workspace (where it would show up in their diff).
func TestMobileUploadLandsInTheSession(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	_, ts := mobileTestServer(t, d)

	code, out := mobileUploadFile(t, ts, s.ID, "secret", "照片 1.png", "image/png", pngTestBytes)
	if code != http.StatusOK {
		t.Fatalf("upload = %d %v, want 200", code, out)
	}
	file, _ := out["file"].(map[string]any)
	name, _ := file["name"].(string)
	// Directory parts are dropped, the name itself survives — it is what the phone shows back.
	if name != "照片 1.png" {
		t.Errorf("stored name = %q, want 照片 1.png", name)
	}
	if kind, _ := file["kind"].(string); kind != "image" {
		t.Errorf("kind = %q, want image", kind)
	}
	store := d.sessionStore()
	path := filepath.Join(store.BaseDir(), s.ID, "uploads", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stored file missing: %v", err)
	}
}

// TestMobileUploadRefusals: each refusal is its own answer, and the size ones carry the numbers —
// the phone shows that sentence, and the reader's only way out is knowing what hit the ceiling.
func TestMobileUploadRefusals(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	_, ts := mobileTestServer(t, d)

	if code, out := mobileUploadFile(t, ts, "nope", "secret", "a.txt", "text/plain", []byte("x")); code != http.StatusNotFound || out["error"] != "no_session" {
		t.Errorf("unknown session = %d %v, want 404 no_session", code, out)
	}
	if code, out := mobileUploadFile(t, ts, s.ID, "secret", "..", "text/plain", []byte("x")); code != http.StatusBadRequest || out["error"] != "bad_request" {
		t.Errorf("nameless upload = %d %v, want 400 bad_request", code, out)
	}
	if code, out := mobileUploadFile(t, ts, s.ID, "secret", "IMG.heic", "image/heic", []byte("heic")); code != http.StatusUnsupportedMediaType || out["error"] != "unsupported_image" {
		t.Errorf("heic = %d %v, want 415 unsupported_image", code, out)
	}

	// A photo over the model's own ceiling is refused at PICK time: stored, it would sit there
	// forever and never be looked at by anyone.
	big := make([]byte, llm.MaxImageSize+1)
	code, out := mobileUploadFile(t, ts, s.ID, "secret", "big.png", "image/png", big)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized image = %d %v, want 413", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "图片太大") {
		t.Errorf("oversized image reason = %q, want a sentence naming the limit", msg)
	}

	// A big document is held to the machine's own configured ceiling, not the image one.
	d.cfg.Mobile.MaxAttachmentMB = 1
	code, out = mobileUploadFile(t, ts, s.ID, "secret", "log.txt", "text/plain", make([]byte, 2*1024*1024))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized file = %d %v, want 413", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "文件太大") {
		t.Errorf("oversized file reason = %q, want a sentence naming the limit", msg)
	}
}

// TestMobileSendRefusesAnUploadItDoesNotHave: the names a send cites are resolved inside this
// session's uploads directory. Anything else — a traversal, a name that is not there — is refused
// rather than silently dropped: a message that loses its photo says nothing about it.
func TestMobileSendRefusesAnUploadItDoesNotHave(t *testing.T) {
	d, _ := newMobileApp(t)
	_, ts := mobileTestServer(t, d)
	s, err := d.sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	id := s.ID

	if _, code := d.mobileSend(id, "hi", "send", []string{"../../meta.json"}); code != "bad_request" {
		t.Errorf("traversal = %q, want bad_request", code)
	}
	if _, code := d.mobileSend(id, "hi", "send", []string{"gone.png"}); code != "no_such_upload" {
		t.Errorf("missing upload = %q, want no_such_upload", code)
	}
	// An attachment on its own is a message: no words needed.
	if _, code := d.mobileSend(id, "", "send", []string{"gone.png"}); code != "no_such_upload" {
		t.Errorf("attachment-only send = %q, want the upload resolved first (no_such_upload)", code)
	}
	// …but nothing at all is not.
	if _, code := d.mobileSend(id, "", "send", nil); code != "bad_request" {
		t.Errorf("empty send = %q, want bad_request", code)
	}
	_ = ts
}

// TestMobileUploadRefs pins what the model and the transcript each read: the reference is an
// ABSOLUTE path under this session's uploads (so @-expansion can resolve it), and the display is
// the name alone (so a phone screen does not have to show a path).
func TestMobileUploadRefs(t *testing.T) {
	d, sm := newMobileApp(t)
	s, err := sm.New("p", t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, _, code := d.mobileUpload(s.ID, "报告.pdf", "application/pdf", []byte("%PDF")); code != "" {
		t.Fatalf("upload: %s", code)
	}
	refs, status, code := d.mobileUploadRefs(s.ID, []string{"报告.pdf"})
	if code != "" {
		t.Fatalf("refs = (%d,%s)", status, code)
	}
	if len(refs) != 1 || !strings.HasPrefix(refs[0], "@"+string(filepath.Separator)) {
		t.Fatalf("refs = %v, want one absolute @-reference", refs)
	}

	full, shown := mobileMessageWithFiles("看看这个", refs, []string{"报告.pdf"})
	if !strings.HasPrefix(full, "看看这个\n\n@") {
		t.Errorf("model text = %q, want the words then the reference", full)
	}
	if shown != "看看这个\n\n📎 报告.pdf" {
		t.Errorf("display = %q, want the name, not the path", shown)
	}

	// A file-only message has no words to keep; the display is just the file line.
	full, shown = mobileMessageWithFiles("", refs, []string{"报告.pdf"})
	if !strings.HasPrefix(full, "@") || shown != "📎 报告.pdf" {
		t.Errorf("file-only message = (%q, %q)", full, shown)
	}
}

// pngTestBytes is a small valid PNG header: the store never inspects the bytes, only @-expansion
// does, and that has its own tests.
var pngTestBytes = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x01}

// TestMobilePingCarriesTheUploadCeiling: a phone refuses an over-limit file BEFORE spending the
// upload, and the only place it can learn the number is ping.
func TestMobilePingCarriesTheUploadCeiling(t *testing.T) {
	d, _ := newMobileApp(t)
	_, ts := mobileTestServer(t, d)
	_, out := mobileCall(t, ts, "GET", "/v1/ping", "", nil)
	mb, ok := out["maxUploadMb"].(float64)
	if !ok || int64(mb) != (config.MobileConfig{}).MaxAttachmentBytes()/(1024*1024) {
		t.Fatalf("ping maxUploadMb = %v, want the configured default in MB", out["maxUploadMb"])
	}
}
