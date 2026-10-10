-- basalt:foreign-keys-off
-- Apps from a docker-compose file. SQLite cannot change a CHECK constraint
-- in place, so the apps table is rebuilt (foreign keys are off meanwhile,
-- otherwise dropping the old table would cascade to deployments and routes).
CREATE TABLE apps_new (
    id                INTEGER PRIMARY KEY,
    server_id         INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    name              TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    image             TEXT    NOT NULL,
    port              INTEGER NOT NULL DEFAULT 0,
    env               BLOB,
    volumes           TEXT    NOT NULL DEFAULT '[]',
    memory_mb         INTEGER NOT NULL DEFAULT 0,
    cpus              REAL    NOT NULL DEFAULT 0,
    created_at        INTEGER NOT NULL DEFAULT (unixepoch()),
    source            TEXT    NOT NULL DEFAULT 'image' CHECK (source IN ('image', 'git', 'compose')),
    git_url           TEXT    NOT NULL DEFAULT '',
    git_branch        TEXT    NOT NULL DEFAULT '',
    git_dockerfile    TEXT    NOT NULL DEFAULT '',
    git_context       TEXT    NOT NULL DEFAULT '',
    deploy_key        BLOB,
    deploy_key_public TEXT    NOT NULL DEFAULT '',
    webhook_secret    BLOB,
    auto_deploy       INTEGER NOT NULL DEFAULT 0 CHECK (auto_deploy IN (0, 1)),
    -- The compose file, encrypted with the master key (associated data
    -- "app-compose:<id>"): such files often hold passwords.
    compose           BLOB,
    -- JSON summary of the compose file for display: services (name, image)
    -- and volume names. No secrets.
    compose_summary   TEXT    NOT NULL DEFAULT '{}',
    -- The compose service the app's domains and port belong to.
    service           TEXT    NOT NULL DEFAULT ''
) STRICT;

INSERT INTO apps_new (id, server_id, name, image, port, env, volumes, memory_mb, cpus, created_at, source,
    git_url, git_branch, git_dockerfile, git_context, deploy_key, deploy_key_public, webhook_secret, auto_deploy)
SELECT id, server_id, name, image, port, env, volumes, memory_mb, cpus, created_at, source,
    git_url, git_branch, git_dockerfile, git_context, deploy_key, deploy_key_public, webhook_secret, auto_deploy
FROM apps;

DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;
CREATE INDEX apps_server ON apps (server_id);

-- The compose file a deployment ran (encrypted, "deployment-compose:<id>")
-- and the image each service ran, so a rollback restores both.
ALTER TABLE deployments ADD COLUMN compose BLOB;
ALTER TABLE deployments ADD COLUMN digests TEXT NOT NULL DEFAULT '{}';
