package ringbuf

import (
	"strings"
	"testing"
)

func TestBuffer_Basic(t *testing.T) {
	b := New(64)
	b.Write([]byte("hello"))

	s := b.String()
	if s != "hello" {
		t.Errorf("expected 'hello', got %q", s)
	}
}

func TestBuffer_Wrap(t *testing.T) {
	b := New(5) // small buffer to force wrapping
	b.Write([]byte("abcde"))
	b.Write([]byte("fgh"))

	// After "abcde" + "fgh" in a 5-byte buffer, oldest 3 bytes ("abc") are
	// overwritten. The remaining content should be "defgh" (d, e from first
	// write + f, g, h from second).
	s := b.String()
	// pos is at 3, filled>0, so output = buf[3:] + buf[:3] = "de" + "fgh" = "defgh"
	if s != "defgh" {
		t.Errorf("expected 'defgh', got %q", s)
	}
}

func TestBuffer_MultiWrap(t *testing.T) {
	b := New(4)
	b.Write([]byte("1234567890")) // 10 bytes into 4-byte buffer
	s := b.String()
	// Only last 4 bytes survive: "7890"
	if s != "7890" {
		t.Errorf("expected '7890', got %q", s)
	}

	// Write more
	b.Write([]byte("ab"))
	// "90ab" — wraps from position after 0
	if s2 := b.String(); s2 != "90ab" {
		t.Errorf("expected '90ab', got %q", s2)
	}
}

func TestBuffer_Empty(t *testing.T) {
	b := New(64)
	if s := b.String(); s != "" {
		t.Errorf("expected empty, got %q", s)
	}
}

func TestBuffer_LargeWrite(t *testing.T) {
	b := New(10)
	data := strings.Repeat("x", 100)
	b.Write([]byte(data))

	s := b.String()
	if len(s) != 10 {
		t.Errorf("expected length 10, got %d", len(s))
	}
	if s != strings.Repeat("x", 10) {
		t.Errorf("expected 10 x's, got %q", s)
	}
}

func TestBuffer_Concurrent(t *testing.T) {
	b := New(1024)
	done := make(chan struct{})
	for range 4 {
		go func() {
			for range 100 {
				b.Write([]byte("concurrent test data "))
			}
			done <- struct{}{}
		}()
	}
	for range 4 {
		<-done
	}
	// Should not panic or data-race
	_ = b.String()
}

func TestBuffer_WriteReturns(t *testing.T) {
	b := New(10)
	n, err := b.Write([]byte("hello"))
	if n != 5 {
		t.Errorf("expected n=5, got %d", n)
	}
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBuffer_ResetBehavior(t *testing.T) {
	b := New(8)
	b.Write([]byte("abcdefgh")) // exactly fills buffer
	s := b.String()
	if s != "abcdefgh" {
		t.Errorf("expected 'abcdefgh', got %q", s)
	}

	// One more byte wraps
	b.Write([]byte("X"))
	// buf capacity 8, filled wraps now pos=1, filled>0
	// output = buf[1:] + buf[:1] = "bcdefgh" + "X" = "bcdefghX"
	s = b.String()
	if s != "bcdefghX" {
		t.Errorf("expected 'bcdefghX', got %q", s)
	}
}