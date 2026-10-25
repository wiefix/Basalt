-- Preview deployments: an app built through a GitHub App gets a copy per
-- pull request into its branch (an app with preview_of = the app and
-- pr_number), served at pr-<number>.<preview_domain> of the app ("": no
-- previews). A preview goes with its app.
ALTER TABLE apps ADD COLUMN preview_domain TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN preview_of INTEGER REFERENCES apps (id) ON DELETE CASCADE;
ALTER TABLE apps ADD COLUMN pr_number INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX apps_preview ON apps (preview_of, pr_number) WHERE preview_of IS NOT NULL;
