package agentproto

import (
	"fmt"
	"time"

	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/sftpd"
)

// HTTP paths on the agent listener.
const (
	PathEnroll  = "/agent/v1/enroll"
	PathConnect = "/agent/v1/connect"
)

// Message types.
const (
	// agent -> server notifications
	TypeHello   = "hello"
	TypeMetrics = "metrics"
	// agent -> server requests
	TypeRenewCert = "renew_cert"
	// server -> agent requests
	TypePing       = "ping"
	TypeProxyApply = "proxy_apply"
	TypeSiteEnsure = "site_ensure"
	TypeSiteDelete = "site_delete"
	TypeFiles      = "files"
	TypeSFTPApply  = "sftp_apply"
	TypeDatabase   = "database"
	TypeAppDeploy  = "app_deploy" // streams LogChunk frames when asked to
	TypeAppDelete  = "app_delete"
	TypeAppStatus  = "app_status"
	TypeAppLogs    = "app_logs"
	// TypeAppLogsFollow streams LogChunk frames of the app's current
	// container (and its successors) until cancelled.
	TypeAppLogsFollow = "app_logs_follow"
	TypeAppsSync      = "apps_sync"
	// TypeAppAttach sets which other apps' networks an app's containers
	// join (a database reachable from apps).
	TypeAppAttach = "app_attach"
)

// EnrollRequest is posted once, authenticated by the one-time token.
// The private key never leaves the agent; only the CSR is sent.
type EnrollRequest struct {
	Token string `json:"token"`
	CSR   []byte `json:"csr"` // DER
}

type EnrollResponse struct {
	ServerID int64  `json:"server_id"`
	Cert     []byte `json:"cert"`    // DER
	CACert   []byte `json:"ca_cert"` // DER
}

// Hello is the first message after connecting.
type Hello struct {
	AgentVersion string `json:"agent_version"`
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Kernel       string `json:"kernel"`
	Distro       string `json:"distro"`
	CPUCount     int    `json:"cpu_count"`
	MemTotal     uint64 `json:"mem_total"`
	// SFTP endpoint, zero port when disabled. The fingerprint (SHA256:...)
	// lets users verify the host on first connect.
	SFTPPort        int    `json:"sftp_port,omitempty"`
	SFTPFingerprint string `json:"sftp_fingerprint,omitempty"`
	// PHPVersions installed on the host, newest first.
	PHPVersions []string `json:"php_versions,omitempty"`
	// Isolated is true when sites run as separate system users.
	Isolated bool `json:"isolated,omitempty"`
	// MariaDB is the version of the local database server ("" if absent).
	MariaDB string `json:"mariadb,omitempty"`
	// Docker is the version of the local Docker Engine ("" if absent or
	// too old for the API Basalt uses).
	Docker string `json:"docker,omitempty"`
}

type Metrics struct {
	At        time.Time `json:"at"`
	CPUPct    float64   `json:"cpu_pct"`
	MemUsed   uint64    `json:"mem_used"`
	MemTotal  uint64    `json:"mem_total"`
	DiskUsed  uint64    `json:"disk_used"`
	DiskTotal uint64    `json:"disk_total"`
	Load1     float64   `json:"load1"`
	Load5     float64   `json:"load5"`
	Load15    float64   `json:"load15"`
	Uptime    uint64    `json:"uptime"` // seconds
}

type RenewCertRequest struct {
	CSR []byte `json:"csr"` // DER
}

type RenewCertResponse struct {
	Cert []byte `json:"cert"` // DER
}

type PingResponse struct {
	AgentTime time.Time `json:"agent_time"`
}

// ProxyApply carries the complete desired proxy state for the host.
type ProxyApply = proxy.Desired

type ProxyApplyResponse struct {
	ConfigID string `json:"config_id"`
}

type SiteRequest struct {
	SiteID int64 `json:"site_id"`
	// PHPVersion enables a PHP-FPM pool for the site ("" = static only).
	PHPVersion string `json:"php_version,omitempty"`
}

// File-manager operations.
const (
	FilesList         = "list"
	FilesStat         = "stat"
	FilesRead         = "read"
	FilesWrite        = "write"
	FilesMkdir        = "mkdir"
	FilesRename       = "rename"
	FilesDelete       = "delete"
	FilesChmod        = "chmod"
	FilesUploadBegin  = "upload_begin"
	FilesUploadChunk  = "upload_chunk"
	FilesUploadCommit = "upload_commit"
	FilesUploadAbort  = "upload_abort"
	FilesExtract      = "extract"
	FilesCompress     = "compress"
)

type FilesRequest struct {
	SiteID    int64    `json:"site_id"`
	Op        string   `json:"op"`
	Path      string   `json:"path,omitempty"`
	To        string   `json:"to,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	Offset    int64    `json:"offset,omitempty"`
	Length    int      `json:"length,omitempty"`
	Data      []byte   `json:"data,omitempty"`
	UploadID  string   `json:"upload_id,omitempty"`
	Mode      uint32   `json:"mode,omitempty"`
	Overwrite bool     `json:"overwrite,omitempty"`
}

type FilesResponse struct {
	Entries  []files.Entry        `json:"entries,omitempty"`
	Entry    *files.Entry         `json:"entry,omitempty"`
	Data     []byte               `json:"data,omitempty"`
	Size     int64                `json:"size,omitempty"`
	UploadID string               `json:"upload_id,omitempty"`
	Extract  *files.ExtractResult `json:"extract,omitempty"`
}

// SFTPApply is the complete set of SFTP accounts for the host.
type SFTPApply struct {
	Revision int64        `json:"revision"`
	Users    []sftpd.User `json:"users"`
}

// Database operations.
const (
	DBCreate   = "create"
	DBDrop     = "drop"
	DBPassword = "password"
)

type DatabaseRequest struct {
	Op       string `json:"op"`
	Name     string `json:"name"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
}

// AppSpec is everything needed to run one deployment of an app. The agent
// derives container, network and volume names from the IDs.
type AppSpec struct {
	AppID        int64             `json:"app_id"`
	DeploymentID int64             `json:"deployment_id"`
	Image        string            `json:"image"`
	Port         int               `json:"port,omitempty"` // HTTP port inside the container (0: not served)
	Env          map[string]string `json:"env,omitempty"`
	Volumes      []AppVolume       `json:"volumes,omitempty"`
	MemoryMB     int               `json:"memory_mb,omitempty"`
	CPUs         float64           `json:"cpus,omitempty"`
	// RegistryAuth holds credentials for the image's registry (private
	// images). Used for the pull only; the agent does not keep them.
	RegistryAuth *docker.AuthConfig `json:"registry_auth,omitempty"`
	// PruneImages are images of earlier deployments to remove once this
	// one has succeeded.
	PruneImages []ImageRef `json:"prune_images,omitempty"`
	// Git, when set, builds Image from a repository instead of pulling it.
	Git *GitSource `json:"git,omitempty"`

	// Services, when set, deploys a compose app: one container per
	// service on the app's network. Image, Port, Env, Volumes, MemoryMB,
	// CPUs, RegistryAuth, PruneImages and Git are not used then.
	Services []ServiceSpec `json:"services,omitempty"`
	// ComposeVolumes are the named volumes the services mount.
	ComposeVolumes []string `json:"compose_volumes,omitempty"`
	// RegistryAuths are credentials for the registries of service images,
	// matched by ServerAddress.
	RegistryAuths []docker.AuthConfig `json:"registry_auths,omitempty"`
	// Attach lists apps whose networks the services join, reachable there
	// by their service name (a database used by those apps).
	Attach []int64 `json:"attach,omitempty"`
}

// AppAttach replaces the networks of other apps an app's running
// containers are attached to (see AppSpec.Attach).
type AppAttach struct {
	AppID  int64   `json:"app_id"`
	Attach []int64 `json:"attach"`
}

// MaxAttach bounds AppSpec.Attach.
const MaxAttach = 200

// ValidateAttach checks the apps an app joins the networks of.
func ValidateAttach(appID int64, attach []int64) error {
	if len(attach) > MaxAttach {
		return fmt.Errorf("at most %d linked apps", MaxAttach)
	}
	seen := map[int64]bool{}
	for _, id := range attach {
		if id <= 0 || id == appID || seen[id] {
			return fmt.Errorf("invalid linked app %d", id)
		}
		seen[id] = true
	}
	return nil
}

// ImageRef is an image an app was deployed from: the reference used and
// the repository digest it resolved to.
type ImageRef struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest,omitempty"`
}

// AppVolume is a persistent named volume mounted at Path.
type AppVolume struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// AppDeployResponse reports a deployment. A failed deployment is a normal
// response (OK false), so its log reaches the panel.
type AppDeployResponse struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	Log         string `json:"log"`
	ContainerID string `json:"container_id,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
	// The commit a git deployment was built from.
	Commit        string `json:"commit,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
	// Digests are the images each service of a compose app runs, pinned
	// by repository digest when the registry gave one.
	Digests map[string]string `json:"digests,omitempty"`
}

type AppRequest struct {
	AppID int64 `json:"app_id"`
	// DeleteVolumes also removes the app's persistent data (app_delete).
	DeleteVolumes bool `json:"delete_volumes,omitempty"`
	// Tail is the number of log lines to return (app_logs).
	Tail int `json:"tail,omitempty"`
	// PruneImages are the app's images to remove with it (app_delete).
	PruneImages []ImageRef `json:"prune_images,omitempty"`
	// Service selects a service of a compose app (status, logs).
	Service string `json:"service,omitempty"`
}

// AppStatus describes the container currently serving an app.
type AppStatus struct {
	State        string    `json:"state"` // missing, created, running, restarting, exited, ...
	Health       string    `json:"health,omitempty"`
	DeploymentID int64     `json:"deployment_id,omitempty"`
	StartedAt    time.Time `json:"started_at,omitzero"`
	RestartCount int       `json:"restart_count,omitempty"`
	ExitCode     int       `json:"exit_code,omitempty"`
	OOMKilled    bool      `json:"oom_killed,omitempty"`
	Error        string    `json:"error,omitempty"`
	// Services of a compose app, by name; the fields above then describe
	// the requested (or published) service.
	Services []ServiceStatus `json:"services,omitempty"`
}

// ServiceStatus describes the current container of one compose service.
type ServiceStatus struct {
	Name         string    `json:"name"`
	Image        string    `json:"image"`
	State        string    `json:"state"`
	Health       string    `json:"health,omitempty"`
	DeploymentID int64     `json:"deployment_id,omitempty"`
	StartedAt    time.Time `json:"started_at,omitzero"`
	RestartCount int       `json:"restart_count,omitempty"`
	ExitCode     int       `json:"exit_code,omitempty"`
	OOMKilled    bool      `json:"oom_killed,omitempty"`
}

type AppLogsResponse struct {
	Log string `json:"log"`
}

// LogChunk is one stream frame of log output: whole lines.
type LogChunk struct {
	Text string `json:"text"`
}

// AppsSync lists every app the panel knows on this server; the agent
// removes containers of apps not in the list (deleted while it was offline).
type AppsSync struct {
	AppIDs []int64 `json:"app_ids"`
}
