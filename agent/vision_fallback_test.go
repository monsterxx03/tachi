package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/monsterxx03/tachi/config"
	"github.com/monsterxx03/tachi/llm"
	"github.com/monsterxx03/tachi/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// visionMockProvider is a config-name-carrying provider stub whose
// CreateChat records the last description request and returns a canned
// description (or error).
type visionMockProvider struct {
	name     string
	model    string
	desc     string
	err      error
	calls    int
	lastText string // the prompt text sent alongside the image
	lastImg  *llm.ContentPart
}

func (p *visionMockProvider) Name() string         { return "mock" }
func (p *visionMockProvider) ProviderName() string { return p.name }
func (p *visionMockProvider) Model() string        { return p.model }
func (p *visionMockProvider) CreateChat(_ context.Context, messages []llm.Message, _ []llm.Tool, _ llm.ChatOptions) (*llm.Response, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	for _, part := range messages[0].ContentParts {
		if part.Type == llm.ContentPartImage {
			cp := part
			p.lastImg = &cp
		} else {
			p.lastText = part.Text
		}
	}
	return &llm.Response{Content: p.desc}, nil
}
func (p *visionMockProvider) CreateChatStream(context.Context, []llm.Message, []llm.Tool, llm.ChatOptions) (<-chan llm.StreamEvent, error) {
	return nil, errors.New("not implemented")
}

// newVisionTestAgent builds a bare agent pinned to a text-only main provider
// with an optional delegate override. Logger is wired so fallback warnings
// are exercisable without touching the user's log dir (discarded when
// logger.Init is not called in tests).
func newVisionTestAgent(t *testing.T, delegate llm.Provider) *AIAgent {
	t.Helper()
	main := &visionMockProvider{name: "deepseek", model: "deepseek-v4-flash"}
	a := newBareTestAgent(t, main, 10)
	a.Config.Resolved.Name = "deepseek"
	a.Config.Resolved.Model = "deepseek-v4-flash"
	a.Config.Resolved.SupportsVision = false
	a.visionDelegateOverride = delegate
	a.SetLogger(logger.New("vision-test"))
	return a
}

func imgPart(data string) llm.ContentPart {
	return llm.ContentPart{Type: llm.ContentPartImage, MediaType: "image/png", Data: data}
}

func userMsgWithImg(text, data string) llm.Message {
	return llm.Message{
		Role:    "user",
		Content: text,
		ContentParts: []llm.ContentPart{
			{Type: llm.ContentPartText, Text: text},
			imgPart(data),
		},
	}
}

func TestDescribeImagesIfNeeded_VisionCapableModel_Noop(t *testing.T) {
	delegate := &visionMockProvider{name: "gpt5", desc: "x"}
	a := newVisionTestAgent(t, delegate)
	a.Config.Resolved.SupportsVision = true // current model sees images natively

	rs := &RunState{Messages: []llm.Message{userMsgWithImg("这是什么", "AAAA")}}
	a.describeImagesIfNeeded(context.Background(), rs, &llm.ChatOptions{})

	assert.Equal(t, llm.ContentPartImage, rs.Messages[0].ContentParts[1].Type, "image parts must stay untouched")
	assert.Zero(t, delegate.calls, "delegate must not be called when the model supports vision")
}

func TestDescribeImagesIfNeeded_UserImage_ReplacedWithDescription(t *testing.T) {
	delegate := &visionMockProvider{name: "gpt5", model: "gpt-5.2", desc: "一只猫趴在键盘上"}
	a := newVisionTestAgent(t, delegate)

	rs := &RunState{Messages: []llm.Message{userMsgWithImg("这张图里是什么", "AAAA")}}
	a.describeImagesIfNeeded(context.Background(), rs, &llm.ChatOptions{SessionID: "sess-1"})

	parts := rs.Messages[0].ContentParts
	require.Len(t, parts, 2)
	assert.Equal(t, llm.ContentPartText, parts[0].Type)
	assert.Equal(t, "这张图里是什么", parts[0].Text)
	assert.Equal(t, llm.ContentPartText, parts[1].Type, "image part must become a text part")
	assert.Contains(t, parts[1].Text, imageDescriptionLabel)
	assert.Contains(t, parts[1].Text, "一只猫趴在键盘上")

	assert.Equal(t, 1, delegate.calls)
	require.NotNil(t, delegate.lastImg)
	assert.Equal(t, "image/png", delegate.lastImg.MediaType)
	assert.Equal(t, "AAAA", delegate.lastImg.Data)
	assert.Contains(t, delegate.lastText, "这张图里是什么", "prompt must carry the message text as context")
}

func TestDescribeImagesIfNeeded_ToolImage_MergedIntoContent(t *testing.T) {
	delegate := &visionMockProvider{name: "gpt5", desc: "一张折线图，销售额上升"}
	a := newVisionTestAgent(t, delegate)

	rs := &RunState{Messages: []llm.Message{{
		Role:         "tool",
		Content:      "ReadFile: /tmp/chart.png",
		ContentParts: []llm.ContentPart{imgPart("BBBB")},
	}}}
	a.describeImagesIfNeeded(context.Background(), rs, &llm.ChatOptions{})

	msg := rs.Messages[0]
	assert.Contains(t, msg.Content, "ReadFile: /tmp/chart.png")
	assert.Contains(t, msg.Content, imageDescriptionLabel)
	assert.Contains(t, msg.Content, "一张折线图，销售额上升")
	assert.Empty(t, msg.ContentParts, "tool-role images are merged into Content (every wire protocol rejects tool image arrays)")
	assert.Equal(t, 1, delegate.calls)
}

func TestDescribeImagesIfNeeded_NoDelegate_DegradesToPlaceholder(t *testing.T) {
	a := newVisionTestAgent(t, nil) // no override, no FullConfig → delegate build fails

	rs := &RunState{Messages: []llm.Message{userMsgWithImg("看这个", "AAAA")}}
	a.describeImagesIfNeeded(context.Background(), rs, &llm.ChatOptions{})

	parts := rs.Messages[0].ContentParts
	require.Len(t, parts, 2)
	assert.Equal(t, llm.ContentPartText, parts[1].Type, "image degrades to text, never passed to a text-only model")
	assert.Contains(t, parts[1].Text, "无法描述")
}

func TestDescribeImagesIfNeeded_DuplicateImages_DescribedOnce(t *testing.T) {
	delegate := &visionMockProvider{name: "gpt5", desc: "同一张截图"}
	a := newVisionTestAgent(t, delegate)

	rs := &RunState{Messages: []llm.Message{
		userMsgWithImg("a", "SAME"),
		{Role: "user", Content: "b", ContentParts: []llm.ContentPart{imgPart("SAME")}},
	}}
	a.describeImagesIfNeeded(context.Background(), rs, &llm.ChatOptions{})

	assert.Equal(t, 1, delegate.calls, "identical images are described once per turn")
	for i, msg := range rs.Messages {
		require.Len(t, msg.ContentParts, 2-i, "first message keeps its text part; second has only the description")
		last := msg.ContentParts[len(msg.ContentParts)-1]
		assert.Equal(t, llm.ContentPartText, last.Type)
		assert.Contains(t, last.Text, "同一张截图")
	}
}

func TestDescribeImagesIfNeeded_DelegateError_UsesPlaceholder(t *testing.T) {
	delegate := &visionMockProvider{name: "gpt5", err: errors.New("rate limited")}
	a := newVisionTestAgent(t, delegate)

	rs := &RunState{Messages: []llm.Message{
		{Role: "user", ContentParts: []llm.ContentPart{imgPart("AAAA")}},
		{Role: "user", ContentParts: []llm.ContentPart{imgPart("BBBB")}},
	}}
	a.describeImagesIfNeeded(context.Background(), rs, &llm.ChatOptions{})

	assert.Equal(t, 2, delegate.calls)
	for _, msg := range rs.Messages {
		require.Len(t, msg.ContentParts, 1)
		assert.Contains(t, msg.ContentParts[0].Text, "描述失败")
	}
}

func TestBuildVisionDelegate_PicksFirstVisionProvider(t *testing.T) {
	a := newVisionTestAgent(t, nil)
	a.Config.FullConfig = &config.Config{
		Provider: "deepseek",
		Providers: []config.ProviderConfig{
			{Name: "deepseek", Type: config.ProviderTypeOpenAI, Model: "deepseek-v4-flash", APIKey: "sk-d"},
			{Name: "gpt5", Type: config.ProviderTypeOpenAI, Model: "gpt-5.2", APIKey: "sk-g"},
			{Name: "claude", Type: config.ProviderTypeAnthropic, Model: "claude-sonnet-4-6", APIKey: "sk-c"},
		},
	}
	a.Config.Resolved.Name = "deepseek"

	delegate, err := a.buildVisionDelegate()
	require.NoError(t, err)
	require.NotNil(t, delegate)
	assert.Equal(t, "gpt5", visionDelegateName(delegate), "first vision-capable provider other than the current one wins")
}

func TestBuildVisionDelegate_NoVisionProvider_ReturnsError(t *testing.T) {
	a := newVisionTestAgent(t, nil)
	a.Config.FullConfig = &config.Config{
		Provider: "deepseek",
		Providers: []config.ProviderConfig{
			{Name: "deepseek", Type: config.ProviderTypeOpenAI, Model: "deepseek-v4-flash", APIKey: "sk-d"},
			{Name: "glm4", Type: config.ProviderTypeOpenAI, Model: "glm-4", APIKey: "sk-g"},
		},
	}
	a.Config.Resolved.Name = "deepseek"

	delegate, err := a.buildVisionDelegate()
	require.Error(t, err)
	assert.Nil(t, delegate)
	assert.True(t, strings.Contains(err.Error(), "no configured provider supports images"))
}

func TestBuildVisionDelegate_ExplicitVisionFlag(t *testing.T) {
	a := newVisionTestAgent(t, nil)
	a.Config.FullConfig = &config.Config{
		Provider: "deepseek",
		Providers: []config.ProviderConfig{
			{Name: "deepseek", Type: config.ProviderTypeOpenAI, Model: "deepseek-v4-flash", APIKey: "sk-d"},
			// 显式 vision: true 即使模型名不命中能力表也可作为描述 provider
			{Name: "custom-vl", Type: config.ProviderTypeOpenAI, Model: "my-private-vl", APIKey: "sk-c", Spec: config.ModelSpec{Vision: boolPtr2(true)}},
		},
	}
	a.Config.Resolved.Name = "deepseek"

	delegate, err := a.buildVisionDelegate()
	require.NoError(t, err)
	assert.Equal(t, "custom-vl", visionDelegateName(delegate))
}

func boolPtr2(b bool) *bool { return &b }
