-- Credentials for private image registries, one per registry host.
CREATE TABLE registries (
    id         INTEGER PRIMARY KEY,
    -- e.g. "ghcr.io", "registry.example.com:5000"; "docker.io" for Docker Hub.
    host       TEXT    NOT NULL UNIQUE,
    username   TEXT    NOT NULL,
    -- Password or access token, encrypted with the master key (associated
    -- data "registry:<host>").
    secret     BLOB    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;
