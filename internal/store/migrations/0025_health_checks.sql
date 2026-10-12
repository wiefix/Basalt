-- Health checks every agent runs; the panel decides each one's status
-- from the results of all of them (a majority).
CREATE TABLE health_checks (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL UNIQUE,
    kind         TEXT    NOT NULL CHECK (kind IN ('http', 'https', 'tcp')),
    host         TEXT    NOT NULL,
    port         INTEGER NOT NULL,
    path         TEXT    NOT NULL DEFAULT '',
    host_header  TEXT    NOT NULL DEFAULT '',
    insecure     INTEGER NOT NULL DEFAULT 0,
    interval_s   INTEGER NOT NULL DEFAULT 30,
    timeout_s    INTEGER NOT NULL DEFAULT 5,
    status       TEXT    NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown', 'up', 'down')),
    status_since INTEGER,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

-- Status changes of each check.
CREATE TABLE health_events (
    id       INTEGER PRIMARY KEY,
    check_id INTEGER NOT NULL REFERENCES health_checks (id) ON DELETE CASCADE,
    status   TEXT    NOT NULL,
    detail   TEXT    NOT NULL DEFAULT '',
    at       INTEGER NOT NULL
) STRICT;
CREATE INDEX health_events_check ON health_events (check_id, at);
