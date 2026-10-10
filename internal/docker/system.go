package docker

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Info is the part of the daemon's /info that describes how containers
// are isolated.
type Info struct {
	SecurityOptions []string `json:"SecurityOptions"` // e.g. "name=seccomp,profile=builtin"
	CgroupVersion   string   `json:"CgroupVersion"`
	CgroupDriver    string   `json:"CgroupDriver"`
	DockerRootDir   string   `json:"DockerRootDir"`
	MemTotal        uint64   `json:"MemTotal"` // memory of the host (or VM) containers run on
}

// Info returns daemon-wide settings.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	err := c.call(ctx, http.MethodGet, "/info", nil, nil, &info)
	return info, err
}

// Security summarises the isolation features the daemon applies to every
// container.
type Security struct {
	UserNS   bool   `json:"userns"`             // userns-remap: root in a container is not root on the host
	Rootless bool   `json:"rootless,omitempty"` // the daemon itself runs unprivileged
	Seccomp  string `json:"seccomp,omitempty"`  // profile name ("builtin" is Docker's default); "" when off
	AppArmor bool   `json:"apparmor"`
	SELinux  bool   `json:"selinux,omitempty"`
	CgroupV2 bool   `json:"cgroup_v2"`
}

func (i Info) Security() Security {
	s := Security{CgroupV2: i.CgroupVersion == "2"}
	for _, opt := range i.SecurityOptions {
		var name, profile string
		for kv := range strings.SplitSeq(opt, ",") {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "name":
				name = v
			case "profile":
				profile = v
			}
		}
		switch name {
		case "userns":
			s.UserNS = true
		case "rootless":
			s.Rootless = true
		case "seccomp":
			s.Seccomp = profile
			if s.Seccomp == "" {
				s.Seccomp = "default"
			}
		case "apparmor":
			s.AppArmor = true
		case "selinux":
			s.SELinux = true
		}
	}
	return s
}

// Stats is one sample of a container's resource usage.
type Stats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"` // nanoseconds
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"` // nanoseconds, all CPUs
		OnlineCPUs  int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
}

// MemoryUsed is the memory a container uses without reclaimable page
// cache, as docker stats reports it.
func (s *Stats) MemoryUsed() uint64 {
	cache := s.MemoryStats.Stats["inactive_file"]                // cgroup v2
	if v, ok := s.MemoryStats.Stats["total_inactive_file"]; ok { // cgroup v1
		cache = v
	}
	return s.MemoryStats.Usage - min(cache, s.MemoryStats.Usage)
}

// ContainerStats returns one sample of a running container's usage without
// waiting for a second one (CPU usage is a difference of two samples; the
// caller keeps the previous one).
func (c *Client) ContainerStats(ctx context.Context, id string) (*Stats, error) {
	var s Stats
	q := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	if err := c.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats", q, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
