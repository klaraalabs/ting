-- +goose Up
ALTER TABLE installations ADD COLUMN mutation_generation bigint NOT NULL DEFAULT 0 CHECK (mutation_generation >= 0);

-- +goose Down
ALTER TABLE installations DROP COLUMN mutation_generation;
