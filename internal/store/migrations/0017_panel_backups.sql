-- Backups of the panel itself (its database and the master key encrypted
-- with the owner's recovery passphrase). The policy and the encrypted key
-- are settings: panel_backup_policy (JSON), recovery_key.
CREATE TABLE panel_backups (
    id          INTEGER PRIMARY KEY,
    storage_id  INTEGER REFERENCES backup_storages (id) ON DELETE SET NULL,
    trigger     TEXT    NOT NULL CHECK (trigger IN ('manual', 'schedule')),
    status      TEXT    NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
    error       TEXT    NOT NULL DEFAULT '',
    log         TEXT    NOT NULL DEFAULT '',
    snapshot    TEXT    NOT NULL DEFAULT '',
    bytes       INTEGER NOT NULL DEFAULT 0,
    actor_id    INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    finished_at INTEGER
) STRICT;
CREATE UNIQUE INDEX panel_backups_running ON panel_backups (status) WHERE status = 'running';
