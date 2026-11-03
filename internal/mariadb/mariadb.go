// Package mariadb manages per-site databases on the host's MariaDB (or
// MySQL) server, connecting over the local unix socket as the account of
// the process's own system user (unix_socket authentication): root's, or
// the one the agent's privileged helper makes for the agent's user
// (AgentAccountSQL).
//
// Identifiers cannot be bound as query parameters, so database and user
// names are restricted to [a-z0-9_] and validated before being quoted.
// Passwords and user names are passed as parameters, escaped by the driver.
package mariadb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/user"
	"regexp"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

var ErrInvalidName = errors.New("name must be 1-32 characters: a-z, 0-9 and _, starting with a letter")

func ValidName(n string) bool { return nameRE.MatchString(n) }

type Server struct {
	db *sql.DB
}

// Open connects to the server listening on socket. It does not fail if the
// server is not running; use Ping to check.
func Open(socket string) (*Server, error) {
	cfg := mysql.NewConfig()
	cfg.Net = "unix"
	cfg.Addr = socket
	cfg.User = User()
	cfg.InterpolateParams = true // CREATE USER / GRANT cannot be prepared
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 30 * time.Second
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(2)
	db.SetConnMaxIdleTime(time.Minute)
	return &Server{db: db}, nil
}

// User is the MariaDB account this process signs in as: its system user's
// name (root when that cannot be read).
func User() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "root"
}

// AgentAccount creates the account of the system user name, signing in
// over the socket only, with every right (the agent creates the sites'
// databases and users, and replicates them): what root's account is to
// root, for a user that is not root.
func (s *Server) AgentAccount(ctx context.Context, name string) error {
	if !accountRE.MatchString(name) {
		return fmt.Errorf("invalid account name %q", name)
	}
	for _, q := range []string{
		"CREATE USER IF NOT EXISTS ?@'localhost' IDENTIFIED VIA unix_socket",
		"GRANT ALL PRIVILEGES ON *.* TO ?@'localhost' WITH GRANT OPTION",
	} {
		if _, err := s.db.ExecContext(ctx, q, name); err != nil {
			return fmt.Errorf("account %s: %w", name, err)
		}
	}
	return nil
}

var accountRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Available reports whether a socket exists at path.
func Available(socket string) bool {
	fi, err := os.Stat(socket)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

func (s *Server) Close() error { return s.db.Close() }

func (s *Server) Version(ctx context.Context) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v)
	return v, err
}

// CreateDatabase creates a database and a user with full rights on it only.
// The user can connect from the local host.
func (s *Server) CreateDatabase(ctx context.Context, name, user, password string) error {
	if !ValidName(name) || !ValidName(user) {
		return ErrInvalidName
	}
	if len(password) < 16 {
		return errors.New("database password too short")
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect to MariaDB: %w", err)
	}
	defer conn.Close()
	steps := []struct {
		q    string
		args []any
	}{
		{"CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", nil},
		{"CREATE USER ?@'localhost' IDENTIFIED BY ?", []any{user, password}},
		{"GRANT ALL PRIVILEGES ON `" + name + "`.* TO ?@'localhost'", []any{user}},
	}
	for i, st := range steps {
		if _, err := conn.ExecContext(ctx, st.q, st.args...); err != nil {
			// Undo what was created so a retry starts clean.
			if i >= 1 {
				conn.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`")
			}
			if i >= 2 {
				conn.ExecContext(ctx, "DROP USER IF EXISTS ?@'localhost'", user)
			}
			return mapError(err)
		}
	}
	return nil
}

// SetPassword replaces a database user's password.
func (s *Server) SetPassword(ctx context.Context, user, password string) error {
	if !ValidName(user) {
		return ErrInvalidName
	}
	if len(password) < 16 {
		return errors.New("database password too short")
	}
	_, err := s.db.ExecContext(ctx, "ALTER USER ?@'localhost' IDENTIFIED BY ?", user, password)
	return mapError(err)
}

// DropDatabase removes the database and its user. Missing objects are fine.
func (s *Server) DropDatabase(ctx context.Context, name, user string) error {
	if !ValidName(name) || !ValidName(user) {
		return ErrInvalidName
	}
	if _, err := s.db.ExecContext(ctx, "DROP USER IF EXISTS ?@'localhost'", user); err != nil {
		return mapError(err)
	}
	_, err := s.db.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`")
	return mapError(err)
}

// Sizes returns the bytes (data and indexes) of each named database; a
// database that does not exist is left out.
func (s *Server) Sizes(ctx context.Context, names []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(names) == 0 {
		return out, nil
	}
	args := make([]any, len(names))
	marks := make([]string, len(names))
	for i, n := range names {
		if !ValidName(n) {
			return nil, fmt.Errorf("invalid database name %q", n)
		}
		args[i], marks[i] = n, "?"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT table_schema, COALESCE(SUM(data_length + index_length), 0)
		FROM information_schema.tables WHERE table_schema IN (`+strings.Join(marks, ",")+`) GROUP BY table_schema`, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

func mapError(err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1007:
			return errors.New("a database with this name already exists on the server")
		case 1396:
			return errors.New("a database user with this name already exists on the server")
		}
		return fmt.Errorf("MariaDB error %d: %s", me.Number, me.Message)
	}
	return err
}
