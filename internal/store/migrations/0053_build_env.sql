-- Variables a git app's build gets (names of its environment variables, a
-- JSON array): passed to the build as secrets, not kept in the image.
ALTER TABLE apps ADD COLUMN build_env TEXT NOT NULL DEFAULT '[]';
