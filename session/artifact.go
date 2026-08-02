package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ArtifactRef identifies an isolated-workflow artifact (research, review,
// …) that the user can follow up on. Injected into session history as a
// MessageTypeReminder block; the LLM reads the artifact file on demand.
type ArtifactRef struct {
	Kind  string `json:"kind"`  // registered kind constant (see below)
	Title string `json:"title"` // one-line topic
	Path  string `json:"path"`  // artifact file or directory
}

// Registered artifact kinds. New workflows add a constant here (and a
// display name in artifactKindDisplay) — nothing else needs touching.
const (
	ArtifactKindResearch = "research"
	ArtifactKindReview   = "review"
)

// artifactKindDisplay maps kinds to Chinese labels in the reminder block.
var artifactKindDisplay = map[string]string{
	ArtifactKindResearch: "研究",
	ArtifactKindReview:   "审查",
}

// artifactsMarker prefixes a machine-readable JSON line inside the reminder
// block, so merging refs never has to parse the human-facing text.
const artifactsMarker = "ARTIFACTS:"

// FormatArtifactReminder renders refs into a MessageTypeReminder block: the
// human-facing lines tell the LLM when to read the artifacts, and the
// trailing ARTIFACTS line carries the refs in structured form for merging.
// Exported so frontends can render the same block into their in-memory
// history.
func FormatArtifactReminder(refs []ArtifactRef) string {
	var sb strings.Builder
	sb.WriteString("<system-reminder>\n")
	sb.WriteString("近期产物（仅当用户主动就该产物追问时，才读取对应文件）：\n")
	for _, r := range refs {
		label := artifactKindDisplay[r.Kind]
		if label == "" {
			label = r.Kind
		}
		sb.WriteString("- [" + label + "] 主题：" + r.Title + " · 产物：" + r.Path + "\n")
	}
	sb.WriteString("若用户问题与上述产物无关，忽略本条。\n")
	if data, err := json.Marshal(refs); err == nil {
		sb.WriteString(artifactsMarker + " " + string(data) + "\n")
	}
	sb.WriteString("</system-reminder>")
	return sb.String()
}

// maxArtifactRefs caps refs per reminder block; newest kept, oldest dropped.
const maxArtifactRefs = 5

// parseArtifactRefs extracts refs from a reminder block's structured line.
// Returns nil for non-artifact blocks (project context, git status…) and for
// empty lists — an empty list must not trigger a merge, or a forged
// "ARTIFACTS: []" line would rebuild the block and drop existing refs.
func parseArtifactRefs(content string) []ArtifactRef {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, artifactsMarker) {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, artifactsMarker))
		var refs []ArtifactRef
		if err := json.Unmarshal([]byte(payload), &refs); err == nil && len(refs) > 0 {
			return refs
		}
	}
	return nil
}

// AppendArtifactTo appends (or merges) an artifact ref into the given
// session's history. Consecutive artifacts merge into one reminder block
// instead of piling up one message per artifact; non-artifact reminder
// blocks (project context etc.) are never overwritten.
func (m *Manager) AppendArtifactTo(sessionID string, ref ArtifactRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appendArtifactLocked(sessionID, ref)
}

// AppendArtifact appends (or merges) an artifact ref into the current
// session's history. See AppendArtifactTo.
func (m *Manager) AppendArtifact(ref ArtifactRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return fmt.Errorf("no active session")
	}
	return m.appendArtifactLocked(m.current.ID, ref)
}

// appendArtifactLocked is the shared implementation; caller must hold m.mu.
func (m *Manager) appendArtifactLocked(sessionID string, ref ArtifactRef) error {
	// Reject empty fields and flatten newlines in the title — a title with
	// embedded newlines could forge an ARTIFACTS: line (or close the
	// reminder block) and corrupt the structured merge data.
	if ref.Kind == "" || ref.Title == "" || ref.Path == "" {
		return fmt.Errorf("artifact ref fields must be non-empty")
	}
	ref.Title = strings.ReplaceAll(ref.Title, "\n", " ")

	msgs, err := m.store.LoadMessages(sessionID)
	if err != nil {
		return fmt.Errorf("load messages: %w", err)
	}

	// Merge into an existing artifact reminder when it is the last message.
	if n := len(msgs); n > 0 && msgs[n-1].Type == MessageTypeReminder {
		if existing := parseArtifactRefs(msgs[n-1].Content); existing != nil {
			refs := append(existing, ref)
			if len(refs) > maxArtifactRefs {
				refs = refs[len(refs)-maxArtifactRefs:]
			}
			last := &msgs[n-1]
			last.Content = FormatArtifactReminder(refs)
			last.Timestamp = time.Now()
			if err := m.store.ReplaceLastMessage(sessionID, last); err != nil {
				return fmt.Errorf("replace artifact reminder: %w", err)
			}
			m.touchSessionMetaLocked(sessionID)
			return nil
		}
	}

	msg := &Message{
		Type:      MessageTypeReminder,
		Content:   FormatArtifactReminder([]ArtifactRef{ref}),
		Timestamp: time.Now(),
	}
	if err := m.store.AppendMessage(sessionID, msg); err != nil {
		return fmt.Errorf("append artifact reminder: %w", err)
	}
	m.touchSessionMetaLocked(sessionID)
	return nil
}

// touchSessionMetaLocked refreshes UpdatedAt so artifact registration keeps
// the session's last-activity semantics aligned with AppendMessage.
// Best-effort: failures are logged, not returned. Caller must hold m.mu.
func (m *Manager) touchSessionMetaLocked(sessionID string) {
	sess, err := m.store.LoadMeta(sessionID)
	if err != nil || sess == nil {
		return
	}
	sess.UpdatedAt = time.Now()
	if err := m.store.UpdateMeta(sess); err != nil {
		if m.logger != nil {
			m.logger.Warn(context.Background(), "session: artifact meta touch failed", "err", err)
		}
	}
}
