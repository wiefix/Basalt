-- Compose apps: other services with domains of their own, besides the
-- published one, as JSON [{"service", "port"}]. Their domains are rows of
-- proxy_route_domains with the service ('' for the published service and
-- every other kind of route).
ALTER TABLE apps ADD COLUMN routes TEXT NOT NULL DEFAULT '[]';
ALTER TABLE proxy_route_domains ADD COLUMN service TEXT NOT NULL DEFAULT '';
