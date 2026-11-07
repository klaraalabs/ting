package notifications

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSendValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*SendRequest)
	}{
		{"NUL user", func(r *SendRequest) { r.UserIDs = []string{"a\x00"} }},
		{"NUL data", func(r *SendRequest) { r.Data = map[string]string{"key": "a\x00"} }},
		{"empty recipients", func(r *SendRequest) { r.UserIDs = nil }},
		{"duplicate recipient", func(r *SendRequest) { r.UserIDs = []string{"a", "a"} }},
		{"too many recipients", func(r *SendRequest) { r.UserIDs = make([]string, 1001) }},
		{"empty alert", func(r *SendRequest) { r.Notification.Body = " " }},
		{"reserved aps", func(r *SendRequest) { r.Data = map[string]string{"aps": "x"} }},
		{"reserved gcm", func(r *SendRequest) { r.Data = map[string]string{"gcm": "x"} }},
		{"FCM options overhead", func(r *SendRequest) { r.Notification.Body = strings.Repeat("x", 4020) }},
		{"reserved google", func(r *SendRequest) { r.Data = map[string]string{"google.test": "x"} }},
		{"multibyte payload", func(r *SendRequest) { r.Notification.Body = strings.Repeat("🙂", 1024) }},
		{"escaped payload", func(r *SendRequest) { r.Notification.Body = strings.Repeat("\"", 2048) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := SendRequest{UserIDs: []string{"a"}, Notification: Alert{"Title", "Body"}}
			test.change(&r)
			if r.Validate() == nil {
				t.Fatal("accepted invalid send")
			}
		})
	}
	r := SendRequest{UserIDs: []string{"a", "b"}, Notification: Alert{"Title", "Body"}, Data: map[string]string{"screen": "orders"}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDataMustContainStrings(t *testing.T) {
	for _, raw := range []string{`null`, `{"key":null}`, `{"key":42}`, `{"key":true}`, `[]`} {
		var data StringData
		if err := json.Unmarshal([]byte(raw), &data); err == nil {
			t.Fatalf("accepted non-string data: %s", raw)
		}
	}
	var data StringData
	if err := json.Unmarshal([]byte(`{"empty":"","orderId":"789"}`), &data); err != nil {
		t.Fatal(err)
	}
}
