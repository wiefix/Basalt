// Package files implements file-manager operations inside one site's
// directory. Every operation goes through os.Root, so no path, ".." or
// symlink can reach anything outside the site.
package files

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

const (
	// MaxChunk bounds a single read or write over the agent channel.
	MaxChunk = 1 << 20
	// MaxEntries bounds a directory listing taken whole (List).
	MaxEntries = 10000
	// MaxDirEntries bounds the directories ListPage pages through: all
	// names are read and sorted to page them, so memory is the limit.
	MaxDirEntries = 200000
	// DefaultPage and MaxPage bound the entries of one ListPage.
	DefaultPage = 500
	MaxPage     = 2000

	uploadPrefix = ".basalt-upload-"
)

var (
	ErrNotDir      = errors.New("not a directory")
	ErrIsDir       = errors.New("is a directory")
	ErrRootTouched = errors.New("the site root itself cannot be changed")
	ErrBadMode     = errors.New("mode must be between 0000 and 0777")
	ErrExists      = errors.New("already exists")
)

// Clean turns user input ("/a/b", "a/../b", "./x") into an os.Root-relative
// path. The result never starts with "/" or "..".
func Clean(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("invalid path %q", p)
	}
	c := strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(p, "\\", "/")), "/")
	if c == "" {
		return ".", nil
	}
	if len(c) > 4096 {
		return "", errors.New("path too long")
	}
	return c, nil
}

type Entry struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"` // file, dir, symlink, other
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"` // octal permission bits, e.g. "0644"
	ModTime time.Time `json:"mod_time"`
	Target  string    `json:"target,omitempty"` // symlink target as stored
}

func entryFrom(name string, fi fs.FileInfo) Entry {
	e := Entry{
		Name:    name,
		Size:    fi.Size(),
		Mode:    fmt.Sprintf("%04o", fi.Mode().Perm()),
		ModTime: fi.ModTime().UTC(),
	}
	switch {
	case fi.Mode().IsRegular():
		e.Type = "file"
	case fi.IsDir():
		e.Type, e.Size = "dir", 0
	case fi.Mode()&fs.ModeSymlink != 0:
		e.Type = "symlink"
	default:
		e.Type = "other"
	}
	return e
}

// Site is one site's directory.
type Site struct {
	root *os.Root
	uid  int // owner for created entries; -1 keeps the caller's identity
}

func Open(dir string) (*Site, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Site{root: r, uid: -1}, nil
}

func (s *Site) Close() error { return s.root.Close() }

// List returns all entries of a small directory in ListPage's order;
// more than MaxEntries is an error.
func (s *Site) List(p string) ([]Entry, error) {
	pg, err := s.ListPage(p, "", MaxEntries)
	if err != nil {
		return nil, err
	}
	if pg.Total > MaxEntries {
		return nil, fmt.Errorf("directory has more than %d entries", MaxEntries)
	}
	return pg.Entries, nil
}

// Page is one page of a directory listing. Next is the cursor of the
// following page, empty on the last one.
type Page struct {
	Entries []Entry
	Total   int
	Next    string
}

// dirKey orders a listing: directories first, then by name ignoring case,
// then by exact name, so the order is total and a cursor is unambiguous.
type dirKey struct {
	dir   bool
	lower string
	name  string
}

func (a dirKey) compare(b dirKey) int {
	if a.dir != b.dir {
		if a.dir {
			return -1
		}
		return 1
	}
	if c := strings.Compare(a.lower, b.lower); c != 0 {
		return c
	}
	return strings.Compare(a.name, b.name)
}

// cursor is "d/" or "f/" and the name: the sort key of the last entry
// shown. Entries added or removed between pages do not shift the next one.
func (k dirKey) cursor() string {
	if k.dir {
		return "d/" + k.name
	}
	return "f/" + k.name
}

func parseCursor(c string) (dirKey, error) {
	kind, name, ok := strings.Cut(c, "/")
	if !ok || (kind != "d" && kind != "f") || name == "" {
		return dirKey{}, errors.New("invalid cursor")
	}
	return dirKey{dir: kind == "d", lower: strings.ToLower(name), name: name}, nil
}

// ListPage returns up to limit entries of a directory that follow cursor
// ("" for the first page), directories first, then by name, and the
// number of entries in it. Only the page's entries are stat'ed. Upload
// temporaries are hidden.
func (s *Site) ListPage(p, cursor string, limit int) (Page, error) {
	if limit <= 0 {
		limit = DefaultPage
	}
	limit = min(limit, MaxEntries)
	var after *dirKey
	if cursor != "" {
		k, err := parseCursor(cursor)
		if err != nil {
			return Page{}, err
		}
		after = &k
	}
	c, err := Clean(p)
	if err != nil {
		return Page{}, err
	}
	d, err := s.root.Open(c)
	if err != nil {
		return Page{}, err
	}
	defer d.Close()
	fi, err := d.Stat()
	if err != nil {
		return Page{}, err
	}
	if !fi.IsDir() {
		return Page{}, ErrNotDir
	}
	des, err := d.ReadDir(MaxDirEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return Page{}, err
	}
	if len(des) > MaxDirEntries {
		return Page{}, fmt.Errorf("directory has more than %d entries; use SFTP", MaxDirEntries)
	}
	type item struct {
		key dirKey
		de  fs.DirEntry
	}
	items := make([]item, 0, len(des))
	for _, de := range des {
		if strings.HasPrefix(de.Name(), uploadPrefix) {
			continue
		}
		items = append(items, item{dirKey{de.IsDir(), strings.ToLower(de.Name()), de.Name()}, de})
	}
	slices.SortFunc(items, func(a, b item) int { return a.key.compare(b.key) })
	i := 0
	if after != nil {
		i, _ = slices.BinarySearchFunc(items, *after, func(it item, k dirKey) int { return it.key.compare(k) })
		if i < len(items) && items[i].key == *after {
			i++
		}
	}
	pg := Page{Entries: make([]Entry, 0, min(limit, len(items)-i)), Total: len(items)}
	for ; i < len(items) && len(pg.Entries) < limit; i++ {
		fi, err := items[i].de.Info()
		if err != nil {
			continue // removed meanwhile
		}
		e := entryFrom(items[i].key.name, fi)
		if e.Type == "symlink" {
			e.Target, _ = s.root.Readlink(path.Join(c, e.Name))
		}
		pg.Entries = append(pg.Entries, e)
	}
	if i < len(items) {
		pg.Next = items[i-1].key.cursor()
	}
	return pg, nil
}

func (s *Site) Stat(p string) (Entry, error) {
	c, err := Clean(p)
	if err != nil {
		return Entry{}, err
	}
	fi, err := s.root.Lstat(c)
	if err != nil {
		return Entry{}, err
	}
	return entryFrom(path.Base(c), fi), nil
}

// Read returns up to n bytes of a regular file starting at off.
func (s *Site) Read(p string, off int64, n int) (data []byte, size int64, err error) {
	c, err := Clean(p)
	if err != nil {
		return nil, 0, err
	}
	if n <= 0 || n > MaxChunk {
		n = MaxChunk
	}
	f, err := s.root.Open(c)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if fi.IsDir() {
		return nil, 0, ErrIsDir
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", p)
	}
	if off < 0 || off > fi.Size() {
		return nil, 0, fmt.Errorf("offset %d out of range", off)
	}
	buf := make([]byte, min(int64(n), fi.Size()-off))
	k, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	return buf[:k], fi.Size(), nil
}

// WriteFile atomically replaces (or creates) a small file, e.g. from the
// editor. The parent directory must exist.
func (s *Site) WriteFile(p string, data []byte) error {
	if len(data) > MaxChunk {
		return fmt.Errorf("file too large for a single write; use an upload")
	}
	u, err := s.BeginUpload(p)
	if err != nil {
		return err
	}
	if err := u.Write(0, data); err != nil {
		u.Abort()
		return err
	}
	return u.Commit()
}

func (s *Site) Mkdir(p string) error {
	c, err := Clean(p)
	if err != nil {
		return err
	}
	if c == "." {
		return ErrExists
	}
	if err := s.root.Mkdir(c, 0o755); errors.Is(err, fs.ErrExist) {
		return ErrExists
	} else if err != nil {
		return err
	}
	return s.chownPath(c)
}

// Rename moves from to to. The destination must not exist.
func (s *Site) Rename(from, to string) error {
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
	if strings.HasPrefix(ct+"/", cf+"/") {
		return errors.New("cannot move a directory into itself")
	}
	if _, err := s.root.Lstat(ct); err == nil {
		return ErrExists
	}
	return s.root.Rename(cf, ct)
}

// Delete removes files or directories recursively. Symlinks are removed
// themselves, never their targets.
func (s *Site) Delete(p string) error {
	c, err := Clean(p)
	if err != nil {
		return err
	}
	if c == "." {
		return ErrRootTouched
	}
	if _, err := s.root.Lstat(c); err != nil {
		return err
	}
	return s.root.RemoveAll(c)
}

// Chmod sets permission bits. setuid, setgid and sticky bits are refused.
func (s *Site) Chmod(p string, mode uint32) error {
	if mode > 0o777 {
		return ErrBadMode
	}
	c, err := Clean(p)
	if err != nil {
		return err
	}
	fi, err := s.root.Lstat(c)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return errors.New("cannot change permissions of a symlink")
	}
	return s.root.Chmod(c, fs.FileMode(mode))
}

// Upload writes a file through a temporary in the destination directory and
// renames it into place on Commit, so readers never see a partial file.
type Upload struct {
	site  *Site
	f     *os.File
	tmp   string
	final string
	size  int64
}

func (s *Site) BeginUpload(p string) (*Upload, error) {
	c, err := Clean(p)
	if err != nil {
		return nil, err
	}
	if c == "." {
		return nil, ErrRootTouched
	}
	if fi, err := s.root.Lstat(c); err == nil && fi.IsDir() {
		return nil, ErrIsDir
	}
	dir := path.Dir(c)
	if fi, err := s.root.Stat(dir); err != nil {
		return nil, fmt.Errorf("parent directory: %w", err)
	} else if !fi.IsDir() {
		return nil, ErrNotDir
	}
	var rnd [8]byte
	rand.Read(rnd[:])
	tmp := path.Join(dir, uploadPrefix+hex.EncodeToString(rnd[:]))
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	if err := s.chownFile(f, tmp); err != nil {
		f.Close()
		s.root.Remove(tmp)
		return nil, err
	}
	return &Upload{site: s, f: f, tmp: tmp, final: c}, nil
}

// Write appends data; off must equal the bytes written so far, which makes
// retried or reordered chunks detectable.
func (u *Upload) Write(off int64, data []byte) error {
	if off != u.size {
		return fmt.Errorf("chunk offset %d, expected %d", off, u.size)
	}
	if len(data) > MaxChunk {
		return fmt.Errorf("chunk larger than %d bytes", MaxChunk)
	}
	n, err := u.f.Write(data)
	u.size += int64(n)
	return err
}

func (u *Upload) Size() int64 { return u.size }

func (u *Upload) Commit() error {
	if err := u.f.Sync(); err != nil {
		u.Abort()
		return err
	}
	if err := u.f.Close(); err != nil {
		u.site.root.Remove(u.tmp)
		return err
	}
	// Keep the permissions of a file being replaced.
	if fi, err := u.site.root.Stat(u.final); err == nil && fi.Mode().IsRegular() {
		u.site.root.Chmod(u.tmp, fi.Mode().Perm())
	}
	if err := u.site.root.Rename(u.tmp, u.final); err != nil {
		u.site.root.Remove(u.tmp)
		return err
	}
	return nil
}

func (u *Upload) Abort() {
	u.f.Close()
	u.site.root.Remove(u.tmp)
}
