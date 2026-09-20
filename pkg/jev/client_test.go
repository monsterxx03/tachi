package jev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/pkg/retry"
)

// testBackoff keeps the retry tests instant. The production backoff's first
// delay is a full second, which would make a retry test the slowest thing in
// the suite for no added coverage.
var testBackoff = retry.Backoff{BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}

// newTestClient points a client at a stub server, with a backoff fast enough
// to retry inside a test.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClient("test-key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.backoff = testBackoff
	return c
}

// oneQuestion is the minimum valid request, for tests that care about the
// transport rather than the payload.
func oneQuestion() map[string]Question {
	return map[string]Question{"q": NewNoul("q?", NoulCriteria{})}
}

func TestEvaluateSendsDerivedTypesAndCriteriaShapes(t *testing.T) {
	// The handler runs on the server's goroutine, where t.Fatalf is illegal,
	// so it only hands the raw body back over a channel; decoding (and any
	// Fatalf it needs) happens on the test goroutine.
	bodyCh := make(chan []byte, 1)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := r.URL.Path; got != evaluatePath {
			t.Errorf("path = %q, want %q", got, evaluatePath)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		bodyCh <- body
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{},"usage":{}}`))
	})

	_, err := c.Evaluate(context.Background(), "some state", map[string]Question{
		"urgent": NewNoul("Does this convey urgency?", NoulCriteria{Yes: "Time-sensitive", No: "No urgency"}),
		"team": NewChoice("Which team should handle this?",
			ChoiceOption{Name: "billing", Rubric: "Payments"},
			ChoiceOption{Name: "other"},
		),
		"tone": NewScore("How frustrated is the customer?", "Calm", "Very angry"),
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	var captured map[string]any
	if err := json.Unmarshal(<-bodyCh, &captured); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if got := captured["state"]; got != "some state" {
		t.Errorf("state = %v, want %q", got, "some state")
	}
	if got := captured["model"]; got != DefaultModel {
		t.Errorf("model = %v, want %q", got, DefaultModel)
	}

	questions, ok := captured["questions"].(map[string]any)
	if !ok {
		t.Fatalf("questions is %T, want object", captured["questions"])
	}

	// Noul: `type` derived from the criteria, criteria keyed true/false.
	noul, _ := questions["urgent"].(map[string]any)
	if got := noul["type"]; got != string(QuestionNoul) {
		t.Errorf("urgent.type = %v, want %q", got, QuestionNoul)
	}
	criteria, _ := noul["criteria"].(map[string]any)
	if got := criteria["true"]; got != "Time-sensitive" {
		t.Errorf("urgent.criteria.true = %v, want %q", got, "Time-sensitive")
	}
	if got := criteria["false"]; got != "No urgency" {
		t.Errorf("urgent.criteria.false = %v, want %q", got, "No urgency")
	}

	// Choice: criteria is an option -> rubric map, and a blank rubric must
	// arrive as null (the API's "no extra detail"), not as an empty string.
	choice, _ := questions["team"].(map[string]any)
	if got := choice["type"]; got != string(QuestionChoice) {
		t.Errorf("team.type = %v, want %q", got, QuestionChoice)
	}
	opts, _ := choice["criteria"].(map[string]any)
	if got := opts["billing"]; got != "Payments" {
		t.Errorf("team.criteria.billing = %v, want %q", got, "Payments")
	}
	if rubric, present := opts["other"]; !present || rubric != nil {
		t.Errorf("team.criteria.other = %v (present=%v), want explicit null", rubric, present)
	}

	// Score: criteria is an ordered array of levels.
	score, _ := questions["tone"].(map[string]any)
	if got := score["type"]; got != string(QuestionScore) {
		t.Errorf("tone.type = %v, want %q", got, QuestionScore)
	}
	levels, ok := score["criteria"].([]any)
	if !ok {
		t.Fatalf("tone.criteria is %T, want array", score["criteria"])
	}
	if len(levels) != 2 || levels[0] != "Calm" || levels[1] != "Very angry" {
		t.Errorf("tone.criteria = %v, want [Calm, Very angry]", levels)
	}
}

func TestEvaluateParsesAnswerTypes(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"model": "jev-1.13.0",
			"answers": {
				"urgent": {"type": "noul", "noul": 0.92},
				"team": {"type": "choice", "choice": "technical",
					"probabilities": {"billing": 0.08, "technical": 0.85, "other": 0.07},
					"confidence": 0.82},
				"tone": {"type": "score", "score": 1.6, "legend": {"0": "Calm", "1": "Very angry"},
					"probabilities": {"0": 0.4, "1": 0.6}, "confidence": 0.78}
			},
			"usage": {"input_tokens": 312, "output_tokens": 48}
		}`))
	})

	resp, err := c.Evaluate(context.Background(), "state", oneQuestion())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if resp.Model != "jev-1.13.0" {
		t.Errorf("model = %q", resp.Model)
	}
	if resp.Usage.InputTokens != 312 || resp.Usage.OutputTokens != 48 {
		t.Errorf("usage = %+v, want 312/48", resp.Usage)
	}

	// A noul answer carries no confidence: the probability IS the answer, and
	// the field must stay absent rather than decode as 0.
	noul := resp.Answers["urgent"]
	if noul.Noul == nil || *noul.Noul != 0.92 {
		t.Errorf("urgent.Noul = %v, want 0.92", noul.Noul)
	}
	if noul.Confidence != nil {
		t.Errorf("urgent.Confidence = %v, want absent", *noul.Confidence)
	}

	choice := resp.Answers["team"]
	if choice.Choice != "technical" {
		t.Errorf("team.Choice = %q, want technical", choice.Choice)
	}
	if choice.Confidence == nil || *choice.Confidence != 0.82 {
		t.Errorf("team.Confidence = %v, want 0.82", choice.Confidence)
	}

	score := resp.Answers["tone"]
	if score.Score == nil || *score.Score != 1.6 {
		t.Errorf("tone.Score = %v, want 1.6", score.Score)
	}
	if score.Legend["1"] != "Very angry" {
		t.Errorf("tone.Legend = %v", score.Legend)
	}
}

func TestAnswerProbabilityOf(t *testing.T) {
	yes := 0.92
	score := 1.6

	tests := []struct {
		name   string
		answer Answer
		label  string
		want   float64
		wantOK bool
	}{
		{name: "noul true", answer: Answer{Type: QuestionNoul, Noul: &yes}, label: "true", want: 0.92, wantOK: true},
		{name: "noul false is the complement", answer: Answer{Type: QuestionNoul, Noul: &yes}, label: "false", want: 0.08, wantOK: true},
		{name: "noul unknown label", answer: Answer{Type: QuestionNoul, Noul: &yes}, label: "maybe", wantOK: false},
		{name: "noul missing value", answer: Answer{Type: QuestionNoul}, label: "true", wantOK: false},
		{
			name:   "choice option",
			answer: Answer{Type: QuestionChoice, Probabilities: map[string]float64{"billing": 0.1, "technical": 0.9}},
			label:  "technical", want: 0.9, wantOK: true,
		},
		{
			name:   "choice absent option",
			answer: Answer{Type: QuestionChoice, Probabilities: map[string]float64{"technical": 0.9}},
			label:  "other", wantOK: false,
		},
		{
			name:   "score has no labelled distribution",
			answer: Answer{Type: QuestionScore, Score: &score, Probabilities: map[string]float64{"0": 0.4, "1": 0.6}},
			label:  "0", wantOK: false,
		},
		{name: "zero value answer", answer: Answer{}, label: "true", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.answer.ProbabilityOf(tc.label)
			if ok != tc.wantOK {
				t.Fatalf("ProbabilityOf(%q) ok = %v, want %v", tc.label, ok, tc.wantOK)
			}
			// Epsilon, not equality: a noul's "false" label is the complement
			// of the yes-probability, so 1-0.92 lands on 0.07999999999999996.
			if ok && math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("ProbabilityOf(%q) = %v, want %v", tc.label, got, tc.want)
			}
		})
	}
}

func TestEvaluateRetriesRetryableStatuses(t *testing.T) {
	// Each stub answers with the listed statuses in order, then repeats the
	// last one. The second entry is always 200, so success means the retry
	// happened.
	tests := []struct {
		name      string
		first     int
		wantCalls int32
	}{
		{name: "429 rate limited", first: 429, wantCalls: 2},
		{name: "529 overloaded", first: 529, wantCalls: 2},
		{name: "503 unavailable", first: 503, wantCalls: 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(tc.first)
					_, _ = w.Write([]byte(`{"error":"retry me"}`))
					return
				}
				_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{},"usage":{}}`))
			})

			if _, err := c.Evaluate(context.Background(), "state", oneQuestion()); err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestEvaluateDoesNotRetryNonRetryableStatuses(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "401 unauthorized", status: 401},
		{name: "422 unprocessable", status: 422},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"the body details the offending field"}`))
			})

			_, err := c.Evaluate(context.Background(), "state", oneQuestion())

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v (%T), want *APIError", err, err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.status)
			}
			// The service's error body is the only trustworthy diagnostic, so
			// it must survive into the message.
			if !strings.Contains(apiErr.Error(), "offending field") {
				t.Errorf("Error() = %q, want the response body included", apiErr.Error())
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("calls = %d, want 1 (no retry)", got)
			}
		})
	}
}

func TestEvaluateGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`{"error":"overloaded"}`))
	})
	WithMaxAttempts(3)(c)

	_, err := c.Evaluate(context.Background(), "state", oneQuestion())

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 529 {
		t.Fatalf("error = %v, want *APIError with 529", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

// A cancelled context must stop the loop rather than sleep out a backoff.
func TestEvaluateStopsRetryingOnCancelledContext(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
	})
	// Long enough that abandoning the backoff is the only way to finish fast.
	c.backoff = retry.Backoff{BaseDelay: time.Second, MaxDelay: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.Evaluate(ctx, "state", oneQuestion())
	if err == nil {
		t.Fatal("Evaluate error = nil, want a cancellation error")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Evaluate took %v, want it to abandon the backoff on cancel", elapsed)
	}
}

func TestEvaluateRejectsMalformedQuestions(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the service must not be contacted for a locally invalid request")
	})

	tests := []struct {
		name string
		qs   map[string]Question
	}{
		{name: "no questions", qs: map[string]Question{}},
		{name: "blank instructions", qs: map[string]Question{
			"q": {Instructions: "   ", Criteria: NoulCriteria{}},
		}},
		{name: "missing criteria", qs: map[string]Question{
			"q": {Instructions: "q?"},
		}},
		{name: "choice with no options", qs: map[string]Question{
			"q": NewChoice("q?"),
		}},
		{name: "score with a single level", qs: map[string]Question{
			"q": NewScore("q?", "only"),
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Evaluate(context.Background(), "state", tc.qs); err == nil {
				t.Fatal("Evaluate error = nil, want a validation error")
			}
		})
	}
}

func TestNewClientRequiresAPIKey(t *testing.T) {
	if _, err := NewClient(""); !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("NewClient(\"\") error = %v, want ErrNoAPIKey", err)
	}
	if _, err := NewClient("   "); !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("NewClient(blank) error = %v, want ErrNoAPIKey", err)
	}
}

// A blank model must fall back to the pinned version, never to the drifting
// `jev-latest` alias.
func TestClientDefaultsAreReapplied(t *testing.T) {
	c, err := NewClient("k", WithModel(""), WithBaseURL(""), WithMaxAttempts(0))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Model() != DefaultModel {
		t.Errorf("model = %q, want %q", c.Model(), DefaultModel)
	}
	if DefaultModel == "jev-latest" {
		t.Error("DefaultModel must pin a version, not the jev-latest alias")
	}
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
	if c.attempts != 1 {
		t.Errorf("attempts = %d, want 1 (clamped)", c.attempts)
	}
}

func TestNewChoiceCriteriaBlankRubricBecomesNull(t *testing.T) {
	got := NewChoiceCriteria(
		ChoiceOption{Name: "a", Rubric: "described"},
		ChoiceOption{Name: "b"},
	)
	if got["a"] == nil || *got["a"] != "described" {
		t.Errorf("criteria[a] = %v, want %q", got["a"], "described")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if rubric, present := decoded["b"]; !present || rubric != nil {
		t.Errorf("criteria[b] = %v (present=%v), want explicit null", rubric, present)
	}
}
