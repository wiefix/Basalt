package mariadb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Replication between servers. Every server taking part has its own
// server_id and GTID domain (its id in the panel) and a binary log; a
// replica reaches each source through a loopback port the agents' tunnel
// forwards (MariaDB itself listens on nothing new), as a named connection
// basalt_<source id> limited to the databases copied from that source.

// ReplUser is the user replicas (and their seeding dumps) connect as.
const ReplUser = "basalt_repl"

// ConnName is the replica connection from a source server.
func ConnName(source int64) string { return "basalt_" + strconv.FormatInt(source, 10) }

var connRE = regexp.MustCompile(`^basalt_[1-9][0-9]{0,9}$`)

// Config is the option file that keeps a server's replication setup over
// restarts: its identity, the binary log, and what each connection copies
// (filters set at run time do not survive a restart, and a connection
// without its filter would copy every database of its source).
func Config(serverID int64, filters map[string][]string) []byte {
	var b bytes.Buffer
	b.WriteString("# Written by Basalt (database replication between servers). Do not edit.\n[mariadbd]\n")
	fmt.Fprintf(&b, "server_id = %d\ngtid_domain_id = %d\n", serverID, serverID)
	b.WriteString("log_bin = basalt-bin\nbinlog_format = ROW\nbinlog_expire_logs_seconds = 604800\nrelay_log = basalt-relay\n")
	names := make([]string, 0, len(filters))
	for n := range filters {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		dbs := slices.Clone(filters[n])
		slices.Sort(dbs)
		for _, d := range dbs {
			fmt.Fprintf(&b, "%s.replicate_do_db = %s\n", n, d)
		}
	}
	return b.Bytes()
}

// BinlogOn reports whether the binary log is enabled (it needs a restart).
func (s *Server) BinlogOn(ctx context.Context) (bool, error) {
	var on bool
	err := s.db.QueryRowContext(ctx, "SELECT @@log_bin").Scan(&on)
	return on, mapError(err)
}

// SetIdentity sets the server's id and GTID domain now (the option file
// keeps them over restarts). The first time (the binary log has no event
// of the server's domain yet), a binary log holding events of other
// domains, written before Basalt set it up, is started over: replicas
// would otherwise replay those. Never after: starting the sequence numbers
// over would make replicas skip new events as seen.
func (s *Server) SetIdentity(ctx context.Context, id int64) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return mapError(err)
	}
	defer conn.Close()
	for _, q := range []string{
		"SET GLOBAL server_id = " + strconv.FormatInt(id, 10),
		"SET GLOBAL gtid_domain_id = " + strconv.FormatInt(id, 10),
		"SET GLOBAL binlog_format = 'ROW'",
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return mapError(err)
		}
	}
	var state string
	if err := conn.QueryRowContext(ctx, "SELECT @@gtid_binlog_state").Scan(&state); err != nil {
		return mapError(err)
	}
	own, other := false, false
	for _, g := range splitGTIDs(state) {
		if g.domain == id {
			own = true
		} else {
			other = true
		}
	}
	if other && !own {
		_, err := conn.ExecContext(ctx, "RESET MASTER")
		return mapError(err)
	}
	return nil
}

// SourceSetup lets replicas in: the replication user with password, and
// what a seeding dump needs on the given databases.
func (s *Server) SourceSetup(ctx context.Context, password string, dbs []string) error {
	if len(password) < 16 {
		return errors.New("replication password too short")
	}
	for _, d := range dbs {
		if !ValidName(d) {
			return ErrInvalidName
		}
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return mapError(err)
	}
	defer conn.Close()
	// Not in the binary log: replicas have no use for this server's
	// replication user.
	steps := []struct {
		q    string
		args []any
	}{
		{"SET SESSION sql_log_bin = 0", nil},
		{"CREATE USER IF NOT EXISTS ?@'localhost' IDENTIFIED BY ?", []any{ReplUser, password}},
		{"ALTER USER ?@'localhost' IDENTIFIED BY ?", []any{ReplUser, password}},
		{"GRANT REPLICATION SLAVE, BINLOG MONITOR, RELOAD ON *.* TO ?@'localhost'", []any{ReplUser}},
	}
	for _, d := range dbs {
		steps = append(steps, struct {
			q    string
			args []any
		}{"GRANT SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES ON `" + d + "`.* TO ?@'localhost'", []any{ReplUser}})
	}
	for _, st := range steps {
		if _, err := conn.ExecContext(ctx, st.q, st.args...); err != nil {
			return mapError(err)
		}
	}
	return nil
}

var userHashRE = regexp.MustCompile(`IDENTIFIED BY PASSWORD '(\*[0-9A-F]{40})'`)

// UserHash returns the password hash of a site database user (to create
// the same user on replicas).
func (s *Server) UserHash(ctx context.Context, user string) (string, error) {
	if !ValidName(user) {
		return "", ErrInvalidName
	}
	var stmt string
	if err := s.db.QueryRowContext(ctx, "SHOW CREATE USER ?@'localhost'", user).Scan(&stmt); err != nil {
		return "", mapError(err)
	}
	m := userHashRE.FindStringSubmatch(stmt)
	if m == nil {
		return "", fmt.Errorf("user %s has no password hash", user)
	}
	return m[1], nil
}

// ValidHash reports whether h is a MariaDB native password hash.
func ValidHash(h string) bool { return regexp.MustCompile(`^\*[0-9A-F]{40}$`).MatchString(h) }

// ReplicaUser creates (or updates) a site database user on a replica, with
// the source's password hash and read-only rights: changes come through
// replication only.
func (s *Server) ReplicaUser(ctx context.Context, name, user, hash string) error {
	if !ValidName(name) || !ValidName(user) || !ValidHash(hash) {
		return ErrInvalidName
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return mapError(err)
	}
	defer conn.Close()
	// Not written to this server's binary log: it is not this server's
	// change to pass on.
	for _, st := range []struct {
		q    string
		args []any
	}{
		{"SET SESSION sql_log_bin = 0", nil},
		{"CREATE USER IF NOT EXISTS ?@'localhost' IDENTIFIED BY PASSWORD ?", []any{user, hash}},
		{"ALTER USER ?@'localhost' IDENTIFIED BY PASSWORD ?", []any{user, hash}},
		{"GRANT SELECT, SHOW VIEW ON `" + name + "`.* TO ?@'localhost'", []any{user}},
	} {
		if _, err := conn.ExecContext(ctx, st.q, st.args...); err != nil {
			return mapError(err)
		}
	}
	return nil
}

// DropCopy removes a replica's copy of a database and its user, outside
// the binary log.
func (s *Server) DropCopy(ctx context.Context, name, user string) error {
	if !ValidName(name) || !ValidName(user) {
		return ErrInvalidName
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return mapError(err)
	}
	defer conn.Close()
	for _, q := range []struct {
		q    string
		args []any
	}{
		{"SET SESSION sql_log_bin = 0", nil},
		{"DROP USER IF EXISTS ?@'localhost'", []any{user}},
		{"DROP DATABASE IF EXISTS `" + name + "`", nil},
	} {
		if _, err := conn.ExecContext(ctx, q.q, q.args...); err != nil {
			return mapError(err)
		}
	}
	return nil
}

// ReplicaStatus is how a replica connection is doing.
type ReplicaStatus struct {
	IORunning  bool
	SQLRunning bool
	// Lag is Seconds_Behind_Master (-1: unknown, e.g. not running).
	Lag      int64
	IOError  string
	SQLError string
	GTIDPos  string
	// DoDB is the connection's replicate_do_db filter.
	DoDB string
}

// Status reports a connection's state; nil if there is no such connection.
func (s *Server) Status(ctx context.Context, conn string) (*ReplicaStatus, error) {
	if !connRE.MatchString(conn) {
		return nil, errors.New("bad connection name")
	}
	rows, err := s.db.QueryContext(ctx, "SHOW SLAVE '"+conn+"' STATUS")
	if noConnection(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		return nil, rows.Err()
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	get := func(name string) string {
		if i := slices.Index(cols, name); i >= 0 {
			return vals[i].String
		}
		return ""
	}
	st := &ReplicaStatus{IORunning: get("Slave_IO_Running") == "Yes", SQLRunning: get("Slave_SQL_Running") == "Yes",
		Lag: -1, IOError: get("Last_IO_Error"), SQLError: get("Last_SQL_Error"), GTIDPos: get("Gtid_IO_Pos"), DoDB: get("Replicate_Do_DB")}
	if v := get("Seconds_Behind_Master"); v != "" {
		st.Lag, _ = strconv.ParseInt(v, 10, 64)
	}
	return st, nil
}

// Source points the connection conn at a loopback port (the tunnel to its
// source), stopping it first; it is left stopped.
func (s *Server) Source(ctx context.Context, conn string, port int, password string) error {
	if !connRE.MatchString(conn) || port < 1024 || port > 65535 {
		return errors.New("bad connection")
	}
	c, err := s.db.Conn(ctx)
	if err != nil {
		return mapError(err)
	}
	defer c.Close()
	c.ExecContext(ctx, "STOP SLAVE '"+conn+"'")
	_, err = c.ExecContext(ctx, "CHANGE MASTER '"+conn+"' TO MASTER_HOST = '127.0.0.1', MASTER_PORT = "+strconv.Itoa(port)+
		", MASTER_USER = ?, MASTER_PASSWORD = ?, MASTER_USE_GTID = slave_pos, MASTER_CONNECT_RETRY = 10", ReplUser, password)
	return mapError(err)
}

// Filter returns the databases a connection copies (none for a missing
// connection).
func (s *Server) Filter(ctx context.Context, conn string) ([]string, error) {
	st, err := s.Status(ctx, conn)
	if err != nil || st == nil {
		return nil, err
	}
	var out []string
	for _, d := range strings.Split(st.DoDB, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out, nil
}

// SetFilter sets the databases a stopped connection copies.
func (s *Server) SetFilter(ctx context.Context, conn string, dbs []string) error {
	if !connRE.MatchString(conn) {
		return errors.New("bad connection name")
	}
	for _, d := range dbs {
		if !ValidName(d) {
			return ErrInvalidName
		}
	}
	c, err := s.db.Conn(ctx)
	if err != nil {
		return mapError(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "SET @@default_master_connection = '"+conn+"'"); err != nil {
		return mapError(err)
	}
	_, err = c.ExecContext(ctx, "SET GLOBAL replicate_do_db = ?", strings.Join(dbs, ","))
	return mapError(err)
}

// Start starts a connection; with until (a GTID position) it stops there.
func (s *Server) Start(ctx context.Context, conn, until string) error {
	if !connRE.MatchString(conn) {
		return errors.New("bad connection name")
	}
	q := "START SLAVE '" + conn + "'"
	var args []any
	if until != "" {
		q += " UNTIL master_gtid_pos = ?"
		args = append(args, until)
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return mapError(err)
}

// Stop stops a connection (a missing one is fine).
func (s *Server) Stop(ctx context.Context, conn string) error {
	if !connRE.MatchString(conn) {
		return errors.New("bad connection name")
	}
	_, err := s.db.ExecContext(ctx, "STOP SLAVE '"+conn+"'")
	if noConnection(err) {
		return nil
	}
	return mapError(err)
}

// WaitStopped waits until a connection's SQL thread has stopped (after
// START SLAVE UNTIL), reporting its error if it stopped on one.
func (s *Server) WaitStopped(ctx context.Context, conn string) error {
	for {
		st, err := s.Status(ctx, conn)
		if err != nil {
			return err
		}
		if st == nil || !st.SQLRunning {
			if st != nil && st.SQLError != "" {
				return errors.New(st.SQLError)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Remove stops a connection and forgets it.
func (s *Server) Remove(ctx context.Context, conn string) error {
	if err := s.Stop(ctx, conn); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "RESET SLAVE '"+conn+"' ALL")
	if noConnection(err) {
		return nil
	}
	return mapError(err)
}

// SetSlavePos sets, for a new connection from the source of domain, the
// GTID position of a dump of it: its domain's entry (none: forgotten), and
// entries of other domains this server has none of (events the source
// wrote outside its domain, already in the dump). The position is shared by
// all connections: they stop meanwhile, those that ran start again.
func (s *Server) SetSlavePos(ctx context.Context, domain int64, pos string) error {
	c, err := s.db.Conn(ctx)
	if err != nil {
		return mapError(err)
	}
	defer c.Close()
	var cur string
	if err := c.QueryRowContext(ctx, "SELECT @@gtid_slave_pos").Scan(&cur); err != nil {
		return mapError(err)
	}
	var keep []string
	have := map[int64]bool{}
	for _, g := range splitGTIDs(cur) {
		if g.domain != domain {
			keep = append(keep, g.text)
			have[g.domain] = true
		}
	}
	for _, g := range splitGTIDs(pos) {
		if g.domain == domain || !have[g.domain] {
			keep = append(keep, g.text)
		}
	}
	// Only the connections that ran start again.
	conns, err := s.Connections(ctx)
	if err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "STOP ALL SLAVES"); err != nil {
		return mapError(err)
	}
	_, err = c.ExecContext(ctx, "SET GLOBAL gtid_slave_pos = ?", strings.Join(keep, ","))
	for _, cn := range conns {
		if cn.Running {
			c.ExecContext(ctx, "START SLAVE '"+cn.Name+"'")
		}
	}
	return mapError(err)
}

// noConnection reports the error of a connection that does not exist
// (ER_MASTER_INFO, "There is no master connection").
func noConnection(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1617 || strings.Contains(me.Message, "no master connection"))
}

type gtid struct {
	domain int64
	text   string
}

func splitGTIDs(s string) []gtid {
	var out []gtid
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		d, _, ok := strings.Cut(part, "-")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, gtid{n, part})
	}
	return out
}

var dumpGTIDRE = regexp.MustCompile(`(?m)^-- SET GLOBAL gtid_slave_pos='([0-9,\-]*)';`)

// DumpGTIDs returns the whole GTID position of a dump (every domain).
func DumpGTIDs(head []byte) (string, error) {
	m := dumpGTIDRE.FindSubmatch(head)
	if m == nil {
		return "", errors.New("the dump has no GTID position")
	}
	return string(m[1]), nil
}

// DumpGTID finds, in the start of a dump made with --gtid --master-data=2,
// the position of domain the dump matches ("" if the domain wrote nothing).
func DumpGTID(head []byte, domain int64) (string, error) {
	m := dumpGTIDRE.FindSubmatch(head)
	if m == nil {
		return "", errors.New("the dump has no GTID position")
	}
	for _, g := range splitGTIDs(string(m[1])) {
		if g.domain == domain {
			return g.text, nil
		}
	}
	return "", nil
}

// Connection is a replica connection and whether it runs.
type Connection struct {
	Name    string
	Running bool
}

// Connections lists the replica connections Basalt made.
func (s *Server) Connections(ctx context.Context) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, "SHOW ALL SLAVES STATUS")
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	name, io, sqlt := slices.Index(cols, "Connection_name"), slices.Index(cols, "Slave_IO_Running"), slices.Index(cols, "Slave_SQL_Running")
	if name < 0 {
		return nil, errors.New("unexpected SHOW ALL SLAVES STATUS")
	}
	var out []Connection
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		if connRE.MatchString(vals[name].String) {
			running := (io >= 0 && vals[io].String == "Yes") || (sqlt >= 0 && vals[sqlt].String == "Yes")
			out = append(out, Connection{Name: vals[name].String, Running: running})
		}
	}
	return out, rows.Err()
}

// WaitPos waits until this server has applied events up to pos (a GTID
// position) or ctx ends.
func (s *Server) WaitPos(ctx context.Context, pos string) error {
	for {
		var r sql.NullInt64
		if err := s.db.QueryRowContext(ctx, "SELECT MASTER_GTID_WAIT(?, 2)", pos).Scan(&r); err != nil {
			return mapError(err)
		}
		if r.Valid && r.Int64 == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
