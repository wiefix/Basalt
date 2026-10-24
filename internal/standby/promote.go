package standby

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/store"
	"github.com/wiefix/basalt/internal/walship"
)

// fileLock is held while a standby follows its panel, so a promotion
// cannot change the copy under it.
const fileLock = "standby.lock"

// Promoted is what a promotion did.
type Promoted struct {
	Epoch int64
	// LastChange is the newest entry of the audit log in the copy: about
	// when the panel last changed something.
	LastChange time.Time
	// PanelURL is the panel's public address as set under Notifications
	// ("" when unset), and Zone the DNS zone in Basalt holding its host
	// ("" when none does).
	PanelURL string
	Zone     string
}

// Promote makes the standby panel in dir the panel: its copy of the
// database is checked and becomes the panel's, with a higher epoch, so
// agents leave the panel it copied (they refuse a lower epoch) once they
// reach this one. The standby must not be running.
func Promote(ctx context.Context, dir string) (*Promoted, error) { return promote(ctx, dir, false) }

// TakeOver promotes the standby in dir that decided by itself to take over
// (Run returned ErrTakeOver; its lock released): as Promote, and the new
// panel announces it was automatic.
func TakeOver(ctx context.Context, dir string) (*Promoted, error) { return promote(ctx, dir, true) }

func promote(ctx context.Context, dir string, auto bool) (*Promoted, error) {
	if !IsStandby(dir) {
		return nil, fmt.Errorf("%s does not belong to a standby panel", dir)
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return nil, errors.New("the standby panel is running: stop it first (systemctl stop basalt-server)")
	}
	defer unlock()
	c, err := NewClient(dir, "", nil)
	if err != nil {
		return nil, err
	}
	if c.removed {
		return nil, errors.New("the panel removed this standby panel: its copy stopped then and is not promoted")
	}
	dbPath := filepath.Join(dir, fileDB)
	for _, f := range []string{dbPath, filepath.Join(dir, fileKey)} {
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("no complete copy of the panel yet (%s): %w", filepath.Base(f), err)
		}
	}
	// The copy as the last frame left it, checked.
	if err := walship.Checkpoint(ctx, dbPath); err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open the copy: %w", err)
	}
	defer db.Close()
	p := &Promoted{}
	var last int64
	db.R.QueryRowContext(ctx, `SELECT COALESCE(MAX(at), 0) FROM audit_log`).Scan(&last)
	if last > 0 {
		p.LastChange = time.Unix(last, 0)
	}
	if p.Epoch, err = db.Promote(ctx, c.cfg.StandbyID, auto, time.Now()); err != nil {
		return nil, fmt.Errorf("promote the copy: %w", err)
	}
	p.PanelURL, p.Zone = panelZone(ctx, db)
	if err := db.Close(); err != nil {
		return nil, err
	}
	// From here on the directory holds a panel.
	for _, f := range []string{FileConfig, fileCreds, fileCA, fileRemoved, fileLock} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := matchOwner(dir); err != nil {
		return nil, fmt.Errorf("give the files to the panel's user: %w", err)
	}
	return p, nil
}

// panelZone finds the panel's public address and the zone in Basalt's DNS
// holding its host.
func panelZone(ctx context.Context, db *store.DB) (panelURL, zone string) {
	panelURL, _ = db.Setting(ctx, "panel_url")
	u, err := url.Parse(panelURL)
	if err != nil || u.Hostname() == "" {
		return panelURL, ""
	}
	host := strings.ToLower(u.Hostname())
	zones, err := db.DNSZones(ctx)
	if err != nil {
		return panelURL, ""
	}
	for _, z := range zones {
		if (host == z.Name || strings.HasSuffix(host, "."+z.Name)) && len(z.Name) > len(zone) {
			zone = z.Name
		}
	}
	return panelURL, zone
}
