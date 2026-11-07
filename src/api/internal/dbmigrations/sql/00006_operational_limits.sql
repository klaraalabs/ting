-- +goose Up
-- Admission and retention take this lock before touching notification records.
CREATE TABLE notification_admission (singleton boolean PRIMARY KEY CHECK (singleton));
INSERT INTO notification_admission VALUES(true);
CREATE TABLE notification_send_limits (
 app_id text PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
 sends integer NOT NULL CHECK (sends>0),
 expires_at timestamptz NOT NULL
);
CREATE INDEX deliveries_queued_app ON deliveries(app_id) WHERE state='queued';
CREATE INDEX notifications_retention ON notifications(created_at,id);
CREATE INDEX admin_sessions_expiry ON admin_sessions(expires_at);

-- +goose Down
DROP INDEX admin_sessions_expiry;
DROP INDEX notifications_retention;
DROP INDEX deliveries_queued_app;
DROP TABLE notification_send_limits;
DROP TABLE notification_admission;
