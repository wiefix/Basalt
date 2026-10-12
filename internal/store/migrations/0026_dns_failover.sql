-- DNS failover: an A/AAAA record may follow a health check; while the
-- check is down the record is left out of the zone (see dns.Failover).
ALTER TABLE dns_records ADD COLUMN health_check_id INTEGER REFERENCES health_checks (id) ON DELETE SET NULL;
CREATE INDEX dns_records_health_check ON dns_records (health_check_id) WHERE health_check_id IS NOT NULL;
