package session

import (
	"encoding/json"
	"time"
)

type Session struct {
	ID           string `json:"id"`
	ThreadID     string `json:"thread_id,omitempty"` // channel ThreadID for session lookup
	Title        string `json:"title"`
	ProviderName string `json:"provider_name,omitempty"` // config provider name (e.g. "deepseek-v4-flash"); empty = default provider
	WorkingDir   string `json:"working_dir,omitempty"`   // working directory at session creation time
	// AdditionalDirs are extra workspace roots (absolute paths, ordered): fully
	// writable, but relative paths NEVER resolve against them — the model is told
	// to use absolute paths, mirroring ACP's additionalDirectories and Claude
	// Code's --add-dir. The effective root set is [WorkingDir, ...AdditionalDirs].
	// Empty for sessions that never added one, so old session files read back with
	// identical behavior.
	AdditionalDirs []string `json:"additional_dirs,omitempty"`
	// ProjectID binds this session to a desktop project (docs/2026-09-14-desktop-project-design.md):
	// when set and the project exists, the PROJECT's roots win and WorkingDir/AdditionalDirs
	// above hold only the snapshot taken when the session was created. Desktop-only — no other
	// entry point resolves it, they read the snapshot as the session's own roots. Empty for every
	// session created outside a project, so old meta.json files read back identically.
	ProjectID string    `json:"project_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Pinned marks a session the reader keeps at hand. It is a property of the RECORD
	// rather than of the reader's window, because it has to mean the same thing to every
	// process that touches the list:
	//   - the desktop sidebar sorts a pinned session to the top of ITS OWN group — a
	//     project's members, or the project-less bucket. The pin never crosses a group
	//     boundary: a pinned member of a project does not jump above another project;
	//   - session cleanup SKIPS it: a pin is exactly the promise that its session will
	//     still be there tomorrow, and a retention sweep that deletes it is a broken
	//     promise, not a smaller list;
	//   - compaction TRANSFERS it to the child the conversation continues in (see
	//     agent/compact.go's FinalizeCompact), because the child is the row a reader sees.
	// Empty for every session that was never pinned, so old meta.json files read back
	// identically.
	Pinned    bool `json:"pinned,omitempty"`
	SkipDream bool `json:"skip_dream,omitempty"` // exclude this session from Dream memory consolidation
	// CronBound marks a conversation a desktop scheduled task writes into.
	//
	// It is a CLEANUP hint, not the badge's source of truth: the desktop derives
	// the number from crons.json, because that is the only place a job exists.
	// This flag is what makes count-based cleanup work in EVERY entry point — a
	// TUI or ACP process sweeping the same data dir has no business reading the
	// cron store, but it can see a session record. The desktop writes it
	// (syncCronProtection) on job create/delete/rebind and at startup, and
	// compaction moves it to the child the conversation continues in.
	//
	// A stale true only keeps a session longer; a stale false could let the
	// target of a job be cleaned. Startup reconciliation is what closes the
	// second window, and a job whose target is gone is paused rather than
	// silently re-created.
	CronBound bool `json:"cron_bound,omitempty"`
	// SkipApproval is this session's answer to 「bash 与文件工具要不要先问你」: the desktop's
	// composer dropdown writes it, and the desktop reads it back (desktop/permission.go) every
	// time it builds an agent for the session. false — the asking mode — is what every session
	// has by default and what an old meta.json reads back as.
	//
	// A record rather than reader state, because the answer has to mean the same thing to every
	// PROCESS that opens the session: the desktop restarting must not forget that this
	// conversation was set to not ask.
	//
	// The field is desktop-only in the same way ProjectID is — no other entry point reads it,
	// and the TUI (which has no such control, and its own prompt for the same questions) builds
	// its agent from its own configuration. So a session left in this mode asks again under the
	// TUI; that is an asymmetry of the frontends, not a promise this field makes.
	//
	// It travels with a FORK (the same standing choice about the same conversation as SkipDream)
	// and deliberately NOT into a compaction's child: a compacted conversation is a different
	// one, and nobody was asked whether it wants the asks off.
	SkipApproval bool `json:"skip_approval,omitempty"`

	// Kind marks a session that a SUBSYSTEM owns rather than the person at the keyboard:
	// today only a receiving node's A2A execution (KindA2AExecution) — design §2.2. Empty
	// is an ordinary conversation, which is what every session written before this field
	// existed reads back as.
	//
	// It is a classification, NOT an authorization: what a kind permits is decided where
	// the session is used (see agent.WithTrustedExecution). What a remote session BELONGS
	// TO has two halves, and they are stored differently on purpose: the node that asked is
	// on the record (SourceNodeID below), while which context and workspace the conversation
	// is lives in the node's own binding record (node/context.go).
	Kind string `json:"kind,omitempty"`

	// SourceNodeID is WHICH node the work in this session came from: the caller's node id on
	// the receiving side of a delegation (design §2.2). It comes out of the key in the
	// certificate that caller presented, so it cannot be claimed — a caller can generate as
	// many keys as it likes, but it cannot present one that derives somebody else's id
	// (node/auth.go).
	//
	// A NAME is deliberately not stored here, and the name a caller gives itself is never
	// used as one: any key can claim any name. Reading it as a name means looking this id up
	// in the machine's own peer directory, and an id that is not in it is reported as the
	// unknown node it is.
	//
	// It is a copy of a fact the node also holds in its context binding, and the copy is the
	// point rather than the drift that arrangement normally risks:
	//
	//   - a reader labels a ROW, and a row is a session. The binding FOLLOWS the
	//     conversation — a compaction re-points it at the successor session — so a session
	//     that has since been compacted (still listed, still readable) would have nothing
	//     left to look up;
	//   - resolving it where it is shown would put the same inference in every frontend.
	//
	// What it cannot drift from is the caller itself: a context belongs to the caller that
	// opened it and the runner refuses another one, so this value is written once and is
	// never rewritten.
	//
	// Empty for an ordinary conversation, for a session written before this field existed,
	// and on a surface that identifies nobody (loopback).
	SourceNodeID string `json:"source_node_id,omitempty"`

	// ThinkingLevel is the per-session thinking override, set via /thinking.
	// One of "", "none", "low", "medium", "high", "xhigh", "max".
	// Empty = use the provider/model default from config. Only affects
	// this session — other sessions keep their own setting (or the default).
	ThinkingLevel string `json:"thinking_level,omitempty"`

	// Compact-related fields: link to child/parent sessions after /compact.
	CompactedChildID     string `json:"compacted_child_id,omitempty"`
	CompactedParentID    string `json:"compacted_parent_id,omitempty"`
	CompactedParentTitle string `json:"compacted_parent_title,omitempty"`

	// LogicalID identifies the CONVERSATION this session belongs to, across compaction.
	//
	// A session's own id cannot be that identity: compaction moves the conversation to a
	// successor session, so anything filed under the id — a delegated task's record, a
	// reminder's idea of "which conversation is this" — would lose the conversation at the
	// first compaction, exactly when a long conversation has the most to remember.
	//
	// Empty means "this session is its own conversation" (see LogicalConversationID), so every
	// session written before this field existed reads back correctly, and only a compaction has
	// to carry a value to the child.
	LogicalID string `json:"logical_id,omitempty"`
}

// LogicalConversationID is the identity to file anything conversation-scoped under.
//
// The fallback is the point: a session that has never been compacted IS its own conversation,
// and writing its id into the field would only create two spellings of the same fact.
func (s *Session) LogicalConversationID() string {
	if s == nil {
		return ""
	}
	if s.LogicalID != "" {
		return s.LogicalID
	}
	return s.ID
}

// KindA2AExecution marks a session created to hold a remote conversation for an A2A
// context (design §2.2). Managed() is written as "any kind at all" so a future kind cannot
// be drivable from a frontend merely because this package had not learned its name.
const KindA2AExecution = "a2a_execution"

// Managed reports whether this session belongs to a subsystem that owns its turns.
func (s *Session) Managed() bool { return s != nil && s.Kind != "" }

type MessageType string

const (
	MessageTypeUser       MessageType = "user"
	MessageTypeAssistant  MessageType = "assistant"
	MessageTypeThinking   MessageType = "thinking"
	MessageTypeToolCall   MessageType = "tool_call"
	MessageTypeToolResult MessageType = "tool_result"
	MessageTypeConfirm    MessageType = "confirm"
	MessageTypeReminder   MessageType = "reminder"
)

// PartType names the pieces a multi-modal message can be built from.
type PartType string

const (
	PartTypeText  PartType = "text"
	PartTypeImage PartType = "image"
)

// Part is one piece of a multi-modal message, kept so a reloaded history can rebuild the very
// request it was sent in. That verbatim-ness is the point: the provider caches by prefix, so an
// image part that disappears on a reload (a restart, `--resume`, a session switch) misses from
// that image to the end of the conversation, and the model also stops being able to see an
// image it was in fact given.
//
// The struct has two shapes. On the way INTO the store an image part carries its bytes in Data;
// the store moves them into the session's images/ directory and fills Ref (see FileStore.
// AppendMessage, the only writer). On the way OUT of it, a part carries only Ref. Data is never
// serialized — a record holding a megabyte of base64 is exactly what this design avoids.
type Part struct {
	Type PartType `json:"type"`
	// Text is a text part's text, verbatim as sent. Stored rather than re-derived from the
	// message's Content on reload: the parts array is the thing that went on the wire, and a
	// rebuild that re-derives one of its pieces is a second source of truth that can drift.
	Text      string `json:"text,omitempty"`
	MediaType string `json:"media_type,omitempty"` // image parts: "image/png", …
	// Ref names an image blob under the session's images/ directory: the SHA-256 of the
	// decoded bytes, so identical images are stored once however many times they are sent.
	Ref string `json:"ref,omitempty"`
	// Data is base64 image data on the way in, empty on the way out. Deliberately not
	// persisted, and deliberately not a second read path: a record that reaches the disk
	// through a writer other than FileStore (the subagent and one-off transcripts) cannot
	// carry bytes, so it drops such parts rather than claiming an image it cannot produce.
	Data string `json:"-"`
}

// Usage records token usage from a single LLM API response.
type Usage struct {
	InputTokens              int64 `json:"input_tokens,omitempty"`
	OutputTokens             int64 `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	EstimatedInputTokens     int64 `json:"estimated_input_tokens,omitempty"` // chars/4 local estimate — matches what statusbar showed during conversation
}

// Message represents a message in the session history.
type Message struct {
	Type       MessageType `json:"type"`
	Content    string      `json:"content,omitempty"`
	Name       string      `json:"name,omitempty"`
	Signature  string      `json:"signature,omitempty"`
	Args       any         `json:"args,omitempty"`
	Result     string      `json:"result,omitempty"`
	IsError    bool        `json:"is_error,omitempty"`
	Diff       string      `json:"diff,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	SubagentID string      `json:"subagent_id,omitempty"` // shortID for SubAgent tool_result → subagent/<id>.jsonl
	Usage      *Usage      `json:"usage,omitempty"`       // token usage from the LLM response that produced this message
	// DurationMs is the wall-clock execution duration of a tool call, recorded
	// on the matching tool_result message. 0 = not a completed tool result.
	DurationMs int64 `json:"duration_ms,omitempty"`
	Iteration  int   `json:"iteration,omitempty"` // 1-based LLM API call that produced this message (0 = not request-bound)
	// Seq is the session-wide request sequence number: monotonic across
	// turns (unlike Iteration, which resets to 1 per turn). API-request-bound
	// messages carry the same Seq as their api_requests.jsonl record, giving
	// consumers a stable cross-turn link. 0 = not request-bound (user /
	// reminder) or legacy data written before Seq existed.
	Seq       int       `json:"seq,omitempty"`
	Timestamp time.Time `json:"timestamp"`

	// Parts is the multi-modal content this message was SENT with — present only when there
	// was an image in it: text parts verbatim, image parts as a reference to a blob under the
	// session's images/ directory. Empty for every text-only message (nearly all of them), so
	// those records read back exactly as before, and empty for records written before images
	// were persisted at all — which is why a conversion must keep working with no parts.
	//
	// It is the persisted mirror of llm.Message.ContentParts, and it exists for one reason:
	// rebuilding a history from the store must reproduce the request byte for byte.
	Parts []Part `json:"parts,omitempty"`

	// DisplayContent is the user's OWN text for a `user` record, kept only when it differs
	// from Content. Today exactly one thing makes them differ: @-file expansion
	// (agent/atfile), which is done by the FRONTEND before it calls the agent — Content is
	// what the model received (the file inlined between UNTRUSTED FILE CONTENT markers),
	// while this is what the user typed (`@path`), which is what a transcript has to show.
	//
	// It is DISPLAY ONLY, and the direction matters: Content stays the authority for the
	// conversation, because ConvertSessionToLLMMessages rebuilds the next request from it —
	// recording the raw text instead would send a different prefix than the one the model
	// actually saw, invalidating the provider's prompt cache and retroactively rewriting
	// what the history says it was told.
	DisplayContent string `json:"display_content,omitempty"`
}

// APITool is a tool definition as sent to the LLM in one API request.
// Parameters carries the full JSON schema (llm.ToolParameters) so transcript
// viewers can inspect exactly what the model was offered.
type APITool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// APIRequest captures one LLM API call's request payload — the system prompt,
// tool schemas, and the user prompt that triggered it. Stored in
// api_requests.jsonl, one line per request, and consumed by the /transcript
// report to show what the agent saw on each call.
type APIRequest struct {
	Timestamp time.Time `json:"timestamp"`
	// Iteration is the 1-based API call sequence number within the turn that
	// made this request (it restarts at 1 for every fresh user prompt).
	// tool_call / tool_result messages carry the same number, linking each
	// tool execution to the request that produced it.
	Iteration int `json:"iteration,omitempty"`
	// Seq is the session-wide request sequence number: monotonic across
	// turns. Request-bound session messages (thinking / assistant /
	// tool_call / tool_result) carry the same Seq, so consumers can link a
	// request record to its messages even when Iteration repeats across
	// turns. 0 = legacy record written before Seq existed.
	Seq int `json:"seq,omitempty"`
	// SystemPrompt is the system message content sent with this request.
	SystemPrompt string `json:"system_prompt"`
	// UserPrompt is the latest user (or steer) message content in the request
	// — the input this call was answering.
	UserPrompt string `json:"user_prompt,omitempty"`
	// Model is the model name this request was sent to (e.g. "deepseek-v4-flash").
	Model string `json:"model,omitempty"`
	// Provider is the config provider name backing the model ("" when unknown).
	Provider string `json:"provider,omitempty"`
	// Thinking captures the thinking mode actually used for this request:
	// "none" (disabled), a reasoning effort ("low"/"high"/...), or "" for the
	// provider default.
	Thinking string    `json:"thinking,omitempty"`
	Tools    []APITool `json:"tools,omitempty"`
	// DurationMs is the wall-clock duration of the LLM API call, from request
	// start until the stream completes. 0 = not yet measured (request failed
	// before a stream, or legacy record written before duration tracking).
	DurationMs int64 `json:"duration_ms,omitempty"`
	// Cost is the precise CNY cost of this API call (¥), taken from the usage
	// ledger's conversation-kind row paired to this request. 0 = not resolved
	// (the recorder never sets it — it's filled only by the web layer for
	// display, so it never persists into the jsonl sidecar).
	Cost float64 `json:"cost,omitempty"`
	// Credit is the precise credit of this API call, taken from the same
	// ledger row as Cost (call-time snapshot; pre-upgrade rows recomputed
	// from the current configured rate). 0 = not resolved (display-only,
	// never persists — same rule as Cost).
	Credit float64 `json:"credit,omitempty"`
}
