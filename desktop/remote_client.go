package main

// 远端客户端：桌面这一侧去够另一台机器上的 tachi-headless。
//
// 它说的是手机那一套接口（docs/agents/mobile-protocol.md），不新造协议：那套东西已经
// 解决了鉴权、事件推送、权限回答、压缩换轨，手机是它的第一个客户端，这里是第二个。
// 代价要认：能做的事被那套接口限定 —— 想补文件树或 diff，得先扩接口（手机也会跟着受益）。
//
// 一条规矩：**每一次调用都点名机器与会话**。桌面自己的那些方法认的是 d.activeID，
// 远端客户端绝不能借用它们，否则点一下远端会话会改到本地正看着的那一段。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
)

// ── 线上类型的 Go 一侧（字段名照协议，不缩写） ──────────────────────────────

type RemoteSessionVO struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	ProjectName    string `json:"projectName,omitempty"`
	UpdatedAt      string `json:"updatedAt"`
	Running        bool   `json:"running"`
	Waiting        string `json:"waiting"`
	ReadOnly       bool   `json:"readOnly"`
	HeldByTask     bool   `json:"heldByTask"`
	Pinned         bool   `json:"pinned,omitempty"`
	ChannelSession bool   `json:"channelSession,omitempty"`
	ChannelName    string `json:"channelName,omitempty"`
	// Cron is how many scheduled jobs write into this conversation, and CronNext the earliest
	// wake-up (RFC3339). The phone draws a clock with them; the desktop's delete confirmation
	// uses the count to say 「任务也会一并删除」 — the same sentence this machine's own sidebar
	// shows for its own sessions.
	Cron     int    `json:"cron,omitempty"`
	CronNext string `json:"cronNext,omitempty"`
}

type RemoteMessageVO struct {
	Index          int    `json:"index"`
	Role           string `json:"role"`
	Content        string `json:"content"`
	DisplayContent string `json:"displayContent,omitempty"`
	Timestamp      string `json:"timestamp,omitempty"`
	Iteration      int    `json:"iteration,omitempty"`
	Seq            int    `json:"seq,omitempty"`
	Turn           int    `json:"turn,omitempty"`
	ToolName       string `json:"toolName,omitempty"`
	ToolCallID     string `json:"toolCallId,omitempty"`
	Args           string `json:"args,omitempty"`
	Title          string `json:"title,omitempty"`
	ToolResult     string `json:"toolResult,omitempty"`
	IsError        bool   `json:"isError,omitempty"`
	Thinking       string `json:"thinking,omitempty"`
	AutoExpand     bool   `json:"autoExpand,omitempty"`
}

type RemoteQuestionVO struct {
	Question    string   `json:"question"`
	Options     []string `json:"options,omitempty"`
	MultiSelect bool     `json:"multiSelect,omitempty"`
}

type RemoteAskVO struct {
	Kind           string             `json:"kind"` // permission / question
	ToolID         string             `json:"toolId"`
	ToolName       string             `json:"toolName,omitempty"`
	PermissionKind string             `json:"permissionKind,omitempty"` // bash / files / mcp_search
	Preview        string             `json:"preview,omitempty"`
	Questions      []RemoteQuestionVO `json:"questions,omitempty"`
}

type RemoteStateVO struct {
	Status  string `json:"status"`
	Label   string `json:"label"`
	Detail  string `json:"detail"`
	Waiting string `json:"waiting"`
}

type RemoteWorkspaceVO struct {
	Primary    string   `json:"primary"`
	Additional []string `json:"additional"`
}

type RemoteUsageVO struct {
	Cost         float64 `json:"cost"`
	Credit       float64 `json:"credit,omitempty"`
	CacheHitRate float64 `json:"cacheHitRate,omitempty"`
}

// RemoteContextPartVO is one bucket of the context breakdown. The wire carries a LABEL, not the
// local colour key (the phone paints these with its own palette), so the remote popover lists
// them without the native per-bucket colours.
type RemoteContextPartVO struct {
	Label  string `json:"label"`
	Tokens int64  `json:"tokens"`
}

type RemoteContextVO struct {
	Estimate int64                 `json:"estimate"`
	Window   int64                 `json:"window"`
	Parts    []RemoteContextPartVO `json:"parts"`
}

type RemoteAttachmentVO struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	Size         int64  `json:"size"`
	Downloadable bool   `json:"downloadable"`
}

// RemoteSnapshotVO 是打开一段远端对话时先给的那一份完整快照，之后才是增量。
type RemoteSnapshotVO struct {
	Session      RemoteSessionVO      `json:"session"`
	Messages     []RemoteMessageVO    `json:"messages"`
	HasMore      bool                 `json:"hasMore"`
	State        RemoteStateVO        `json:"state"`
	Asks         []RemoteAskVO        `json:"asks"`
	Model        string               `json:"model"`
	SkipApproval bool                 `json:"skipApproval"`
	NotifyOff    bool                 `json:"notifyOff"`
	Workspace    RemoteWorkspaceVO    `json:"workspace"`
	Usage        RemoteUsageVO        `json:"usage"`
	Context      RemoteContextVO      `json:"context"`
	Attachments  []RemoteAttachmentVO `json:"attachments"`
}

type RemotePageVO struct {
	Messages []RemoteMessageVO `json:"messages"`
	HasMore  bool              `json:"hasMore"`
}

type RemoteProviderVO struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

// ── HTTP ────────────────────────────────────────────────────────────────────

// remoteAPIError 是远端受理层给出的拒绝码（turn_running、no_session……）。它和一次
// 连不上是两件事：前者是那台机器答复了我们，后者是我们根本没碰到它。
type remoteAPIError struct {
	Status int
	Code   string
}

func (e *remoteAPIError) Error() string { return e.Code }

type remoteEndpoint struct {
	urls   []string
	token  string
	client *http.Client
}

func remoteEndpointFor(h remoteHost) *remoteEndpoint {
	return &remoteEndpoint{urls: h.URLs, token: h.Token, client: &http.Client{Timeout: remoteRequestTimeout}}
}

// call 按机器记住的顺序试地址：局域网那条在外面常常先超时，后面还有 Tailscale 的。
func (e *remoteEndpoint) call(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	var lastErr error
	for _, base := range e.urls {
		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+e.token)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := e.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		err = func() error {
			defer resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				var e struct {
					Error string `json:"error"`
				}
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				_ = json.Unmarshal(b, &e)
				if e.Error == "" {
					e.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
				}
				return &remoteAPIError{Status: resp.StatusCode, Code: e.Error}
			}
			if out == nil {
				return nil
			}
			return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
		}()
		if err != nil {
			var api *remoteAPIError
			if errors.As(err, &api) {
				return err
			}
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用地址")
	}
	return lastErr
}

// remoteReason turns a failure into the sentence a reader can act on. A code is kept
// verbatim: the frontend has its own table for the protocol's refusal codes (that is where
// the wording of "它正在跑；插话或打断？" lives), and inventing a second table here would
// make the two screens say different things.
func remoteReason(err error) string {
	if err == nil {
		return ""
	}
	var api *remoteAPIError
	if errors.As(err, &api) {
		return api.Code
	}
	return "连不上这台机器：" + err.Error()
}

func sessionPath(id, suffix string) string {
	return "/v1/sessions/" + url.PathEscape(id) + suffix
}

// ── AgentService 的绑定方法 ─────────────────────────────────────────────────

func (s *AgentService) remoteEndpoint(hostID string) (*remoteEndpoint, remoteHost, string) {
	d := s.desk
	host, err := d.remoteHostByID(hostID)
	if err != nil {
		return nil, remoteHost{}, err.Error()
	}
	return remoteEndpointFor(host), host, ""
}

func (s *AgentService) RemoteSessions(hostID string) ([]RemoteSessionVO, string) {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return nil, why
	}
	var out struct {
		Sessions []RemoteSessionVO `json:"sessions"`
	}
	if err := e.call(context.Background(), http.MethodGet, "/v1/sessions", nil, &out); err != nil {
		return nil, remoteReason(err)
	}
	if out.Sessions == nil {
		out.Sessions = []RemoteSessionVO{}
	}
	// 服务端已经排好序（置顶优先、其次最新）；这里不再排一次，免得两处对「行应该在哪」各说一套。
	return out.Sessions, ""
}

// RemoteSetSessionPinned pins (or unpins) a conversation on the other machine. The list is
// ordered pinned-first by the server, so the row's new place comes from the refresh it broadcasts
// — this side must not sort a copy of its own.
func (s *AgentService) RemoteSetSessionPinned(hostID, sessionID string, pinned bool) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	body := map[string]any{"pinned": pinned}
	if err := e.call(context.Background(), http.MethodPost, sessionPath(sessionID, "/pin"), body, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}

// RemoteSetSessionNotify flips this conversation's phone-push switch on the other machine. The
// value lives in that machine's session record (the same field its own desktop reads), and the
// broadcast `session_prefs` is how every screen hears about it.
func (s *AgentService) RemoteSetSessionNotify(hostID, sessionID string, off bool) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	body := map[string]any{"off": off}
	if err := e.call(context.Background(), http.MethodPost, sessionPath(sessionID, "/notify"), body, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}

// RemoteRenameSession renames a conversation on the other machine. An empty title is refused by
// that machine (bad_request), not silently ignored here — the reader typed something and the
// refusal is the only way they learn why nothing changed.
func (s *AgentService) RemoteRenameSession(hostID, sessionID, title string) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	body := map[string]any{"title": title}
	if err := e.call(context.Background(), http.MethodPost, sessionPath(sessionID, "/title"), body, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}

// RemoteDeleteSession deletes a conversation on the other machine. That machine refuses a
// conversation with a turn in flight (turn_running); the refusal goes back to the confirmation
// box, exactly like the native one, so the reader acts on the same words either way.
func (s *AgentService) RemoteDeleteSession(hostID, sessionID string) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	if err := e.call(context.Background(), http.MethodDelete, sessionPath(sessionID, ""), nil, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}

// RemoteNewSession creates a conversation on the OTHER machine — same endpoint the phone uses,
// so it lands under that machine's own default working directory (~/workspace) rather than
// borrowing anything from this window. Returning the row (not just "ok") is what lets the page
// open it right away: the list refresh may not have reached the caller yet.
func (s *AgentService) RemoteNewSession(hostID string) (RemoteSessionVO, string) {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return RemoteSessionVO{}, why
	}
	var out struct {
		Session RemoteSessionVO `json:"session"`
	}
	// An empty object is legal: 「无项目、默认工作目录」 (see the server's handleCreateSession).
	if err := e.call(context.Background(), http.MethodPost, "/v1/sessions", map[string]any{}, &out); err != nil {
		return RemoteSessionVO{}, remoteReason(err)
	}
	return out.Session, ""
}

func (s *AgentService) RemoteOpenSession(hostID, sessionID string) (RemoteSnapshotVO, string) {
	var empty RemoteSnapshotVO
	if sessionID == "" {
		return empty, "先选一段对话"
	}
	d := s.desk
	host, err := d.remoteHostByID(hostID)
	if err != nil {
		return empty, err.Error()
	}
	conn, err := d.remoteConnection(host)
	if err != nil {
		return empty, "连不上这台机器：" + err.Error()
	}
	snap, err := conn.open(sessionID)
	if err != nil {
		// Verbatim: the server's word is usually a protocol code the page already knows
		// (no_session), and a generic wrapper would turn it into something nobody can act on.
		return empty, err.Error()
	}
	if snap.Workspace.Additional == nil {
		snap.Workspace.Additional = []string{}
	}
	if snap.Messages == nil {
		snap.Messages = []RemoteMessageVO{}
	}
	if snap.Asks == nil {
		snap.Asks = []RemoteAskVO{}
	}
	if snap.Attachments == nil {
		snap.Attachments = []RemoteAttachmentVO{}
	}
	return snap, ""
}

func (s *AgentService) RemotePage(hostID, sessionID string, before, limit int) (RemotePageVO, string) {
	var empty RemotePageVO
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return empty, why
	}
	q := url.Values{}
	q.Set("before", strconv.Itoa(before))
	q.Set("limit", strconv.Itoa(limit))
	var out RemotePageVO
	if err := e.call(context.Background(), http.MethodGet, sessionPath(sessionID, "/page")+"?"+q.Encode(), nil, &out); err != nil {
		return empty, remoteReason(err)
	}
	if out.Messages == nil {
		out.Messages = []RemoteMessageVO{}
	}
	return out, ""
}

// RemoteSend posts one message. mode is send / steer / interrupt, the protocol's own three
// words: the composer's decision (排队 or 直接发) is the same decision it makes locally.
// attachments are the NAMES an upload answered with (RemoteUploadFile), never paths — one of the
// protocol's own rules, and the reason a token cannot be turned into "read that machine's files".
func (s *AgentService) RemoteSend(hostID, sessionID, text, mode string, attachments []string) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	if mode != "send" && mode != "steer" && mode != "interrupt" {
		mode = "send"
	}
	body := map[string]any{"text": text, "mode": mode}
	if len(attachments) > 0 {
		body["attachments"] = attachments
	}
	if err := e.call(context.Background(), http.MethodPost, sessionPath(sessionID, "/messages"), body, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}

func (s *AgentService) RemoteStop(hostID, sessionID string) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	if err := e.call(context.Background(), http.MethodPost, sessionPath(sessionID, "/stop"), nil, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}

func (s *AgentService) RemoteAnswerPermission(hostID, sessionID, toolID, decision string) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	body := map[string]any{"decision": decision}
	path := sessionPath(sessionID, "/permissions/"+url.PathEscape(toolID))
	if err := e.call(context.Background(), http.MethodPost, path, body, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}

func (s *AgentService) RemoteAnswerQuestion(hostID, sessionID string, answers map[string]string) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	body := map[string]any{"answers": answers}
	if err := e.call(context.Background(), http.MethodPost, sessionPath(sessionID, "/questions"), body, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}

func (s *AgentService) RemoteDeclineQuestion(hostID, sessionID string) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	body := map[string]any{"decline": true}
	if err := e.call(context.Background(), http.MethodPost, sessionPath(sessionID, "/questions"), body, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}

func (s *AgentService) RemoteProviders(hostID string) ([]RemoteProviderVO, string) {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return nil, why
	}
	var out struct {
		Providers []RemoteProviderVO `json:"providers"`
	}
	if err := e.call(context.Background(), http.MethodGet, "/v1/providers", nil, &out); err != nil {
		return nil, remoteReason(err)
	}
	if out.Providers == nil {
		out.Providers = []RemoteProviderVO{}
	}
	sort.SliceStable(out.Providers, func(i, j int) bool { return out.Providers[i].Name < out.Providers[j].Name })
	return out.Providers, ""
}

func (s *AgentService) RemoteSetProvider(hostID, sessionID, name string) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	body := map[string]any{"name": name}
	if err := e.call(context.Background(), http.MethodPost, sessionPath(sessionID, "/provider"), body, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}

func (s *AgentService) RemoteSetSkipApproval(hostID, sessionID string, skip bool) string {
	e, _, why := s.remoteEndpoint(hostID)
	if why != "" {
		return why
	}
	body := map[string]any{"skip": skip}
	if err := e.call(context.Background(), http.MethodPost, sessionPath(sessionID, "/skip-approval"), body, nil); err != nil {
		return remoteReason(err)
	}
	return "ok"
}

// RemoteDisconnect drops the live connection to one machine. Leaving the remote view calls
// this: a connection nobody draws should not keep collecting another machine's events.
func (s *AgentService) RemoteDisconnect(hostID string) {
	s.desk.closeRemoteConn(hostID)
}
