package store

import (
	"context"
	"errors"
	"testing"
	"time"

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
	if err := db.DeleteApp(ctx, app.ID); err != nil {
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
	if err := db.DeleteApp(ctx, a1.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	db.R.QueryRow(`SELECT COUNT(*) FROM app_metrics`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d samples left after deleting the app", n)
	}
}
