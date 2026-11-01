package main

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/logger"
)

// Server 是那个假服务端：内存里几个会话 + 一条广播出去的事件流。
type Server struct {
	mu      sync.Mutex
	token   string
	order   []string
	byID    map[string]*session
	clients map[*client]struct{}
	log     *logger.Logger
	// cron 是假服务端的定时任务表：手机上那三个动作要改得动它，面板才看得出同步
	cron []map[string]any
	// awake 是这台「电脑」的保持唤醒开关。它不属于任何会话，所以和 cron 一样是一份独立状态，
	// 由 POST /v1/awake 改、由 awake_changed 广播 —— 手机端的卡片在两个假服务端之间也能被验证。
	awake AwakeState
}

// AwakeState 与 docs/agents/mobile-protocol.md 的线上类型一字不差。
type AwakeState struct {
	On bool `json:"on"`
	// DurationMin 是偏好（0 = 不自动关），Until 是这次开着时到点自动关的时刻
	DurationMin int    `json:"durationMin"`
	Until       string `json:"until"`
}

// 时长的默认与边界，与桌面 awake.go 同一套（默认 3 小时，0 = 不自动关）
const (
	mockAwakeDefaultMinutes = 180
	mockAwakeMinMinutes     = 5
	mockAwakeMaxMinutes     = 24 * 60
)

func New(token string) *Server {
	order, byID := seed()
	return &Server{
		token:   token,
		order:   order,
		byID:    byID,
		clients: map[*client]struct{}{},
		log:     logger.New("mockapi"),
		awake:   AwakeState{DurationMin: mockAwakeDefaultMinutes},
		cron: []map[string]any{
			{"id": "c1", "name": "每日收盘估值", "scheduleText": "每个工作日 15:30", "status": "active",
				"targetKind": "session", "targetTitle": "索引估值提醒", "nextRun": "2026-10-20T15:30:00+08:00",
				"lastRunStatus": "complete", "lastRunError": "", "running": false},
			{"id": "c2", "name": "清理旧日志", "scheduleText": "每天 04:00", "status": "paused",
				"targetKind": "missing", "targetTitle": "", "nextRun": "",
				"lastRunStatus": "error", "lastRunError": "会话已经不在了", "running": false},
			// pending：专属会话还没创建（第一次运行才建）。它和 missing 一样没有标题，
			// 所以这一行是「没有标题不等于会话没了」那条规则在种子数据里的活样本。
			{"id": "c3", "name": "每日站会总结", "scheduleText": "每个工作日 09:30", "status": "active",
				"targetKind": "pending", "targetTitle": "", "nextRun": "2026-10-21T09:30:00+08:00",
				"lastRunStatus": "", "lastRunError": "", "running": false},
		},
	}
}

// ---- 小工具 ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail 用协议里那张错误码表：手机拿到的 code 决定它显示哪句话
func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func (s *Server) get(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id]
}

// ---- 路由 ------------------------------------------------------------------

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", s.handlePing)
	mux.HandleFunc("GET /v1/ws", s.handleWS)
	mux.HandleFunc("GET /v1/sessions", s.guard(s.handleSessions))
	mux.HandleFunc("POST /v1/sessions", s.guard(s.handleCreateSession))
	mux.HandleFunc("GET /v1/projects", s.guard(s.handleProjects))
	mux.HandleFunc("GET /v1/providers", s.guard(s.handleProviders))
	mux.HandleFunc("GET /v1/awake", s.guard(s.handleAwake))
	mux.HandleFunc("POST /v1/awake", s.guard(s.handleSetAwake))
	mux.HandleFunc("GET /v1/sessions/{id}/page", s.guard(s.handlePage))
	mux.HandleFunc("GET /v1/sessions/{id}/files/{attachmentId}", s.guard(s.handleFile))
	mux.HandleFunc("POST /v1/sessions/{id}/files", s.guard(s.handleUpload))
	mux.HandleFunc("GET /v1/sessions/{id}/panels", s.guard(s.handlePanels))
	mux.HandleFunc("POST /v1/sessions/{id}/messages", s.guard(s.handleMessage))
	mux.HandleFunc("POST /v1/sessions/{id}/stop", s.guard(s.handleStop))
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.guard(s.handleDeleteSession))
	mux.HandleFunc("POST /v1/sessions/{id}/pin", s.guard(s.handlePin))
	mux.HandleFunc("POST /v1/sessions/{id}/title", s.guard(s.handleRename))
	mux.HandleFunc("POST /v1/cron/{id}/run", s.guard(s.handleCronRun))
	mux.HandleFunc("POST /v1/cron/{id}/pause", s.guard(s.handleCronPause))
	mux.HandleFunc("DELETE /v1/cron/{id}", s.guard(s.handleCronDelete))
	mux.HandleFunc("POST /v1/sessions/{id}/provider", s.guard(s.handleProvider))
	mux.HandleFunc("POST /v1/sessions/{id}/skip-approval", s.guard(s.handleSkipApproval))
	mux.HandleFunc("POST /v1/sessions/{id}/permissions/{toolId}", s.guard(s.handlePermission))
	mux.HandleFunc("POST /v1/sessions/{id}/questions", s.guard(s.handleQuestions))
	return allowWebPreview(mux)
}

// allowWebPreview 给浏览器里的界面补上 CORS。
//
// 只属于这个假服务端：`npm run web` 的页面在 8082 上，浏览器会拦掉它发往 9797 的 fetch（手机
// App 不跨域，所以真服务端不需要这些头——那边开 CORS 等于让任意网页读手机接口）。OPTIONS 必须
// 在最外层答，因为 guard 会因为它没带令牌而拒掉预检。
//
// WebSocket 不受 CORS 约束，mockapi 的 upgrader 本来就放开 Origin（ws.go）。
func allowWebPreview(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guard 检查 Bearer 令牌；比较是常量时间的（协议里那条：令牌是凭据）
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !constTimeEqual(got, s.token) {
			fail(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func constTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// ---- 端点 ------------------------------------------------------------------

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	// ping 不要令牌：配对页就是拿它来验证地址与令牌的，先要能连上
	//
	// urls 是这台机器对外的全部地址（协议里那一项）。假服务端只有一条 —— 就是请求自己用的那条，
	// 因为它本来就在这个地址上应答；手机端照样能走到「服务端告诉我地址表」这条路。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"name":     "tachi-mockapi",
		"hostname": "mock-machine",
		"version":  "0.1",
		"urls":     []string{"http://" + r.Host},
		// 上传上限跟着真服务端那份配置走；假服务端取默认的 50 MB
		"maxUploadMb": 50,
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 置顶在前，其余保持原来的次序 —— 与真服务端同一条规矩（协议：pinned first, then newest）
	ids := make([]string, 0, len(s.order))
	for _, id := range s.order {
		if s.byID[id].Session.Pinned {
			ids = append(ids, id)
		}
	}
	for _, id := range s.order {
		if !s.byID[id].Session.Pinned {
			ids = append(ids, id)
		}
	}
	out := make([]SessionSummary, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.byID[id].Session)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// handleProjects 是新建会话时那个项目下拉框的数据源
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"projects": []map[string]any{
		{"id": "p-tachi", "name": "tachi", "primary": "~/repos/tachi", "runnable": true},
		{"id": "p-notes", "name": "笔记", "primary": "~/notes", "runnable": true},
		{"id": "p-gone", "name": "已删除的项目", "primary": "~/gone", "runnable": false},
	}})
}

// handlePanels 回这段会话的三个只读面板。假数据，形状与真服务端一致。
func (s *Server) handlePanels(w http.ResponseWriter, r *http.Request) {
	if s.get(r.PathValue("id")) == nil {
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	s.mu.Lock()
	cron := make([]map[string]any, 0, len(s.cron))
	for _, j := range s.cron {
		cp := make(map[string]any, len(j))
		for k, v := range j {
			cp[k] = v
		}
		cron = append(cron, cp)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"panels": map[string]any{
		"mcp": []map[string]any{
			{"name": "日志检索", "type": "stdio", "connected": true, "needsAuth": false, "loaded": 3, "tools": 12},
			{"name": "任务板", "type": "http", "connected": false, "needsAuth": true, "loaded": 0, "tools": 0},
		},
		"agents": map[string]any{
			"anyChecked": true,
			"checkedAt":  "刚刚",
			"peers": []map[string]any{
				{"name": "构建机", "label": "ci-vm", "id": "a1b2c3d4e5f60718", "url": "http://10.0.0.8:8790", "note": "能跑发布与回归",
					"reach": "可达", "skills": []string{"build", "deploy"}, "err": ""},
				{"name": "笔记本", "label": "", "id": "", "url": "http://10.0.0.9:8790", "note": "",
					"reach": "连不上", "skills": []string{}, "err": "timeout"},
			},
		},
		"cron": cron,
	}})
}

type cronPauseReq struct {
	Paused bool `json:"paused"`
}

// cronIndex 找到一条任务在表里的位置；调用方持锁
func (s *Server) cronIndex(id string) int {
	for i, j := range s.cron {
		if j["id"] == id {
			return i
		}
	}
	return -1
}

func (s *Server) handleCronRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	i := s.cronIndex(id)
	if i < 0 {
		s.mu.Unlock()
		fail(w, http.StatusNotFound, "no_job")
		return
	}
	if s.cron[i]["running"] == true {
		s.mu.Unlock()
		fail(w, http.StatusConflict, "cron_failed")
		return
	}
	s.cron[i]["running"] = true
	s.mu.Unlock()
	s.broadcast("", Event{Type: "cron_changed", ID: id})
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})

	// 跑一轮：两秒后结束，让面板看得见「运行中 → 上次成功」
	go func() {
		time.Sleep(2 * time.Second)
		s.mu.Lock()
		if j := s.cronIndex(id); j >= 0 {
			s.cron[j]["running"] = false
			s.cron[j]["lastRunStatus"] = "complete"
			s.cron[j]["lastRunError"] = ""
		}
		s.mu.Unlock()
		s.broadcast("", Event{Type: "cron_changed", ID: id})
	}()
}

func (s *Server) handleCronPause(w http.ResponseWriter, r *http.Request) {
	var req cronPauseReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	i := s.cronIndex(id)
	if i < 0 {
		s.mu.Unlock()
		fail(w, http.StatusNotFound, "no_job")
		return
	}
	if req.Paused {
		s.cron[i]["status"] = "paused"
	} else {
		s.cron[i]["status"] = "active"
	}
	s.mu.Unlock()
	s.broadcast("", Event{Type: "cron_changed", ID: id})
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

func (s *Server) handleCronDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	i := s.cronIndex(id)
	if i < 0 {
		s.mu.Unlock()
		fail(w, http.StatusNotFound, "no_job")
		return
	}
	s.cron = append(s.cron[:i], s.cron[i+1:]...)
	s.mu.Unlock()
	s.broadcast("", Event{Type: "cron_changed", ID: id})
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

type pinReq struct {
	Pinned bool `json:"pinned"`
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	var req pinReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	s.mu.Lock()
	sess, ok := s.byID[r.PathValue("id")]
	if !ok {
		s.mu.Unlock()
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	sess.Session.Pinned = req.Pinned
	s.mu.Unlock()
	s.broadcast("", Event{Type: "sessions_changed"})
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

type newSessionReq struct {
	ProjectID  string `json:"projectId"`
	WorkingDir string `json:"workingDir"`
}

// handleCreateSession 建一段新会话。真的服务端不碰桌面正在看的那段，也不再建 agent；
// 假服务端照这个形状回一行，然后广播 sessions_changed。
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req newSessionReq
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "bad_request")
			return
		}
	}
	projectName := ""
	primary := ""
	additional := []string{}
	switch req.ProjectID {
	case "":
		wd := strings.TrimSpace(req.WorkingDir)
		if wd == "" {
			wd = "~/workspace"
		}
		primary = expandHome(wd)
	case "p-tachi":
		projectName, primary, additional = "tachi", "~/repos/tachi", []string{"~/repos/shared"}
	case "p-notes":
		projectName, primary = "笔记", "~/notes"
	default:
		fail(w, http.StatusConflict, "no_project")
		return
	}

	s.mu.Lock()
	id := "s" + strconv.Itoa(len(s.order)+1)
	for s.byID[id] != nil {
		id += "x"
	}
	ns := &session{}
	ns.Session = SessionSummary{
		ID: id, Title: "新会话", ProjectName: projectName, UpdatedAt: "刚刚",
	}
	ns.Model = "Alpha 4.5"
	ns.Workspace = WorkspaceInfo{Primary: primary, Additional: additional}
	ns.Context = ContextInfo{Estimate: 0, Window: 200000, Parts: []ContextPart{}}
	ns.State = SessionState{Status: "idle", Label: "空闲", Detail: "新会话"}
	s.byID[id] = ns
	s.order = append([]string{id}, s.order...)
	row := ns.Session
	s.mu.Unlock()

	s.broadcast("", Event{Type: "sessions_changed"})
	writeJSON(w, http.StatusOK, map[string]any{"session": row})
}

// expandHome 只是让假服务端的日志看起来像真的：真服务端展开的是它那台机器的主目录。
func expandHome(p string) string {
	if p == "~" {
		return "/home/mock"
	}
	if strings.HasPrefix(p, "~/") {
		return "/home/mock/" + strings.TrimPrefix(p, "~/")
	}
	return p
}

// handleAwake 读的是那台「电脑」的开关：手机卡片画的就是它。
func (s *Server) handleAwake(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	st := s.awake
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"awake": st})
}

// handleSetAwake 走和桌面同一条规矩：越界的时长是 400 bad_request，成功之后把完整状态广播给
// 每一条连接（假服务端没有系统能拒，所以 awake_failed 在这里不会发生）。
func (s *Server) handleSetAwake(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On          bool `json:"on"`
		DurationMin *int `json:"durationMin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}

	s.mu.Lock()
	st := s.awake
	if req.DurationMin != nil {
		m := *req.DurationMin
		if m != 0 && (m < mockAwakeMinMinutes || m > mockAwakeMaxMinutes) {
			s.mu.Unlock()
			fail(w, http.StatusBadRequest, "bad_request")
			return
		}
		st.DurationMin = m
	}
	st.On = req.On
	st.Until = ""
	if st.On && st.DurationMin > 0 {
		st.Until = time.Now().Add(time.Duration(st.DurationMin) * time.Minute).UTC().Format(time.RFC3339)
	}
	s.awake = st
	s.mu.Unlock()

	s.broadcast("", Event{Type: "awake_changed", Awake: &st})
	writeJSON(w, http.StatusOK, map[string]any{"awake": st})
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": []map[string]string{
		{"name": "Alpha 4.5", "model": "alpha-4-5"},
		{"name": "Beta 5.2", "model": "beta-5-2"},
		{"name": "Gamma K2", "model": "gamma-k2"},
		{"name": "Delta V3", "model": "delta-v3"},
	}})
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	before := -1
	if v := r.URL.Query().Get("before"); v != "" {
		before, _ = strconv.Atoi(v)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]Message, 0, len(sess.Messages))
	for _, m := range sess.Messages {
		if before >= 0 && m.Index >= before {
			continue
		}
		kept = append(kept, m)
	}
	hasMore := len(kept) > limit
	if hasMore {
		kept = kept[len(kept)-limit:]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": kept, "hasMore": hasMore, "session": sess.Session,
	})
}

// handleUpload 收下手机发的一个文件。假服务端不真的存字节：它只要把「存下来的名字」还回去，
// 因为手机关心的就是这个名字（发送时递它），而协议里从不出现路径。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	if sess.Session.ReadOnly {
		fail(w, http.StatusConflict, "read_only")
		return
	}
	f, header, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	defer f.Close()
	// 大小只用来回报给手机；字节本身没有用处
	buf, err := io.ReadAll(f)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	name := filepath.Base(header.Filename)
	kind := "binary"
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		kind = "image"
	case ".txt", ".md", ".log", ".json", ".go", ".ts", ".yaml", ".yml":
		kind = "text"
	}
	s.mu.Lock()
	sess.Uploads = append(sess.Uploads, name)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"file": map[string]any{"name": name, "size": len(buf), "kind": kind},
	})
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	id := r.PathValue("attachmentId")
	var found *AttachmentRef
	for i := range sess.Attachments {
		if sess.Attachments[i].ID == id {
			found = &sess.Attachments[i]
			break
		}
	}
	if found == nil {
		// 只认交付记录里的 id，不接受路径
		fail(w, http.StatusNotFound, "no_such_file")
		return
	}
	body, ok := fileBodies[id]
	if ok {
		w.Header().Set("Content-Type", found.MimeType)
		_, _ = w.Write([]byte(body))
		return
	}
	if strings.HasPrefix(found.MimeType, "image/") {
		// 图片现画一张：手机端那条「缩略图铺在气泡里」的路需要真的像素
		w.Header().Set("Content-Type", found.MimeType)
		_, _ = w.Write(chartPNG())
		return
	}
	fail(w, http.StatusNotFound, "no_such_file")
}

type messageReq struct {
	Text string `json:"text"`
	Mode string `json:"mode"`
	// 上传时拿到的名字（不是路径）。假服务端只把它们写进那条消息的气泡里。
	Attachments []string `json:"attachments"`
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	var req messageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		(strings.TrimSpace(req.Text) == "" && len(req.Attachments) == 0) {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	// 气泡里是「自己的话 + 每个文件一行名字」，与真服务端写记录的那一条一致
	shown := strings.TrimSpace(req.Text)
	for _, n := range req.Attachments {
		if shown != "" {
			shown += "\n\n"
		}
		shown += "📎 " + n
	}
	if sess.Session.ReadOnly {
		fail(w, http.StatusConflict, "read_only")
		return
	}

	s.mu.Lock()
	running := sess.Session.Running
	s.mu.Unlock()

	switch req.Mode {
	case "send":
		if running {
			// 恰好有人抢先起了一轮
			fail(w, http.StatusConflict, "turn_running")
			return
		}
	case "steer", "interrupt":
		if !running {
			fail(w, http.StatusConflict, "not_running")
			return
		}
	default:
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}

	go s.runTurn(sess, shown, req.Mode)
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

type titleReq struct {
	Title string `json:"title"`
}

// handleRename 改一段对话的标题。标题是列表里那一行的字段，所以改完广播 sessions_changed ——
// 与真服务端同一条规矩（那台电脑上开着的窗口也靠它改名字）。
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var req titleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Title) == "" {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	s.mu.Lock()
	sess, ok := s.byID[r.PathValue("id")]
	if !ok {
		s.mu.Unlock()
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	sess.Session.Title = strings.TrimSpace(req.Title)
	s.mu.Unlock()
	s.broadcast("", Event{Type: "sessions_changed"})
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	s.mu.Lock()
	sess.Session.Running = false
	s.mu.Unlock()
	s.broadcast(sess.Session.ID, Event{Type: "idle", End: "interrupted"})
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

type providerReq struct {
	Name string `json:"name"`
}

// handleDeleteSession 删一段会话。正在跑的那一轮拒绝（协议里 turn_running 那条），
// 删掉之后广播 sessions_changed —— 列表不属于任何一条会话流。
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	sess, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	if sess.Session.Running {
		s.mu.Unlock()
		fail(w, http.StatusConflict, "turn_running")
		return
	}
	delete(s.byID, id)
	for i, x := range s.order {
		if x == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	s.broadcast("", Event{Type: "sessions_changed"})
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

func (s *Server) handleProvider(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	var req providerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	s.mu.Lock()
	sess.Model = req.Name
	snap := sess.Snapshot
	s.mu.Unlock()
	s.pushSnapshot(sess.Session.ID)
	_ = snap
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

type skipReq struct {
	Skip bool `json:"skip"`
}

func (s *Server) handleSkipApproval(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	var req skipReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request")
		return
	}
	s.mu.Lock()
	sess.SkipApprove = req.Skip
	s.mu.Unlock()
	s.pushSnapshot(sess.Session.ID)
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	toolID := r.PathValue("toolId")

	s.mu.Lock()
	idx := -1
	for i, a := range sess.Asks {
		if p, ok := a.(PermissionAsk); ok && p.ToolID == toolID {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		// 过期的 tool id 要得到明确的 409，而不是静默成功
		fail(w, http.StatusConflict, "no_such_pending_request")
		return
	}
	sess.Asks = append(sess.Asks[:idx], sess.Asks[idx+1:]...)
	sess.Session.Waiting = ""
	sess.State = SessionState{Status: "working", Label: "在跑", Detail: "已收到授权"}
	s.mu.Unlock()

	s.broadcast(sess.Session.ID, Event{Type: "ask_answered", ToolID: toolID})
	// 待答项没了、状态变了 —— 让另一块屏重读一份快照（快照只能从 WS 的 open 拿）
	s.pushSnapshot(sess.Session.ID)
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})

	// 授权之后把那一轮接着跑完
	go s.finishTurn(sess, "好，我按你的授权继续 —— 命令跑完了，build/ 里清掉 6 个旧产物，最近的两次留着。")
}

type answerReq struct {
	Answers map[string]string `json:"answers"`
	Decline bool              `json:"decline"`
}

func (s *Server) handleQuestions(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	var req answerReq
	_ = json.NewDecoder(r.Body).Decode(&req)

	s.mu.Lock()
	if len(sess.Asks) == 0 {
		s.mu.Unlock()
		fail(w, http.StatusConflict, "no_such_pending_request")
		return
	}
	var toolID string
	for _, a := range sess.Asks {
		if q, ok := a.(QuestionAsk); ok {
			toolID = q.ToolID
		}
	}
	sess.Asks = nil
	sess.Session.Waiting = ""
	sess.State = SessionState{Status: "working", Label: "在跑", Detail: "已收到回答"}
	s.mu.Unlock()

	s.broadcast(sess.Session.ID, Event{Type: "ask_answered", ToolID: toolID})
	s.pushSnapshot(sess.Session.ID)
	go s.finishTurn(sess, "按你选的来 ——「"+joinAnswers(req.Answers)+"」，我照着改。")
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}

func joinAnswers(answers map[string]string) string {
	if len(answers) == 0 {
		return "（跳过）"
	}
	// 桌面的约定：值是选中项用 ", " 连接
	var parts []string
	for _, v := range answers {
		parts = append(parts, v)
	}
	return strings.Join(parts, ", ")
}
