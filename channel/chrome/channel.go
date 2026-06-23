package chrome

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/debuglog"
)

func init() {
	channel.Register("chrome", func(rawCfg map[string]any) (channel.Channel, error) {
		// Chrome channel needs no special config — the Native Messaging
		// protocol is self-describing via stdin/stdout. The config is
		// validated for an "enabled" flag.
		if enabled, ok := rawCfg["enabled"]; ok {
			if b, ok := enabled.(bool); ok && !b {
				return nil, fmt.Errorf("chrome: disabled")
			}
		}
		return NewChromeChannel("chrome"), nil
	})
}

// ChromeChannel implements channel.Channel for Chrome Native Messaging.
//
// Communication uses stdin/stdout with the Native Messaging protocol:
//
//	[4-byte little-endian message length] [JSON message body]
//
// Chrome launches tachi with --chrome flag for each extension instance.
// The channel reads ChromeRequest messages from stdin, routes them through
// the agent via channel.MessageHandler, and writes ChromeResponse messages
// back to stdout.
type ChromeChannel struct {
	reader io.Reader
	writer io.Writer
	name   string

	logger *debuglog.Logger
}

// NewChromeChannel creates a ChromeChannel that reads from stdin and
// writes to stdout — the standard Native Messaging file descriptors.
func NewChromeChannel(name string) *ChromeChannel {
	return &ChromeChannel{
		reader: os.Stdin,
		writer: os.Stdout,
		name:   name,
		logger: debuglog.DefaultLogger.WithSource("channel:chrome"),
	}
}

// NewChromeChannelWithIO creates a ChromeChannel with custom IO, for testing.
func NewChromeChannelWithIO(name string, reader io.Reader, writer io.Writer) *ChromeChannel {
	return &ChromeChannel{
		reader: reader,
		writer: writer,
		name:   name,
		logger: debuglog.DefaultLogger.WithSource("channel:chrome"),
	}
}

// Name returns the channel type identifier.
func (c *ChromeChannel) Name() string { return c.name }

// OnStart implements channel.Channel. Chrome channel has no pre-start
// initialisation needed — the connection is already established when
// the process starts.
func (c *ChromeChannel) OnStart(_ context.Context) error {
	c.logger.Log("chrome: channel starting (pid=%d)", os.Getpid())
	return nil
}

// Run implements channel.Channel. It enters the Native Messaging read loop:
// for each incoming ChromeRequest, it calls the handler and writes the
// response back to stdout.
//
// The loop exits when:
//   - stdin is closed (Chrome terminated the native host)
//   - ctx is cancelled (graceful shutdown)
//   - an unrecoverable read error occurs
func (c *ChromeChannel) Run(ctx context.Context, handler channel.MessageHandler) error {
	c.logger.Log("chrome: entering message loop")

	for {
		select {
		case <-ctx.Done():
			c.logger.Log("chrome: context cancelled, shutting down")
			return nil
		default:
		}

		req, err := c.readMessage()
		if err != nil {
			if err == io.EOF {
				c.logger.Log("chrome: stdin closed (EOF), shutting down")
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			c.logger.Log("chrome: read error: %v", err)
			return fmt.Errorf("chrome read: %w", err)
		}

		// Handle ping — respond immediately without invoking the agent.
		if req.Action == "ping" {
			c.writeMessage(ChromeResponse{
				ID:       req.ID,
				Type:     "result",
				ThreadID: req.ThreadID,
				Content:  "pong",
			})
			continue
		}

		c.logger.Log("chrome: recv action=%s thread=%s id=%s", req.Action, req.ThreadID, req.ID)

		// Convert to IncomingMessage and pass to the handler.
		incoming := c.toIncoming(req)
		result := handler(ctx, incoming)

		if result.Err != nil {
			c.logger.Log("chrome: handler error: %v", result.Err)
			c.writeMessage(ChromeResponse{
				ID:       req.ID,
				Type:     "error",
				ThreadID: req.ThreadID,
				Content:  fmt.Sprintf("❌ %v", result.Err),
			})
			continue
		}

		if result.Steered {
			c.logger.Log("chrome: message steered (thread %s already active)", req.ThreadID)
			continue
		}

		// Send the reply back.
		c.writeMessage(ChromeResponse{
			ID:       req.ID,
			Type:     "result",
			ThreadID: req.ThreadID,
			Content:  result.Reply.Content,
		})
	}
}

// Send implements channel.MessageSender for proactive message delivery
// (used by cron job triggers, progress updates, etc.).
func (c *ChromeChannel) Send(_ context.Context, msg channel.OutgoingMessage) error {
	// Chrome Native Messaging is request-response per message.
	// Proactive sends don't have a matching request ID, so we use
	// an empty ID and set type to "result".
	c.logger.Log("chrome: proactive send thread=%s", msg.ThreadID)
	return c.writeMessage(ChromeResponse{
		ID:       "",
		Type:     "result",
		ThreadID: msg.ThreadID,
		Content:  msg.Content,
	})
}

// toIncoming converts a ChromeRequest to a channel.IncomingMessage,
// building the prompt based on the action type.
func (c *ChromeChannel) toIncoming(req ChromeRequest) channel.IncomingMessage {
	return channel.IncomingMessage{
		ThreadID:  req.ThreadID,
		MessageID: req.ID,
		Content:   c.buildPrompt(req),
	}
}

// buildPrompt constructs the LLM-friendly prompt string based on the
// action requested by the Chrome extension.
func (c *ChromeChannel) buildPrompt(req ChromeRequest) string {
	switch req.Action {
	case "summarize":
		// The extension pre-builds a structured summarization prompt
		// with page content, title, and URL. Pass it through as-is.
		return req.Content

	default:
		// Catch-all: return content if present, otherwise selection text.
		if req.Content != "" {
			return req.Content
		}
		return req.Selection.Text
	}
}

// readMessage reads a complete Native Messaging message from the reader.
// Format: [4-byte little-endian uint32 length][JSON bytes].
func (c *ChromeChannel) readMessage() (ChromeRequest, error) {
	var length uint32
	if err := binary.Read(c.reader, binary.LittleEndian, &length); err != nil {
		c.logger.Log("chrome: readMessage: binary.Read(length) failed: %v (type: %T)", err, err)
		return ChromeRequest{}, err
	}

	if length == 0 {
		c.logger.Log("chrome: readMessage: zero-length message; sending shutdown")
		return ChromeRequest{}, io.EOF
	}

	data := make([]byte, length)
	n, err := io.ReadFull(c.reader, data)
	if err != nil {
		if n > 0 {
			c.logger.Log("chrome: readMessage: io.ReadFull body failed after %d/%d bytes: %v (type: %T)", n, length, err, err)
		} else {
			c.logger.Log("chrome: readMessage: io.ReadFull body failed (0/%d bytes): %v (type: %T)", length, err, err)
		}
		return ChromeRequest{}, err
	}
	c.logger.Log("chrome: readMessage: read %d bytes OK", n)

	var req ChromeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		c.logger.Log("chrome: readMessage: json.Unmarshal failed: %v (type: %T); raw data (first 512 bytes): %q", err, err, truncateBytes(data, 512))
		return ChromeRequest{}, fmt.Errorf("unmarshal ChromeRequest: %w", err)
	}
	return req, nil
}

func truncateBytes(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + fmt.Sprintf("... (%d more bytes)", len(b)-max)
}

// writeMessage writes a ChromeResponse to the writer using the Native
// Messaging protocol format: [4-byte little-endian length][JSON bytes].
func (c *ChromeChannel) writeMessage(resp ChromeResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal ChromeResponse: %w", err)
	}

	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(len(data)))

	if _, err := c.writer.Write(header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := c.writer.Write(data); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	return nil
}

// compile-time interface checks
var _ channel.Channel = (*ChromeChannel)(nil)
var _ channel.MessageSender = (*ChromeChannel)(nil)
