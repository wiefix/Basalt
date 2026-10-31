-- TLS on the public ports of container databases: public_require_tls
-- refuses clients that do not switch to TLS; each server presents its own
-- certificate (DER), issued by the panel's database CA, with its key sealed
-- by the master key.
ALTER TABLE apps ADD COLUMN public_require_tls INTEGER NOT NULL DEFAULT 0;
ALTER TABLE servers ADD COLUMN db_tls_cert BLOB;
ALTER TABLE servers ADD COLUMN db_tls_key BLOB;
