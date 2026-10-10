// Package dbengine describes the databases Basalt runs as containers: their
// official images, the versions offered, how credentials reach the image,
// where data lives and how readiness is checked.
//
// A database is an app with a single service whose container the panel
// defines here, so deployments, logs and status work as for compose apps.
package dbengine

import (
	"crypto/rand"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
)

// Volume is the name of the volume that holds a database's data.
const Volume = "data"

// Engine is one kind of database.
type Engine struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Versions offered, newest first; the first is the default. A version
	// is a major release: the image tag follows its minor updates.
	Versions []string `json:"versions"`
	Port     int      `json:"port"`
	// EnvVar is the variable apps conventionally read the URL from.
	EnvVar string `json:"env_var"`
	// SQL engines have a database and user; Redis only a password.
	SQL bool `json:"sql"`

	image string // repository on Docker Hub
	tag   string // suffix of the image tag after the version
}

var engines = []*Engine{
	{ID: "postgres", Name: "PostgreSQL", Versions: []string{"18", "17", "16", "15"}, Port: 5432,
		EnvVar: "DATABASE_URL", SQL: true, image: "postgres", tag: "-alpine"},
	{ID: "mysql", Name: "MySQL", Versions: []string{"8.4"}, Port: 3306,
		EnvVar: "DATABASE_URL", SQL: true, image: "mysql"},
	{ID: "mariadb", Name: "MariaDB", Versions: []string{"11.8", "11.4", "10.11"}, Port: 3306,
		EnvVar: "DATABASE_URL", SQL: true, image: "mariadb"},
	{ID: "redis", Name: "Redis", Versions: []string{"8", "7.4"}, Port: 6379,
		EnvVar: "REDIS_URL", image: "redis", tag: "-alpine"},
}

// All returns the engines offered.
func All() []*Engine { return engines }

// Get returns an engine by ID.
func Get(id string) (*Engine, bool) {
	i := slices.IndexFunc(engines, func(e *Engine) bool { return e.ID == id })
	if i < 0 {
		return nil, false
	}
	return engines[i], true
}

// ValidVersion reports whether the engine offers version.
func (e *Engine) ValidVersion(v string) bool { return slices.Contains(e.Versions, v) }

// Image is the image reference of a version.
func (e *Engine) Image(version string) string { return e.image + ":" + version + e.tag }

// NameRE is the rule for database names: they are also the host name apps
// reach the database by, so a lowercase DNS label.
var NameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}[a-z0-9]$`)

// Credentials are what a client signs in with.
type Credentials struct {
	Database     string `json:"database,omitempty"`
	User         string `json:"user,omitempty"`
	Password     string `json:"password"`
	RootPassword string `json:"root_password,omitempty"` // MySQL and MariaDB
}

// NewCredentials generates credentials for a database named name.
func (e *Engine) NewCredentials(name string) Credentials {
	c := Credentials{Password: rand.Text()}
	if e.SQL {
		// Database and user are named after the database, as identifiers.
		c.Database = strings.ReplaceAll(name, "-", "_")
		c.User = c.Database
		if len(c.User) > 32 { // MySQL's limit
			c.User = c.User[:32]
		}
	}
	if e.ID == "mysql" || e.ID == "mariadb" {
		c.RootPassword = rand.Text()
	}
	return c
}

// Env is how the credentials are stored, as the app's environment: the
// variables the image initialises the database from.
func (e *Engine) Env(c Credentials) map[string]string {
	switch e.ID {
	case "postgres":
		return map[string]string{"POSTGRES_DB": c.Database, "POSTGRES_USER": c.User, "POSTGRES_PASSWORD": c.Password}
	case "mysql":
		return map[string]string{"MYSQL_DATABASE": c.Database, "MYSQL_USER": c.User, "MYSQL_PASSWORD": c.Password,
			"MYSQL_ROOT_PASSWORD": c.RootPassword}
	case "mariadb":
		return map[string]string{"MARIADB_DATABASE": c.Database, "MARIADB_USER": c.User, "MARIADB_PASSWORD": c.Password,
			"MARIADB_ROOT_PASSWORD": c.RootPassword}
	}
	return map[string]string{"REDIS_PASSWORD": c.Password}
}

// CredentialsFrom reads the credentials back from the stored environment.
func (e *Engine) CredentialsFrom(env map[string]string) Credentials {
	switch e.ID {
	case "postgres":
		return Credentials{Database: env["POSTGRES_DB"], User: env["POSTGRES_USER"], Password: env["POSTGRES_PASSWORD"]}
	case "mysql":
		return Credentials{Database: env["MYSQL_DATABASE"], User: env["MYSQL_USER"], Password: env["MYSQL_PASSWORD"],
			RootPassword: env["MYSQL_ROOT_PASSWORD"]}
	case "mariadb":
		return Credentials{Database: env["MARIADB_DATABASE"], User: env["MARIADB_USER"], Password: env["MARIADB_PASSWORD"],
			RootPassword: env["MARIADB_ROOT_PASSWORD"]}
	}
	return Credentials{Password: env["REDIS_PASSWORD"]}
}

// URL is the connection URL for a client reaching the database at host.
func (e *Engine) URL(host string, c Credentials) string {
	u := url.URL{Host: net.JoinHostPort(host, strconv.Itoa(e.Port))}
	switch e.ID {
	case "postgres":
		u.Scheme, u.User, u.Path = "postgres", url.UserPassword(c.User, c.Password), "/"+c.Database
	case "mysql", "mariadb":
		u.Scheme, u.User, u.Path = "mysql", url.UserPassword(c.User, c.Password), "/"+c.Database
	default:
		u.Scheme, u.User = "redis", url.UserPassword("", c.Password)
	}
	return u.String()
}

// Limits on a database container.
type Limits struct {
	MemoryMB int
	CPUs     float64
}

// Service is the container that runs a database named name (also its
// service name, and so its host name on the networks it joins).
func (e *Engine) Service(name, version string, env map[string]string, l Limits) (agentproto.ServiceSpec, error) {
	if !NameRE.MatchString(name) {
		return agentproto.ServiceSpec{}, fmt.Errorf("invalid database name %q", name)
	}
	if !e.ValidVersion(version) {
		return agentproto.ServiceSpec{}, fmt.Errorf("%s %s is not offered", e.Name, version)
	}
	c := e.CredentialsFrom(env)
	if c.Password == "" || e.SQL && (c.Database == "" || c.User == "") {
		return agentproto.ServiceSpec{}, fmt.Errorf("the database's credentials are missing")
	}
	svc := agentproto.ServiceSpec{
		Name: name, Image: e.Image(version), Env: e.Env(c), Restart: "unless-stopped",
		MemoryMB: l.MemoryMB, CPUs: l.CPUs,
		// Databases flush and close their files on stop; give them time.
		StopGrace: 60,
		Health: &docker.HealthConfig{Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 5,
			StartPeriod: 5 * time.Minute, StartInterval: time.Second},
	}
	path := "/var/lib/mysql"
	switch e.ID {
	case "postgres":
		// From 18 the image keeps data in a versioned directory below
		// /var/lib/postgresql, which eases major upgrades.
		path = "/var/lib/postgresql/data"
		if major, _ := strconv.Atoi(version); major >= 18 {
			path = "/var/lib/postgresql"
		}
		// The default 64 MB of shared memory fails parallel queries.
		svc.ShmSizeMB = 256
		// Over TCP: during initialisation the server listens on its socket only.
		svc.Health.Test = []string{"CMD", "pg_isready", "-q", "-h", "127.0.0.1", "-U", c.User, "-d", c.Database}
	case "mysql":
		// mysqladmin ping succeeds once the server answers, even when access is
		// denied; over TCP, as the server initialising is socket-only.
		svc.Health.Test = []string{"CMD", "mysqladmin", "ping", "-h", "127.0.0.1", "--silent"}
	case "mariadb":
		svc.Health.Test = []string{"CMD", "healthcheck.sh", "--connect", "--innodb_initialized"}
	case "redis":
		path = "/data"
		svc.Command = []string{"redis-server", "--appendonly", "yes", "--requirepass", c.Password}
		svc.Env = map[string]string{"REDISCLI_AUTH": c.Password} // for the health check
		svc.Health.Test = []string{"CMD-SHELL", "redis-cli ping | grep -q PONG"}
	}
	svc.Mounts = []agentproto.ServiceMount{{Volume: Volume, Path: path}}
	return svc, nil
}
