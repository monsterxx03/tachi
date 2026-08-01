// RingBuf is a fixed-size thread-safe circular byte buffer for capturing
// recent output of a running process.
package container

import (
	"bytes"
	"sync"
)

// RingBuf is a fixed-size circular byte buffer. Writes beyond capacity
// overwrite the oldest data. It is safe for concurrent use.
type RingBuf struct {
	buf    []byte
	pos    int
	filled int // number of times pos wrapped (0 = not yet wrapped)
	mu     sync.Mutex
}

// New creates a RingBuf with the given capacity in bytes.
func NewRingBuf(cap int) *RingBuf {
	return &RingBuf{
		buf: make([]byte, cap),
	}
}

// Write appends p to the buffer. If the buffer is full, oldest data is
// overwritten. Returns len(p), nil — never returns an error.
func (rb *RingBuf) Write(p []byte) (n int, err error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	for _, b := range p {
		rb.buf[rb.pos] = b
		rb.pos++
		if rb.pos >= len(rb.buf) {
			rb.pos = 0
			rb.filled++
		}
	}
	return len(p), nil
}

// String returns the buffer contents in order (oldest first).
// Unwritten trailing slots are trimmed.
func (rb *RingBuf) String() string {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.filled == 0 {
		// Not yet wrapped — return bytes from 0 to pos.
		return string(bytes.TrimRight(rb.buf[:rb.pos], "\x00"))
	}

	// Wrapped at least once — return pos..end + 0..pos.
	var out bytes.Buffer
	out.Write(rb.buf[rb.pos:])
	out.Write(rb.buf[:rb.pos])
	return stringsTrimRightNull(out.Bytes())
}

func stringsTrimRightNull(b []byte) string {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return string(b)
}
