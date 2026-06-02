package hashline

import (
	"crypto/sha256"
	"encoding/hex"
)

// ComputeTag computes a 4-character hex hash tag from file content.
// Uses the first 16 bits (2 bytes) of SHA-256: h[0] and h[1].
// 4 hex chars = 65536 possible values, sufficient for session-scoped versioning.
func ComputeTag(content string) string {
	return computeTagBytes(content, 2)
}

// computeTagBytes computes an N-byte hex hash tag from file content.
// Returns a hex string of length 2*numBytes (e.g., numBytes=2 → "a1f0").
func computeTagBytes(content string, numBytes int) string {
	h := sha256.Sum256([]byte(content))
	if numBytes > 32 {
		numBytes = 32
	}
	return hex.EncodeToString(h[:numBytes])
}
