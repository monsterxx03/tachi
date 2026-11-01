package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/monsterxx03/tachi/pkg/strutil"
)

// HTTP 端点：命令走这里（能独立重试、能用 curl 调），过程走 WebSocket。
// 端点、请求体与返回形状以 docs/agents/mobile-protocol.md 为准。

// mobileVersion is what /v1/ping reports. The pairing page only uses it to show what answered;
// there is no compatibility negotiation in V1.
const mobileVersion = "0.1"

// pairingURLs is every address a phone can dial to reach this listener, primary first.
//
// It exists so the phone can SELF-HEAL: an address appears or disappears when a laptop joins a
// different network or starts Tailscale, and a phone that paired yesterday would otherwise never
// learn the new one. Ping is the one call every connection makes, so the list rides on it.
func (m *mobileServer) pairingURLs() []string {
	cands := mobileLANCandidates(m.pairingPort())
	want := ""
	if st, ok, _ := readMobileTokenFile(); ok {
		want = st.Interface
	}
	chosen := ""
	if c, ok := chooseMobileCandidate(cands, want); ok {
		chosen = c.URL
	}
	return mobilePairingURLs(cands, chosen)
}

func (m *mobileServer) handlePing(w http.ResponseWriter, r *http.Request) {
	// No token: the pairing page is verifying the address AND the token, so it has to be able
	// to reach something first. It reveals nothing a LAN scan would not.
	// hostname is what a phone with several saved computers labels this one with. It is the
	// MACHINE's name, not the product's: "name" answers "what is this app", which is the same string
	// on every install.
	// urls is the same set the QR code carried (see pairingURLs): a phone that has one address uses
	// this to learn the rest, so pairing once is enough even as networks come and go.
	mobileWriteJSON(w, map[string]any{
		"ok":       true,
		"name":     "tachi-desktop",
		"hostname": m.hostname,
		"version":  mobileVersion,
		"urls":     m.pairingURLs(),
		// deviceId is the machine's stable identity on the phone's side: a push notification
		// names a session ON A MACHINE, and the phone may remember several. It is the same value
		// the QR payload carries, so a phone paired a while ago learns it on its next ping.
		"deviceId": machineID(),
		// maxUploadMb is this machine's ceiling for a file the phone sends. It rides on ping
		// because a picker should be able to refuse a 90 MB video BEFORE spending the upload: the
		// number is the machine's configuration, and the phone has no way to know it otherwise.
		"maxUploadMb": m.desk.mobileAttachmentLimit() / (1024 * 1024),
	})
}

func (m *mobileServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	list, err := m.desk.mobileSessions()
	if err != nil {
		mobileFail(w, http.StatusInternalServerError, err.Error())
		return
	}
	mobileWriteJSON(w, map[string]any{"sessions": list})
}

func (m *mobileServer) handleProviders(w http.ResponseWriter, r *http.Request) {
	cfg := m.desk.config()
	type providerVO struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	out := make([]providerVO, 0)
	if cfg != nil {
		for _, p := range cfg.Providers {
			out = append(out, providerVO{Name: p.Name, Model: p.Model})
		}
	}
	mobileWriteJSON(w, map[string]any{"providers": out})
}

func (m *mobileServer) handlePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	before := 0
	if v := q.Get("before"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			mobileFail(w, http.StatusBadRequest, "bad_request")
			return
		}
		before = n
	}
	messages, hasMore, ss, err := m.desk.mobilePage(r.PathValue("id"), before, limit)
	if err != nil {
		mobileFail(w, http.StatusNotFound, "no_session")
		return
	}
	mobileWriteJSON(w, map[string]any{
		"messages": messages,
		"hasMore":  hasMore,
		"session":  m.desk.mobileSummary(ss, m.desk.busySessions()),
	})
}

type mobileMessageReq struct {
	Text string `json:"text"`
	Mode string `json:"mode"`
	// Attachments are the NAMES uploads answered with, in the order the reader attached them.
	// Never paths: the server joins each onto this session's own uploads directory, so holding the
	// token cannot name a file the phone never sent (docs/agents/mobile-protocol.md).
	Attachments []string `json:"attachments"`
}

func (m *mobileServer) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req mobileMessageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	if status, code := m.desk.mobileSend(r.PathValue("id"), req.Text, req.Mode, req.Attachments); code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

func (m *mobileServer) handleStop(w http.ResponseWriter, r *http.Request) {
	if status, code := m.desk.mobileStop(r.PathValue("id")); code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

// handleDeleteSession removes a conversation. The phone and the desktop share one delete path,
// and the list event it broadcasts brings every other screen back in step.
func (m *mobileServer) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if status, code := m.desk.mobileDeleteSession(r.PathValue("id")); code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

type mobileProviderReq struct {
	Name string `json:"name"`
}

func (m *mobileServer) handleProvider(w http.ResponseWriter, r *http.Request) {
	var req mobileProviderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	id := r.PathValue("id")
	if m.desk.sessionRecord(id) == nil {
		mobileFail(w, http.StatusNotFound, "no_session")
		return
	}
	if ss := m.desk.sessionRecord(id); ss != nil && ss.Kind != "" {
		mobileFail(w, http.StatusConflict, "read_only")
		return
	}
	if err := m.desk.switchProviderFor(id, req.Name); err != nil {
		// The reason itself is the useful thing (an unknown provider name names the name);
		// handing back a generic code would leave the reader nothing to act on.
		mobileFail(w, http.StatusConflict, err.Error())
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

type mobileSkipReq struct {
	Skip bool `json:"skip"`
}

func (m *mobileServer) handleSkipApproval(w http.ResponseWriter, r *http.Request) {
	var req mobileSkipReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	id := r.PathValue("id")
	ss := m.desk.sessionRecord(id)
	if ss == nil {
		mobileFail(w, http.StatusNotFound, "no_session")
		return
	}
	if ss.Kind != "" {
		mobileFail(w, http.StatusConflict, "read_only")
		return
	}
	if err := m.desk.setSkipApprovalFor(id, req.Skip); err != nil {
		mobileFail(w, http.StatusConflict, err.Error())
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

type mobileNotifyReq struct {
	Off bool `json:"off"`
}

// handleNotify flips this conversation's push switch from the phone. Same shape as
// skip-approval: the record is the truth, and the event it broadcasts updates every screen.
func (m *mobileServer) handleNotify(w http.ResponseWriter, r *http.Request) {
	var req mobileNotifyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	id := r.PathValue("id")
	ss := m.desk.sessionRecord(id)
	if ss == nil {
		mobileFail(w, http.StatusNotFound, "no_session")
		return
	}
	if ss.Kind != "" {
		mobileFail(w, http.StatusConflict, "read_only")
		return
	}
	if err := m.desk.setNotifyOffFor(id, req.Off); err != nil {
		mobileFail(w, http.StatusConflict, err.Error())
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

type mobilePermissionReq struct {
	Decision string `json:"decision"`
}

func (m *mobileServer) handlePermission(w http.ResponseWriter, r *http.Request) {
	var req mobilePermissionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	if status, code := m.desk.mobileAnswerPermission(r.PathValue("id"), r.PathValue("toolId"), req.Decision); code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

type mobileQuestionsReq struct {
	Answers map[string]string `json:"answers"`
	Decline bool              `json:"decline"`
}

func (m *mobileServer) handleQuestions(w http.ResponseWriter, r *http.Request) {
	var req mobileQuestionsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	if status, code := m.desk.mobileAnswerQuestions(r.PathValue("id"), req.Answers, req.Decline); code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

func (m *mobileServer) handleFile(w http.ResponseWriter, r *http.Request) {
	m.desk.serveMobileAttachment(w, r, r.PathValue("id"), r.PathValue("attachmentId"))
}

// handleUpload stores a file the phone sends into the session's own directory and answers the name
// to cite when the message goes out.
//
// multipart/form-data, not JSON+base64: a phone's picker hands over a FILE (a uri on disk), and
// multipart lets the platform stream it from there. Base64 would make the phone build a third
// larger copy in JavaScript memory before the first byte left — 67 MB for a 50 MB document, on the
// device least able to spare it. The server side is bounded either way (see below).
//
// The body is bounded BEFORE anything is read or spilled to disk: this listener is reachable by
// anything on the network that holds the token, and an unbounded body is free disk and memory to
// take. mobileUpload applies the configured limit itself, once, so the two cannot drift apart.
func (m *mobileServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	limit := m.desk.mobileAttachmentLimit()
	// The extra megabyte is multipart framing (boundaries, headers), never file bytes.
	r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)
	overLimit := func(err error) bool {
		var tooBig *http.MaxBytesError
		return errors.As(err, &tooBig) || strings.Contains(err.Error(), "request body too large")
	}
	// 32 MiB stays in memory, the rest spills to a temp file — the parse itself must not be the
	// thing that decides how much memory an upload costs.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		if overLimit(err) {
			mobileFail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("文件太大（上限 %s）", strutil.HumanBytes(limit)))
			return
		}
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	f, header, err := r.FormFile("file")
	if err != nil {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		if overLimit(err) {
			mobileFail(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("文件太大（上限 %s）", strutil.HumanBytes(limit)))
			return
		}
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	// The part's own filename and type: that is what the picker called the file, and what the
	// phone shows back while the message is being written.
	vo, status, code := m.desk.mobileUpload(r.PathValue("id"), header.Filename, header.Header.Get("Content-Type"), raw)
	if code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"file": vo})
}

type mobileTitleReq struct {
	Title string `json:"title"`
}

// handleRename is the phone's 重命名. The row's title is a list fact, so the success answer is the
// usual accepted + the sessions_changed broadcast every screen re-reads.
func (m *mobileServer) handleRename(w http.ResponseWriter, r *http.Request) {
	var req mobileTitleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	if status, code := m.desk.mobileRename(r.PathValue("id"), req.Title); code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

type mobilePinReq struct {
	Pinned bool `json:"pinned"`
}

// handlePin is the phone's 置顶 / 取消置顶. The row's position is a list fact, so the success
// answer is the usual accepted + the sessions_changed broadcast every screen re-reads.
func (m *mobileServer) handlePin(w http.ResponseWriter, r *http.Request) {
	var req mobilePinReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	if status, code := m.desk.mobileSetPinned(r.PathValue("id"), req.Pinned); code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

type mobileNewSessionReq struct {
	ProjectID  string `json:"projectId"`
	WorkingDir string `json:"workingDir"`
}

// handleCreateSession creates a conversation from the phone and answers with the row the list
// would show, so the caller does not have to wait for its own sessions_changed to see it.
func (m *mobileServer) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req mobileNewSessionReq
	// An empty body is legal: it means "无项目、默认工作目录".
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			mobileFail(w, http.StatusBadRequest, "bad_request")
			return
		}
	}
	row, status, code := m.desk.mobileCreateSession(req.ProjectID, req.WorkingDir)
	if code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"session": row})
}

// handlePanels answers the session's MCP / agent / scheduled-job panels. All three are reads.
func (m *mobileServer) handlePanels(w http.ResponseWriter, r *http.Request) {
	panels, status, code := m.desk.mobileSessionPanels(r.PathValue("id"))
	if code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"panels": panels})
}

type mobilePauseReq struct {
	Paused bool `json:"paused"`
}

// handleCronRun / handleCronPause / handleCronDelete are the phone's three actions on one
// scheduled job. They go through the SAME writers the window uses, and each one emits
// cron_changed — which is how the other screen hears about it.
func (m *mobileServer) handleCronRun(w http.ResponseWriter, r *http.Request) {
	if status, code := m.desk.mobileCronRun(r.PathValue("id")); code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

func (m *mobileServer) handleCronPause(w http.ResponseWriter, r *http.Request) {
	var req mobilePauseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	if status, code := m.desk.mobileCronPause(r.PathValue("id"), req.Paused); code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

func (m *mobileServer) handleCronDelete(w http.ResponseWriter, r *http.Request) {
	if status, code := m.desk.mobileCronDelete(r.PathValue("id")); code != "" {
		mobileFail(w, status, code)
		return
	}
	mobileWriteJSON(w, map[string]any{"accepted": true})
}

// handleProjects lists what the phone's project picker offers.
func (m *mobileServer) handleProjects(w http.ResponseWriter, r *http.Request) {
	mobileWriteJSON(w, map[string]any{"projects": m.desk.mobileProjects()})
}

// ── 保持唤醒 ─────────────────────────────────────────────────────────────────
//
// 这台电脑的一个开关，不属于任何一段会话（awake.go）。两个端点都没有 id：托盘的勾选和这里是
// 同一个值的两块屏，谁改的都会经 awake_changed 落到另一块上。

// handleAwake answers the switch's current state, which is what the phone's card draws.
func (m *mobileServer) handleAwake(w http.ResponseWriter, r *http.Request) {
	mobileWriteJSON(w, map[string]any{"awake": m.desk.awakeState()})
}

type mobileAwakeReq struct {
	On bool `json:"on"`
	// DurationMin is optional: omit it to keep the stored preference (and let a 开启 use it as it
	// is). Send it to change the preference AND this 开启's length — that is what the phone's
	// 时长 chips do.
	DurationMin *int `json:"durationMin"`
}

// handleSetAwake is the phone's half of the switch. The write goes through the same
// desktopApp.setAwake the tray menu calls, so neither screen can invent a state the other does
// not have; the answer is the state the machine is ACTUALLY in, which is off when the operating
// system refused the assertion.
func (m *mobileServer) handleSetAwake(w http.ResponseWriter, r *http.Request) {
	var req mobileAwakeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	st, err := m.desk.setAwake(req.On, req.DurationMin)
	if err != nil {
		if errors.Is(err, errAwakeDuration) {
			mobileFail(w, http.StatusBadRequest, "bad_request")
			return
		}
		// The machine (or its desktop environment) would not hold the assertion. The phone shows
		// "这台电脑没能保持唤醒" and re-reads, so its switch falls back to off instead of lying.
		mobileFail(w, http.StatusInternalServerError, "awake_failed")
		return
	}
	mobileWriteJSON(w, map[string]any{"awake": st})
}
