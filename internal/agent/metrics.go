package agent

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wiefix/basalt/internal/agentproto"
)

// collector samples host metrics. CPU usage needs two samples, so it keeps
// the previous /proc/stat reading.
type collector struct {
	mu       sync.Mutex
	prevCPU  cpuTimes
	havePrev bool
}

func newCollector() *collector { return &collector{} }

type cpuTimes struct{ idle, total uint64 }

func (c *collector) collect(diskPath string) (agentproto.Metrics, error) {
	m := agentproto.Metrics{At: time.Now()}

	var st unix.Statfs_t
	if err := unix.Statfs(diskPath, &st); err != nil {
		return m, fmt.Errorf("statfs %s: %w", diskPath, err)
	}
	bsize := uint64(st.Bsize)
	m.DiskTotal = uint64(st.Blocks) * bsize
	m.DiskUsed = (uint64(st.Blocks) - uint64(st.Bfree)) * bsize

	c.mu.Lock()
	defer c.mu.Unlock()
	return m, c.collectPlatform(&m)
}

// parseCPU reads the aggregate "cpu" line of /proc/stat.
func parseCPU(r io.Reader) (cpuTimes, error) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var t cpuTimes
		for i, v := range f[1:] {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return t, fmt.Errorf("parse /proc/stat: %w", err)
			}
			// Fields: user nice system idle iowait irq softirq steal guest guest_nice.
			// guest and guest_nice are already counted in user and nice.
			if i >= 8 {
				break
			}
			t.total += n
			if i == 3 || i == 4 {
				t.idle += n
			}
		}
		return t, nil
	}
	return cpuTimes{}, fmt.Errorf("parse /proc/stat: no cpu line")
}

func cpuPercent(prev, cur cpuTimes) float64 {
	dt := cur.total - prev.total
	if cur.total <= prev.total || dt == 0 {
		return 0
	}
	busy := dt - min(dt, cur.idle-prev.idle)
	return float64(busy) / float64(dt) * 100
}

// parseMeminfo returns total and used bytes, where used excludes reclaimable
// page cache (MemTotal - MemAvailable), matching what `free` reports.
func parseMeminfo(r io.Reader) (total, used uint64, err error) {
	var avail uint64
	var haveTotal, haveAvail bool
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total, haveTotal = n*1024, true
		case "MemAvailable:":
			avail, haveAvail = n*1024, true
		}
	}
	if !haveTotal || !haveAvail {
		return 0, 0, fmt.Errorf("parse /proc/meminfo: missing MemTotal or MemAvailable")
	}
	return total, total - min(total, avail), nil
}

func parseLoadavg(s string) (l1, l5, l15 float64, err error) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return 0, 0, 0, fmt.Errorf("parse /proc/loadavg: %q", s)
	}
	vals := make([]float64, 3)
	for i := range vals {
		if vals[i], err = strconv.ParseFloat(f[i], 64); err != nil {
			return 0, 0, 0, fmt.Errorf("parse /proc/loadavg: %w", err)
		}
	}
	return vals[0], vals[1], vals[2], nil
}

func parseUptime(s string) (uint64, error) {
	f := strings.Fields(s)
	if len(f) < 1 {
		return 0, fmt.Errorf("parse /proc/uptime: %q", s)
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return uint64(v), err
}
