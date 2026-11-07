-- +goose Up
ALTER TABLE notifications ADD COLUMN source text NOT NULL DEFAULT 'api' CHECK (source IN ('api','operator_test'));
CREATE INDEX installations_recent ON installations(app_id,last_seen_at DESC,id DESC);

-- +goose Down
DROP INDEX installations_recent;
ALTER TABLE notifications DROP COLUMN source;
