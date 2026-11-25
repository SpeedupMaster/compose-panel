// Package secret encrypts sensitive fields with AES-256-GCM using a master key file.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

const prefix = "v1:"

type Box struct{ aead cipher.AEAD }

// LoadOrCreate reads a hex encoded 32-byte key from path, creating it if missing.
func LoadOrCreate(path string) (*Box, error) {
	raw, err := os.ReadFile(path)
	var key []byte
	switch {
	case errors.Is(err, os.ErrNotExist):
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("write master key: %w", err)
		}
	case err != nil:
		return nil, err
	default:
		key, err = hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("invalid master key file %s", path)
		}
	}
	return New(key)
}

func New(key []byte) (*Box, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts s. The empty string stays empty.
func (b *Box) Seal(s string) string {
	if s == "" {
		return ""
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return prefix + base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(s), nil))
}

// Open decrypts a value produced by Seal.
func (b *Box) Open(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !strings.HasPrefix(s, prefix) {
		return "", errors.New("value is not encrypted")
	}
	data, err := base64.StdEncoding.DecodeString(s[len(prefix):])
	if err != nil {
		return "", err
	}
	n := b.aead.NonceSize()
	if len(data) < n {
		return "", errors.New("ciphertext too short")
	}
	plain, err := b.aead.Open(nil, data[:n], data[n:], nil)
	if err != nil {
		return "", errors.New("decrypt failed (wrong master key?)")
	}
	return string(plain), nil
}

// MustOpen returns "" when decryption fails.
func (b *Box) MustOpen(s string) string {
	v, err := b.Open(s)
	if err != nil && strings.HasPrefix(s, prefix) {
		slog.Warn("secret decrypt failed", "err", err)
	}
	return v
}
