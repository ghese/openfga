-- +goose Up
ALTER TABLE authorization_model ADD schema_version VARCHAR(5) NOT NULL CONSTRAINT df_auth_model_schema_version DEFAULT '1.0';

-- +goose Down
ALTER TABLE authorization_model DROP COLUMN schema_version;
