CREATE TABLE databases (
    id         INTEGER PRIMARY KEY,
    site_id    INTEGER NOT NULL REFERENCES sites (id) ON DELETE RESTRICT,
    server_id  INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    username   TEXT    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE (server_id, name),
    UNIQUE (server_id, username)
) STRICT;

CREATE INDEX databases_site ON databases (site_id);

-- MariaDB version reported by the agent; empty when unavailable.
ALTER TABLE servers ADD COLUMN mariadb TEXT NOT NULL DEFAULT '';
