package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLinksInside: a repository whose symbolic links stay inside it may be
// analysed by railpack; one with a link to this host's files may not.
func TestLinksInside(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src", "lib"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "lib", "a.js"), nil, 0o644)
	os.Symlink("lib/a.js", filepath.Join(dir, "src", "b.js"))
	os.Symlink("../README", filepath.Join(dir, "src", "readme"))
	os.Symlink(".", filepath.Join(dir, "self"))
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.Symlink("/etc/passwd", filepath.Join(dir, ".git", "ignored")) // git's own files are not analysed
	if err := linksInside(dir); err != nil {
		t.Fatalf("links inside: %v", err)
	}
	for name, target := range map[string]string{"abs": "/etc/passwd", "up": "../../etc/passwd", "sneaky": "src/../../x"} {
		d := t.TempDir()
		os.Symlink(target, filepath.Join(d, name))
		if err := linksInside(d); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s -> %s: %v", name, target, err)
		}
	}
}
