package weixin

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/monsterxx03/tachi/pkg/channel"
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
			ch.logger.Log("weixin: polling loop exiting (ctx cancelled)")
			return nil
		default:
		}

		resp, err := ch.cli.getUpdates(buf)
		if err != nil {
			ch.logger.Log("weixin: getUpdates error: %v", err)
			failures++
			if failures >= maxConsecutiveFailures {
				ch.logger.Log("weixin: %d consecutive failures, backing off for %v", failures, longBackoff)
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
			ch.logger.Log("weixin: session expired, pausing for %v", sessionExpiredPause)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(sessionExpiredPause):
			}
			failures = 0
			continue
		}

		if resp.Ret != 0 {
			ch.logger.Log("weixin: getUpdates ret=%d, errcode=%d", resp.Ret, resp.ErrCode)
			failures++
			if failures >= maxConsecutiveFailures {
				ch.logger.Log("weixin: %d consecutive non-zero ret, backing off for %v", failures, longBackoff)
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
	mediaRefs := extractMediaItems(msg.ItemList)

	// Download and decrypt any files / images attached to this message.
	var attachments []channel.Attachment
	if len(mediaRefs) > 0 {
		attachments = ch.processMedia(mediaRefs, msg.FromUserID)
		ch.logger.Log("weixin: processed %d media items for msg %d -> %d attachments",
			len(mediaRefs), msg.MessageID, len(attachments))
	}

	// If we have a context_token, store it.
	if msg.ContextToken != "" && msg.FromUserID != "" {
		ch.store.saveContextToken(ch.accountID, msg.FromUserID, msg.ContextToken)
	}

	// Check allowlist.
	if !ch.store.isUserAllowed(ch.accountID, msg.FromUserID) {
		ch.logger.Log("weixin: user %s not in allowlist, ignoring", msg.FromUserID)
		return
	}

	// Build the incoming channel message.
	threadID := ch.accountID + ":" + msg.FromUserID
	messageID := fmt.Sprintf("%d", msg.MessageID)
	if msg.ClientID != "" {
		messageID = msg.ClientID
	}

	inMsg := channel.IncomingMessage{
		ThreadID:    threadID,
		MessageID:   messageID,
		Content:     text,
		ChannelID:   msg.GroupID,
		Attachments: attachments,
	}

	ch.logger.Log("weixin: dispatching msg from %s (thread=%s): %s", msg.FromUserID, threadID, truncate(text, 100))

	// Start typing indicator while LLM processes.
	typingDone := make(chan struct{})
	go ch.runTyping(ctx, msg.FromUserID, typingDone)

	// Dispatch to agent handler.
	result := handler(ctx, inMsg)

	// Stop typing.
	close(typingDone)

	if result.Steered {
		// Message was injected into an already-running agent turn via steer.
		// The original conversation will produce the final reply — nothing to
		// send here.
		ch.logger.Log("weixin: msg steered for thread=%s", threadID)
		return
	}

	if result.Err != nil {
		ch.logger.Log("weixin: handler error for %s: %v", threadID, result.Err)
		// Send error message back to user.
		errorText := fmt.Sprintf("❌ %v", result.Err)
		ch.sendTextReply(msg.FromUserID, msg.ContextToken, errorText)
		return
	}

	if result.Reply.Content == "" {
		return
	}

	// Send the reply.
	ch.sendTextReply(msg.FromUserID, msg.ContextToken, result.Reply.Content)
}

// --- Typing Indicator ---

// runTyping sends typing status every 5 seconds until done is closed.
// Must be called in its own goroutine.
func (ch *Channel) runTyping(ctx context.Context, userID string, done <-chan struct{}) {
	ticket, err := ch.typingTickets.get(userID, "")
	if err != nil {
		ch.logger.Log("weixin: typing ticket fetch failed for %s: %v", userID, err)
		return
	}

	// Send initial typing immediately.
	ch.sendTyping(userID, ticket, TypingStatusTyping)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ch.sendTyping(userID, ticket, TypingStatusTyping)
		case <-done:
			ch.sendTyping(userID, ticket, TypingStatusCancel)
			return
		case <-ctx.Done():
			ch.sendTyping(userID, ticket, TypingStatusCancel)
			return
		}
	}
}

func (ch *Channel) sendTyping(userID, ticket string, status int) {
	req := &SendTypingRequest{
		ILinkUserID:  userID,
		TypingTicket: ticket,
		Status:       status,
		BaseInfo:     BaseInfo{ChannelVersion: defaultChannelVersion},
	}
	if err := ch.cli.sendTyping(req); err != nil {
		ch.logger.Log("weixin: sendTyping error: %v", err)
	}
}

// --- Typing Ticket Cache ---

type typingTicketCache struct {
	mu        sync.Mutex
	ticket    string
	expiresAt time.Time
	client    *client
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
