package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/session"
)

// 手机写的那一半：按会话寻址的加载、发送、停止、回答，以及附件下载。
//
// 这一层最容易做错的地方不是权限，而是**地址**：桌面那几个「当前会话」的方法
// （LoadSession / SwitchProvider / GetSkipApproval / SetSkipApproval）认的是 d.activeID，
// 手机一调就会改到桌面上正看着的那一段 —— 所以这里每一条都只认调用方点名的 id。

// loadSessionForRemote is LoadSession's first two steps WITHOUT the third.
//
//  1. prepareSession — build this session's own agent and manager;
//  2. restore its LLM history into r.history;
//  3. (nothing: no activeID, no state event)
//
// The missing third step is the point: the phone opening a conversation must not move the
// window. The history restore is not optional either — startTurn ALWAYS reads r.history
// (runHistory), and prepareSession does not fill it, so a first message sent from the phone to
// a session the desktop never opened would silently drop everything the model had been told
// before. It fails in the worst way: the screen looks fine.
func (d *desktopApp) loadSessionForRemote(id string) (*sessionRun, error) {
	if d.sm == nil {
		return nil, fmt.Errorf("no session manager")
	}
	if d.sessionRecord(id) == nil {
		return nil, errMobileNoSession
	}
	r, err := d.prepareSession(context.Background(), id)
	if err != nil {
		return nil, err
	}
	if r.agent == nil {
		return nil, fmt.Errorf("agent not ready")
	}
	var history []llm.Message
	if h, herr := r.agent.LoadSessionHistory(); herr == nil {
		history = h
	}
	d.mu.Lock()
	// A turn in flight owns the history: it is writing the next request out of it, and
	// replacing it under the turn would make that request skip work already done.
	if !r.running {
		r.history = history
	}
	d.mu.Unlock()
	return r, nil
}

// mobileSend handles one of the three send modes and answers the protocol's (status, code).
// The refusal reason is passed through verbatim where it is specific enough to act on: the
// phone screen is small, and the reason IS the only thing the reader can act on.
//
// attachments are the names uploads answered with (POST /v1/sessions/{id}/files) — NOT paths. They
// become @-references here, resolved inside this session's own uploads directory, and then travel
// the existing route: @-expansion at turn start turns a text file into content, an image into a
// multi-modal part, anything else into "read it with your tools". There is no second way for a file
// to reach the model, which is why this layer only has to name one.
func (d *desktopApp) mobileSend(id, text, mode string, attachments []string) (int, string) {
	if text == "" && len(attachments) == 0 {
		return http.StatusBadRequest, "bad_request"
	}
	ss := d.sessionRecord(id)
	if ss == nil {
		return http.StatusNotFound, "no_session"
	}
	if ss.Kind != "" {
		return http.StatusConflict, "read_only"
	}
	if d.busySessions()[id] {
		return http.StatusConflict, "held_by_task"
	}
	// A conversation a channel owns is driven by THAT runtime, not by this one: the two keep
	// separate in-memory histories over the same session, so this side may speak but must not write.
	// See channel_mirror.go.
	if ss.ThreadID != "" {
		return d.mobileSendToChannel(ss, id, text, mode, attachments)
	}
	refs, status, code := d.mobileUploadRefs(id, attachments)
	if code != "" {
		return status, code
	}
	// full goes to the model, shown to the transcript (see mobileMessageWithFiles).
	full, shown := mobileMessageWithFiles(text, refs, attachments)
	text = full
	switch mode {
	case "send":
		if running, _ := d.mobileRunState(id); running {
			return http.StatusConflict, "turn_running"
		}
		// Restore the context BEFORE the turn starts: see loadSessionForRemote.
		if _, err := d.loadSessionForRemote(id); err != nil {
			if errors.Is(err, errMobileNoSession) {
				return http.StatusNotFound, "no_session"
			}
			return http.StatusConflict, "agent_unavailable"
		}
		switch d.startTurnFrom(id, text, shown, originMobile) {
		case "ok":
			return 0, ""
		case refuseTurnRunning:
			return http.StatusConflict, "turn_running"
		case refuseNoSession:
			return http.StatusNotFound, "no_session"
		default:
			return http.StatusConflict, "send_refused"
		}
	case "steer":
		running, hasSteer := d.mobileRunState(id)
		if !running {
			return http.StatusConflict, "not_running"
		}
		// A scheduled turn deliberately has no steer channel: its message queue waits for the
		// job to finish (see the cron section of desktop.md). Saying "not running" here would
		// send the reader looking for a turn that is very much running.
		if !hasSteer {
			return http.StatusConflict, "steer_unsupported"
		}
		switch d.steerFrom(id, text, shown, originMobile) {
		case "ok":
			return 0, ""
		case "not running":
			return http.StatusConflict, "not_running"
		default:
			return http.StatusConflict, "steer_failed"
		}
	case "interrupt":
		// Nothing is running, so this is really a plain send: the context still has to be
		// restored before the turn starts (same reason as the "send" case).
		if running, _ := d.mobileRunState(id); !running {
			if _, err := d.loadSessionForRemote(id); err != nil {
				if errors.Is(err, errMobileNoSession) {
					return http.StatusNotFound, "no_session"
				}
				return http.StatusConflict, "agent_unavailable"
			}
		}
		switch d.stopAndSendFrom(id, text, shown, originMobile) {
		case "ok":
			return 0, ""
		case "empty":
			return http.StatusBadRequest, "bad_request"
		case refuseTurnRunning:
			return http.StatusConflict, "turn_running"
		default:
			return http.StatusConflict, "send_refused"
		}
	}
	return http.StatusBadRequest, "bad_request"
}

// mobileStop cancels the named session's turn. It is addressed by id, not by "whatever the
// window is showing" (Stop is the window's version of this).
func (d *desktopApp) mobileStop(id string) (int, string) {
	if d.sessionRecord(id) == nil {
		return http.StatusNotFound, "no_session"
	}
	d.stopTurnFor(id)
	return 0, ""
}

// mobileDeleteSession deletes a conversation from the phone. It goes through DeleteSession, so
// the same rule holds here: a session with a turn in flight is refused, not stopped underneath.
func (d *desktopApp) mobileDeleteSession(id string) (int, string) {
	if d.sessionRecord(id) == nil {
		return http.StatusNotFound, "no_session"
	}
	switch res := (&AgentService{desk: d}).DeleteSession(id); res {
	case "ok":
		return 0, ""
	case refuseDeleteRunning:
		return http.StatusConflict, "turn_running"
	default:
		return http.StatusInternalServerError, "delete_failed"
	}
}

// mobileRunState reads a session's turn state WITHOUT creating a run entry: a lookup that
// only asks "is something running here?" must not leave an empty sessionRun behind.
func (d *desktopApp) mobileRunState(id string) (running, hasSteer bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// A turn the phone asked a CHANNEL to run is in flight too, and its conversation steers (that is
	// what the channel does with a message that lands mid-turn).
	if _, ok := d.mirrors[id]; ok {
		return true, true
	}
	r := d.ownerOfLocked(id)
	if r == nil {
		return false, false
	}
	return r.running, r.steerCh != nil
}

func (d *desktopApp) stopTurnFor(id string) {
	d.mu.Lock()
	var cancel context.CancelFunc
	if turn, ok := d.mirrors[id]; ok {
		// A channel-owned conversation: Stop reaches the turn through the channel runtime, which
		// owns it (see channel_mirror.go).
		cancel = turn.cancel
	} else if r := d.ownerOfLocked(id); r != nil {
		cancel = r.turnCancel
	}
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// mobileAnswerPermission goes through the SAME function the window and the device use: one
// decision has one implementation, so a click on the phone and a click on the card cannot
// behave differently (design 不变量 2).
func (d *desktopApp) mobileAnswerPermission(id, toolID, decision string) (int, string) {
	if decision != permAllowOnce && decision != permDeny {
		// V1 deliberately has no 「本会话全部允许」: it is a one-shot batch grant, and a
		// mis-tap on a phone costs too much. The desktop card keeps it.
		return http.StatusBadRequest, "bad_request"
	}
	err := d.answerPermission(id, toolID, decision)
	if err == nil {
		return 0, ""
	}
	switch err.Error() {
	case "no such pending request", "already answered":
		return http.StatusConflict, "no_such_pending_request"
	}
	return http.StatusConflict, "answer_failed"
}

// mobileAnswerQuestions answers the session's pending AskUserQuestion.
//
// The answers' shape is the desktop's own convention (key = the question's full text,
// values joined with ", "), because the model must read the same words whichever screen asked.
// `decline` is an EMPTY answer set: the model is told the question went unanswered, rather
// than being handed a choice nobody made.
func (d *desktopApp) mobileAnswerQuestions(id string, answers map[string]string, decline bool) (int, string) {
	if d.sessionRecord(id) == nil {
		return http.StatusNotFound, "no_session"
	}
	if !d.hasPendingAsk(id, askKindAskUser) {
		return http.StatusConflict, "no_such_pending_request"
	}
	if decline {
		answers = map[string]string{}
	}
	if err := d.answerAskUser(id, answers, nil); err != nil {
		return http.StatusConflict, "no_such_pending_request"
	}
	return 0, ""
}

func (d *desktopApp) hasPendingAsk(sessionID string, kind pendingAskKind) bool {
	for _, a := range d.snapshotAsks() {
		if a.sessionID == sessionID && a.kind == kind {
			return true
		}
	}
	return false
}

// switchProviderFor is SwitchProvider addressed by session: the model list's choice lands on
// the conversation it was made in, never on whatever the window happens to display.
func (d *desktopApp) switchProviderFor(id, name string) error {
	if id == "" {
		return errors.New(refuseNoSession)
	}
	if d.config() == nil {
		return errors.New("agent not ready")
	}
	r, err := d.prepareSession(context.Background(), id)
	if err != nil {
		return err
	}
	if r.agent == nil {
		return errors.New(refuseNoSession)
	}
	if _, err := r.agent.SetResolvedProvider(name); err != nil {
		return err
	}
	if r.sm != nil {
		if cur := r.sm.Current(); cur != nil {
			applyThinking(r.agent, cur.ThinkingLevel)
			if cur.ProviderName != name {
				cur.ProviderName = name
				_ = r.sm.UpdateMeta(cur) // best-effort, same as the window's switch
			}
		}
	}
	d.mu.Lock()
	r.agentProvider = name
	d.mu.Unlock()
	d.notifySessionPrefs(id, name, d.skipApprovalFor(id))
	return nil
}

// skipApprovalFor reads the session RECORD's mode (the durable half; the live half is the
// agent's session-wide approval, derived from this record whenever an agent is built).
func (d *desktopApp) skipApprovalFor(id string) bool {
	if id == "" {
		return false
	}
	r := d.runOf(id)
	if r == nil || r.sm == nil {
		if ss := d.sessionRecord(id); ss != nil {
			return ss.SkipApproval
		}
		return false
	}
	if cur := r.sm.Current(); cur != nil {
		return cur.SkipApproval
	}
	return false
}

// setSkipApprovalFor is SetSkipApproval addressed by session. The record is written FIRST and
// the live agent second: a failed write puts the record back and touches no agent, so the two
// halves cannot end up describing different modes.
//
// It deliberately does NOT prepareSession first. This is a preference on the RECORD, and
// building an agent (tools, provider, MCP roots) just to flip one bool would fail on a machine
// mid-reconfiguration — the very machine whose phone should still be able to turn cards off.
func (d *desktopApp) setSkipApprovalFor(id string, skip bool) error {
	if d.config() == nil {
		return errors.New("agent not ready")
	}
	if id == "" {
		return errors.New(refuseNoSession)
	}
	// A live run owns its own copy of the record (a turn writes meta from it), so when there is
	// one, write through THAT: writing through the global store would be overwritten by the
	// turn's next save.
	if r := d.runOf(id); r != nil && r.sm != nil {
		if cur := r.sm.Current(); cur != nil {
			if cur.SkipApproval == skip {
				return nil
			}
			prev := cur.SkipApproval
			cur.SkipApproval = skip
			if err := r.sm.UpdateMeta(cur); err != nil {
				cur.SkipApproval = prev
				return fmt.Errorf("保存会话失败：%v", err)
			}
			if r.agent != nil {
				r.agent.SetAllowAllAsksForSession(skip)
			}
			provider, _ := d.sessionPrefs(id)
			d.notifySessionPrefs(id, provider, skip)
			return nil
		}
	}
	if d.sm == nil {
		return errors.New(refuseNoSession)
	}
	ss := d.sessionRecord(id)
	if ss == nil {
		return errors.New(refuseNoSession)
	}
	if ss.SkipApproval == skip {
		return nil
	}
	prev := ss.SkipApproval
	ss.SkipApproval = skip
	if err := d.sm.UpdateMeta(ss); err != nil {
		ss.SkipApproval = prev
		return fmt.Errorf("保存会话失败：%v", err)
	}
	d.notifySessionPrefs(id, ss.ProviderName, skip)
	return nil
}

// mobileAttachment resolves a downloadable attachment: the id must name a SendFile call IN THIS
// session's records. It never takes a path — that is what keeps "the token can read any file"
// from being a capability of this interface (design 不变量 9).
func (d *desktopApp) mobileAttachment(sessionID, attachmentID string) (path, mimeType string, size int64, status int, code string) {
	ss := d.sessionRecord(sessionID)
	if ss == nil {
		return "", "", 0, http.StatusNotFound, "no_session"
	}
	if d.sm == nil {
		return "", "", 0, http.StatusInternalServerError, "no_session"
	}
	raw, err := d.sm.LoadSessionMessages(sessionID)
	if err != nil {
		return "", "", 0, http.StatusNotFound, "no_session"
	}
	for _, rm := range raw {
		if rm.Type != session.MessageTypeToolCall || rm.Name != tools.ToolNameSendFile || rm.ToolCallID != attachmentID {
			continue
		}
		p := sendFilePath(rm.Args)
		if p == "" {
			break
		}
		abs := d.resolveAttachmentPath(ss, p)
		if abs == "" {
			break
		}
		st, statErr := os.Stat(abs)
		if statErr != nil || !st.Mode().IsRegular() {
			break
		}
		if st.Size() > d.mobileAttachmentLimit() {
			return "", "", 0, http.StatusRequestEntityTooLarge, "too_large"
		}
		return abs, mobileMimeType(p), st.Size(), 0, ""
	}
	return "", "", 0, http.StatusNotFound, "no_such_file"
}

// serveMobileAttachment streams one delivered file. The path is never taken from the request:
// it came out of the session's own record a moment ago.
func (d *desktopApp) serveMobileAttachment(w http.ResponseWriter, r *http.Request, sessionID, attachmentID string) {
	path, mimeType, size, status, code := d.mobileAttachment(sessionID, attachmentID)
	if code != "" {
		mobileFail(w, status, code)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		mobileFail(w, http.StatusNotFound, "no_such_file")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", fmt.Sprint(size))
	w.Header().Set("Content-Disposition", "inline; filename="+filepath.Base(path))
	_, _ = io.Copy(w, f)
}
