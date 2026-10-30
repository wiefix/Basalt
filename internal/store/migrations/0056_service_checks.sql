-- Replicated compose apps: the health check each server's copy of another
-- service with domains of its own follows (that of the published service
-- is proxy_routes.health_check_id). The checks are deleted with the route
-- by the code that removes it.
CREATE TABLE proxy_route_checks (
	route_id        INTEGER NOT NULL REFERENCES proxy_routes(id) ON DELETE CASCADE,
	service         TEXT NOT NULL,
	health_check_id INTEGER NOT NULL REFERENCES health_checks(id) ON DELETE CASCADE,
	PRIMARY KEY (route_id, service)
);
CREATE INDEX proxy_route_checks_check ON proxy_route_checks(health_check_id);
