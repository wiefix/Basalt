-- Empty for static sites; e.g. "8.4" for PHP sites.
ALTER TABLE sites ADD COLUMN php_version TEXT NOT NULL DEFAULT '';
-- Reported by the agent: installed PHP-FPM versions (JSON array) and
-- whether sites run as separate system users.
ALTER TABLE servers ADD COLUMN php_versions TEXT NOT NULL DEFAULT '[]';
ALTER TABLE servers ADD COLUMN isolated INTEGER NOT NULL DEFAULT 0;
