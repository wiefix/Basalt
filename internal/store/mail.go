package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/dns"
)

// MailDomain is a domain the mail server accepts mail for. DNSRecords are
// what the mail server asks to publish (absolute names).
type MailDomain struct {
	ID         int64
	AppID      int64
	Name       string
	RemoteID   string
	DNSRecords []dns.Record
	CreatedAt  time.Time
}

const mailDomainColumns = `id, app_id, name, remote_id, dns_records, created_at`

func scanMailDomain(row interface{ Scan(...any) error }) (*MailDomain, error) {
	var d MailDomain
	var recs string
	var created int64
	if err := row.Scan(&d.ID, &d.AppID, &d.Name, &d.RemoteID, &recs, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(recs), &d.DNSRecords); err != nil {
		return nil, err
	}
	d.CreatedAt = time.Unix(created, 0)
	return &d, nil
}

func (db *DB) MailDomains(ctx context.Context) ([]*MailDomain, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+mailDomainColumns+` FROM mail_domains ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MailDomain
	for rows.Next() {
		d, err := scanMailDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (db *DB) MailDomain(ctx context.Context, id int64) (*MailDomain, error) {
	return scanMailDomain(db.R.QueryRowContext(ctx, `SELECT `+mailDomainColumns+` FROM mail_domains WHERE id = ?`, id))
}

func (db *DB) CreateMailDomain(ctx context.Context, appID int64, name, remoteID string) (*MailDomain, error) {
	d := &MailDomain{AppID: appID, Name: name, RemoteID: remoteID, DNSRecords: []dns.Record{}}
	var created int64
	err := db.W.QueryRowContext(ctx,
		`INSERT INTO mail_domains (app_id, name, remote_id) VALUES (?, ?, ?) RETURNING id, created_at`,
		appID, name, remoteID).Scan(&d.ID, &created)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: mail_domains.name") {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	d.CreatedAt = time.Unix(created, 0)
	return d, nil
}

func (db *DB) SetMailDomainRemote(ctx context.Context, id int64, remoteID string) error {
	return db.execOne(ctx, `UPDATE mail_domains SET remote_id = ? WHERE id = ?`, remoteID, id)
}

// SetMailDomainDNS stores the records the mail server asks for.
func (db *DB) SetMailDomainDNS(ctx context.Context, id int64, recs []dns.Record) error {
	if recs == nil {
		recs = []dns.Record{}
	}
	b, _ := json.Marshal(recs)
	return db.execOne(ctx, `UPDATE mail_domains SET dns_records = ? WHERE id = ?`, string(b), id)
}

func (db *DB) DeleteMailDomain(ctx context.Context, id int64) error {
	return db.execOne(ctx, `DELETE FROM mail_domains WHERE id = ?`, id)
}

// MailApp returns the mail server app, or ErrNotFound.
func (db *DB) MailApp(ctx context.Context) (*App, error) {
	var id int64
	err := db.R.QueryRowContext(ctx, `SELECT id FROM apps WHERE source = ?`, SourceMail).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return db.AppByID(ctx, id)
}

// mailDNS lists the records every mail domain asks for.
func mailDNS(ctx context.Context, q queryer) ([]dns.Record, error) {
	rows, err := q.QueryContext(ctx, `SELECT dns_records FROM mail_domains ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dns.Record
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		var recs []dns.Record
		if err := json.Unmarshal([]byte(s), &recs); err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	return out, rows.Err()
}
