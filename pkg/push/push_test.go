package push

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBarkSendsOneBatchToEveryKey(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := &Bark{BaseURL: srv.URL + "/", Keys: []string{"k1", "k2"}, HTTP: srv.Client()}
	err := b.Send(context.Background(), Message{
		Title: "Tachi", Subtitle: "修推送", Body: "回合完成", Group: "s1", Click: "tachi://session/s1?device=d1",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if path != "/push" {
		t.Errorf("path = %q, want /push", path)
	}
	keys, _ := got["device_keys"].([]any)
	if len(keys) != 2 || keys[0] != "k1" || keys[1] != "k2" {
		t.Errorf("device_keys = %v, want both keys in one request", got["device_keys"])
	}
	if got["subtitle"] != "修推送" || got["body"] != "回合完成" {
		t.Errorf("payload = %v", got)
	}
	if got["url"] != "tachi://session/s1?device=d1" {
		t.Errorf("url = %v, want the deep link", got["url"])
	}
}

func TestNtfyPostsJSONWithToken(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/" {
			t.Errorf("path = %q, want / (JSON publishing names the topic in the body)", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Ntfy{BaseURL: srv.URL, Topic: "tachi-abc", Token: "tk_x", HTTP: srv.Client()}
	if err := n.Send(context.Background(), Message{Title: "Tachi", Body: "回合完成", Click: "tachi://session/s1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if auth != "Bearer tk_x" {
		t.Errorf("Authorization = %q", auth)
	}
	if got["topic"] != "tachi-abc" || got["message"] != "回合完成" || got["click"] != "tachi://session/s1" {
		t.Errorf("payload = %v", got)
	}
}

func TestEmptyConfigurationIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	if err := (&Bark{}).Send(ctx, Message{Body: "x"}); err != nil {
		t.Errorf("empty bark: %v", err)
	}
	if err := (&Ntfy{BaseURL: "https://ntfy.sh"}).Send(ctx, Message{Body: "x"}); err != nil {
		t.Errorf("ntfy without a topic: %v", err)
	}
}

func TestMultiKeepsGoingAfterOneRelayFails(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer bad.Close()
	delivered := false
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered = true
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	m := Multi{
		&Bark{BaseURL: bad.URL, Keys: []string{"k"}, HTTP: bad.Client()},
		&Ntfy{BaseURL: good.URL, Topic: "t", HTTP: good.Client()},
	}
	if err := m.Send(context.Background(), Message{Body: "x"}); err == nil {
		t.Fatal("expected the failing relay to be reported")
	}
	if !delivered {
		t.Error("the second relay was skipped after the first failed")
	}
}

func TestDeepLinkNamesTheMachine(t *testing.T) {
	if got := DeepLink("s 1", "d/1"); got != "tachi://session/s%201?device=d%2F1" {
		t.Errorf("DeepLink = %q", got)
	}
	if got := DeepLink("", "d1"); got != "" {
		t.Errorf("no session must mean no link, got %q", got)
	}
}
