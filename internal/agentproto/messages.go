package agentproto

import (
	"time"

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
