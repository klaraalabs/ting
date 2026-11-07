package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klaraalabs/ting/src/api/internal/secrets"
)

var ErrLeaseLost = errors.New("delivery lease is no longer owned")

type Store struct {
	DB    *pgxpool.Pool
	Vault *secrets.Vault
}

type Lease struct {
	ID             int64
	Token          string
	AppID          string
	NotificationID string
	InstallationID string
	Revision       int64
	Attempt        int
}

// Claim uses one short transaction implicit in the statement. No connection stays
// checked out while a provider sends. Unsupported platforms are left untouched.
func (s *Store) Claim(ctx context.Context, platforms []string, c Config) (*Lease, error) {
	if len(platforms) == 0 {
		return nil, nil
	}
	l := &Lease{Token: uuid.NewString()}
	err := s.DB.QueryRow(ctx, `WITH candidate AS (
 SELECT d.id FROM deliveries d JOIN installations i ON i.app_id=d.app_id AND i.id=d.installation_id
 WHERE d.state='queued' AND d.next_attempt_at<=clock_timestamp()
 AND (d.locked_until IS NULL OR d.locked_until<=clock_timestamp())
 AND d.expires_at>clock_timestamp() AND d.attempt_count<$3 AND i.platform=ANY($1::text[])
 ORDER BY d.next_attempt_at,d.id LIMIT 1 FOR UPDATE OF d SKIP LOCKED
 ) UPDATE deliveries d SET lease_id=$2,locked_until=clock_timestamp()+$4*interval '1 microsecond',
 attempt_count=d.attempt_count+1,updated_at=clock_timestamp()
 FROM candidate c WHERE d.id=c.id
 RETURNING d.id,d.app_id,d.notification_id,d.installation_id,d.installation_revision,d.attempt_count`,
		platforms, l.Token, c.MaxAttempts, c.LeaseDuration.Microseconds()).Scan(&l.ID, &l.AppID, &l.NotificationID, &l.InstallationID, &l.Revision, &l.Attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

// Prepare rechecks the current association and lease before exposing a token to a
// sender. It returns a safe skip/failure category when no network call should occur.
func (s *Store) Prepare(ctx context.Context, l Lease, reserve time.Duration) (Message, string, time.Duration, error) {
	var m Message
	var enabled, active bool
	var revision int64
	var encrypted, data []byte
	var ttl, leaseRemaining float64
	err := s.DB.QueryRow(ctx, `SELECT a.enabled,i.active,i.revision,i.platform,i.apns_environment,i.encrypted_push_token,n.title,n.body,n.data,
 EXTRACT(EPOCH FROM (d.expires_at-clock_timestamp()))::float8,
 EXTRACT(EPOCH FROM (d.locked_until-clock_timestamp()))::float8
 FROM deliveries d JOIN apps a ON a.id=d.app_id
 JOIN installations i ON i.app_id=d.app_id AND i.id=d.installation_id
 JOIN notifications n ON n.app_id=d.app_id AND n.id=d.notification_id
 WHERE d.id=$1 AND d.lease_id=$2 AND d.state='queued' AND d.locked_until>clock_timestamp()`, l.ID, l.Token).Scan(
		&enabled, &active, &revision, &m.Platform, &m.Environment, &encrypted, &m.Title, &m.Body, &data, &ttl, &leaseRemaining)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, "", 0, ErrLeaseLost
	}
	if err != nil {
		return m, "", 0, err
	}
	if !enabled {
		return m, "app_disabled", 0, nil
	}
	if !active || revision != l.Revision {
		return m, "installation_changed", 0, nil
	}
	if ttl <= 0 {
		return m, "deadline_exceeded", 0, nil
	}
	budget := time.Duration(leaseRemaining*float64(time.Second)) - reserve
	if budget <= 0 {
		return m, "", 0, ErrLeaseLost
	}
	m.AppID = l.AppID
	m.NotificationID = l.NotificationID
	m.InstallationID = l.InstallationID
	m.RemainingTTL = time.Duration(ttl * float64(time.Second))
	if m.RemainingTTL < budget {
		budget = m.RemainingTTL
	}
	plaintext, err := s.Vault.Open(l.AppID+":installation:"+l.InstallationID, encrypted)
	if err != nil {
		return m, "credential_error", 0, nil
	}
	if err = json.Unmarshal(data, &m.Data); err != nil {
		return m, "internal_error", 0, nil
	}
	m.PushToken = string(plaintext)
	return m, "", budget, nil
}

// Sweep terminates expired or exhausted work in bounded batches. Active leases
// are never stolen by maintenance, including when their delivery TTL elapses.
func (s *Store) Sweep(ctx context.Context, maxAttempts int) (int64, error) {
	tag, err := s.DB.Exec(ctx, `WITH candidates AS (
 SELECT id FROM deliveries WHERE state='queued'
 AND (locked_until IS NULL OR locked_until<=clock_timestamp())
 AND (expires_at<=clock_timestamp() OR attempt_count>=$1)
 ORDER BY expires_at,id LIMIT 100 FOR UPDATE SKIP LOCKED
 ) UPDATE deliveries d SET state='failed',
 error_category=CASE WHEN d.expires_at<=clock_timestamp() THEN 'deadline_exceeded' ELSE 'attempts_exhausted' END,
 lease_id=NULL,locked_until=NULL,updated_at=clock_timestamp()
 FROM candidates c WHERE d.id=c.id`, maxAttempts)
	return tag.RowsAffected(), err
}

// Release is for a claimed job that has not started when shutdown arrives. Once
// a network call may have happened, leave the lease recoverable instead.
func (s *Store) Release(ctx context.Context, l Lease) error {
	_, err := s.DB.Exec(ctx, `UPDATE deliveries SET lease_id=NULL,locked_until=NULL,
 attempt_count=GREATEST(attempt_count-1,0),updated_at=clock_timestamp()
 WHERE id=$1 AND lease_id=$2 AND state='queued' AND locked_until>clock_timestamp()`, l.ID, l.Token)
	return err
}
