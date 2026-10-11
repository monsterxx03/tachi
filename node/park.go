package node

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/monsterxx03/tachi/agent/tools"
)

// Parking: what a task does while it waits for an answer to a question.
//
// A parked task is neither running nor finished. The agent loop that asked is blocked
// inside its turn on the agent's own answer channel, and the protocol call that started the
// turn has already returned — INPUT_REQUIRED is where a blocking message/send stops
// (design §6.2). The caller's next message is the only thing that resumes it.
//
// Three rules keep that from answering the wrong question, and each is a specific failure:
//
//   - ONE slot per task, holding the question's id and the questions themselves. An answer
//     that names a different question, or names none, is refused rather than delivered to
//     whatever happens to be waiting (§7.4: expired or mismatched answers are refused, not
//     handed to another waiting slot).
//   - The slot is CONSUMED by the answer it accepts. The agent's answer channel holds one
//     value and carries no request id, so a value left behind would be handed to the NEXT
//     question — a stale "yes" arriving as the answer to something nobody was asked.
//   - Every slot has a DEADLINE, and waiting questions have a LIMIT (§7.4). Without them, a
//     caller that walks away holds a goroutine, a session and a task open forever, and the
//     node has no way to say that the work ended.

// QuestionIDMetadataKey is the metadata key a question's id travels under: on the status
// message that publishes the question, and on the answer message that comes back for it.
//
// The id is the AskUserQuestion tool call's id — it is already unique per question, and it
// is what ties an answer to the question it answers when a caller answers late.
const QuestionIDMetadataKey = "questionId"

// parkedAsk is one question waiting for its answer, and the two things that can end it.
//
// Everything but timer is written once, before the slot becomes visible, which is what lets
// a reader hold the slot (the admission guard, deciding whether a message is an answer)
// without copying it.
type parkedAsk struct {
	taskID     a2a.TaskID
	contextID  string
	questionID string
	questions  []tools.Question

	// wake hands the answers to the loop that asked, and reports whether a loop took them.
	wake func(map[string]string) bool
	// stop ends the waiting turn: the question is not going to be answered by anyone.
	stop func()

	timer *time.Timer
}

// disarm cancels the slot's deadline. Callers hold the lot's lock: the slot and its timer
// become visible together (see park), so they must also be read together.
func (a *parkedAsk) disarm() {
	if a.timer != nil {
		a.timer.Stop()
	}
}

// errTooManyQuestions is the refusal a question gets when the node is already holding as
// many as it will. It is logged and the turn carries on: the model is handed the same empty
// answer a frontend that cannot ask gives it (see liveTurn.asking), because the alternative —
// leaving the turn waiting for a question the caller was never told about — would park work
// nobody can resume.
var errTooManyQuestions = errors.New("node: too many questions are already waiting for an answer")

// errAnswerShape is the refusal an answer gets when it cannot be mapped onto the waiting
// questions — the wrong number of answers, a name that is not a waiting question, or no
// usable content at all. It is a rule of this node's own making (§7.4), which is why it is
// reported with our domain rather than the protocol's InvalidParams.
var errAnswerShape = errors.New("node: the answer does not match the question it answers")

// errNoWaitingQuestion is the refusal an answer gets when this process is not holding the
// question it answers: the turn it was for is gone (cancelled, stopped, or never here —
// a record restored by a process that did not run the turn).
var errNoWaitingQuestion = errors.New("node: no turn in this process is waiting for that answer")

// errQuestionMismatch is the refusal an answer gets when it names a different question — or
// none at all. The answer is not delivered to whatever happens to be parked: the question it
// was for is not the question that is open.
var errQuestionMismatch = errors.New("node: the answer names a different question")

// parking is the set of questions this node is waiting on.
//
// It is keyed by task, which is the granularity the protocol gives us: a task has at most
// one turn, a turn asks one question at a time (the AskUser tool cannot run in a parallel
// group), and a second question can only be asked after the first is answered.
type parking struct {
	// timeout is how long a question waits. Zero or less means no deadline — allowed, and
	// only sensible when something else is watching (a test, a person).
	timeout time.Duration
	// max is how many questions may wait at once.
	max int
	// onExpire ends a task whose question ran out of time. It is called without any lock
	// held: it stops an agent loop and writes a task record.
	onExpire func(*parkedAsk)

	mu    sync.Mutex
	slots map[a2a.TaskID]*parkedAsk
}

func newParking(timeout time.Duration, max int, onExpire func(*parkedAsk)) *parking {
	return &parking{
		timeout:  timeout,
		max:      max,
		onExpire: onExpire,
		slots:    make(map[a2a.TaskID]*parkedAsk),
	}
}

// park registers a waiting question and arms its deadline.
func (p *parking) park(ask *parkedAsk) error {
	if ask == nil || ask.questionID == "" {
		return errors.New("node: a question without an id cannot be answered reliably")
	}

	p.mu.Lock()
	if p.max > 0 && len(p.slots) >= p.max {
		p.mu.Unlock()
		return fmt.Errorf("%w (limit %d)", errTooManyQuestions, p.max)
	}
	p.slots[ask.taskID] = ask
	if p.timeout > 0 && p.onExpire != nil {
		// Armed while the lot's lock is held, so the slot and its deadline become visible
		// together: a timer that fires immediately still finds the slot it belongs to (and
		// the takedown reads the timer under the same lock).
		ask.timer = time.AfterFunc(p.timeout, func() { p.expire(ask) })
	}
	p.mu.Unlock()
	return nil
}

// slot returns the question a task is waiting on, if any.
func (p *parking) slot(taskID a2a.TaskID) (*parkedAsk, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ask, ok := p.slots[taskID]
	return ask, ok
}

// take removes and returns a task's waiting question.
//
// Removing and returning in one step is what makes the slot single-use: two answers racing
// for one question cannot both find it, so the loser is refused instead of being delivered
// to the next question that arrives.
func (p *parking) take(taskID a2a.TaskID) (*parkedAsk, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ask, ok := p.slots[taskID]
	if !ok {
		return nil, false
	}
	delete(p.slots, taskID)
	ask.disarm()
	return ask, true
}

// forget drops a task's waiting question without answering it: the turn was cancelled,
// stopped, or ended on its own. The slot is gone, so a late answer is refused instead of
// being delivered to a loop that is no longer listening — and its deadline can no longer
// fire against a question that is already resolved.
func (p *parking) forget(taskID a2a.TaskID) {
	_, _ = p.take(taskID)
}

// expire fires a slot's deadline.
func (p *parking) expire(ask *parkedAsk) {
	// Only the slot that is still parked expires: the deadline may fire while the answer is
	// being delivered, and answering beats expiring.
	if got, ok := p.take(ask.taskID); !ok || got != ask {
		return
	}
	if p.onExpire != nil {
		p.onExpire(ask)
	}
}

// waiting reports how many questions are parked.
func (p *parking) waiting() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.slots)
}

// QuestionID reads the question id an answer message names, if any.
func QuestionID(msg *a2a.Message) string {
	if msg == nil || msg.Metadata == nil {
		return ""
	}
	id, _ := msg.Metadata[QuestionIDMetadataKey].(string)
	return id
}

// answersFor maps an answer message onto the questions that are waiting, in the shape the
// agent's AskUserQuestion expects: a map keyed by the FULL question text, with values
// holding what the caller chose (the convention the TUI and the desktop established, so the
// model sees one shape whichever frontend asked).
//
// One waiting question takes one text answer — the caller's free text, a picked option's
// label, it is all the same to this node. SEVERAL waiting questions need the answers NAMED:
// one text answer cannot say which question it belongs to, and a message has no positions.
// A shape that cannot be mapped is refused rather than guessed at, because the friendly
// guess (first answer to first question) is the one that silently answers something else.
func answersFor(questions []tools.Question, msg *a2a.Message) (map[string]string, error) {
	if msg == nil {
		return nil, fmt.Errorf("%w: the answer is empty", errAnswerShape)
	}

	var text strings.Builder
	var named map[string]any
	for _, part := range msg.Parts {
		switch v := part.Content.(type) {
		case a2a.Text:
			text.WriteString(string(v))
		case a2a.Data:
			if kv, ok := v.Value.(map[string]any); ok {
				if named == nil {
					named = make(map[string]any, len(kv))
				}
				for k, val := range kv {
					named[k] = val
				}
			}
		case a2a.Raw, a2a.URL:
			// A question is answered with words or with a choice, not with bytes: a file on an
			// answer is not a shape this node can map onto a question, and the rule here is that an
			// unmappable shape is refused rather than ignored (§7.4) — silently dropping it is what
			// an earlier version of this path did.
			return nil, fmt.Errorf("%w: an answer cannot carry a file; a waiting question is answered "+
				"with text, or with a data part naming the question", errAnswerShape)
		}
	}

	if len(named) > 0 {
		return keyedAnswers(questions, named)
	}
	if len(questions) == 1 {
		if strings.TrimSpace(text.String()) == "" {
			return nil, fmt.Errorf("%w: this question needs an answer, and the message carried no text",
				errAnswerShape)
		}
		return map[string]string{questions[0].Question: text.String()}, nil
	}
	return nil, fmt.Errorf("%w: %d questions are waiting, so the answers must name the question they answer "+
		"(a data part: {\"<question>\": \"<answer>\"})", errAnswerShape, len(questions))
}

// keyedAnswers turns a named answer set into the agent's shape, refusing anything that does
// not name a waiting question: an answer to a question this task is not asking is a client
// that has the wrong idea, and delivering it would answer whichever question happens to be
// parked instead.
func keyedAnswers(questions []tools.Question, named map[string]any) (map[string]string, error) {
	known := make(map[string]bool, len(questions))
	for _, q := range questions {
		known[q.Question] = true
	}

	out := make(map[string]string, len(named))
	for k, v := range named {
		if !known[k] {
			return nil, fmt.Errorf("%w: %q is not a question this task is asking", errAnswerShape, k)
		}
		out[k] = answerText(v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: the answer named no question", errAnswerShape)
	}
	return out, nil
}

// answerText renders one answer value the way the TUI renders a multi-select: the chosen
// labels joined with ", ". Anything that is not a string is rendered by its value rather
// than dropped — a caller that answered with `true` or `3` answered.
func answerText(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case []any:
		parts := make([]string, 0, len(val))
		for _, item := range val {
			parts = append(parts, answerText(item))
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(val)
	}
}
