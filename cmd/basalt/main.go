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
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/buildinfo"
	"github.com/wiefix/basalt/internal/config"
	"github.com/wiefix/basalt/internal/logging"
	"github.com/wiefix/basalt/internal/secrets"
	"github.com/wiefix/basalt/internal/server"
	"github.com/wiefix/basalt/internal/store"
)

const usage = `Usage: basalt <command> [flags]

Commands:
  server    run the control plane (API and web UI)
  agent     run the agent on a managed host
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
	cfg, err := config.ParseServer(args, stderr)
	if err != nil {
		return err
	}
	log, err := logging.New(stderr, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	log = log.With("component", "server")

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

	var setupToken string
	users, err := db.CountUsers(ctx)
	if err != nil {
		return err
	}
	if users == 0 {
		setupToken = auth.NewToken()
		log.Warn("no users yet: complete setup with this one-time token (valid until restart)",
			"setup_token", setupToken)
	}
	if cfg.InsecureCookies {
		log.Warn("insecure cookies enabled: session cookies are sent over plain HTTP; development only")
	}

	return server.New(server.Options{
		Log:           log,
		DB:            db,
		Box:           box,
		Hasher:        auth.NewHasher(auth.DefaultArgon2Params, min(4, runtime.NumCPU())),
		SetupToken:    setupToken,
		SecureCookies: !cfg.InsecureCookies,
	}).Serve(ctx, cfg.Listen)
}

func runAgent(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, err := config.ParseAgent(args, stderr)
	if err != nil {
		return err
	}
	log, err := logging.New(stderr, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	log = log.With("component", "agent")
	log.Info("starting", "version", buildinfo.Version, "data_dir", cfg.DataDir)

	// Enrollment and the control channel arrive in phase 1, step 3.
	log.Warn("agent is not implemented yet; idling until stopped")
	<-ctx.Done()
	log.Info("stopped", slog.String("reason", context.Cause(ctx).Error()))
	return nil
}
