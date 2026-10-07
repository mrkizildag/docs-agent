package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
)

// seal encrypts tokens with AES-256-GCM, binding the blob to aad (the session's
// id hash) so it cannot be moved to another session row.
func seal(key [32]byte, aad []byte, tokens Tokens) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(tokens)
	if err != nil {
		return nil, fmt.Errorf("encode tokens: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return gcm.Seal(nonce, nonce, plain, aad), nil
}

func open(key [32]byte, aad, sealed []byte) (Tokens, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return Tokens{}, err
	}
	if len(sealed) < gcm.NonceSize() {
		return Tokens{}, errors.New("open tokens: blob too short")
	}
	nonce, ciphertext := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return Tokens{}, fmt.Errorf("open tokens: %w", err)
	}
	var tokens Tokens
	if err := json.Unmarshal(plain, &tokens); err != nil {
		return Tokens{}, fmt.Errorf("decode tokens: %w", err)
	}
	return tokens, nil
}

func newGCM(key [32]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return gcm, nil
}
