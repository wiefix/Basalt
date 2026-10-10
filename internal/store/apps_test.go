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
