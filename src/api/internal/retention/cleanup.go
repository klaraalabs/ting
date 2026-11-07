package retention

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Age       time.Duration
	Interval  time.Duration
	BatchSize int
}

func DefaultConfig() Config {
	return Config{Age: 7 * 24 * time.Hour, Interval: time.Minute, BatchSize: 100}
}
func (c Config) Validate() error {
	if c.Age < time.Hour || c.Age > 365*24*time.Hour {
		return errors.New("TING_RETENTION must be between 1h and 8760h")
	}
	if c.Interval < time.Second || c.Interval > time.Hour {
		return errors.New("TING_RETENTION_INTERVAL must be between 1s and 1h")
	}
	if c.BatchSize < 1 || c.BatchSize > 1000 {
		return errors.New("TING_RETENTION_BATCH_SIZE must be between 1 and 1000")
	}
	return nil
}

type Service struct {
	DB     *pgxpool.Pool
	Config Config
}
type Counts struct{ Notifications, Sessions, LoginLimits, SendLimits int64 }

func (s *Service) Cleanup(ctx context.Context) (Counts, error) {
	var out Counts
	if err := s.Config.Validate(); err != nil {
		return out, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	// Share admission's lock so idempotency replay cannot observe a record between
	// an ON CONFLICT and a retention deletion. Busy admission defers cleanup.
	var locked bool
	err = tx.QueryRow(ctx, `SELECT singleton FROM notification_admission WHERE singleton FOR UPDATE SKIP LOCKED`).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	tag, err := tx.Exec(ctx, `WITH expired AS (
 SELECT n.id FROM notifications n WHERE n.created_at<clock_timestamp()-$1*interval '1 microsecond'
 AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.notification_id=n.id AND
 (d.state='queued' OR d.updated_at>=clock_timestamp()-$1*interval '1 microsecond' OR d.locked_until>clock_timestamp()))
 ORDER BY n.created_at,n.id LIMIT $2 FOR UPDATE OF n SKIP LOCKED
 ) DELETE FROM notifications n USING expired e WHERE n.id=e.id`, s.Config.Age.Microseconds(), s.Config.BatchSize)
	if err != nil {
		return out, err
	}
	out.Notifications = tag.RowsAffected()
	// Installation records, including inactive generation fences, are intentionally
	// retained. Removing them could let delayed client mutations recreate an owner.
	tag, err = tx.Exec(ctx, `WITH expired AS (SELECT token_hash FROM admin_sessions WHERE expires_at<=clock_timestamp() OR revoked_at IS NOT NULL ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED)
 DELETE FROM admin_sessions s USING expired e WHERE s.token_hash=e.token_hash`, s.Config.BatchSize)
	if err != nil {
		return out, err
	}
	out.Sessions = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `WITH expired AS (SELECT key_hash FROM admin_login_limits WHERE expires_at<=clock_timestamp() ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED)
 DELETE FROM admin_login_limits l USING expired e WHERE l.key_hash=e.key_hash`, s.Config.BatchSize)
	if err != nil {
		return out, err
	}
	out.LoginLimits = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `WITH expired AS (SELECT app_id FROM notification_send_limits WHERE expires_at<=clock_timestamp() ORDER BY expires_at LIMIT $1 FOR UPDATE SKIP LOCKED)
 DELETE FROM notification_send_limits l USING expired e WHERE l.app_id=e.app_id`, s.Config.BatchSize)
	if err != nil {
		return out, err
	}
	out.SendLimits = tag.RowsAffected()
	if err = tx.Commit(ctx); err != nil {
		return Counts{}, err
	}
	return out, nil
}
func (s *Service) Run(ctx context.Context) {
	for ctx.Err() == nil {
		operation, cancel := context.WithTimeout(ctx, 5*time.Second)
		var counts Counts
		var err error
		// Catch up without monopolizing admission. Each batch commits and releases
		// its lock; both the pass count and the total SQL time remain bounded.
		for pass := 0; pass < 10; pass++ {
			var batch Counts
			batch, err = s.Cleanup(operation)
			if err != nil {
				break
			}
			counts.Notifications += batch.Notifications
			counts.Sessions += batch.Sessions
			counts.LoginLimits += batch.LoginLimits
			counts.SendLimits += batch.SendLimits
			bound := int64(s.Config.BatchSize)
			if batch.Notifications < bound && batch.Sessions < bound && batch.LoginLimits < bound && batch.SendLimits < bound {
				break
			}
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("retention cleanup failed")
		} else if counts != (Counts{}) {
			slog.Info("retention cleanup completed", "notifications", counts.Notifications, "sessions", counts.Sessions, "loginLimits", counts.LoginLimits, "sendLimits", counts.SendLimits)
		}
		timer := time.NewTimer(s.Config.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
