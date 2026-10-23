//go:build unix

package standby

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockDir takes the standby's lock, failing at once if it is held.
func lockDir(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, fileLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// matchOwner gives the files of dir to the owner of dir (a promotion run
// as root leaves them to the panel's user).
func matchOwner(dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid == 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Lchown(filepath.Join(dir, e.Name()), int(sys.Uid), int(sys.Gid)); err != nil {
			return err
		}
	}
	return nil
}
