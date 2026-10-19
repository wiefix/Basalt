-- Limits of a project (JSON ProjectLimits; 0 or missing: no limit) set by
-- the panel's admins, and whether the project's admins were told that its
-- sites went over its disk limit.
ALTER TABLE projects ADD COLUMN limits TEXT NOT NULL DEFAULT '{}';
ALTER TABLE projects ADD COLUMN disk_alerted INTEGER NOT NULL DEFAULT 0;
