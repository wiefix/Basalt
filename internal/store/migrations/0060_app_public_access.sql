-- A container database can be reachable from outside its server: its
-- agent listens on public_port (0: not public) and forwards connections
-- from the addresses in public_allow (JSON list of CIDR prefixes).
ALTER TABLE apps ADD COLUMN public_port INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN public_allow TEXT NOT NULL DEFAULT '[]';
CREATE UNIQUE INDEX apps_public_port ON apps (server_id, public_port) WHERE public_port > 0;
