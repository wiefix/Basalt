package agentproto

import (
	"errors"
	"fmt"
	"regexp"
)

// Replication of site databases between servers (MariaDB). A source keeps
// a binary log and a replication user; a replica copies some of each
// source's databases through a loopback port its tunnel forwards there.

const (
	// TypeDBReplSetup prepares a server taking part (DBReplSetup).
	TypeDBReplSetup = "db_repl_setup"
	// TypeDBReplApply sets what a replica copies from which source
	// (DBReplApply). Streaming: seeding a copy takes a while (no frames).
	TypeDBReplApply = "db_repl_apply"
	// TypeDBUserHashes returns the password hashes of database users.
	TypeDBUserHashes = "db_user_hashes"
)

type DBReplSetup struct {
	// Password of the replication user replicas connect as; "" on a server
	// that is no source.
	Password string `json:"password,omitempty"`
	// Databases replicas copy from this server.
	Databases []string `json:"databases,omitempty"`
}

type DBReplSetupResponse struct {
	// Restarted: MariaDB was restarted to turn its binary log on.
	Restarted bool `json:"restarted,omitempty"`
}

// DBCopy is a database a replica copies, with its user (read-only there)
// and the user's password hash on the source.
type DBCopy struct {
	Name string `json:"name"`
	User string `json:"user"`
	Hash string `json:"hash"`
}

// DBReplSource is a server a replica copies databases from, through the
// loopback Port.
type DBReplSource struct {
	ServerID  int64    `json:"server_id"`
	Port      int      `json:"port"`
	Password  string   `json:"password"`
	Databases []DBCopy `json:"databases"`
}

// DBReplApply is everything a replica copies; sources and databases left
// out are dropped (their copies too), except the sources in Keep (the
// panel cannot reach them now: their copies stay as they are).
type DBReplApply struct {
	Sources []DBReplSource `json:"sources"`
	Keep    []int64        `json:"keep,omitempty"`
}

// DBReplState is how copying from a source goes.
type DBReplState struct {
	ServerID  int64    `json:"server_id"`
	Running   bool     `json:"running"`
	Lag       int64    `json:"lag"` // seconds behind (-1: unknown)
	Error     string   `json:"error,omitempty"`
	Databases []string `json:"databases"` // copied (seeded)
}

type DBReplApplyResponse struct {
	Sources []DBReplState `json:"sources"`
}

type DBUserHashes struct {
	Users []string `json:"users"`
}

type DBUserHashesResponse struct {
	Hashes map[string]string `json:"hashes"`
}

var hashRE = regexp.MustCompile(`^\*[0-9A-F]{40}$`)

// Validate checks names, ports and hashes.
func (r *DBReplApply) Validate() error {
	seen := map[int64]bool{}
	for _, s := range r.Sources {
		if s.ServerID <= 0 || seen[s.ServerID] {
			return errors.New("bad or repeated source")
		}
		seen[s.ServerID] = true
		if s.Port < 1024 || s.Port > 65535 {
			return fmt.Errorf("source %d: bad port", s.ServerID)
		}
		if len(s.Password) < 16 {
			return fmt.Errorf("source %d: password too short", s.ServerID)
		}
		for _, d := range s.Databases {
			if !dbNameRE.MatchString(d.Name) || !dbNameRE.MatchString(d.User) || !hashRE.MatchString(d.Hash) {
				return fmt.Errorf("source %d: bad database %q", s.ServerID, d.Name)
			}
		}
	}
	return nil
}
