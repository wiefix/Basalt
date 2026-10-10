package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/mariadb"
	"github.com/wiefix/basalt/internal/restic"
)

// backupHost is the host name snapshots are recorded under. Each server has
// its own repository, so it only needs to stay the same.
const backupHost = "basalt"

const maxConcurrentBackups = 2

// backupManager runs backups and restores with restic.
type backupManager struct {
	log     *slog.Logger
	binary  string
	dataDir string
	sites   *sitesManager
	mysql   string // MariaDB socket
	apps    *appsManager

	slots chan struct{}
	lmu   sync.Mutex
	locks map[string]*sync.Mutex // by resource
}

func newBackupManager(cfg Config, sites *sitesManager, apps *appsManager, log *slog.Logger) *backupManager {
	return &backupManager{log: log, binary: cfg.Restic, dataDir: cfg.DataDir, sites: sites, apps: apps, mysql: cfg.MySQLSocket,
		slots: make(chan struct{}, maxConcurrentBackups), locks: map[string]*sync.Mutex{}}
}

func (b *backupManager) lock(resource string) func() {
	b.lmu.Lock()
	l := b.locks[resource]
	if l == nil {
		l = &sync.Mutex{}
		b.locks[resource] = l
	}
	b.lmu.Unlock()
	l.Lock()
	return l.Unlock
}

// Version reports the restic version, "" when backups cannot run here.
func (b *backupManager) Version(ctx context.Context) string {
	if b.binary == "" {
		return ""
	}
	r := b.runner(nil)
	v, err := r.Version(ctx)
	if err != nil {
		if _, lookErr := exec.LookPath(b.binary); lookErr == nil {
			b.log.Warn("restic not usable", "err", err)
		}
		return ""
	}
	return v
}

func (b *backupManager) runner(line func(string)) *restic.Runner {
	return &restic.Runner{Binary: b.binary, CacheDir: filepath.Join(b.dataDir, "restic-cache"), Line: line}
}

// repo turns the panel's description into what restic opens.
func (b *backupManager) repo(spec agentproto.BackupRepo) (restic.Repo, error) {
	if err := spec.Validate(); err != nil {
		return restic.Repo{}, err
	}
	if b.binary == "" {
		return restic.Repo{}, errors.New("backups are disabled on this server (no restic)")
	}
	if err := os.MkdirAll(filepath.Join(b.dataDir, "restic-cache"), 0o700); err != nil {
		return restic.Repo{}, err
	}
	r := restic.Repo{Password: spec.Password, Env: map[string]string{}}
	switch spec.Kind {
	case agentproto.StorageS3:
		r.Location = "s3:" + strings.TrimSuffix(spec.Endpoint, "/") + "/" + spec.Bucket
		if p := strings.Trim(spec.Path, "/"); p != "" {
			r.Location += "/" + p
		}
		r.Env["AWS_ACCESS_KEY_ID"], r.Env["AWS_SECRET_ACCESS_KEY"] = spec.AccessKey, spec.SecretKey
		if spec.Region != "" {
			r.Env["AWS_DEFAULT_REGION"] = spec.Region
		}
	case agentproto.StorageSFTP:
		key, knownHosts, err := b.sshFiles(spec)
		if err != nil {
			return restic.Repo{}, err
		}
		r.Location = "sftp:" + spec.User + "@" + spec.Host + ":" + spec.Path
		// restic splits sftp.command on spaces; every part here is free of them.
		r.Options = []string{"sftp.command=ssh -i " + key + " -p " + strconv.Itoa(spec.Port) +
			" -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new" +
			" -o UserKnownHostsFile=" + knownHosts + " -o ConnectTimeout=20 -o ServerAliveInterval=30" +
			" " + spec.User + "@" + spec.Host + " -s sftp"}
	case agentproto.StorageLocal:
		if err := b.localPathAllowed(spec.Path); err != nil {
			return restic.Repo{}, err
		}
		if err := os.MkdirAll(spec.Path, 0o700); err != nil {
			return restic.Repo{}, fmt.Errorf("create the backup directory: %w", err)
		}
		r.Location = spec.Path
	}
	return r, nil
}

// localPathAllowed keeps local repositories out of places other users
// reach (site directories) or Basalt manages.
func (b *backupManager) localPathAllowed(p string) error {
	for _, forbidden := range []string{b.sites.root, b.dataDir} {
		if forbidden == "" {
			continue
		}
		abs, err := filepath.Abs(forbidden)
		if err != nil {
			continue
		}
		if p == abs || strings.HasPrefix(p+"/", abs+"/") || strings.HasPrefix(abs+"/", p+"/") {
			return errors.New("the backup directory must not contain or be inside the sites or the agent's data directory")
		}
	}
	for _, sys := range []string{"/proc", "/sys", "/dev", "/run", "/etc", "/bin", "/sbin", "/usr", "/lib", "/boot"} {
		if p == sys || strings.HasPrefix(p, sys+"/") {
			return fmt.Errorf("the backup directory cannot be under %s", sys)
		}
	}
	return nil
}

// sshFiles writes the storage's SSH key and returns it with the storage's
// known_hosts file (the host key is accepted on first use).
func (b *backupManager) sshFiles(spec agentproto.BackupRepo) (key, knownHosts string, err error) {
	dir := filepath.Join(b.dataDir, "backup-ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	if strings.ContainsAny(dir, " \t") {
		return "", "", errors.New("the agent's data directory must not contain spaces for SFTP backups")
	}
	sum := sha256.Sum256([]byte(spec.SSHKey))
	key = filepath.Join(dir, hex.EncodeToString(sum[:8])+".key")
	if _, err := os.Stat(key); err != nil {
		data := spec.SSHKey
		if !strings.HasSuffix(data, "\n") {
			data += "\n"
		}
		tmp := key + ".tmp"
		if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
			return "", "", err
		}
		if err := os.Rename(tmp, key); err != nil {
			return "", "", err
		}
	}
	return key, filepath.Join(dir, "known_hosts_"+spec.Host+"_"+strconv.Itoa(spec.Port)), nil
}

// Check opens the repository, creating it if needed.
func (b *backupManager) Check(ctx context.Context, spec agentproto.BackupRepo) error {
	repo, err := b.repo(spec)
	if err != nil {
		return err
	}
	_, err = b.runner(nil).Init(ctx, repo)
	return err
}

func itemTags(resource string, it agentproto.BackupItem) []string {
	part := "files"
	switch it.Kind {
	case agentproto.ItemMariaDB:
		part = "mariadb-" + it.Database
	case agentproto.ItemVolume:
		part = "volume-" + it.Volume
	case agentproto.ItemDBDump:
		part = "dump-" + it.Database
	}
	return []string{"res=" + resource, "part=" + part}
}

// dumpFile names a database dump in its snapshot (flat: restic before
// 0.19 rejects directories in --stdin-filename).
func dumpFile(database string) string { return "mariadb-" + database + ".sql" }

// Run backs the items up, one snapshot each, then applies the retention
// policy to the resource's snapshots.
func (b *backupManager) Run(ctx context.Context, req agentproto.BackupRequest, stream func(any) error) agentproto.BackupResponse {
	lg := deployLog{stream: stream}
	var snaps []agentproto.BackupSnapshot
	fail := func(err error) agentproto.BackupResponse {
		lg.forceWrite(fmt.Sprintf("%s Backup failed: %v\n", time.Now().UTC().Format("15:04:05"), err))
		return agentproto.BackupResponse{Error: err.Error(), Log: lg.String(), Snapshots: snaps}
	}
	if err := req.Validate(); err != nil {
		return fail(err)
	}
	repo, err := b.repo(req.Repo)
	if err != nil {
		return fail(err)
	}
	b.slots <- struct{}{}
	defer func() { <-b.slots }()
	defer b.lock(req.Resource)()

	r := b.runner(func(s string) { lg.printf("restic: %s", s) })
	lg.printf("Opening the repository")
	if created, err := r.Init(ctx, repo); err != nil {
		return fail(err)
	} else if created {
		lg.printf("Created a new repository")
	}
	for _, it := range req.Items {
		o := restic.BackupOptions{Host: backupHost, Tags: itemTags(req.Resource, it)}
		switch it.Kind {
		case agentproto.ItemSiteFiles:
			dir, err := b.sites.dir(it.SiteID)
			if err != nil {
				return fail(err)
			}
			lg.printf("Backing up the site's files")
			o.Paths = []string{dir}
			// Temporary files and the file manager's unfinished uploads.
			// tmp/ itself stays, so a restore keeps the directory PHP uses.
			o.Excludes = []string{filepath.Join(dir, "tmp", "*"), filepath.Join(dir, "**", ".basalt-upload-*")}
		case agentproto.ItemMariaDB:
			cmd, err := b.dumpCommand(it.Database)
			if err != nil {
				return fail(err)
			}
			lg.printf("Dumping database %s", it.Database)
			o.StdinCommand, o.StdinFilename = cmd, dumpFile(it.Database)
		default:
			cmd, file, err := b.appBackupOptions(ctx, it, &lg)
			if err != nil {
				return fail(err)
			}
			o.StdinCommand, o.StdinFilename = cmd, file
		}
		s, err := r.Backup(ctx, repo, o)
		if err != nil {
			return fail(err)
		}
		if s.Incomplete {
			lg.printf("Warning: some files could not be read; the snapshot holds the rest")
		}
		lg.printf("Snapshot %s: %s processed, %s new", s.SnapshotID[:8], humanBytes(s.BytesProcessed), humanBytes(s.DataAdded))
		snaps = append(snaps, agentproto.BackupSnapshot{Item: it, ID: s.SnapshotID, Bytes: s.BytesProcessed, DataAdded: s.DataAdded})
	}
	lg.printf("Removing snapshots the retention policy does not keep")
	if err := r.Forget(ctx, repo, backupHost, []string{"res=" + req.Resource}, restic.Keep(req.Keep)); err != nil {
		return fail(err)
	}
	if req.Prune {
		lg.printf("Freeing the space of removed snapshots")
		if err := r.Prune(ctx, repo); err != nil {
			return fail(err)
		}
	}
	lg.printf("Backup finished")
	return agentproto.BackupResponse{OK: true, Log: lg.String(), Snapshots: snaps}
}

// dumpCommand dumps a database consistently without locking its tables.
func (b *backupManager) dumpCommand(name string) ([]string, error) {
	if !mariadb.ValidName(name) {
		return nil, fmt.Errorf("invalid database name %q", name)
	}
	if b.mysql == "" || !mariadb.Available(b.mysql) {
		return nil, errors.New("MariaDB is not available on this server")
	}
	bin, err := lookFirst("mariadb-dump", "mysqldump")
	if err != nil {
		return nil, err
	}
	return []string{bin, "--socket=" + b.mysql, "--user=root", "--single-transaction", "--quick", "--routines",
		"--events", "--triggers", "--hex-blob", "--default-character-set=utf8mb4", name}, nil
}

func lookFirst(names ...string) (string, error) {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found", names[0])
}

// Snapshots lists a resource's snapshots, newest first.
func (b *backupManager) Snapshots(ctx context.Context, req agentproto.SnapshotsRequest) (agentproto.SnapshotsResponse, error) {
	if err := req.Validate(); err != nil {
		return agentproto.SnapshotsResponse{}, err
	}
	repo, err := b.repo(req.Repo)
	if err != nil {
		return agentproto.SnapshotsResponse{}, err
	}
	list, err := b.runner(nil).Snapshots(ctx, repo, backupHost, []string{"res=" + req.Resource})
	if errors.Is(err, restic.ErrNoRepository) {
		return agentproto.SnapshotsResponse{Snapshots: []agentproto.SnapshotInfo{}}, nil
	}
	if err != nil {
		return agentproto.SnapshotsResponse{}, err
	}
	out := agentproto.SnapshotsResponse{Snapshots: make([]agentproto.SnapshotInfo, 0, len(list))}
	for i := len(list) - 1; i >= 0; i-- {
		s := list[i]
		info := agentproto.SnapshotInfo{ID: s.ID, Time: s.Time}
		for _, t := range s.Tags {
			part, ok := strings.CutPrefix(t, "part=")
			switch {
			case !ok:
			case part == "files":
				info.Item.Kind = agentproto.ItemSiteFiles
			case strings.HasPrefix(part, "mariadb-"):
				info.Item.Kind, info.Item.Database = agentproto.ItemMariaDB, strings.TrimPrefix(part, "mariadb-")
			case strings.HasPrefix(part, "volume-"):
				info.Item.Kind, info.Item.Volume = agentproto.ItemVolume, strings.TrimPrefix(part, "volume-")
			case strings.HasPrefix(part, "dump-"):
				info.Item.Kind, info.Item.Database = agentproto.ItemDBDump, strings.TrimPrefix(part, "dump-")
			}
		}
		if s.Summary != nil {
			info.Bytes = s.Summary.TotalBytes
		}
		out.Snapshots = append(out.Snapshots, info)
	}
	return out, nil
}

// Restore puts each item back as it was in its snapshot.
func (b *backupManager) Restore(ctx context.Context, req agentproto.RestoreRequest, stream func(any) error) agentproto.RestoreResponse {
	lg := deployLog{stream: stream}
	fail := func(err error) agentproto.RestoreResponse {
		lg.forceWrite(fmt.Sprintf("%s Restore failed: %v\n", time.Now().UTC().Format("15:04:05"), err))
		return agentproto.RestoreResponse{Error: err.Error(), Log: lg.String()}
	}
	if err := req.Validate(); err != nil {
		return fail(err)
	}
	repo, err := b.repo(req.Repo)
	if err != nil {
		return fail(err)
	}
	b.slots <- struct{}{}
	defer func() { <-b.slots }()
	defer b.lock(req.Resource)()
	r := b.runner(func(s string) { lg.printf("restic: %s", s) })
	for _, it := range req.Items {
		switch it.Item.Kind {
		case agentproto.ItemSiteFiles:
			dir, err := b.sites.dir(it.Item.SiteID)
			if err != nil {
				return fail(err)
			}
			lg.printf("Restoring the site's files from snapshot %s", it.Snapshot[:8])
			if err := r.RestoreTree(ctx, repo, it.Snapshot, dir, dir); err != nil {
				return fail(err)
			}
		case agentproto.ItemMariaDB:
			lg.printf("Restoring database %s from snapshot %s", it.Item.Database, it.Snapshot[:8])
			if err := b.restoreDatabase(ctx, r, repo, it); err != nil {
				return fail(err)
			}
		default:
			if err := b.restoreAppItem(ctx, r, repo, it, &lg); err != nil {
				return fail(err)
			}
		}
	}
	lg.printf("Restore finished")
	return agentproto.RestoreResponse{OK: true, Log: lg.String()}
}

// restoreDatabase feeds a dump from the repository to the MariaDB client.
func (b *backupManager) restoreDatabase(ctx context.Context, r *restic.Runner, repo restic.Repo, it agentproto.RestoreItem) error {
	if b.mysql == "" || !mariadb.Available(b.mysql) {
		return errors.New("MariaDB is not available on this server")
	}
	bin, err := lookFirst("mariadb", "mysql")
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "--socket="+b.mysql, "--user=root", "--default-character-set=utf8mb4",
		"--binary-mode", it.Item.Database)
	if err := pipeSnapshot(ctx, r, repo, it.Snapshot, "/"+dumpFile(it.Item.Database), cmd); err != nil {
		return fmt.Errorf("load the dump: %w", err)
	}
	return nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
