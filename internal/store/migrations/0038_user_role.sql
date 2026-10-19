-- basalt:foreign-keys-off
-- The global role "user": no rights of its own on the panel, only those of
-- the projects the user belongs to. The CHECK on users.role changes, so the
-- table is rebuilt (the references to it stay by name).
CREATE TABLE users_new (
    id              INTEGER PRIMARY KEY,
    email           TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    name            TEXT    NOT NULL,
    password_hash   TEXT    NOT NULL,
    role            TEXT    NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'viewer', 'user')),
    totp_secret     BLOB,
    totp_pending    BLOB,
    totp_last_step  INTEGER NOT NULL DEFAULT 0,
    disabled_at     INTEGER,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    webauthn_handle BLOB
) STRICT;

INSERT INTO users_new (id, email, name, password_hash, role, totp_secret, totp_pending, totp_last_step,
    disabled_at, created_at, updated_at, webauthn_handle)
SELECT id, email, name, password_hash, role, totp_secret, totp_pending, totp_last_step,
    disabled_at, created_at, updated_at, webauthn_handle FROM users;

DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
CREATE UNIQUE INDEX users_webauthn_handle ON users (webauthn_handle) WHERE webauthn_handle IS NOT NULL;
