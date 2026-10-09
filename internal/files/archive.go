package files

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// Limits bound archive extraction (zip bombs, inode exhaustion).
type Limits struct {
	MaxTotalBytes int64
	MaxFiles      int
}

var DefaultLimits = Limits{MaxTotalBytes: 10 << 30, MaxFiles: 100_000}

var ErrArchiveTooLarge = errors.New("archive exceeds extraction limits")

type ExtractResult struct {
	Files   int      `json:"files"`
	Dirs    int      `json:"dirs"`
	Bytes   int64    `json:"bytes"`
	Skipped []string `json:"skipped,omitempty"` // existing files (no overwrite) and unsupported entries
}

// Extract unpacks a .zip, .tar, .tar.gz or .tgz archive located in the site
// into destDir. Entry names are confined like any other path; symlinks,
// devices and other special entries are skipped; modes are normalised
// (0644/0755 for files, 0755 for directories). Existing files are kept
// unless overwrite is set.
func (s *Site) Extract(archive, destDir string, overwrite bool, lim Limits) (*ExtractResult, error) {
	ca, err := Clean(archive)
	if err != nil {
		return nil, err
	}
	cd, err := Clean(destDir)
	if err != nil {
		return nil, err
	}
	if fi, err := s.root.Stat(cd); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, ErrNotDir
	}
	f, err := s.root.Open(ca)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	x := &extractor{site: s, dest: cd, overwrite: overwrite, lim: lim, res: &ExtractResult{}}
	name := strings.ToLower(ca)
	switch {
	case strings.HasSuffix(name, ".zip"):
		err = x.zip(f, fi.Size())
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		gz, gerr := gzip.NewReader(f)
		if gerr != nil {
			return nil, gerr
		}
		err = x.tar(gz)
	case strings.HasSuffix(name, ".tar"):
		err = x.tar(f)
	default:
		return nil, errors.New("unsupported archive type (use .zip, .tar, .tar.gz or .tgz)")
	}
	return x.res, err
}

type extractor struct {
	site      *Site
	dest      string
	overwrite bool
	lim       Limits
	res       *ExtractResult
}

func (x *extractor) zip(r io.ReaderAt, size int64) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return err
	}
	if len(zr.File) > x.lim.MaxFiles {
		return ErrArchiveTooLarge
	}
	for _, zf := range zr.File {
		mode := zf.Mode()
		switch {
		case mode.IsDir():
			if err := x.dir(zf.Name); err != nil {
				return err
			}
		case mode.IsRegular():
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			err = x.file(zf.Name, mode, rc)
			rc.Close()
			if err != nil {
				return err
			}
		default:
			x.res.Skipped = append(x.res.Skipped, zf.Name)
		}
	}
	return nil
}

func (x *extractor) tar(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = x.dir(h.Name)
		case tar.TypeReg:
			err = x.file(h.Name, fs.FileMode(h.Mode), tr)
		case tar.TypeXGlobalHeader:
		default:
			x.res.Skipped = append(x.res.Skipped, h.Name)
		}
		if err != nil {
			return err
		}
	}
}

func (x *extractor) target(name string) (string, error) {
	c, err := Clean(name)
	if err != nil {
		return "", err
	}
	if c == "." {
		return "", nil
	}
	return path.Join(x.dest, c), nil
}

func (x *extractor) count() error {
	if x.res.Files+x.res.Dirs >= x.lim.MaxFiles {
		return ErrArchiveTooLarge
	}
	return nil
}

func (x *extractor) dir(name string) error {
	t, err := x.target(name)
	if err != nil || t == "" {
		return err
	}
	if err := x.count(); err != nil {
		return err
	}
	if err := x.site.mkdirAll(t, 0o755); err != nil {
		return err
	}
	x.res.Dirs++
	return nil
}

func (x *extractor) file(name string, mode fs.FileMode, r io.Reader) error {
	t, err := x.target(name)
	if err != nil || t == "" {
		return err
	}
	if err := x.count(); err != nil {
		return err
	}
	if fi, err := x.site.root.Lstat(t); err == nil {
		if !x.overwrite || !fi.Mode().IsRegular() {
			x.res.Skipped = append(x.res.Skipped, name)
			return nil
		}
	}
	if err := x.site.mkdirAll(path.Dir(t), 0o755); err != nil {
		return err
	}
	perm := fs.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	_, existed := x.site.root.Lstat(t)
	f, err := x.site.root.OpenFile(t, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if existed != nil {
		if err := x.site.chownFile(f, t); err != nil {
			f.Close()
			return err
		}
	}
	// Enforce the size budget on actual decompressed bytes, not on headers,
	// which an attacker controls.
	budget := x.lim.MaxTotalBytes - x.res.Bytes
	n, err := io.Copy(f, io.LimitReader(r, budget+1))
	cerr := f.Close()
	x.res.Bytes += n
	if n > budget {
		x.site.root.Remove(t)
		return ErrArchiveTooLarge
	}
	if err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	x.res.Files++
	return nil
}

// Compress writes the given paths (files or directories, recursively) into a
// new zip archive at dest. Symlinks are not followed.
func (s *Site) Compress(paths []string, dest string) error {
	cd, err := Clean(dest)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.ToLower(cd), ".zip") {
		return errors.New("destination must end in .zip")
	}
	if _, err := s.root.Lstat(cd); err == nil {
		return ErrExists
	}
	u, err := s.BeginUpload(cd)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(u.f)
	for _, p := range paths {
		c, err := Clean(p)
		if err != nil || c == "." {
			zw.Close()
			u.Abort()
			return fmt.Errorf("cannot archive %q", p)
		}
		base := path.Dir(c)
		err = fs.WalkDir(s.root.FS(), c, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == u.tmp {
				return nil
			}
			rel := strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
			if base == "." {
				rel = p
			}
			if d.IsDir() {
				_, err := zw.Create(rel + "/")
				return err
			}
			if !d.Type().IsRegular() {
				return nil // symlinks and special files are skipped
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			h, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			h.Name, h.Method = rel, zip.Deflate
			w, err := zw.CreateHeader(h)
			if err != nil {
				return err
			}
			f, err := s.root.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(w, f)
			return err
		})
		if err != nil {
			zw.Close()
			u.Abort()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		u.Abort()
		return err
	}
	return u.Commit()
}
