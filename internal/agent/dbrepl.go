package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/mariadb"
)

// Replication of site databases (see agentproto.DBReplApply): this server
// as a source keeps a binary log and lets the replication user in; as a
// replica it copies what the panel lists, seeding each new database from a
// dump taken through the tunnel and aligned with the replication stream.

const defaultMariaDBConf = "/etc/mysql/mariadb.conf.d/90-basalt.cnf"

type dbRepl struct {
	mu sync.Mutex // one change at a time
	// sources remembers the port and password each connection was pointed
	// at (MariaDB does not say the password back).
	sources map[string]string
}

func (a *Agent) mariaDB() (*mariadb.Server, error) {
	if a.mysql == "" || !mariadb.Available(a.mysql) {
		return nil, errors.New("MariaDB is not available on this server")
	}
	return mariadb.Open(a.mysql)
}

// writeMariaDBConf keeps the option file in line with the server's id and
// the filters of its connections now.
func (a *Agent) writeMariaDBConf(ctx context.Context, db *mariadb.Server) error {
	filters := map[string][]string{}
	conns, err := db.Connections(ctx)
	if err != nil {
		return err
	}
	for _, c := range conns {
		f, err := db.Filter(ctx, c.Name)
		if err != nil {
			return err
		}
		filters[c.Name] = f
	}
	path := a.mariadbConf
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return fmt.Errorf("MariaDB's configuration folder %s is missing", filepath.Dir(path))
	}
	return writeAtomic(path, mariadb.Config(a.ServerID(), filters), 0o644)
}

func (a *Agent) dbReplSetup(ctx context.Context, req agentproto.DBReplSetup) (agentproto.DBReplSetupResponse, error) {
	var resp agentproto.DBReplSetupResponse
	a.dbrepl.mu.Lock()
	defer a.dbrepl.mu.Unlock()
	db, err := a.mariaDB()
	if err != nil {
		return resp, err
	}
	defer db.Close()
	if err := a.writeMariaDBConf(ctx, db); err != nil {
		return resp, err
	}
	on, err := db.BinlogOn(ctx)
	if err != nil {
		return resp, err
	}
	if !on {
		// The binary log only starts with the server: once.
		a.log.Warn("restarting MariaDB to turn its binary log on (database replication)")
		if err := a.restartMariaDB(ctx); err != nil {
			return resp, fmt.Errorf("restart MariaDB to turn its binary log on: %w", err)
		}
		resp.Restarted = true
		db.Close()
		if db, err = a.waitMariaDB(ctx); err != nil {
			return resp, err
		}
		defer db.Close()
		if on, err = db.BinlogOn(ctx); err != nil || !on {
			return resp, fmt.Errorf("MariaDB's binary log is still off after a restart (%v)", err)
		}
	}
	if err := db.SetIdentity(ctx, a.ServerID()); err != nil {
		return resp, err
	}
	if req.Password != "" {
		if err := db.SourceSetup(ctx, req.Password, req.Databases); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

func (a *Agent) restartMariaDB(ctx context.Context) error {
	if a.restartDB != nil {
		return a.restartDB()
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("no systemd here: restart MariaDB yourself")
	}
	out, err := exec.CommandContext(ctx, "systemctl", "restart", "mariadb").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitMariaDB waits for MariaDB to answer again after a restart.
func (a *Agent) waitMariaDB(ctx context.Context) (*mariadb.Server, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if mariadb.Available(a.mysql) {
			db, err := mariadb.Open(a.mysql)
			if err == nil {
				if _, err = db.Version(ctx); err == nil {
					return db, nil
				}
				db.Close()
			}
		}
		if time.Now().After(deadline) {
			return nil, errors.New("MariaDB did not come back after the restart")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (a *Agent) dbUserHashes(ctx context.Context, req agentproto.DBUserHashes) (agentproto.DBUserHashesResponse, error) {
	resp := agentproto.DBUserHashesResponse{Hashes: map[string]string{}}
	if len(req.Users) > 10000 {
		return resp, errors.New("too many users")
	}
	db, err := a.mariaDB()
	if err != nil {
		return resp, err
	}
	defer db.Close()
	for _, u := range req.Users {
		h, err := db.UserHash(ctx, u)
		if err != nil {
			return resp, fmt.Errorf("%s: %w", u, err)
		}
		resp.Hashes[u] = h
	}
	return resp, nil
}

func (a *Agent) dbReplApply(ctx context.Context, req agentproto.DBReplApply) (agentproto.DBReplApplyResponse, error) {
	resp := agentproto.DBReplApplyResponse{Sources: []agentproto.DBReplState{}}
	if err := req.Validate(); err != nil {
		return resp, err
	}
	a.dbrepl.mu.Lock()
	defer a.dbrepl.mu.Unlock()
	db, err := a.mariaDB()
	if err != nil {
		return resp, err
	}
	defer db.Close()
	want := map[string]agentproto.DBReplSource{}
	keep := map[string]bool{}
	for _, id := range req.Keep {
		keep[mariadb.ConnName(id)] = true
	}
	for _, s := range req.Sources {
		if s.ServerID == a.ServerID() {
			return resp, errors.New("a server cannot copy from itself")
		}
		want[mariadb.ConnName(s.ServerID)] = s
	}
	// Sources no longer listed: their connection and copies go.
	conns, err := db.Connections(ctx)
	if err != nil {
		return resp, err
	}
	for _, c := range conns {
		if _, ok := want[c.Name]; ok || keep[c.Name] {
			continue
		}
		dbs, err := db.Filter(ctx, c.Name)
		if err != nil {
			return resp, err
		}
		if err := db.Remove(ctx, c.Name); err != nil {
			return resp, err
		}
		for _, d := range dbs {
			if err := db.DropCopy(ctx, d, d); err != nil {
				return resp, fmt.Errorf("drop the copy of %s: %w", d, err)
			}
		}
		delete(a.dbrepl.sources, c.Name)
		a.log.Info("stopped copying databases", "connection", c.Name)
	}
	for _, s := range req.Sources {
		st := a.applySource(ctx, db, s)
		resp.Sources = append(resp.Sources, st)
	}
	if err := a.writeMariaDBConf(ctx, db); err != nil {
		return resp, err
	}
	return resp, nil
}

// applySource brings one source's connection to what s lists and reports
// how it goes.
func (a *Agent) applySource(ctx context.Context, db *mariadb.Server, s agentproto.DBReplSource) agentproto.DBReplState {
	conn := mariadb.ConnName(s.ServerID)
	st := agentproto.DBReplState{ServerID: s.ServerID, Lag: -1, Databases: []string{}}
	fail := func(err error) agentproto.DBReplState {
		st.Error = err.Error()
		if cur, ferr := db.Filter(ctx, conn); ferr == nil && cur != nil {
			st.Databases = cur
		}
		return st
	}
	status, err := db.Status(ctx, conn)
	if err != nil {
		return fail(err)
	}
	key := strconv.Itoa(s.Port) + " " + s.Password
	if status == nil || a.dbrepl.sources[conn] != key {
		if err := db.Source(ctx, conn, s.Port, s.Password); err != nil {
			return fail(err)
		}
		if a.dbrepl.sources == nil {
			a.dbrepl.sources = map[string]string{}
		}
		a.dbrepl.sources[conn] = key
	}
	cur, err := db.Filter(ctx, conn)
	if err != nil {
		return fail(err)
	}
	var names []string
	byName := map[string]agentproto.DBCopy{}
	for _, d := range s.Databases {
		names = append(names, d.Name)
		byName[d.Name] = d
	}
	var add, keep []string
	for _, n := range names {
		if slices.Contains(cur, n) {
			keep = append(keep, n)
		} else {
			add = append(add, n)
		}
	}
	var drop []string
	for _, n := range cur {
		if !slices.Contains(names, n) {
			drop = append(drop, n)
		}
	}
	if len(drop) > 0 {
		if err := db.Stop(ctx, conn); err != nil {
			return fail(err)
		}
		if err := db.SetFilter(ctx, conn, keep); err != nil {
			return fail(err)
		}
		for _, d := range drop {
			if err := db.DropCopy(ctx, d, d); err != nil {
				a.log.Warn("drop database copy", "database", d, "err", err)
			}
		}
		cur = keep
	}
	if len(add) > 0 {
		if err := a.seed(ctx, db, s, conn, cur, add); err != nil {
			return fail(fmt.Errorf("copy %s: %w", strings.Join(add, ", "), err))
		}
		cur = append(cur, add...)
	}
	// A connection set up with database filters moves to table filters
	// (mariadb.wildPattern); seeding and dropping above did already.
	if status != nil && status.DoDB != "" && len(drop) == 0 && len(add) == 0 && len(cur) > 0 {
		if err := db.Stop(ctx, conn); err != nil {
			return fail(err)
		}
		if err := db.SetFilter(ctx, conn, cur); err != nil {
			return fail(err)
		}
	}
	// The users, read-only here (a changed password arrives as a new hash).
	for _, n := range cur {
		if d, ok := byName[n]; ok {
			if err := db.ReplicaUser(ctx, d.Name, d.User, d.Hash); err != nil {
				return fail(err)
			}
		}
	}
	st.Databases = cur
	if len(cur) == 0 {
		return st
	}
	if status, err = db.Status(ctx, conn); err != nil {
		return fail(err)
	}
	if status != nil && !status.IORunning && !status.SQLRunning && status.SQLError == "" {
		if err := db.Start(ctx, conn, ""); err != nil {
			return fail(err)
		}
		// Give it a moment to reach the source, to report how that went.
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
			if status, err = db.Status(ctx, conn); err != nil || status == nil || status.IORunning || status.IOError != "" || status.SQLError != "" {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	if status != nil {
		st.Running = status.IORunning && status.SQLRunning
		st.Lag = status.Lag
		switch {
		case status.SQLError != "":
			st.Error = status.SQLError
		case status.IOError != "":
			st.Error = status.IOError
		}
	}
	return st
}

// seedDir holds the dumps of seeds under way.
const seedDir = "dbseed"

// clearSeeds removes dumps left behind when the agent stopped mid-seed
// (none can be under way before it runs).
func (a *Agent) clearSeeds() {
	dir := filepath.Join(a.dir, seedDir)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			a.log.Info("removed a leftover database dump", "file", e.Name())
		}
	}
}

// seed copies new databases from the source: with the connection stopped,
// a dump through the tunnel gives their state at a GTID position; the
// connection catches up to exactly there, then copies them from there on.
func (a *Agent) seed(ctx context.Context, db *mariadb.Server, s agentproto.DBReplSource, conn string, cur, add []string) error {
	if err := db.Stop(ctx, conn); err != nil {
		return err
	}
	dir := filepath.Join(a.dir, seedDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var rnd [8]byte
	rand.Read(rnd[:])
	file := filepath.Join(dir, hex.EncodeToString(rnd[:])+".sql")
	defer os.Remove(file)
	if err := a.dumpFrom(ctx, s, add, file); err != nil {
		return err
	}
	head, err := readHead(file, 64<<10)
	if err != nil {
		return err
	}
	pos, err := mariadb.DumpGTID(head, s.ServerID)
	if err != nil {
		return err
	}
	all, _ := mariadb.DumpGTIDs(head)
	a.log.Info("seeding database copies", "source", s.ServerID, "databases", add, "copying", cur, "position", pos)
	if len(cur) > 0 {
		// Copies already flowing catch up to the dump's position.
		if pos != "" {
			if err := db.Start(ctx, conn, pos); err != nil {
				return err
			}
			wctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
			err := db.WaitPos(wctx, pos)
			cancel()
			if err != nil {
				if st, _ := db.Status(ctx, conn); st != nil && st.SQLError != "" {
					err = errors.New(st.SQLError)
				}
				return fmt.Errorf("catch up to the dump: %w", err)
			}
			if err := db.Stop(ctx, conn); err != nil {
				return err
			}
		}
	} else if err := db.SetSlavePos(ctx, s.ServerID, all); err != nil {
		return err
	}
	// A copy there before (a former primary coming back) starts over.
	for _, d := range add {
		if err := db.DropCopy(ctx, d, s.UserOf(d)); err != nil {
			return err
		}
	}
	if err := a.importDump(ctx, file); err != nil {
		return err
	}
	return db.SetFilter(ctx, conn, append(slices.Clone(cur), add...))
}

func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	k, err := io.ReadFull(f, b)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b[:k], nil
}

// dumpFrom writes a consistent dump of the source's databases, with its
// GTID position, through the tunnel's loopback port.
func (a *Agent) dumpFrom(ctx context.Context, s agentproto.DBReplSource, dbs []string, file string) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	args := []string{"--host=127.0.0.1", "--port=" + strconv.Itoa(s.Port), "--user=" + mariadb.ReplUser,
		"--single-transaction", "--gtid", "--master-data=2", "--routines", "--triggers", "--events", "--databases"}
	args = append(args, dbs...)
	cmd := exec.CommandContext(ctx, a.dumpBin("mariadb-dump", "mysqldump"), args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+s.Password)
	cmd.Stdout = f
	var stderr strings.Builder
	cmd.Stderr = &limitWriter{w: &stderr, n: 4096}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dump on the source: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return f.Sync()
}

// importDump loads a dump into the local server, outside its binary log.
func (a *Agent) importDump(ctx context.Context, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, a.dumpBin("mariadb", "mysql"), "--socket="+a.mysql, "--user=root",
		"--init-command=SET SESSION sql_log_bin = 0")
	cmd.Stdin = bufio.NewReaderSize(f, 1<<20)
	var stderr strings.Builder
	cmd.Stderr = &limitWriter{w: &stderr, n: 4096}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("load the copy: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// dumpBin is the first of the names found in PATH (MariaDB's or MySQL's
// client tools), or the first name.
func (a *Agent) dumpBin(names ...string) string {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return names[0]
}

// limitWriter keeps the first n bytes written.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// dbDemote leaves a primary's site databases only readable and returns the
// position its replicas must reach to have everything.
func (a *Agent) dbDemote(ctx context.Context, req agentproto.DBHandover) (agentproto.DBDemoteResponse, error) {
	var resp agentproto.DBDemoteResponse
	if err := req.Validate(); err != nil {
		return resp, err
	}
	a.dbrepl.mu.Lock()
	defer a.dbrepl.mu.Unlock()
	db, err := a.mariaDB()
	if err != nil {
		return resp, err
	}
	defer db.Close()
	for _, d := range req.Databases {
		if err := db.ReadOnly(ctx, d.Name, d.User); err != nil {
			return resp, err
		}
	}
	resp.Pos, err = db.BinlogPos(ctx)
	return resp, err
}

// dbPromote makes this server's copies of a site's databases its own:
// caught up with the former primary if it says where to, out of the
// replication from it, writable by the site's users.
func (a *Agent) dbPromote(ctx context.Context, req agentproto.DBHandover) error {
	if err := req.Validate(); err != nil {
		return err
	}
	if req.Source <= 0 {
		return errors.New("source required")
	}
	a.dbrepl.mu.Lock()
	defer a.dbrepl.mu.Unlock()
	db, err := a.mariaDB()
	if err != nil {
		return err
	}
	defer db.Close()
	conn := mariadb.ConnName(req.Source)
	if req.WaitPos != "" {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := db.WaitPos(wctx, req.WaitPos)
		cancel()
		if err != nil {
			return fmt.Errorf("the copies did not catch up with the primary: %w", err)
		}
	}
	cur, err := db.Filter(ctx, conn)
	if err != nil {
		return err
	}
	var rest []string
	for _, n := range cur {
		if !slices.ContainsFunc(req.Databases, func(d agentproto.DBCopy) bool { return d.Name == n }) {
			rest = append(rest, n)
		}
	}
	if len(rest) == 0 {
		if err := db.Remove(ctx, conn); err != nil {
			return err
		}
		delete(a.dbrepl.sources, conn)
	} else {
		if err := db.Stop(ctx, conn); err != nil {
			return err
		}
		if err := db.SetFilter(ctx, conn, rest); err != nil {
			return err
		}
		if err := db.Start(ctx, conn, ""); err != nil {
			return err
		}
	}
	for _, d := range req.Databases {
		if err := db.Owner(ctx, d.Name, d.User); err != nil {
			return err
		}
	}
	return a.writeMariaDBConf(ctx, db)
}
