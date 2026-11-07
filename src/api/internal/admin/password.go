package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"unicode/utf8"

	"github.com/klaraalabs/ting/src/api/internal/apierror"
	"golang.org/x/crypto/argon2"
)

// Fixed, versioned parameters also prevent a corrupt stored hash from requesting
// unbounded memory. These meet OWASP's Argon2id baseline.
const hashPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"

var passwordSlots = make(chan struct{}, 2)

func ValidatePassword(password string) error {
	if !utf8.ValidString(password) || len(password) > 512 || utf8.RuneCountInString(password) < 15 || utf8.RuneCountInString(password) > 128 || strings.ContainsRune(password, 0) {
		return apierror.Invalid("Password must contain 15 to 128 characters, at most 512 bytes, without NUL.")
	}
	return nil
}

func passwordKey(ctx context.Context, password string, salt []byte) ([]byte, error) {
	select {
	case passwordSlots <- struct{}{}:
		defer func() { <-passwordSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
	if err := ctx.Err(); err != nil {
		clear(key)
		return nil, err
	}
	return key, nil
}
func hashPassword(ctx context.Context, password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := passwordKey(ctx, password, salt)
	if err != nil {
		return "", err
	}
	defer clear(key)
	return hashPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}
func checkPassword(ctx context.Context, password, encoded string) (bool, error) {
	salt, want := make([]byte, 16), make([]byte, 32)
	valid := false
	if strings.HasPrefix(encoded, hashPrefix) {
		parts := strings.Split(strings.TrimPrefix(encoded, hashPrefix), "$")
		if len(parts) == 2 {
			s, se := base64.RawStdEncoding.DecodeString(parts[0])
			k, ke := base64.RawStdEncoding.DecodeString(parts[1])
			if se == nil && ke == nil && len(s) == 16 && len(k) == 32 {
				salt, want, valid = s, k, true
			}
		}
	}
	// Missing users and corrupt hashes do the same work as a real password check.
	key, err := passwordKey(ctx, password, salt)
	if err != nil {
		return false, err
	}
	defer clear(key)
	match := subtle.ConstantTimeCompare(key, want) == 1
	return valid && match, nil
}
