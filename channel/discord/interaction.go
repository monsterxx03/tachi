package discord

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/strutil"
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
//     without options tell the user to reply with text. Discord hard limits
//     (5 action rows, 25 menu options, 2000-char messages) are enforced
//     before sending so a prompt can never fail the API call and strand the
//     agent turn.
//   - Button/menu clicks arrive as INTERACTION_CREATE (message component)
//     events in a discordgo callback goroutine, while the agent turn blocks
//     in the manager. The click handler atomically claims the pending state,
//     builds an IncomingMessage with AskUserAnswers and delegates to the
//     same message handler the manager passed to Run(), which routes the
//     answers back to the waiting agent (it returns Steered=true and never
//     produces a reply to send).
//   - The question message is edited after an answer is recorded (via the
//     interaction token when available, falling back to a plain message
//     edit) to disable the components and show what was chosen, preventing
//     double answers.
//   - When the user answers with plain text (manager fallback) or cancels,
//     the manager calls AcknowledgeAskUser (AskUserAcknowledger hook) and
//     the channel retires the pending state + disables the buttons, so a
//     stale click can never start an unintended second turn.
//
// Pending states live in a container.LockedMap keyed by a per-prompt token
// embedded in the CustomIDs. PresentQuestions stores a placeholder entry
// before sending, then re-stores the full state (with the message ID) under
// the lock after a successful send — any subsequent claim is guaranteed to
// observe the complete state. Claiming (button click) uses atomic
// LoadAndDelete so concurrent double-clicks can never both deliver answers.
//
// CustomID namespace: "tachi:ask:<token>:q<idx>:o<optIdx>" (buttons) and
// "tachi:ask:<token>:q<idx>" (select menus).

const (
	// askCustomIDPrefix is the shared prefix for every AskUserQuestion
	// component CustomID. Kept namespaced to avoid colliding with any
	// future component features (design doc §8.1).
	askCustomIDPrefix = "tachi:ask:"

	// askStateTTL is how long a pending question stays answerable.
	// Discord interaction tokens expire ~15 minutes after the message is
	// sent; after that button clicks fail client-side anyway, and typing a
	// text reply remains the fallback (manager routes raw text as an answer
	// to the first question). Claim checks this TTL after LoadAndDelete.
	askStateTTL = 15 * time.Minute

	// maxActionRows is Discord's limit of action rows per message.
	maxActionRows = 5
	// maxButtonsPerRow is Discord's limit of buttons per action row.
	maxButtonsPerRow = 5
	// maxSelectMenuOptions is Discord's limit of options in a select menu.
	maxSelectMenuOptions = 25
	// maxButtonLabelRunes is Discord's button label limit.
	maxButtonLabelRunes = 80
	// maxSelectOptionDescRunes is Discord's select option description limit.
	maxSelectOptionDescRunes = 100
	// maxQuestionMsgRunes is the pre-send cap for the question message body;
	// it leaves headroom under Discord's 2000-char limit for the "已选择"
	// summary appended by markQuestionAnswered.
	maxQuestionMsgRunes = discordMessageLimit - 60
)

// questionState tracks a delivered AskUserQuestion prompt so component
// interactions can be routed back to the waiting agent turn.
//
// A state's lifetime in the registry equals its answerability: it is stored
// on presentation, atomically claimed (LoadAndDelete) on a button/menu
// click, and deleted by AcknowledgeAskUser on text-fallback answers. The
// absence of an entry therefore means "already settled".
type questionState struct {
	token     string // unique token embedded in CustomIDs
	threadID  string // manager thread ID the answers must be routed to
	channelID string // Discord channel the question message lives in
	messageID string // Discord message ID; only set on the post-send version
	questions []channel.Question
	created   time.Time
}

// Compile-time assertions that DiscordChannel satisfies the interactive
// contract expected by the manager: keeping AskUserQuestion registered and
// retiring UI state when prompts settle via the text fallback.
var (
	_ channel.InteractiveChannel  = (*DiscordChannel)(nil)
	_ channel.AskUserAcknowledger = (*DiscordChannel)(nil)
)

// Interactive implements channel.InteractiveChannel.
func (ch *DiscordChannel) Interactive() bool { return true }

// PresentQuestions implements channel.InteractiveChannel. It renders the
// agent's questions as a Discord message with buttons / select menus and
// registers a pending state so subsequent component interactions can be
// routed back to the waiting agent turn. Returns nil once the message is
// sent; the actual answer arrives asynchronously via a component
// interaction, which re-enters through the normal message handler.
//
// Any leftover pending state for the same thread is discarded first (a
// previous prompt may have timed out or settled without UI interaction),
// keeping the registry bounded per thread.
func (ch *DiscordChannel) PresentQuestions(ctx context.Context, threadID, replyID string, questions []channel.Question) error {
	if len(questions) == 0 {
		return fmt.Errorf("discord: PresentQuestions called with no questions")
	}
	sess := ch.session
	if sess == nil {
		return fmt.Errorf("discord: session not initialized")
	}
	channelID := channelIDFromThreadID(threadID)
	if channelID == "" {
		return fmt.Errorf("discord: invalid threadID %q", threadID)
	}

	// Drop any stale pending state for this thread before presenting a new
	// prompt (bounded registry, no unbounded growth across repeated turns).
	ch.cleanupThreadQuestions(threadID)

	token := strutil.ShortUUID(16)
	ch.questionStates.Store(token, &questionState{
		token:     token,
		threadID:  threadID,
		channelID: channelID,
		questions: questions,
		created:   time.Now(),
	})

	content, components := buildQuestionMessage(token, questions)
	sent, err := sess.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:    content,
		Components: components,
	})
	if err != nil {
		ch.questionStates.Delete(token)
		return fmt.Errorf("discord: send question message: %w", err)
	}

	// Re-store under the lock with the message ID filled in. Any claim
	// (component click) happens after this Store returns, so it is
	// guaranteed to observe the complete state (no torn reads).
	ch.questionStates.Store(token, &questionState{
		token:     token,
		threadID:  threadID,
		channelID: channelID,
		messageID: sent.ID,
		questions: questions,
		created:   time.Now(),
	})
	ch.logger.Info(ctx, "discord: AskUser questions presented", "thread", threadID,
		"count", len(questions), "channel", channelID, "message", sent.ID)
	return nil
}

// buildQuestionMessage renders questions into a Discord message: a text
// prompt plus one action row per multiple-choice question (buttons for
// single-select with ≤5 options, a select menu otherwise). Questions
// without options get no components — the user replies with plain text.
// Discord hard limits are enforced: ≤5 action rows, ≤25 menu options
// (excess options fall back to a text hint), and the body is truncated to
// stay under the 2000-char message limit.
func buildQuestionMessage(token string, questions []channel.Question) (string, []discordgo.MessageComponent) {
	var b strings.Builder
	b.WriteString("❓ **Tachi 需要你确认几个问题**\n请逐题回答，回答后我会继续询问剩余问题。\n")

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

		// Allocate an action row while the 5-row budget lasts. Menus with
		// more than 25 options are rendered truncated (the menu itself caps
		// at 25) with a hint that the rest must be typed.
		if len(rows) < maxActionRows && len(q.Options) > 0 {
			if row := buildQuestionRow(token, i, q); row != nil {
				if len(q.Options) > maxSelectMenuOptions {
					b.WriteString("→ ⚠️ 选项过多，仅展示前 25 个；其他选项请直接回复文字\n")
				}
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

	content := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(content) > maxQuestionMsgRunes {
		content = strutil.TruncatePlain(content, maxQuestionMsgRunes) + "\n\n⚠️ 问题列表过长，已截断…"
	}
	return content, rows
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
// MinValues is pinned to 1 so a selection is always submitted. Options are
// capped at Discord's 25-option menu limit (excess handled by the caller's
// text hint).
func buildQuestionSelectMenu(token string, qi int, q channel.Question) *discordgo.SelectMenu {
	if len(q.Options) == 0 {
		return nil
	}
	opts := make([]discordgo.SelectMenuOption, 0, min(len(q.Options), maxSelectMenuOptions))
	for _, opt := range q.Options[:min(len(q.Options), maxSelectMenuOptions)] {
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
		max = len(opts) // computed after the 25-option cap
	}
	return &discordgo.SelectMenu{
		CustomID:    fmt.Sprintf("%s%s:q%d", askCustomIDPrefix, token, qi),
		Placeholder: "请选择…",
		MinValues:   &min,
		MaxValues:   max,
		Options:     opts,
	}
}

// parseAskCustomID splits a "tachi:ask:<token>:q<idx>:o<optIdx>" CustomID
// into its parts, validating segment roles: the question segment must start
// with 'q' and the optional option segment with 'o'. oi is -1 for
// select-menu interactions (no button option index).
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
	if token == "" {
		return "", 0, 0, false
	}
	qi, ok = parseIdxSegment(parts[1], 'q')
	if !ok {
		return "", 0, 0, false
	}
	oi = -1
	if len(parts) == 3 {
		if oi, ok = parseIdxSegment(parts[2], 'o'); !ok {
			return "", 0, 0, false
		}
	}
	return token, qi, oi, true
}

// parseIdxSegment parses "<letter><digits>" (e.g. "q0", "o12"). The letter
// must match the expected role marker, keeping malformed CustomIDs (e.g.
// swapped q/o roles) from ever resolving.
func parseIdxSegment(s string, letter byte) (int, bool) {
	if len(s) < 2 || s[0] != letter {
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

// cleanupThreadQuestions removes every pending state for the given thread.
// Used when presenting a new prompt for a thread (old prompts may have
// settled without UI: turn-timeout, /stop, etc.).
func (ch *DiscordChannel) cleanupThreadQuestions(threadID string) {
	var stale []string
	ch.questionStates.Range(func(token string, st *questionState) bool {
		if st.threadID == threadID {
			stale = append(stale, token)
		}
		return true
	})
	for _, token := range stale {
		ch.questionStates.Delete(token)
	}
}

// claimQuestionState atomically claims the pending state for a token on a
// button/menu click. Claims are take-once: LoadAndDelete removes the entry
// under the lock, so concurrent double-clicks can never both receive the
// state (the loser gets nil and short-circuits with an ephemeral notice).
func (ch *DiscordChannel) claimQuestionState(token string) *questionState {
	st, ok := ch.questionStates.LoadAndDelete(token)
	if !ok || time.Since(st.created) > askStateTTL {
		return nil
	}
	return st
}

// AcknowledgeAskUser implements channel.AskUserAcknowledger. Called by the
// manager after an AskUserQuestion prompt is settled through the fallback
// path (plain-text answer or explicit cancel, both of which the channel
// cannot observe directly). The matching pending state is retired and its
// buttons disabled so a stale click cannot start an unintended second turn.
// UI-click answers also trigger this (the state is already claimed/removed
// by then), so the lookup simply finds nothing and returns — idempotent.
func (ch *DiscordChannel) AcknowledgeAskUser(threadID string) {
	var st *questionState
	ch.questionStates.Range(func(token string, s *questionState) bool {
		if s.threadID == threadID {
			st = s
			return false
		}
		return true
	})
	if st == nil {
		return
	}
	ch.questionStates.Delete(st.token)
	ch.markQuestionAnswered(st, nil, "")
}

// markQuestionAnswered flags a pending question as settled and edits the
// question message to disable its components, showing what was chosen.
// When a component interaction is available (UI-click path) the edit is
// performed through the interaction token (InteractionResponseEdit), which
// properly completes the interaction; otherwise a plain message edit is
// used. Best-effort: failures are logged, not propagated (the answer itself
// is already on its way to the agent).
func (ch *DiscordChannel) markQuestionAnswered(st *questionState, interaction *discordgo.Interaction, summary string) {
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

	// Preferred: complete the interaction via its token. This also removes
	// the client-side loading state on the clicked button.
	if interaction != nil {
		if _, err := sess.InteractionResponseEdit(interaction, &discordgo.WebhookEdit{
			Content:    &content,
			Components: &components,
		}); err == nil {
			return
		} else {
			ch.logger.Error(context.Background(), "discord: interaction response edit failed, falling back to message edit", err)
		}
	}
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
// AskUserQuestion prompt. It atomically claims the pending state, routes
// the chosen answer back to the waiting agent turn through the message
// handler, and edits the question message to disable the components.
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

	st := ch.claimQuestionState(token)
	if st == nil {
		// Unknown / expired / already claimed (double-click). Reply
		// ephemerally so the user knows to type the answer instead of
		// clicking a dead button.
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
		ch.logger.Warn(context.Background(), "discord: question interaction out of range", "token", token, "qi", qi, "oi", oi)
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "⚠️ 无效的问题选项，请直接发文字告诉我。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// ACK fast (deferred update hides the "waiting" state on the button).
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}); err != nil {
		ch.logger.Error(context.Background(), "discord: question interaction ack failed", err, "token", token)
	}

	// Disable the components and show the choice (best-effort).
	ch.markQuestionAnswered(st, i.Interaction, summary)

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
		// Defensive: this should not normally happen (a claimed click implies
		// a waiting agent turn), but if the turn ended between claim and
		// delivery the handler runs a full turn whose reply would otherwise
		// be silently dropped — surface it instead of swallowing it.
		ch.logger.Warn(context.Background(), "discord: question click produced non-steered result; forwarding reply",
			"thread", st.threadID)
		if result.Reply.Content != "" {
			if err := ch.sendText(st.channelID, result.Reply.Content); err != nil {
				ch.logger.Error(context.Background(), "discord: forward stale question click reply failed", err)
			}
		}
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
