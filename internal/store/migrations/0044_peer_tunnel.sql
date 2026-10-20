-- The port of a server's agent-to-agent tunnel (0: off) and why it could
-- not listen.
ALTER TABLE servers ADD COLUMN peer_port INTEGER NOT NULL DEFAULT 0;
ALTER TABLE servers ADD COLUMN peer_error TEXT NOT NULL DEFAULT '';
