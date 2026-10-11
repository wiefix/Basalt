-- DNSSEC signing key of a zone: the public DNSKEY record and the private
-- key, encrypted with the master key. NULL/empty: the zone is not signed.
ALTER TABLE dns_zones ADD COLUMN dnssec_dnskey TEXT NOT NULL DEFAULT '';
ALTER TABLE dns_zones ADD COLUMN dnssec_key BLOB;
