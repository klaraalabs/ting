// Package secrets protects persisted credentials and generates opaque bearer tokens.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

type Vault struct{ aead cipher.AEAD }

func New(key []byte) (*Vault, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption key must contain 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead}, nil
}

// Context binds ciphertext to its App, purpose and record. Version 1 is also authenticated.
func (v *Vault) Seal(context string, plaintext []byte) []byte {
	nonce := make([]byte, v.aead.NonceSize())
	rand.Read(nonce)
	result := append([]byte{1}, nonce...)
	return v.aead.Seal(result, nonce, plaintext, []byte("ting:v1:"+context))
}
func (v *Vault) Open(context string, encrypted []byte) ([]byte, error) {
	n := v.aead.NonceSize()
	if len(encrypted) < 1+n+v.aead.Overhead() || encrypted[0] != 1 {
		return nil, errors.New("invalid encrypted credential")
	}
	return v.aead.Open(nil, encrypted[1:1+n], encrypted[1+n:], []byte("ting:v1:"+context))
}
func Token(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}
func AppID() string            { b := make([]byte, 16); rand.Read(b); return "app_" + hex.EncodeToString(b) }
func Hash(value string) []byte { h := sha256.Sum256([]byte(value)); return h[:] }
