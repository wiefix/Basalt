package sftpd

import (
	"io"
	"io/fs"
	"os"
	"path"

	"github.com/pkg/sftp"

	"github.com/wiefix/basalt/internal/files"
)

// handlers maps SFTP requests onto one site.
type handlers struct {
	site *files.Site
}

func (h *handlers) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	f, err := h.site.OpenFile(r.Filepath, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (h *handlers) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	return h.open(r)
}

// OpenFile handles read-write opens.
func (h *handlers) OpenFile(r *sftp.Request) (sftp.WriterAtReaderAt, error) {
	return h.open(r)
}

func (h *handlers) open(r *sftp.Request) (*os.File, error) {
	pf := r.Pflags()
	flag := 0
	switch {
	case pf.Read && pf.Write:
		flag = os.O_RDWR
	case pf.Write:
		flag = os.O_WRONLY
	default:
		flag = os.O_RDONLY
	}
	if pf.Creat {
		flag |= os.O_CREATE
	}
	if pf.Trunc {
		flag |= os.O_TRUNC
	}
	if pf.Excl {
		flag |= os.O_EXCL
	}
	if pf.Append {
		flag |= os.O_APPEND
	}
	perm := fs.FileMode(0o644)
	if r.AttrFlags().Permissions {
		if m := r.Attributes().FileMode().Perm(); m != 0 {
			perm = m
		}
	}
	return h.site.OpenFile(r.Filepath, flag, perm)
}

func (h *handlers) Filecmd(r *sftp.Request) error {
	switch r.Method {
	case "Setstat":
		return h.setstat(r)
	case "Rename":
		return h.site.Rename(r.Filepath, r.Target)
	case "Rmdir", "Remove":
		return h.site.Remove(r.Filepath)
	case "Mkdir":
		return h.site.Mkdir(r.Filepath)
	case "Link", "Symlink":
		// Links could later be followed by processes that do not use os.Root.
		return sftp.ErrSSHFxPermissionDenied
	}
	return sftp.ErrSSHFxOpUnsupported
}

// PosixRename replaces the destination, as OpenSSH's posix-rename does.
func (h *handlers) PosixRename(r *sftp.Request) error {
	return h.site.RenameReplace(r.Filepath, r.Target)
}

func (h *handlers) setstat(r *sftp.Request) error {
	af := r.AttrFlags()
	attrs := r.Attributes()
	if af.UidGid {
		return sftp.ErrSSHFxPermissionDenied
	}
	if af.Size {
		if err := h.site.Truncate(r.Filepath, int64(attrs.Size)); err != nil {
			return err
		}
	}
	if af.Permissions {
		if err := h.site.Chmod(r.Filepath, uint32(attrs.FileMode().Perm())); err != nil {
			return err
		}
	}
	if af.Acmodtime {
		if err := h.site.Chtimes(r.Filepath, attrs.AccessTime(), attrs.ModTime()); err != nil {
			return err
		}
	}
	return nil
}

type listAt []fs.FileInfo

func (l listAt) ListAt(dst []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(dst, l[off:])
	if n < len(dst) {
		return n, io.EOF
	}
	return n, nil
}

func (h *handlers) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		infos, err := h.site.ReadDirInfo(r.Filepath)
		if err != nil {
			return nil, err
		}
		return listAt(infos), nil
	case "Stat":
		fi, err := h.site.StatInfo(r.Filepath)
		if err != nil {
			return nil, err
		}
		return listAt{named(fi, r.Filepath)}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

func (h *handlers) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	fi, err := h.site.LstatInfo(r.Filepath)
	if err != nil {
		return nil, err
	}
	return listAt{named(fi, r.Filepath)}, nil
}

func (h *handlers) Readlink(p string) (string, error) {
	return h.site.Readlink(p)
}

// RealPath presents the site as the filesystem root.
func (h *handlers) RealPath(p string) (string, error) {
	c, err := files.Clean(p)
	if err != nil {
		return "", err
	}
	if c == "." {
		return "/", nil
	}
	return "/" + c, nil
}

// named gives the site root the name "/" instead of its host directory name,
// which would otherwise leak the site's numeric id and layout.
func named(fi fs.FileInfo, p string) fs.FileInfo {
	return renamed{fi, path.Base(path.Clean("/" + p))}
}

type renamed struct {
	fs.FileInfo
	name string
}

func (r renamed) Name() string { return r.name }

var (
	_ sftp.OpenFileWriter       = (*handlers)(nil)
	_ sftp.PosixRenameFileCmder = (*handlers)(nil)
	_ sftp.LstatFileLister      = (*handlers)(nil)
	_ sftp.ReadlinkFileLister   = (*handlers)(nil)
	_ sftp.RealPathFileLister   = (*handlers)(nil)
)
