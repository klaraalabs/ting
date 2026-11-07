package apierror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

type Error struct {
	Status     int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	RetryAfter int    `json:"-"`
}

func (e *Error) Error() string { return e.Message }
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}
func Retry(status int, code, message string, seconds int) *Error {
	return &Error{Status: status, Code: code, Message: message, RetryAfter: seconds}
}

var Unauthorized = New(401, "unauthorized", "Valid credentials are required.")
var NotFound = New(404, "not_found", "Resource not found.")

func Invalid(message string) *Error { return New(400, "invalid_request", message) }
func Write(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = New(500, "internal_error", "The request could not be completed.")
		if errors.Is(err, context.DeadlineExceeded) {
			e = New(504, "request_timeout", "The request deadline was exceeded.")
		}
		category := "internal_error"
		var sqlState string
		var databaseError *pgconn.PgError
		if errors.As(err, &databaseError) {
			category = "database_error"
			// SQLSTATE is a fixed-format code. Never log Message, Detail, or the
			// error string, which can contain request data or stored credentials.
			if validSQLState(databaseError.Code) {
				sqlState = databaseError.Code
			}
		} else if errors.Is(err, context.DeadlineExceeded) {
			category = "deadline_exceeded"
		} else if errors.Is(err, context.Canceled) {
			category = "request_canceled"
		}
		attrs := []any{"requestId", w.Header().Get("X-Request-Id"), "route", r.Pattern, "status", e.Status, "category", category}
		if sqlState != "" {
			attrs = append(attrs, "sqlState", sqlState)
		}
		slog.ErrorContext(r.Context(), "HTTP request failed", attrs...)
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	JSON(w, e.Status, struct {
		Error *Error `json:"error"`
	}{e})
}

func validSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for _, c := range code {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
func JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func Decode(w http.ResponseWriter, r *http.Request, value any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return New(415, "unsupported_media_type", "Content-Type must be application/json.")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return context.DeadlineExceeded
		}
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			return New(413, "payload_too_large", fmt.Sprintf("Request body exceeds %d bytes.", large.Limit))
		}
		return Invalid("Malformed JSON or unknown fields.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return context.DeadlineExceeded
		}
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			return New(413, "payload_too_large", fmt.Sprintf("Request body exceeds %d bytes.", large.Limit))
		}
		return Invalid("Expected one JSON object.")
	}
	return nil
}
