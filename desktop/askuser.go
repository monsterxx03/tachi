package main

import (
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
	d := s.desk
	d.mu.Lock()
	r := d.getRun(sessionID)
	a := r.agent
	d.mu.Unlock()
	if a == nil {
		return "no agent"
	}
	a.RespondToAskUser(answers, annotations)
	return "ok"
}
