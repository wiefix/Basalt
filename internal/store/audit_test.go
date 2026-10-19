package store

import (
	"context"
	"strings"
	"testing"
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
