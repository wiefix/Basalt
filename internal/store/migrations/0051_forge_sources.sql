-- basalt:foreign-keys-off
-- Git sources on GitLab and Gitea (or Forgejo): an instance (base_url) and
-- an access token (in secrets). Apps from them keep the ids of the deploy
-- key and push webhook the panel added to their repository, to remove them.
CREATE TABLE git_sources_new (
    id         INTEGER PRIMARY KEY,
    kind       TEXT    NOT NULL CHECK (kind IN ('github', 'gitlab', 'gitea')),
    name       TEXT    NOT NULL UNIQUE,
    app_id     INTEGER NOT NULL,
    slug       TEXT    NOT NULL,
    owner      TEXT    NOT NULL DEFAULT '',
    html_url   TEXT    NOT NULL DEFAULT '',
    base_url   TEXT    NOT NULL DEFAULT '',
    secrets    BLOB    NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;
INSERT INTO git_sources_new (id, kind, name, app_id, slug, owner, html_url, secrets, created_at)
    SELECT id, kind, name, app_id, slug, owner, html_url, secrets, created_at FROM git_sources;
DROP TABLE git_sources;
ALTER TABLE git_sources_new RENAME TO git_sources;

ALTER TABLE apps ADD COLUMN forge_key_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN forge_hook_id INTEGER NOT NULL DEFAULT 0;
