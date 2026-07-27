package memory

import (
	"testing"
)

func TestNew_Unknown(t *testing.T) {
	_, err := New("unknown_backend", Config{}, nil)
	if err == nil {
		t.Fatal("New(unknown): expected error, got nil")
	}
}

func TestNew_Empty(t *testing.T) {
	_, err := New("", Config{}, nil)
	if err == nil {
		t.Fatal("New(''): expected error, got nil")
	}
}
