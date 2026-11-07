package notifications

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/klaraalabs/ting/src/api/internal/apierror"
)

type Limits struct {
	MaxPending       int
	MaxPendingPerApp int
	MaxTargets       int
	SendsPerMinute   int
}

func DefaultLimits() Limits {
	return Limits{MaxPending: 100000, MaxPendingPerApp: 10000, MaxTargets: 1000, SendsPerMinute: 120}
}
func (l Limits) Validate() error {
	if l.MaxPending < 1 || l.MaxPending > 1000000 {
		return errors.New("TING_QUEUE_MAX_PENDING must be between 1 and 1000000")
	}
	if l.MaxPendingPerApp < 1 || l.MaxPendingPerApp > l.MaxPending {
		return errors.New("TING_QUEUE_MAX_PENDING_PER_APP must be positive and at most TING_QUEUE_MAX_PENDING")
	}
	if l.MaxTargets < 1 || l.MaxTargets > 10000 || l.MaxTargets > l.MaxPendingPerApp {
		return errors.New("TING_MAX_TARGETS_PER_SEND must be 1 to 10000 and at most the per-App queue capacity")
	}
	if l.SendsPerMinute < 1 || l.SendsPerMinute > 100000 {
		return errors.New("TING_APP_SENDS_PER_MINUTE must be between 1 and 100000")
	}
	return nil
}
func (l Limits) admit(ctx context.Context, tx pgx.Tx, appID string, targets int) error {
	if targets > l.MaxTargets {
		return apierror.New(413, "fanout_too_large", "This send exceeds the configured installation target limit.")
	}
	// Only newly accepted sends consume this persistent App budget. A failed
	// capacity check rolls back the budget along with the notification insert.
	var sends int
	err := tx.QueryRow(ctx, `INSERT INTO notification_send_limits(app_id,sends,expires_at) VALUES($1,1,clock_timestamp()+interval '1 minute')
 ON CONFLICT(app_id) DO UPDATE SET sends=CASE WHEN notification_send_limits.expires_at<=clock_timestamp() THEN 1 ELSE notification_send_limits.sends+1 END,
 expires_at=CASE WHEN notification_send_limits.expires_at<=clock_timestamp() THEN clock_timestamp()+interval '1 minute' ELSE notification_send_limits.expires_at END RETURNING sends`, appID).Scan(&sends)
	if err != nil {
		return err
	}
	if sends > l.SendsPerMinute {
		return apierror.Retry(429, "app_send_rate_limited", "This App has reached its send budget. Retry later.", 60)
	}
	if targets == 0 {
		return nil
	}
	var appPending, totalPending int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM deliveries WHERE app_id=$1 AND state='queued' LIMIT $2) q`, appID, l.MaxPendingPerApp).Scan(&appPending)
	if err != nil {
		return err
	}
	if targets > l.MaxPendingPerApp-appPending {
		return apierror.Retry(429, "app_queue_full", "This App's delivery queue is full. Retry later.", 60)
	}
	err = tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM deliveries WHERE state='queued' LIMIT $1) q`, l.MaxPending).Scan(&totalPending)
	if err != nil {
		return err
	}
	if targets > l.MaxPending-totalPending {
		return apierror.Retry(503, "queue_full", "The delivery queue is full. Retry later.", 60)
	}
	return nil
}
