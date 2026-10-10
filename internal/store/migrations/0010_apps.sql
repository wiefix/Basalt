CREATE TABLE apps (
    id         INTEGER PRIMARY KEY,
    server_id  INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    image      TEXT    NOT NULL,
    -- HTTP port inside the container; 0 when the app is not served.
    port       INTEGER NOT NULL DEFAULT 0,
    -- JSON object of environment variables, encrypted with the master key
    -- (associated data "app-env:<id>"); NULL when there are none.
    env        BLOB,
    -- JSON array of {"name","path"} persistent volumes.
    volumes    TEXT    NOT NULL DEFAULT '[]',
    memory_mb  INTEGER NOT NULL DEFAULT 0,
    cpus       REAL    NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

CREATE INDEX apps_server ON apps (server_id);

CREATE TABLE deployments (
    id           INTEGER PRIMARY KEY,
    app_id       INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    -- Image reference deployed; a rollback uses the digest of an earlier one.
    image        TEXT    NOT NULL,
    status       TEXT    NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
    error        TEXT    NOT NULL DEFAULT '',
    log          TEXT    NOT NULL DEFAULT '',
    image_digest TEXT    NOT NULL DEFAULT '',
    actor_id     INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    finished_at  INTEGER
) STRICT;

CREATE INDEX deployments_app ON deployments (app_id, id);
-- At most one deployment of an app runs at a time.
CREATE UNIQUE INDEX deployments_running ON deployments (app_id) WHERE status = 'running';

-- Routes that serve a managed app; the agent resolves their upstream.
ALTER TABLE proxy_routes ADD COLUMN app_id INTEGER REFERENCES apps (id) ON DELETE CASCADE;
CREATE UNIQUE INDEX proxy_routes_app ON proxy_routes (app_id) WHERE app_id IS NOT NULL;
