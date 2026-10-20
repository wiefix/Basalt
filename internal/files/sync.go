package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// Copying a site's files to another server: Walk lists what a copy needs,
// SyncWrite builds a file next to its place and puts it there whole.

// WalkEntry is a file or folder found by Walk.
type WalkEntry struct {
	Path  string
	Dir   bool
	Size  int64
	MTime time.Time
	Mode  fs.FileMode // permission bits
}

// Walk lists the site's folders and regular files, parents before their
// children, leaving out symlinks, special files, upload temporaries and
// whatever skip returns true for (a skipped folder with its contents).
func (s *Site) Walk(skip func(p string, dir bool) bool, fn func(WalkEntry) error) error {
	return fs.WalkDir(s.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), uploadPrefix) {
			return nil
		}
		if skip != nil && skip(p, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed meanwhile
			}
			return err
		}
		return fn(WalkEntry{Path: p, Dir: d.IsDir(), Size: fi.Size(), MTime: fi.ModTime(), Mode: fi.Mode().Perm()})
	})
}

// syncTemp is where SyncWrite builds the file c: next to it, hidden like an
// upload, the same name for every try so a new one starts it over.
func syncTemp(c string) string {
	h := sha256.Sum256([]byte(c))
	return path.Join(path.Dir(c), uploadPrefix+"sync-"+hex.EncodeToString(h[:8]))
}

// SyncWrite writes data at off of a copy of the file p (off 0 starts it
// over); final puts the copy in place with mode and mtime, replacing a file
// there.
func (s *Site) SyncWrite(p string, off int64, data []byte, final bool, mode fs.FileMode, mtime time.Time) error {
	c, err := Clean(p)
	if err != nil {
		return err
	}
	if c == "." {
		return ErrRootTouched
	}
	tmp := syncTemp(c)
	var f *os.File
	if off == 0 {
		s.root.Remove(tmp)
		f, err = s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if err = s.chownFile(f, tmp); err != nil {
				f.Close()
				s.root.Remove(tmp)
				return err
			}
		}
	} else {
		f, err = s.root.OpenFile(tmp, os.O_WRONLY, 0)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	fi, err := f.Stat()
	if err == nil && fi.Size() != off {
		err = fmt.Errorf("%s: write at %d, the copy has %d bytes", p, off, fi.Size())
	}
	if err == nil && len(data) > 0 {
		_, err = f.WriteAt(data, off)
	}
	if err == nil && final {
		err = f.Chmod(mode.Perm())
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		s.root.Remove(tmp)
		return err
	}
	if !final {
		return nil
	}
	if err := s.root.Chtimes(tmp, mtime, mtime); err != nil {
		s.root.Remove(tmp)
		return err
	}
	if fi, err := s.root.Lstat(c); err == nil && fi.IsDir() {
		s.root.Remove(tmp)
		return fmt.Errorf("%s: %w", p, ErrIsDir)
	}
	if err := s.root.Rename(tmp, c); err != nil {
		s.root.Remove(tmp)
		return err
	}
	return nil
}

// SyncMkdir creates the folder p (and missing parents) with mode, or sets
// the mode of the folder there.
func (s *Site) SyncMkdir(p string, mode fs.FileMode) error {
	c, err := Clean(p)
	if err != nil {
		return err
	}
	if c == "." {
		return ErrRootTouched
	}
	if err := s.mkdirAll(c, 0o755); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return s.SyncChmod(p, mode)
}

// SyncChmod sets the permission bits of p (a file or folder, never a
// symlink's target).
func (s *Site) SyncChmod(p string, mode fs.FileMode) error {
	c, err := Clean(p)
	if err != nil {
		return err
	}
	fi, err := s.root.Lstat(c)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", p)
	}
	// The setgid bit of the site's folders (their group) stays.
	return s.root.Chmod(c, fi.Mode()&fs.ModeSetgid|mode.Perm())
}

// SyncRead reads n bytes of the regular file p at off, with its size and
// mtime; missing reports that p is gone or not a regular file.
func (s *Site) SyncRead(p string, off int64, n int) (data []byte, size int64, mtime time.Time, missing bool, err error) {
	c, err := Clean(p)
	if err != nil {
		return nil, 0, time.Time{}, false, err
	}
	fi, err := s.root.Lstat(c)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.Mode().IsRegular()) {
		return nil, 0, time.Time{}, true, nil
	}
	if err != nil {
		return nil, 0, time.Time{}, false, err
	}
	f, err := s.root.Open(c)
	if err != nil {
		return nil, 0, time.Time{}, false, err
	}
	defer f.Close()
	fi, err = f.Stat()
	if err != nil {
		return nil, 0, time.Time{}, false, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, time.Time{}, true, nil
	}
	if off > fi.Size() {
		off = fi.Size()
	}
	buf := make([]byte, min(int64(n), fi.Size()-off))
	k, err := f.ReadAt(buf, off)
	if k == len(buf) {
		err = nil
	}
	return buf[:k], fi.Size(), fi.ModTime(), false, err
}
