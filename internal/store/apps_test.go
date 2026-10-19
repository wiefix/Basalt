package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/dns"
	"github.com/wiefix/basalt/internal/proxy"
)

func TestDeploymentsOneAtATime(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	srv, err := db.CreateServer(ctx, "s", []byte("t"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp(ctx, srv.ID, "web", AppSettings{Image: "nginx:1.27", Port: 80}, []string{}, proxy.TLSAuto)
	if err != nil {
		t.Fatal(err)
	}
	d1, err := db.CreateDeployment(ctx, app.ID, "nginx:1.27", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateDeployment(ctx, app.ID, "nginx:1.27", nil); !errors.Is(err, ErrDeployInProgress) {
		t.Fatalf("second concurrent deployment: %v", err)
	}
	if err := db.FinishDeployment(ctx, d1.ID, DeploymentResult{OK: true, Log: "log", ImageDigest: "nginx@sha256:x"}); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishDeployment(ctx, d1.ID, DeploymentResult{Error: "late"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished twice: %v", err)
	}
	d2, err := db.CreateDeployment(ctx, app.ID, "nginx:1.28", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := db.FailUnfinishedDeployments(ctx); err != nil || n != 1 {
		t.Fatalf("FailUnfinished = %d, %v", n, err)
	}
	got, _ := db.DeploymentByID(ctx, app.ID, d2.ID)
	if got.Status != "failed" || got.FinishedAt == nil {
		t.Fatalf("interrupted deployment = %+v", got)
	}
	// An app's route has no domains until some are set: it never reaches the proxy.
	d, err := db.DesiredProxy(ctx, srv.ID)
	if err != nil || len(d.Routes) != 0 {
		t.Fatalf("desired = %+v, %v", d, err)
	}
	if err := db.SetAppDomains(ctx, app.ID, []string{"web.example.com"}); err != nil {
		t.Fatal(err)
	}
	d, _ = db.DesiredProxy(ctx, srv.ID)
	if len(d.Routes) != 1 || d.Routes[0].AppID != app.ID || d.Routes[0].Upstream != "" {
		t.Fatalf("app route = %+v", d.Routes)
	}
	if _, err := db.DeleteApp(ctx, app.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.Deployments(ctx, app.ID, 10); len(list) != 0 {
		t.Fatal("deployments outlived their app")
	}
}

func TestAppMetrics(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	s1, _ := db.CreateServer(ctx, "s1", []byte("t1"), time.Now().Add(time.Hour))
	s2, _ := db.CreateServer(ctx, "s2", []byte("t2"), time.Now().Add(time.Hour))
	a1, err := db.CreateApp(ctx, s1.ID, "a1", AppSettings{Image: "nginx:1.27", ReadOnly: true}, []string{}, proxy.TLSAuto)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := db.CreateApp(ctx, s2.ID, "a2", AppSettings{Image: "nginx:1.27"}, []string{}, proxy.TLSAuto)
	if got, _ := db.AppByID(ctx, a1.ID); !got.ReadOnly {
		t.Fatal("read_only not stored")
	}
	old, now := time.Now().Add(-3*time.Hour), time.Now()
	// A server reports only its own apps: a2 is on s2.
	if err := db.InsertAppMetrics(ctx, s1.ID, []AppMetrics{
		{AppID: a1.ID, At: old, CPUPct: 5}, {AppID: a1.ID, At: now, CPUPct: 150, MemUsed: 10, MemLimit: 20, NetRx: 1, NetTx: 2},
		{AppID: a2.ID, At: now, CPUPct: 1}, {AppID: 999, At: now},
	}); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.AppMetricsSince(ctx, a2.ID, old.Add(-time.Hour), 10); len(m) != 0 {
		t.Fatalf("metrics of another server's app stored: %v", m)
	}
	m, err := db.AppMetricsSince(ctx, a1.ID, old.Add(-time.Hour), 10)
	if err != nil || len(m) != 2 || m[1].CPUPct != 150 || m[1].NetTx != 2 {
		t.Fatalf("metrics = %+v, %v", m, err)
	}
	if _, err := db.DeleteMetricsBefore(ctx, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.AppMetricsSince(ctx, a1.ID, old.Add(-time.Hour), 10); len(m) != 1 {
		t.Fatalf("after pruning: %+v", m)
	}
	if _, err := db.DeleteApp(ctx, a1.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	db.R.QueryRow(`SELECT COUNT(*) FROM app_metrics`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d samples left after deleting the app", n)
	}
}

func TestAppReplicas(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	check := func(z *dns.Zone) error { return z.Validate() }
	var servers []int64
	for i, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"} {
		srv, err := db.CreateServer(ctx, "s"+strconv.Itoa(i), []byte{byte(i)}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		db.W.Exec(`UPDATE servers SET enrolled_at = 1, public_ips = ? WHERE id = ?`, `["`+ip+`"]`, srv.ID)
		servers = append(servers, srv.ID)
	}
	if _, err := db.CreateDNSZone(ctx, 0, "example.com", nil, check, time.Now()); err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp(ctx, servers[0], "web", AppSettings{Image: "nginx:1.27", Port: 80}, []string{"app.example.com"}, proxy.TLSAuto)
	if err != nil {
		t.Fatal(err)
	}
	hc := func(name string) *HealthCheck {
		return &HealthCheck{Name: name, Kind: "https", Host: "192.0.2.1", Port: 443, Path: "/", Interval: 30, Timeout: 5}
	}
	c0, c1 := hc("primary"), hc("replica-1")
	if err := db.AddAppReplica(ctx, app.ID, servers[1], c1, c0); err != nil {
		t.Fatal(err)
	}
	if err := db.AddAppReplica(ctx, app.ID, servers[1], hc("again"), nil); !errors.Is(err, ErrReplicaExists) {
		t.Fatalf("second replica on the same server: %v", err)
	}
	c2 := hc("replica-2")
	if err := db.AddAppReplica(ctx, app.ID, servers[2], c2, nil); err != nil {
		t.Fatal(err)
	}
	// One app, routes on three servers, each following its check.
	got, _ := db.AppByID(ctx, app.ID)
	if got.ServerID != servers[0] || len(got.Replicas) != 2 || len(got.Domains) != 1 {
		t.Fatalf("app = %+v", got)
	}
	if list, _ := db.Apps(ctx); len(list) != 1 {
		t.Fatalf("apps listed %d times", len(list))
	}
	ps, _ := db.AppPlacements(ctx, app.ID)
	if len(ps) != 3 || !ps[0].Primary || ps[0].HealthCheck != c0.ID || ps[1].HealthCheck != c1.ID || ps[2].HealthCheck != c2.ID {
		t.Fatalf("placements = %+v", ps)
	}
	for _, id := range servers {
		d, _ := db.DesiredProxy(ctx, id)
		if len(d.Routes) != 1 || d.Routes[0].AppID != app.ID || !d.Routes[0].DNSChallenge {
			t.Fatalf("server %d routes = %+v", id, d.Routes)
		}
		if ids, _ := db.AppIDsOnServer(ctx, id); len(ids) != 1 || ids[0] != app.ID {
			t.Fatalf("apps on %d = %v", id, ids)
		}
	}
	served := func() string {
		_, zones, err := db.DesiredDNS(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range zones[0].Records {
			out = append(out, r.Value)
		}
		return strings.Join(out, " ")
	}
	if got := served(); got != "192.0.2.1 192.0.2.2 192.0.2.3" {
		t.Fatalf("served %q", got)
	}
	// A copy that fails its check leaves DNS; the zone's revision moves.
	if d, err := db.SetHealthStatus(ctx, c1.ID, HealthDown, "x", time.Now()); err != nil || !d {
		t.Fatalf("set status: %v %v", d, err)
	}
	if got := served(); got != "192.0.2.1 192.0.2.3" {
		t.Fatalf("served %q", got)
	}
	// Domains change on every server.
	if err := db.SetAppDomains(ctx, app.ID, []string{"app.example.com", "www.example.com"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := db.DesiredProxy(ctx, servers[2]); len(d.Routes[0].Domains) != 2 {
		t.Fatalf("replica domains = %v", d.Routes[0].Domains)
	}
	// Removing replicas: the last one takes the primary's check along.
	if err := db.RemoveAppReplica(ctx, app.ID, servers[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing the primary: %v", err)
	}
	if err := db.RemoveAppReplica(ctx, app.ID, servers[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.HealthCheck(ctx, c1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replica check kept: %v", err)
	}
	if _, err := db.HealthCheck(ctx, c0.ID); err != nil {
		t.Fatalf("primary check gone early: %v", err)
	}
	if err := db.RemoveAppReplica(ctx, app.ID, servers[2]); err != nil {
		t.Fatal(err)
	}
	if checks, _ := db.HealthChecks(ctx); len(checks) != 0 {
		t.Fatalf("checks left: %+v", checks)
	}
	if d, _ := db.DesiredProxy(ctx, servers[0]); d.Routes[0].DNSChallenge {
		t.Fatal("single server still uses DNS-01")
	}
	// Deleting a replicated app reports its servers and drops its checks.
	if err := db.AddAppReplica(ctx, app.ID, servers[1], hc("r"), hc("p")); err != nil {
		t.Fatal(err)
	}
	gone, err := db.DeleteApp(ctx, app.ID)
	if err != nil || len(gone) != 2 || gone[0] != servers[0] || gone[1] != servers[1] {
		t.Fatalf("delete: %v %v", gone, err)
	}
	if checks, _ := db.HealthChecks(ctx); len(checks) != 0 {
		t.Fatalf("checks left after delete: %+v", checks)
	}
}
