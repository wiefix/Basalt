package agentproto

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// Backup requests. The panel owns storages, schedules and history; the
// agent runs restic against the repository the panel describes.
const (
	// TypeBackupCheck opens a repository, creating it if needed.
	TypeBackupCheck = "backup_check"
	// TypeBackupRun backs a resource up; streams LogChunk frames.
	TypeBackupRun = "backup_run"
	// TypeBackupSnapshots lists a resource's snapshots.
	TypeBackupSnapshots = "backup_snapshots"
	// TypeBackupRestore restores snapshots; streams LogChunk frames.
	TypeBackupRestore = "backup_restore"
	// TypeBackupVerify checks a repository's integrity (restic check);
	// streams LogChunk frames.
	TypeBackupVerify = "backup_verify"
)

// Storage kinds.
const (
	StorageS3    = "s3"
	StorageSFTP  = "sftp"
	StorageLocal = "local"
)

// BackupRepo locates a restic repository and holds what opens it.
type BackupRepo struct {
	Kind string `json:"kind"`
	// S3 and compatible services.
	Endpoint  string `json:"endpoint,omitempty"` // https://host[:port]
	Bucket    string `json:"bucket,omitempty"`
	Region    string `json:"region,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	// SFTP (key authentication).
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	User   string `json:"user,omitempty"`
	SSHKey string `json:"ssh_key,omitempty"` // OpenSSH private key
	// Path is the prefix in the bucket, the directory on the SFTP host
	// (relative to its home unless absolute) or the local directory.
	Path     string `json:"path"`
	Password string `json:"password"` // repository password
}

var (
	bucketRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRE    = regexp.MustCompile(`^[a-z0-9-]{0,32}$`)
	tokenRE     = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`) // printable, no spaces
	repoPathRE  = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,256}$`)
	sftpUserRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,31}$`)
	sftpHostRE  = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,251}[a-zA-Z0-9])?$`)
	resourceRE  = regexp.MustCompile(`^(site|app)-[1-9][0-9]{0,18}$`)
	snapshotRE  = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	subsetRE    = regexp.MustCompile(`^([1-9][0-9]{0,3})/([1-9][0-9]{0,3})$`)
	dbNameRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	sqlNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	errBadValue = errors.New("invalid value")
)

// cleanRelPath reports whether p is a relative path without . or ..
// segments (empty is allowed).
func cleanRelPath(p string) bool {
	if p == "" {
		return true
	}
	if !repoPathRE.MatchString(p) || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Validate checks every field that ends up in a restic argument.
func (r *BackupRepo) Validate() error {
	if r.Password == "" || len(r.Password) > 256 || strings.ContainsAny(r.Password, "\x00\n") {
		return errors.New("invalid repository password")
	}
	switch r.Kind {
	case StorageS3:
		u, err := url.Parse(r.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(r.Endpoint, " \x00") {
			return errors.New("endpoint must be like https://s3.example.com (scheme and host only)")
		}
		if !bucketRE.MatchString(r.Bucket) {
			return errors.New("invalid bucket name")
		}
		if !regionRE.MatchString(r.Region) {
			return errors.New("invalid region")
		}
		if !tokenRE.MatchString(r.AccessKey) || !tokenRE.MatchString(r.SecretKey) {
			return errors.New("access key and secret key are required (printable, no spaces)")
		}
		if !cleanRelPath(strings.TrimPrefix(r.Path, "/")) {
			return errors.New("path in the bucket may use letters, digits, . _ - and /")
		}
	case StorageSFTP:
		if !sftpHostRE.MatchString(r.Host) && net.ParseIP(r.Host) == nil {
			return errors.New("invalid SFTP host")
		}
		if r.Port < 1 || r.Port > 65535 {
			return errors.New("invalid SFTP port")
		}
		if !sftpUserRE.MatchString(r.User) {
			return errors.New("invalid SFTP user")
		}
		if !strings.HasPrefix(r.SSHKey, "-----BEGIN OPENSSH PRIVATE KEY-----") || len(r.SSHKey) > 16<<10 {
			return errors.New("invalid SSH key")
		}
		if p := strings.TrimPrefix(r.Path, "/"); p == "" || !cleanRelPath(p) {
			return errors.New("SFTP path may use letters, digits, . _ - and /")
		}
	case StorageLocal:
		if !strings.HasPrefix(r.Path, "/") || r.Path == "/" || !cleanRelPath(r.Path[1:]) {
			return errors.New("local directory must be an absolute path such as /var/backups/basalt")
		}
	default:
		return fmt.Errorf("unknown storage kind %q", r.Kind)
	}
	return nil
}

// Item kinds.
const (
	ItemSiteFiles = "site_files" // a site's directory
	ItemMariaDB   = "mariadb"    // a database of the server's MariaDB
	ItemVolume    = "volume"     // a named volume of an app
	ItemDBDump    = "db_dump"    // a logical dump of a database app's service
)

// Engines a db_dump item can dump.
var dumpEngines = map[string]bool{"postgres": true, "mysql": true, "mariadb": true}

// BackupItem is one part of a resource; each becomes a snapshot.
type BackupItem struct {
	Kind     string `json:"kind"`
	SiteID   int64  `json:"site_id,omitempty"`
	AppID    int64  `json:"app_id,omitempty"`
	Volume   string `json:"volume,omitempty"`   // volume: the app's name for it
	Service  string `json:"service,omitempty"`  // db_dump: the service running the database
	Engine   string `json:"engine,omitempty"`   // db_dump: postgres, mysql or mariadb
	Database string `json:"database,omitempty"` // mariadb, db_dump
	User     string `json:"user,omitempty"`     // db_dump (PostgreSQL connects as it)
}

// validate checks an item and that it belongs to resource.
func (it BackupItem) validate(resource string) error {
	owner := fmt.Sprintf("site-%d", it.SiteID)
	if it.Kind == ItemVolume || it.Kind == ItemDBDump {
		owner = fmt.Sprintf("app-%d", it.AppID)
	}
	if it.Kind != ItemMariaDB && owner != resource {
		return errors.New("backup item of another resource")
	}
	switch it.Kind {
	case ItemSiteFiles:
		if it.SiteID <= 0 {
			return errBadValue
		}
	case ItemMariaDB:
		if !dbNameRE.MatchString(it.Database) {
			return fmt.Errorf("invalid database name %q", it.Database)
		}
	case ItemVolume:
		if it.AppID <= 0 || !ServiceNameRE.MatchString(it.Volume) {
			return fmt.Errorf("invalid volume %q", it.Volume)
		}
	case ItemDBDump:
		if it.AppID <= 0 || !ServiceNameRE.MatchString(it.Service) || !dumpEngines[it.Engine] ||
			!sqlNameRE.MatchString(it.Database) || !sqlNameRE.MatchString(it.User) {
			return errors.New("invalid database dump")
		}
	default:
		return fmt.Errorf("unknown backup item %q", it.Kind)
	}
	return nil
}

// Keep is a retention policy (see restic forget): the newest Last
// snapshots and the newest of each of the last Daily days, Weekly weeks
// and Monthly months.
type Keep struct {
	Last    int `json:"last"`
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}

func (k Keep) Validate() error {
	for _, n := range []int{k.Last, k.Daily, k.Weekly, k.Monthly} {
		if n < 0 || n > 1000 {
			return errors.New("retention counts must be between 0 and 1000")
		}
	}
	if k.Last < 1 {
		return errors.New("keep at least the last snapshot")
	}
	return nil
}

// BackupRequest backs a resource ("site-5") up into Repo.
type BackupRequest struct {
	Repo     BackupRepo   `json:"repo"`
	Resource string       `json:"resource"`
	Items    []BackupItem `json:"items"`
	Keep     Keep         `json:"keep"`
	// Prune frees the space of forgotten snapshots (slow; not every run).
	Prune bool `json:"prune,omitempty"`
}

func (r *BackupRequest) Validate() error {
	if err := r.Repo.Validate(); err != nil {
		return err
	}
	if !resourceRE.MatchString(r.Resource) {
		return errors.New("invalid resource")
	}
	if len(r.Items) == 0 || len(r.Items) > 200 {
		return errors.New("nothing to back up")
	}
	for _, it := range r.Items {
		if err := it.validate(r.Resource); err != nil {
			return err
		}
	}
	return r.Keep.Validate()
}

// BackupSnapshot is one snapshot a backup made.
type BackupSnapshot struct {
	Item      BackupItem `json:"item"`
	ID        string     `json:"id"`
	Bytes     uint64     `json:"bytes"`      // processed
	DataAdded uint64     `json:"data_added"` // new data stored
}

type BackupResponse struct {
	OK        bool             `json:"ok"`
	Error     string           `json:"error,omitempty"`
	Log       string           `json:"log"`
	Snapshots []BackupSnapshot `json:"snapshots,omitempty"`
}

// SnapshotsRequest lists the snapshots of a resource.
type SnapshotsRequest struct {
	Repo     BackupRepo `json:"repo"`
	Resource string     `json:"resource"`
}

func (r *SnapshotsRequest) Validate() error {
	if err := r.Repo.Validate(); err != nil {
		return err
	}
	if !resourceRE.MatchString(r.Resource) {
		return errors.New("invalid resource")
	}
	return nil
}

// SnapshotInfo describes a snapshot of a resource.
type SnapshotInfo struct {
	ID    string     `json:"id"`
	Time  time.Time  `json:"time"`
	Item  BackupItem `json:"item"`
	Bytes uint64     `json:"bytes"`
}

type SnapshotsResponse struct {
	Snapshots []SnapshotInfo `json:"snapshots"`
}

// RestoreItem restores one snapshot of an item.
type RestoreItem struct {
	Item     BackupItem `json:"item"`
	Snapshot string     `json:"snapshot"`
}

// RestoreRequest puts items of a resource back as they were in snapshots:
// site files exactly (newer files are deleted), databases from their dumps.
type RestoreRequest struct {
	Repo     BackupRepo    `json:"repo"`
	Resource string        `json:"resource"`
	Items    []RestoreItem `json:"items"`
}

func (r *RestoreRequest) Validate() error {
	if err := r.Repo.Validate(); err != nil {
		return err
	}
	if !resourceRE.MatchString(r.Resource) {
		return errors.New("invalid resource")
	}
	if len(r.Items) == 0 || len(r.Items) > 200 {
		return errors.New("nothing to restore")
	}
	for _, it := range r.Items {
		if err := it.Item.validate(r.Resource); err != nil {
			return err
		}
		if !snapshotRE.MatchString(it.Snapshot) {
			return errors.New("invalid snapshot id")
		}
	}
	return nil
}

type RestoreResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Log   string `json:"log"`
}

// VerifyRequest checks the integrity of a repository: its structure and,
// with ReadData ("n/t": the n-th of t parts), that part of its data can be
// read back and decrypted.
type VerifyRequest struct {
	Repo     BackupRepo `json:"repo"`
	ReadData string     `json:"read_data,omitempty"`
}

func (r *VerifyRequest) Validate() error {
	if err := r.Repo.Validate(); err != nil {
		return err
	}
	if r.ReadData == "" {
		return nil
	}
	m := subsetRE.FindStringSubmatch(r.ReadData)
	if m == nil || len(m[1]) > len(m[2]) || (len(m[1]) == len(m[2]) && m[1] > m[2]) {
		return errors.New("read_data must be n/t with 1 <= n <= t")
	}
	return nil
}

type VerifyResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Log   string `json:"log"`
	// Empty: the repository holds no backups yet (nothing was checked).
	Empty bool `json:"empty,omitempty"`
}
