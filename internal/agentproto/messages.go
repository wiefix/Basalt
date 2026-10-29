package agentproto

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/wiefix/basalt/internal/dns"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/php"
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
	// TypeACMEChallenge sets or removes a DNS-01 challenge record for the
	// host's proxy (ACMEChallenge).
	TypeACMEChallenge = "acme_challenge"
	// server -> agent requests
	TypePing       = "ping"
	TypeProxyApply = "proxy_apply"
	TypeSiteEnsure = "site_ensure"
	TypeSiteDelete = "site_delete"
	TypeFiles      = "files"
	TypeSFTPApply  = "sftp_apply"
	TypeSFTPBans   = "sftp_bans" // lists, and with Unban lifts, SFTP bans
	TypeDNSApply   = "dns_apply"
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
	// TypeComposeFile reads the compose file of a git app from its
	// repository (ComposeFileRequest).
	TypeComposeFile = "compose_file"
	// TypeMailAPI relays a management call to the mail server (MailAPI).
	TypeMailAPI = "mail_api"
	// TypeMailCert loads the proxy's certificate into the mail server.
	TypeMailCert = "mail_cert"
	// TypeMailCheck runs the deliverability checks of the mail server's
	// host (MailCheck).
	TypeMailCheck = "mail_check"
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
	// DockerSecurity describes how the daemon isolates containers.
	DockerSecurity *docker.Security `json:"docker_security,omitempty"`
	// Restic is the version of restic backups run with ("" if absent).
	Restic string `json:"restic,omitempty"`
	// Railpack is the version apps without a Dockerfile are built with
	// ("" if absent, or no Docker).
	Railpack string `json:"railpack,omitempty"`
	// DNSPort is the port the authoritative DNS server listens on (0:
	// disabled); DNSError says why it could not listen.
	DNSPort  int    `json:"dns_port,omitempty"`
	DNSError string `json:"dns_error,omitempty"`
	// PeerPort is the port other servers' agents reach this one's tunnel
	// on (0: disabled); PeerError says why it could not listen.
	PeerPort  int    `json:"peer_port,omitempty"`
	PeerError string `json:"peer_error,omitempty"`
	// PublicIPs are the public addresses on the host's interfaces; the
	// panel publishes them in automatic DNS records.
	PublicIPs []string `json:"public_ips,omitempty"`
	// HTTPSPort is the port the proxy serves HTTPS on (0: unknown, 443).
	HTTPSPort int `json:"https_port,omitempty"`
	// Updatable: the agent can update Basalt here (installed with systemd
	// units by the installer).
	Updatable bool `json:"updatable,omitempty"`
	// PHPLimits: each site's PHP runs as its own systemd service, so the
	// sites' CPU and memory limits apply.
	PHPLimits bool `json:"php_limits,omitempty"`
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
	// Apps is the resource usage of each app's running containers.
	Apps []AppMetrics `json:"apps,omitempty"`
}

// AppMetrics sums the usage of an app's running containers.
type AppMetrics struct {
	AppID int64 `json:"app_id"`
	// CPUPct is in percent of one CPU (two busy cores: 200).
	CPUPct   float64 `json:"cpu_pct"`
	MemUsed  uint64  `json:"mem_used"`
	MemLimit uint64  `json:"mem_limit"` // the containers' limits, or the host's memory
	// NetRx and NetTx count bytes since the containers started.
	NetRx      uint64 `json:"net_rx"`
	NetTx      uint64 `json:"net_tx"`
	Containers int    `json:"containers"`
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
	// PHP are the site's PHP options.
	PHP php.Settings `json:"php,omitzero"`
	// DB, on a server serving a copy of the site, is where the site's
	// databases live: its PHP reaches them through the tunnel.
	DB *SiteDBRoute `json:"db,omitempty"`
}

// SiteDBRoute is the server whose MariaDB a site uses, and where its
// tunnel listens.
type SiteDBRoute struct {
	ServerID int64  `json:"server_id"`
	Addr     string `json:"addr"`
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

// SFTPBans lists the addresses the SFTP server refuses for failed logins;
// Unban (an IP address) lets one in again first.
type SFTPBans struct {
	Unban string `json:"unban,omitempty"`
}

type SFTPBansResponse struct {
	Bans     []sftpd.Ban `json:"bans"`
	Unbanned bool        `json:"unbanned,omitempty"`
}

// ACMEChallenge asks the panel to publish (or withdraw) the TXT record
// _acme-challenge.<domain> on every DNS server. The panel answers once the
// servers have it.
type ACMEChallenge struct {
	Name    string `json:"name"` // _acme-challenge.<domain>, no trailing dot
	Value   string `json:"value"`
	Present bool   `json:"present"`
}

// DNSApply is the complete set of DNS zones. Every agent keeps them; the
// ones with a DNS listener serve them.
type DNSApply struct {
	Revision int64      `json:"revision"`
	Zones    []dns.Zone `json:"zones"`
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
	// ReadOnly runs the container with a read-only root filesystem;
	// /tmp and /run stay writable (in memory).
	ReadOnly bool `json:"read_only,omitempty"`
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
	// CPUs and RegistryAuth are not used then; Git (compose builder) is the
	// repository services with a Build come from, PruneImages the images
	// earlier deployments built.
	Services []ServiceSpec `json:"services,omitempty"`
	// ComposeVolumes are the named volumes the services mount.
	ComposeVolumes []string `json:"compose_volumes,omitempty"`
	// RegistryAuths are credentials for the registries of service images,
	// matched by ServerAddress.
	RegistryAuths []docker.AuthConfig `json:"registry_auths,omitempty"`
	// Attach lists apps whose networks the services join, reachable there
	// by their service name (a database used by those apps).
	Attach []int64 `json:"attach,omitempty"`
	// Mail marks the mail server, the one app allowed to publish ports on
	// the host (ServiceSpec.HostPorts).
	Mail bool `json:"mail,omitempty"`
}

// MailAPI is a management call to the mail server of app AppID: the
// agent posts Body to its JMAP endpoint with the credentials and returns
// the answer. The agent keeps no credentials.
type MailAPI struct {
	AppID    int64  `json:"app_id"`
	User     string `json:"user"`
	Password string `json:"password"`
	Body     []byte `json:"body"`
}

// MailCert asks the agent to load the proxy's certificate for Hostname
// into the mail server of app AppID (administrator credentials included).
type MailCert struct {
	AppID    int64  `json:"app_id"`
	Hostname string `json:"hostname"`
	User     string `json:"user"`
	Password string `json:"password"`
}

type MailCertResponse struct {
	Installed bool      `json:"installed"`
	NotAfter  time.Time `json:"not_after"`
}

// MailCheck asks the agent of the mail server's host to check what
// decides whether its mail is delivered: outgoing port 25, reverse DNS and
// blocklists of its public addresses, and the DNS records of each domain
// as resolvers see them.
type MailCheck struct {
	Hostname string            `json:"hostname"`
	IPs      []string          `json:"ips"`
	Domains  []MailCheckDomain `json:"domains"`
}

// MailCheckDomain is a mail domain with the records the mail server wants
// published (absolute names).
type MailCheckDomain struct {
	Name    string       `json:"name"`
	Records []dns.Record `json:"records"`
}

// MailCheckResult is the outcome of one check. Domain is empty for the
// checks of the server; Target is the address or name checked.
type MailCheckResult struct {
	Domain string `json:"domain,omitempty"`
	Check  string `json:"check"`
	Target string `json:"target,omitempty"`
	Status string `json:"status"` // ok, warn, fail, unknown
	Detail string `json:"detail"`
}

type MailCheckResponse struct {
	Results []MailCheckResult `json:"results"`
}

type MailAPIResponse struct {
	Status int    `json:"status"`
	Body   []byte `json:"body"`
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

// TypeSiteUsage measures the disk space of sites and their databases.
// Streaming (walking big sites takes a while; no frames).
const TypeSiteUsage = "site_usage"

type SiteUsageRequest struct {
	Sites     []int64  `json:"sites"`
	Databases []string `json:"databases,omitempty"`
}

type SiteUsage struct {
	SiteID int64  `json:"site_id"`
	Bytes  int64  `json:"bytes"` // space taken on disk
	Files  int64  `json:"files"`
	Error  string `json:"error,omitempty"`
}

type SiteUsageResponse struct {
	Sites     []SiteUsage      `json:"sites"`
	Databases map[string]int64 `json:"databases,omitempty"`
}

// TypeSiteInstall installs a web application into a site's empty public
// folder. Streaming (the download takes a while; no frames).
const TypeSiteInstall = "site_install"

// Site applications.
const AppWordPress = "wordpress"

type SiteInstall struct {
	SiteID int64  `json:"site_id"`
	App    string `json:"app"`
	// The database the application uses (created by the panel).
	DBName     string `json:"db_name"`
	DBUser     string `json:"db_user"`
	DBPassword string `json:"db_password"`
}

type SiteInstallResponse struct {
	Version string `json:"version,omitempty"`
	Files   int    `json:"files"`
}

// TypeSiteTrash lists the folders of deleted sites waiting in the trash;
// with Remove (an entry's name) it deletes that one first, with Restore
// it makes that one the folder of site SiteID (a new site), its files
// given to the site's user.
const TypeSiteTrash = "site_trash"

type SiteTrash struct {
	Remove  string `json:"remove,omitempty"`
	Restore string `json:"restore,omitempty"`
	SiteID  int64  `json:"site_id,omitempty"`
}

type TrashEntry struct {
	Name      string    `json:"name"` // <site id>-<unix time>
	SiteID    int64     `json:"site_id"`
	DeletedAt time.Time `json:"deleted_at"`
	Bytes     int64     `json:"bytes"`
}

type SiteTrashResponse struct {
	Entries []TrashEntry `json:"entries"`
}

// TypeAccessLog returns the latest requests to a managed site or app from
// the proxy's access log.
const TypeAccessLog = "access_log"

type AccessLogRequest struct {
	SiteID int64 `json:"site_id,omitempty"`
	AppID  int64 `json:"app_id,omitempty"`
	Limit  int   `json:"limit"`
}

// AccessEntry is one request.
type AccessEntry struct {
	At         time.Time `json:"at"`
	IP         string    `json:"ip"`
	Method     string    `json:"method"`
	Host       string    `json:"host"`
	URI        string    `json:"uri"`
	Proto      string    `json:"proto"`
	Status     int       `json:"status"`
	Bytes      int64     `json:"bytes"`
	DurationMS float64   `json:"duration_ms"`
	UserAgent  string    `json:"user_agent,omitempty"`
}

type AccessLogResponse struct {
	Entries []AccessEntry `json:"entries"`
}

// TypeDBPassword changes the password of a container database's user
// (PostgreSQL, MySQL, MariaDB) inside its running container.
const TypeDBPassword = "db_password"

type ContainerDBPassword struct {
	AppID    int64  `json:"app_id"`
	Service  string `json:"service"`
	Engine   string `json:"engine"` // postgres, mysql, mariadb
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password"`
}

var dbPasswordRE = regexp.MustCompile(`^[A-Za-z0-9]{16,128}$`)

func (r *ContainerDBPassword) Validate() error {
	switch {
	case r.AppID <= 0 || !ServiceNameRE.MatchString(r.Service):
		return errors.New("invalid database app")
	case !dumpEngines[r.Engine]:
		return errors.New("only PostgreSQL, MySQL and MariaDB passwords are changed this way")
	case !sqlNameRE.MatchString(r.User) || !sqlNameRE.MatchString(r.Database):
		return errors.New("invalid database or user name")
	case !dbPasswordRE.MatchString(r.Password):
		return errors.New("invalid password")
	}
	return nil
}
