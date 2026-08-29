package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/monsterxx03/tachi/pkg/channel"
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
		if btn.CustomID != "tachi:ask:tok123:q0:o"+string(rune('0'+i)) {
			t.Errorf("customID = %q", btn.CustomID)
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
	if got := len([]rune(btn.Label)); got > maxButtonLabelRunes {
		t.Errorf("label runes = %d, want ≤ %d", got, maxButtonLabelRunes)
	}
}

// ---------------------------------------------------------------------------
// parseAskCustomID / parseQuestionIdx
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
		{"trailing junk", "tachi:ask:tok:q0:o1:zzz", "", 0, 0, false},
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

func TestParseQuestionIdx(t *testing.T) {
	tests := []struct {
		in   string
		want int
		ok   bool
	}{
		{"q0", 0, true},
		{"q1", 1, true},
		{"q42", 42, true},
		{"o3", 3, true},
		{"", 0, false},
		{"q", 0, false},
		{"0", 0, false},
		{"q-1", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseQuestionIdx(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseQuestionIdx(%q) = (%d,%v), want (%d,%v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// ---------------------------------------------------------------------------
// resolveQuestionAnswers
// ---------------------------------------------------------------------------

func TestResolveQuestionAnswers_Button(t *testing.T) {
	questions := []channel.Question{{Options: []channel.QuestionOption{{Label: "A", Description: "x"}, {Label: "B"}}}}
	data := &discordgo.MessageComponentInteractionData{CustomID: "tachi:ask:t:q0:o1"}
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
	data := &discordgo.MessageComponentInteractionData{Values: []string{"B"}}
	answers, _, ok := resolveQuestionAnswers(questions, data, 0, -1)
	if !ok || answers["q0"] != "B" {
		t.Errorf("answers = %v, ok = %v", answers, ok)
	}
}

func TestResolveQuestionAnswers_MultiSelect(t *testing.T) {
	questions := []channel.Question{{MultiSelect: true, Options: []channel.QuestionOption{{Label: "A"}, {Label: "B"}, {Label: "C"}}}}
	data := &discordgo.MessageComponentInteractionData{Values: []string{"A", "C"}}
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
	if _, _, ok := resolveQuestionAnswers(questions, &discordgo.MessageComponentInteractionData{}, 3, -1); ok {
		t.Error("out-of-range question index should fail")
	}
	if _, _, ok := resolveQuestionAnswers(questions, &discordgo.MessageComponentInteractionData{}, 0, 5); ok {
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

func TestQuestionStateRegistry(t *testing.T) {
	ch := &DiscordChannel{
		questionStates: make(map[string]*questionState),
	}

	st := &questionState{token: "tok1", threadID: "guild:1:channel:2", created: time.Now()}
	ch.registerQuestionState(st)

	if got := ch.lookupQuestionState("tok1"); got != st {
		t.Fatalf("lookup returned %v, want %v", got, st)
	}
	if got := ch.lookupQuestionState("nope"); got != nil {
		t.Fatalf("unknown token should return nil, got %v", got)
	}

	// Answered entries are removed.
	st.answered = true
	if got := ch.lookupQuestionState("tok1"); got != nil {
		t.Fatalf("answered entry should be cleaned up, got %v", got)
	}

	// Expired entries are removed.
	ch.registerQuestionState(&questionState{token: "tok2", created: time.Now().Add(-(askStateTTL + time.Minute))})
	if got := ch.lookupQuestionState("tok2"); got != nil {
		t.Fatalf("expired entry should be cleaned up, got %v", got)
	}

	// Unregister removes entries.
	ch.registerQuestionState(&questionState{token: "tok3", created: time.Now()})
	ch.unregisterQuestionState("tok3")
	if got := ch.lookupQuestionState("tok3"); got != nil {
		t.Fatal("unregistered token should be gone")
	}
}

func TestNewQuestionToken_Format(t *testing.T) {
	a := newQuestionToken()
	b := newQuestionToken()
	if a == "" || a == b {
		t.Errorf("tokens should be unique non-empty, got %q and %q", a, b)
	}
	// Token must not contain ':' (it is embedded in a colon-separated CustomID).
	for _, c := range a + b {
		if c == ':' {
			t.Fatalf("token contains ':': %q", a)
		}
	}
}
