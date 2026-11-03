package privhelper

import (
	"bytes"
	"errors"

	"golang.org/x/sys/unix"
)

// exchange swaps folders a and b at once: the site is never without its
// folder. A file system that cannot swaps them through spare.
func exchange(a, b, spare string) error {
	err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return renameSwap(a, b, spare)
	}
	return err
}

// dropXattrs removes every extended attribute of a file, not following a
// symbolic link.
func dropXattrs(path string) error {
	buf := make([]byte, 4096)
	n, err := unix.Llistxattr(path, buf)
	if errors.Is(err, unix.ERANGE) {
		if n, err = unix.Llistxattr(path, nil); err == nil {
			buf = make([]byte, n)
			n, err = unix.Llistxattr(path, buf)
		}
	}
	if errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	if err != nil {
		return err
	}
	for name := range bytes.SplitSeq(buf[:n], []byte{0}) {
		if len(name) == 0 {
			continue
		}
		if err := unix.Lremovexattr(path, string(name)); err != nil && !errors.Is(err, unix.ENODATA) {
			return err
		}
	}
	return nil
}
