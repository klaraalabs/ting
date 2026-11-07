package delivery

import (
	"context"
	"time"
)

// Sender must honor context cancellation, reuse transports, and select credentials
// using Message.AppID and Environment. It must never log the message or push token.
// Provider adapters are intentionally separate from the queue and worker lifecycle.
type Sender interface {
	Send(context.Context, Message) Result
}

type Message struct {
	AppID          string
	NotificationID string
	InstallationID string
	Platform       string
	Environment    string
	PushToken      string
	Title          string
	Body           string
	Data           map[string]string
	// The provider should also set its native expiry/TTL using this remaining budget.
	RemainingTTL time.Duration
}

type Outcome string

const (
	Accepted         Outcome = "accepted"
	Retryable        Outcome = "retryable"
	PermanentFailure Outcome = "permanent_failure"
	InvalidToken     Outcome = "invalid_token"
)

type Result struct {
	Outcome Outcome
	// Category must be one of the safe values below. Unknown strings are replaced;
	// raw provider error messages must never reach the status API or logs.
	Category   string
	RetryAfter time.Duration
	// APNs can report when it last confirmed a token was unregistered. A more
	// recent registration must not be retired by that historical response.
	InvalidatedAt time.Time
}

func safeCategory(category, fallback string) string {
	switch category {
	case "provider_unavailable", "provider_timeout", "provider_throttled", "provider_auth", "invalid_payload", "provider_error", "invalid_token", "credential_error", "internal_error":
		return category
	default:
		return fallback
	}
}
