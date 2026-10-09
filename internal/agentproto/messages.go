package agentproto

import "time"

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
	TypePing = "ping"
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
