package weixin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/monsterxx03/tachi/config"
)

// notifyStub records what the send-only path actually put on the wire.
type notifyStub struct {
	mu       sync.Mutex
	requests []stubRequest
}

type stubRequest struct {
	path   string
	auth   string
	toUser string
	text   string
	token  string
}

func (s *notifyStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			Msg struct {
				ToUserID     string `json:"to_user_id"`
				ContextToken string `json:"context_token"`
				ItemList     []struct {
					TextItem *struct {
						Text string `json:"text"`
					} `json:"text_item"`
				} `json:"item_list"`
			} `json:"msg"`
		}
		_ = json.Unmarshal(body, &msg)

		text := ""
		if len(msg.Msg.ItemList) > 0 && msg.Msg.ItemList[0].TextItem != nil {
			text = msg.Msg.ItemList[0].TextItem.Text
		}

		s.mu.Lock()
		s.requests = append(s.requests, stubRequest{
			path:   r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			toUser: msg.Msg.ToUserID,
			text:   text,
			token:  msg.Msg.ContextToken,
		})
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
	}
}

func (s *notifyStub) all() []stubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubRequest(nil), s.requests...)
}

// A notification is a send-only operation: it must not fall into the login flow,
// because there is nobody at a terminal to scan a QR code.
func TestNotifyUserWithoutStoredLoginRefuses(t *testing.T) {
	ch, err := NewChannel(config.WeixinConfig{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}

	err = ch.NotifyUser(context.Background(), "hi")
	if err == nil {
		t.Fatal("a machine with no stored login must refuse, not start a QR login")
	}
	if !strings.Contains(err.Error(), "no stored login") {
		t.Errorf("error %q should say what is missing", err.Error())
	}
}

// The whole send path, against a stand-in for the API: it goes to the logged-in
// user, carries the stored bearer token, and touches nothing but the send
// endpoint (no getUpdates, no notifyStart — those belong to the receive session).
func TestNotifyUserSendsToTheLoggedInUser(t *testing.T) {
	stub := &notifyStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	ch, err := NewChannel(config.WeixinConfig{StateDir: t.TempDir(), RouteTag: "tag-1"})
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}
	// What a real login leaves behind on disk.
	const accountID = "acct@im.bot"
	if err := ch.store.saveAccount(accountID, &AccountData{
		Token:   "bot-token",
		BaseURL: srv.URL,
		UserID:  "owner-1",
	}); err != nil {
		t.Fatalf("saveAccount: %v", err)
	}
	ch.store.saveContextToken(accountID, "owner-1", "ctx-token")

	if err := ch.NotifyUser(context.Background(), "任务跑完了"); err != nil {
		t.Fatalf("NotifyUser: %v", err)
	}

	got := stub.all()
	if len(got) != 1 {
		t.Fatalf("expected exactly one request (the send), got %d: %+v", len(got), got)
	}
	req := got[0]
	if req.path != "/ilink/bot/sendmessage" {
		t.Errorf("path = %q, want the send endpoint", req.path)
	}
	if req.toUser != "owner-1" {
		t.Errorf("to_user_id = %q, want the logged-in user", req.toUser)
	}
	if req.auth != "Bearer bot-token" {
		t.Errorf("Authorization = %q, want the stored token", req.auth)
	}
	if req.token != "ctx-token" {
		t.Errorf("context_token = %q, want the stored one", req.token)
	}
	if req.text != "任务跑完了" {
		t.Errorf("text = %q", req.text)
	}
}

// The platform refusing the send is a failed call, not a quiet success.
func TestNotifyUserSurfacesAPlatformRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":0,"errcode":40001,"errmsg":"invalid token"}`))
	}))
	defer srv.Close()

	ch, err := NewChannel(config.WeixinConfig{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}
	if err := ch.store.saveAccount("acct@im.bot", &AccountData{
		Token: "stale", BaseURL: srv.URL, UserID: "owner-1",
	}); err != nil {
		t.Fatalf("saveAccount: %v", err)
	}

	err = ch.NotifyUser(context.Background(), "hi")
	if err == nil {
		t.Fatal("a platform refusal must surface as an error")
	}
	if !strings.Contains(err.Error(), "invalid token") {
		t.Errorf("error %q should carry the platform's reason", err.Error())
	}
}
