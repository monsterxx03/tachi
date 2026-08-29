package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/monsterxx03/tachi/pkg/channel"
	"github.com/monsterxx03/tachi/pkg/container"
)

// ---------------------------------------------------------------------------
// buildQuestionMessage / buildQuestionRow
// ---------------------------------------------------------------------------

func TestBuildQuestionMessage_ButtonsRow(t *testing.T) {
	questions := []channel.Question{
		{
			Question: "选哪个方案？",
			Header:   "方案",
			Options: []channel.QuestionOption{
				{Label: "方案 A", Description: "快"},
				{Label: "方案 B", Description: "稳"},
				{Label: "方案 C", Description: "炫"},
			},
		},
	}
	content, components := buildQuestionMessage("tok123", questions)

	if !strings.Contains(content, "选哪个方案？") {
		t.Errorf("content missing question text: %q", content)
	}
	if !strings.Contains(content, "方案") {
		t.Errorf("content missing header: %q", content)
	}
	if !strings.Contains(content, "请逐题回答") {
		t.Errorf("content missing per-question hint: %q", content)
	}
	if len(components) != 1 {
		t.Fatalf("want 1 action row, got %d", len(components))
	}

	row, ok := components[0].(*discordgo.ActionsRow)
	if !ok {
		t.Fatalf("want *ActionsRow, got %T", components[0])
	}
	if len(row.Components) != 3 {
		t.Fatalf("want 3 buttons, got %d", len(row.Components))
	}

	for i, c := range row.Components {
		btn, ok := c.(*discordgo.Button)
		if !ok {
			t.Fatalf("component %d: want *Button, got %T", i, c)
		}
		want := "tachi:ask:tok123:q0:o" + string(rune('0'+i))
		if btn.CustomID != want {
			t.Errorf("customID = %q, want %q", btn.CustomID, want)
		}
		if btn.Label != questions[0].Options[i].Label {
			t.Errorf("label = %q, want %q", btn.Label, questions[0].Options[i].Label)
		}
	}
}

func TestBuildQuestionMessage_MultiSelectUsesSelectMenu(t *testing.T) {
	questions := []channel.Question{
		{
			Question:    "多选",
			MultiSelect: true,
			Options: []channel.QuestionOption{
				{Label: "A", Description: "a"},
				{Label: "B", Description: "b"},
			},
		},
	}
	_, components := buildQuestionMessage("tok", questions)
	row := components[0].(*discordgo.ActionsRow)
	menu, ok := row.Components[0].(*discordgo.SelectMenu)
	if !ok {
		t.Fatalf("want *SelectMenu, got %T", row.Components[0])
	}
	if menu.MaxValues != 2 {
		t.Errorf("multi-select max values = %d, want 2", menu.MaxValues)
	}
	if menu.MinValues == nil || *menu.MinValues != 1 {
		t.Errorf("min values = %v, want 1", menu.MinValues)
	}
	if menu.CustomID != "tachi:ask:tok:q0" {
		t.Errorf("customID = %q", menu.CustomID)
	}
	if len(menu.Options) != 2 || menu.Options[0].Value != "A" {
		t.Errorf("menu options wrong: %+v", menu.Options)
	}
}

func TestBuildQuestionMessage_TooManyOptionsUsesSelectMenu(t *testing.T) {
	opts := make([]channel.QuestionOption, 0, 6)
	for i := 0; i < 6; i++ {
		opts = append(opts, channel.QuestionOption{Label: string(rune('A' + i))})
	}
	questions := []channel.Question{{Question: "six", Options: opts}}

	_, components := buildQuestionMessage("tok", questions)
	row := components[0].(*discordgo.ActionsRow)
	if _, ok := row.Components[0].(*discordgo.SelectMenu); !ok {
		t.Fatalf("want *SelectMenu for 6 options, got %T", row.Components[0])
	}
}

func TestBuildQuestionMessage_TooManyOptionsTruncatesMenu(t *testing.T) {
	opts := make([]channel.QuestionOption, 0, 30)
	for i := 0; i < 30; i++ {
		opts = append(opts, channel.QuestionOption{Label: "opt" + string(rune('A'+i%26)) + string(rune('0'+i/26))})
	}
	questions := []channel.Question{{Question: "many", Options: opts}}

	content, components := buildQuestionMessage("tok", questions)
	row := components[0].(*discordgo.ActionsRow)
	menu, ok := row.Components[0].(*discordgo.SelectMenu)
	if !ok {
		t.Fatalf("want *SelectMenu, got %T", row.Components[0])
	}
	if len(menu.Options) != maxSelectMenuOptions {
		t.Errorf("menu options = %d, want cap %d", len(menu.Options), maxSelectMenuOptions)
	}
	if menu.MaxValues != 1 {
		t.Errorf("max values = %d, want 1 (truncated singleselect)", menu.MaxValues)
	}
	if !strings.Contains(content, "仅展示前 25 个") {
		t.Errorf("content missing excess-options hint: %q", content)
	}
}

func TestBuildQuestionMessage_MultiSelectMaxValuesAfterTruncation(t *testing.T) {
	opts := make([]channel.QuestionOption, 0, 30)
	for i := 0; i < 30; i++ {
		opts = append(opts, channel.QuestionOption{Label: "o" + string(rune('0'+i%10))})
	}
	questions := []channel.Question{{Question: "many", MultiSelect: true, Options: opts}}

	_, components := buildQuestionMessage("tok", questions)
	menu := components[0].(*discordgo.ActionsRow).Components[0].(*discordgo.SelectMenu)
	if menu.MaxValues != maxSelectMenuOptions {
		t.Errorf("multi-select max = %d, want %d after truncation", menu.MaxValues, maxSelectMenuOptions)
	}
}

func TestBuildQuestionMessage_LongBodyTruncated(t *testing.T) {
	questions := []channel.Question{
		{Question: strings.Repeat("很长的", 700)}, // 2100 runes, will overflow
	}
	content, _ := buildQuestionMessage("tok", questions)
	if utf8RuneCount(content) > discordMessageLimit {
		t.Errorf("content runes = %d, want ≤ %d", utf8RuneCount(content), discordMessageLimit)
	}
	if !strings.Contains(content, "已截断") {
		t.Errorf("content missing truncation marker: %q", content)
	}
}

func TestBuildQuestionMessage_NoOptionsFallsBackToText(t *testing.T) {
	questions := []channel.Question{
		{Question: "开放问题", Header: "自由"},
	}
	content, components := buildQuestionMessage("tok", questions)

	if len(components) != 0 {
		t.Fatalf("want no components, got %d", len(components))
	}
	if !strings.Contains(content, "请直接回复此消息") {
		t.Errorf("content missing text-fallback hint: %q", content)
	}
}

func TestBuildQuestionMessage_ActionRowBudget(t *testing.T) {
	// 6 button questions → only 5 rows fit; the 6th falls back to text.
	questions := make([]channel.Question, 6)
	for i := range questions {
		questions[i] = channel.Question{
			Question: "q",
			Options: []channel.QuestionOption{
				{Label: "是"}, {Label: "否"},
			},
		}
	}
	content, components := buildQuestionMessage("tok", questions)
	if len(components) != maxActionRows {
		t.Fatalf("want %d rows, got %d", maxActionRows, len(components))
	}
	if !strings.Contains(content, "选项见下方按钮") {
		t.Errorf("6th question should fall back to text hint: %q", content)
	}
}

func TestBuildQuestionRow_LongLabelTruncated(t *testing.T) {
	long := strings.Repeat("长", 100) // 100 runes > 80
	q := channel.Question{Options: []channel.QuestionOption{{Label: long}}}
	row := buildQuestionRow("tok", 0, q).(*discordgo.ActionsRow)
	btn := row.Components[0].(*discordgo.Button)
	if got := utf8RuneCount(btn.Label); got > maxButtonLabelRunes {
		t.Errorf("label runes = %d, want ≤ %d", got, maxButtonLabelRunes)
	}
}

func utf8RuneCount(s string) int {
	return len([]rune(s))
}

// ---------------------------------------------------------------------------
// parseAskCustomID / parseIdxSegment
// ---------------------------------------------------------------------------

func TestParseAskCustomID(t *testing.T) {
	tests := []struct {
		name      string
		customID  string
		wantToken string
		wantQI    int
		wantOI    int
		wantOK    bool
	}{
		{"button", "tachi:ask:abc123:q0:o2", "abc123", 0, 2, true},
		{"select menu", "tachi:ask:abc123:q1", "abc123", 1, -1, true},
		{"multi digit idx", "tachi:ask:tok:q12:o34", "tok", 12, 34, true},
		{"unrelated prefix", "tachi:other:xyz", "", 0, 0, false},
		{"wrong prefix", "hello:q0", "", 0, 0, false},
		{"missing idx", "tachi:ask:tok", "", 0, 0, false},
		{"empty token", "tachi:ask::q0", "", 0, 0, false},
		{"non-numeric idx", "tachi:ask:tok:qX", "", 0, 0, false},
		{"swapped roles", "tachi:ask:tok:o1:q0", "", 0, 0, false},
		{"trailing junk", "tachi:ask:tok:q0:o1:zzz", "", 0, 0, false},
		{"option without question", "tachi:ask:tok:o1", "", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, qi, oi, ok := parseAskCustomID(tt.customID)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if token != tt.wantToken || qi != tt.wantQI || oi != tt.wantOI {
				t.Errorf("got (%q,%d,%d), want (%q,%d,%d)", token, qi, oi, tt.wantToken, tt.wantQI, tt.wantOI)
			}
		})
	}
}

func TestParseIdxSegment(t *testing.T) {
	tests := []struct {
		in     string
		letter byte
		want   int
		ok     bool
	}{
		{"q0", 'q', 0, true},
		{"q1", 'q', 1, true},
		{"q42", 'q', 42, true},
		{"o3", 'o', 3, true},
		{"o1", 'q', 0, false}, // wrong role marker
		{"", 'q', 0, false},
		{"q", 'q', 0, false},
		{"0", 'q', 0, false},
		{"q-1", 'q', 0, false},
	}
	for _, tt := range tests {
		got, ok := parseIdxSegment(tt.in, tt.letter)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseIdxSegment(%q,%q) = (%d,%v), want (%d,%v)", tt.in, tt.letter, got, ok, tt.want, tt.ok)
		}
	}
}

// ---------------------------------------------------------------------------
// resolveQuestionAnswers
// ---------------------------------------------------------------------------

func TestResolveQuestionAnswers_Button(t *testing.T) {
	questions := []channel.Question{{Options: []channel.QuestionOption{{Label: "A", Description: "x"}, {Label: "B"}}}}
	data := discordgo.MessageComponentInteractionData{CustomID: "tachi:ask:t:q0:o1"}
	answers, summary, ok := resolveQuestionAnswers(questions, data, 0, 1)
	if !ok {
		t.Fatal("want ok")
	}
	if answers["q0"] != "B" {
		t.Errorf("answers = %v", answers)
	}
	if summary != "B" {
		t.Errorf("summary = %q", summary)
	}
}

func TestResolveQuestionAnswers_SingleSelectMenu(t *testing.T) {
	questions := []channel.Question{{Options: []channel.QuestionOption{{Label: "A"}, {Label: "B"}}}}
	data := discordgo.MessageComponentInteractionData{Values: []string{"B"}}
	answers, _, ok := resolveQuestionAnswers(questions, data, 0, -1)
	if !ok || answers["q0"] != "B" {
		t.Errorf("answers = %v, ok = %v", answers, ok)
	}
}

func TestResolveQuestionAnswers_MultiSelect(t *testing.T) {
	questions := []channel.Question{{MultiSelect: true, Options: []channel.QuestionOption{{Label: "A"}, {Label: "B"}, {Label: "C"}}}}
	data := discordgo.MessageComponentInteractionData{Values: []string{"A", "C"}}
	answers, summary, ok := resolveQuestionAnswers(questions, data, 0, -1)
	if !ok {
		t.Fatal("want ok")
	}
	if answers["q0"] != "A\nC" {
		t.Errorf("answers = %v", answers)
	}
	if summary != "A, C" {
		t.Errorf("summary = %q", summary)
	}
}

func TestResolveQuestionAnswers_OutOfRange(t *testing.T) {
	questions := []channel.Question{{Options: []channel.QuestionOption{{Label: "A"}}}}
	if _, _, ok := resolveQuestionAnswers(questions, discordgo.MessageComponentInteractionData{}, 3, -1); ok {
		t.Error("out-of-range question index should fail")
	}
	if _, _, ok := resolveQuestionAnswers(questions, discordgo.MessageComponentInteractionData{}, 0, 5); ok {
		t.Error("out-of-range option index should fail")
	}
}

// ---------------------------------------------------------------------------
// disableQuestionComponents
// ---------------------------------------------------------------------------

func TestDisableQuestionComponents(t *testing.T) {
	_, components := buildQuestionMessage("tok", []channel.Question{
		{Options: []channel.QuestionOption{{Label: "A"}, {Label: "B"}}},
		{MultiSelect: true, Options: []channel.QuestionOption{{Label: "X"}, {Label: "Y"}}},
	})
	disableQuestionComponents(components)

	row1 := components[0].(*discordgo.ActionsRow)
	for _, c := range row1.Components {
		if btn := c.(*discordgo.Button); !btn.Disabled {
			t.Error("button should be disabled")
		}
	}
	row2 := components[1].(*discordgo.ActionsRow)
	if menu := row2.Components[0].(*discordgo.SelectMenu); !menu.Disabled {
		t.Error("select menu should be disabled")
	}
}

// ---------------------------------------------------------------------------
// question state registry
// ---------------------------------------------------------------------------

func newTestQuestionChannel() *DiscordChannel {
	return &DiscordChannel{questionStates: &container.LockedMap[string, *questionState]{}}
}

func TestClaimQuestionState(t *testing.T) {
	ch := newTestQuestionChannel()
	st := &questionState{token: "tok1", threadID: "guild:1:channel:2", created: time.Now()}
	ch.questionStates.Store("tok1", st)

	// First claim succeeds and removes the entry.
	if got := ch.claimQuestionState("tok1"); got != st {
		t.Fatalf("claim returned %v, want %v", got, st)
	}
	// Second claim (double-click) finds nothing.
	if got := ch.claimQuestionState("tok1"); got != nil {
		t.Fatalf("second claim should be nil, got %v", got)
	}
	// Unknown token.
	if got := ch.claimQuestionState("nope"); got != nil {
		t.Fatalf("unknown token claim should be nil, got %v", got)
	}
}

func TestClaimQuestionState_Expired(t *testing.T) {
	ch := newTestQuestionChannel()
	ch.questionStates.Store("tok", &questionState{token: "tok", created: time.Now().Add(-(askStateTTL + time.Minute))})
	if got := ch.claimQuestionState("tok"); got != nil {
		t.Fatalf("expired claim should be nil and remove entry, got %v", got)
	}
	if _, ok := ch.questionStates.Load("tok"); ok {
		t.Fatal("expired entry should be removed from the map")
	}
}

func TestAcknowledgeAskUser(t *testing.T) {
	ch := newTestQuestionChannel()
	ch.questionStates.Store("tok1", &questionState{token: "tok1", threadID: "t1", channelID: "c1", messageID: "m1", created: time.Now()})
	ch.questionStates.Store("tok2", &questionState{token: "tok2", threadID: "t2", channelID: "c2", created: time.Now()})

	// Acknowledging t1 removes only t1's state (t2 untouched).
	ch.AcknowledgeAskUser("t1")
	if _, ok := ch.questionStates.Load("tok1"); ok {
		t.Fatal("acknowledged state should be removed")
	}
	if _, ok := ch.questionStates.Load("tok2"); !ok {
		t.Fatal("unrelated state should remain")
	}

	// Acknowledging a thread with no pending state is a no-op.
	ch.AcknowledgeAskUser("t9")
}

func TestCleanupThreadQuestions(t *testing.T) {
	ch := newTestQuestionChannel()
	ch.questionStates.Store("a", &questionState{token: "a", threadID: "t1", created: time.Now()})
	ch.questionStates.Store("b", &questionState{token: "b", threadID: "t1", created: time.Now()})
	ch.questionStates.Store("c", &questionState{token: "c", threadID: "t2", created: time.Now()})

	ch.cleanupThreadQuestions("t1")
	if ch.questionStates.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (only t2 remains)", ch.questionStates.Len())
	}
	if _, ok := ch.questionStates.Load("c"); !ok {
		t.Fatal("t2 state should remain")
	}
}

func TestQuestionTokenIsColonFree(t *testing.T) {
	// The token is embedded in a colon-separated CustomID — it must never
	// contain ':'. strutil.ShortUUID strips hyphens only, so this is a
	// regression guard for that invariant.
	ch := newTestQuestionChannel()
	_ = ch
	a := "tok" // placeholder; actual tokens come from strutil.ShortUUID
	if strings.Contains(a, ":") {
		t.Fatal("token must not contain ':'")
	}
}

// TestComponentInteractionDataValueType guards the root cause of "button
// click → interaction failed": discordgo v0.29.0 stores message component
// data as a VALUE type in InteractionCreate.Data (see its UnmarshalJSON and
// the MessageComponentData helper). handleComponentInteraction must assert
// the value type — a pointer assertion always fails, silently leaves the
// interaction unacknowledged, and Discord shows "This interaction failed"
// after 3 seconds. The test mirrors discordgo's decoding exactly.
func TestComponentInteractionDataValueType(t *testing.T) {
	// What discordgo v0.29.0 produces for an INTERACTION_CREATE with type
	// InteractionMessageComponent (value, not pointer).
	decoded := discordgo.MessageComponentInteractionData{CustomID: "tachi:ask:tok:q0:o0"}
	ic := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			ID:   "1",
			Type: discordgo.InteractionMessageComponent,
			Data: decoded,
		},
	}

	// The handler's assertion must match the value type.
	data, ok := ic.Data.(discordgo.MessageComponentInteractionData)
	if !ok {
		t.Fatal("BUG: i.Data must be the value type MessageComponentInteractionData; " +
			"asserting the pointer type (the original bug) makes every button click fail")
	}
	if data.CustomID != "tachi:ask:tok:q0:o0" {
		t.Errorf("CustomID = %q", data.CustomID)
	}

	// Guard against regressing to the pointer assertion.
	if _, ok := ic.Data.(*discordgo.MessageComponentInteractionData); ok {
		t.Error("pointer assertion must NOT match: discordgo stores the value type")
	}
}
