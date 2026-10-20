-- A site can be served by several servers: one route per server, the
-- primary's files copied one way to the others. A replica route records
-- when its files last matched the primary's and the error of the last try.
DROP INDEX proxy_routes_site;
CREATE UNIQUE INDEX proxy_routes_site ON proxy_routes (site_id, server_id) WHERE site_id IS NOT NULL;
ALTER TABLE proxy_routes ADD COLUMN synced_at INTEGER;
ALTER TABLE proxy_routes ADD COLUMN sync_error TEXT NOT NULL DEFAULT '';
