-- Version of restic on the server ("" when backups cannot run there).
ALTER TABLE servers ADD COLUMN restic TEXT NOT NULL DEFAULT '';

-- Where backups go. Each server keeps its own restic repository under
-- path/server-<id> of a storage.
CREATE TABLE backup_storages (
    id             INTEGER PRIMARY KEY,
    name           TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    kind           TEXT    NOT NULL CHECK (kind IN ('s3', 'sftp', 'local')),
    -- JSON without secrets: endpoint, bucket, region, access_key (S3);
    -- host, port, user (SFTP); path (all).
    config         TEXT    NOT NULL DEFAULT '{}',
    -- S3 secret key or SFTP private key, encrypted ("backup-storage-secret:<id>").
    secret         BLOB,
    ssh_public_key TEXT    NOT NULL DEFAULT '',
    -- Password of the repositories, encrypted ("backup-storage-password:<id>").
    password       BLOB,
    created_at     INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

-- What is backed up where and when: one policy per site or app.
CREATE TABLE backup_policies (
    id           INTEGER PRIMARY KEY,
    site_id      INTEGER UNIQUE REFERENCES sites (id) ON DELETE CASCADE,
    app_id       INTEGER UNIQUE REFERENCES apps (id) ON DELETE CASCADE,
    storage_id   INTEGER NOT NULL REFERENCES backup_storages (id) ON DELETE RESTRICT,
    -- manual, hourly (at minute at_minute % 60) or daily (at minute
    -- at_minute of the day, UTC).
    schedule     TEXT    NOT NULL DEFAULT 'daily' CHECK (schedule IN ('manual', 'hourly', 'daily')),
    at_minute    INTEGER NOT NULL DEFAULT 180 CHECK (at_minute BETWEEN 0 AND 1439),
    keep_last    INTEGER NOT NULL DEFAULT 1,
    keep_daily   INTEGER NOT NULL DEFAULT 7,
    keep_weekly  INTEGER NOT NULL DEFAULT 4,
    keep_monthly INTEGER NOT NULL DEFAULT 6,
    updated_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    CHECK ((site_id IS NULL) <> (app_id IS NULL))
) STRICT;
CREATE INDEX backup_policies_storage ON backup_policies (storage_id);

-- Backup and restore runs.
CREATE TABLE backups (
    id          INTEGER PRIMARY KEY,
    site_id     INTEGER REFERENCES sites (id) ON DELETE CASCADE,
    app_id      INTEGER REFERENCES apps (id) ON DELETE CASCADE,
    server_id   INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    storage_id  INTEGER REFERENCES backup_storages (id) ON DELETE SET NULL,
    kind        TEXT    NOT NULL CHECK (kind IN ('backup', 'restore')),
    trigger     TEXT    NOT NULL CHECK (trigger IN ('manual', 'schedule')),
    status      TEXT    NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
    error       TEXT    NOT NULL DEFAULT '',
    log         TEXT    NOT NULL DEFAULT '',
    -- JSON list of {item, id, bytes, data_added} (backups) or {item, snapshot} (restores).
    snapshots   TEXT    NOT NULL DEFAULT '[]',
    bytes       INTEGER NOT NULL DEFAULT 0,
    data_added  INTEGER NOT NULL DEFAULT 0,
    actor_id    INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    finished_at INTEGER,
    CHECK ((site_id IS NULL) <> (app_id IS NULL))
) STRICT;
CREATE INDEX backups_site ON backups (site_id, id);
CREATE INDEX backups_app ON backups (app_id, id);
-- One backup or restore of a resource at a time.
CREATE UNIQUE INDEX backups_running_site ON backups (site_id) WHERE status = 'running' AND site_id IS NOT NULL;
CREATE UNIQUE INDEX backups_running_app ON backups (app_id) WHERE status = 'running' AND app_id IS NOT NULL;

-- When each repository (storage, server) last had its space freed.
CREATE TABLE backup_prunes (
    storage_id INTEGER NOT NULL REFERENCES backup_storages (id) ON DELETE CASCADE,
    server_id  INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    pruned_at  INTEGER NOT NULL,
    PRIMARY KEY (storage_id, server_id)
) STRICT;
