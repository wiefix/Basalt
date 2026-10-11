CREATE TABLE dns_zones (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE,
    -- SOA serial, YYYYMMDDnn; bumped on every change of the zone.
    serial     INTEGER NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

CREATE TABLE dns_records (
    id         INTEGER PRIMARY KEY,
    zone_id    INTEGER NOT NULL REFERENCES dns_zones (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    type       TEXT    NOT NULL,
    ttl        INTEGER NOT NULL,
    value      TEXT    NOT NULL,
    priority   INTEGER NOT NULL DEFAULT 0,
    weight     INTEGER NOT NULL DEFAULT 0,
    port       INTEGER NOT NULL DEFAULT 0,
    flags      INTEGER NOT NULL DEFAULT 0,
    tag        TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

CREATE INDEX dns_records_zone ON dns_records (zone_id);

-- Every agent receives all zones; servers with a DNS listener serve them.
ALTER TABLE servers ADD COLUMN dns_port INTEGER NOT NULL DEFAULT 0;
ALTER TABLE servers ADD COLUMN dns_listen_error TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN dns_applied_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE servers ADD COLUMN dns_error TEXT NOT NULL DEFAULT '';
