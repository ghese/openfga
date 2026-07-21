-- +goose Up
-- All textual identifier columns use the binary collation Latin1_General_BIN2 so that
-- equality, uniqueness, and ordering are case-sensitive and bytewise, matching the
-- behavior of the Postgres, MySQL, and SQLite datastores. Do not rely on the database
-- default collation, which is typically case-insensitive on SQL Server.
CREATE TABLE tuple (
    store CHAR(26) COLLATE Latin1_General_BIN2 NOT NULL,
    object_type VARCHAR(128) COLLATE Latin1_General_BIN2 NOT NULL,
    object_id VARCHAR(255) COLLATE Latin1_General_BIN2 NOT NULL,
    relation VARCHAR(50) COLLATE Latin1_General_BIN2 NOT NULL,
    _user VARCHAR(256) COLLATE Latin1_General_BIN2 NOT NULL,
    user_type VARCHAR(7) COLLATE Latin1_General_BIN2 NOT NULL,
    ulid CHAR(26) COLLATE Latin1_General_BIN2 NOT NULL,
    inserted_at DATETIME2 NOT NULL,
    PRIMARY KEY (store, object_type, object_id, relation, _user)
);

CREATE UNIQUE INDEX idx_tuple_ulid ON tuple (ulid);

CREATE TABLE authorization_model (
    store CHAR(26) COLLATE Latin1_General_BIN2 NOT NULL,
    authorization_model_id CHAR(26) COLLATE Latin1_General_BIN2 NOT NULL,
    type VARCHAR(256) COLLATE Latin1_General_BIN2 NOT NULL,
    type_definition VARBINARY(MAX),
    PRIMARY KEY (store, authorization_model_id, type)
);

CREATE TABLE store (
    id CHAR(26) COLLATE Latin1_General_BIN2 PRIMARY KEY,
    name NVARCHAR(64) COLLATE Latin1_General_BIN2 NOT NULL,
    created_at DATETIME2 NOT NULL,
    updated_at DATETIME2,
    deleted_at DATETIME2
);

CREATE TABLE assertion (
    store CHAR(26) COLLATE Latin1_General_BIN2 NOT NULL,
    authorization_model_id CHAR(26) COLLATE Latin1_General_BIN2 NOT NULL,
    assertions VARBINARY(MAX),
    PRIMARY KEY (store, authorization_model_id)
);

CREATE TABLE changelog (
    store CHAR(26) COLLATE Latin1_General_BIN2 NOT NULL,
    object_type VARCHAR(256) COLLATE Latin1_General_BIN2 NOT NULL,
    object_id VARCHAR(256) COLLATE Latin1_General_BIN2 NOT NULL,
    relation VARCHAR(50) COLLATE Latin1_General_BIN2 NOT NULL,
    _user VARCHAR(512) COLLATE Latin1_General_BIN2 NOT NULL,
    operation INTEGER NOT NULL,
    ulid CHAR(26) COLLATE Latin1_General_BIN2 NOT NULL,
    inserted_at DATETIME2 NOT NULL,
    PRIMARY KEY (store, ulid, object_type)
);

-- +goose Down
DROP TABLE IF EXISTS tuple;
DROP TABLE IF EXISTS authorization_model;
DROP TABLE IF EXISTS store;
DROP TABLE IF EXISTS assertion;
DROP TABLE IF EXISTS changelog;
