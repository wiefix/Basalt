-- API tokens: a user's credentials for scripts and CI. Only the SHA-256 of
-- a token is stored. A token acts as its user with at most role (the lower
-- of the two), until expires_at (NULL: no expiry).
CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    token_hash   BLOB    NOT NULL UNIQUE,
    -- The token's first characters, to recognize it in lists.
    prefix       TEXT    NOT NULL,
    role         TEXT    NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
    expires_at   INTEGER,
    last_used_at INTEGER,
    last_used_ip TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE (user_id, name)
) STRICT;
CREATE INDEX api_tokens_user ON api_tokens (user_id);
