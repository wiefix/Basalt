package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/restic"
)

// volumeHelperImage reads and writes volumes for backups: a throwaway
// container with the volume mounted streams it as tar. It runs under the
// daemon's user namespaces like the app, so file owners come out right.
const volumeHelperImage = "busybox:1.37"

// Commands run inside database containers. The root password comes from
// the container's own environment, so it never passes through the agent;
// the database name is an argument ($0), not part of the script.
var (
	mysqlDump    = `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump -uroot --single-transaction --quick --routines --events --triggers --hex-blob --default-character-set=utf8mb4 "$0"`
	mysqlLoad    = `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot --default-character-set=utf8mb4 --binary-mode "$0"`
	mariadbDump  = `MYSQL_PWD="$MARIADB_ROOT_PASSWORD" exec mariadb-dump -uroot --single-transaction --quick --routines --events --triggers --hex-blob --default-character-set=utf8mb4 "$0"`
	mariadbLoad  = `MYSQL_PWD="$MARIADB_ROOT_PASSWORD" exec mariadb -uroot --default-character-set=utf8mb4 --binary-mode "$0"`
	errNoRunning = errors.New("the database is not running; deploy it first")
)

func volumeFile(name string) string { return "volume-" + name + ".tar" }
func dumpFileOf(db string) string   { return "dump-" + db + ".sql" }

func (b *backupManager) dockerCLI() (string, error) {
	if b.apps == nil || b.apps.dc == nil {
		return "", errors.New("Docker is not available on this server")
	}
	bin, err := exec.LookPath("docker")
	if err != nil {
		return "", errors.New("the docker command is not installed")
	}
	return bin, nil
}

// dockerArgs is a docker command line (bin first) for the local daemon.
func (b *backupManager) dockerArgs(bin string, args ...string) []string {
	return append([]string{bin, "--host", "unix://" + b.apps.socket}, args...)
}

func command(ctx context.Context, argv []string) *exec.Cmd {
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}

// helperImage makes sure the volume helper image is present.
func (b *backupManager) helperImage(ctx context.Context, lg *deployLog) error {
	if _, err := b.apps.dc.ImageInspect(ctx, volumeHelperImage); err == nil {
		return nil
	}
	_, _, err := b.apps.pull(ctx, b.apps.dc, volumeHelperImage, nil, lg)
	return err
}

// appBackupOptions says how one part of an app is backed up: the command
// whose output becomes the snapshot.
func (b *backupManager) appBackupOptions(ctx context.Context, it agentproto.BackupItem, lg *deployLog) (cmd []string, file string, err error) {
	bin, err := b.dockerCLI()
	if err != nil {
		return nil, "", err
	}
	switch it.Kind {
	case agentproto.ItemVolume:
		if err := b.helperImage(ctx, lg); err != nil {
			return nil, "", err
		}
		vol := b.apps.volume(it.AppID, it.Volume)
		if _, err := b.apps.dc.VolumeInspect(ctx, vol); errors.Is(err, docker.ErrNotFound) {
			return nil, "", fmt.Errorf("volume %s does not exist yet (deploy the app first)", it.Volume)
		}
		lg.printf("Backing up volume %s", it.Volume)
		return b.dockerArgs(bin, "run", "--rm", "--network", "none", "--log-driver", "none",
			"--mount", "type=volume,source="+vol+",target=/data,readonly", volumeHelperImage,
			"tar", "-C", "/data", "-cf", "-", "."), volumeFile(it.Volume), nil
	case agentproto.ItemDBDump:
		c, err := b.apps.current(ctx, b.apps.dc, it.AppID, it.Service)
		if err != nil {
			return nil, "", err
		}
		if c == nil || c.State != "running" {
			return nil, "", errNoRunning
		}
		lg.printf("Dumping database %s", it.Database)
		var inner []string
		switch it.Engine {
		case "postgres":
			inner = []string{"pg_dump", "--username", it.User, "--dbname", it.Database, "--clean", "--if-exists", "--no-password"}
		case "mysql":
			inner = []string{"sh", "-c", mysqlDump, it.Database}
		case "mariadb":
			inner = []string{"sh", "-c", mariadbDump, it.Database}
		}
		return b.dockerArgs(bin, append([]string{"exec", c.ID}, inner...)...), dumpFileOf(it.Database), nil
	}
	return nil, "", fmt.Errorf("unknown item %q", it.Kind)
}

// restoreAppItem puts one part of an app back.
func (b *backupManager) restoreAppItem(ctx context.Context, r *restic.Runner, repo restic.Repo, it agentproto.RestoreItem, lg *deployLog) error {
	bin, err := b.dockerCLI()
	if err != nil {
		return err
	}
	dc := b.apps.dc
	switch it.Item.Kind {
	case agentproto.ItemVolume:
		if err := b.helperImage(ctx, lg); err != nil {
			return err
		}
		vol := b.apps.volume(it.Item.AppID, it.Item.Volume)
		if err := dc.VolumeCreate(ctx, vol, b.apps.appLabels(it.Item.AppID, labelVolume, it.Item.Volume)); err != nil {
			return err
		}
		// Containers using the volume stop while it is replaced.
		stopped, err := b.stopUsers(ctx, it.Item.AppID, vol, lg)
		defer func() {
			for _, id := range stopped {
				if err := dc.ContainerStart(context.WithoutCancel(ctx), id); err != nil {
					lg.printf("Could not start a container again: %v", err)
				}
			}
			if len(stopped) > 0 {
				lg.printf("Started %d container(s) again", len(stopped))
				b.apps.refresh(context.WithoutCancel(ctx))
			}
		}()
		if err != nil {
			return err
		}
		lg.printf("Restoring volume %s from snapshot %s", it.Item.Volume, it.Snapshot[:8])
		cmd := command(ctx, b.dockerArgs(bin, "run", "--rm", "-i", "--network", "none", "--log-driver", "none",
			"--mount", "type=volume,source="+vol+",target=/data", volumeHelperImage,
			"sh", "-c", "find /data -mindepth 1 -delete && tar -C /data -xf -"))
		return pipeSnapshot(ctx, r, repo, it.Snapshot, "/"+volumeFile(it.Item.Volume), cmd)
	case agentproto.ItemDBDump:
		c, err := b.apps.current(ctx, dc, it.Item.AppID, it.Item.Service)
		if err != nil {
			return err
		}
		if c == nil || c.State != "running" {
			return errNoRunning
		}
		lg.printf("Restoring database %s from snapshot %s", it.Item.Database, it.Snapshot[:8])
		var inner []string
		switch it.Item.Engine {
		case "postgres":
			inner = []string{"psql", "--username", it.Item.User, "--dbname", it.Item.Database, "--no-password",
				"--quiet", "--single-transaction", "--set", "ON_ERROR_STOP=1"}
		case "mysql":
			inner = []string{"sh", "-c", mysqlLoad, it.Item.Database}
		case "mariadb":
			inner = []string{"sh", "-c", mariadbLoad, it.Item.Database}
		}
		cmd := command(ctx, b.dockerArgs(bin, append([]string{"exec", "-i", c.ID}, inner...)...))
		return pipeSnapshot(ctx, r, repo, it.Snapshot, "/"+dumpFileOf(it.Item.Database), cmd)
	}
	return fmt.Errorf("unknown item %q", it.Item.Kind)
}

// stopUsers stops the app's running containers that mount vol and returns
// their IDs.
func (b *backupManager) stopUsers(ctx context.Context, appID int64, vol string, lg *deployLog) ([]string, error) {
	list, err := b.apps.containers(ctx, b.apps.dc, appID)
	if err != nil {
		return nil, err
	}
	var stopped []string
	for _, c := range list {
		if c.State != "running" {
			continue
		}
		info, err := b.apps.dc.ContainerInspect(ctx, c.ID)
		if err != nil {
			return stopped, err
		}
		for _, m := range info.HostConfig.Mounts {
			if m.Source == vol {
				lg.printf("Stopping %s while its volume is restored", strings.TrimPrefix(info.Name, "/"))
				if err := b.apps.dc.ContainerStop(ctx, c.ID, appStopTimeout); err != nil {
					return stopped, err
				}
				stopped = append(stopped, c.ID)
				break
			}
		}
	}
	return stopped, nil
}

// pipeSnapshot feeds a file of a snapshot to cmd's standard input.
func pipeSnapshot(ctx context.Context, r *restic.Runner, repo restic.Repo, snapshot, file string, cmd *exec.Cmd) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	cmd.Stdin = pr
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	dumpErr := r.Dump(ctx, repo, snapshot, file, pw)
	pw.CloseWithError(dumpErr)
	cmdErr := cmd.Wait()
	if dumpErr != nil {
		return fmt.Errorf("read the backup: %w", dumpErr)
	}
	if cmdErr != nil {
		msg := strings.TrimSpace(out.String())
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		return fmt.Errorf("%v: %s", cmdErr, msg)
	}
	return nil
}

// Changing a database user's password inside its container: the SQL goes
// in on standard input, MySQL and MariaDB connect as root with the root
// password from the container's own environment.
const (
	mysqlRoot   = `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot`
	mariadbRoot = `MYSQL_PWD="$MARIADB_ROOT_PASSWORD" exec mariadb -uroot`
)

func (b *backupManager) setDBPassword(ctx context.Context, req agentproto.ContainerDBPassword) error {
	if err := req.Validate(); err != nil {
		return err
	}
	bin, err := b.dockerCLI()
	if err != nil {
		return err
	}
	c, err := b.apps.current(ctx, b.apps.dc, req.AppID, req.Service)
	if err != nil {
		return err
	}
	if c == nil || c.State != "running" {
		return errors.New("the database is not running: start it first")
	}
	// Names and the password match fixed alphabets (Validate): nothing in
	// the SQL needs escaping.
	var inner []string
	var sql string
	switch req.Engine {
	case "postgres":
		inner = []string{"psql", "--username", req.User, "--dbname", req.Database, "--no-password", "--quiet", "--set", "ON_ERROR_STOP=1"}
		sql = fmt.Sprintf(`ALTER ROLE "%s" WITH PASSWORD '%s';`, req.User, req.Password)
	case "mysql", "mariadb":
		script := mysqlRoot
		if req.Engine == "mariadb" {
			script = mariadbRoot
		}
		inner = []string{"sh", "-c", script}
		sql = fmt.Sprintf("ALTER USER '%s'@'%%' IDENTIFIED BY '%s';\nFLUSH PRIVILEGES;\n", req.User, req.Password)
	}
	cmd := command(ctx, b.dockerArgs(bin, append([]string{"exec", "-i", c.ID}, inner...)...))
	cmd.Stdin = strings.NewReader(sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return fmt.Errorf("change the password: %s", msg)
	}
	return nil
}
