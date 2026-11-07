package admin

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPasswordHash(t *testing.T) {
	ctx := context.Background()
	password := "a long operator passphrase"
	first, err := hashPassword(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashPassword(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, hashPrefix) || strings.Contains(first, password) {
		t.Fatal("password not salted/hashed")
	}
	for _, tc := range []struct {
		password, hash string
		want           bool
	}{{password, first, true}, {"a different long password", first, false}, {password, "", false}, {password, "$argon2id$v=19$m=999999999,t=100,p=8$bad$bad", false}, {password, hashPrefix + "bad$bad", false}} {
		got, err := checkPassword(ctx, tc.password, tc.hash)
		if err != nil || got != tc.want {
			t.Fatalf("password verification: got %v, %v", got, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := hashPassword(canceled, password); err == nil {
		t.Fatal("ignored canceled password operation")
	}
	for _, value := range []string{"short", strings.Repeat("a", 129), "bad\x00password that is long", string([]byte{0xff})} {
		if ValidatePassword(value) == nil {
			t.Fatal("invalid password accepted")
		}
	}
	if ValidatePassword(strings.Repeat("界", 15)) != nil {
		t.Fatal("unicode passphrase rejected")
	}
}
func TestAdminConfig(t *testing.T) {
	c := DefaultConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"http://localhost:8080", "https://example.org/", "https://example.org/path", "https://user:password@example.org", "https://example.org?", "https://example.org#fragment", "null", "https://"} {
		c := DefaultConfig()
		c.Origin = origin
		if c.Validate() == nil {
			t.Fatalf("accepted origin %q", origin)
		}
	}
	c.SessionTTL = 25 * time.Hour
	if c.Validate() == nil {
		t.Fatal("accepted excessive TTL")
	}
}
