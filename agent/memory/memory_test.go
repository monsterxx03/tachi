package memory

import (
	"testing"
	"time"
)

func TestNew_ValidMem9(t *testing.T) {
	cfg := Config{
		Mem9: Mem9Config{
			APIKey: "test-key",
		},
	}
	b, err := New("mem9", cfg)
	if err != nil {
		t.Fatalf("New(mem9): unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("New(mem9): expected non-nil backend")
	}
}

func TestNew_Unknown(t *testing.T) {
	_, err := New("unknown_backend", Config{})
	if err == nil {
		t.Fatal("New(unknown): expected error, got nil")
	}
}

func TestNew_Empty(t *testing.T) {
	_, err := New("", Config{})
	if err == nil {
		t.Fatal("New(''): expected error, got nil")
	}
}

func TestNew_DefaultTimeouts(t *testing.T) {
	cfg := Config{
		Mem9: Mem9Config{
			APIKey: "test-key",
		},
	}
	// Timeout and Mem9.RequestTimeout are both 0, should get defaults.
	b, err := New("mem9", cfg)
	if err != nil {
		t.Fatalf("New(mem9): unexpected error: %v", err)
	}
	mb, ok := b.(*Mem9Backend)
	if !ok {
		t.Fatal("expected *Mem9Backend")
	}
	if mb.requestTimeout != 15*time.Second {
		t.Errorf("requestTimeout = %v, want 15s", mb.requestTimeout)
	}
}

func TestNew_CustomTimeouts(t *testing.T) {
	cfg := Config{
		Timeout: 5 * time.Second,
		Mem9: Mem9Config{
			APIKey:         "test-key",
			RequestTimeout: 20 * time.Second,
		},
	}
	b, err := New("mem9", cfg)
	if err != nil {
		t.Fatalf("New(mem9): unexpected error: %v", err)
	}
	mb, ok := b.(*Mem9Backend)
	if !ok {
		t.Fatal("expected *Mem9Backend")
	}
	if mb.requestTimeout != 20*time.Second {
		t.Errorf("requestTimeout = %v, want 20s", mb.requestTimeout)
	}
}

func TestNew_DefaultBaseURL(t *testing.T) {
	cfg := Config{
		Mem9: Mem9Config{
			APIKey: "test-key",
		},
	}
	b, err := New("mem9", cfg)
	if err != nil {
		t.Fatalf("New(mem9): unexpected error: %v", err)
	}
	mb := b.(*Mem9Backend)
	if mb.baseURL != "https://api.mem9.ai" {
		t.Errorf("baseURL = %q, want %q", mb.baseURL, "https://api.mem9.ai")
	}
	if mb.agentID != "tachi" {
		t.Errorf("agentID = %q, want %q", mb.agentID, "tachi")
	}
	if mb.mode != "smart" {
		t.Errorf("mode = %q, want %q", mb.mode, "smart")
	}
}

func TestNew_CustomBaseURL(t *testing.T) {
	cfg := Config{
		Mem9: Mem9Config{
			APIKey:  "test-key",
			APIURL:  "https://custom.mem9.example.com/",
			AgentID: "my-agent",
			Mode:    "raw",
		},
	}
	b, err := New("mem9", cfg)
	if err != nil {
		t.Fatalf("New(mem9): unexpected error: %v", err)
	}
	mb := b.(*Mem9Backend)
	if mb.baseURL != "https://custom.mem9.example.com" {
		t.Errorf("baseURL = %q, want %q", mb.baseURL, "https://custom.mem9.example.com")
	}
	if mb.agentID != "my-agent" {
		t.Errorf("agentID = %q, want %q", mb.agentID, "my-agent")
	}
	if mb.mode != "raw" {
		t.Errorf("mode = %q, want %q", mb.mode, "raw")
	}
}
