-- basalt:foreign-keys-off
-- Databases (PostgreSQL, MySQL, MariaDB, Redis) run as apps with one
-- container. The apps table is rebuilt to allow the new source.
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
    source            TEXT    NOT NULL DEFAULT 'image' CHECK (source IN ('image', 'git', 'compose', 'database')),
    git_url           TEXT    NOT NULL DEFAULT '',
    git_branch        TEXT    NOT NULL DEFAULT '',
    git_dockerfile    TEXT    NOT NULL DEFAULT '',
    git_context       TEXT    NOT NULL DEFAULT '',
    deploy_key        BLOB,
    deploy_key_public TEXT    NOT NULL DEFAULT '',
    webhook_secret    BLOB,
    auto_deploy       INTEGER NOT NULL DEFAULT 0 CHECK (auto_deploy IN (0, 1)),
    compose           BLOB,
    compose_summary   TEXT    NOT NULL DEFAULT '{}',
    service           TEXT    NOT NULL DEFAULT '',
    -- Databases: the engine (postgres, mysql, mariadb, redis) and its
    -- major version. Credentials are the app's (encrypted) environment.
    engine            TEXT    NOT NULL DEFAULT '',
    engine_version    TEXT    NOT NULL DEFAULT ''
) STRICT;

INSERT INTO apps_new (id, server_id, name, image, port, env, volumes, memory_mb, cpus, created_at, source,
    git_url, git_branch, git_dockerfile, git_context, deploy_key, deploy_key_public, webhook_secret, auto_deploy,
    compose, compose_summary, service)
SELECT id, server_id, name, image, port, env, volumes, memory_mb, cpus, created_at, source,
    git_url, git_branch, git_dockerfile, git_context, deploy_key, deploy_key_public, webhook_secret, auto_deploy,
    compose, compose_summary, service
FROM apps;

DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;
CREATE INDEX apps_server ON apps (server_id);

-- Apps a database is reachable from: its container joins their networks.
CREATE TABLE app_links (
    database_id INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    app_id      INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    PRIMARY KEY (database_id, app_id)
) STRICT;
CREATE INDEX app_links_app ON app_links (app_id);
