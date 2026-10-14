-- Notification channels: where panel events are sent. The settings,
-- secrets included (tokens, passwords, webhook URLs), are encrypted.
CREATE TABLE notification_channels (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL UNIQUE,
    kind         TEXT    NOT NULL CHECK (kind IN ('webhook', 'slack', 'discord', 'telegram', 'email')),
    config       BLOB    NOT NULL,
    events       TEXT    NOT NULL DEFAULT '[]', -- JSON array of event names
    last_sent_at INTEGER,
    last_error   TEXT    NOT NULL DEFAULT '',
    last_error_at INTEGER,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;
