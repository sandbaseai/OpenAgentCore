-- +goose Up
ALTER TABLE runtime_deployment ADD COLUMN compute_profiles jsonb NOT NULL DEFAULT '{}'
    CHECK (jsonb_typeof(compute_profiles) = 'object');
CREATE TABLE session_compute_generations (
    session_id uuid PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    profile text NOT NULL CHECK (profile ~ '^[a-z][a-z0-9-]{0,47}$'),
    generation bigint NOT NULL CHECK (generation > 0)
);
CREATE INDEX session_compute_generations_generation ON session_compute_generations(generation);

-- +goose Down
DROP TABLE session_compute_generations;
ALTER TABLE runtime_deployment DROP COLUMN compute_profiles;
