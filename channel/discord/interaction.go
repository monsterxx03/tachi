package discord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/monsterxx03/tachi/pkg/channel"
)

// Interactive AskUserQuestion support.
//
// The manager layer detects interactive channels via the
// channel.InteractiveChannel interface (Interactive() + PresentQuestions)
// and keeps the AskUserQuestion tool registered for their threads. When the
// LLM calls AskUserQuestion, the manager delivers the questions through
// PresentQuestions and then blocks waiting for an answer routed back via a
// regular IncomingMessage carrying AskUserAnswers.
//
// This file implements that contract for Discord:
//
//   - PresentQuestions renders the questions as a Discord message: each
//     multiple-choice question becomes a row of buttons (single-select with
//     ≤5 options) or a select menu (multi-select / more options); questions
//     without options tell the user to reply with text.
//   - Button/menu clicks arrive as INTERACTION_CREATE (message component)
//     events in a discordgo callback goroutine, while the agent turn blocks
//     in the manager. The click handler builds an IncomingMessage with
//     AskUserAnswers and delegates to the same message handler the manager
//     passed to Run(), which routes the answers back to the waiting agent
//     (it returns Steered=true and never produces a reply to send).
//   - The question message is edited after an answer is recorded to disable
//     the components and show what was chosen, preventing double answers.
//
// The CustomID namespace is "tachi:ask:<token>:q<idx>:o<optIdx>" (buttons)
// and "tachi:ask:<token>:q<idx>" (select menues). <token> is unique per
// PresentQuestions call and is the key into the pending-questions registry.

const (
	// askCustomIDPrefix is the shared prefix for every AskUserQuestion
	// component CustomID. Kept namespaced to avoid colliding with any
	// future component features (design doc §8.1).
	askCustomIDPrefix = "tachi:ask:"

	// askStateTTL is how long a pending question stays answerable.
	// Discord interaction tokens expire ~15 minutes after the message is
	// sent; after that button clicks fail client-side anyway, and typing a
	// text reply remains the fallback (manager routes raw text as an answer
	// to the first question). This TTL only guards the registry.
	askStateTTL = 15 * time.Minute

	// maxActionRows is Discord's limit of action rows per message.
	maxActionRows = 5
	// maxButtonsPerRow is Discord's limit of buttons per action row.
	maxButtonsPerRow = 5
	// maxButtonLabelRunes is Discord's button label limit.
	maxButtonLabelRunes = 80
	// maxSelectOptionDescRunes is Discord's select option description limit.
	maxSelectOptionDescRunes = 100
)

// questionState tracks a delivered AskUserQuestion prompt so component
// interactions can be routed back to the waiting agent turn.
type questionState struct {
	token     string // unique token embedded in CustomIDs
	threadID  string // manager thread ID the answers must be routed to
	channelID string // Discord channel the question message lives in
	messageID string // Discord message ID of the question message
	questions []channel.Question
	answered  bool // already answered (clicked) — reject further clicks
	created   time.Time
}

// Interactive implements channel.InteractiveChannel.
func (ch *DiscordChannel) Interactive() bool { return true }

// Compile-time assertion that DiscordChannel satisfies the interactive
// channel contract expected by the manager (keeps AskUserQuestion
// registered for Discord threads and routes answers back).
var _ channel.InteractiveChannel = (*DiscordChannel)(nil)

// PresentQuestions implements channel.InteractiveChannel. It renders the
// agent's questions as a Discord message with buttons / select menus and
// registers a pending state so subsequent component interactions can be
// routed back to the waiting agent turn. Returns nil once the message is
// sent; the actual answer arrives asynchronously via a component
// interaction, which re-enters through the normal message handler.
func (ch *DiscordChannel) PresentQuestions(ctx context.Context, threadID, replyID string, questions []channel.Question) error {
	sess := ch.session
	if sess == nil {
		return fmt.Errorf("discord: session not initialized")
	}
	channelID := channelIDFromThreadID(threadID)
	if channelID == "" {
		return fmt.Errorf("discord: invalid threadID %q", threadID)
	}

	token := newQuestionToken()
	st := &questionState{
		token:     token,
		threadID:  threadID,
		channelID: channelID,
		questions: questions,
		created:   time.Now(),
	}
	ch.registerQuestionState(st)

	content, components := buildQuestionMessage(token, questions)
	sent, err := sess.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:    content,
		Components: components,
	})
	if err != nil {
		ch.unregisterQuestionState(token)
		return fmt.Errorf("discord: send question message: %w", err)
	}

	st.messageID = sent.ID
	ch.logger.Info(ctx, "discord: AskUser questions presented", "thread", threadID,
		"count", len(questions), "channel", channelID, "message", sent.ID)
	return nil
}

// buildQuestionMessage renders questions into a Discord message: a text
// prompt plus one action row per multiple-choice question (buttons for
// single-select with ≤5 options, a select menu otherwise). Questions
// without options get no components — the user replies with plain text.
func buildQuestionMessage(token string, questions []channel.Question) (string, []discordgo.MessageComponent) {
	var b strings.Builder
	b.WriteString("❓ **Tachi 需要你确认几个问题**\n")

	var rows []discordgo.MessageComponent
	for i, q := range questions {
		if i > 0 {
			b.WriteString("\n")
		}
		// Render the question line.
		header := strings.TrimSpace(q.Header)
		if header != "" {
			fmt.Fprintf(&b, "**%d. %s** — *%s*\n", i+1, q.Question, header)
		} else {
			fmt.Fprintf(&b, "**%d. %s**\n", i+1, q.Question)
		}

		// Allocate an action row only while the 5-row budget lasts.
		if len(rows) < maxActionRows && len(q.Options) > 0 {
			if row := buildQuestionRow(token, i, q); row != nil {
				rows = append(rows, row)
				continue
			}
		}
		if len(q.Options) == 0 {
			b.WriteString("→ 💬 请直接回复此消息回答\n")
		} else {
			b.WriteString("→ 选项见下方按钮 / 或直接回复此消息\n")
		}
	}

	return strings.TrimSpace(b.String()), rows
}

// buildQuestionRow builds a single action row for question qi:
//   - single-select with ≤5 options → one row of buttons
//   - multi-select or >5 options → a select menu
//
// Returns nil when the question has no options (callers fall back to text).
func buildQuestionRow(token string, qi int, q channel.Question) discordgo.MessageComponent {
	if len(q.Options) > maxButtonsPerRow || q.MultiSelect {
		menu := buildQuestionSelectMenu(token, qi, q)
		if menu == nil {
			return nil
		}
		return &discordgo.ActionsRow{Components: []discordgo.MessageComponent{menu}}
	}

	row := discordgo.ActionsRow{}
	for oi, opt := range q.Options {
		row.Components = append(row.Components, &discordgo.Button{
			Label:    truncateRunes(opt.Label, maxButtonLabelRunes),
			Style:    discordgo.PrimaryButton,
			CustomID: fmt.Sprintf("%s%s:q%d:o%d", askCustomIDPrefix, token, qi, oi),
		})
	}
	if len(row.Components) == 0 {
		return nil
	}
	return &row
}

// buildQuestionSelectMenu builds the select menu for question qi.
// The option value IS its label — the agent's answer carries the label text.
// MinValues is pinned to 1 so a selection is always submitted.
func buildQuestionSelectMenu(token string, qi int, q channel.Question) *discordgo.SelectMenu {
	if len(q.Options) == 0 {
		return nil
	}
	opts := make([]discordgo.SelectMenuOption, 0, len(q.Options))
	for _, opt := range q.Options {
		label := truncateRunes(opt.Label, maxButtonLabelRunes)
		opts = append(opts, discordgo.SelectMenuOption{
			Label:       label,
			Value:       label,
			Description: truncateRunes(opt.Description, maxSelectOptionDescRunes),
		})
	}
	min := 1
	max := 1
	if q.MultiSelect {
		max = len(opts)
	}
	return &discordgo.SelectMenu{
		CustomID:    fmt.Sprintf("%s%s:q%d", askCustomIDPrefix, token, qi),
		Placeholder: "请选择…",
		MinValues:   &min,
		MaxValues:   max,
		Options:     opts,
	}
}

// parseAskCustomID splits a "tachi:ask:<token>:q<idx>[:o<optIdx>]" CustomID.
// oi is -1 for select-menu interactions (no button option index).
func parseAskCustomID(customID string) (token string, qi, oi int, ok bool) {
	if !strings.HasPrefix(customID, askCustomIDPrefix) {
		return "", 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(customID, askCustomIDPrefix), ":")
	// Exactly 2 segments for select menus / questions, 3 for buttons.
	if len(parts) < 2 || len(parts) > 3 {
		return "", 0, 0, false
	}
	token = parts[0]
	qi, ok = parseQuestionIdx(parts[1])
	if !ok || token == "" {
		return "", 0, 0, false
	}
	oi = -1
	if len(parts) >= 3 {
		if oi, ok = parseQuestionIdx(parts[2]); !ok {
			return "", 0, 0, false
		}
	}
	return token, qi, oi, true
}

// parseQuestionIdx parses an index segment consisting of a single letter
// followed by digits ("q0", "o3", "q12"...). Returns the parsed value.
func parseQuestionIdx(s string) (int, bool) {
	if len(s) < 2 {
		return 0, false
	}
	c := s[0]
	if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
		return 0, false
	}
	n := 0
	for _, d := range s[1:] {
		if d < '0' || d > '9' {
			return 0, false
		}
		n = n*10 + int(d-'0')
		if n > 1<<20 { // sanity cap
			return 0, false
		}
	}
	return n, true
}

// --- pending question state registry -------------------------------------

// registerQuestionState stores a pending question prompt keyed by token.
func (ch *DiscordChannel) registerQuestionState(st *questionState) {
	ch.questionStatesMu.Lock()
	ch.questionStates[st.token] = st
	ch.questionStatesMu.Unlock()
}

// unregisterQuestionState removes a pending question prompt.
func (ch *DiscordChannel) unregisterQuestionState(token string) {
	ch.questionStatesMu.Lock()
	delete(ch.questionStates, token)
	ch.questionStatesMu.Unlock()
}

// lookupQuestionState returns the pending state for token, or nil when
// unknown, already answered, or expired (expired entries are cleaned up).
func (ch *DiscordChannel) lookupQuestionState(token string) *questionState {
	ch.questionStatesMu.Lock()
	defer ch.questionStatesMu.Unlock()

	st, ok := ch.questionStates[token]
	if !ok {
		return nil
	}
	if st.answered || time.Since(st.created) > askStateTTL {
		delete(ch.questionStates, token)
		return nil
	}
	return st
}

// markQuestionAnswered flags a pending question as answered and edits the
// question message to disable its components, showing what was chosen.
// Best-effort: failures are logged, not propagated (the answer itself is
// already on its way to the agent).
func (ch *DiscordChannel) markQuestionAnswered(st *questionState, summary string) {
	ch.questionStatesMu.Lock()
	st.answered = true
	ch.questionStatesMu.Unlock()

	sess := ch.session
	if sess == nil || st.messageID == "" {
		return
	}

	// Re-render the message with all components disabled and a note about
	// the chosen answer appended to the text.
	content, components := buildQuestionMessage(st.token, st.questions)
	if summary != "" {
		content = content + "\n\n✅ 已选择: " + summary
	}
	disableQuestionComponents(components)

	if _, err := sess.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    st.channelID,
		ID:         st.messageID,
		Content:    &content,
		Components: &components,
	}); err != nil {
		ch.logger.Error(context.Background(), "discord: disable question components failed", err, "channel", st.channelID)
	}
}

// disableQuestionComponents marks every button / select menu in the
// component tree disabled so users can't answer the same question twice.
// The tree is built with pointers (see buildQuestionRow), so mutations
// here reach the marshaled message.
func disableQuestionComponents(components []discordgo.MessageComponent) {
	for _, c := range components {
		switch comp := c.(type) {
		case *discordgo.ActionsRow:
			disableQuestionComponents(comp.Components)
		case *discordgo.Button:
			comp.Disabled = true
		case *discordgo.SelectMenu:
			comp.Disabled = true
		}
	}
}

// --- component interaction handling ---------------------------------------

// handleComponentInteraction processes an INTERACTION_CREATE of type
// message component (button click / select menu selection) for an
// AskUserQuestion prompt. It routes the chosen answer back to the waiting
// agent turn through the message handler and edits the question message to
// disable the components.
func (ch *DiscordChannel) handleComponentInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, handler channel.MessageHandler) {
	data, ok := i.Data.(*discordgo.MessageComponentInteractionData)
	if !ok {
		return
	}

	token, qi, oi, ok := parseAskCustomID(data.CustomID)
	if !ok {
		// Not one of ours — ignore silently.
		return
	}

	st := ch.lookupQuestionState(token)
	if st == nil {
		// Unknown / expired / double-clicked. Reply ephemerally so the user
		// knows to type the answer instead of clicking a dead button.
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "⚠️ 这个问题已经回答过或已过期，请直接发文字告诉我。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// Resolve the answers from the interaction payload.
	answers, summary, ok := resolveQuestionAnswers(st.questions, data, qi, oi)
	if !ok {
		return
	}

	// ACK fast (deferred update hides the "waiting" state on the button).
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}); err != nil {
		ch.logger.Error(context.Background(), "discord: question interaction ack failed", err, "token", token)
	}

	// Disable the components and show the choice (best-effort).
	ch.markQuestionAnswered(st, summary)

	// Deliver the answer to the agent turn. The manager handler routes
	// AskUserAnswers back to the waiting drainEvents and returns
	// Steered=true — there is nothing to send as a reply.
	incoming := channel.IncomingMessage{
		ThreadID:       st.threadID,
		MessageID:      i.Message.ID,
		Content:        summary,
		AskUserAnswers: answers,
	}
	if result := handler(context.Background(), incoming); result.Steered {
		ch.logger.Info(context.Background(), "discord: AskUser answer routed to agent",
			"thread", st.threadID, "answers", len(answers))
	} else {
		ch.logger.Warn(context.Background(), "discord: component handler returned non-steered result", "thread", st.threadID)
	}
}

// resolveQuestionAnswers maps a component interaction payload to the answer
// map expected by the agent ("q<idx>" → label text). summary is a short
// human-readable "what was chosen" note for the edited message.
func resolveQuestionAnswers(questions []channel.Question, data *discordgo.MessageComponentInteractionData, qi, oi int) (answers map[string]string, summary string, ok bool) {
	if qi < 0 || qi >= len(questions) {
		return nil, "", false
	}
	q := questions[qi]

	var value string
	switch {
	case oi >= 0: // button click
		if oi >= len(q.Options) {
			return nil, "", false
		}
		value = q.Options[oi].Label
	case len(data.Values) > 0: // select menu selection (multi or single)
		value = strings.Join(data.Values, "\n")
	default:
		return nil, "", false
	}

	key := fmt.Sprintf("q%d", qi)
	answers = map[string]string{key: value}
	if len(data.Values) > 1 {
		summary = strings.Join(data.Values, ", ")
	} else {
		summary = value
	}
	return answers, summary, true
}

// newQuestionToken returns a random hex token used in component CustomIDs.
func newQuestionToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is effectively impossible on normal systems;
		// fall back to a time-based token rather than failing the turn.
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
