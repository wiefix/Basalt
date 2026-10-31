-- Old audit entries can be removed after a retention period. The chain then
-- starts at the oldest entry kept: its base holds that entry's number and
-- the hash of the entry before it, signed with the audit key, so removing
-- more entries from the start (or keeping an older base) breaks the chain.
CREATE TABLE audit_log_base (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    first_id  INTEGER NOT NULL, -- the oldest entry kept
    prev_hash BLOB,             -- hash of the entry before it (NULL: unsigned)
    pruned    INTEGER NOT NULL, -- entries removed so far
    pruned_at INTEGER NOT NULL,
    mac       BLOB    NOT NULL
) STRICT;
