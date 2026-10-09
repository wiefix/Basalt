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
	return r.FS().Open(n)
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
	_ caddy.Provisioner  = (*RootFS)(nil)
	_ caddy.CleanerUpper = (*RootFS)(nil)
)
