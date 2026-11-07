-- +goose Up
CREATE TABLE admin_users (
 id uuid PRIMARY KEY,
 username text NOT NULL UNIQUE,
 password_hash text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE admin_sessions (
 token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash)=32),
 user_id uuid NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz,
 CHECK (expires_at > created_at)
);
CREATE INDEX admin_sessions_user ON admin_sessions(user_id, created_at);
CREATE TABLE admin_login_limits (
 key_hash bytea PRIMARY KEY CHECK (octet_length(key_hash)=32),
 attempts integer NOT NULL,
 expires_at timestamptz NOT NULL
);
CREATE INDEX admin_login_limits_expiry ON admin_login_limits(expires_at);

-- +goose Down
DROP TABLE admin_login_limits;
DROP TABLE admin_sessions;
DROP TABLE admin_users;
