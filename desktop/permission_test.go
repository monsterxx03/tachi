package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/permission"
	"github.com/monsterxx03/tachi/agent/tools"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
)

// askCmd + askArgs are one Bash call the way the agent sees it: the tool args as
// JSON (what the notification names) and the preview the agent renders for the card
// (what the user reads).
const (
	askCmd  = "git push origin main"
	askArgs = `{"command":"git push origin main"}`
	askPrev = "$ git push origin main\n\n命中 ask 规则: \"git push*\""
)

// newAskTest builds the pieces one parked ask needs: the app (a bare one — with no
// *application.App the event emit is skipped, which is exactly what lets this run
// without a window server) and an agent carrying a policy whose only rule is an
// `ask` on git push.
//
// The agent is a REAL one (see newPermissionTestAgent) rather than a zero-value
// construct: answering 「本会话全部允许」 lands on the session's PermissionState as
// well as on the policy — one answer, two readers, and the judgment gates read the
// former — so an agent without session state is no longer enough to answer a card.
// No provider traffic either way: the agent is built with SkipConfigure.
func newAskTest(t *testing.T) (*desktopApp, *AgentService, *agent.AIAgent, *permission.Policy) {
	t.Helper()
	d := newDesktopApp()
	p := permission.NewPolicy(permission.Rules{Ask: []string{"git push*"}}, permission.Rules{})
	a := newPermissionTestAgent(t)
	a.SetPermissionPolicy(p)
	return d, &AgentService{desk: d}, a, p
}

// newPermissionTestAgent builds a REAL agent (a zero-value one has no
// PermissionState, which is where the session-wide switches live) without any
// provider traffic: llm.NewProvider only constructs a client, it never dials.
func newPermissionTestAgent(t *testing.T) *agent.AIAgent {
	t.Helper()
	prov, err := llm.NewProvider("openai", "k", "https://api.test/v1", "m")
	if err != nil {
		t.Fatalf("llm.NewProvider: %v", err)
	}
	a, _, err := agent.NewAIAgentWithConfig(t.Context(), agent.AgentConfig{
		Resolved:      &llm.ResolvedProvider{Provider: prov},
		Logger:        logger.Default(),
		SkipConfigure: true,
	})
	if err != nil {
		t.Fatalf("agent.NewAIAgentWithConfig: %v", err)
	}
	return a
}

// startAsk runs askPermission in the background and returns the channel its
// outcome arrives on, waiting until the ask is actually registered (a fixed sleep
// would be a race in both directions).
func startAsk(t *testing.T, d *desktopApp, a *agent.AIAgent) chan askOutcome {
	t.Helper()
	out := make(chan askOutcome, 1)
	go func() {
		ok, err := d.askPermission(context.Background(), "s1", a, "Bash", "call_1", askPrev, askArgs)
		out <- askOutcome{approved: ok, err: err}
	}()
	waitAskRegistered(t, d, "s1", "call_1", true)
	return out
}

type askOutcome struct {
	approved bool
	err      error
}

// waitAskRegistered polls until a pending ask is (or is no longer) registered.
func waitAskRegistered(t *testing.T, d *desktopApp, sessionID, toolID string, want bool) {
	t.Helper()
	key := permKey(sessionID, toolID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		_, ok := d.asks[key]
		d.mu.Unlock()
		if ok == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending ask %q never became %v", key, want)
}

// awaitAsk reads the outcome, failing (rather than hanging) if the wait never
// ended — a parked goroutine that is never released is the failure mode this
// whole feature must not have.
func awaitAsk(t *testing.T, out chan askOutcome) askOutcome {
	t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(3 * time.Second):
		t.Fatal("askPermission never returned")
		return askOutcome{}
	}
}

func TestBashCommandOf(t *testing.T) {
	if got := bashCommandOf(askArgs); got != askCmd {
		t.Errorf("command = %q, want %q", got, askCmd)
	}
	// Junk, an empty object, and a missing field all mean "no command" — never a
	// remembered approval for something that was not the user's choice.
	for _, args := range []string{"", "not json", "{}", `{"other":"x"}`} {
		if got := bashCommandOf(args); got != "" {
			t.Errorf("bashCommandOf(%q) = %q, want empty", args, got)
		}
	}
}

func TestAnswerPermissionRefusesUnknownRequests(t *testing.T) {
	d, svc, a, _ := newAskTest(t)
	out := startAsk(t, d, a)

	if got := svc.AnswerPermission("s2", "call_1", permAllowOnce); got != "no such pending request" {
		t.Errorf("another session's tool id was accepted: %q", got)
	}
	if got := svc.AnswerPermission("s1", "call_9", permDeny); got != "no such pending request" {
		t.Errorf("an unknown tool id was accepted: %q", got)
	}
	if got := svc.AnswerPermission("s1", "call_1", "yes please"); got != "unknown decision" {
		t.Errorf("a bogus decision was accepted: %q", got)
	}
	// Still parked: no answer has been delivered, so the agent is still waiting.
	select {
	case o := <-out:
		t.Fatalf("ask finished without a valid answer: %+v", o)
	default:
	}
	if got := svc.AnswerPermission("s1", "call_1", permAllowOnce); got != "ok" {
		t.Fatalf("valid answer refused: %q", got)
	}
	if o := awaitAsk(t, out); !o.approved || o.err != nil {
		t.Fatalf("allow_once = %+v, want approved", o)
	}
}

func TestAskPermissionAllowOnceDoesNotRemember(t *testing.T) {
	d, svc, a, p := newAskTest(t)
	out := startAsk(t, d, a)

	if got := svc.AnswerPermission("s1", "call_1", permAllowOnce); got != "ok" {
		t.Fatalf("answer refused: %q", got)
	}
	if o := awaitAsk(t, out); !o.approved || o.err != nil {
		t.Fatalf("allow_once = %+v, want approved", o)
	}
	// 一次性就是一次性: the same command must ask again next time.
	if dec, _ := p.CheckBash(askCmd); dec != permission.DecisionAsk {
		t.Errorf("allow_once remembered the command: decision = %v", dec)
	}
}

// 「本会话全部允许」 stops the ASKING, not just this one call: the answer flips the
// switch on the session's own policy, and the agent consults that BEFORE reaching
// this handler — so the next ask rule, for a DIFFERENT command (the case that
// matters: an agent's commands do not repeat verbatim), is simply allowed.
//
// The end-to-end half of that (a second command that shows no card) is the
// `perm-session` smoke scenario; it needs the real loop, since with the switch on
// the agent never calls this handler at all.
func TestAskPermissionAllowSessionFlipsTheSessionsPolicy(t *testing.T) {
	d, svc, a, p := newAskTest(t)
	out := startAsk(t, d, a)

	if got := svc.AnswerPermission("s1", "call_1", permAllowSession); got != "ok" {
		t.Fatalf("answer refused: %q", got)
	}
	if o := awaitAsk(t, out); !o.approved || o.err != nil {
		t.Fatalf("allow_session = %+v, want approved", o)
	}
	// A command this session never saw, matching the same ask rule: allowed too.
	if dec, _ := p.CheckBash("git push origin dev"); dec != permission.DecisionAllow {
		t.Errorf("the session switch was not flipped: decision = %v", dec)
	}
	// And the GATES' half of the same answer (PermState.AllowAllAsks). One answer, two readers: the
	// gates ask a different question than the rules do and run after every rule has spoken, so a
	// switch only the policy holds is exactly the button that kept asking about commands no rule
	// covered (TestSessionAllowAllReachesTheJudgmentGate in the agent package pins that half).
	if !a.PermState.AllowAllAsks {
		t.Error("「本会话全部允许」 did not reach the judgment gates' half of the session state")
	}
	// Nothing parked: the wait ended with the answer, not with a leftover row.
	d.mu.Lock()
	n := len(d.asks)
	d.mu.Unlock()
	if n != 0 {
		t.Errorf("%d pending ask(s) left behind", n)
	}
}

// The switch is the SESSION's, i.e. the agent's own policy — a second conversation
// must still be asked (each desktop session builds its own agent).
func TestAskPermissionAllowSessionIsScopedToItsSession(t *testing.T) {
	d, svc, a1, p1 := newAskTest(t)
	other := &agent.AIAgent{}
	otherPolicy := permission.NewPolicy(permission.Rules{Ask: []string{"git push*"}}, permission.Rules{})
	other.SetPermissionPolicy(otherPolicy)

	out := startAsk(t, d, a1)
	if got := svc.AnswerPermission("s1", "call_1", permAllowSession); got != "ok" {
		t.Fatalf("answer refused: %q", got)
	}
	awaitAsk(t, out)
	if dec, _ := p1.CheckBash(askCmd); dec != permission.DecisionAllow {
		t.Fatalf("first session's switch was not flipped: %v", dec)
	}
	if !a1.PermState.AllowAllAsks {
		t.Fatal("first session's gates half was not flipped")
	}

	// Another session's policy is untouched, so its ask still parks here.
	otherOut := make(chan askOutcome, 1)
	go func() {
		ok, err := d.askPermission(context.Background(), "s2", other, "Bash", "call_9", askPrev, askArgs)
		otherOut <- askOutcome{approved: ok, err: err}
	}()
	waitAskRegistered(t, d, "s2", "call_9", true)
	if dec, _ := otherPolicy.CheckBash(askCmd); dec != permission.DecisionAsk {
		t.Errorf("another session's policy was affected: %v", dec)
	}
	if got := svc.AnswerPermission("s2", "call_9", permDeny); got != "ok" {
		t.Fatalf("answer refused: %q", got)
	}
	if o := awaitAsk(t, otherOut); o.approved {
		t.Fatalf("the other session's ask = %+v, want denied", o)
	}
}

func TestAskPermissionDenyLeavesThePolicyAlone(t *testing.T) {
	d, svc, a, p := newAskTest(t)
	out := startAsk(t, d, a)

	if got := svc.AnswerPermission("s1", "call_1", permDeny); got != "ok" {
		t.Fatalf("answer refused: %q", got)
	}
	if o := awaitAsk(t, out); o.approved || o.err != nil {
		t.Fatalf("deny = %+v, want not approved and no error", o)
	}
	if dec, _ := p.CheckBash(askCmd); dec != permission.DecisionAsk {
		t.Errorf("deny changed the policy: decision = %v", dec)
	}
}

func TestAskPermissionStoppedTurnReleasesTheWait(t *testing.T) {
	d, _, a, _ := newAskTest(t)

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan askOutcome, 1)
	go func() {
		ok, err := d.askPermission(ctx, "s1", a, "Bash", "call_1", askPrev, askArgs)
		out <- askOutcome{approved: ok, err: err}
	}()
	waitAskRegistered(t, d, "s1", "call_1", true)

	// Stop cancels the turn's context; the wait must end (a parked turn that
	// ignores cancellation would hang the Stop button forever).
	cancel()
	o := awaitAsk(t, out)
	if o.approved || o.err == nil {
		t.Fatalf("cancelled ask = %+v, want a refusal carrying the error", o)
	}
	// And the entry is gone, so a late click cannot approve a command that is no
	// longer running.
	waitAskRegistered(t, d, "s1", "call_1", false)
}

func TestAnsweredAskIsDropped(t *testing.T) {
	d, svc, a, _ := newAskTest(t)
	out := startAsk(t, d, a)

	if got := svc.AnswerPermission("s1", "call_1", permAllowOnce); got != "ok" {
		t.Fatalf("answer refused: %q", got)
	}
	awaitAsk(t, out)
	waitAskRegistered(t, d, "s1", "call_1", false)
	d.mu.Lock()
	n := len(d.asks)
	d.mu.Unlock()
	if n != 0 {
		t.Errorf("%d pending ask(s) left behind", n)
	}
}

// A side-channel run (/review, /commit) has no turn on screen, so it must NOT park: the
// refusal has to come back immediately, with nothing registered for a card that no one can
// answer (the run would otherwise hang until Stop).
func TestAskPermissionInSideChannelRunRefusesInsteadOfParking(t *testing.T) {
	d, _, a, p := newAskTest(t)

	done := make(chan askOutcome, 1)
	go func() {
		ok, err := d.askPermission(withoutAsk(context.Background()), "s1", a, "Bash", "call_1", askPrev, askArgs)
		done <- askOutcome{approved: ok, err: err}
	}()
	o := awaitAsk(t, done) // returns at once: if it parked, this would time out
	if o.approved || o.err == nil {
		t.Fatalf("side-channel ask = %+v, want a refusal carrying the reason", o)
	}
	d.mu.Lock()
	n := len(d.asks)
	d.mu.Unlock()
	if n != 0 {
		t.Errorf("a side-channel ask registered %d pending request(s)", n)
	}
	// The policy was not touched either: nothing was approved.
	if dec, _ := p.CheckBash(askCmd); dec != permission.DecisionAsk {
		t.Errorf("side-channel refusal changed the policy: decision = %v", dec)
	}
}

// ...and the mark belongs to ONE context: a conversation turn's ctx (or a plain
// background one) is still askable. Without this the guard could silently disable the whole
// feature, and every other test here would still pass.
func TestAskPermissionIsAskableWithoutTheMark(t *testing.T) {
	d, svc, a, _ := newAskTest(t)
	out := startAsk(t, d, a) // startAsk passes a plain context
	if got := svc.AnswerPermission("s1", "call_1", permAllowOnce); got != "ok" {
		t.Fatalf("a plain context was treated as un-askable: %q", got)
	}
	if o := awaitAsk(t, out); !o.approved || o.err != nil {
		t.Fatalf("plain-context ask = %+v, want approved", o)
	}
}

// The event the card is built from must survive JSON to the webview with the
// fields the frontend matches on — sessionId/toolId are the pair that addresses
// the answer, kind is what picks the card's copy, and a renamed field would
// silently break one of the two.
func TestPermissionEventJSONShape(t *testing.T) {
	b, err := json.Marshal(PermissionEvent{SessionID: "s1", ToolID: "c1", ToolName: "Bash", Kind: kindBash, Preview: askPrev})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"sessionId", "toolId", "toolName", "kind", "preview"} {
		if _, ok := got[k]; !ok {
			t.Errorf("event is missing %q: %s", k, b)
		}
	}
}

// The card's kind comes from the TOOL, which is the same identity the agent's
// gate uses to decide whether it is asking about a command or about a spend.
func TestPermEventKind(t *testing.T) {
	if got := permEventKind(tools.ToolNameMCPSearchTools); got != kindMCPSearch {
		t.Errorf("search tool kind = %q, want %q", got, kindMCPSearch)
	}
	if got := permEventKind("Bash"); got != kindBash {
		t.Errorf("other tools keep the bash card: got %q", got)
	}
}

// 「本会话全部允许」 on a search cost card has to reach the GATE, not only the bash
// policy: the gate reads a bool from this handler (its own question is money, not
// rules), so a card that flipped only the policy would ask again on the next
// search.
func TestAskPermissionSessionAllowFlipsTheSearchSwitch(t *testing.T) {
	d := newDesktopApp()
	a := newPermissionTestAgent(t)

	out := make(chan askOutcome, 1)
	go func() {
		ok, err := d.askPermission(context.Background(), "s1", a, tools.ToolNameMCPSearchTools, "call_1", askPrev, askArgs)
		out <- askOutcome{approved: ok, err: err}
	}()
	waitAskRegistered(t, d, "s1", "call_1", true)

	if got := (&AgentService{desk: d}).AnswerPermission("s1", "call_1", permAllowSession); got != "ok" {
		t.Fatalf("AnswerPermission = %q", got)
	}
	if o := awaitAsk(t, out); !o.approved || o.err != nil {
		t.Fatalf("session-allowed ask = %+v, want approved", o)
	}
	if !a.PermState.MCPSearchAllowed {
		t.Error("「本会话全部允许」 did not stop the MCP search cost card")
	}
}

// A bash ask must NOT flip that switch: the two are different questions, and
// approving a command says nothing about accepting a cache re-read.
func TestAskPermissionBashSessionAllowLeavesTheSearchSwitchAlone(t *testing.T) {
	d := newDesktopApp()
	a := newPermissionTestAgent(t)

	out := make(chan askOutcome, 1)
	go func() {
		ok, err := d.askPermission(context.Background(), "s1", a, "Bash", "call_1", askPrev, askArgs)
		out <- askOutcome{approved: ok, err: err}
	}()
	waitAskRegistered(t, d, "s1", "call_1", true)
	(&AgentService{desk: d}).AnswerPermission("s1", "call_1", permAllowSession)
	awaitAsk(t, out)

	if a.PermState.MCPSearchAllowed {
		t.Error("a bash approval must not silence the MCP search cost card")
	}
}

func TestPermBody(t *testing.T) {
	if got := permBody(askCmd); got != notifyPermWaiting+"："+askCmd {
		t.Errorf("permBody = %q", got)
	}
	if got := permBody("   "); got != notifyPermWaiting {
		t.Errorf("empty command should fall back to the bare phrase: %q", got)
	}
	long := permBody("echo " + strings.Repeat("x", 200))
	// truncateRunes cuts to max runes and adds one for the ellipsis.
	if got := len([]rune(long)); got > len([]rune(notifyPermWaiting))+1+notifyPermBodyMaxRune+1 {
		t.Errorf("body is not bounded: %d runes", got)
	}
}

// The permission NOTIFICATION names what is being asked, and a file ask has no
// command in it: the line is the target path. Without that branch every file ask
// popped a notification with nothing in it (permBody's bare waiting phrase).
func TestAskSubjectOf(t *testing.T) {
	if got := askSubjectOf("Bash", `{"command":"git push --force"}`); got != "git push --force" {
		t.Errorf("bash subject = %q", got)
	}
	for _, tool := range []string{"ReadFile", "WriteFile", "EditFile"} {
		if got := askSubjectOf(tool, `{"path":"/etc/hosts"}`); got != "/etc/hosts" {
			t.Errorf("%s subject = %q, want the path", tool, got)
		}
	}
	// Junk and the wrong shape both mean "no subject": permBody then falls back to
	// the bare phrase rather than showing a field that is not there.
	for _, tc := range []struct{ tool, args string }{
		{"ReadFile", ""}, {"ReadFile", "not json"}, {"ReadFile", `{"command":"x"}`},
		{"Bash", `{"path":"/etc/hosts"}`}, {"MCPSearchTools", `{"query":"x"}`},
	} {
		if got := askSubjectOf(tc.tool, tc.args); got != "" {
			t.Errorf("askSubjectOf(%q, %q) = %q, want empty", tc.tool, tc.args, got)
		}
	}
}
