package store

import (
	"context"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/proxy"
)

// TestRollUpMetrics: samples older than RawMetricsKept become one sample
// per 5-minute bucket (averages for gauges, highest values for totals and
// counters), recent samples stay as they are, and a second run changes
// nothing.
func TestRollUpMetrics(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	srv, _ := db.CreateServer(ctx, "s1", []byte("t1"), time.Now().Add(time.Hour))
	app, err := db.CreateApp(ctx, srv.ID, "a1", AppSettings{Image: "nginx:1.27"}, []string{}, proxy.TLSAuto)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0) // a bucket boundary: 1.8e9 / 300 is whole
	old := now.Add(-RawMetricsKept - time.Hour)
	// Two buckets of old samples: 30 per bucket, 10 s apart.
	for i := range 60 {
		at := old.Add(time.Duration(i) * 10 * time.Second)
		if err := db.InsertMetrics(ctx, srv.ID, ServerMetrics{At: at, CPUPct: float64(i % 30), MemUsed: 100,
			MemTotal: uint64(1000 + i), DiskUsed: 50, DiskTotal: 500, Load1: 1, Uptime: uint64(i)}); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertAppMetrics(ctx, srv.ID, []AppMetrics{{AppID: app.ID, At: at, CPUPct: 10, MemUsed: uint64(i),
			MemLimit: 64, NetRx: uint64(1000 * i), NetTx: uint64(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	recent := now.Add(-time.Minute)
	db.InsertMetrics(ctx, srv.ID, ServerMetrics{At: recent, CPUPct: 77})
	db.InsertAppMetrics(ctx, srv.ID, []AppMetrics{{AppID: app.ID, At: recent, CPUPct: 77}})

	n, err := db.RollUpMetrics(ctx, now)
	if err != nil || n != 120 {
		t.Fatalf("rolled up %d samples, %v", n, err)
	}
	m, _ := db.MetricsSince(ctx, srv.ID, old.Add(-time.Hour), 100)
	if len(m) != 3 {
		t.Fatalf("server samples after roll-up: %d", len(m))
	}
	first, second := m[0], m[1]
	if !first.At.Equal(old) || !second.At.Equal(old.Add(5*time.Minute)) {
		t.Fatalf("bucket times: %v %v", first.At, second.At)
	}
	if first.CPUPct != 14.5 || first.MemUsed != 100 || first.MemTotal != 1029 || first.Uptime != 29 || second.MemTotal != 1059 {
		t.Fatalf("server bucket: %+v / %+v", first, second)
	}
	if m[2].CPUPct != 77 || !m[2].At.Equal(recent) {
		t.Fatalf("a recent sample changed: %+v", m[2])
	}
	a, _ := db.AppMetricsSince(ctx, app.ID, old.Add(-time.Hour), 100)
	if len(a) != 3 || a[0].CPUPct != 10 || a[0].MemLimit != 64 || a[0].NetRx != 29000 || a[1].NetTx != 59 || a[0].MemUsed != 15 {
		t.Fatalf("app samples after roll-up: %+v", a)
	}
	// Already rolled up: nothing changes, also an hour later for these.
	if n, err := db.RollUpMetrics(ctx, now); err != nil || n != 0 {
		t.Fatalf("second run: %d, %v", n, err)
	}
	if n, err := db.RollUpMetrics(ctx, now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("later run touched rolled-up samples or recent ones: %d, %v", n, err)
	}
	if m, _ := db.MetricsSince(ctx, srv.ID, old.Add(-time.Hour), 100); len(m) != 3 || m[0].CPUPct != 14.5 {
		t.Fatalf("rolled-up samples changed: %+v", m)
	}
	// Once old enough, the recent sample is rolled up too.
	if n, err := db.RollUpMetrics(ctx, now.Add(RawMetricsKept+5*time.Minute)); err != nil || n != 2 {
		t.Fatalf("roll-up of the recent sample: %d, %v", n, err)
	}
	if m, _ := db.MetricsSince(ctx, srv.ID, old.Add(-time.Hour), 100); len(m) != 3 || !m[2].At.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("after rolling up the recent sample: %+v", m)
	}
}
