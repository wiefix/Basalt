-- Whether the server's agent can update Basalt there (installed with the
-- installer's systemd units).
ALTER TABLE servers ADD COLUMN updatable INTEGER NOT NULL DEFAULT 0;
