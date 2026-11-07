-- +goose Up
ALTER TABLE deliveries ADD COLUMN expires_at timestamptz;
-- Existing work keeps a deadline relative to acceptance, not the migration time.
UPDATE deliveries SET expires_at = created_at + interval '24 hours';
ALTER TABLE deliveries ALTER COLUMN expires_at SET NOT NULL;
ALTER TABLE deliveries ALTER COLUMN expires_at SET DEFAULT (now() + interval '24 hours');
ALTER TABLE deliveries ADD CONSTRAINT deliveries_attempt_count_nonnegative CHECK (attempt_count >= 0);
CREATE INDEX deliveries_expiration ON deliveries(expires_at) WHERE state='queued';
CREATE INDEX deliveries_attempts ON deliveries(attempt_count) WHERE state='queued';

-- +goose Down
DROP INDEX deliveries_attempts;
DROP INDEX deliveries_expiration;
ALTER TABLE deliveries DROP CONSTRAINT deliveries_attempt_count_nonnegative;
ALTER TABLE deliveries DROP COLUMN expires_at;
