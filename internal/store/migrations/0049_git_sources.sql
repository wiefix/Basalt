-- Git sources: a GitHub App the panel created on the user's account or
-- organization (manifest flow). Its credentials (client secret, webhook
-- secret, private key) are one encrypted JSON blob. Apps built from a
-- repository it reaches name it and the repository.
CREATE TABLE git_sources (
    id         INTEGER PRIMARY KEY,
    kind       TEXT    NOT NULL CHECK (kind IN ('github')),
    name       TEXT    NOT NULL UNIQUE,
    app_id     INTEGER NOT NULL,
    slug       TEXT    NOT NULL,
    owner      TEXT    NOT NULL DEFAULT '',
    html_url   TEXT    NOT NULL DEFAULT '',
    secrets    BLOB    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

ALTER TABLE apps ADD COLUMN git_source_id INTEGER REFERENCES git_sources (id) ON DELETE SET NULL;
ALTER TABLE apps ADD COLUMN git_repo TEXT NOT NULL DEFAULT '';
CREATE INDEX apps_git_repo ON apps (git_repo) WHERE git_repo <> '';
