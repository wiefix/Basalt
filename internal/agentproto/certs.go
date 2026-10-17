package agentproto

import "time"

// TypeCertStatus asks which certificates the proxy holds (CertStatus →
// CertStatusResponse).
const TypeCertStatus = "cert_status"

// MaxCertNames bounds a CertStatus request.
const MaxCertNames = 2000

// CertStatus lists the names (route domains, wildcards included) whose
// certificates the panel wants to know about.
type CertStatus struct {
	Names []string `json:"names"`
}

// CertInfo is the newest certificate the proxy holds for a name; a zero
// NotAfter means none.
type CertInfo struct {
	Name     string    `json:"name"`
	NotAfter time.Time `json:"not_after,omitzero"`
	Issuer   string    `json:"issuer,omitempty"`
	// Error is the proxy's latest failure to get or renew it, if any.
	Error   string    `json:"error,omitempty"`
	ErrorAt time.Time `json:"error_at,omitzero"`
}

type CertStatusResponse struct {
	Certs []CertInfo `json:"certs"`
}
