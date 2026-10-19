package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

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
}

func New(token string) *Server {
	order, byID := seed()
	return &Server{
		token:   token,
		order:   order,
		byID:    byID,
		clients: map[*client]struct{}{},
		log:     logger.New("mockapi"),
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
	mux.HandleFunc("GET /v1/providers", s.guard(s.handleProviders))
	mux.HandleFunc("GET /v1/sessions/{id}/page", s.guard(s.handlePage))
	mux.HandleFunc("GET /v1/sessions/{id}/files/{attachmentId}", s.guard(s.handleFile))
	mux.HandleFunc("POST /v1/sessions/{id}/messages", s.guard(s.handleMessage))
	mux.HandleFunc("POST /v1/sessions/{id}/stop", s.guard(s.handleStop))
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.guard(s.handleDeleteSession))
	mux.HandleFunc("POST /v1/sessions/{id}/provider", s.guard(s.handleProvider))
	mux.HandleFunc("POST /v1/sessions/{id}/skip-approval", s.guard(s.handleSkipApproval))
	mux.HandleFunc("POST /v1/sessions/{id}/permissions/{toolId}", s.guard(s.handlePermission))
	mux.HandleFunc("POST /v1/sessions/{id}/questions", s.guard(s.handleQuestions))
	return mux
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": "tachi-mockapi", "version": "0.1"})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionSummary, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.byID[id].Session)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
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
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r.PathValue("id"))
	if sess == nil {
		fail(w, http.StatusNotFound, "no_session")
		return
	}
	var req messageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		fail(w, http.StatusBadRequest, "bad_request")
		return
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

	go s.runTurn(sess, req.Text, req.Mode)
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
