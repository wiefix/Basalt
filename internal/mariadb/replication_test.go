package mariadb

import (
	"strings"
	"testing"
)

func TestReplicationConfig(t *testing.T) {
	got := string(Config(7, map[string][]string{"basalt_3": {"s5_wp", "s2_shop"}, "basalt_1": {"s9_x"}}))
	want := `server_id = 7
gtid_domain_id = 7
log_bin = basalt-bin
binlog_format = ROW
binlog_expire_logs_seconds = 604800
relay_log = basalt-relay
basalt_1.replicate_wild_do_table = s9\\_x.%
basalt_3.replicate_wild_do_table = s2\\_shop.%
basalt_3.replicate_wild_do_table = s5\\_wp.%
`
	if !strings.HasSuffix(got, want) || !strings.Contains(got, "[mariadbd]\n") {
		t.Fatalf("config:\n%s", got)
	}
}

func TestDumpGTID(t *testing.T) {
	head := []byte("-- MariaDB dump\n--\n-- SET GLOBAL gtid_slave_pos='0-1-9,3-3-120';\n-- CHANGE MASTER TO MASTER_USE_GTID=slave_pos;\n")
	if g, err := DumpGTID(head, 3); err != nil || g != "3-3-120" {
		t.Fatalf("domain 3: %q %v", g, err)
	}
	if g, err := DumpGTID(head, 5); err != nil || g != "" {
		t.Fatalf("domain 5: %q %v", g, err)
	}
	if _, err := DumpGTID([]byte("-- nothing\n"), 3); err == nil {
		t.Fatal("a dump without a position")
	}
	if g, _ := DumpGTID([]byte("-- SET GLOBAL gtid_slave_pos='';\n"), 3); g != "" {
		t.Fatalf("empty position: %q", g)
	}
	if !ValidHash("*6BB4837EB74329105EE4568DDA7DC67ED2CA2AD9") || ValidHash("*6bb4") || ValidHash("x' OR 1") {
		t.Fatal("ValidHash")
	}
	if ConnName(12) != "basalt_12" || !connRE.MatchString("basalt_12") || connRE.MatchString("basalt_0") || connRE.MatchString("x'") {
		t.Fatal("connection names")
	}
}

// TestFiltered: the databases a connection copies, from table patterns
// (escaped underscores) and from a filter set before them.
func TestFiltered(t *testing.T) {
	st := &ReplicaStatus{WildDoTable: `s1\_app.%,s1\_my\_db.%,bad.x,s1\_app.%`, DoDB: "s1_old,s1_app"}
	if got := strings.Join(st.Filtered(), " "); got != "s1_app s1_my_db s1_old" {
		t.Fatalf("filtered: %s", got)
	}
}
