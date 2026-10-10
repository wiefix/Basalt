package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/wiefix/basalt/internal/agenthub"
	"github.com/wiefix/basalt/internal/secrets"
	"github.com/wiefix/basalt/internal/server"
	"github.com/wiefix/basalt/internal/store"
)

// runRecover turns a restored panel backup back into a panel: it decrypts
// the master key with the recovery passphrase and puts the key and the
// database into the data directory.
func runRecover(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("basalt server recover", flag.ContinueOnError)
	fl.SetOutput(stderr)
	dataDir := fl.String("data-dir", "/var/lib/basalt/server", "the panel's data directory")
	from := fl.String("from", "", "directory restored from a panel backup (searched for "+server.PanelBackupKey+")")
	force := fl.Bool("force", false, "replace an existing database (kept as .bak-<time>)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return errors.New("--from is required: the directory a panel backup was restored to")
	}
	src, err := findBackup(*from)
	if err != nil {
		return err
	}
	wrapped, err := os.ReadFile(filepath.Join(src, server.PanelBackupKey))
	if err != nil {
		return err
	}
	pass, err := passphrase(stdin, stderr)
	if err != nil {
		return err
	}
	key, err := secrets.UnwrapKey(wrapped, pass)
	if err != nil {
		return err
	}

	dbPath, keyPath := filepath.Join(*dataDir, "basalt.db"), filepath.Join(*dataDir, "master.key")
	if _, err := os.Stat(dbPath); err == nil {
		if !*force {
			return fmt.Errorf("%s already exists; stop the panel and use --force to replace it (the current files are kept)", dbPath)
		}
		suffix := ".bak-" + time.Now().UTC().Format("20060102-150405")
		for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", keyPath} {
			if err := os.Rename(p, p+suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		fmt.Fprintf(stdout, "Kept the previous files with suffix %s\n", suffix)
	}
	info, err := os.Stat(*dataDir)
	if err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(src, server.PanelBackupDB))
	if err != nil {
		return err
	}
	for path, content := range map[string][]byte{dbPath: data, keyPath: key} {
		if err := writePrivate(path, content, info); err != nil {
			return err
		}
	}

	// Check the result: the database opens and the key decrypts its secrets.
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open the recovered database: %w", err)
	}
	defer db.Close()
	box, err := secrets.NewBox(key)
	if err != nil {
		return err
	}
	instanceID, err := db.InstanceID(ctx)
	if err != nil {
		return err
	}
	if _, err := agenthub.LoadOrCreateCA(ctx, db, box, instanceID, time.Now()); err != nil {
		return fmt.Errorf("the key does not open the database's secrets: %w", err)
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		chownLike(p, info) // files the check created
	}
	servers, _ := db.ListServers(ctx)
	fmt.Fprintf(stdout, "Recovered the panel (%d servers) into %s. Start it; agents reconnect by themselves.\n", len(servers), *dataDir)
	return nil
}

// findBackup returns the directory under root that holds a panel backup.
func findBackup(root string) (string, error) {
	var found []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == server.PanelBackupKey {
			if _, err := os.Stat(filepath.Join(filepath.Dir(p), server.PanelBackupDB)); err == nil {
				found = append(found, filepath.Dir(p))
			}
		}
		return nil
	})
	switch {
	case err != nil:
		return "", err
	case len(found) == 0:
		return "", fmt.Errorf("no panel backup (%s and %s) under %s", server.PanelBackupDB, server.PanelBackupKey, root)
	case len(found) > 1:
		return "", fmt.Errorf("several panel backups under %s: %s", root, strings.Join(found, ", "))
	}
	return found[0], nil
}

// passphrase reads the recovery passphrase: from BASALT_RECOVERY_PASSPHRASE,
// from the terminal without echo, or a line of standard input.
func passphrase(stdin *os.File, stderr io.Writer) (string, error) {
	if v := os.Getenv("BASALT_RECOVERY_PASSPHRASE"); v != "" {
		return v, nil
	}
	if term.IsTerminal(int(stdin.Fd())) {
		fmt.Fprint(stderr, "Recovery passphrase: ")
		b, err := term.ReadPassword(int(stdin.Fd()))
		fmt.Fprintln(stderr)
		return string(b), err
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no recovery passphrase on standard input")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// writePrivate writes a 0600 file owned like dir.
func writePrivate(path string, data []byte, dir fs.FileInfo) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	chownLike(tmp, dir)
	return os.Rename(tmp, path)
}

// chownLike gives path the owner of dir (the panel runs as its own user;
// recovery runs as root).
func chownLike(path string, dir fs.FileInfo) {
	if st, ok := dir.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		os.Lchown(path, int(st.Uid), int(st.Gid))
	}
}
