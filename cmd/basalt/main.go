// Command basalt is the single binary for every Basalt role:
// the control plane (server) and the per-host agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wiefix/basalt/deploy"
	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/agenthub"
	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/buildinfo"
	"github.com/wiefix/basalt/internal/catalog"
	"github.com/wiefix/basalt/internal/compose"
	"github.com/wiefix/basalt/internal/config"
	"github.com/wiefix/basalt/internal/logging"
	"github.com/wiefix/basalt/internal/proxy/caddyrun"
	"github.com/wiefix/basalt/internal/secrets"
	"github.com/wiefix/basalt/internal/server"
	"github.com/wiefix/basalt/internal/standby"
	"github.com/wiefix/basalt/internal/store"
	"github.com/wiefix/basalt/internal/walship"
)

const usage = `Usage: basalt <command> [flags]

Commands:
  server    run the control plane (API and web UI)
            (server recover: restore the panel from a backup;
             server standby-enroll: make this host a standby panel;
             server promote: make this standby panel the panel)
  agent     run the agent on a managed host
            (agent enroll: register this host with a panel)
  proxy     run the web proxy (embedded Caddy) on a managed host
  catalog   catalog build DIR: check the app templates in DIR and print
            the catalog index (sign it with ssh-keygen -Y sign -n basalt-catalog)
  version   print version information

Run "basalt <command> -h" for command flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "server":
		err = runServer(ctx, args[1:], stderr)
	case "agent":
		err = runAgent(ctx, args[1:], stderr)
	case "proxy":
		err = runProxy(ctx, args[1:], stderr)
	case "catalog":
		err = runCatalog(ctx, args[1:], stdout)
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, buildinfo.String())
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "basalt: unknown command %q\n\n%s", args[0], usage)
		return 2
	}

	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	default:
		fmt.Fprintf(stderr, "basalt: %v\n", err)
		return 1
	}
}

func runServer(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "local-enroll" {
		return runLocalEnroll(ctx, args[1:], os.Stdout, stderr)
	}
	if len(args) > 0 && args[0] == "recover" {
		return runRecover(ctx, args[1:], os.Stdin, os.Stdout, stderr)
	}
	if len(args) > 0 && args[0] == "standby-enroll" {
		return runStandbyEnroll(ctx, args[1:], stderr)
	}
	if len(args) > 0 && args[0] == "promote" {
		return runPromote(ctx, args[1:], os.Stdout, stderr)
	}
	cfg, err := config.ParseServer(args, stderr)
	if err != nil {
		return err
	}
	log, err := logging.New(stderr, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	compose.SilenceLibraryLogs()
	return runPanel(ctx, cfg, log)
}

// runPanel runs the panel, or the standby panel the data directory belongs
// to (which goes on as the panel if it takes over).
func runPanel(ctx context.Context, cfg config.Server, baseLog *slog.Logger) error {
	log := baseLog.With("component", "server")
	if standby.IsStandby(cfg.DataDir) {
		return runStandby(ctx, cfg, baseLog)
	}

	dbPath := filepath.Join(cfg.DataDir, "basalt.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	// Checkpoints the database (store turns SQLite's own off) and feeds
	// standby panels.
	ship := walship.NewSource(dbPath, db, baseLog.With("component", "walship"))
	if err := db.GuessPanelServer(ctx); err != nil {
		log.Warn("find the panel's own server", "err", err)
	}

	instanceID, err := db.InstanceID(ctx)
	if err != nil {
		return err
	}
	schema, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	log.Info("starting",
		"version", buildinfo.Version,
		"instance_id", instanceID,
		"schema_version", schema,
		"data_dir", cfg.DataDir,
	)

	box, err := secrets.LoadOrCreateKey(filepath.Join(cfg.DataDir, "master.key"))
	if err != nil {
		return err
	}
	if n, err := db.FailUnfinishedDeployments(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("deployments interrupted by the restart were marked as failed", "count", n)
	}
	if err := db.FailUnfinishedPanelBackups(ctx); err != nil {
		return err
	}
	if err := db.FailUnfinishedBackupChecks(ctx); err != nil {
		return err
	}
	if n, err := db.FailUnfinishedBackups(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("backups interrupted by the restart were marked as failed", "count", n)
	}

	var setupToken string
	setupFile := filepath.Join(cfg.DataDir, "setup-token")
	users, err := db.CountUsers(ctx)
	if err != nil {
		return err
	}
	if users == 0 {
		setupToken = auth.NewToken()
		log.Warn("no users yet: complete setup with this one-time token (valid until restart)",
			"setup_token", setupToken, "file", setupFile)
		if err := os.WriteFile(setupFile, []byte(setupToken+"\n"), 0o600); err != nil {
			log.Warn("write setup token file", "err", err)
		}
	} else {
		os.Remove(setupFile)
	}
	trusted, err := server.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return err
	}
	if cfg.InsecureCookies {
		log.Warn("insecure cookies enabled: session cookies are sent over plain HTTP; development only")
	}

	ca, err := agenthub.LoadOrCreateCA(ctx, db, box, instanceID, time.Now())
	if err != nil {
		return err
	}
	hub := agenthub.New(baseLog.With("component", "agenthub"), db, ca, nil)
	hub.SetPanelURL(cfg.AgentURL)
	masterKey, err := os.ReadFile(filepath.Join(cfg.DataDir, "master.key"))
	if err != nil {
		return err
	}
	standbys := standby.NewFeeder(db, ca, ship, masterKey, baseLog.With("component", "standby"), nil)
	standbys.Register(hub.Mount)
	standbys.OnAgentURL(hub.PushPanelInfo)
	standbys.UseHub(hub)
	hub.OnPanelInfo(standbys.PushPanels)
	hub.ServeInstaller(deploy.InstallScript, cfg.BinariesDir)
	hub.ServeReleases(filepath.Join(cfg.DataDir, "releases"))

	api := server.New(server.Options{
		Log:                 log,
		DB:                  db,
		Box:                 box,
		Hasher:              auth.NewHasher(auth.DefaultArgon2Params, min(4, runtime.NumCPU())),
		SetupToken:          setupToken,
		SecureCookies:       !cfg.InsecureCookies,
		Agents:              hub,
		Standbys:            standbys,
		AgentURL:            cfg.AgentURL,
		TrustedProxies:      trusted,
		SetupTokenFile:      setupFile,
		DataDir:             cfg.DataDir,
		Restic:              cfg.Restic,
		MailHostPorts:       cfg.MailPorts,
		ReleaseURL:          cfg.ReleaseURL,
		ReleaseKey:          cfg.ReleaseKey,
		ReleaseAllowPrivate: cfg.ReleaseAllowPrivate,
		GitHubAPI:           cfg.GitHubAPI,
		GitHubWeb:           cfg.GitHubURL,
	})
	hub.OnConnect(func(ctx context.Context, serverID int64) {
		api.SyncSites(ctx, serverID)
		api.AnchorAudit(ctx, serverID)
	})
	hub.OnACMEChallenge(api.ACMEChallenge)
	hub.OnHealthReport(api.HealthReport)
	hub.OnSiteChanged(api.SiteChanged)
	standbys.UseFailover(api.FailoverFor)

	// Run both listeners; the first failure stops the other.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	wg.Go(func() { ship.Run(ctx) })
	for _, serve := range []func(context.Context) error{
		func(ctx context.Context) error { return api.Serve(ctx, cfg.Listen) },
		func(ctx context.Context) error { return hub.Serve(ctx, cfg.AgentListen) },
	} {
		wg.Go(func() {
			if err := serve(ctx); err != nil {
				cancel(err)
			}
		})
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// runStandby keeps a copy of another panel (the data directory belongs to
// a standby panel) and answers on the panel's address with its state.
func runStandby(ctx context.Context, cfg config.Server, baseLog *slog.Logger) error {
	log := baseLog.With("component", "server")
	c, err := standby.NewClient(cfg.DataDir, cfg.AgentURL, baseLog.With("component", "standby"))
	if err != nil {
		return err
	}
	unlock, err := c.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	st := c.Status()
	log.Info("starting as a standby panel", "version", buildinfo.Version, "panel", st.Panel, "standby_id", st.StandbyID, "data_dir", cfg.DataDir)
	srv := &http.Server{Addr: cfg.Listen, Handler: c, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	ran := make(chan error, 1)
	go func() { ran <- c.Run(ctx) }()
	var runErr error
	select {
	case err := <-errc:
		return err
	case runErr = <-ran:
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil || !errors.Is(runErr, standby.ErrTakeOver) {
		return err
	}
	// The panel is gone and the servers agree: this standby takes over and
	// goes on as the panel.
	unlock()
	p, err := standby.TakeOver(ctx, cfg.DataDir)
	if err != nil {
		return fmt.Errorf("take over the panel: %w", err)
	}
	log.Warn("this server took over the panel", "epoch", p.Epoch, "copy_up_to", p.LastChange)
	return runPanel(ctx, cfg, baseLog)
}

// runPromote makes the standby panel in the data directory the panel.
func runPromote(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := config.ParsePromote(args, stderr)
	if err != nil {
		return err
	}
	p, err := standby.Promote(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "This server holds the panel now (epoch %d).\n", p.Epoch)
	if !p.LastChange.IsZero() {
		fmt.Fprintf(stdout, "Its copy has the panel's changes up to about %s (the latest audit log entry).\n", p.LastChange.UTC().Format(time.RFC3339))
	}
	fmt.Fprint(stdout, "\nNext:\n  1. Start it: systemctl start basalt-server. Agents and the other standby\n     panels that cannot reach the former panel come here by themselves (they\n     learned this address).\n")
	switch {
	case p.Zone != "":
		fmt.Fprintf(stdout, "  2. The panel's domain follows by itself: %s is in the zone %s of your\n     Basalt DNS, whose automatic A/AAAA records point at the server the panel\n     runs on (unless a record you added there overrides them).\n", p.PanelURL, p.Zone)
	case p.PanelURL != "":
		fmt.Fprintf(stdout, "  2. Point the panel's domain (%s) at this server at its DNS provider.\n", p.PanelURL)
	default:
		fmt.Fprint(stdout, "  2. Point the panel's domain, if it has one, at this server.\n")
	}
	fmt.Fprint(stdout, "  3. Keep the former panel stopped. If it comes back, its agents and standbys\n     stay here (they know the newer epoch) and it shows that it was replaced.\n     To make it a standby of this panel, add a standby panel here and install\n     it there.\n")
	return nil
}

func runStandbyEnroll(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, err := config.ParseStandbyEnroll(args, stderr)
	if err != nil {
		return err
	}
	id, err := standby.Enroll(ctx, standby.EnrollOptions{DataDir: cfg.DataDir, PanelURL: cfg.PanelURL, Token: cfg.Token, CASHA256: cfg.CASHA256})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "enrolled as standby panel %d of %s; `basalt server --data-dir %s` now keeps a copy of it\n", id, cfg.PanelURL, cfg.DataDir)
	return nil
}

func runAgent(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "enroll" {
		return runAgentEnroll(ctx, args[1:], stderr)
	}
	cfg, err := config.ParseAgent(args, stderr)
	if err != nil {
		return err
	}
	log, err := logging.New(stderr, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	log = log.With("component", "agent")

	a, err := agent.New(agent.Config{
		DataDir:         cfg.DataDir,
		ProxyDir:        cfg.ProxyDir,
		SitesRoot:       cfg.SitesRoot,
		HTTPPort:        cfg.HTTPPort,
		HTTPSPort:       cfg.HTTPSPort,
		ACMEDirectory:   cfg.ACMEDirectory,
		ACMECARoots:     splitList(cfg.ACMECARoot),
		ACMEResolvers:   splitList(cfg.ACMEResolvers),
		Resolvers:       splitList(cfg.Resolvers),
		SFTPListen:      cfg.SFTPListen,
		DNSListen:       cfg.DNSListen,
		PeerListen:      cfg.PeerListen,
		Isolate:         cfg.Isolate == "on" || (cfg.Isolate == "auto" && runtime.GOOS == "linux" && os.Geteuid() == 0),
		ProxyUser:       cfg.ProxyUser,
		MySQLSocket:     cfg.MySQLSocket,
		DockerSocket:    cfg.DockerSocket,
		DockerNamespace: cfg.DockerNamespace,
		GitCAFile:       cfg.GitCAFile,
		Restic:          cfg.Restic,
		Railpack:        cfg.Railpack,
		PanelDomain:     cfg.PanelDomain,
		ReleaseKey:      cfg.ReleaseKey,
		WordPressURL:    cfg.WordPressURL,
		PanelUpstream:   cfg.PanelUpstream,
	}, log)
	if err != nil {
		return err
	}
	log.Info("starting", "version", buildinfo.Version, "server_id", a.ServerID(), "data_dir", cfg.DataDir)
	return a.Run(ctx)
}

// runLocalEnroll creates (or reuses) a server entry for this host directly in
// the database and prints the flags for `basalt agent enroll`. Used by the
// installer so the panel's own host is managed without a manual step.
func runLocalEnroll(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := config.ParseLocalEnroll(args, stderr)
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "basalt.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	box, err := secrets.LoadOrCreateKey(filepath.Join(cfg.DataDir, "master.key"))
	if err != nil {
		return err
	}
	instanceID, err := db.InstanceID(ctx)
	if err != nil {
		return err
	}
	ca, err := agenthub.LoadOrCreateCA(ctx, db, box, instanceID, time.Now())
	if err != nil {
		return err
	}
	token := auth.NewToken()
	expires := time.Now().Add(time.Hour)
	srv, err := db.CreateServer(ctx, cfg.Name, auth.HashToken(token), expires)
	if errors.Is(err, store.ErrNameTaken) {
		return fmt.Errorf("a server named %q already exists", cfg.Name)
	} else if err != nil {
		return err
	}
	// The panel's own host: it does not vote on whether the panel is gone.
	if err := db.SetSetting(ctx, store.SettingPanelServer, strconv.FormatInt(srv.ID, 10)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "--panel %s --token %s --ca-sha256 %s\n", cfg.AgentURL, token, ca.Fingerprint())
	return nil
}

func runProxy(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, err := config.ParseProxy(args, stderr)
	if err != nil {
		return err
	}
	log, err := logging.New(stderr, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	log = log.With("component", "proxy")
	log.Info("starting", "version", buildinfo.Version, "data_dir", cfg.DataDir)
	return caddyrun.Run(ctx, cfg.DataDir, log)
}

func runAgentEnroll(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, err := config.ParseAgentEnroll(args, stderr)
	if err != nil {
		return err
	}
	id, err := agent.Enroll(ctx, agent.EnrollOptions{
		DataDir:  cfg.DataDir,
		PanelURL: cfg.PanelURL,
		Token:    cfg.Token,
		CASHA256: cfg.CASHA256,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "enrolled as server %d; credentials saved in %s\n", id, cfg.DataDir)
	// Installed by install.sh, whose service starts the agent next.
	if !agent.InstalledAsService() {
		fmt.Fprintf(stderr, "start the agent with: basalt agent --data-dir %s\n", cfg.DataDir)
	}
	return nil
}

// splitList splits a comma-separated flag value, dropping empty items.
func splitList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// runCatalog builds a catalog index from a directory of templates (one
// directory per template with template.yaml and compose.yaml).
func runCatalog(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) != 2 || args[0] != "build" {
		return errors.New("usage: basalt catalog build DIR > catalog.json")
	}
	data, err := catalog.BuildIndex(ctx, os.DirFS(args[1]), ".", time.Now())
	if err != nil {
		return fmt.Errorf("%s: %w", args[1], err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}
