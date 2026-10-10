package caddyrun

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/wiefix/basalt/internal/proxy"
	// Standard Caddy modules (http, tls, file_server, reverse_proxy, ...).
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

// Run starts the embedded Caddy with the last configuration the agent wrote,
// so sites keep working even when the agent or panel is down. It blocks
// until ctx is cancelled.
func Run(ctx context.Context, dir string, log *slog.Logger) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A socket left over from an unclean exit would block the admin listener.
	if err := os.Remove(proxy.AdminSocket(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale admin socket: %w", err)
	}

	if err := loadTable(dir); err != nil {
		log.Warn("saved app upstreams ignored", "err", err)
	}
	cfg, err := os.ReadFile(proxy.ConfigPath(dir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		log.Info("no saved configuration yet; waiting for the agent")
		if cfg, err = proxy.BaseConfig(dir); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("read saved config: %w", err)
	}
	if err := loadWithRetry(ctx, cfg); err != nil {
		// A broken saved config must not keep the proxy down: start with the
		// admin endpoint only and let the agent push a fresh one.
		log.Error("saved configuration failed to load; starting empty", "err", err)
		base, _ := proxy.BaseConfig(dir)
		if err := caddy.Load(base, true); err != nil {
			return fmt.Errorf("start proxy: %w", err)
		}
	}
	log.Info("proxy running", "admin", proxy.AdminSocket(dir))
	<-ctx.Done()
	log.Info("stopping proxy")
	// caddy.Stop leaves the admin endpoint running; replacing the config with
	// one that disables it shuts down the sites and the admin socket together.
	if err := caddy.Load([]byte(`{"admin":{"disabled":true,"config":{"persist":false}}}`), true); err != nil {
		log.Warn("shutdown", "err", err)
	}
	return caddy.Stop()
}

// loadWithRetry tolerates transient start-up failures (e.g. a listener of a
// previous instance still shutting down) before the caller gives up on the
// saved configuration.
func loadWithRetry(ctx context.Context, cfg []byte) error {
	var err error
	for attempt := range 5 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(time.Duration(attempt) * 300 * time.Millisecond):
			}
		}
		if err = caddy.Load(cfg, true); err == nil {
			return nil
		}
	}
	return err
}
