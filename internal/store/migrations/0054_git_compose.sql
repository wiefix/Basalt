-- Compose apps from git: the compose file a git app runs ('' for apps
-- built from a Dockerfile or by Railpack). Such an app's builder reads as
-- 'compose'; git_builder stays '' (its CHECK predates the builder).
ALTER TABLE apps ADD COLUMN git_compose_file TEXT NOT NULL DEFAULT '';
