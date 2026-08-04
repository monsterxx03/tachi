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
	buf     []byte
	pos     int    // next write position
	written int    // total bytes written (never decremented)
	mu      sync.Mutex
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

	n = len(p)
	rb.written += n
	if n == 0 {
		return 0, nil
	}

	if n >= len(rb.buf) {
		// Write as large as the buffer: keep only the trailing bytes.
		copy(rb.buf, p[n-len(rb.buf):])
		rb.pos = 0
		return n, nil
	}

	// Copy up to the end of the ring, wrap and copy the rest.
	first := min(n, len(rb.buf)-rb.pos)
	copy(rb.buf[rb.pos:], p[:first])
	if first < n {
		copy(rb.buf, p[first:])
		rb.pos = n - first
	} else {
		rb.pos += first
		if rb.pos >= len(rb.buf) {
			rb.pos = 0
		}
	}
	return n, nil
}

// Wrapped reports whether the buffer has been overwritten at least once
// (i.e. the earliest written bytes are no longer present).
func (rb *RingBuf) Wrapped() bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.written > len(rb.buf)
}

// Len returns the number of bytes currently retained (0..capacity).
func (rb *RingBuf) Len() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return min(rb.written, len(rb.buf))
}

// String returns the buffer contents in order (oldest first). NUL bytes
// written by the process are preserved — only unwritten tail slots are
// excluded, via the exact written count.
func (rb *RingBuf) String() string {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if rb.written == 0 {
		return ""
	}
	if rb.written <= len(rb.buf) {
		// Never wrapped: bytes 0..written are the exact content.
		return string(rb.buf[:rb.written])
	}

	// Wrapped: the ring holds the most recent len(buf) bytes; pos is the
	// oldest byte.
	var out bytes.Buffer
	out.Write(rb.buf[rb.pos:])
	out.Write(rb.buf[:rb.pos])
	return out.String()
}
