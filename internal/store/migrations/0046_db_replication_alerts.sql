-- Since when a replica's copying from a source has been failing (0: it
-- works) and whether that was notified.
ALTER TABLE db_replication ADD COLUMN failing_since INTEGER NOT NULL DEFAULT 0;
ALTER TABLE db_replication ADD COLUMN alerted INTEGER NOT NULL DEFAULT 0;
