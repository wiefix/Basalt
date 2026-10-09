CREATE TABLE sftp_users (
    id            INTEGER PRIMARY KEY,
    site_id       INTEGER NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    username      TEXT    NOT NULL UNIQUE,
    -- argon2id; empty when only key authentication is allowed.
    password_hash TEXT    NOT NULL DEFAULT '',
    -- JSON array of authorized_keys lines.
    public_keys   TEXT    NOT NULL DEFAULT '[]',
    created_at    INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

CREATE INDEX sftp_users_site ON sftp_users (site_id);

ALTER TABLE servers ADD COLUMN sftp_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE servers ADD COLUMN sftp_port INTEGER NOT NULL DEFAULT 0;
ALTER TABLE servers ADD COLUMN sftp_fingerprint TEXT NOT NULL DEFAULT '';
