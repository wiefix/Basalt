-- Integrity checks (restic check) of the repositories backups are kept in:
-- one per server and storage, and the panel's own (server_id NULL).
CREATE TABLE backup_checks (
    id          INTEGER PRIMARY KEY,
    storage_id  INTEGER NOT NULL REFERENCES backup_storages (id) ON DELETE CASCADE,
    server_id   INTEGER REFERENCES servers (id) ON DELETE CASCADE,
    trigger     TEXT    NOT NULL CHECK (trigger IN ('manual', 'schedule')),
    status      TEXT    NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
    -- The part of the data read back ("n/t"), besides the structure.
    read_data   TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    log         TEXT    NOT NULL DEFAULT '',
    actor_id    INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    finished_at INTEGER
) STRICT;
CREATE INDEX backup_checks_repo ON backup_checks (storage_id, server_id, id);
-- One check of a repository at a time.
CREATE UNIQUE INDEX backup_checks_running ON backup_checks (storage_id, COALESCE(server_id, 0)) WHERE status = 'running';
