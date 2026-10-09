package files

import (
	"io/fs"
	"os"
	"path"
	"syscall"
)

// OpenOwned opens a site whose new files and directories are handed to uid,
// with the group of the directory they are created in (as a setgid
// directory would). Used when the agent runs as root and each site has its
// own system user; without it, files would be created as root.
func OpenOwned(dir string, uid int) (*Site, error) {
	s, err := Open(dir)
	if err != nil {
		return nil, err
	}
	s.uid = uid
	return s, nil
}

func (s *Site) owned() bool { return s.uid >= 0 }

// parentGID returns the group of the directory containing c.
func (s *Site) parentGID(c string) int {
	fi, err := s.root.Stat(path.Dir(c))
	if err != nil {
		return -1
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Gid)
	}
	return -1
}

// chownFile sets ownership on an open file descriptor (no path race).
func (s *Site) chownFile(f *os.File, c string) error {
	if !s.owned() {
		return nil
	}
	return f.Chown(s.uid, s.parentGID(c))
}

// chownPath sets ownership of a path just created. Lchown never follows a
// symlink swapped in meanwhile, and os.Root keeps it inside the site.
func (s *Site) chownPath(c string) error {
	if !s.owned() {
		return nil
	}
	return s.root.Lchown(c, s.uid, s.parentGID(c))
}

// mkdirAll creates c and any missing parents, owning each created directory.
func (s *Site) mkdirAll(c string, perm fs.FileMode) error {
	if c == "." {
		return nil
	}
	if fi, err := s.root.Stat(c); err == nil {
		if fi.IsDir() {
			return nil
		}
		return ErrNotDir
	}
	if err := s.mkdirAll(path.Dir(c), perm); err != nil {
		return err
	}
	if err := s.root.Mkdir(c, perm); err != nil {
		if fi, serr := s.root.Stat(c); serr == nil && fi.IsDir() {
			return nil // created concurrently
		}
		return err
	}
	return s.chownPath(c)
}
