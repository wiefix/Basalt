-- Apps built from a git repository instead of a ready image.
ALTER TABLE apps ADD COLUMN source TEXT NOT NULL DEFAULT 'image' CHECK (source IN ('image', 'git'));
ALTER TABLE apps ADD COLUMN git_url TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN git_branch TEXT NOT NULL DEFAULT '';
-- Paths inside the repository.
ALTER TABLE apps ADD COLUMN git_dockerfile TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN git_context TEXT NOT NULL DEFAULT '';
-- Deploy key: OpenSSH ed25519 private key encrypted with the master key
-- (associated data "app-deploy-key:<id>"), and its public half.
ALTER TABLE apps ADD COLUMN deploy_key BLOB;
ALTER TABLE apps ADD COLUMN deploy_key_public TEXT NOT NULL DEFAULT '';
-- Webhook secret, encrypted ("app-webhook:<id>"); pushes to the branch deploy
-- when auto_deploy is on.
ALTER TABLE apps ADD COLUMN webhook_secret BLOB;
ALTER TABLE apps ADD COLUMN auto_deploy INTEGER NOT NULL DEFAULT 0 CHECK (auto_deploy IN (0, 1));

-- The commit a git deployment was built from.
ALTER TABLE deployments ADD COLUMN commit_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN commit_message TEXT NOT NULL DEFAULT '';
