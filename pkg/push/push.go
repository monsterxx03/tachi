// Package push sends one short notification to a phone through a relay app.
//
// It is deliberately not a client for our own app: iOS push needs a paid developer
// certificate and Android needs each vendor's channel, so the notification is handed to an
// app that already solved that (Bark on iOS, ntfy on Android). The relay's address is
// configuration, so a self-hosted server and a public one are the same code path.
//
// Nothing here decides WHEN to notify — that rule belongs to the caller (the host's
// notifier), which is the only place that knows whether somebody is already looking at the
// conversation.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds one delivery. A relay that does not answer must not hold a goroutine
// forever; the caller is fire-and-forget anyway, so a timeout is only about leaking.
const DefaultTimeout = 8 * time.Second

// Message is one notification, already reduced to what a lock screen can show: a title and a
// single line. It never carries the model's reply — the app is one tap away, and a wall of
// text is not a glance.
type Message struct {
	// Title is the bold line: usually "Tachi".
	Title string
	// Subtitle names the CONVERSATION. With several sessions running, that is the one fact
	// the phone screen does not already say.
	Subtitle string
	// Body is the sentence about what happened ("回合完成", "等待你的授权：rm -rf …").
	Body string
	// Group keeps one conversation's notifications together in the phone's list.
	Group string
	// Click is the deep link a tap opens: which machine, which conversation.
	Click string
}

// Sender is one relay. Send must return only transport and status errors: an empty
// configuration is "nothing to do", not a failure.
type Sender interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// Multi fans one message out to every configured relay.
//
// A failing sender does not stop the others: iOS and Android are separate deliveries, and
// one relay being down must not mean the other phone stays silent.
type Multi []Sender

func (m Multi) Send(ctx context.Context, msg Message) error {
	var errs []error
	for _, s := range m {
		if err := s.Send(ctx, msg); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// Bark delivers through the Bark app for iOS (https://bark.day.app).
//
// One request carries every key, so all of the account's iPhones ring together. The server
// (official or self-hosted) turns the request into an APNs push; the phone never has to be
// able to reach the relay, which is what makes this the cheap half of the pair.
type Bark struct {
	// BaseURL is the bark-server address. Empty disables this sender.
	BaseURL string
	// Keys are per-device keys. Empty disables this sender.
	Keys []string
	// HTTP is for tests; nil means the package default.
	HTTP *http.Client
}

func (b *Bark) Name() string { return "bark" }

func (b *Bark) Send(ctx context.Context, m Message) error {
	if b == nil || strings.TrimSpace(b.BaseURL) == "" || len(b.Keys) == 0 {
		return nil
	}
	payload := map[string]any{
		"title":       m.Title,
		"body":        m.Body,
		"device_keys": b.Keys,
		// "active" makes it show while the phone is unlocked, which is exactly when a
		// background turn finishing is news.
		"level": "active",
	}
	if m.Subtitle != "" {
		payload["subtitle"] = m.Subtitle
	}
	if m.Group != "" {
		payload["group"] = m.Group
	}
	if m.Click != "" {
		payload["url"] = m.Click
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(b.BaseURL, "/") + "/push"
	return postJSON(ctx, b.client(), endpoint, "", body)
}

func (b *Bark) client() *http.Client { return clientOf(b.HTTP) }

// Ntfy delivers through ntfy (https://ntfy.sh). Its Android app keeps a connection to the
// server; its iOS app goes through ntfy.sh's own APNs relay. One topic reaches every phone
// subscribed to it, which is how two devices get the same notification.
//
// JSON publishing is used rather than headers: the messages are Chinese, and an HTTP header
// may not carry UTF-8 without RFC 2047 encoding.
type Ntfy struct {
	BaseURL string
	Topic   string
	// Token is optional (a server with authentication turned on).
	Token string
	HTTP  *http.Client
}

func (n *Ntfy) Name() string { return "ntfy" }

func (n *Ntfy) Send(ctx context.Context, m Message) error {
	if n == nil || strings.TrimSpace(n.BaseURL) == "" || strings.TrimSpace(n.Topic) == "" {
		return nil
	}
	payload := map[string]any{
		"topic":   n.Topic,
		"title":   m.Title,
		"message": m.Body,
	}
	if m.Group != "" {
		payload["tags"] = []string{m.Group}
	}
	if m.Click != "" {
		payload["click"] = m.Click
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(n.BaseURL, "/") + "/"
	return postJSON(ctx, clientOf(n.HTTP), endpoint, n.Token, body)
}

// postJSON is the one HTTP shape both relays accept: a POST of JSON, answered with any 2xx.
// The body of an error is read (bounded) because a server's own sentence — a bad key, a
// disabled topic — is what makes a silent phone diagnosable.
func postJSON(ctx context.Context, c *http.Client, endpoint, token string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	text := strings.TrimSpace(string(detail))
	if text == "" {
		return fmt.Errorf("%s: HTTP %d", endpoint, resp.StatusCode)
	}
	return fmt.Errorf("%s: HTTP %d: %s", endpoint, resp.StatusCode, text)
}

func clientOf(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: DefaultTimeout}
}

// DeepLink builds the URL a tapped notification opens: the conversation on the machine that
// sent it. The device is named because a phone may remember several computers, and opening
// the same session id on the wrong one is worse than not opening anything.
//
// The format is tachi://session/<id>?device=<id>, handled by the mobile app's scheme (a
// custom scheme needs no certificate and no store listing).
func DeepLink(sessionID, deviceID string) string {
	if sessionID == "" {
		return ""
	}
	l := "tachi://session/" + url.PathEscape(sessionID)
	if deviceID != "" {
		l += "?device=" + url.QueryEscape(deviceID)
	}
	return l
}
