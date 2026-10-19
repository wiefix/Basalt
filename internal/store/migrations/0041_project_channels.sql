-- basalt:foreign-keys-off
-- A notification channel can belong to a project: it then receives only
-- the events about that project's resources, and the project's admins
-- manage it. Names are unique per project (and among the panel's own
-- channels), so the table is rebuilt without its UNIQUE on name.
CREATE TABLE notification_channels_new (
    id            INTEGER PRIMARY KEY,
    -- NULL: a channel of the whole panel (every event).
    project_id    INTEGER REFERENCES projects (id) ON DELETE CASCADE,
    name          TEXT    NOT NULL,
    kind          TEXT    NOT NULL CHECK (kind IN ('webhook', 'slack', 'discord', 'telegram', 'email')),
    config        BLOB    NOT NULL,
    events        TEXT    NOT NULL DEFAULT '[]', -- JSON array of event names
    last_sent_at  INTEGER,
    last_error    TEXT    NOT NULL DEFAULT '',
    last_error_at INTEGER,
    created_at    INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

INSERT INTO notification_channels_new (id, name, kind, config, events, last_sent_at, last_error, last_error_at, created_at)
SELECT id, name, kind, config, events, last_sent_at, last_error, last_error_at, created_at FROM notification_channels;

DROP TABLE notification_channels;
ALTER TABLE notification_channels_new RENAME TO notification_channels;
CREATE UNIQUE INDEX notification_channels_name ON notification_channels (COALESCE(project_id, 0), name);
CREATE INDEX notification_channels_project ON notification_channels (project_id);
