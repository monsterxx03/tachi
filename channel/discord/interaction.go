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
//   - PresentQuestions renders the questions as a Discord message. The
//     primary interaction model is a modal: the message lists every
//     question (options included) plus a single "开始回答" button whose
//     click opens a modal with one text input per question; submitting the
//     modal delivers ALL answers in one round trip — matching the
//     AskUserQuestion tool semantics (one call returns all answers). The
//     modal is capped at 5 questions (Discord's modal component limit);
//     prompts beyond that fall back to per-question button rows
//     (single-select ≤5 options → buttons, multi-select / more options →
//     select menu), a legacy mode kept for capability.
//   - Component interactions arrive as INTERACTION_CREATE events in a
//     discordgo callback goroutine, while the agent turn blocks in the
//     manager: the "开始回答" click responds with the modal; the modal
//     submission (InteractionModalSubmit) atomically claims the pending
//     state, builds an IncomingMessage with AskUserAnswers and delegates to
//     the same message handler the manager passed to Run(), which routes
//     the answers back to the waiting agent (it returns Steered=true and
//     never produces a reply to send).
//   - The question message is edited after answers are recorded (via the
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
// observe the complete state. Claiming (button click / modal submission)
// uses atomic LoadAndDelete so concurrent double-interactions can never
// both deliver answers; the modal's "开始回答" button itself only reads
// (Load) the state, which stays claimable until the final submission.
//
// CustomID namespace: "tachi:ask:<token>:q<idx>:o<optIdx>" (button),
// "tachi:ask:<token>:q<idx>" (select menu), "tachi:ask:<token>:begin"
// (open the answer modal) and "tachi:ask:<token>:submit" (modal
// submission).

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

	// maxModalQuestions is Discord's cap on modal components (each question
	// takes one action row holding a text input). Prompts with more
	// questions fall back to per-question button rows.
	maxModalQuestions = 5
	// maxModalTitleRunes is Discord's modal title limit.
	maxModalTitleRunes = 45
	// maxModalLabelRunes is Discord's text-input label limit.
	maxModalLabelRunes = 45
	// maxModalPlaceholderRunes is Discord's text-input placeholder limit.
	maxModalPlaceholderRunes = 100
	// maxModalAnswerRunes caps each typed answer (well under Discord's 4000).
	maxModalAnswerRunes = 400
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
	// content and components cache the exact rendering sent to Discord.
	// markQuestionAnswered reuses them (disabling the components, appending
	// the answer summary) so the question message is never re-rendered into
	// a different layout — e.g. a modal prompt must not turn back into a
	// pile of per-question button rows after submission.
	content    string
	components []discordgo.MessageComponent
	questions  []channel.Question
	created    time.Time
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

	content, components := buildQuestionPresentation(token, questions)
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
	// guaranteed to observe the complete state (no torn reads). The exact
	// sent rendering is cached so markQuestionAnswered can disable its
	// components without re-rendering the prompt into a different layout.
	ch.questionStates.Store(token, &questionState{
		token:      token,
		threadID:   threadID,
		channelID:  channelID,
		messageID:  sent.ID,
		content:    content,
		components: components,
		questions:  questions,
		created:    time.Now(),
	})
	ch.logger.Info(ctx, "discord: AskUser questions presented", "thread", threadID,
		"count", len(questions), "channel", channelID, "message", sent.ID)
	return nil
}

// buildQuestionPresentation picks the rendering strategy for a prompt:
//   - ≤maxModalQuestions questions → modal mode: a question list plus a
//     "start answering" button that opens a modal collecting every answer
//     in a single submission (the interaction model users expect from
//     AskUserQuestion — one round trip, all answers at once)
//   - more questions → per-question button rows (Discord hard-caps modal
//     components at 5), keeping the per-question click model as fallback
func buildQuestionPresentation(token string, questions []channel.Question) (string, []discordgo.MessageComponent) {
	if len(questions) <= maxModalQuestions {
		return buildModalModeMessage(token, questions)
	}
	return buildQuestionMessage(token, questions)
}

// buildModalModeMessage renders the modal-mode prompt: the full question
// list in the body (options included, so the user can read everything
// before opening the modal) plus a single "开始回答" button whose click
// opens the answer modal. One action row only, so there is never any
// button-row clutter regardless of question count.
func buildModalModeMessage(token string, questions []channel.Question) (string, []discordgo.MessageComponent) {
	var b strings.Builder
	b.WriteString("❓ **Tachi 需要你确认几个问题**\n")
	b.WriteString("点击下方按钮，在弹窗中一次性回答所有问题：每个问题一个输入框，选择题填入选项文字，多选请用逗号分隔。\n")
	for i, q := range questions {
		if i > 0 {
			b.WriteString("\n")
		}
		header := strings.TrimSpace(q.Header)
		if header != "" {
			fmt.Fprintf(&b, "**%d. %s** — *%s*\n", i+1, strings.TrimSpace(q.Question), header)
		} else {
			fmt.Fprintf(&b, "**%d. %s**\n", i+1, strings.TrimSpace(q.Question))
		}
		if len(q.Options) > 0 {
			b.WriteString("选项: ")
			for oi, opt := range q.Options {
				if oi > 0 {
					b.WriteString(" / ")
				}
				b.WriteString(opt.Label)
			}
			b.WriteString("\n")
		}
		if q.MultiSelect {
			b.WriteString("（可多选，用逗号分隔）\n")
		}
	}

	content := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(content) > maxQuestionMsgRunes {
		content = strutil.TruncatePlain(content, maxQuestionMsgRunes) + "\n\n⚠️ 问题列表过长，已截断…"
	}
	row := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		&discordgo.Button{
			Label:    "✅ 开始回答",
			Style:    discordgo.PrimaryButton,
			CustomID: fmt.Sprintf("%s%s:begin", askCustomIDPrefix, token),
		},
	}}
	return content, []discordgo.MessageComponent{&row}
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

// askCustomIDKind identifies what a parsed AskUserQuestion CustomID refers
// to: a component click on an answer row, the "start answering" button that
// opens the modal, or the modal's own submission ID.
type askCustomIDKind int

const (
	askCustomIDSelect askCustomIDKind = iota // "q<idx>" — select menu selection
	askCustomIDButton                        // "q<idx>:o<optIdx>" — button click
	askCustomIDBegin                         // "begin" — open the answer modal
	askCustomIDSubmit                        // "submit" — modal submission
)

func (k askCustomIDKind) String() string {
	switch k {
	case askCustomIDSelect:
		return "select"
	case askCustomIDButton:
		return "button"
	case askCustomIDBegin:
		return "begin"
	case askCustomIDSubmit:
		return "submit"
	}
	return "unknown"
}

// parseAskCustomID splits a "tachi:ask:<token>:<rest>" CustomID into its
// parts, validating segment roles:
//
//   - "q<idx>"             → select-menu interaction (kind=Select, oi=-1)
//   - "q<idx>:o<optIdx>"   → button interaction (kind=Button)
//   - "begin"              → open-answer-modal button (kind=Begin)
//   - "submit"             → modal submission (kind=Submit)
func parseAskCustomID(customID string) (token string, kind askCustomIDKind, qi, oi int, ok bool) {
	if !strings.HasPrefix(customID, askCustomIDPrefix) {
		return "", 0, 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(customID, askCustomIDPrefix), ":")
	// Exactly 2 segments for select/begin/submit, 3 for buttons.
	if len(parts) < 2 || len(parts) > 3 {
		return "", 0, 0, 0, false
	}
	token = parts[0]
	if token == "" {
		return "", 0, 0, 0, false
	}
	switch parts[1] {
	case "begin":
		if len(parts) != 2 {
			return "", 0, 0, 0, false
		}
		return token, askCustomIDBegin, 0, -1, true
	case "submit":
		if len(parts) != 2 {
			return "", 0, 0, 0, false
		}
		return token, askCustomIDSubmit, 0, -1, true
	}
	qi, ok = parseIdxSegment(parts[1], 'q')
	if !ok {
		return "", 0, 0, 0, false
	}
	oi = -1
	if len(parts) == 3 {
		if oi, ok = parseIdxSegment(parts[2], 'o'); !ok {
			return "", 0, 0, 0, false
		}
		return token, askCustomIDButton, qi, oi, true
	}
	return token, askCustomIDSelect, qi, oi, true
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
// The edit reuses the exact rendering cached at send time (questionState
// content/components) rather than re-rendering via buildQuestion* — a
// modal prompt must not morph back into per-question button rows.
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

	// Keep the sent layout; disable all components and note the choice.
	content, components := answeredEditing(st, summary)

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

// answeredEditing builds the settled-message edit from a questionState's
// cached rendering: the original content plus an answer summary, with every
// cached component disabled. It deliberately does NOT re-render via
// buildQuestion* — a modal prompt (question list + "开始回答" button) must
// keep that shape after submission instead of morphing back into per-
// question button rows.
func answeredEditing(st *questionState, summary string) (string, []discordgo.MessageComponent) {
	content := st.content
	if summary != "" {
		content = content + "\n\n✅ 已选择: " + summary
	}
	components := st.components
	disableQuestionComponents(components)
	return content, components
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

// --- answer modal ----------------------------------------------------------

// buildAnswerModal builds the modal that collects answers to every question
// in one submission. Each question becomes one action row holding a short
// text input; the modal's CustomID carries the pending-state token so the
// submission can be routed back to the waiting agent turn. Callers must
// have already checked len(questions) <= maxModalQuestions.
func buildAnswerModal(token string, questions []channel.Question) *discordgo.InteractionResponseData {
	rows := make([]discordgo.MessageComponent, 0, len(questions))
	for i, q := range questions {
		label := strings.TrimSpace(q.Header)
		if label == "" {
			label = strings.TrimSpace(q.Question)
		}
		label = truncateRunes(fmt.Sprintf("Q%d. %s", i+1, label), maxModalLabelRunes)
		rows = append(rows, &discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			&discordgo.TextInput{
				CustomID:    fmt.Sprintf("%s%s:q%d", askCustomIDPrefix, token, i),
				Label:       label,
				Style:       discordgo.TextInputShort,
				Placeholder: buildModalPlaceholder(q),
				Required:    true,
				MaxLength:   maxModalAnswerRunes,
			},
		}})
	}
	return &discordgo.InteractionResponseData{
		CustomID:   fmt.Sprintf("%s%s:submit", askCustomIDPrefix, token),
		Title:      truncateRunes("Tachi 提问", maxModalTitleRunes),
		Components: rows,
	}
}

// buildModalPlaceholder builds the placeholder for a question's text input:
// the question text, the available options ("1=label / 2=label ...") for
// choice questions, and a multi-select hint — all truncated to Discord's
// 100-char placeholder limit.
func buildModalPlaceholder(q channel.Question) string {
	var b strings.Builder
	b.WriteString("回答: ")
	b.WriteString(strings.TrimSpace(q.Question))
	if len(q.Options) > 0 {
		b.WriteString(" | 填选项文字或编号, 可选: ")
		for oi, opt := range q.Options {
			if oi > 0 {
				b.WriteString(" / ")
			}
			fmt.Fprintf(&b, "%d=%s", oi+1, opt.Label)
		}
	} else {
		b.WriteString(" | 自由作答")
	}
	if q.MultiSelect {
		b.WriteString(" (多选, 逗号分隔)")
	}
	return truncateRunes(b.String(), maxModalPlaceholderRunes)
}

// modalTextInputValues walks a modal's component tree and collects every
// text input's CustomID → submitted value.
func modalTextInputValues(components []discordgo.MessageComponent) map[string]string {
	values := make(map[string]string)
	var walk func([]discordgo.MessageComponent)
	walk = func(comps []discordgo.MessageComponent) {
		for _, c := range comps {
			switch comp := c.(type) {
			case *discordgo.ActionsRow:
				walk(comp.Components)
			case *discordgo.TextInput:
				if comp.CustomID != "" {
					values[comp.CustomID] = comp.Value
				}
			}
		}
	}
	walk(components)
	return values
}

// modalAnswers maps the modal's submitted values to the answer map expected
// by the agent ("q<idx>" → text) plus a human-readable summary. Missing or
// blank inputs are skipped.
func modalAnswers(questions []channel.Question, token string, raw map[string]string) (answers map[string]string, summary string) {
	answers = make(map[string]string)
	var parts []string
	for i := range questions {
		key := fmt.Sprintf("%s%s:q%d", askCustomIDPrefix, token, i)
		v := strings.TrimSpace(raw[key])
		if v == "" {
			continue
		}
		answers[fmt.Sprintf("q%d", i)] = v
		parts = append(parts, fmt.Sprintf("Q%d: %s", i+1, v))
	}
	return answers, strings.Join(parts, " | ")
}

// pendingQuestionState reads the pending state for a token WITHOUT claiming
// it — used by the modal's "start answering" button, which may be clicked
// several times while the user works through the modal. The state is only
// claimed (LoadAndDelete) on the final submission, so opening the modal can
// never consume the answerability of the prompt.
func (ch *DiscordChannel) pendingQuestionState(token string) *questionState {
	st, ok := ch.questionStates.Load(token)
	if !ok || time.Since(st.created) > askStateTTL {
		return nil
	}
	return st
}

// handleBeginAnswer handles a click on the modal-mode "开始回答" button: it
// responds with the answer modal. The pending state is left untouched so
// the submission (a separate interaction) can claim it later.
func (ch *DiscordChannel) handleBeginAnswer(s *discordgo.Session, i *discordgo.InteractionCreate, token string) {
	st := ch.pendingQuestionState(token)
	if st == nil {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "⚠️ 这个问题已过期或已回答，请直接发文字告诉我。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: buildAnswerModal(token, st.questions),
	}); err != nil {
		ch.logger.Error(context.Background(), "discord: open answer modal failed", err, "token", token)
	}
}

// handleModalSubmit processes an INTERACTION_CREATE of type
// InteractionModalSubmit for an AskUserQuestion modal. It claims the
// pending state, collects all text-input values into the agent's answer
// map, routes them to the waiting turn via the message handler, and edits
// the question message to disable its components and show the summary.
func (ch *DiscordChannel) handleModalSubmit(s *discordgo.Session, i *discordgo.InteractionCreate, handler channel.MessageHandler) {
	// Same value-type caveat as message component data: discordgo v0.29.0
	// stores ModalSubmitInteractionData by value in i.Data.
	data, ok := i.Data.(discordgo.ModalSubmitInteractionData)
	if !ok {
		ch.logger.Error(context.Background(), "discord: unexpected modal submit data type",
			fmt.Errorf("type=%v", i.Type), "dataType", fmt.Sprintf("%T", i.Data))
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ 提交数据解析失败，请直接发文字回复。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}
	token, kind, _, _, ok := parseAskCustomID(data.CustomID)
	if !ok || kind != askCustomIDSubmit {
		return // not one of ours — ignore silently
	}

	st := ch.claimQuestionState(token)
	if st == nil {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "⚠️ 这个问题已过期或已回答，请直接发文字告诉我。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	answers, summary := modalAnswers(st.questions, token, modalTextInputValues(data.Components))
	if len(answers) == 0 {
		ch.logger.Warn(context.Background(), "discord: modal submit with no answers", "token", token)
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "⚠️ 没有收到你的任何回答，请直接发文字告诉我。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// ACK fast (deferred update hides the loading state).
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}); err != nil {
		ch.logger.Error(context.Background(), "discord: modal submit ack failed", err, "token", token)
	}
	ch.markQuestionAnswered(st, i.Interaction, summary)

	msgID := i.Message.ID
	if msgID == "" {
		msgID = st.messageID
	}
	incoming := channel.IncomingMessage{
		ThreadID:       st.threadID,
		MessageID:      msgID,
		Content:        summary,
		AskUserAnswers: answers,
	}
	if result := handler(context.Background(), incoming); result.Steered {
		ch.logger.Info(context.Background(), "discord: AskUser modal answers routed to agent",
			"thread", st.threadID, "answers", len(answers))
	} else {
		ch.logger.Warn(context.Background(), "discord: modal submit produced non-steered result; forwarding reply",
			"thread", st.threadID)
		if result.Reply.Content != "" {
			if err := ch.sendText(st.channelID, result.Reply.Content); err != nil {
				ch.logger.Error(context.Background(), "discord: forward stale modal submit reply failed", err)
			}
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
	// discordgo v0.29.0 stores message component data as a VALUE type in
	// i.Data (see Interaction.UnmarshalJSON and the official
	// MessageComponentData helper). Asserting the pointer type instead —
	// the original bug — always fails, silently returning without
	// acknowledging the interaction and leaving the user staring at a
	// "This interaction failed" error after Discord's 3s timeout.
	data, ok := i.Data.(discordgo.MessageComponentInteractionData)
	if !ok {
		ch.logger.Error(context.Background(), "discord: unexpected component interaction data type",
			fmt.Errorf("type=%v", i.Type), "dataType", fmt.Sprintf("%T", i.Data))
		// Never walk away from an unanswered interaction: acknowledge it
		// (best-effort) so the user gets a hint instead of a dead button.
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ 组件数据解析失败，请直接发文字回复。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	token, kind, qi, oi, ok := parseAskCustomID(data.CustomID)
	if !ok {
		// Not one of ours — ignore silently.
		return
	}
	if kind == askCustomIDBegin {
		ch.handleBeginAnswer(s, i, token)
		return
	}
	if kind == askCustomIDSubmit {
		// Modals are handled by handleModalSubmit (INTERACTION_CREATE
		// with type InteractionModalSubmit), not component clicks — a
		// submit CustomID here is out of place; ignore.
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
func resolveQuestionAnswers(questions []channel.Question, data discordgo.MessageComponentInteractionData, qi, oi int) (answers map[string]string, summary string, ok bool) {
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
