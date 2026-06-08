package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// jsonrpcConn is a minimal JSON-RPC 2.0 client over stdio using the LSP
// wire format (Content-Length header framing). It handles concurrent
// Call/Notify and dispatches server-initiated requests/notifications.
type jsonrpcConn struct {
	reader   *bufio.Reader
	writer   io.WriteCloser
	closed   atomic.Bool
	writeMu  sync.Mutex

	// pending responses: request ID → response channel
	pending   map[string]chan<- jsonrpcMessage
	pendingMu sync.Mutex
	nextID    atomic.Int64

	// server-initiated request handlers
	handlers   map[string]rpcHandler
	handlersMu sync.RWMutex
}

type rpcHandler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// jsonrpcMessage is the wire format for JSON-RPC 2.0.
type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcErr     `json:"error,omitempty"`
}

type jsonrpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *jsonrpcErr) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// newRPCConn creates a JSON-RPC 2.0 connection over stdio and starts
// a background read loop.
func newRPCConn(stdout io.ReadCloser, stdin io.WriteCloser) *jsonrpcConn {
	c := &jsonrpcConn{
		reader:   bufio.NewReader(stdout),
		writer:   stdin,
		pending:  make(map[string]chan<- jsonrpcMessage),
		handlers: make(map[string]rpcHandler),
	}
	go c.readLoop()
	return c
}

// Call sends a request and blocks until the response arrives or ctx is cancelled.
func (c *jsonrpcConn) Call(ctx context.Context, method string, params, result any) error {
	id := fmt.Sprintf("lsp-%d", c.nextID.Add(1))

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
		rawParams = b
	}

	respCh := make(chan jsonrpcMessage, 1)
	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	msg := jsonrpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  rawParams,
	}
	if err := c.writeMsg(msg); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	select {
	case resp := <-respCh:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && resp.Result != nil {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Notify sends a fire-and-forget notification. Does not wait for a response.
func (c *jsonrpcConn) Notify(ctx context.Context, method string, params any) error {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
		rawParams = b
	}
	msg := jsonrpcMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	}
	return c.writeMsg(msg)
}

// RegisterHandler registers a handler for server-initiated requests.
func (c *jsonrpcConn) RegisterHandler(method string, handler rpcHandler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.handlers[method] = handler
}

// Close closes the writer stream. The read loop will notice on next error.
func (c *jsonrpcConn) Close() error {
	c.closed.Store(true)
	return c.writer.Close()
}

// writeMsg serializes msg as JSON and writes it with LSP Content-Length framing.
func (c *jsonrpcConn) writeMsg(msg jsonrpcMessage) error {
	if c.closed.Load() {
		return fmt.Errorf("connection closed")
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := c.writer.Write([]byte(header)); err != nil {
		return err
	}
	_, err = c.writer.Write(data)
	return err
}

// readLoop reads messages from the server stdout and dispatches them.
func (c *jsonrpcConn) readLoop() {
	for {
		msg, err := c.readMsg()
		if err != nil {
			if !c.closed.Load() {
				continue // transient error, keep reading
			}
			return
		}

		if msg.Method == "" {
			// Response to a previous Call (JSON-RPC 2.0 responses have no method).
			idStr := fmt.Sprintf("%v", msg.ID)
			c.pendingMu.Lock()
			ch, ok := c.pending[idStr]
			delete(c.pending, idStr)
			c.pendingMu.Unlock()
			if ok {
				select {
				case ch <- msg:
				default:
				}
			}
		} else {
			// Server-initiated request (with ID) or notification (without ID).
			hasID := msg.ID != nil

			c.handlersMu.RLock()
			handler, ok := c.handlers[msg.Method]
			c.handlersMu.RUnlock()

			if ok && handler != nil {
				if hasID {
					// Request — respond asynchronously.
					resp, err := handler(context.Background(), msg.Method, msg.Params)
					_ = c.writeMsg(c.makeResponse(msg.ID, resp, err))
				} else {
					// Notification — fire-and-forget handler.
					go handler(context.Background(), msg.Method, msg.Params)
				}
			}
		}
	}
}

func (c *jsonrpcConn) makeResponse(id any, result any, err error) jsonrpcMessage {
	if err != nil {
		return jsonrpcMessage{
			JSONRPC: "2.0",
			ID:      id,
			Error:   &jsonrpcErr{Code: -32603, Message: err.Error()},
		}
	}
	var raw json.RawMessage
	if result != nil {
		b, _ := json.Marshal(result)
		raw = b
	}
	return jsonrpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Result:  raw,
	}
}

// readMsg reads one JSON-RPC message using LSP Content-Length framing.
func (c *jsonrpcConn) readMsg() (jsonrpcMessage, error) {
	var msg jsonrpcMessage
	var contentLength int
	headersDone := false

	for !headersDone {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return msg, err
		}
		line = trimCRLF(line)

		if line == "" {
			headersDone = true
			break
		}
		if len(line) > 16 && line[:16] == "Content-Length: " {
			if _, err := fmt.Sscanf(line, "Content-Length: %d", &contentLength); err != nil {
				return msg, fmt.Errorf("parse Content-Length: %w", err)
			}
		}
	}

	if contentLength == 0 {
		return msg, fmt.Errorf("no Content-Length header")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(c.reader, body); err != nil {
		return msg, fmt.Errorf("read body: %w", err)
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return msg, fmt.Errorf("unmarshal: %w", err)
	}
	return msg, nil
}

// trimCRLF removes trailing \r and/or \n from s.
func trimCRLF(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\r' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}
