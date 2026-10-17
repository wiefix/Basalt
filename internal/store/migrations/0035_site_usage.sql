-- Disk space a site takes (files, and its MariaDB databases), when it was
-- measured, and an optional soft quota with whether it was reported.
ALTER TABLE sites ADD COLUMN disk_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sites ADD COLUMN disk_files INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sites ADD COLUMN db_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sites ADD COLUMN usage_at INTEGER;
ALTER TABLE sites ADD COLUMN quota_mb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sites ADD COLUMN quota_alerted INTEGER NOT NULL DEFAULT 0;
