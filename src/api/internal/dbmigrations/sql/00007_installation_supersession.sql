-- +goose Up
ALTER TABLE installations ADD COLUMN superseded_at timestamptz;
CREATE INDEX installation_unsuperseded_token
 ON installations(app_id,platform,apns_environment,push_token_hash)
 WHERE superseded_at IS NULL;
-- Older releases did not distinguish transfer from logout/invalidation. Fence
-- inactive duplicate-token records conservatively, including when both owners
-- have since logged out. Leave unique-token recovery and active owners intact.
UPDATE installations i SET superseded_at=now()
WHERE NOT i.active AND EXISTS (
 SELECT 1 FROM installations other WHERE other.app_id=i.app_id AND other.id<>i.id
 AND other.platform=i.platform AND other.apns_environment=i.apns_environment
 AND other.push_token_hash=i.push_token_hash AND other.superseded_at IS NULL
);
ALTER TABLE installations ADD CONSTRAINT installation_superseded_inactive
 CHECK (superseded_at IS NULL OR NOT active);

-- +goose Down
ALTER TABLE installations DROP CONSTRAINT installation_superseded_inactive;
DROP INDEX installation_unsuperseded_token;
ALTER TABLE installations DROP COLUMN superseded_at;
