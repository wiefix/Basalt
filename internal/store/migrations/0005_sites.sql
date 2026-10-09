CREATE TABLE sites (
    id         INTEGER PRIMARY KEY,
    server_id  INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    name       TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    kind       TEXT    NOT NULL CHECK (kind IN ('static')),
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

CREATE INDEX sites_server ON sites (server_id);

-- Routes that serve a managed site; the agent resolves their directory.
ALTER TABLE proxy_routes ADD COLUMN site_id INTEGER REFERENCES sites (id) ON DELETE CASCADE;
CREATE UNIQUE INDEX proxy_routes_site ON proxy_routes (site_id) WHERE site_id IS NOT NULL;
