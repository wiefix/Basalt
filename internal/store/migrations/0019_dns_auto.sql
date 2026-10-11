-- Hash of a zone's automatic records (A/AAAA for site and app domains), so
-- the serial changes whenever they do.
ALTER TABLE dns_zones ADD COLUMN auto_hash TEXT NOT NULL DEFAULT '';

-- Public addresses the agent found on its interfaces (JSON array), and the
-- ones an admin set instead (empty: use the detected ones).
ALTER TABLE servers ADD COLUMN detected_ips TEXT NOT NULL DEFAULT '[]';
ALTER TABLE servers ADD COLUMN public_ips TEXT NOT NULL DEFAULT '[]';
