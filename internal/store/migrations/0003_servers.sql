CREATE TABLE servers (
    id                INTEGER PRIMARY KEY,
    name              TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    -- One-time enrollment token (SHA-256); NULL once used.
    enroll_token_hash BLOB    UNIQUE,
    enroll_expires_at INTEGER,
    enrolled_at       INTEGER,
    -- SHA-256 fingerprints of the accepted agent certificates. The previous
    -- one stays valid until the agent reconnects with the renewed one.
    cert_fp           TEXT,
    prev_cert_fp      TEXT,
    cert_expires_at   INTEGER,
    -- Reported by the agent in its hello message.
    hostname          TEXT    NOT NULL DEFAULT '',
    os                TEXT    NOT NULL DEFAULT '',
    arch              TEXT    NOT NULL DEFAULT '',
    kernel            TEXT    NOT NULL DEFAULT '',
    distro            TEXT    NOT NULL DEFAULT '',
    agent_version     TEXT    NOT NULL DEFAULT '',
    cpu_count         INTEGER NOT NULL DEFAULT 0,
    mem_total         INTEGER NOT NULL DEFAULT 0,
    last_seen_at      INTEGER,
    last_ip           TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

CREATE TABLE server_metrics (
    server_id  INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    at         INTEGER NOT NULL,
    cpu_pct    REAL    NOT NULL,
    mem_used   INTEGER NOT NULL,
    mem_total  INTEGER NOT NULL,
    disk_used  INTEGER NOT NULL,
    disk_total INTEGER NOT NULL,
    load1      REAL    NOT NULL,
    load5      REAL    NOT NULL,
    load15     REAL    NOT NULL,
    uptime     INTEGER NOT NULL,
    PRIMARY KEY (server_id, at)
) STRICT, WITHOUT ROWID;
