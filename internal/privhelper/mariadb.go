package privhelper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wiefix/basalt/internal/mariadb"
)

// DefaultMariaDBConf is MariaDB's option file of database replication.
const DefaultMariaDBConf = "/etc/mysql/mariadb.conf.d/90-basalt.cnf"

// MariaDBConfig is the replication setup MariaDB keeps over restarts: the
// server's id, and the databases each replica connection copies.
type MariaDBConfig struct {
	ServerID int64               `json:"server_id"`
	Filters  map[string][]string `json:"filters"`
}

// WriteMariaDBConfig writes MariaDB's option file of replication.
func (h *Helper) WriteMariaDBConfig(_ context.Context, c MariaDBConfig) error {
	if c.ServerID <= 0 || c.ServerID > 1<<32-1 {
		return errors.New("invalid server id")
	}
	for conn, dbs := range c.Filters {
		if !mariadb.ValidConnName(conn) {
			return fmt.Errorf("invalid replica connection %q", conn)
		}
		for _, d := range dbs {
			if !mariadb.ValidName(d) {
				return fmt.Errorf("invalid database name %q", d)
			}
		}
	}
	path := h.cfg.MariaDBConf
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return fmt.Errorf("MariaDB's configuration folder %s is missing", filepath.Dir(path))
	}
	h.mariadbMu.Lock()
	defer h.mariadbMu.Unlock()
	return writeFile(path, mariadb.Config(c.ServerID, c.Filters), 0o644)
}

// MariaDBAccount gives the agent's system user (when not root) an account
// of MariaDB on the server's socket (mariadb.AgentAccount); the agent signs
// in with it.
func (h *Helper) MariaDBAccount(ctx context.Context) error {
	if h.cfg.AgentUser == "" || h.cfg.AgentUser == "root" {
		return nil
	}
	if !mariadb.Available(h.cfg.MySQLSocket) {
		return errors.New("MariaDB is not running on this server")
	}
	db, err := mariadb.Open(h.cfg.MySQLSocket)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.AgentAccount(ctx, h.cfg.AgentUser)
}

// RestartMariaDB restarts MariaDB (its binary log only starts with it).
func (h *Helper) RestartMariaDB(ctx context.Context) error {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("no systemd here: restart MariaDB yourself")
	}
	out, err := exec.CommandContext(ctx, "systemctl", "restart", "mariadb").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeFile replaces path with data, never leaving it half written.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
