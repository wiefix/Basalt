package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
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
