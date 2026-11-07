package delivery

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Complete fences every result with both the lease token and its expiration.
// Lock order matches registration/logout: App, installation, then delivery.
// This also makes invalid-token retirement atomic with recording its outcome.
func (s *Store) Complete(ctx context.Context, l Lease, result Result, skip string, c Config) (bool, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var enabled, active bool
	var revision int64
	var registeredAt time.Time
	if err = tx.QueryRow(ctx, `SELECT enabled FROM apps WHERE id=$1 FOR UPDATE`, l.AppID).Scan(&enabled); err != nil {
		return false, err
	}
	if err = tx.QueryRow(ctx, `SELECT active,revision,last_seen_at FROM installations WHERE app_id=$1 AND id=$2 FOR UPDATE`, l.AppID, l.InstallationID).Scan(&active, &revision, &registeredAt); err != nil {
		return false, err
	}
	var attempt int
	var remaining float64
	err = tx.QueryRow(ctx, `SELECT attempt_count,EXTRACT(EPOCH FROM (expires_at-clock_timestamp()))::float8
 FROM deliveries WHERE id=$1 AND app_id=$2 AND lease_id=$3 AND state='queued' AND locked_until>clock_timestamp() FOR UPDATE`, l.ID, l.AppID, l.Token).Scan(&attempt, &remaining)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state, category := "failed", "internal_error"
	var delay time.Duration
	invalidate := false
	switch {
	case !enabled:
		state, category = "skipped", "app_disabled"
	case !active || revision != l.Revision:
		state, category = "skipped", "installation_changed"
	case skip != "":
		switch skip {
		case "app_disabled", "installation_changed":
			state, category = "skipped", skip
		case "deadline_exceeded", "credential_error", "internal_error":
			category = skip
		}
	default:
		switch result.Outcome {
		case Accepted:
			state, category = "accepted", ""
		case PermanentFailure:
			category = safeCategory(result.Category, "provider_error")
		case InvalidToken:
			category = "invalid_token"
			invalidate = result.InvalidatedAt.IsZero() || !registeredAt.After(result.InvalidatedAt)
		case Retryable:
			state, category = "queued", safeCategory(result.Category, "provider_error")
			delay = retryDelay(c, attempt, result.RetryAfter)
			if remaining <= 0 || delay >= time.Duration(remaining*float64(time.Second)) {
				state, category = "failed", "deadline_exceeded"
			} else if attempt >= c.MaxAttempts {
				state, category = "failed", "attempts_exhausted"
			}
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE deliveries SET state=$4,error_category=NULLIF($5,''),
 next_attempt_at=CASE WHEN $4='queued' THEN clock_timestamp()+$6*interval '1 microsecond' ELSE next_attempt_at END,
 lease_id=NULL,locked_until=NULL,updated_at=clock_timestamp()
 WHERE id=$1 AND app_id=$2 AND lease_id=$3 AND state='queued' AND locked_until>clock_timestamp()`, l.ID, l.AppID, l.Token, state, category, delay.Microseconds())
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if invalidate {
		_, err = tx.Exec(ctx, `UPDATE installations SET active=false,invalidated_at=clock_timestamp(),revision=revision+1 WHERE app_id=$1 AND id=$2 AND revision=$3 AND active`, l.AppID, l.InstallationID, l.Revision)
		if err != nil {
			return false, err
		}
		_, err = tx.Exec(ctx, `UPDATE deliveries SET state='skipped',error_category='installation_invalidated',lease_id=NULL,locked_until=NULL,updated_at=clock_timestamp() WHERE app_id=$1 AND installation_id=$2 AND installation_revision=$3 AND state='queued'`, l.AppID, l.InstallationID, l.Revision)
		if err != nil {
			return false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
