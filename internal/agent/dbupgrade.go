package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
)

// A PostgreSQL major upgrade (agentproto.DBUpgrade) loads the old data into
// a new cluster in the same volume: the new version's container mounts it
// at pgUpgradeMount and initialises the cluster in <version>/docker, the
// directory the image uses from 18 on. Mounted there (not at the image's
// own path), the image does not refuse to initialise beside the old data.
// pgUpgradeInit empties that directory first: what an earlier attempt left.
const (
	pgUpgradeMount = "/upgrade"
	pgUpgradeInit  = `rm -rf "$0" && exec docker-entrypoint.sh postgres`
	// pgUpgradeDrop removes from the volume (at /v) everything but the new
	// version's directory: the old data, once the new version runs.
	pgUpgradeDrop = `find /v -mindepth 1 -maxdepth 1 ! -name "$0" -exec rm -rf {} +`
)

// dbTempConfig is the container of a database service run alone for an
// upgrade: no network (no client reaches it), no restarts, not one of the
// app's containers.
func (m *appsManager) dbTempConfig(spec agentproto.AppSpec, svc agentproto.ServiceSpec) docker.ContainerConfig {
	cfg := m.serviceConfig(spec, svc, "")
	cfg.Labels, cfg.NetworkingConfig, cfg.ExposedPorts = nil, nil, nil
	hc := *cfg.HostConfig
	hc.NetworkMode, hc.RestartPolicy, hc.PortBindings = "none", docker.RestartPolicy{}, nil
	cfg.HostConfig = &hc
	return cfg
}

// upgradePostgres moves the database to the new major version while its
// service is stopped: a container of the old version dumps everything
// (pg_dumpall), the new version's loads it (psql). On an error both are
// removed and the old data is as it was.
func (m *appsManager) upgradePostgres(ctx context.Context, dc *docker.Client, spec agentproto.AppSpec, svc agentproto.ServiceSpec, lg *deployLog) error {
	up := spec.DBUpgrade
	bin, err := exec.LookPath("docker")
	if err != nil {
		return errors.New("the docker command is not installed")
	}
	user := svc.Env["POSTGRES_USER"]
	if user == "" || up.From.Env["POSTGRES_USER"] != user {
		return errors.New("upgrade: the database's user is missing")
	}
	if _, _, err := m.pull(ctx, dc, up.From.Image, authFor(spec.RegistryAuths, up.From.Image), lg); err != nil {
		return err
	}
	oldName, newName := m.network(spec.AppID)+"-upgrade-from", m.network(spec.AppID)+"-upgrade-to"
	bg := context.WithoutCancel(ctx)
	cleanup := func() {
		dc.ContainerRemove(bg, oldName, true)
		dc.ContainerRemove(bg, newName, true)
	}
	cleanup() // left by an interrupted attempt
	defer cleanup()

	start := func(name string, cfg docker.ContainerConfig, what string) (string, error) {
		id, err := dc.ContainerCreate(ctx, name, cfg)
		if err != nil {
			return "", fmt.Errorf("create the %s container: %w", what, err)
		}
		if err := dc.ContainerStart(ctx, id); err != nil {
			return "", fmt.Errorf("start the %s container: %w", what, err)
		}
		if err := m.waitContainer(ctx, dc, spec.AppID, 0, id, what+": ", lg); err != nil {
			lg.printf("Last output of the %s container:", what)
			lg.write(m.tail(bg, dc, id, 50))
			return "", err
		}
		return id, nil
	}
	lg.printf("Starting the old version alone, without a network, to dump it")
	oldID, err := start(oldName, m.dbTempConfig(spec, up.From), "Old version")
	if err != nil {
		return err
	}
	dir := pgUpgradeMount + "/" + up.To
	cfg := m.dbTempConfig(spec, svc)
	cfg.HostConfig.Mounts = slices.Clone(cfg.HostConfig.Mounts)
	cfg.HostConfig.Mounts[0].Target = pgUpgradeMount
	cfg.Env = slices.DeleteFunc(slices.Clone(cfg.Env), func(e string) bool {
		return strings.HasPrefix(e, "PGDATA=") || strings.HasPrefix(e, "POSTGRES_DB=")
	})
	// The dump creates the databases (and the user, but for the line
	// below); "postgres" exists in every cluster.
	cfg.Env = append(cfg.Env, "PGDATA="+dir+"/docker", "POSTGRES_DB=postgres")
	cfg.Entrypoint, cfg.Cmd = []string{"sh", "-c", pgUpgradeInit, dir}, nil
	if cfg.Healthcheck != nil {
		h := *cfg.Healthcheck
		h.Test = []string{"CMD", "pg_isready", "-q", "-h", "127.0.0.1", "-U", user, "-d", "postgres"}
		cfg.Healthcheck = &h
	}
	lg.printf("Starting PostgreSQL %s alone with a new cluster", up.To)
	newID, err := start(newName, cfg, "New version")
	if err != nil {
		return err
	}

	lg.printf("Copying the data to PostgreSQL %s", up.To)
	n, err := pgCopy(ctx, bin, m.socket, oldID, newID, user)
	if err != nil {
		return err
	}
	lg.printf("Copied %s of SQL", humanBytes(uint64(n)))
	// Stopped cleanly: the new cluster's files are complete on disk
	// before the service's container takes them over.
	if err := dc.ContainerStop(ctx, newID, stopTimeout(svc)); err != nil {
		return fmt.Errorf("stop the new version: %w", err)
	}
	return nil
}

// pgCopy pipes pg_dumpall of the old container into psql of the new one,
// leaving out the line that creates the user the new cluster already has.
// It returns the bytes copied.
func pgCopy(ctx context.Context, bin, socket, oldID, newID, user string) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	docker := func(args ...string) *exec.Cmd {
		return command(ctx, append([]string{bin, "--host", "unix://" + socket}, args...))
	}
	dump := docker("exec", oldID, "pg_dumpall", "--username", user, "--no-password")
	load := docker("exec", "-i", newID, "psql", "--username", user, "--dbname", "postgres", "--no-password",
		"--quiet", "--set", "ON_ERROR_STOP=1", "--output", "/dev/null")
	var dumpErr, loadErr tailBuffer
	dump.Stderr, load.Stderr, load.Stdout = &dumpErr, &loadErr, io.Discard
	out, err := dump.StdoutPipe()
	if err != nil {
		return 0, err
	}
	pr, pw := io.Pipe()
	load.Stdin = pr
	if err := load.Start(); err != nil {
		return 0, err
	}
	if err := dump.Start(); err != nil {
		pw.Close()
		load.Wait()
		return 0, err
	}
	var n int64
	copied := make(chan error, 1)
	go func() {
		var err error
		n, err = skipLine(pw, out, []byte("CREATE ROLE "+user+";\n"))
		pw.CloseWithError(err)
		copied <- err
	}()
	lerr := load.Wait()
	if lerr != nil {
		cancel()
		pr.CloseWithError(lerr)
	}
	cerr := <-copied
	derr := dump.Wait()
	switch {
	case derr != nil && lerr == nil:
		return n, fmt.Errorf("dump the old version: %v: %s", derr, dumpErr.String())
	case lerr != nil:
		return n, fmt.Errorf("load into the new version: %v: %s", lerr, loadErr.String())
	case cerr != nil:
		return n, cerr
	}
	return n, nil
}

// skipLine copies src to dst without the lines equal to line (which ends
// with a newline). Lines of any length pass through.
func skipLine(dst io.Writer, src io.Reader, line []byte) (int64, error) {
	r := bufio.NewReaderSize(src, 64<<10)
	var n int64
	atStart := true
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 && !(atStart && err == nil && bytes.Equal(chunk, line)) {
			w, werr := dst.Write(chunk)
			n += int64(w)
			if werr != nil {
				return n, werr
			}
		}
		switch {
		case err == nil:
			atStart = true
		case errors.Is(err, bufio.ErrBufferFull):
			atStart = false
		case errors.Is(err, io.EOF):
			return n, nil
		default:
			return n, err
		}
	}
}

// tailBuffer keeps the end of what is written (a command's errors).
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2000 {
		t.b = t.b[len(t.b)-1000:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return strings.TrimSpace(string(t.b)) }

// dropOldPostgres removes the old version's data from the volume once the
// new version runs. A failure leaves it there, which only costs space.
func (m *appsManager) dropOldPostgres(ctx context.Context, dc *docker.Client, spec agentproto.AppSpec, lg *deployLog) {
	up := spec.DBUpgrade
	bin, err := exec.LookPath("docker")
	if err == nil {
		if _, ierr := dc.ImageInspect(ctx, volumeHelperImage); ierr != nil {
			_, _, err = m.pull(ctx, dc, volumeHelperImage, nil, lg)
		}
	}
	if err == nil {
		vol := m.volume(spec.AppID, up.From.Mounts[0].Volume)
		var out []byte
		out, err = command(ctx, []string{bin, "--host", "unix://" + m.socket, "run", "--rm", "--network", "none", "--log-driver", "none",
			"--mount", "type=volume,source=" + vol + ",target=/v", volumeHelperImage, "sh", "-c", pgUpgradeDrop, up.To}).CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if err != nil {
		lg.printf("Warning: the old version's data was not removed from the volume: %v", err)
		return
	}
	lg.printf("Removed the old version's data from the volume")
}
