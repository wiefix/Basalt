-- Notifications not delivered yet: one row per channel and message, gone
-- once delivered or given up. They outlive a restart of the panel.
CREATE TABLE notification_queue (
    id         INTEGER PRIMARY KEY,
    channel_id INTEGER NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
    message    TEXT    NOT NULL, -- the message as JSON
    attempts   INTEGER NOT NULL DEFAULT 0,
    -- No attempt before this; also set ahead while an attempt runs, so
    -- that nothing else picks the row up meanwhile.
    next_at    INTEGER NOT NULL,
    last_error TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;
CREATE INDEX notification_queue_next ON notification_queue (next_at);
CREATE INDEX notification_queue_channel ON notification_queue (channel_id);
