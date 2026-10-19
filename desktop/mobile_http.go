package main

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// HTTP 端点：命令走这里（能独立重试、能用 curl 调），过程走 WebSocket。
// 端点、请求体与返回形状以 docs/agents/mobile-protocol.md 为准。

// mobileVersion is what /v1/ping reports. The pairing page only uses it to show what answered;
// there is no compatibility negotiation in V1.
const mobileVersion = "0.1"

func (m *mobileServer) handlePing(w http.ResponseWriter, r *http.Request) {
	// No token: the pairing page is verifying the address AND the token, so it has to be able
	// to reach something first. It reveals nothing a LAN scan would not.
	mobileWriteJSON(w, map[string]any{"ok": true, "name": "tachi-desktop", "version": mobileVersion})
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
}

func (m *mobileServer) handleMessage(w http.ResponseWriter, r *http.Request) {
	var req mobileMessageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		mobileFail(w, http.StatusBadRequest, "bad_request")
		return
	}
	if status, code := m.desk.mobileSend(r.PathValue("id"), req.Text, req.Mode); code != "" {
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
