// Command basalt is the single binary for every Basalt role:
// the control plane (server) and the per-host agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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
	"github.com/wiefix/basalt/internal/store"
)

const usage = `Usage: basalt <command> [flags]

Commands:
  server    run the control plane (API and web UI)
            (server recover: restore the panel from a backup)
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
	cfg, err := config.ParseServer(args, stderr)
	if err != nil {
		return err
	}
	log, err := logging.New(stderr, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	baseLog := log
	log = log.With("component", "server")
	compose.SilenceLibraryLogs()

	db, err := store.Open(ctx, filepath.Join(cfg.DataDir, "basalt.db"))
	if err != nil {
		return err
	}
	defer db.Close()

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
	hub.ServeInstaller(deploy.InstallScript, cfg.BinariesDir)

	api := server.New(server.Options{
		Log:            log,
		DB:             db,
		Box:            box,
		Hasher:         auth.NewHasher(auth.DefaultArgon2Params, min(4, runtime.NumCPU())),
		SetupToken:     setupToken,
		SecureCookies:  !cfg.InsecureCookies,
		Agents:         hub,
		AgentURL:       cfg.AgentURL,
		TrustedProxies: trusted,
		SetupTokenFile: setupFile,
		DataDir:        cfg.DataDir,
		Restic:         cfg.Restic,
		MailHostPorts:  cfg.MailPorts,
	})
	hub.OnConnect(api.SyncSites)
	hub.OnACMEChallenge(api.ACMEChallenge)
	hub.OnHealthReport(api.HealthReport)

	// Run both listeners; the first failure stops the other.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
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
		DataDir:       cfg.DataDir,
		ProxyDir:      cfg.ProxyDir,
		SitesRoot:     cfg.SitesRoot,
		HTTPPort:      cfg.HTTPPort,
		HTTPSPort:     cfg.HTTPSPort,
		ACMEDirectory: cfg.ACMEDirectory,
		ACMECARoots:   splitList(cfg.ACMECARoot),
		ACMEResolvers: splitList(cfg.ACMEResolvers),
		Resolvers:     splitList(cfg.Resolvers),
		SFTPListen:    cfg.SFTPListen,
		DNSListen:     cfg.DNSListen,
		Isolate:       cfg.Isolate == "on" || (cfg.Isolate == "auto" && runtime.GOOS == "linux" && os.Geteuid() == 0),
		ProxyUser:     cfg.ProxyUser,
		MySQLSocket:   cfg.MySQLSocket,
		DockerSocket:  cfg.DockerSocket,
		GitCAFile:     cfg.GitCAFile,
		Restic:        cfg.Restic,
		PanelDomain:   cfg.PanelDomain,
		PanelUpstream: cfg.PanelUpstream,
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
	if _, err := db.CreateServer(ctx, cfg.Name, auth.HashToken(token), expires); errors.Is(err, store.ErrNameTaken) {
		return fmt.Errorf("a server named %q already exists", cfg.Name)
	} else if err != nil {
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
	fmt.Fprintf(stderr, "enrolled as server %d; credentials saved in %s\nstart the agent with: basalt agent --data-dir %s\n",
		id, cfg.DataDir, cfg.DataDir)
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
