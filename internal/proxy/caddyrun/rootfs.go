// Package caddyrun runs the embedded Caddy server behind `basalt proxy` and
// provides Basalt's own Caddy modules.
package caddyrun

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2"
)

func init() {
	caddy.RegisterModule(RootFS{})
}

// RootFS is a Caddy file system confined to one directory. Unlike Caddy's
// default file system it refuses to follow symlinks or ".." out of the site
// root (via os.Root), so a user who uploads a symlink cannot make the proxy
// serve another site's files or system files.
type RootFS struct {
	Root string `json:"root"`
	// StatUnreadable makes Open return a stat-only file for an existing file
	// the proxy may not read. Caddy keeps file systems behind a wrapper with
	// only Open, so its file matcher checks existence by opening; without this
	// a PHP script readable only by the site user (0600) counts as missing and
	// the request falls through to index.php. Matchers only: file_server keeps
	// a strict file system and answers 403 for unreadable files.
	StatUnreadable bool `json:"stat_unreadable,omitempty"`

	state *rootState
}

type rootState struct {
	mu   sync.Mutex
	root *os.Root
}

func (RootFS) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.fs.basalt_root",
		New: func() caddy.Module { return new(RootFS) },
	}
}

// Provision does not fail if the directory is missing: one absent site must
// not prevent the whole configuration from loading. Opening is retried lazily.
func (f *RootFS) Provision(caddy.Context) error {
	if f.Root == "" {
		return errors.New("basalt_root: root is required")
	}
	f.state = &rootState{}
	f.open()
	return nil
}

func (f *RootFS) Cleanup() error {
	if f.state == nil {
		return nil
	}
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	if f.state.root != nil {
		err := f.state.root.Close()
		f.state.root = nil
		return err
	}
	return nil
}

func (f *RootFS) open() (*os.Root, error) {
	if f.state == nil {
		return nil, errors.New("basalt_root: not provisioned")
	}
	f.state.mu.Lock()
	defer f.state.mu.Unlock()
	if f.state.root != nil {
		return f.state.root, nil
	}
	r, err := os.OpenRoot(f.Root)
	if err != nil {
		return nil, err
	}
	f.state.root = r
	return r, nil
}

// clean maps the names Caddy passes ("./a/b", "/a/b", ".") to fs.ValidPath form.
func clean(name string) (string, error) {
	name = path.Clean("/" + strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		name = "."
	}
	if !fs.ValidPath(name) {
		return "", fs.ErrInvalid
	}
	return name, nil
}

func (f *RootFS) Open(name string) (fs.File, error) {
	n, err := clean(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	r, err := f.open()
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	file, err := r.FS().Open(n)
	if err != nil && f.StatUnreadable && errors.Is(err, fs.ErrPermission) {
		if info, serr := r.Stat(n); serr == nil {
			return statOnlyFile{info: info, name: n}, nil
		}
	}
	return file, err
}

// statOnlyFile is an existing file the proxy may not read: only its metadata
// is available.
type statOnlyFile struct {
	info fs.FileInfo
	name string
}

func (f statOnlyFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f statOnlyFile) Close() error               { return nil }
func (f statOnlyFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: f.name, Err: fs.ErrPermission}
}
func (f statOnlyFile) ReadDir(int) ([]fs.DirEntry, error) {
	return nil, &fs.PathError{Op: "readdir", Path: f.name, Err: fs.ErrPermission}
}

func (f *RootFS) Stat(name string) (fs.FileInfo, error) {
	n, err := clean(name)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	r, err := f.open()
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return fs.Stat(r.FS(), n)
}

var (
	_ fs.StatFS          = (*RootFS)(nil)
	_ fs.ReadDirFile     = statOnlyFile{}
	_ caddy.Provisioner  = (*RootFS)(nil)
	_ caddy.CleanerUpper = (*RootFS)(nil)
)
