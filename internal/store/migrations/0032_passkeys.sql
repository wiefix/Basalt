-- Passkeys (WebAuthn credentials): sign-in without a password, the
-- authenticator verifying the user (PIN or biometrics).
CREATE TABLE passkeys (
    id              INTEGER PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    credential_id   BLOB    NOT NULL UNIQUE,
    -- COSE_Key of the credential and its algorithm.
    public_key      BLOB    NOT NULL,
    alg             INTEGER NOT NULL,
    sign_count      INTEGER NOT NULL DEFAULT 0,
    -- The host name (WebAuthn RP ID) it was registered on.
    rp_id           TEXT    NOT NULL,
    name            TEXT    NOT NULL,
    aaguid          BLOB,
    backup_eligible INTEGER NOT NULL DEFAULT 0,
    backed_up       INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    last_used_at    INTEGER,
    UNIQUE (user_id, name)
) STRICT;
CREATE INDEX passkeys_user ON passkeys (user_id);

-- The random handle passkeys know a user by (no personal data in it).
ALTER TABLE users ADD COLUMN webauthn_handle BLOB;
CREATE UNIQUE INDEX users_webauthn_handle ON users (webauthn_handle) WHERE webauthn_handle IS NOT NULL;
