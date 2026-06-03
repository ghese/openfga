-- +goose Up
CREATE TABLE tuple (
    store CHAR(26) NOT NULL,
    object_type VARCHAR(128) NOT NULL,
    object_id VARCHAR(255) NOT NULL,
    relation VARCHAR(50) NOT NULL,
    _user VARCHAR(256) NOT NULL,
    user_type VARCHAR(7) NOT NULL,
    ulid CHAR(26) NOT NULL,
    inserted_at DATETIME2 NOT NULL,
    condition_name VARCHAR(256),
    condition_context VARBINARY(MAX),
    PRIMARY KEY (store, object_type, object_id, relation, _user)
);

CREATE UNIQUE INDEX idx_tuple_ulid ON tuple (ulid);
CREATE INDEX idx_reverse_lookup_user ON tuple (store, object_type, relation, _user);

CREATE TABLE authorization_model (
    store CHAR(26) NOT NULL,
    authorization_model_id CHAR(26) NOT NULL,
    type VARCHAR(256) NOT NULL,
    type_definition VARBINARY(MAX),
    schema_version VARCHAR(5) NOT NULL DEFAULT '1.0',
    serialized_protobuf VARBINARY(MAX),
    PRIMARY KEY (store, authorization_model_id, type)
);

CREATE TABLE store (
    id CHAR(26) PRIMARY KEY,
    name NVARCHAR(64) NOT NULL,
    created_at DATETIME2 NOT NULL,
    updated_at DATETIME2,
    deleted_at DATETIME2
);

CREATE TABLE assertion (
    store CHAR(26) NOT NULL,
    authorization_model_id CHAR(26) NOT NULL,
    assertions VARBINARY(MAX),
    PRIMARY KEY (store, authorization_model_id)
);

CREATE TABLE changelog (
    store CHAR(26) NOT NULL,
    object_type VARCHAR(256) NOT NULL,
    object_id VARCHAR(256) NOT NULL,
    relation VARCHAR(50) NOT NULL,
    _user VARCHAR(512) NOT NULL,
    operation INTEGER NOT NULL,
    ulid CHAR(26) NOT NULL,
    inserted_at DATETIME2 NOT NULL,
    condition_name VARCHAR(256),
    condition_context VARBINARY(MAX),
    PRIMARY KEY (store, ulid, object_type)
);

-- +goose Down
DROP TABLE IF EXISTS tuple;
DROP TABLE IF EXISTS authorization_model;
DROP TABLE IF EXISTS store;
DROP TABLE IF EXISTS assertion;
DROP TABLE IF EXISTS changelog;
