package agent

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
	"github.com/wiefix/basalt/internal/sysuser"
)

// Deleted sites wait in <sites-root>/.trash/<id>-<unix time> for trashKeep,
// then the janitor removes them.
const trashKeep = 30 * 24 * time.Hour

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

// trash lists the trash, removing entry remove first ("" for none).
func (m *sitesManager) trash(ctx context.Context, remove string) (agentproto.SiteTrashResponse, error) {
	dir := filepath.Join(m.root, trashDir)
	if remove != "" {
		if _, _, ok := parseTrashName(remove); !ok {
			return agentproto.SiteTrashResponse{}, errors.New("invalid trash entry")
		}
		p := filepath.Join(dir, remove)
		if _, err := os.Lstat(p); err != nil {
			return agentproto.SiteTrashResponse{}, errors.New("no such file in the trash")
		}
		if err := os.RemoveAll(p); err != nil {
			return agentproto.SiteTrashResponse{}, publicError(err)
		}
		m.log.Info("removed a deleted site's files", "entry", remove)
	}
	out := agentproto.SiteTrashResponse{Entries: []agentproto.TrashEntry{}}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return out, publicError(err)
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

// cleanTrash removes deleted sites older than trashKeep.
func (m *sitesManager) cleanTrash(now time.Time) {
	dir := filepath.Join(m.root, trashDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_, at, ok := parseTrashName(e.Name())
		if !ok || now.Sub(at) < trashKeep || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			m.log.Warn("clean the trash", "entry", e.Name(), "err", err)
			continue
		}
		m.log.Info("removed a deleted site's files after 30 days", "entry", e.Name())
	}
}

// restoreTrash makes the trash entry name the folder of site id (a new
// site, whose folder does not exist yet). The deleted site's user is gone:
// its files go to the new site's user (the www group stays), without
// following symlinks.
func (m *sitesManager) restoreTrash(ctx context.Context, name string, id int64) error {
	if _, _, ok := parseTrashName(name); !ok || id <= 0 {
		return errors.New("invalid trash entry")
	}
	src := filepath.Join(m.root, trashDir, name)
	if fi, err := os.Lstat(src); err != nil || !fi.IsDir() {
		return errors.New("no such folder in the trash")
	}
	dst, err := m.dir(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("site %d has a folder already", id)
	}
	uid, gid, www := -1, -1, -1
	if m.isolate {
		if uid, gid, err = sysuser.EnsureUser(ctx, sysuser.SiteUser(id), dst); err != nil {
			return err
		}
		if www, err = sysuser.LookupGroup(m.wwwGroup); err != nil {
			return err
		}
	}
	if err := os.Rename(src, dst); err != nil {
		return publicError(err)
	}
	m.log.Info("restored a deleted site's files", "entry", name, "site_id", id)
	if !m.isolate {
		return nil
	}
	return filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
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
		return os.Lchown(p, uid, g)
	})
}
