package agent

import (
	"context"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/mariadb"
)

// siteUsage measures sites' folders (the helper does) and the sizes of
// their MariaDB databases.
func (a *Agent) siteUsage(ctx context.Context, req agentproto.SiteUsageRequest) (agentproto.SiteUsageResponse, error) {
	out := agentproto.SiteUsageResponse{Sites: []agentproto.SiteUsage{}}
	if len(req.Sites) > 0 {
		// The helper reads every site's folder.
		u, err := a.sites.priv.SiteUsage(ctx, req.Sites)
		if err != nil {
			return out, err
		}
		out.Sites = u.Sites
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
