// Package ai is the AI configuration assistant: users bring their own
// provider key, the assistant proposes definitions as structured JSON, and the server
// validates every proposal with the same rules as hand-written configuration. A
// proposal changes nothing until a person applies it to the draft, and the draft goes
// live only through the normal, explicit publish.
package ai

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// Sealer encrypts provider API keys at rest with AES-256-GCM. The key comes from the
// deployment (AI_ENCRYPTION_KEY), never from the database, so a database dump alone
// does not reveal anyone's provider key. GCM authenticates as well as encrypts: a
// tampered ciphertext fails to open instead of decrypting to garbage.
type Sealer struct {
	aead    cipher.AEAD
	Version int // stored with each ciphertext so the key can be rotated later
}

// NewSealer parses a base64-encoded 32-byte key.
func NewSealer(encoded string) (*Sealer, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("AI_ENCRYPTION_KEY is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("AI_ENCRYPTION_KEY must decode to 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead, Version: 1}, nil
}

// Seal encrypts plaintext with a fresh random nonce. Reusing a nonce with the same
// key would break GCM's security, so the nonce is never derived from the input.
func (s *Sealer) Seal(plaintext string, additional []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return s.aead.Seal(nil, nonce, []byte(plaintext), additional), nonce, nil
}

// Open decrypts what Seal produced. additional must match: binding the user ID means a
// ciphertext copied onto another user's row does not decrypt.
func (s *Sealer) Open(ciphertext, nonce, additional []byte) (string, error) {
	if len(nonce) != s.aead.NonceSize() {
		return "", errors.New("invalid nonce")
	}
	plain, err := s.aead.Open(nil, nonce, ciphertext, additional)
	if err != nil {
		return "", errors.New("the stored key could not be decrypted")
	}
	return string(plain), nil
}
