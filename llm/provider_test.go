package llm

import (
	"testing"

	"github.com/monsterxx03/tachi/config"
)

func TestUserAgent_Dev(t *testing.T) {
	oldVersion := Version
	Version = ""
	t.Cleanup(func() { Version = oldVersion })

	ua := userAgent()
	if ua != "tachi/dev" {
		t.Errorf("userAgent() = %q, want %q", ua, "tachi/dev")
	}
}

func TestUserAgent_VersionSet(t *testing.T) {
	oldVersion := Version
	Version = "v1.0.0"
	t.Cleanup(func() { Version = oldVersion })

	ua := userAgent()
	if ua != "tachi/v1.0.0" {
		t.Errorf("userAgent() = %q, want %q", ua, "tachi/v1.0.0")
	}
}

func TestWithSessionID_StoresAndRetrieves(t *testing.T) {
	ctx := t.Context()
	id := "session-12345"

	ctx = WithSessionID(ctx, id)
	got, ok := SessionIDFromCtx(ctx)
	if !ok {
		t.Error("SessionIDFromCtx() = false, want true")
	}
	if got != id {
		t.Errorf("SessionIDFromCtx() = %q, want %q", got, id)
	}
}

func TestSessionIDFromCtx_NotFound(t *testing.T) {
	ctx := t.Context()
	_, ok := SessionIDFromCtx(ctx)
	if ok {
		t.Error("SessionIDFromCtx() on bare context = true, want false")
	}
}

func TestSessionIDFromCtx_EmptyString(t *testing.T) {
	ctx := WithSessionID(t.Context(), "")
	got, ok := SessionIDFromCtx(ctx)
	if !ok {
		t.Error("SessionIDFromCtx() = false, want true (empty string is a valid value)")
	}
	if got != "" {
		t.Errorf("SessionIDFromCtx() = %q, want %q", got, "")
	}
}

func TestNewTool(t *testing.T) {
	tool := NewTool("bash", "Run a bash command",
		map[string]ToolParameterProperty{
			"cmd": {Type: "string", Description: "The command to run"},
		},
		[]string{"cmd"},
	)

	if tool.Name != "bash" {
		t.Errorf("Name = %q, want %q", tool.Name, "bash")
	}
	if tool.Description != "Run a bash command" {
		t.Errorf("Description = %q, want %q", tool.Description, "Run a bash command")
	}
	if tool.Parameters.Type != "object" {
		t.Errorf("Parameters.Type = %q, want %q", tool.Parameters.Type, "object")
	}
	if len(tool.Parameters.Required) != 1 || tool.Parameters.Required[0] != "cmd" {
		t.Errorf("Required = %v, want [cmd]", tool.Parameters.Required)
	}
	prop, ok := tool.Parameters.Properties["cmd"]
	if !ok {
		t.Fatal("missing property 'cmd'")
	}
	if prop.Type != "string" || prop.Description != "The command to run" {
		t.Errorf("cmd property = %+v", prop)
	}
}

func TestNewTool_NoProperties(t *testing.T) {
	tool := NewTool("noop", "Does nothing", nil, nil)
	if tool.Name != "noop" {
		t.Errorf("Name = %q", tool.Name)
	}
	if tool.Parameters.Properties != nil {
		t.Error("expected nil properties")
	}
	if len(tool.Parameters.Required) != 0 {
		t.Error("expected empty required")
	}
}

func TestNewProvider_OpenAI(t *testing.T) {
	p, err := NewProvider(config.ProviderTypeOpenAI, "sk-test", "", "gpt-4o")
	if err != nil {
		t.Fatalf("NewProvider(openai) error: %v", err)
	}
	if p.Name() != config.ProviderTypeOpenAI {
		t.Errorf("Name() = %q, want %q", p.Name(), config.ProviderTypeOpenAI)
	}
}

func TestNewProvider_Anthropic(t *testing.T) {
	p, err := NewProvider(config.ProviderTypeAnthropic, "sk-ant-test", "", "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("NewProvider(anthropic) error: %v", err)
	}
	if p.Name() != config.ProviderTypeAnthropic {
		t.Errorf("Name() = %q, want %q", p.Name(), config.ProviderTypeAnthropic)
	}
}

func TestNewProvider_Unknown(t *testing.T) {
	p, err := NewProvider("unknown", "key", "", "model")
	if err == nil {
		t.Fatal("NewProvider(unknown) expected error, got nil")
	}
	if p != nil {
		t.Errorf("expected nil provider, got %+v", p)
	}
}

func TestNewProvider_OpenAIWithBaseURL(t *testing.T) {
	p, err := NewProvider(config.ProviderTypeOpenAI, "key", "https://api.example.com/v1", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("NewProvider error: %v", err)
	}
	if p.Name() != config.ProviderTypeOpenAI {
		t.Errorf("Name() = %q", p.Name())
	}
}

// TestNewNamedProvider_CarriesConfigName verifies that NewNamedProvider
// attaches the config provider name to the constructed provider AND that the
// name survives the decorator chain (RetryProvider for openai, bare
// instances for anthropic/openai-res) — ProviderName() must resolve it at
// every layer, since usage-ledger wrapping relies on it to group rows by the
// real config name.
func TestNewNamedProvider_CarriesConfigName(t *testing.T) {
	cases := []struct {
		name string
		typ  string
	}{
		{"deepseek-v4-flash", config.ProviderTypeOpenAI},        // wrapped: RetryProvider → OpenAIProvider
		{"deepseek-v4-pro", config.ProviderTypeOpenAIResponses}, // bare instance
		{"anthropic-main", config.ProviderTypeAnthropic},        // bare instance
	}
	for _, c := range cases {
		p, err := NewNamedProvider(c.typ, c.name, "sk-test", "", "some-model")
		if err != nil {
			t.Fatalf("NewNamedProvider(%s) error: %v", c.typ, err)
		}
		if got := p.ProviderName(); got != c.name {
			t.Errorf("%s: ProviderName() = %q, want %q", c.typ, got, c.name)
		}
	}

	// NewProvider (unnamed) yields a provider that reports no config name.
	p, err := NewProvider(config.ProviderTypeOpenAI, "sk-test", "", "some-model")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.ProviderName(); got != "" {
		t.Errorf("NewProvider: ProviderName() = %q, want \"\"", got)
	}
}
