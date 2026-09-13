package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/monsterxx03/tachi/agent"
	"github.com/monsterxx03/tachi/agent/permission"
)

// askCmd + askArgs are one Bash call the way the agent sees it: the tool args as
// JSON (what the notification names) and the preview the agent renders for the card
// (what the user reads).
const (
	askCmd  = "git push origin main"
	askArgs = `{"command":"git push origin main"}`
	askPrev = "$ git push origin main\n\nmatched ask rule: \"git push*\""
)

// newAskTest builds the pieces one parked ask needs: the app (a bare one — with no
// *application.App the event emit is skipped, which is exactly what lets this run
// without a window server) and an agent carrying a policy whose only rule is an
// `ask` on git push.
func newAskTest(t *testing.T) (*desktopApp, *AgentService, *agent.AIAgent, *permission.Policy) {
	t.Helper()
	d := newDesktopApp()
	p := permission.NewPolicy(permission.Rules{Ask: []string{"git push*"}}, permission.Rules{})
	// A zero-value agent is enough: the ask path only reaches Config through
	// PermissionPolicy. No provider, no Configure, no network.
	a := &agent.AIAgent{}
	a.SetPermissionPolicy(p)
	return d, &AgentService{desk: d}, a, p
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
		_, ok := d.perms[key]
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
	// Nothing parked: the wait ended with the answer, not with a leftover row.
	d.mu.Lock()
	n := len(d.perms)
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
	n := len(d.perms)
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
	n := len(d.perms)
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
// the answer, and a renamed field would silently break the answer path.
func TestPermissionEventJSONShape(t *testing.T) {
	b, err := json.Marshal(PermissionEvent{SessionID: "s1", ToolID: "c1", ToolName: "Bash", Preview: askPrev})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"sessionId", "toolId", "toolName", "preview"} {
		if _, ok := got[k]; !ok {
			t.Errorf("event is missing %q: %s", k, b)
		}
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
