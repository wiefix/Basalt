-- Run an app with a read-only root filesystem (writable /tmp and /run).
ALTER TABLE apps ADD COLUMN read_only INTEGER NOT NULL DEFAULT 0 CHECK (read_only IN (0, 1));

-- How the server's Docker daemon isolates containers (JSON reported by the
-- agent: userns, seccomp, apparmor, ...); empty when unknown.
ALTER TABLE servers ADD COLUMN docker_security TEXT NOT NULL DEFAULT '';

-- Resource usage of each app's running containers, sampled with the
-- server's metrics and kept as long.
CREATE TABLE app_metrics (
    app_id    INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    at        INTEGER NOT NULL,
    cpu_pct   REAL    NOT NULL, -- percent of one CPU
    mem_used  INTEGER NOT NULL,
    mem_limit INTEGER NOT NULL,
    net_rx    INTEGER NOT NULL, -- bytes since the containers started
    net_tx    INTEGER NOT NULL,
    PRIMARY KEY (app_id, at)
) STRICT, WITHOUT ROWID;
CREATE INDEX app_metrics_at ON app_metrics (at);
