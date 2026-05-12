package channel

import (
	"context"
	"errors"
	"testing"
)

// ---- Mock for registry tests ----

type mockChannel struct {
	name string
}

func (m *mockChannel) Name() string { return m.name }

func (m *mockChannel) OnStart(ctx context.Context) error { return nil }

func (m *mockChannel) Run(ctx context.Context, handler MessageHandler) error {
	<-ctx.Done()
	return nil
}

// ---- Registry Tests ----

func TestRegisterAndList(t *testing.T) {
	// Clean up from any prior test cruft.
	cleanupRegistry()

	Register("testchan", func(rawCfg map[string]any) (Channel, error) {
		return &mockChannel{name: "testchan"}, nil
	})

	reg := ListRegistered()
	if len(reg) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(reg), reg)
	}
	if _, ok := reg["testchan"]; !ok {
		t.Fatalf("expected 'testchan' in registry, got %v", reg)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	cleanupRegistry()

	Register("dupchan", func(rawCfg map[string]any) (Channel, error) {
		return &mockChannel{name: "dupchan"}, nil
	})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration, got none")
		}
	}()

	Register("dupchan", func(rawCfg map[string]any) (Channel, error) {
		return &mockChannel{name: "dupchan-v2"}, nil
	})
}

func TestRegisterNilFactoryPanics(t *testing.T) {
	cleanupRegistry()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil factory, got none")
		}
	}()

	Register("nilchan", nil)
}

func TestRegisterEmptyNamePanics(t *testing.T) {
	cleanupRegistry()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty name, got none")
		}
	}()

	Register("", func(rawCfg map[string]any) (Channel, error) {
		return nil, errors.New("nope")
	})
}

func TestListRegisteredReturnsCopy(t *testing.T) {
	cleanupRegistry()

	Register("alpha", func(rawCfg map[string]any) (Channel, error) {
		return &mockChannel{name: "alpha"}, nil
	})

	reg := ListRegistered()
	// Mutating the returned map should not affect the internal registry.
	reg["injected"] = nil

	reg2 := ListRegistered()
	if _, ok := reg2["injected"]; ok {
		t.Fatal("ListRegistered should return a copy; mutation leaked through")
	}
}

func cleanupRegistry() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Factory{}
}
