package agent

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// TestRepoLock: one job per repository at a time; others wait and say so.
func TestRepoLock(t *testing.T) {
	b := &backupManager{locks: map[string]*sync.Mutex{}}
	repo := agentproto.BackupRepo{Kind: agentproto.StorageLocal, Path: "/backups/server-1"}
	other := agentproto.BackupRepo{Kind: agentproto.StorageLocal, Path: "/backups/server-2"}
	var first deployLog
	unlock := b.lockRepo(repo, &first)
	// Another repository is free.
	var free deployLog
	b.lockRepo(other, &free)()
	got := make(chan string)
	go func() {
		var second deployLog
		b.lockRepo(repo, &second)()
		got <- second.String()
	}()
	select {
	case <-got:
		t.Fatal("second job did not wait")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if s := <-got; !strings.Contains(s, "Waiting for the repository") {
		t.Fatalf("second job log %q", s)
	}
	if first.String() != "" || free.String() != "" {
		t.Fatalf("logged without waiting: %q %q", first.String(), free.String())
	}
}

// TestBackupCommandFails: a dump that fails part way leaves no snapshot,
// and its error output says why.
func TestBackupCommandFails(t *testing.T) {
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	b := testBackups(t, bin, dir)
	repo, _ := b.repo(agentproto.BackupRepo{Kind: agentproto.StorageLocal, Path: filepath.Join(dir, "repo"), Password: "pw"})
	r := b.runner(nil)
	if _, err := r.Init(ctx, repo); err != nil {
		t.Fatal(err)
	}
	tags := []string{"res=site-1", "part=mariadb-shop"}
	_, err = r.BackupCommand(ctx, repo, tags, "mariadb-shop.sql",
		exec.Command("sh", "-c", "echo 'CREATE TABLE'; sleep 1; echo 'lost connection' >&2; exit 2"))
	if err == nil || !strings.Contains(err.Error(), "lost connection") {
		t.Fatalf("failed dump: %v", err)
	}
	if list, err := r.Snapshots(ctx, repo, tags); err != nil || len(list) != 0 {
		t.Fatalf("a failed dump left %d snapshots (%v)", len(list), err)
	}
	s, err := r.BackupCommand(ctx, repo, tags, "mariadb-shop.sql", exec.Command("echo", "CREATE TABLE t;"))
	if err != nil || s.SnapshotID == "" {
		t.Fatalf("dump: %+v %v", s, err)
	}
}
