-- Replication of site databases between servers. A server that is a
-- source has a replication password (encrypted); each replica records how
-- copying from each source goes.
ALTER TABLE servers ADD COLUMN repl_password BLOB;
CREATE TABLE db_replication (
    replica_id  INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    source_id   INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    running     INTEGER NOT NULL DEFAULT 0,
    lag         INTEGER NOT NULL DEFAULT -1,
    error       TEXT    NOT NULL DEFAULT '',
    databases   TEXT    NOT NULL DEFAULT '[]', -- JSON: copied (seeded)
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (replica_id, source_id)
) STRICT;
