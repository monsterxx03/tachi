package weixin

import (
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestEncryptDecryptAesEcb(t *testing.T) {
	key := make([]byte, 16) // zero key for test
	for i := range key {
		key[i] = byte(i)
	}

	plaintext := []byte("Hello, World! This is a test message for AES-ECB.")

	ciphertext, err := encryptAesEcb(plaintext, key)
	if err != nil {
		t.Fatalf("encryptAesEcb: %v", err)
	}

	if len(ciphertext)%aes.BlockSize != 0 {
		t.Errorf("ciphertext length %d is not a multiple of block size", len(ciphertext))
	}

	decrypted, err := decryptAesEcb(ciphertext, key)
	if err != nil {
		t.Fatalf("decryptAesEcb: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("round-trip failed: got %q, want %q", decrypted, plaintext)
	}
}

func TestEncryptDecryptAesEcb_ExactBlock(t *testing.T) {
	key := make([]byte, 16)
	plaintext := make([]byte, 16*3) // exact multiple of block size

	ciphertext, err := encryptAesEcb(plaintext, key)
	if err != nil {
		t.Fatalf("encryptAesEcb: %v", err)
	}

	// PKCS7 always adds padding, so ciphertext should be > plaintext.
	if len(ciphertext) <= len(plaintext) {
		t.Errorf("ciphertext length %d <= plaintext length %d (expected padding)", len(ciphertext), len(plaintext))
	}

	decrypted, err := decryptAesEcb(ciphertext, key)
	if err != nil {
		t.Fatalf("decryptAesEcb: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("round-trip failed for exact block: len=%d", len(plaintext))
	}
}

func TestEncryptDecryptAesEcb_SingleByte(t *testing.T) {
	key := make([]byte, 16)
	plaintext := []byte("A")

	ciphertext, err := encryptAesEcb(plaintext, key)
	if err != nil {
		t.Fatalf("encryptAesEcb: %v", err)
	}

	if len(ciphertext) != aes.BlockSize {
		t.Errorf("expected ciphertext length %d, got %d", aes.BlockSize, len(ciphertext))
	}

	decrypted, err := decryptAesEcb(ciphertext, key)
	if err != nil {
		t.Fatalf("decryptAesEcb: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Errorf("single byte round-trip failed: got %q, want %q", decrypted, plaintext)
	}
}

func TestAesECBPaddedSize(t *testing.T) {
	tests := []struct {
		plainSize int
		want      int
	}{
		{0, 16},    // 0+1=1, ceil(1/16)*16=16
		{1, 16},    // 1+1=2, ceil(2/16)*16=16
		{15, 16},   // 15+1=16, ceil(16/16)*16=16
		{16, 32},   // 16+1=17, ceil(17/16)*16=32
		{31, 32},   // 31+1=32, ceil(32/16)*16=32
		{32, 48},   // 32+1=33, ceil(33/16)*16=48
		{100, 112}, // 100+1=101, ceil(101/16)*16=112
	}

	for _, tt := range tests {
		got := aesECBPaddedSize(tt.plainSize)
		if got != tt.want {
			t.Errorf("aesECBPaddedSize(%d) = %d, want %d", tt.plainSize, got, tt.want)
		}
	}
}

func TestDecodeAESKey_DirectBase64(t *testing.T) {
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i + 1)
	}

	encoded := base64.StdEncoding.EncodeToString(key)

	decoded, err := decodeAESKey(encoded)
	if err != nil {
		t.Fatalf("decodeAESKey: %v", err)
	}

	if !bytes.Equal(decoded, key) {
		t.Errorf("direct base64: got %x, want %x", decoded, key)
	}
}

func TestDecodeAESKey_HexThenBase64(t *testing.T) {
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i + 1)
	}

	hexStr := hex.EncodeToString(key) // 32 hex chars
	encoded := base64.StdEncoding.EncodeToString([]byte(hexStr))

	decoded, err := decodeAESKey(encoded)
	if err != nil {
		t.Fatalf("decodeAESKey: %v", err)
	}

	if !bytes.Equal(decoded, key) {
		t.Errorf("hex+base64: got %x, want %x", decoded, key)
	}
}

func TestDecodeAESKey_InvalidLength(t *testing.T) {
	// 8 bytes → unexpected length.
	encoded := base64.StdEncoding.EncodeToString([]byte("12345678"))
	_, err := decodeAESKey(encoded)
	if err == nil {
		t.Error("expected error for unexpected length")
	}
}

func TestDecodeAESKey_NotHex(t *testing.T) {
	// 32 bytes of non-hex → hex decode fails.
	data := make([]byte, 32)
	for i := range data {
		data[i] = 'Z'
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	_, err := decodeAESKey(encoded)
	if err == nil {
		t.Error("expected error for non-hex 32-byte value")
	}
}

func TestResolveAESKey_PrefersAESKeyField(t *testing.T) {
	key := hex.EncodeToString([]byte("abcdefghijklmnop")) // 16 bytes → 32 hex chars
	item := &MediaItem{
		AESKey: key,
		Media: MediaData{
			AESKey: base64.StdEncoding.EncodeToString([]byte("zzzzzzzzzzzzzzzz")),
		},
	}

	resolved, err := resolveAESKey(item)
	if err != nil {
		t.Fatalf("resolveAESKey: %v", err)
	}

	expected, _ := hex.DecodeString(key)
	if !bytes.Equal(resolved, expected) {
		t.Errorf("resolveAESKey: got %x, want %x", resolved, expected)
	}
}

func TestResolveAESKey_FallsBackToMedia(t *testing.T) {
	key := []byte("abcdefghijklmnop")
	item := &MediaItem{
		AESKey: "", // empty
		Media: MediaData{
			AESKey: base64.StdEncoding.EncodeToString(key),
		},
	}

	resolved, err := resolveAESKey(item)
	if err != nil {
		t.Fatalf("resolveAESKey: %v", err)
	}

	if !bytes.Equal(resolved, key) {
		t.Errorf("resolveAESKey: got %x, want %x", resolved, key)
	}
}
