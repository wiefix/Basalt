CREATE TABLE users (
    id             INTEGER PRIMARY KEY,
    email          TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    name           TEXT    NOT NULL,
    password_hash  TEXT    NOT NULL,
    role           TEXT    NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
    -- TOTP seed encrypted with the master key; NULL when 2FA is off.
    totp_secret    BLOB,
    -- Seed awaiting confirmation during 2FA enrolment.
    totp_pending   BLOB,
    -- Last accepted TOTP time step; prevents replaying a code.
    totp_last_step INTEGER NOT NULL DEFAULT 0,
    disabled_at    INTEGER,
    created_at     INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at     INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

CREATE TABLE sessions (
    -- SHA-256 of the cookie token; the token itself is never stored.
    token_hash   BLOB    PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- 1 while the user has passed the password step but not yet 2FA.
    mfa_pending  INTEGER NOT NULL DEFAULT 0 CHECK (mfa_pending IN (0, 1)),
    ip           TEXT    NOT NULL,
    user_agent   TEXT    NOT NULL,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    last_seen_at INTEGER NOT NULL DEFAULT (unixepoch()),
    expires_at   INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX sessions_user ON sessions (user_id);
CREATE INDEX sessions_expires ON sessions (expires_at);

CREATE TABLE recovery_codes (
    user_id   INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash BLOB    NOT NULL,
    used_at   INTEGER,
    PRIMARY KEY (user_id, code_hash)
) STRICT, WITHOUT ROWID;

CREATE TABLE audit_log (
    id         INTEGER PRIMARY KEY,
    at         INTEGER NOT NULL DEFAULT (unixepoch()),
    actor_id   INTEGER REFERENCES users (id) ON DELETE SET NULL,
    action     TEXT    NOT NULL,
    ip         TEXT    NOT NULL DEFAULT '',
    -- JSON object with action-specific details. Never contains secrets.
    details    TEXT    NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX audit_log_at ON audit_log (at);
