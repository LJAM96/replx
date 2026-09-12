// Package crypto holds Replx Edge application-level encryption.
//
// Ciphertext fields (owner JWT, PMS access token, Ed25519 private key) are
// AES-256-GCM encrypted with a key derived per purpose from
// REPLX_EDGE_SECRET_KEY: SHA-256(secret || 0x00 || purpose). Context
// separation means a ciphertext for one purpose never decrypts as another.
//
// Plex user tokens are fingerprinted (HMAC-SHA256 hex) for identity lookup
// so raw tokens never appear in logs, cache keys or database indexes.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// DeriveKey derives a 32-byte purpose-separated key from the root secret.
// The root secret must hold at least 32 characters of entropy (enforced by
// config validation); purpose is a short ASCII label such as "owner-token".
func DeriveKey(rootSecret, purpose string) []byte {
	h := sha256.New()
	h.Write([]byte(rootSecret))
	h.Write([]byte{0x00})
	h.Write([]byte(purpose))
	return h.Sum(nil)
}

// Encrypt encrypts plaintext with a fresh random nonce. Output is
// nonce(12) || ciphertext. Plaintext must exist in memory only for the
// shortest necessary time; callers should zero buffers when practical.
func Encrypt(rootSecret, purpose string, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(DeriveKey(rootSecret, purpose))
	if err != nil {
		return nil, fmt.Errorf("crypto: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: nonce: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// Decrypt reverses Encrypt. A wrong purpose or corrupted input fails closed.
func Decrypt(rootSecret, purpose string, packed []byte) ([]byte, error) {
	block, err := aes.NewCipher(DeriveKey(rootSecret, purpose))
	if err != nil {
		return nil, fmt.Errorf("crypto: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm: %w", err)
	}
	n := gcm.NonceSize()
	if len(packed) < n {
		return nil, fmt.Errorf("crypto: ciphertext too short")
	}
	plain, err := gcm.Open(nil, packed[:n], packed[n:], nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return plain, nil
}

// Fingerprint returns the HMAC-SHA256 hex of a Plex token for identity
// lookup. The raw token is never stored alongside the fingerprint in
// plaintext-indexed columns.
func Fingerprint(rootSecret, token string) string {
	mac := hmac.New(sha256.New, []byte(rootSecret))
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}
