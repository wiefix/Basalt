//go:build linux_integration

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agent"
)

// Two MariaDB servers run in the test container (deploy/dev/linux-test.sh):
// the default one and a second at secondSocket, both with a binary log.
const secondSocket = "/run/mysqld2/mysqld.sock"

func sqlOn(t *testing.T, socket, user, password, query string) (string, error) {
	t.Helper()
	args := []string{"--socket=" + socket, "--user=" + user, "-N", "-B", "-e", query}
	cmd := exec.Command("mariadb", args...)
	if password != "" {
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+password)
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestLinuxDBReplication: the databases of a site served by two servers are
// copied from its server's MariaDB to the other's through the agents'
// tunnel: seeded, then kept up to date; read-only for the site's user
// there; a new database joins, a dropped one and one of another site stay
// away; removing the copy drops it.
func TestLinuxDBReplication(t *testing.T) {
	if os.Geteuid() != 0 || !mariadbUp(secondSocket) {
		t.Skip("needs root and two MariaDB servers")
	}
	p := startPanel(t)
	// The test servers' MariaDB is shared with the other tests: leave no
	// site databases, users or connections behind.
	t.Cleanup(func() {
		for _, sock := range []string{"/run/mysqld/mysqld.sock", secondSocket} {
			sqlOn(t, sock, "root", "", "STOP ALL SLAVES")
			names, _ := sqlOn(t, sock, "root", "", "SELECT schema_name FROM information_schema.schemata WHERE schema_name REGEXP '^s[0-9]+_'")
			users, _ := sqlOn(t, sock, "root", "", "SELECT user FROM mysql.user WHERE user REGEXP '^s[0-9]+_' OR user = 'basalt_repl'")
			for _, n := range strings.Fields(names) {
				sqlOn(t, sock, "root", "", "DROP DATABASE `"+n+"`")
			}
			for _, u := range strings.Fields(users) {
				sqlOn(t, sock, "root", "", "DROP USER '"+u+"'@'localhost'")
			}
		}
	})
	confDir := t.TempDir()
	newHost := func(socket, conf string) *host {
		return startHostWith(t, p, "", func(c *agent.Config) {
			c.MySQLSocket = socket
			c.PeerListen = "127.0.0.1:0"
			c.SiteDBDir = filepath.Join(confDir, conf+".db")
			c.MariaDBConf = filepath.Join(confDir, conf)
			c.RestartMariaDB = func() error { t.Error("MariaDB restart asked for: its binary log is on"); return nil }
		})
	}
	h1, h2 := newHost("/run/mysqld/mysqld.sock", "one.cnf"), newHost(secondSocket, "two.cnf")
	for _, h := range []*host{h1, h2} {
		p.call(200, "PUT", "/api/v1/servers/"+h.id+"/public-ips", map[string]any{"ips": []string{"127.0.0.1"}})
		eventually(t, "tunnel and MariaDB reported", func() bool {
			s := p.server(h.id)
			return s["peer_port"] != nil && s["peer_port"].(float64) > 0 && s["mariadb"] != ""
		})
	}
	p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "repl.test"})
	newSite := func(name string) string {
		return jsonID(p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{"server_id": mustAtoi(h1.id), "name": name,
			"domains": []string{name + ".repl.test"}, "tls": "internal"})["site"].(map[string]any)["id"])
	}
	shop, other := newSite("shop"), newSite("other")
	cred := p.call(http.StatusCreated, "POST", "/api/v1/sites/"+shop+"/databases", map[string]any{"name": "app"})["credentials"].(map[string]any)
	db, user, pw := cred["database"].(string), cred["username"].(string), cred["password"].(string)
	otherDB := p.call(http.StatusCreated, "POST", "/api/v1/sites/"+other+"/databases", map[string]any{"name": "app"})["credentials"].(map[string]any)["database"].(string)
	src := "/run/mysqld/mysqld.sock"
	mustSQL := func(socket, u, pass, q string) string {
		t.Helper()
		out, err := sqlOn(t, socket, u, pass, q)
		if err != nil {
			t.Fatalf("%s: %v: %s", q, err, out)
		}
		return out
	}
	mustSQL(src, user, pw, "CREATE TABLE "+db+".t (id INT PRIMARY KEY, v VARCHAR(20)); INSERT INTO "+db+".t VALUES (1, 'before')")
	mustSQL(src, "root", "", "CREATE TABLE "+otherDB+".t (v INT); INSERT INTO "+otherDB+".t VALUES (1)")

	base := "/api/v1/sites/" + shop
	p.call(http.StatusCreated, "POST", base+"/replicas", map[string]any{"server_id": mustAtoi(h2.id)})
	copyState := func() map[string]any {
		pl := p.call(200, "GET", base, nil)["placements"].([]any)[1].(map[string]any)
		d, _ := pl["databases"].(map[string]any)
		return d
	}
	copied := func(names ...string) func() bool {
		return func() bool {
			d := copyState()
			if d == nil || d["running"] != true {
				return false
			}
			for _, n := range names {
				if !slices.Contains(jsonStrings(d["copied"]), n) {
					return false
				}
			}
			return true
		}
	}
	waitFor(t, "the first copy", 60*time.Second, copied(db), func() string { return jsonString(copyState()) })
	dst := secondSocket
	if got := mustSQL(dst, "root", "", "SELECT v FROM "+db+".t WHERE id = 1"); got != "before" {
		t.Fatalf("seeded row: %q", got)
	}
	// Changes follow.
	mustSQL(src, user, pw, "INSERT INTO "+db+".t VALUES (2, 'after'); UPDATE "+db+".t SET v = 'changed' WHERE id = 1")
	waitFor(t, "changes copied", 30*time.Second, func() bool {
		out, _ := sqlOn(t, dst, "root", "", "SELECT GROUP_CONCAT(v ORDER BY id) FROM "+db+".t")
		return out == "changed,after"
	}, nil)
	// The site's user reads its copy there, and cannot write.
	if got := mustSQL(dst, user, pw, "SELECT COUNT(*) FROM "+db+".t"); got != "2" {
		t.Fatalf("read on the copy: %q", got)
	}
	if out, err := sqlOn(t, dst, user, pw, "INSERT INTO "+db+".t VALUES (3, 'x')"); err == nil {
		t.Fatalf("wrote on the copy: %s", out)
	}
	// The copy of the site on the second server reaches the database on
	// the first through its socket there: what it writes lands on the
	// source and comes back as a copy.
	siteSock := filepath.Join(confDir, "two.cnf.db", "site-"+shop+".sock")
	waitFor(t, "the copy's database socket", 30*time.Second, func() bool { return mariadbUp(siteSock) }, nil)
	mustSQL(siteSock, user, pw, "INSERT INTO "+db+".t VALUES (10, 'from-copy')")
	if got := mustSQL(src, "root", "", "SELECT v FROM "+db+".t WHERE id = 10"); got != "from-copy" {
		t.Fatalf("written through the copy: %q", got)
	}
	waitFor(t, "the copy's write copied back", 30*time.Second, func() bool {
		out, _ := sqlOn(t, dst, "root", "", "SELECT v FROM "+db+".t WHERE id = 10")
		return out == "from-copy"
	}, nil)
	mustSQL(src, "root", "", "DELETE FROM "+db+".t WHERE id = 10")
	// Another site's database is not copied.
	if out, _ := sqlOn(t, dst, "root", "", "SHOW DATABASES LIKE '"+otherDB+"'"); out != "" {
		t.Fatalf("another site's database was copied: %q", out)
	}
	mustSQL(src, "root", "", "INSERT INTO "+otherDB+".t VALUES (2)")

	// A new database of the site joins the copy, aligned with the first.
	cred2 := p.call(http.StatusCreated, "POST", base+"/databases", map[string]any{"name": "logs"})["credentials"].(map[string]any)
	db2 := cred2["database"].(string)
	mustSQL(src, "root", "", "INSERT INTO "+db+".t VALUES (4, 'during')")
	p.call(http.StatusAccepted, "POST", base+"/sync", nil) // a round also comes with the database; this one makes sure
	waitFor(t, "the second database", 60*time.Second, copied(db, db2), func() string { return jsonString(copyState()) })
	// A table made naming its database, with no current database (as
	// migration tools and consoles may), reaches the copy: filtering by
	// database would judge the statement by the current one and skip it.
	mustSQL(src, "root", "", "CREATE TABLE "+db2+".l (v INT); INSERT INTO "+db2+".l VALUES (7)")
	waitFor(t, "both databases current", 30*time.Second, func() bool {
		a, _ := sqlOn(t, dst, "root", "", "SELECT COUNT(*) FROM "+db+".t")
		b, _ := sqlOn(t, dst, "root", "", "SELECT SUM(v) FROM "+db2+".l")
		return a == "3" && b == "7"
	}, func() string {
		a, _ := sqlOn(t, dst, "root", "", "SELECT GROUP_CONCAT(id) FROM "+db+".t")
		b, _ := sqlOn(t, dst, "root", "", "SELECT SUM(v) FROM "+db2+".l")
		srcRows, _ := sqlOn(t, src, "root", "", "SELECT GROUP_CONCAT(id), @@gtid_binlog_pos FROM "+db+".t")
		pos, _ := sqlOn(t, dst, "root", "", "SELECT @@gtid_slave_pos, @@gtid_current_pos")
		return a + " / " + b + " / source " + srcRows + " / replica pos " + pos + " / " + jsonString(copyState())
	})
	// The replica's option file keeps what it copies over a restart.
	filter := func(d string) string {
		return "basalt_" + h1.id + ".replicate_wild_do_table = " + strings.ReplaceAll(d, "_", `\\_`) + ".%\n"
	}
	if b, _ := os.ReadFile(filepath.Join(confDir, "two.cnf")); !strings.Contains(string(b), filter(db)) ||
		!strings.Contains(string(b), filter(db2)) || !strings.Contains(string(b), "server_id = "+h2.id) {
		t.Fatalf("two.cnf:\n%s", b)
	}
	// A connection made by an older Basalt (filtering by database) moves
	// to table filters without copying its databases again.
	conn := "basalt_" + h1.id
	// A row only the copy has: a new seed would lose it.
	mustSQL(dst, "root", "", "SET sql_log_bin = 0; INSERT INTO "+db2+".l VALUES (1000)")
	mustSQL(dst, "root", "", "SET @@default_master_connection = '"+conn+"'; STOP SLAVE; SET GLOBAL replicate_wild_do_table = ''; "+
		"SET GLOBAL replicate_do_db = '"+db+","+db2+"'; START SLAVE")
	p.call(http.StatusAccepted, "POST", base+"/sync", nil)
	waitFor(t, "the old filter replaced", 30*time.Second, func() bool {
		out, _ := sqlOn(t, dst, "root", "", "SHOW SLAVE '"+conn+"' STATUS\\G")
		// Values only (no names): the old filter's value gone, the new one there,
		// both threads running.
		return !strings.Contains(out, db+","+db2) && strings.Contains(out, `\_logs.%`) && strings.Count(out, "\nYes\n") >= 2
	}, func() string { out, _ := sqlOn(t, dst, "root", "", "SHOW SLAVE '"+conn+"' STATUS\\G"); return out })
	if got := mustSQL(dst, "root", "", "SELECT COUNT(*) FROM "+db2+".l WHERE v = 1000"); got != "1" {
		t.Fatal("the databases were copied again")
	}
	mustSQL(dst, "root", "", "SET sql_log_bin = 0; DELETE FROM "+db2+".l WHERE v = 1000")
	mustSQL(src, "root", "", "INSERT INTO "+db2+".l VALUES (1)")
	waitFor(t, "the second database follows", 30*time.Second, func() bool {
		b, _ := sqlOn(t, dst, "root", "", "SELECT SUM(v) FROM "+db2+".l")
		return b == "8"
	}, nil)

	// A planned switch: the second server becomes the primary; its own
	// MariaDB takes the writes, and the first copies from it.
	r := p.call(200, "PUT", base+"/primary", map[string]any{"server_id": mustAtoi(h2.id)})
	if jsonString(r["site"].(map[string]any)["server_id"]) != h2.id || r["warning"] != nil {
		t.Fatalf("switch: %v", r)
	}
	mustSQL(dst, user, pw, "INSERT INTO "+db+".t VALUES (20, 'on-two')")
	if out, err := sqlOn(t, src, user, pw, "INSERT INTO "+db+".t VALUES (21, 'x')"); err == nil {
		t.Fatalf("the former primary still takes writes: %s", out)
	}
	back := func() bool {
		out, _ := sqlOn(t, src, "root", "", "SELECT v FROM "+db+".t WHERE id = 20")
		return out == "on-two"
	}
	waitFor(t, "the former primary copying from the new one", 60*time.Second, back, func() string { return jsonString(p.call(200, "GET", base, nil)["placements"]) })
	// And back again.
	p.call(200, "PUT", base+"/primary", map[string]any{"server_id": mustAtoi(h1.id)})
	mustSQL(src, user, pw, "DELETE FROM "+db+".t WHERE id = 20")
	waitFor(t, "the copy following the first server again", 60*time.Second, func() bool {
		out, _ := sqlOn(t, dst, "root", "", "SELECT COUNT(*) FROM "+db+".t WHERE id = 20")
		return out == "0"
	}, func() string { return jsonString(p.call(200, "GET", base, nil)["placements"]) })
	if out, err := sqlOn(t, dst, user, pw, "INSERT INTO "+db+".t VALUES (22, 'x')"); err == nil {
		t.Fatalf("the copy takes writes again: %s", out)
	}
	// The first server, after it was a copy once, becomes one again: its
	// copy starts over (MariaDB keeps the filters of a removed connection
	// by its name: the new one must not inherit them and skip the seed),
	// so what only it had is gone.
	mustSQL(src, "root", "", "SET sql_log_bin = 0; INSERT INTO "+db+".t VALUES (30, 'only-here')")
	p.call(200, "PUT", base+"/primary", map[string]any{"server_id": mustAtoi(h2.id)})
	waitFor(t, "the first server's copy made again", 60*time.Second, func() bool {
		out, _ := sqlOn(t, src, "root", "", "SELECT COUNT(*) FROM "+db+".t WHERE id = 30")
		d := copyStateOf(p, base, h1.id)
		return out == "0" && d != nil && d["running"] == true
	}, func() string { return jsonString(p.call(200, "GET", base, nil)["placements"]) })
	p.call(200, "PUT", base+"/primary", map[string]any{"server_id": mustAtoi(h1.id)})
	waitFor(t, "the second server copying again", 60*time.Second, func() bool {
		d := copyState()
		return d != nil && d["running"] == true
	}, func() string { return jsonString(p.call(200, "GET", base, nil)["placements"]) })
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/primary", map[string]any{"server_id": mustAtoi(h1.id)})

	// Taking the copy away drops the databases there.
	p.call(200, "DELETE", base+"/replicas/"+h2.id, nil)
	waitFor(t, "the copies dropped", 60*time.Second, func() bool {
		out, _ := sqlOn(t, dst, "root", "", "SELECT GROUP_CONCAT(schema_name) FROM information_schema.schemata WHERE schema_name REGEXP '^s[0-9]+_'")
		conns, _ := sqlOn(t, dst, "root", "", "SELECT COUNT(*) FROM information_schema.SLAVE_STATUS")
		return (out == "" || out == "NULL") && (conns == "0" || conns == "")
	}, func() string {
		out, _ := sqlOn(t, dst, "root", "", "SELECT GROUP_CONCAT(schema_name) FROM information_schema.schemata WHERE schema_name REGEXP '^s[0-9]+_'")
		conns, err := sqlOn(t, dst, "root", "", "SELECT COUNT(*) FROM information_schema.SLAVE_STATUS")
		return out + " / " + conns + " " + fmt.Sprint(err)
	})
	// The source's databases are untouched.
	if got := mustSQL(src, "root", "", "SELECT COUNT(*) FROM "+db+".t"); got != "3" {
		t.Fatalf("source rows: %q", got)
	}
	// The option files keep the identities and the filters.
	b, _ := os.ReadFile(filepath.Join(confDir, "one.cnf"))
	if !strings.Contains(string(b), "server_id = "+h1.id) || !strings.Contains(string(b), "log_bin") {
		t.Fatalf("one.cnf:\n%s", b)
	}
}

// copyStateOf is how server id copies the site's databases.
func copyStateOf(p *panel, base, id string) map[string]any {
	for _, pl := range p.call(200, "GET", base, nil)["placements"].([]any) {
		if pl := pl.(map[string]any); jsonID(pl["server_id"]) == id {
			d, _ := pl["databases"].(map[string]any)
			return d
		}
	}
	return nil
}

func mariadbUp(socket string) bool {
	fi, err := os.Stat(socket)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

func jsonStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// waitFor is eventually with a longer limit and what to say when it fails.
func waitFor(t *testing.T, what string, limit time.Duration, cond func() bool, state func() string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			msg := ""
			if state != nil {
				msg = ": " + state()
			}
			t.Fatalf("timed out waiting for %s%s", what, msg)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
