package weixin

import (
	"crypto/rand"
)

// readRandom reads len(b) random bytes from crypto/rand into b.
func readRandom(b []byte) {
	if _, err := rand.Read(b); err != nil {
		// Fallback: never fails in practice; panic to catch programming errors.
		panic("crypto/rand.Read failed: " + err.Error())
	}
}
