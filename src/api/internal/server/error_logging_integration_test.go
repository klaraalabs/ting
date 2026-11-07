package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestHTTPFailuresHaveRedactedDiagnostics(t *testing.T) {
	m := managementSetup(t)
	a := m.app(t, true)
	id := uuid.NewString()
	// Both public and management status reads now return a real PostgreSQL error.
	if _, err := m.db.Exec(context.Background(), `ALTER TABLE notifications RENAME TO unavailable_notifications`); err != nil {
		t.Fatal(err)
	}
	for _, management := range []bool{false, true} {
		name, path, pattern := "public", "/v1/notifications/"+id, "GET /v1/notifications/{id}"
		if management {
			name, path, pattern = "management", appPath(a.id)+"/notifications/"+id, "GET /admin/api/v1/apps/{appId}/notifications/{notificationId}"
		}
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			var w *httptest.ResponseRecorder
			if management {
				w = m.request("GET", path+"?secret=secret-query", nil, func(r *http.Request) { r.Header.Set("X-Request-Id", "secret-client-id") })
			} else {
				w = m.fixture.request("GET", path+"?secret=secret-query", a.token, "", "", "", nil)
			}
			expect(t, w, 500)
			requestID := w.Header().Get("X-Request-Id")
			if _, err := uuid.Parse(requestID); err != nil {
				t.Fatal("missing generated request identifier")
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("expected exactly one log event: %v", err)
			}
			if event["requestId"] != requestID || event["route"] != pattern || event["category"] != "database_error" || event["sqlState"] != "42P01" {
				t.Fatalf("incomplete diagnostics: %v", event)
			}
			for _, secret := range []string{"secret-query", "secret-client-id", a.token, m.cookie.Value, a.secret, id, "unavailable_notifications"} {
				if strings.Contains(logs.String()+w.Body.String(), secret) {
					t.Fatal("HTTP error exposed request or database contents")
				}
			}
		})
	}
}
