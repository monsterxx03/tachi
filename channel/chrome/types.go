// Package chrome implements the channel.Channel interface for Chrome
// Extension Native Messaging. It reads/writes JSON messages prefixed
// by a 4-byte little-endian length header on stdin/stdout.
package chrome

// ChromeRequest is a message sent from the Chrome extension to Tachi.
type ChromeRequest struct {
	ID       string `json:"id"`
	Action   string `json:"action"`
	ThreadID string `json:"threadID"`
	Selection struct {
		Text  string `json:"text"`
		URL   string `json:"url,omitempty"`
		Title string `json:"title,omitempty"`
	} `json:"selection"`
	Content string `json:"content,omitempty"`
}

// ChromeResponse is a message sent from Tachi back to the Chrome extension.
type ChromeResponse struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "result", "error", "stream"
	ThreadID string `json:"threadID"`
	Content  string `json:"content"`
	Done     bool   `json:"done,omitempty"`
}
