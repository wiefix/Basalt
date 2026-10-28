-- Builds without a Dockerfile: a git app's builder ('' builds its
-- Dockerfile, 'railpack' works out how to build it), and the version of
-- Railpack on each server ('' when it cannot build that way).
ALTER TABLE apps ADD COLUMN git_builder TEXT NOT NULL DEFAULT '' CHECK (git_builder IN ('', 'railpack'));
ALTER TABLE servers ADD COLUMN railpack TEXT NOT NULL DEFAULT '';
