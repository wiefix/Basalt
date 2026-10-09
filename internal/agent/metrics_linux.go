package agent

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/wiefix/basalt/internal/agentproto"
)

func (c *collector) collectPlatform(m *agentproto.Metrics) error {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return err
	}
	cur, err := parseCPU(f)
	f.Close()
	if err != nil {
		return err
	}
	if c.havePrev {
		m.CPUPct = cpuPercent(c.prevCPU, cur)
	}
	c.prevCPU, c.havePrev = cur, true

	mf, err := os.Open("/proc/meminfo")
	if err != nil {
		return err
	}
	m.MemTotal, m.MemUsed, err = parseMeminfo(mf)
	mf.Close()
	if err != nil {
		return err
	}

	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		m.Load1, m.Load5, m.Load15, _ = parseLoadavg(string(b))
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		m.Uptime, _ = parseUptime(string(b))
	}
	return nil
}

func platformFacts() (kernel, distro string, memTotal uint64) {
	var u unix.Utsname
	if unix.Uname(&u) == nil {
		kernel = unix.ByteSliceToString(u.Release[:])
	}
	if f, err := os.Open("/etc/os-release"); err == nil {
		s := bufio.NewScanner(f)
		for s.Scan() {
			if v, ok := strings.CutPrefix(s.Text(), "PRETTY_NAME="); ok {
				distro = strings.Trim(v, `"'`)
			}
		}
		f.Close()
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		memTotal, _, _ = parseMeminfo(f)
		f.Close()
	}
	return kernel, distro, memTotal
}
