-- +goose Up
ALTER TABLE deployment_model_providers ADD COLUMN headers jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(headers) = 'object');

-- +goose Down
ALTER TABLE deployment_model_providers DROP COLUMN headers;
