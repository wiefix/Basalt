-- Docker Engine version reported by the agent; empty when unavailable.
ALTER TABLE servers ADD COLUMN docker TEXT NOT NULL DEFAULT '';
