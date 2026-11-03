package privhelper

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/sysuser"
)

// The sites' folders: <SitesRoot>/<id>/{public,logs,tmp}. The sites folder
// and the trash belong to root; what is inside a site's folder belongs to
// its user (with isolation), with the shared web group on the folders the
// proxy reads: mode 2750, so the proxy (a member of the web group) can
// read, other sites cannot even enter, and setgid hands the web group to
// new files.

const (
	// MsgSiteFolderEnsure creates a site's folders, with its user (with
	// isolation) (SiteRequest); MsgSiteFolderTrash moves them to the trash
	// (SiteRequest).
	MsgSiteFolderEnsure = "site_folder_ensure"
	MsgSiteFolderTrash  = "site_folder_trash"
	// MsgTrash lists the trash, after removing an entry or restoring one as
	// a new site's folder (agentproto.SiteTrash →
	// agentproto.SiteTrashResponse); MsgTrashClean removes the entries
	// older than TrashKeep.
	MsgTrash      = "trash"
	MsgTrashClean = "trash_clean"
	// MsgSiteUsage measures sites' folders (SiteUsageRequest →
	// SiteUsageResult).
	MsgSiteUsage = "site_usage"
	// MsgSiteTemps removes the sites' upload and copy temporaries last
	// written before a time (SiteTemps → RemovedTemps).
	MsgSiteTemps = "site_temps"
)

const (
	trashDir = ".trash"
	// TrashKeep is how long deleted sites wait in the trash.
	TrashKeep = 30 * 24 * time.Hour
)

type SiteUsageRequest struct {
	SiteIDs []int64 `json:"site_ids"`
}

type SiteUsageResult struct {
	Sites []agentproto.SiteUsage `json:"sites"`
}

type SiteTemps struct {
	Before time.Time `json:"before"`
}

// RemovedTemps is how many temporaries each site lost (sites without any
// left out).
type RemovedTemps struct {
	Sites map[int64]int `json:"sites"`
}

const placeholderPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Site is ready</title></head>
<body style="font-family:system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem">
<h1>It works!</h1><p>Upload your files to replace this page.</p></body></html>
`

// siteFolder is the folder of site id (which may not exist).
func (h *Helper) siteFolder(id int64) (string, error) {
	if id <= 0 {
		return "", errSiteID
	}
	return h.siteDir(id), nil
}

// EnsureSiteFolder creates the site's folders (and, with isolation, its
// user, who owns them) and puts a placeholder page in an empty public/.
func (h *Helper) EnsureSiteFolder(ctx context.Context, id int64) error {
	d, err := h.siteFolder(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.cfg.SitesRoot, 0o755); err != nil {
		return err
	}
	if fi, err := os.Lstat(d); err == nil && !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", d)
	}
	uid, gid, wwwGID := -1, -1, -1
	if h.cfg.Isolate {
		if uid, gid, err = h.EnsureSiteUser(ctx, id); err != nil {
			return err
		}
		if wwwGID, err = sysuser.LookupGroup(h.cfg.WWWGroup); err != nil {
			return err
		}
	}

	// The site's folder is in the root-owned sites folder; what is inside
	// belongs to the site's user, who could swap a folder for a symlink
	// meanwhile: it is reached through os.Root and changed through the
	// open folder, never by path.
	if err := os.Mkdir(d, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	r, err := os.OpenRoot(d)
	if err != nil {
		return err
	}
	defer r.Close()
	layout := []struct {
		name string
		mode fs.FileMode
		gid  int
	}{
		{".", 0o750 | fs.ModeSetgid, wwwGID},
		{"public", 0o750 | fs.ModeSetgid, wwwGID},
		{"logs", 0o750 | fs.ModeSetgid, wwwGID},
		{"tmp", 0o700, gid},
	}
	for _, l := range layout {
		if l.name != "." {
			if err := r.Mkdir(l.name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		}
		if !h.cfg.Isolate {
			continue
		}
		if err := ownDir(r, l.name, uid, l.gid, l.mode); err != nil {
			return fmt.Errorf("%s: %w", filepath.Join(d, l.name), err)
		}
	}

	var s *files.Site
	if h.cfg.Isolate {
		s, err = files.OpenOwned(d, uid)
	} else {
		s, err = files.Open(d)
	}
	if err != nil {
		return err
	}
	defer s.Close()
	if entries, err := s.List("/public"); err == nil && len(entries) == 0 {
		return s.WriteFile("/public/index.html", []byte(placeholderPage))
	}
	return nil
}

// ownDir gives the folder name in r its owner and mode, through the open
// folder: a symlink put there instead cannot redirect the change (one
// swapped in after the check leads, at most, to another folder of the
// site).
func ownDir(r *os.Root, name string, uid, gid int, mode fs.FileMode) error {
	if fi, err := r.Lstat(name); err != nil || !fi.IsDir() {
		return errors.New("not a directory")
	}
	f, err := r.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.IsDir() {
		return errors.New("not a directory")
	}
	if err := f.Chown(uid, gid); err != nil {
		return err
	}
	return f.Chmod(mode)
}

// TrashSiteFolder moves the site's files to the trash rather than erasing
// them, so an accidental deletion can be recovered.
func (h *Helper) TrashSiteFolder(ctx context.Context, id int64) error {
	d, err := h.siteFolder(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(d); err != nil {
		return nil
	}
	trash := filepath.Join(h.cfg.SitesRoot, trashDir)
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	// The same site deleted twice within a second (a copy removed from a
	// server and the site deleted) takes the next free second.
	ts := time.Now().Unix()
	dest := filepath.Join(trash, fmt.Sprintf("%d-%d", id, ts))
	for _, err := os.Lstat(dest); err == nil; _, err = os.Lstat(dest) {
		ts++
		dest = filepath.Join(trash, fmt.Sprintf("%d-%d", id, ts))
	}
	if err := os.Rename(d, dest); err != nil {
		return err
	}
	h.log.Info("site moved to trash", "site_id", id, "path", dest)
	return nil
}

var trashNameRE = regexp.MustCompile(`^([1-9][0-9]{0,18})-([0-9]{1,19})$`)

func parseTrashName(name string) (siteID int64, at time.Time, ok bool) {
	m := trashNameRE.FindStringSubmatch(name)
	if m == nil {
		return 0, time.Time{}, false
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	sec, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return 0, time.Time{}, false
	}
	return id, time.Unix(sec, 0), true
}

// Trash restores entry req.Restore as the folder of site req.SiteID and
// removes entry req.Remove (either may be ""), then lists the trash.
func (h *Helper) Trash(ctx context.Context, req agentproto.SiteTrash) (agentproto.SiteTrashResponse, error) {
	out := agentproto.SiteTrashResponse{Entries: []agentproto.TrashEntry{}}
	if req.Restore != "" {
		if err := h.restoreTrash(ctx, req.Restore, req.SiteID); err != nil {
			return out, err
		}
	}
	dir := filepath.Join(h.cfg.SitesRoot, trashDir)
	if req.Remove != "" {
		if _, _, ok := parseTrashName(req.Remove); !ok {
			return out, errors.New("invalid trash entry")
		}
		p := filepath.Join(dir, req.Remove)
		if _, err := os.Lstat(p); err != nil {
			return out, errors.New("no such file in the trash")
		}
		if err := os.RemoveAll(p); err != nil {
			return out, files.PublicError(err)
		}
		h.log.Info("removed a deleted site's files", "entry", req.Remove)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return out, files.PublicError(err)
	}
	for _, e := range entries {
		id, at, ok := parseTrashName(e.Name())
		if !ok {
			continue
		}
		bytes, _, _ := diskUsage(ctx, filepath.Join(dir, e.Name()))
		out.Entries = append(out.Entries, agentproto.TrashEntry{Name: e.Name(), SiteID: id, DeletedAt: at, Bytes: bytes})
	}
	slices.SortFunc(out.Entries, func(a, b agentproto.TrashEntry) int { return b.DeletedAt.Compare(a.DeletedAt) })
	return out, nil
}

// CleanTrash removes deleted sites older than TrashKeep.
func (h *Helper) CleanTrash(context.Context) error {
	h.cleanTrash(time.Now())
	return nil
}

func (h *Helper) cleanTrash(now time.Time) {
	dir := filepath.Join(h.cfg.SitesRoot, trashDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_, at, ok := parseTrashName(e.Name())
		if !ok || now.Sub(at) < TrashKeep || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			h.log.Warn("clean the trash", "entry", e.Name(), "err", err)
			continue
		}
		h.log.Info("removed a deleted site's files after 30 days", "entry", e.Name())
	}
}

// restoreTrash makes the trash entry name the folder of site id (a new
// site, whose folder does not exist yet). The deleted site's user is gone:
// its files go to the new site's user (the www group stays), without
// following symlinks (through os.Root: the walk stays inside the folder).
func (h *Helper) restoreTrash(ctx context.Context, name string, id int64) error {
	if _, _, ok := parseTrashName(name); !ok || id <= 0 {
		return errors.New("invalid trash entry")
	}
	src := filepath.Join(h.cfg.SitesRoot, trashDir, name)
	if fi, err := os.Lstat(src); err != nil || !fi.IsDir() {
		return errors.New("no such folder in the trash")
	}
	dst, err := h.siteFolder(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("site %d has a folder already", id)
	}
	uid, gid, www := -1, -1, -1
	if h.cfg.Isolate {
		if uid, gid, err = h.EnsureSiteUser(ctx, id); err != nil {
			return err
		}
		if www, err = sysuser.LookupGroup(h.cfg.WWWGroup); err != nil {
			return err
		}
	}
	if err := os.Rename(src, dst); err != nil {
		return files.PublicError(err)
	}
	h.log.Info("restored a deleted site's files", "entry", name, "site_id", id)
	if !h.cfg.Isolate {
		return nil
	}
	r, err := os.OpenRoot(dst)
	if err != nil {
		return files.PublicError(err)
	}
	defer r.Close()
	return fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		g := gid
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Gid) == www {
			g = www
		}
		return r.Lchown(p, uid, g)
	})
}

// SiteUsage measures the sites' folders (without following symlinks).
func (h *Helper) SiteUsage(ctx context.Context, ids []int64) (SiteUsageResult, error) {
	out := SiteUsageResult{Sites: []agentproto.SiteUsage{}}
	if len(ids) > 10000 {
		return out, errors.New("too many sites")
	}
	for _, id := range ids {
		u := agentproto.SiteUsage{SiteID: id}
		dir, err := h.siteFolder(id)
		if err == nil {
			u.Bytes, u.Files, err = diskUsage(ctx, dir)
		}
		if err != nil {
			u.Error = files.PublicError(err).Error()
		}
		out.Sites = append(out.Sites, u)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
	}
	return out, nil
}

// diskUsage adds up the space files under dir take (blocks, so sparse files
// count as stored) and counts them.
func diskUsage(ctx context.Context, dir string) (bytes, files int64, err error) {
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // vanished or unreadable: skipped
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			bytes += int64(st.Blocks) * 512
		} else {
			bytes += fi.Size()
		}
		if !d.IsDir() {
			files++
		}
		return nil
	})
	return bytes, files, err
}

// RemoveTemps removes the sites' upload and copy temporaries last written
// before before.
func (h *Helper) RemoveTemps(ctx context.Context, before time.Time) (RemovedTemps, error) {
	out := RemovedTemps{Sites: map[int64]int{}}
	entries, err := os.ReadDir(h.cfg.SitesRoot)
	if err != nil {
		return out, nil
	}
	for _, e := range entries {
		id, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil || id <= 0 || !e.IsDir() || ctx.Err() != nil {
			continue
		}
		s, err := files.Open(filepath.Join(h.cfg.SitesRoot, e.Name()))
		if err != nil {
			continue
		}
		n, err := s.RemoveTemps(before)
		s.Close()
		if n > 0 || err != nil {
			h.log.Info("removed leftover temporary files", "site_id", id, "files", n, "err", err)
		}
		if n > 0 {
			out.Sites[id] = n
		}
	}
	return out, nil
}
