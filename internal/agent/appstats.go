package agent

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
)

// cpuSample is a container's CPU counters at one sample; usage is the
// difference between two.
type cpuSample struct{ container, system uint64 }

// appStats keeps what usage sampling needs between samples.
type appStats struct {
	mu      sync.Mutex
	prev    map[string]cpuSample // by container ID
	hostMem uint64
	memAt   time.Time
}

// Stats samples the resource usage of every app's running containers.
func (m *appsManager) Stats(ctx context.Context) []agentproto.AppMetrics {
	dc, err := m.client()
	if err != nil {
		return nil
	}
	list, err := m.containers(ctx, dc, 0)
	if err != nil {
		return nil
	}
	m.stats.mu.Lock()
	if m.stats.prev == nil {
		m.stats.prev = map[string]cpuSample{}
	}
	if time.Since(m.stats.memAt) > 10*time.Minute {
		if info, err := dc.Info(ctx); err == nil {
			m.stats.hostMem, m.stats.memAt = info.MemTotal, time.Now()
		}
	}
	hostMem := m.stats.hostMem
	m.stats.mu.Unlock()

	type result struct {
		app   int64
		id    string
		stats *docker.Stats
	}
	var running []docker.ContainerSummary
	for _, c := range list {
		if c.State == "running" && labelInt(c.Labels, labelApp) > 0 {
			running = append(running, c)
		}
	}
	results := make([]result, len(running))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, c := range running {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			s, err := dc.ContainerStats(ctx, c.ID)
			if err == nil {
				results[i] = result{labelInt(c.Labels, labelApp), c.ID, s}
			}
		})
	}
	wg.Wait()

	m.stats.mu.Lock()
	defer m.stats.mu.Unlock()
	byApp := map[int64]*agentproto.AppMetrics{}
	unlimited := map[int64]bool{}
	seen := map[string]bool{}
	for _, r := range results {
		if r.stats == nil {
			continue
		}
		seen[r.id] = true
		a := byApp[r.app]
		if a == nil {
			a = &agentproto.AppMetrics{AppID: r.app}
			byApp[r.app] = a
		}
		s := r.stats
		cur := cpuSample{s.CPUStats.CPUUsage.TotalUsage, s.CPUStats.SystemUsage}
		if prev, ok := m.stats.prev[r.id]; ok {
			a.CPUPct += cpuUsage(prev, cur, s.CPUStats.OnlineCPUs)
		}
		m.stats.prev[r.id] = cur
		a.MemUsed += s.MemoryUsed()
		if hostMem > 0 && s.MemoryStats.Limit >= hostMem {
			unlimited[r.app] = true
		} else {
			a.MemLimit += s.MemoryStats.Limit
		}
		for _, n := range s.Networks {
			a.NetRx += n.RxBytes
			a.NetTx += n.TxBytes
		}
		a.Containers++
	}
	maps.DeleteFunc(m.stats.prev, func(id string, _ cpuSample) bool { return !seen[id] })
	out := make([]agentproto.AppMetrics, 0, len(byApp))
	for _, id := range slices.Sorted(maps.Keys(byApp)) {
		a := byApp[id]
		if unlimited[id] {
			a.MemLimit = hostMem // some container may use all of it
		}
		out = append(out, *a)
	}
	return out
}

// cpuUsage is the CPU a container used between two samples, in percent of
// one CPU.
func cpuUsage(prev, cur cpuSample, cpus int) float64 {
	if cur.container < prev.container || cur.system <= prev.system || cpus <= 0 {
		return 0
	}
	return float64(cur.container-prev.container) / float64(cur.system-prev.system) * float64(cpus) * 100
}
