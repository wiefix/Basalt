package agent

import (
	"context"
	"io/fs"
	"path/filepath"
	"syscall"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/mariadb"
)

// siteUsage measures sites' directories (without following symlinks) and
// the sizes of their MariaDB databases.
func (a *Agent) siteUsage(ctx context.Context, req agentproto.SiteUsageRequest) (agentproto.SiteUsageResponse, error) {
	out := agentproto.SiteUsageResponse{Sites: []agentproto.SiteUsage{}}
	for _, id := range req.Sites {
		u := agentproto.SiteUsage{SiteID: id}
		dir, err := a.sites.dir(id)
		if err == nil {
			u.Bytes, u.Files, err = diskUsage(ctx, dir)
		}
		if err != nil {
			u.Error = publicError(err).Error()
		}
		out.Sites = append(out.Sites, u)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
	}
	if len(req.Databases) > 0 && a.mysql != "" && mariadb.Available(a.mysql) {
		db, err := mariadb.Open(a.mysql)
		if err != nil {
			return out, err
		}
		defer db.Close()
		if out.Databases, err = db.Sizes(ctx, req.Databases); err != nil {
			return out, err
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
