package secrets

import (
	"bytes"
	"testing"
)

func TestVaultBindingAndTampering(t *testing.T) {
	v, _ := New(bytes.Repeat([]byte{1}, 32))
	plain := []byte("private credential")
	first := v.Seal("app_a:identity:key", plain)
	second := v.Seal("app_a:identity:key", plain)
	if bytes.Equal(first, second) {
		t.Fatal("nonce reused")
	}
	got, err := v.Open("app_a:identity:key", first)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip: %v", err)
	}
	for _, context := range []string{"app_b:identity:key", "app_a:apns:key", "app_a:identity:other"} {
		if _, err := v.Open(context, first); err == nil {
			t.Fatal("accepted wrong context")
		}
	}
	first[len(first)-1] ^= 1
	if _, err := v.Open("app_a:identity:key", first); err == nil {
		t.Fatal("accepted tampered ciphertext")
	}
	for _, bad := range [][]byte{nil, {1}, {2, 3, 4}} {
		if _, err := v.Open("x", bad); err == nil {
			t.Fatal("accepted malformed ciphertext")
		}
	}
}
