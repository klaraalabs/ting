package apierror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestUnexpectedErrorDiagnosticsAreRedacted(t *testing.T) {
	for _, test := range []struct {
		name               string
		err                error
		status             int
		category, sqlState string
	}{
		{"database", fmt.Errorf("secret wrapper: %w", &pgconn.PgError{Code: "23505", Message: "secret message", Detail: "secret credential", TableName: "secret table"}), 500, "database_error", "23505"},
		{"invalid SQLSTATE", &pgconn.PgError{Code: "secret", Message: "secret message"}, 500, "database_error", ""},
		{"decryption", errors.New("secret decryption failure"), 500, "internal_error", ""},
		{"deadline", fmt.Errorf("secret: %w", context.DeadlineExceeded), 504, "deadline_exceeded", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			r := httptest.NewRequest("GET", "https://ting.test/secret?token=secret", strings.NewReader("secret body"))
			r.Header.Set("Authorization", "Bearer secret")
			r.Pattern = "GET /v1/notifications/{id}"
			w := httptest.NewRecorder()
			w.Header().Set("X-Request-Id", "generated-request-id")
			Write(w, r, test.err)
			if w.Code != test.status {
				t.Fatalf("status=%d", w.Code)
			}
			if strings.Contains(logs.String()+w.Body.String(), "secret") {
				t.Fatal("diagnostics leaked error or request contents")
			}
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatalf("expected exactly one structured event: %v", err)
			}
			if event["requestId"] != "generated-request-id" || event["route"] != r.Pattern || event["category"] != test.category || event["status"] != float64(test.status) {
				t.Fatalf("missing safe error context: %v", event)
			}
			if got, _ := event["sqlState"].(string); got != test.sqlState {
				t.Fatalf("SQLSTATE=%q", got)
			}
		})
	}
}

func TestExpectedErrorsDoNotLog(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	w := httptest.NewRecorder()
	Write(w, httptest.NewRequest("GET", "https://ting.test/", nil), Unauthorized)
	if w.Code != 401 || logs.Len() != 0 {
		t.Fatal("expected authentication error logged or changed status")
	}
}
