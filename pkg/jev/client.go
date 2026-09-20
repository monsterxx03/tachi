// Package jev is a client for TypeSafe's System One evaluation endpoint —
// the model family TypeSafe ships as Jev.
//
// It is NOT a chat-completion API, and deliberately does not implement
// llm.Provider. A request carries a `state` and a map of typed `questions`;
// the response carries one typed answer per question — a probability (noul),
// an option plus its full distribution (choice), or a probability-weighted
// position on ordered levels (score). No text is generated, so a Jev call can
// never stand in for one that generates — only for a call whose RESULT the
// caller immediately interprets with code (an if, a threshold, a switch).
//
// Four properties of the service shape how it must be called. They are
// measured facts rather than advertised behaviour, so re-check them before
// leaning on them — the service adjusts its limits dynamically and the model
// is versioned:
//
//   - Two token limits exist and the TIGHTER one binds: `state` plus the
//     LONGEST single question must fit under roughly 32k input tokens, well
//     before the 64k total-request budget is reached. Question COUNT spends
//     the 64k budget, not the 32k one, so prefer many short questions over
//     one enormous one.
//   - Non-text state fails SILENTLY and CONFIDENTLY. An image sent as base64
//     returns HTTP 200 with a plausible answer whose confidence is higher
//     than the same request carrying noise — proving the answer was read off
//     nothing. Convert images and audio to text (OCR/CV) before they reach
//     this package.
//   - `confidence` measures how concentrated the distribution is over the
//     options the CALLER supplied, not whether the answer is right. Adding,
//     removing or rewording an option rescales it, so a threshold calibrated
//     against one criteria set does not transfer to another.
//   - HTTP 200 with a high confidence is not evidence of a correct answer.
//     Only 4xx/5xx — surfaced here as *APIError — are trustworthy signals.
//
// Pin Model to a concrete version: the service's `jev-latest` alias moves,
// and a threshold swept against the old weights is silently invalidated when
// it does.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/monsterxx03/tachi/pkg/httpx"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/monsterxx03/tachi/pkg/retry"
	"github.com/monsterxx03/tachi/pkg/strutil"
)

const (
	// DefaultBaseURL is TypeSafe's API host.
	DefaultBaseURL = "https://api.typesafe.ai"

	// evaluatePath is the System One evaluation endpoint, relative to BaseURL.
	evaluatePath = "/v1/systemone"

	// DefaultModel pins the model VERSION. Do not replace this with the
	// `jev-latest` alias: the alias moves, and every threshold calibrated
	// against the pinned weights stops applying when it does.
	DefaultModel = "jev-1.13.0"

	// DefaultTimeout bounds a single HTTP attempt; the retry loop can outlast
	// it several times over. It is the value config.JevConfig.Timeout carries
	// by default (config.DefaultJevTimeout), so a caller that builds a client
	// with no config is bounded like one that passes the field through.
	DefaultTimeout = 10 * time.Second

	// DefaultMaxAttempts bounds the retry loop for retryable failures.
	DefaultMaxAttempts = 4

	// maxResponseBytes caps a response body. An evaluation response is a few
	// answers plus a usage block — kilobytes at most — so this is slack that
	// never fires in practice, not a working limit.
	maxResponseBytes = 1 << 20

	// errorBodyLimit bounds how much of an error body reaches an error string
	// and a log line.
	errorBodyLimit = 512

	// minScoreLevels is the API's floor on a score question's criteria.
	minScoreLevels = 2

	// labelTrue and labelFalse are the labels ProbabilityOf accepts for a
	// noul answer. They mirror the wire criteria keys.
	labelTrue  = "true"
	labelFalse = "false"
)

// retryBackoff is the exponential backoff for retryable failures — the
// service's documented 429 / 529 guidance, applied to any retryable status.
var retryBackoff = retry.Backoff{BaseDelay: time.Second, MaxDelay: 30 * time.Second}

// ErrNoAPIKey is returned by NewClient when no API key is supplied.
var ErrNoAPIKey = errors.New("jev: api key is required")

// ErrNoQuestions is returned by Evaluate when the questions map is empty:
// the endpoint would reject it with 422, which is worth catching locally.
var ErrNoQuestions = errors.New("jev: at least one question is required")

// QuestionType is the wire value of a question's `type` field.
type QuestionType string

const (
	QuestionNoul   QuestionType = "noul"
	QuestionChoice QuestionType = "choice"
	QuestionScore  QuestionType = "score"
)

// Criteria is a question's rubric. Exactly three types implement it —
// NoulCriteria, ChoiceCriteria and ScoreCriteria, one per QuestionType. The
// questionType method is unexported, which seals the set: a caller cannot
// pair choice criteria with a noul question, and that pairing is the only
// way to build a request the service rejects with 422.
type Criteria interface {
	questionType() QuestionType
}

// NoulCriteria states what the two poles mean. Both fields are optional in
// the API, but supplying them is what makes the returned probability
// interpretable — without them the model has to guess what "yes" covers, and
// the guess tends to inflate the number toward the label it picked.
type NoulCriteria struct {
	// Yes describes what a value near 1 means.
	Yes string `json:"true,omitempty"`
	// No describes what a value near 0 means.
	No string `json:"false,omitempty"`
}

func (NoulCriteria) questionType() QuestionType { return QuestionNoul }

// ChoiceCriteria maps each option to the rubric that selects it. A nil
// rubric marshals to null, which the API accepts for an option that needs no
// extra detail.
//
// Always include a catch-all option ("other" / "none of the above" /
// "unknown"), even when the option list looks exhaustive. It is not merely a
// net for inputs you forgot: it is what calibrates the distribution. Without
// one the model is forced to pick something, and it resolves that by raising
// its confidence — so a threshold swept on the result comes out
// systematically too high, and the gate it drives then misses exactly the
// cases it exists to catch.
type ChoiceCriteria map[string]*string

func (ChoiceCriteria) questionType() QuestionType { return QuestionChoice }

// ChoiceOption is one option of a ChoiceCriteria, for callers that prefer
// building the map through NewChoice over taking pointers by hand.
type ChoiceOption struct {
	// Name is the option id, and is what a choice answer reports back.
	Name string
	// Rubric describes when this option applies; empty means null.
	Rubric string
}

// NewChoiceCriteria builds ChoiceCriteria from options. Option ORDER is not
// preserved — the wire form is a JSON object and encoding/json emits its keys
// sorted — so the option names must carry the meaning on their own.
func NewChoiceCriteria(options ...ChoiceOption) ChoiceCriteria {
	c := make(ChoiceCriteria, len(options))
	for _, o := range options {
		if o.Rubric == "" {
			c[o.Name] = nil
			continue
		}
		rubric := o.Rubric
		c[o.Name] = &rubric
	}
	return c
}

// ScoreCriteria is an ordered set of level descriptions, low to high. The
// API requires at least two. Levels must describe concrete situations that
// stand on their own: a level defined only by contrast with its neighbour is
// not scorable.
type ScoreCriteria []string

func (ScoreCriteria) questionType() QuestionType { return QuestionScore }

// Question is one typed judgment. The `type` field is derived from Criteria
// rather than stored, so the two cannot disagree.
type Question struct {
	// Instructions states the judgment in full. The question's map key is
	// for the caller only — it is NOT sent to the model — so everything the
	// model needs to know about what is being asked belongs here.
	Instructions string `json:"instructions"`
	// Criteria is the rubric, and is required.
	Criteria Criteria `json:"criteria"`
}

// NewNoul builds a yes/no question returning the probability the answer is yes.
func NewNoul(instructions string, criteria NoulCriteria) Question {
	return Question{Instructions: instructions, Criteria: criteria}
}

// NewChoice builds a question that picks one option from a defined set and
// returns the whole distribution over them.
func NewChoice(instructions string, options ...ChoiceOption) Question {
	return Question{Instructions: instructions, Criteria: NewChoiceCriteria(options...)}
}

// NewScore builds a question rating the state along ordered levels.
func NewScore(instructions string, levels ...string) Question {
	return Question{Instructions: instructions, Criteria: ScoreCriteria(levels)}
}

// MarshalJSON emits the wire form, deriving `type` from the criteria.
func (q Question) MarshalJSON() ([]byte, error) {
	type wireQuestion struct {
		Type         QuestionType `json:"type"`
		Instructions string       `json:"instructions"`
		Criteria     Criteria     `json:"criteria"`
	}
	if q.Criteria == nil {
		return nil, errors.New("no criteria")
	}
	return json.Marshal(wireQuestion{
		Type:         q.Criteria.questionType(),
		Instructions: q.Instructions,
		Criteria:     q.Criteria,
	})
}

// Validate reports whether the question can be sent. It catches the shape
// errors the service would answer with 422, so a malformed question fails
// before spending a round trip.
func (q Question) Validate() error {
	if strings.TrimSpace(q.Instructions) == "" {
		return errors.New("instructions is required")
	}
	switch c := q.Criteria.(type) {
	case NoulCriteria:
		return nil
	case ChoiceCriteria:
		if len(c) == 0 {
			return errors.New("choice criteria needs at least one option")
		}
		return nil
	case ScoreCriteria:
		if len(c) < minScoreLevels {
			return fmt.Errorf("score criteria needs at least %d levels, got %d", minScoreLevels, len(c))
		}
		return nil
	default:
		return fmt.Errorf("criteria is required, got %T", q.Criteria)
	}
}

// Usage reports the token cost of one evaluation. Billing is on INPUT tokens;
// output is free, though a large speculative fan-out still counts toward rate
// limiting.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Answer is the typed result of one question. Only the fields matching Type
// are populated: Noul for a noul question; Choice, Probabilities and
// Confidence for a choice; Score, Legend, Probabilities and Confidence for a
// score.
//
// The scalar fields are pointers because absence is meaningful — a noul
// answer carries no confidence at all, and a missing field must not read as a
// zero value.
type Answer struct {
	Type QuestionType `json:"type"`
	// Noul is the yes-probability in 0..1. A value near 0.5 means yes and no
	// are about equally likely — not that the answer is "medium".
	Noul *float64 `json:"noul,omitempty"`
	// Choice is the highest-probability option.
	Choice string `json:"choice,omitempty"`
	// Probabilities is the full distribution over the options the caller
	// defined; it sums to 1.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Score is the probability-weighted position, which may land BETWEEN
	// levels. Use it for thresholding, not for reading off a precise value.
	Score *float64 `json:"score,omitempty"`
	// Legend maps each level index, as a string key, back to its description.
	Legend map[string]string `json:"legend,omitempty"`
	// Confidence summarises how concentrated Probabilities is. It is relative
	// to the option set, so it does not transfer between criteria.
	Confidence *float64 `json:"confidence,omitempty"`
}

// ProbabilityOf reports the probability this answer assigns to a label, and
// whether that label means anything for the answer's type.
//
// It exists because gating is always "act when p(label) clears a threshold",
// and the label is read differently per primitive: an option's entry in a
// choice distribution, or the yes-probability (label "true") or its
// complement (label "false") for a noul. A score answer has no labelled
// distribution — threshold on Score instead — so every label returns false.
func (a Answer) ProbabilityOf(label string) (float64, bool) {
	switch a.Type {
	case QuestionNoul:
		if a.Noul == nil {
			return 0, false
		}
		switch label {
		case labelTrue:
			return *a.Noul, true
		case labelFalse:
			return 1 - *a.Noul, true
		}
		return 0, false
	case QuestionChoice:
		p, ok := a.Probabilities[label]
		return p, ok
	default:
		return 0, false
	}
}

// Response is the result of one evaluation: one answer per question, keyed by
// the ids the caller supplied.
type Response struct {
	// Model echoes the model that performed the evaluation.
	Model string `json:"model"`
	// Answers carries one Answer per question id.
	Answers map[string]Answer `json:"answers"`
	// Usage is the token cost of the request.
	Usage Usage `json:"usage"`
}

// APIError is a non-2xx response from the evaluation endpoint. The status
// code is the trustworthy half of the service's behaviour — 401 (bad key),
// 422 (malformed request), 429 (rate limited) and 529 (overloaded) all mean
// what they say, unlike a 200 carrying a confident wrong answer.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	if body := strutil.TruncatePlain(e.Body, errorBodyLimit); body != "" {
		return fmt.Sprintf("jev: HTTP %d: %s", e.StatusCode, body)
	}
	return fmt.Sprintf("jev: HTTP %d", e.StatusCode)
}

// request is the wire body of one evaluation.
type request struct {
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

// Client evaluates states against typed questions. It is safe for concurrent
// use.
type Client struct {
	apiKey   string
	baseURL  string
	model    string
	http     *http.Client
	logger   *logger.Logger
	backoff  retry.Backoff
	attempts int
}

// Option configures a Client. Options are applied in order.
type Option func(*Client)

// WithBaseURL overrides the API host. An empty value keeps DefaultBaseURL.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = baseURL }
}

// WithModel overrides the model. An empty value keeps DefaultModel — which is
// the safe direction, so a blank config field cannot silently opt a
// deployment into the drifting `jev-latest` alias.
func WithModel(model string) Option {
	return func(c *Client) { c.model = model }
}

// WithHTTPClient replaces the transport wholesale, including the timeout
// NewClient would otherwise set from DefaultTimeout.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// WithLogger attaches a logger. Without one, calls log through the package
// default.
func WithLogger(l *logger.Logger) Option {
	return func(c *Client) { c.logger = l }
}

// WithMaxAttempts bounds the retry loop. Values below 1 clamp to 1, i.e. no
// retry.
func WithMaxAttempts(n int) Option {
	return func(c *Client) { c.attempts = n }
}

// NewClient builds a client for the given API key. It does not contact the
// service, so a bad key surfaces on the first Evaluate as *APIError with
// status 401.
func NewClient(apiKey string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, ErrNoAPIKey
	}
	c := &Client{
		apiKey:   apiKey,
		baseURL:  DefaultBaseURL,
		model:    DefaultModel,
		http:     httpx.NewClient(DefaultTimeout, ""),
		backoff:  retryBackoff,
		attempts: DefaultMaxAttempts,
	}
	for _, opt := range opts {
		opt(c)
	}
	// Re-apply the defaults an option may have blanked out.
	if strings.TrimSpace(c.model) == "" {
		c.model = DefaultModel
	}
	if strings.TrimSpace(c.baseURL) == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.attempts < 1 {
		c.attempts = 1
	}
	return c, nil
}

// Model reports the model version this client sends, so a caller can record
// which weights produced a calibration.
func (c *Client) Model() string { return c.model }

// Evaluate runs state against questions in a single request and returns the
// typed answers.
//
// All questions in one request are evaluated over the same state, run in
// parallel, and cannot see one another's answers — so a judgment that depends
// on an earlier answer needs a second call.
//
// Keep `state` plus the LONGEST single question under roughly 32k input
// tokens: that is the limit that binds, not the 64k total. Response.Usage
// reports what the request actually cost.
func (c *Client) Evaluate(ctx context.Context, state any, questions map[string]Question) (*Response, error) {
	if len(questions) == 0 {
		return nil, ErrNoQuestions
	}
	for id, q := range questions {
		if err := q.Validate(); err != nil {
			return nil, fmt.Errorf("jev: question %q: %w", id, err)
		}
	}
	body, err := json.Marshal(request{State: state, Model: c.model, Questions: questions})
	if err != nil {
		return nil, fmt.Errorf("jev: encode request: %w", err)
	}

	start := time.Now()
	var lastErr error
	for attempt := 1; attempt <= c.attempts; attempt++ {
		resp, retryable, err := c.post(ctx, body)
		if err == nil {
			c.logger.Debug(ctx, "jev: evaluated",
				"model", resp.Model, "questions", len(questions),
				"input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens,
				"attempt", attempt, "elapsed", time.Since(start).Round(time.Millisecond).String())
			return resp, nil
		}
		lastErr = err
		if !retryable || attempt == c.attempts {
			break
		}
		// A cancelled context is not a transient failure: stop rather than
		// sleep out the backoff.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		delay := c.backoff.Delay(attempt)
		c.logger.Warn(ctx, "jev: retrying", "attempt", attempt, "delay", delay.String(), "error", err.Error())
		if err := retry.Sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// post performs one HTTP attempt. The bool reports whether the failure is
// worth retrying: 429 and any 5xx per httpx.IsRetryableStatus (529 Overloaded
// included), plus transport errors, which are as likely transient as an
// overload is.
func (c *Client) post(ctx context.Context, body []byte) (*Response, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpx.JoinURL(c.baseURL, evaluatePath), bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("jev: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("jev: request: %w", err)
	}
	defer resp.Body.Close()

	data, readErr := httpx.ReadAllLimited(resp.Body, maxResponseBytes)
	if readErr != nil {
		return nil, false, fmt.Errorf("jev: read response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, httpx.IsRetryableStatus(resp.StatusCode), &APIError{StatusCode: resp.StatusCode, Body: string(data)}
	}
	var out Response
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, false, fmt.Errorf("jev: decode response: %w", err)
	}
	return &out, false, nil
}
