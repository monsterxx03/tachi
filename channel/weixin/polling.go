package weixin

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/channel"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

// --- Polling Loop ---

const (
	maxConsecutiveFailures = 3
	sessionExpiredPause    = 1 * time.Hour
	shortBackoff           = 2 * time.Second
	longBackoff            = 30 * time.Second
)

// pollingLoop is the main long-polling loop that receives and dispatches
// messages. It runs until ctx is cancelled or an unrecoverable error occurs.
func (ch *Channel) pollingLoop(ctx context.Context, handler channel.MessageHandler) error {
	buf := ch.store.loadSyncBuf(ch.accountID)
	nextTimeout := ch.cli.getUpdatesTimeout

	failures := 0

	for {
		select {
		case <-ctx.Done():
			debuglog.Log("weixin: polling loop exiting (ctx cancelled)")
			return nil
		default:
		}

		resp, err := ch.cli.getUpdates(buf)
		if err != nil {
			debuglog.Log("weixin: getUpdates error: %v", err)
			failures++
			if failures >= maxConsecutiveFailures {
				debuglog.Log("weixin: %d consecutive failures, backing off for %v", failures, longBackoff)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(longBackoff):
				}
				failures = 0
			} else {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(shortBackoff):
				}
			}
			continue
		}

		// Update polling timeout if server indicates one.
		if resp.LongpollingTimeoutMs > 0 {
			nextTimeout = time.Duration(resp.LongpollingTimeoutMs) * time.Millisecond
			ch.cli.getUpdatesTimeout = nextTimeout
		}

		// Handle session expiry.
		if resp.ErrCode == ErrCodeSessionExpired {
			debuglog.Log("weixin: session expired, pausing for %v", sessionExpiredPause)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(sessionExpiredPause):
			}
			failures = 0
			continue
		}

		if resp.Ret != 0 {
			debuglog.Log("weixin: getUpdates ret=%d, errcode=%d", resp.Ret, resp.ErrCode)
			failures++
			if failures >= maxConsecutiveFailures {
				debuglog.Log("weixin: %d consecutive non-zero ret, backing off for %v", failures, longBackoff)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(longBackoff):
				}
				failures = 0
			}
			continue
		}

		failures = 0

		// Persist sync buf.
		if resp.GetUpdatesBuf != "" {
			ch.store.saveSyncBuf(ch.accountID, resp.GetUpdatesBuf)
			buf = resp.GetUpdatesBuf
		}

		// Process messages.
		for _, msg := range resp.Msgs {
			ch.processMessage(ctx, msg, handler)
		}
	}
}

// processMessage handles a single incoming message.
func (ch *Channel) processMessage(ctx context.Context, msg WeixinMessage, handler channel.MessageHandler) {
	// Only process user messages (not bot messages).
	if msg.MessageType != MessageTypeUser {
		return
	}

	// Extract text content and media references.
	text, _ := extractMessageText(msg.ItemList)

	// If we have a context_token, store it.
	if msg.ContextToken != "" && msg.FromUserID != "" {
		ch.store.saveContextToken(ch.accountID, msg.FromUserID, msg.ContextToken)
	}

	// Check allowlist.
	if !ch.store.isUserAllowed(ch.accountID, msg.FromUserID) {
		debuglog.Log("weixin: user %s not in allowlist, ignoring", msg.FromUserID)
		return
	}

	// Build the incoming channel message.
	threadID := ch.accountID + ":" + msg.FromUserID
	messageID := fmt.Sprintf("%d", msg.MessageID)
	if msg.ClientID != "" {
		messageID = msg.ClientID
	}

	inMsg := channel.IncomingMessage{
		ThreadID:  threadID,
		MessageID: messageID,
		Content:   text,
		ChannelID: msg.GroupID,
	}

	debuglog.Log("weixin: dispatching msg from %s (thread=%s): %s", msg.FromUserID, threadID, truncate(text, 100))

	// Dispatch to agent handler.
	outMsg, err := handler(ctx, inMsg)
	if err != nil {
		debuglog.Log("weixin: handler error for %s: %v", threadID, err)
		// Send error message back to user.
		errorText := fmt.Sprintf("❌ %v", err)
		ch.sendTextReply(msg.FromUserID, msg.ContextToken, errorText)
		return
	}

	if outMsg.Content == "" {
		return
	}

	// Send the reply.
	ch.sendTextReply(msg.FromUserID, msg.ContextToken, outMsg.Content)
}

// --- Typing Indicator ---

// typingTicker manages the periodic "typing" status update to WeChat.
type typingTicker struct {
	mu       sync.Mutex
	stopCh   chan struct{}
	stopped  bool
	client   *client
	userID   string
	ticket   string
}

// newTypingTicker creates a typing indicator ticker.
func (ch *Channel) newTypingTicker(userID string, ticket string) *typingTicker {
	return &typingTicker{
		stopCh: make(chan struct{}),
		client: ch.cli,
		userID: userID,
		ticket: ticket,
	}
}

// start begins sending typing status every 5 seconds.
func (t *typingTicker) start() {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		// Send immediately.
		t.send(TypingStatusTyping)

		for {
			select {
			case <-ticker.C:
				t.send(TypingStatusTyping)
			case <-t.stopCh:
				t.send(TypingStatusCancel)
				return
			}
		}
	}()
}

// stop stops the typing indicator.
func (t *typingTicker) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.stopped {
		t.stopped = true
		close(t.stopCh)
	}
}

func (t *typingTicker) send(status int) {
	req := &SendTypingRequest{
		ILinkUserID:  t.userID,
		TypingTicket: t.ticket,
		Status:       status,
		BaseInfo:     BaseInfo{ChannelVersion: defaultChannelVersion},
	}
	if err := t.client.sendTyping(req); err != nil {
		debuglog.Log("weixin: sendTyping error: %v", err)
	}
}

// --- Typing Ticket Cache ---

type typingTicketCache struct {
	mu         sync.Mutex
	ticket     string
	expiresAt  time.Time
	client     *client
}

const typingTicketTTL = 24 * time.Hour

func newTypingTicketCache(cli *client) *typingTicketCache {
	return &typingTicketCache{client: cli}
}

func (tc *typingTicketCache) get(userID string, contextToken string) (string, error) {
	tc.mu.Lock()
	if tc.ticket != "" && time.Now().Before(tc.expiresAt) {
		ticket := tc.ticket
		tc.mu.Unlock()
		return ticket, nil
	}
	tc.mu.Unlock()

	// Fetch a new ticket.
	req := &GetConfigRequest{
		ILinkUserID:  userID,
		ContextToken: contextToken,
		BaseInfo:     BaseInfo{ChannelVersion: defaultChannelVersion},
	}
	resp, err := tc.client.getConfig(req)
	if err != nil {
		return "", err
	}

	if resp.TypingTicket == "" {
		return "", fmt.Errorf("no typing_ticket in getConfig response (ret=%d, errmsg=%s)", resp.Ret, resp.ErrMsg)
	}

	tc.mu.Lock()
	tc.ticket = resp.TypingTicket
	tc.expiresAt = time.Now().Add(typingTicketTTL)
	tc.mu.Unlock()

	return resp.TypingTicket, nil
}
