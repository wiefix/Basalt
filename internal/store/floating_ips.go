package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// CloudAccount holds the API token of a cloud provider's account.
type CloudAccount struct {
	ID        int64
	Provider  string
	Name      string
	Secret    []byte // encrypted API token
	CreatedAt time.Time
	UpdatedAt time.Time
}

const cloudAccountColumns = `id, provider, name, secret, created_at, updated_at`

func scanCloudAccount(row interface{ Scan(...any) error }) (*CloudAccount, error) {
	var a CloudAccount
	var created, updated int64
	err := row.Scan(&a.ID, &a.Provider, &a.Name, &a.Secret, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt, a.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &a, nil
}

func (db *DB) CreateCloudAccount(ctx context.Context, provider, name string, secret []byte) (*CloudAccount, error) {
	a, err := scanCloudAccount(db.W.QueryRowContext(ctx,
		`INSERT INTO cloud_accounts (provider, name, secret) VALUES (?, ?, ?) RETURNING `+cloudAccountColumns,
		provider, name, secret))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: cloud_accounts.name") {
		return nil, ErrNameTaken
	}
	return a, err
}

// UpdateCloudAccount renames an account and, when secret is not nil,
// replaces its token.
func (db *DB) UpdateCloudAccount(ctx context.Context, id int64, name string, secret []byte) error {
	err := db.execOne(ctx, `UPDATE cloud_accounts SET name = ?, secret = COALESCE(?, secret), updated_at = unixepoch() WHERE id = ?`,
		name, secret, id)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: cloud_accounts.name") {
		return ErrNameTaken
	}
	return err
}

// ErrInUse: the account still has floating IPs.
var ErrInUse = errors.New("still in use")

func (db *DB) DeleteCloudAccount(ctx context.Context, id int64) error {
	err := db.execOne(ctx, `DELETE FROM cloud_accounts WHERE id = ?`, id)
	if err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		return ErrInUse
	}
	return err
}

func (db *DB) CloudAccount(ctx context.Context, id int64) (*CloudAccount, error) {
	return scanCloudAccount(db.R.QueryRowContext(ctx, `SELECT `+cloudAccountColumns+` FROM cloud_accounts WHERE id = ?`, id))
}

func (db *DB) CloudAccounts(ctx context.Context) ([]*CloudAccount, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+cloudAccountColumns+` FROM cloud_accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CloudAccount{}
	for rows.Next() {
		a, err := scanCloudAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// FloatingIP is a provider's address the panel routes to one server of
// its group.
type FloatingIP struct {
	ID         int64
	AccountID  int64
	ProviderID string
	Name       string
	// Address is configured on the servers; Network is how the provider
	// names it (an IPv6 block).
	Address  string
	Network  string
	Location string
	// ServerID is the server the provider routes it to (0: none of the
	// group, as far as the panel knows), since AssignedAt.
	ServerID   int64
	AssignedAt *time.Time
	Servers    []FloatingIPServer // in failover order
	CreatedAt  time.Time
}

// FloatingIPServer is a server of a floating IP's group.
type FloatingIPServer struct {
	ServerID         int64
	ProviderServerID string
}

const floatingColumns = `id, account_id, provider_id, name, address, network, location, COALESCE(server_id, 0), assigned_at, created_at`

func scanFloatingIP(row interface{ Scan(...any) error }) (*FloatingIP, error) {
	var f FloatingIP
	var assigned sql.NullInt64
	var created int64
	err := row.Scan(&f.ID, &f.AccountID, &f.ProviderID, &f.Name, &f.Address, &f.Network, &f.Location,
		&f.ServerID, &assigned, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.AssignedAt = timePtr(assigned)
	f.CreatedAt = time.Unix(created, 0)
	return &f, nil
}

// ErrAddressTaken: the address is already a floating IP of the panel.
var ErrAddressTaken = errors.New("address already added")

// CreateFloatingIP records a floating IP with its group of servers.
func (db *DB) CreateFloatingIP(ctx context.Context, f FloatingIP) (*FloatingIP, error) {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var server any
	if f.ServerID != 0 {
		server = f.ServerID
	}
	out, err := scanFloatingIP(tx.QueryRowContext(ctx,
		`INSERT INTO floating_ips (account_id, provider_id, name, address, network, location, server_id, assigned_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, CASE WHEN ? IS NULL THEN NULL ELSE unixepoch() END) RETURNING `+floatingColumns,
		f.AccountID, f.ProviderID, f.Name, f.Address, f.Network, f.Location, server, server))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return nil, ErrAddressTaken
		}
		return nil, err
	}
	if err := setFloatingServers(ctx, tx, out.ID, f.Servers); err != nil {
		return nil, err
	}
	out.Servers = f.Servers
	return out, tx.Commit()
}

func setFloatingServers(ctx context.Context, tx *sql.Tx, id int64, servers []FloatingIPServer) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM floating_ip_servers WHERE floating_ip_id = ?`, id); err != nil {
		return err
	}
	for i, s := range servers {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO floating_ip_servers (floating_ip_id, server_id, position, provider_server_id) VALUES (?, ?, ?, ?)`,
			id, s.ServerID, i, s.ProviderServerID); err != nil {
			return err
		}
	}
	return nil
}

// SetFloatingIPServers replaces the group of a floating IP.
func (db *DB) SetFloatingIPServers(ctx context.Context, id int64, servers []FloatingIPServer) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM floating_ips WHERE id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := setFloatingServers(ctx, tx, id, servers); err != nil {
		return err
	}
	return tx.Commit()
}

// SetFloatingIPServer records the server a floating IP is routed to (0:
// none of the group); the time changes only when the server does.
func (db *DB) SetFloatingIPServer(ctx context.Context, id, serverID int64) error {
	var server any
	if serverID != 0 {
		server = serverID
	}
	return db.execOne(ctx,
		`UPDATE floating_ips SET assigned_at = CASE WHEN server_id IS ? THEN assigned_at WHEN ? IS NULL THEN NULL ELSE unixepoch() END,
		 server_id = ? WHERE id = ?`, server, server, server, id)
}

func (db *DB) DeleteFloatingIP(ctx context.Context, id int64) error {
	return db.execOne(ctx, `DELETE FROM floating_ips WHERE id = ?`, id)
}

func (db *DB) floatingIPs(ctx context.Context, where string, args ...any) ([]*FloatingIP, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+floatingColumns+` FROM floating_ips `+where+` ORDER BY address`, args...)
	if err != nil {
		return nil, err
	}
	var out []*FloatingIP
	byID := map[int64]*FloatingIP{}
	for rows.Next() {
		f, err := scanFloatingIP(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		f.Servers = []FloatingIPServer{}
		out = append(out, f)
		byID[f.ID] = f
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	members, err := db.R.QueryContext(ctx,
		`SELECT floating_ip_id, server_id, provider_server_id FROM floating_ip_servers ORDER BY floating_ip_id, position`)
	if err != nil {
		return nil, err
	}
	defer members.Close()
	for members.Next() {
		var id int64
		var s FloatingIPServer
		if err := members.Scan(&id, &s.ServerID, &s.ProviderServerID); err != nil {
			return nil, err
		}
		if f := byID[id]; f != nil {
			f.Servers = append(f.Servers, s)
		}
	}
	return out, members.Err()
}

func (db *DB) FloatingIPs(ctx context.Context) ([]*FloatingIP, error) {
	out, err := db.floatingIPs(ctx, "")
	if out == nil && err == nil {
		out = []*FloatingIP{}
	}
	return out, err
}

func (db *DB) FloatingIP(ctx context.Context, id int64) (*FloatingIP, error) {
	out, err := db.floatingIPs(ctx, "WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out[0], nil
}

// FloatingAddressesOn lists the addresses a server configures: those of
// the floating IPs whose group it is in.
func (db *DB) FloatingAddressesOn(ctx context.Context, serverID int64) ([]string, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT f.address FROM floating_ips f JOIN floating_ip_servers m ON m.floating_ip_id = f.id
		 WHERE m.server_id = ? ORDER BY f.address`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetServerFloatingError records why a server could not configure its
// floating IPs ("": it did).
func (db *DB) SetServerFloatingError(ctx context.Context, serverID int64, msg string) error {
	return db.execOne(ctx, `UPDATE servers SET floating_error = ? WHERE id = ?`, msg, serverID)
}

// ServerFloatingErrors maps servers to why they could not configure their
// floating IPs (only those that could not).
func (db *DB) ServerFloatingErrors(ctx context.Context) (map[int64]string, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT id, floating_error FROM servers WHERE floating_error != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var msg string
		if err := rows.Scan(&id, &msg); err != nil {
			return nil, err
		}
		out[id] = msg
	}
	return out, rows.Err()
}
