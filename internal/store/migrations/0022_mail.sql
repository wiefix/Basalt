-- basalt:foreign-keys-off
-- The mail server (Stalwart) runs as an app with one container, like the
-- databases. The apps table is rebuilt to allow the new source and to hold
-- the mail server's host name.
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
    source            TEXT    NOT NULL DEFAULT 'image' CHECK (source IN ('image', 'git', 'compose', 'database', 'mail')),
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
    engine            TEXT    NOT NULL DEFAULT '',
    engine_version    TEXT    NOT NULL DEFAULT '',
    read_only         INTEGER NOT NULL DEFAULT 0 CHECK (read_only IN (0, 1)),
    -- Mail server: the host name it answers to (SMTP greeting, MX target).
    hostname          TEXT    NOT NULL DEFAULT ''
) STRICT;

INSERT INTO apps_new (id, server_id, name, image, port, env, volumes, memory_mb, cpus, created_at, source,
    git_url, git_branch, git_dockerfile, git_context, deploy_key, deploy_key_public, webhook_secret, auto_deploy,
    compose, compose_summary, service, engine, engine_version, read_only)
SELECT id, server_id, name, image, port, env, volumes, memory_mb, cpus, created_at, source,
    git_url, git_branch, git_dockerfile, git_context, deploy_key, deploy_key_public, webhook_secret, auto_deploy,
    compose, compose_summary, service, engine, engine_version, read_only
FROM apps;

DROP TABLE apps;
ALTER TABLE apps_new RENAME TO apps;
CREATE INDEX apps_server ON apps (server_id);
-- One mail server per panel.
CREATE UNIQUE INDEX apps_mail ON apps (source) WHERE source = 'mail';

-- Mail domains served by the mail server. Mailboxes live in the mail
-- server itself; the panel keeps the DNS records it asks for.
CREATE TABLE mail_domains (
    id          INTEGER PRIMARY KEY,
    app_id      INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    name        TEXT    NOT NULL UNIQUE,
    -- The domain's ID in the mail server.
    remote_id   TEXT    NOT NULL DEFAULT '',
    -- JSON array of dns.Record (relative names are not used: FQDNs).
    dns_records TEXT    NOT NULL DEFAULT '[]',
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;
CREATE INDEX mail_domains_app ON mail_domains (app_id);
