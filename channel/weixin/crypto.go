package weixin

import (
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// --- AES-128-ECB encryption for CDN media ---

// aesECBPaddedSize returns the padded size for AES-128-ECB with PKCS7 padding.
// Note the +1: at least one padding byte is always added (PKCS7).
func aesECBPaddedSize(plaintextSize int) int {
	const blockSize = 16
	return ((plaintextSize + 1 + blockSize - 1) / blockSize) * blockSize
}

// pkcs7Pad adds PKCS7 padding to data for blockSize.
func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - (len(data) % blockSize)
	pad := make([]byte, padding)
	for i := range pad {
		pad[i] = byte(padding)
	}
	return append(data, pad...)
}

// pkcs7Unpad removes PKCS7 padding from data.
func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}
	padding := int(data[len(data)-1])
	if padding <= 0 || padding > aes.BlockSize || padding > len(data) {
		return nil, fmt.Errorf("invalid PKCS7 padding: %d", padding)
	}
	// Verify all padding bytes.
	for i := range padding {
		if data[len(data)-1-i] != byte(padding) {
			return nil, fmt.Errorf("invalid PKCS7 padding sequence")
		}
	}
	return data[:len(data)-padding], nil
}

// encryptAesEcb encrypts plaintext with AES-128-ECB and PKCS7 padding.
func encryptAesEcb(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}

	padded := pkcs7Pad(plaintext, block.BlockSize())
	result := make([]byte, len(padded))

	for i := 0; i < len(padded); i += block.BlockSize() {
		block.Encrypt(result[i:], padded[i:])
	}

	return result, nil
}

// decryptAesEcb decrypts AES-128-ECB ciphertext and removes PKCS7 padding.
func decryptAesEcb(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}

	if len(ciphertext)%block.BlockSize() != 0 {
		return nil, fmt.Errorf("ciphertext length %d is not a multiple of block size %d", len(ciphertext), block.BlockSize())
	}

	result := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += block.BlockSize() {
		block.Decrypt(result[i:], ciphertext[i:])
	}

	return pkcs7Unpad(result)
}

// --- aes_key decoding ---

// decodeAESKey decodes the aes_key field which can be in two formats:
//   1. base64(16-byte-key) → decode → 16 bytes
//   2. base64(hex(16-byte-key)) → decode → 32 bytes ASCII hex → hex decode → 16 bytes
func decodeAESKey(encoded string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("base64 decode aes_key: %w", err)
	}

	switch len(decoded) {
	case 16:
		return decoded, nil
	case 32:
		// Try hex decode: 32 ASCII hex chars → 16 bytes.
		key := make([]byte, 16)
		_, err := hex.Decode(key, decoded)
		if err != nil {
			return nil, fmt.Errorf("aes_key is 32 bytes but not valid hex: %w", err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("unexpected aes_key decoded length: %d", len(decoded))
	}
}

// resolveAESKey picks the best aes key from a media item.
// Prefers the top-level aeskey field (hex) over media.aes_key (base64).
func resolveAESKey(item *MediaItem) ([]byte, error) {
	if item.AESKey != "" {
		// aeskey is hex directly.
		key, err := hex.DecodeString(item.AESKey)
		if err != nil {
			return nil, fmt.Errorf("decode aeskey hex: %w", err)
		}
		if len(key) == 16 {
			return key, nil
		}
	}
	return decodeAESKey(item.Media.AESKey)
}
