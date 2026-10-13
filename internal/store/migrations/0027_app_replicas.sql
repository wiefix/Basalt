-- App replicas: an app runs on several servers. Each placement is a route
-- of the app on that server (the app's own server_id is the primary); a
-- replicated app's routes each follow a health check, so DNS stops
-- pointing at a server whose copy does not answer.
DROP INDEX proxy_routes_app;
CREATE UNIQUE INDEX proxy_routes_app ON proxy_routes (app_id, server_id) WHERE app_id IS NOT NULL;
ALTER TABLE proxy_routes ADD COLUMN health_check_id INTEGER REFERENCES health_checks (id) ON DELETE SET NULL;
-- The port the server's proxy serves HTTPS on, from the agent's hello.
ALTER TABLE servers ADD COLUMN https_port INTEGER NOT NULL DEFAULT 0;
