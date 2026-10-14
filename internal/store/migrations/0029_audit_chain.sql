-- basalt:foreign-keys-off
-- The audit log becomes a hash chain: each entry carries an HMAC of its
-- contents and of the previous entry's hash, keyed from the master key, so
-- an entry changed or removed in the database is detected. actor_id loses
-- its foreign key: deleting a user must not rewrite past entries.
CREATE TABLE audit_log_new (
    id         INTEGER PRIMARY KEY,
    at         INTEGER NOT NULL,
    actor_id   INTEGER,
    action     TEXT    NOT NULL,
    ip         TEXT    NOT NULL DEFAULT '',
    -- JSON object with action-specific details. Never contains secrets.
    details    TEXT    NOT NULL DEFAULT '{}',
    -- HMAC-SHA256 chain (see store.auditHash); NULL for entries written
    -- before the chain existed.
    hash       BLOB
) STRICT;

INSERT INTO audit_log_new (id, at, actor_id, action, ip, details)
SELECT id, at, actor_id, action, ip, details FROM audit_log;

DROP TABLE audit_log;
ALTER TABLE audit_log_new RENAME TO audit_log;
CREATE INDEX audit_log_at ON audit_log (at);
CREATE INDEX audit_log_actor ON audit_log (actor_id, id);
CREATE INDEX audit_log_action ON audit_log (action, id);

-- Entries are only ever added.
CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'the audit log is append-only');
END;
CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'the audit log is append-only');
END;
