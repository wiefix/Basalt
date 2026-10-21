-- Standby panels: each keeps a copy of this database (walship) and can
-- take over. They enroll with a one-time token like servers; server_id is
-- the server their host's agent belongs to.
CREATE TABLE standby_panels (
    id                INTEGER PRIMARY KEY,
    name              TEXT    NOT NULL UNIQUE,
    server_id         INTEGER REFERENCES servers (id) ON DELETE SET NULL,
    enroll_token_hash BLOB,
    enroll_expires_at INTEGER,
    cert_fp           TEXT    NOT NULL DEFAULT '',
    prev_cert_fp      TEXT    NOT NULL DEFAULT '',
    cert_expires_at   INTEGER,
    enrolled_at       INTEGER,
    last_seen_at      INTEGER,
    last_ip           TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;
