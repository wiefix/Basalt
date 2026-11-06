-- Floating IPs (22.1): a cloud account holds a provider's API token
-- (sealed by the master key); a floating IP of the account is configured
-- on the interfaces of the servers of its group (floating_ip_servers, in
-- the order failover tries them), and the provider routes it to one of
-- them (server_id). provider_server_id is the provider's id of the server,
-- matched by its addresses when it joined the group.
CREATE TABLE cloud_accounts (
	id         INTEGER PRIMARY KEY,
	provider   TEXT NOT NULL,
	name       TEXT NOT NULL UNIQUE,
	secret     BLOB NOT NULL,
	created_at INTEGER NOT NULL DEFAULT (unixepoch()),
	updated_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE floating_ips (
	id          INTEGER PRIMARY KEY,
	account_id  INTEGER NOT NULL REFERENCES cloud_accounts (id) ON DELETE RESTRICT,
	provider_id TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	address     TEXT NOT NULL UNIQUE,
	network     TEXT NOT NULL,
	location    TEXT NOT NULL DEFAULT '',
	server_id   INTEGER REFERENCES servers (id) ON DELETE SET NULL,
	assigned_at INTEGER,
	created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
	UNIQUE (account_id, provider_id)
);

CREATE TABLE floating_ip_servers (
	floating_ip_id     INTEGER NOT NULL REFERENCES floating_ips (id) ON DELETE CASCADE,
	server_id          INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
	position           INTEGER NOT NULL,
	provider_server_id TEXT NOT NULL,
	PRIMARY KEY (floating_ip_id, server_id)
);
CREATE INDEX floating_ip_servers_server ON floating_ip_servers (server_id);

-- Why the server's agent could not configure its floating IPs ("": it did).
ALTER TABLE servers ADD COLUMN floating_error TEXT NOT NULL DEFAULT '';
