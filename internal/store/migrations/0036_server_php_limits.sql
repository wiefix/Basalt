-- Whether each site's PHP runs as its own systemd service on the server,
-- so its CPU and memory limits apply.
ALTER TABLE servers ADD COLUMN php_limits INTEGER NOT NULL DEFAULT 0;
