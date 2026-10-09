package files

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// Lower-level operations used by protocol servers (SFTP). They keep the same
// confinement as everything else in this package.

// OpenFile opens a regular file. Directories cannot be opened for writing
// and the site root is never a valid target.
func (s *Site) OpenFile(p string, flag int, perm fs.FileMode) (*os.File, error) {
	c, err := Clean(p)
	if err != nil {
		return nil, err
	}
	if c == "." {
		return nil, ErrIsDir
	}
	if perm == 0 || perm&^0o777 != 0 {
		perm = 0o644
	}
	_, existed := s.root.Lstat(c)
	f, err := s.root.OpenFile(c, flag, perm)
	if err != nil {
		return nil, err
	}
	if existed != nil && flag&os.O_CREATE != 0 {
		if err := s.chownFile(f, c); err != nil {
			f.Close()
			return nil, err
		}
	}
	if fi, err := f.Stat(); err == nil && fi.IsDir() && flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		f.Close()
		return nil, ErrIsDir
	}
	return f, nil
}

func (s *Site) LstatInfo(p string) (fs.FileInfo, error) {
	c, err := Clean(p)
	if err != nil {
		return nil, err
	}
	return s.root.Lstat(c)
}

func (s *Site) StatInfo(p string) (fs.FileInfo, error) {
	c, err := Clean(p)
	if err != nil {
		return nil, err
	}
	return s.root.Stat(c)
}

// ReadDirInfo lists a directory as FileInfos, hiding upload temporaries.
func (s *Site) ReadDirInfo(p string) ([]fs.FileInfo, error) {
	c, err := Clean(p)
	if err != nil {
		return nil, err
	}
	d, err := s.root.Open(c)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	des, err := d.ReadDir(MaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	out := make([]fs.FileInfo, 0, len(des))
	for _, de := range des {
		if strings.HasPrefix(de.Name(), uploadPrefix) {
			continue
		}
		if fi, err := de.Info(); err == nil {
			out = append(out, fi)
		}
	}
	return out, nil
}

func (s *Site) Readlink(p string) (string, error) {
	c, err := Clean(p)
	if err != nil {
		return "", err
	}
	return s.root.Readlink(c)
}

// Remove deletes a file or an empty directory (not recursive).
func (s *Site) Remove(p string) error {
	c, err := Clean(p)
	if err != nil {
		return err
	}
	if c == "." {
		return ErrRootTouched
	}
	return s.root.Remove(c)
}

// RenameReplace renames, replacing an existing destination file (POSIX rename).
func (s *Site) RenameReplace(from, to string) error {
	cf, err := Clean(from)
	if err != nil {
		return err
	}
	ct, err := Clean(to)
	if err != nil {
		return err
	}
	if cf == "." || ct == "." {
		return ErrRootTouched
	}
	return s.root.Rename(cf, ct)
}

func (s *Site) Truncate(p string, size int64) error {
	f, err := s.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

func (s *Site) Chtimes(p string, atime, mtime time.Time) error {
	c, err := Clean(p)
	if err != nil {
		return err
	}
	return s.root.Chtimes(c, atime, mtime)
}
