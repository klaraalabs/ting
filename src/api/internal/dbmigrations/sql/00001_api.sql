-- +goose Up
CREATE TABLE apps (
 id text PRIMARY KEY, name text NOT NULL, slug text UNIQUE, enabled boolean NOT NULL DEFAULT true,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE app_api_tokens (
 id uuid PRIMARY KEY, app_id text NOT NULL REFERENCES apps(id), token_hash bytea NOT NULL UNIQUE,
 suffix text NOT NULL, label text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 last_used_at timestamptz, revoked_at timestamptz
);
CREATE INDEX app_api_tokens_app ON app_api_tokens(app_id);
CREATE TABLE app_identity_secrets (
 app_id text NOT NULL REFERENCES apps(id), kid uuid NOT NULL, encrypted_secret bytea NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), retired_at timestamptz, PRIMARY KEY(app_id,kid)
);
CREATE TABLE apns_credentials (
 app_id text NOT NULL REFERENCES apps(id), environment text NOT NULL CHECK(environment IN ('sandbox','production')),
 bundle_id text NOT NULL, team_id text NOT NULL, key_id text NOT NULL, encrypted_key bytea NOT NULL,
 version uuid NOT NULL, updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(app_id,environment)
);
CREATE TABLE fcm_credentials (
 app_id text PRIMARY KEY REFERENCES apps(id), project_id text NOT NULL, client_email text NOT NULL,
 encrypted_json bytea NOT NULL, version uuid NOT NULL, updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE installations (
 app_id text NOT NULL REFERENCES apps(id), id uuid NOT NULL, external_user_id text NOT NULL,
 platform text NOT NULL CHECK(platform IN ('ios','android')),
 apns_environment text NOT NULL CHECK(apns_environment IN ('','sandbox','production')),
 encrypted_push_token bytea NOT NULL, push_token_hash bytea NOT NULL,
 credential_hash bytea NOT NULL, active boolean NOT NULL DEFAULT true, revision bigint NOT NULL DEFAULT 1,
 created_at timestamptz NOT NULL DEFAULT now(), last_seen_at timestamptz NOT NULL DEFAULT now(),
 token_updated_at timestamptz NOT NULL DEFAULT now(), invalidated_at timestamptz,
 PRIMARY KEY(app_id,id),
 CHECK ((platform='ios' AND apns_environment IN ('sandbox','production')) OR (platform='android' AND apns_environment=''))
);
CREATE UNIQUE INDEX installation_active_token ON installations(app_id,platform,apns_environment,push_token_hash) WHERE active;
CREATE INDEX installations_recipients ON installations(app_id,external_user_id) WHERE active;
CREATE TABLE notifications (
 id uuid PRIMARY KEY, app_id text NOT NULL REFERENCES apps(id), title text NOT NULL, body text NOT NULL,
 data jsonb NOT NULL DEFAULT '{}', idempotency_key text, request_hash bytea NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(app_id,id), UNIQUE(app_id,idempotency_key)
);
CREATE TABLE deliveries (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, app_id text NOT NULL, notification_id uuid NOT NULL,
 installation_id uuid NOT NULL, installation_revision bigint NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','accepted','failed','skipped')),
 attempt_count integer NOT NULL DEFAULT 0, next_attempt_at timestamptz NOT NULL DEFAULT now(),
 locked_until timestamptz, lease_id uuid, error_category text, provider_reference text,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(app_id,notification_id) REFERENCES notifications(app_id,id) ON DELETE CASCADE,
 FOREIGN KEY(app_id,installation_id) REFERENCES installations(app_id,id),
 UNIQUE(notification_id,installation_id)
);
CREATE INDEX deliveries_pending ON deliveries(next_attempt_at) WHERE state='queued';
CREATE INDEX notifications_recent ON notifications(app_id,created_at DESC);

-- +goose Down
DROP TABLE deliveries;
DROP TABLE notifications;
DROP TABLE installations;
DROP TABLE fcm_credentials;
DROP TABLE apns_credentials;
DROP TABLE app_identity_secrets;
DROP TABLE app_api_tokens;
DROP TABLE apps;
