package main

import (
	"fmt"

	"github.com/monsterxx03/tachi/agent/tools"
)

// AskEvent is emitted to the frontend when the agent asks the user questions
// (the AskUserQuestion tool). The turn is parked on the answer channel until the
// frontend replies via AgentService.AnswerQuestion — same mechanism the TUI uses
// through AIAgent.RespondToAskUser.
type AskEvent struct {
	SessionID string           `json:"sessionId"`
	ToolID    string           `json:"toolId"`
	Questions []tools.Question `json:"questions"`
}

// AnswerQuestion delivers the user's answers to a session's pending
// AskUserQuestion, unblocking the agent loop.
//
// Answers are keyed by the full question text, with values holding the selected
// option labels (and any free text) joined with ", " — the convention the TUI
// established, so the model sees one consistent shape whichever frontend asked.
// Empty answers mean the user declined/cancelled: the model is told the question
// went unanswered rather than being handed a made-up choice.
func (s *AgentService) AnswerQuestion(sessionID string, answers, annotations map[string]string) string {
	if err := s.desk.answerAskUser(sessionID, answers, annotations); err != nil {
		return err.Error()
	}
	return "ok"
}

// answerAskUser is that answer path itself; the device link (badge.go) comes
// through here too.
//
// It must also drop the pending row. Nothing else unwinds an AskUser wait from this
// side: the agent loop is parked on ITS OWN channel (RespondToAskUser), not on one
// this half owns, so there is no waiting goroutine whose defer could clean up. A row
// left behind would keep the device showing a question that has already been answered
// — and, worse, would offer it a button that answers nothing.
func (d *desktopApp) answerAskUser(sessionID string, answers, annotations map[string]string) error {
	d.mu.Lock()
	r := d.getRun(sessionID)
	a := r.agent
	d.mu.Unlock()
	if a == nil {
		return fmt.Errorf("no agent")
	}
	a.RespondToAskUser(answers, annotations)
	d.dropAsks(sessionID, askKindAskUser)
	// toolID 这里没有：AskUser 的回答按会话走（见 answerAskUser 的注释），
	// 所以 toolID 留空，前端按会话清。
	d.notifyAskAnswered(sessionID, "", askKindAskUser)
	return nil
}
