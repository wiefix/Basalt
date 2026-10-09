CREATE TABLE settings (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;
