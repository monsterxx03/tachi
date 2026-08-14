package memory

import (
	"testing"

	"github.com/monsterxx03/tachi/config"
)

func TestNew_Unknown(t *testing.T) {
	_, err := New("unknown_backend", config.MemoryConfig{}, nil)
	if err == nil {
		t.Fatal("New(unknown): expected error, got nil")
	}
}

func TestNew_Empty(t *testing.T) {
	_, err := New("", config.MemoryConfig{}, nil)
	if err == nil {
		t.Fatal("New(''): expected error, got nil")
	}
}
