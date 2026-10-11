package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/dns"
)

const (
	settingDNSRevision    = "dns_revision"
	settingDNSNameservers = "dns_nameservers"
)

type DNSZone struct {
	ID      int64
	Name    string
	Serial  uint32
	Records int
	// DNSKEY is the zone's public signing key ("" when not signed).
	DNSKEY    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type DNSRecord struct {
	ID     int64
	ZoneID int64
	dns.Record
	CreatedAt time.Time
}

// DNSCheck validates the records a zone would hold after a change.
type DNSCheck func(z *dns.Zone) error

// nextSerial is the zone serial after a change: today's YYYYMMDD00, or
// one more than the current serial if that is already higher.
func nextSerial(cur uint32, now time.Time) uint32 {
	d, _ := strconv.ParseUint(now.UTC().Format("20060102")+"00", 10, 32)
	return max(cur+1, uint32(d))
}

func bumpDNSRevision(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, '1')
		 ON CONFLICT (key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT), updated_at = unixepoch()`,
		settingDNSRevision)
	return err
}

func (db *DB) CreateDNSZone(ctx context.Context, name string, records []dns.Record, check DNSCheck, now time.Time) (*DNSZone, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	z := &DNSZone{Name: name, Serial: nextSerial(0, now), Records: len(records)}
	if err := check(&dns.Zone{Name: name, Serial: z.Serial, Records: records}); err != nil {
		return nil, err
	}
	var created int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO dns_zones (name, serial, created_at, updated_at) VALUES (?, ?, ?, ?) RETURNING id, created_at`,
		name, z.Serial, now.Unix(), now.Unix()).Scan(&z.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: dns_zones.name") {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	z.CreatedAt, z.UpdatedAt = time.Unix(created, 0), time.Unix(created, 0)
	for i := range records {
		if _, err := insertDNSRecord(ctx, tx, z.ID, &records[i], now); err != nil {
			return nil, err
		}
	}
	if err := bumpDNSRevision(ctx, tx); err != nil {
		return nil, err
	}
	return z, tx.Commit()
}

func insertDNSRecord(ctx context.Context, tx *sql.Tx, zoneID int64, r *dns.Record, now time.Time) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO dns_records (zone_id, name, type, ttl, value, priority, weight, port, flags, tag, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		zoneID, r.Name, r.Type, r.TTL, r.Value, r.Priority, r.Weight, r.Port, r.Flags, r.Tag, now.Unix()).Scan(&id)
	return id, err
}

const dnsZoneColumns = `z.id, z.name, z.serial, z.created_at, z.updated_at,
	(SELECT COUNT(*) FROM dns_records r WHERE r.zone_id = z.id), z.dnssec_dnskey`

func scanDNSZone(row interface{ Scan(...any) error }) (*DNSZone, error) {
	var z DNSZone
	var created, updated int64
	if err := row.Scan(&z.ID, &z.Name, &z.Serial, &created, &updated, &z.Records, &z.DNSKEY); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	z.CreatedAt, z.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &z, nil
}

func (db *DB) DNSZones(ctx context.Context) ([]*DNSZone, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+dnsZoneColumns+` FROM dns_zones z ORDER BY z.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DNSZone
	for rows.Next() {
		z, err := scanDNSZone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, z)
	}
	return out, rows.Err()
}

func (db *DB) DNSZone(ctx context.Context, id int64) (*DNSZone, error) {
	return scanDNSZone(db.R.QueryRowContext(ctx, `SELECT `+dnsZoneColumns+` FROM dns_zones z WHERE z.id = ?`, id))
}

const dnsRecordColumns = `id, zone_id, name, type, ttl, value, priority, weight, port, flags, tag, created_at`

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func dnsRecords(ctx context.Context, q queryer, zoneID int64) ([]*DNSRecord, error) {
	query := `SELECT ` + dnsRecordColumns + ` FROM dns_records`
	var args []any
	if zoneID > 0 {
		query += ` WHERE zone_id = ?`
		args = append(args, zoneID)
	}
	// Apex first, then by name and type; insertion order within a set.
	query += ` ORDER BY zone_id, name <> '@', name, type, id`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DNSRecord
	for rows.Next() {
		var r DNSRecord
		var created int64
		if err := rows.Scan(&r.ID, &r.ZoneID, &r.Name, &r.Type, &r.TTL, &r.Value, &r.Priority, &r.Weight,
			&r.Port, &r.Flags, &r.Tag, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(created, 0)
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (db *DB) DNSRecords(ctx context.Context, zoneID int64) ([]*DNSRecord, error) {
	return dnsRecords(ctx, db.R, zoneID)
}

// PutDNSRecord creates (id 0), replaces (rec set) or deletes (rec nil) one
// record of a zone. check sees the whole zone as it would be after the
// change, inside the write transaction, so concurrent edits cannot combine
// into an invalid zone.
func (db *DB) PutDNSRecord(ctx context.Context, zoneID, id int64, rec *dns.Record, check DNSCheck, now time.Time) (*DNSRecord, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var name string
	var serial uint32
	if err := tx.QueryRowContext(ctx, `SELECT name, serial FROM dns_zones WHERE id = ?`, zoneID).Scan(&name, &serial); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	current, err := dnsRecords(ctx, tx, zoneID)
	if err != nil {
		return nil, err
	}
	zone := dns.Zone{Name: name, Serial: nextSerial(serial, now)}
	found := id == 0
	for _, r := range current {
		if r.ID == id {
			found = true
			if rec == nil {
				continue
			}
			zone.Records = append(zone.Records, *rec)
			continue
		}
		zone.Records = append(zone.Records, r.Record)
	}
	if !found {
		return nil, ErrNotFound
	}
	if id == 0 {
		zone.Records = append(zone.Records, *rec)
	}
	if rec != nil {
		if err := check(&zone); err != nil {
			return nil, err
		}
	}
	out := &DNSRecord{ID: id, ZoneID: zoneID, CreatedAt: now}
	switch {
	case rec == nil:
		if _, err := tx.ExecContext(ctx, `DELETE FROM dns_records WHERE id = ?`, id); err != nil {
			return nil, err
		}
	case id == 0:
		if out.ID, err = insertDNSRecord(ctx, tx, zoneID, rec, now); err != nil {
			return nil, err
		}
	default:
		if _, err := tx.ExecContext(ctx,
			`UPDATE dns_records SET name = ?, type = ?, ttl = ?, value = ?, priority = ?, weight = ?, port = ?,
			 flags = ?, tag = ? WHERE id = ?`,
			rec.Name, rec.Type, rec.TTL, rec.Value, rec.Priority, rec.Weight, rec.Port, rec.Flags, rec.Tag, id); err != nil {
			return nil, err
		}
	}
	if rec != nil {
		out.Record = *rec
	}
	if _, err := tx.ExecContext(ctx, `UPDATE dns_zones SET serial = ?, updated_at = ? WHERE id = ?`,
		zone.Serial, now.Unix(), zoneID); err != nil {
		return nil, err
	}
	if err := bumpDNSRevision(ctx, tx); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// SetZoneDNSSEC stores a zone's signing key (empty dnskey: unsigned) and
// bumps its serial.
func (db *DB) SetZoneDNSSEC(ctx context.Context, id int64, dnskey string, sealed []byte, now time.Time) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var serial uint32
	if err := tx.QueryRowContext(ctx, `SELECT serial FROM dns_zones WHERE id = ?`, id).Scan(&serial); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if dnskey == "" {
		sealed = nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE dns_zones SET dnssec_dnskey = ?, dnssec_key = ?, serial = ?, updated_at = ? WHERE id = ?`,
		dnskey, sealed, nextSerial(serial, now), now.Unix(), id); err != nil {
		return err
	}
	if err := bumpDNSRevision(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) DeleteDNSZone(ctx context.Context, id int64) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM dns_zones WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := bumpDNSRevision(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Unseal decrypts a zone's private signing key.
type Unseal func(zoneID int64, sealed []byte) (string, error)

// DesiredDNS assembles every zone with its records, automatic ones
// included, and with the signing keys when unseal is given.
func (db *DB) DesiredDNS(ctx context.Context, unseal Unseal) (rev int64, zones []dns.Zone, err error) {
	tx, err := db.R.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	rev, zones, _, err = db.manualDNS(ctx, tx, unseal)
	if err != nil {
		return 0, nil, err
	}
	auto, err := autoDNS(ctx, tx, zones)
	if err != nil {
		return 0, nil, err
	}
	for i, auto := range auto {
		for _, r := range auto {
			zones[i].Records = append(zones[i].Records, r.Record)
		}
	}
	return rev, zones, nil
}

// manualDNS loads the revision and every zone with the records users made,
// with the zone IDs.
func (db *DB) manualDNS(ctx context.Context, q queryer, unseal Unseal) (rev int64, zones []dns.Zone, ids []int64, err error) {
	var v string
	rows, err := q.QueryContext(ctx, `SELECT value FROM settings WHERE key = ?`, settingDNSRevision)
	if err != nil {
		return 0, nil, nil, err
	}
	if rows.Next() {
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return 0, nil, nil, err
		}
		rev, _ = strconv.ParseInt(v, 10, 64)
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT id, name, serial, dnssec_dnskey, dnssec_key FROM dns_zones ORDER BY id`)
	if err != nil {
		return 0, nil, nil, err
	}
	index := map[int64]int{}
	for rows.Next() {
		var id int64
		var z dns.Zone
		var dnskey string
		var sealed []byte
		if err := rows.Scan(&id, &z.Name, &z.Serial, &dnskey, &sealed); err != nil {
			rows.Close()
			return 0, nil, nil, err
		}
		if dnskey != "" && unseal != nil {
			priv, err := unseal(id, sealed)
			if err != nil {
				rows.Close()
				return 0, nil, nil, fmt.Errorf("zone %s key: %w", z.Name, err)
			}
			z.DNSSEC = &dns.ZoneKey{DNSKEY: dnskey, PrivateKey: priv}
		}
		index[id] = len(zones)
		zones = append(zones, z)
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, nil, err
	}
	recs, err := dnsRecords(ctx, q, 0)
	if err != nil {
		return 0, nil, nil, err
	}
	for _, r := range recs {
		z := &zones[index[r.ZoneID]]
		z.Records = append(z.Records, r.Record)
	}
	if zones == nil {
		zones = []dns.Zone{}
	}
	return rev, zones, ids, nil
}

// DNSNameservers returns the host names new zones get NS records for.
func (db *DB) DNSNameservers(ctx context.Context) ([]string, error) {
	v, err := db.Setting(ctx, settingDNSNameservers)
	if errors.Is(err, ErrNotFound) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (db *DB) SetDNSNameservers(ctx context.Context, hosts []string) error {
	b, _ := json.Marshal(hosts)
	return db.SetSetting(ctx, settingDNSNameservers, string(b))
}

// SetServerDNS records whether a server serves DNS (from its hello).
func (db *DB) SetServerDNS(ctx context.Context, serverID int64, port int, listenErr string) error {
	return db.execOne(ctx, `UPDATE servers SET dns_port = ?, dns_listen_error = ? WHERE id = ?`, port, listenErr, serverID)
}

// SetDNSStatus records the result of delivering revision rev.
func (db *DB) SetDNSStatus(ctx context.Context, serverID, rev int64, applyErr string) error {
	if applyErr != "" {
		_, err := db.W.ExecContext(ctx, `UPDATE servers SET dns_error = ? WHERE id = ?`, applyErr, serverID)
		return err
	}
	_, err := db.W.ExecContext(ctx,
		`UPDATE servers SET dns_applied_revision = MAX(dns_applied_revision, ?), dns_error = '' WHERE id = ?`,
		rev, serverID)
	return err
}

// DNSServerStatus is how one server stands with DNS.
type DNSServerStatus struct {
	ServerID        int64
	Name            string
	Port            int
	ListenError     string
	AppliedRevision int64
	Error           string
	LastIP          string
}

// DNSServers lists enrolled servers with their DNS state.
func (db *DB) DNSServers(ctx context.Context) ([]DNSServerStatus, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT id, name, dns_port, dns_listen_error, dns_applied_revision, dns_error, last_ip
		 FROM servers WHERE enrolled_at IS NOT NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DNSServerStatus
	for rows.Next() {
		var s DNSServerStatus
		if err := rows.Scan(&s.ServerID, &s.Name, &s.Port, &s.ListenError, &s.AppliedRevision, &s.Error, &s.LastIP); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DNSTargets lists every proxied domain with the addresses of its server,
// the input of automatic records.
func DNSTargets(ctx context.Context, q queryer) ([]dns.AutoTarget, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT d.domain, s.public_ips, s.detected_ips, s.last_ip,
		        CASE WHEN st.id IS NOT NULL THEN 'site ' || st.name
		             WHEN a.id IS NOT NULL THEN 'app ' || a.name ELSE 'route' END
		 FROM proxy_route_domains d
		 JOIN proxy_routes r ON r.id = d.route_id
		 JOIN servers s ON s.id = d.server_id
		 LEFT JOIN sites st ON st.id = r.site_id
		 LEFT JOIN apps a ON a.id = r.app_id
		 WHERE s.enrolled_at IS NOT NULL
		 ORDER BY d.domain, s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dns.AutoTarget
	for rows.Next() {
		var domain, public, detected, source string
		var srv Server
		if err := rows.Scan(&domain, &public, &detected, &srv.LastIP, &source); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(public), &srv.PublicIPs); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(detected), &srv.DetectedIPs); err != nil {
			return nil, err
		}
		if ips := srv.DNSAddrs(); len(ips) > 0 {
			out = append(out, dns.AutoTarget{Domain: domain, IPs: ips, Source: source})
		}
	}
	return out, rows.Err()
}

// dnsChallenges lists the DNS-01 challenge records not yet expired.
func dnsChallenges(ctx context.Context, q queryer) ([]dns.Challenge, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name, value FROM dns_challenges WHERE expires_at > unixepoch() ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dns.Challenge
	for rows.Next() {
		var c dns.Challenge
		if err := rows.Scan(&c.Name, &c.Value); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// autoDNS computes the automatic records (addresses of sites and apps,
// DNS-01 challenges) of every zone.
func autoDNS(ctx context.Context, q queryer, zones []dns.Zone) ([][]dns.AutoRecord, error) {
	targets, err := DNSTargets(ctx, q)
	if err != nil {
		return nil, err
	}
	challenges, err := dnsChallenges(ctx, q)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(zones))
	for i, z := range zones {
		names[i] = z.Name
	}
	out := make([][]dns.AutoRecord, len(zones))
	for i := range zones {
		out[i] = append(dns.AutoRecords(&zones[i], names, targets), dns.ChallengeRecords(&zones[i], names, challenges)...)
		if room := dns.MaxRecords - len(zones[i].Records); len(out[i]) > room {
			out[i] = out[i][:max(room, 0)]
		}
	}
	return out, nil
}

// AddDNSChallenge publishes a DNS-01 challenge record for a server's proxy
// until expires (or RemoveDNSChallenge).
func (db *DB) AddDNSChallenge(ctx context.Context, serverID int64, name, value string, expires time.Time) error {
	var n int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM dns_challenges WHERE server_id = ?`, serverID).Scan(&n); err != nil {
		return err
	}
	if n >= maxChallengesPerServer {
		return ErrTooManyChallenges
	}
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO dns_challenges (server_id, name, value, expires_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (server_id, name, value) DO UPDATE SET expires_at = excluded.expires_at`,
		serverID, name, value, expires.Unix())
	return err
}

const maxChallengesPerServer = 100

var ErrTooManyChallenges = errors.New("too many pending DNS challenges for this server")

func (db *DB) RemoveDNSChallenge(ctx context.Context, serverID int64, name, value string) error {
	_, err := db.W.ExecContext(ctx, `DELETE FROM dns_challenges WHERE server_id = ? AND name = ? AND value = ?`,
		serverID, name, value)
	return err
}

// DeleteExpiredDNSChallenges removes challenges past their expiry.
func (db *DB) DeleteExpiredDNSChallenges(ctx context.Context, now time.Time) (int64, error) {
	res, err := db.W.ExecContext(ctx, `DELETE FROM dns_challenges WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ServerHasDomain reports whether a route on the server serves domain.
func (db *DB) ServerHasDomain(ctx context.Context, serverID int64, domain string) (bool, error) {
	var n int
	err := db.R.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM proxy_route_domains WHERE server_id = ? AND domain = ?`, serverID, domain).Scan(&n)
	return n > 0, err
}

func autoHash(recs []dns.AutoRecord) string {
	if len(recs) == 0 {
		return ""
	}
	h := sha256.New()
	for _, r := range recs {
		fmt.Fprintf(h, "%s %s %s\n", r.Name, r.Type, r.Value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DNSZoneAuto returns the automatic records of one zone.
func (db *DB) DNSZoneAuto(ctx context.Context, zoneID int64) ([]dns.AutoRecord, error) {
	_, zones, ids, err := db.manualDNS(ctx, db.R, nil)
	if err != nil {
		return nil, err
	}
	auto, err := autoDNS(ctx, db.R, zones)
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		if id == zoneID {
			return auto[i], nil
		}
	}
	return nil, ErrNotFound
}

// SyncAutoDNS bumps the serial (and the DNS revision) of every zone whose
// automatic records changed since the last call. It reports whether any did.
func (db *DB) SyncAutoDNS(ctx context.Context, now time.Time) (bool, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	_, zones, ids, err := db.manualDNS(ctx, tx, nil)
	if err != nil {
		return false, err
	}
	auto, err := autoDNS(ctx, tx, zones)
	if err != nil {
		return false, err
	}
	hashes := map[int64]string{}
	rows, err := tx.QueryContext(ctx, `SELECT id, auto_hash FROM dns_zones`)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id int64
		var h string
		if err := rows.Scan(&id, &h); err != nil {
			rows.Close()
			return false, err
		}
		hashes[id] = h
	}
	rows.Close()
	changed := false
	for i, auto := range auto {
		h := autoHash(auto)
		if h == hashes[ids[i]] {
			continue
		}
		changed = true
		if _, err := tx.ExecContext(ctx, `UPDATE dns_zones SET auto_hash = ?, serial = ?, updated_at = ? WHERE id = ?`,
			h, nextSerial(zones[i].Serial, now), now.Unix(), ids[i]); err != nil {
			return false, err
		}
	}
	if !changed {
		return false, nil
	}
	if err := bumpDNSRevision(ctx, tx); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
