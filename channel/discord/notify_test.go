package discord

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// roundTripFunc lets a test stand in for Discord's REST API without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// restStub answers the two calls a DM notification makes and records their paths.
type restStub struct {
	paths []string
	// failWith, when non-empty, is returned as a 403 body for every call.
	failWith string
}

func (s *restStub) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)

		status, body := http.StatusOK, "{}"
		switch {
		case s.failWith != "":
			status, body = http.StatusForbidden, `{"message":"`+s.failWith+`","code":50007}`
		case strings.HasSuffix(r.URL.Path, "/users/@me/channels"):
			body = `{"id":"dm-channel-1"}`
		case strings.Contains(r.URL.Path, "/messages"):
			body = `{"id":"msg-1"}`
		}

		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
}

// notifyingChannel builds a channel with a live session whose HTTP client is
// stubbed — the shape channel mode has after Run: session present, transport
// replaced by the test.
func notifyingChannel(t *testing.T, stub *restStub, cfg DiscordConfig) *DiscordChannel {
	t.Helper()
	sess, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	sess.Client = stub.client()

	ch := testChannel(cfg)
	ch.session = sess
	return ch
}

// The happy path: the DM channel is opened through the API, and the text goes to
// that channel — not to whatever thread the agent happens to be answering in.
func TestNotifyUserSendsADirectMessage(t *testing.T) {
	stub := &restStub{}
	ch := notifyingChannel(t, stub, DiscordConfig{
		Token:        "test-token",
		AllowedUsers: []string{"42"},
	})

	if err := ch.NotifyUser(context.Background(), "任务跑完了"); err != nil {
		t.Fatalf("NotifyUser: %v", err)
	}

	want := []string{"POST /api/v9/users/@me/channels", "POST /api/v9/channels/dm-channel-1/messages"}
	if len(stub.paths) != len(want) {
		t.Fatalf("call sequence = %v, want %v", stub.paths, want)
	}
	for i := range want {
		if stub.paths[i] != want[i] {
			t.Fatalf("call sequence = %v, want %v", stub.paths, want)
		}
	}
}

// A platform refusal is a failed call — never a quiet success.
func TestNotifyUserSurfacesAPlatformRefusal(t *testing.T) {
	stub := &restStub{failWith: "Cannot send messages to this user"}
	ch := notifyingChannel(t, stub, DiscordConfig{
		Token:        "test-token",
		AllowedUsers: []string{"42"},
	})

	err := ch.NotifyUser(context.Background(), "hi")
	if err == nil {
		t.Fatal("a refusal must surface as an error")
	}
	if !strings.Contains(err.Error(), "Cannot send messages to this user") {
		t.Errorf("error %q should carry the platform's reason", err.Error())
	}
}

// No recipient configured is a refusal BEFORE anything is attempted: guessing
// would deliver the user's notifications to a stranger.
func TestNotifyUserWithoutATargetDoesNotCallTheAPI(t *testing.T) {
	stub := &restStub{}
	ch := notifyingChannel(t, stub, DiscordConfig{Token: "test-token"})

	err := ch.NotifyUser(context.Background(), "hi")
	if err == nil {
		t.Fatal("no allowed_users means no target")
	}
	if !strings.Contains(err.Error(), "allowed_users") {
		t.Errorf("error %q should name the config key to fix", err.Error())
	}
	if len(stub.paths) != 0 {
		t.Fatalf("nothing should have been sent, got %v", stub.paths)
	}
}

func TestNotifyUserRejectsBlankText(t *testing.T) {
	stub := &restStub{}
	ch := notifyingChannel(t, stub, DiscordConfig{
		Token:        "test-token",
		AllowedUsers: []string{"42"},
	})

	if err := ch.NotifyUser(context.Background(), "   "); err == nil {
		t.Fatal("blank text must be refused")
	}
	if len(stub.paths) != 0 {
		t.Fatalf("nothing should have been sent, got %v", stub.paths)
	}
}

// A process that is NOT running the channel (tui/desktop) has no live session:
// the notifier builds a REST-only one — no Open(), which is what keeps it from
// holding a gateway connection or consuming events — and reuses it.
func TestNotifySessionIsBuiltWithoutAGatewayAndReused(t *testing.T) {
	ch := testChannel(DiscordConfig{Token: "test-token"})
	if ch.session != nil {
		t.Fatal("this case is about a channel with no live session")
	}

	first, err := ch.notifySession()
	if err != nil {
		t.Fatalf("notifySession: %v", err)
	}
	second, err := ch.notifySession()
	if err != nil {
		t.Fatalf("notifySession (second call): %v", err)
	}
	if first != second {
		t.Error("the send-only session should be prepared once and reused")
	}

	// A session without a token cannot notify; the error has to say so.
	noToken := testChannel(DiscordConfig{})
	if _, err := noToken.notifySession(); err == nil {
		t.Error("no token must be an error")
	}
}
