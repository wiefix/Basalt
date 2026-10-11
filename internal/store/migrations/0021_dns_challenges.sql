-- DNS-01 challenge records a server's proxy asked for, served as TXT
-- records in the zone until removed or expired.
CREATE TABLE dns_challenges (
    id         INTEGER PRIMARY KEY,
    server_id  INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    expires_at INTEGER NOT NULL,
    UNIQUE (server_id, name, value)
) STRICT;
