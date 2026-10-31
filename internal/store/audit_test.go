package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAuditChain(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	db.SetAuditKey([]byte("audit key"))

	// Entries written before the chain existed: no hash.
	if _, err := db.W.Exec(`DROP TRIGGER audit_log_no_update`); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"legacy.one", "legacy.two"} {
		if _, err := db.W.Exec(`INSERT INTO audit_log (at, action) VALUES (1, ?)`, a); err != nil {
			t.Fatal(err)
		}
	}
	u, err := db.CreateUser(ctx, "a@example.com", "Ann", "x", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range []string{"app.created", "app.deploy_started", "site.created", "app.deleted"} {
		actor := &u.ID
		if i == 1 {
			actor = nil
		}
		if err := db.Audit(ctx, actor, a, "192.0.2.1", map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := db.VerifyAudit(ctx)
	if err != nil || c.Entries != 6 || c.Legacy != 2 || c.BrokenAt != 0 || c.LastID != 6 {
		t.Fatalf("intact: %+v %v", c, err)
	}

	// Listing: filters by action prefix, actor, system entries; paging.
	list, _ := db.AuditEntries(ctx, AuditQuery{Action: "app.", Limit: 10})
	if len(list) != 3 || list[0].Action != "app.deleted" || list[0].ActorName != "Ann" || list[0].Details["n"] != float64(3) {
		t.Fatalf("by action: %+v", list)
	}
	if list, _ := db.AuditEntries(ctx, AuditQuery{Action: "app_", Limit: 10}); len(list) != 0 {
		t.Fatal("_ in a prefix matched any character")
	}
	if list, _ := db.AuditEntries(ctx, AuditQuery{ActorID: u.ID, Limit: 10}); len(list) != 3 {
		t.Fatalf("by actor: %d", len(list))
	}
	if list, _ := db.AuditEntries(ctx, AuditQuery{System: true, Limit: 10}); len(list) != 3 {
		t.Fatalf("system: %d", len(list)) // two legacy entries and the deploy
	}
	page, _ := db.AuditEntries(ctx, AuditQuery{Before: 5, Limit: 2})
	if len(page) != 2 || page[0].ID != 4 || page[1].ID != 3 {
		t.Fatalf("page: %+v", page)
	}
	if acts, _ := db.AuditActions(ctx, 0); strings.Join(acts, " ") != "app.created app.deleted app.deploy_started legacy.one legacy.two site.created" {
		t.Fatal(acts)
	}

	// The log is append-only.
	if _, err := db.W.Exec(`DELETE FROM audit_log WHERE id = 3`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("delete: %v", err)
	}

	// Another key (another master key) does not verify the chain.
	db.SetAuditKey([]byte("other key"))
	if c, _ := db.VerifyAudit(ctx); c.BrokenAt != 3 {
		t.Fatalf("other key: %+v", c)
	}
	db.SetAuditKey([]byte("audit key"))

	// A changed entry breaks the chain at that entry.
	if _, err := db.W.Exec(`UPDATE audit_log SET details = '{"n":9}' WHERE id = 4`); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.VerifyAudit(ctx); c.BrokenAt != 4 || c.Entries != 6 {
		t.Fatalf("changed: %+v", c)
	}
	if _, err := db.W.Exec(`UPDATE audit_log SET details = '{"n":1}' WHERE id = 4`); err != nil {
		t.Fatal(err)
	}
	// Recomputing an entry's hash without the key does not help.
	if _, err := db.W.Exec(`UPDATE audit_log SET actor_id = NULL WHERE id = 5`); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.VerifyAudit(ctx); c.BrokenAt != 5 {
		t.Fatalf("actor changed: %+v", c)
	}
	db.W.Exec(`UPDATE audit_log SET actor_id = ? WHERE id = 5`, u.ID)
	// A removed entry breaks the chain at the next one.
	if _, err := db.W.Exec(`DROP TRIGGER audit_log_no_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.Exec(`DELETE FROM audit_log WHERE id = 4`); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.VerifyAudit(ctx); c.BrokenAt != 5 {
		t.Fatalf("removed: %+v", c)
	}
	// An entry stripped of its hash after the chain began is not legacy.
	if _, err := db.W.Exec(`UPDATE audit_log SET hash = NULL WHERE id = 6`); err != nil {
		t.Fatal(err)
	}
	db.W.Exec(`DELETE FROM audit_log WHERE id = 5`)
	if c, _ := db.VerifyAudit(ctx); c.BrokenAt != 6 || c.Legacy != 2 {
		t.Fatalf("no hash: %+v", c)
	}
}

func TestAuditPrune(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	db.SetAuditKey([]byte("audit key"))

	// Two legacy entries dated 1970, then signed ones written now.
	if _, err := db.W.Exec(`DROP TRIGGER audit_log_no_update`); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"legacy.one", "legacy.two"} {
		if _, err := db.W.Exec(`INSERT INTO audit_log (at, action) VALUES (0, ?)`, a); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 8 {
		if err := db.Audit(ctx, nil, "app.deployed", "", map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	// Nothing before the cutoff: nothing removed.
	if n, _, err := db.PruneAudit(ctx, time.Unix(0, 0)); err != nil || n != 0 {
		t.Fatalf("nothing: %d %v", n, err)
	}
	// Remove the legacy ones only.
	n, through, err := db.PruneAudit(ctx, time.Unix(1, 0))
	if err != nil || n != 2 || through != 2 {
		t.Fatalf("legacy: %d %d %v", n, through, err)
	}
	c, err := db.VerifyAudit(ctx)
	if err != nil || c.BrokenAt != 0 || c.Entries != 8 || c.Legacy != 0 || c.Pruned != 2 || c.FirstID != 3 {
		t.Fatalf("after legacy: %+v %v", c, err)
	}
	// Remove everything old, a few per transaction: the newest entry
	// always stays.
	auditPruneBatch = 3
	defer func() { auditPruneBatch = 10_000 }()
	n, through, err = db.PruneAudit(ctx, time.Now().Add(time.Hour))
	if err != nil || n != 7 || through != 9 {
		t.Fatalf("all: %d %d %v", n, through, err)
	}
	c, _ = db.VerifyAudit(ctx)
	if c.BrokenAt != 0 || c.Entries != 1 || c.Pruned != 9 || c.FirstID != 10 || c.LastID != 10 {
		t.Fatalf("after all: %+v", c)
	}
	// The log goes on from the kept entry.
	if err := db.Audit(ctx, nil, "audit.pruned", "", nil); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.VerifyAudit(ctx); c.BrokenAt != 0 || c.LastID != 11 {
		t.Fatalf("appended: %+v", c)
	}
	// Still append-only outside pruning.
	if _, err := db.W.Exec(`DELETE FROM audit_log WHERE id = 10`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("delete after prune: %v", err)
	}

	// Removing more from the start without the key: the base no longer
	// matches the first entry.
	if _, err := db.W.Exec(`DROP TRIGGER audit_log_no_delete`); err != nil {
		t.Fatal(err)
	}
	var h10 []byte
	db.W.QueryRow(`SELECT hash FROM audit_log WHERE id = 10`).Scan(&h10)
	if _, err := db.W.Exec(`DELETE FROM audit_log WHERE id = 10`); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.VerifyAudit(ctx); c.BrokenAt != 11 {
		t.Fatalf("head removed: %+v", c)
	}
	// Moving the base along without the key does not help.
	if _, err := db.W.Exec(`UPDATE audit_log_base SET first_id = 11, prev_hash = ?`, h10); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.VerifyAudit(ctx); c.BrokenAt != 11 {
		t.Fatalf("base moved: %+v", c)
	}
	// Nor does dropping the base.
	if _, err := db.W.Exec(`DELETE FROM audit_log_base`); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.VerifyAudit(ctx); c.BrokenAt != 11 {
		t.Fatalf("base dropped: %+v", c)
	}
}

func TestAuditRetentionSetting(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	if d, err := db.AuditRetention(ctx); err != nil || d != 0 {
		t.Fatalf("default: %d %v", d, err)
	}
	if err := db.SetAuditRetention(ctx, 365); err != nil {
		t.Fatal(err)
	}
	if d, _ := db.AuditRetention(ctx); d != 365 {
		t.Fatal(d)
	}
}
