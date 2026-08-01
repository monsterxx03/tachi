package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type loc struct {
	URI   string `json:"uri"`
	Range rng    `json:"range"`
}

type rng struct {
	Start pos `json:"start"`
	End   pos `json:"end"`
}

type pos struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

func writeMsg(w io.Writer, m any) {
	data, _ := json.Marshal(m)
	h := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	w.Write([]byte(h))
	w.Write(data)
}

func main() {
	reader := bufio.NewReader(os.Stdin)
	for {
		var contentLength int
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if strings.HasPrefix(line, "Content-Length: ") {
				fmt.Sscanf(line, "Content-Length: %d", &contentLength)
			}
		}
		if contentLength == 0 {
			continue
		}
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(reader, body); err != nil {
			return
		}
		var req msg
		json.Unmarshal(body, &req)

		switch req.Method {
		case "initialize":
			capResult := map[string]any{
				"capabilities": map[string]any{
					"textDocumentSync":   1,
					"definitionProvider": true,
					"referencesProvider": true,
					"hoverProvider":      true,
				},
			}
			writeMsg(os.Stdout, map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  capResult,
			})
		case "textDocument/definition":
			writeMsg(os.Stdout, map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  []loc{{URI: "file:///test/foo.go", Range: rng{Start: pos{Line: 10, Character: 5}, End: pos{Line: 10, Character: 15}}}},
			})
		case "textDocument/hover":
			writeMsg(os.Stdout, map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"contents": map[string]string{"kind": "markdown", "value": "**func** Foo(bar string) string"},
					"range":    map[string]any{"start": map[string]uint32{"line": 10, "character": 5}, "end": map[string]uint32{"line": 10, "character": 15}},
				},
			})
		case "textDocument/references":
			writeMsg(os.Stdout, map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  []loc{},
			})
		case "textDocument/didOpen", "textDocument/didChange":
			// notification, no response
		case "shutdown":
			writeMsg(os.Stdout, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": nil})
		case "exit":
			return
		default:
			writeMsg(os.Stdout, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": nil})
		}
	}
}
