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
	"gopkg.in/yaml.v3"
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
		b, err := yaml.Marshal(rawCfg)
		if err != nil {
			return nil, fmt.Errorf("chrome: marshal config: %w", err)
		}
		var cfg struct {
			Enabled     bool   `yaml:"enabled"`
			ExtensionID string `yaml:"extension_id"`
		}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("chrome: unmarshal config: %w", err)
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
	case "search":
		return fmt.Sprintf("搜索以下内容并返回结果：%s", req.Selection.Text)

	case "explain":
		return fmt.Sprintf(
			"请解释以下概念。先用 100 字以内给出核心定义，再用 2-3 个要点展开。"+
				"最后给一个生活中的类比。\n\n概念：%s",
			req.Selection.Text,
		)

	case "remember":
		url := req.Selection.URL
		if url == "" {
			url = "(未知来源)"
		}
		return fmt.Sprintf(
			"使用 RecordMemory 工具记录以下内容到记忆中。\n\n内容：%s\n来源：%s",
			req.Selection.Text, url,
		)

	case "recall":
		return fmt.Sprintf(
			"使用 MemoryRecall 工具搜索记忆中是否有与以下内容相关的信息。\n\n查询：%s",
			req.Selection.Text,
		)

	case "ask_tachi":
		title := req.Selection.Title
		if title == "" {
			title = req.Selection.URL
		}
		return fmt.Sprintf(
			"用户从浏览器中提问。\n\n当前页面：%s\n选中文本：%s\n\n用户问题：%s",
			title, req.Selection.Text, req.Content,
		)

	default:
		return req.Selection.Text
	}
}

// readMessage reads a complete Native Messaging message from the reader.
// Format: [4-byte little-endian uint32 length][JSON bytes].
func (c *ChromeChannel) readMessage() (ChromeRequest, error) {
	var length uint32
	if err := binary.Read(c.reader, binary.LittleEndian, &length); err != nil {
		return ChromeRequest{}, err
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(c.reader, data); err != nil {
		return ChromeRequest{}, err
	}

	var req ChromeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return ChromeRequest{}, fmt.Errorf("unmarshal ChromeRequest: %w", err)
	}
	return req, nil
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
