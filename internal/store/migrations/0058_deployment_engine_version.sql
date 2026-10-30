-- The major version a database's deployment ran: a rollback must not put
-- an older version on data a newer one has upgraded. Every deployment so
-- far ran its app's present version (there were no upgrades before).
ALTER TABLE deployments ADD COLUMN engine_version TEXT NOT NULL DEFAULT '';
UPDATE deployments SET engine_version = (SELECT a.engine_version FROM apps a WHERE a.id = deployments.app_id)
 WHERE app_id IN (SELECT id FROM apps WHERE source = 'database');
