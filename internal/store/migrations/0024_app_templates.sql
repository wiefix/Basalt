-- The catalog template an app was installed from, and its revision.
ALTER TABLE apps ADD COLUMN template TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN template_revision INTEGER NOT NULL DEFAULT 0;
