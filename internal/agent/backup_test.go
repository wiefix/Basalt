package agent

import (
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
