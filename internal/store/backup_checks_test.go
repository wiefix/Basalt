package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackupChecks(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	seal := func(int64) ([]byte, []byte) { return nil, []byte("sealed") }
	a := &BackupStorage{Name: "a", Kind: "local", Config: StorageConfig{Path: "/backups/a"}}
	b := &BackupStorage{Name: "b", Kind: "local", Config: StorageConfig{Path: "/backups/b"}}
	for _, st := range []*BackupStorage{a, b} {
		if err := db.CreateBackupStorage(ctx, st, seal); err != nil {
			t.Fatal(err)
		}
	}
	s1, _ := db.CreateServer(ctx, "s1", []byte("t1"), time.Now().Add(time.Hour))
	s2, _ := db.CreateServer(ctx, "s2", []byte("t2"), time.Now().Add(time.Hour))
	site := func(srv *Server, name string) int64 {
		x, err := db.CreateSite(ctx, 0, srv.ID, name, []string{name + ".example.com"}, "auto", "")
		if err != nil {
			t.Fatal(err)
		}
		return x.ID
	}
	backup := func(siteID, serverID, storageID int64, ok bool) {
		bk := &Backup{SiteID: siteID, ServerID: serverID, StorageID: storageID, Kind: "backup", Trigger: "manual"}
		if err := db.CreateBackup(ctx, bk); err != nil {
			t.Fatal(err)
		}
		db.FinishBackup(ctx, bk.ID, ok, "", "", nil, 0, 0)
	}
	x, y := site(s1, "x"), site(s2, "y")
	backup(x, s1.ID, a.ID, true)
	backup(x, s1.ID, a.ID, true)
	backup(y, s2.ID, a.ID, false) // failed: nothing to check there
	backup(y, s2.ID, b.ID, true)
	pb := &PanelBackup{StorageID: b.ID, Trigger: "manual"}
	db.CreatePanelBackup(ctx, pb)
	db.FinishPanelBackup(ctx, pb.ID, true, "", "", "abc", 1)

	targets, err := db.BackupCheckTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// By storage name; the panel's repository first in its storage.
	if len(targets) != 3 || targets[0].StorageID != a.ID || targets[0].ServerID != s1.ID || targets[0].ServerName != "s1" ||
		targets[1].StorageID != b.ID || targets[1].ServerID != 0 || targets[2].ServerID != s2.ID || targets[0].Last != nil {
		t.Fatalf("targets: %+v", targets)
	}

	c := &BackupCheck{StorageID: a.ID, ServerID: s1.ID, Trigger: "schedule", ReadData: "1/52"}
	if err := db.CreateBackupCheck(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBackupCheck(ctx, &BackupCheck{StorageID: a.ID, ServerID: s1.ID, Trigger: "manual"}); !errors.Is(err, ErrCheckInProgress) {
		t.Fatalf("second running check: %v", err)
	}
	// The panel's repository and the same server's in another storage are others.
	p := &BackupCheck{StorageID: b.ID, Trigger: "manual"}
	if err := db.CreateBackupCheck(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateBackupCheck(ctx, &BackupCheck{StorageID: b.ID, Trigger: "manual"}); !errors.Is(err, ErrCheckInProgress) {
		t.Fatalf("second check of the panel's repository: %v", err)
	}
	if err := db.FinishBackupCheck(ctx, c.ID, false, "repository contains errors", "the log"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishBackupCheck(ctx, c.ID, true, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished twice: %v", err)
	}
	got, _ := db.BackupCheck(ctx, c.ID)
	if got.Status != "failed" || got.Log != "the log" || got.ReadData != "1/52" || got.FinishedAt == nil {
		t.Fatalf("check: %+v", got)
	}
	targets, _ = db.BackupCheckTargets(ctx)
	if l := targets[0].Last; l == nil || l.ID != c.ID || l.Log != "" || targets[1].Last.Status != "running" || targets[2].Last != nil {
		t.Fatalf("latest checks: %+v %+v", targets[0].Last, targets[1].Last)
	}
	if err := db.FailUnfinishedBackupChecks(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.BackupCheck(ctx, p.ID); got.Status != "failed" {
		t.Fatalf("unfinished check: %+v", got)
	}
	// Old checks are forgotten.
	db.W.Exec(`UPDATE backup_checks SET created_at = 1 WHERE id = ?`, c.ID)
	if err := db.DeleteBackupChecksBefore(ctx, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BackupCheck(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("old check kept")
	}
	// A deleted server's repository leaves the list.
	db.DeleteServer(ctx, s2.ID)
	if targets, _ = db.BackupCheckTargets(ctx); len(targets) != 2 {
		t.Fatalf("after deleting a server: %+v", targets)
	}
}
