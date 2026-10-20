package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/proxy"
)

// TestSiteReplicas: a site served by a second server keeps one row with
// its primary's route, routes everywhere with the same domains and checks,
// DNS-01 on every route, and the state of each copy of its files.
func TestSiteReplicas(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	srv, _ := db.CreateServer(ctx, "s1", []byte("t1"), time.Now().Add(time.Hour))
	srv2, _ := db.CreateServer(ctx, "s2", []byte("t2"), time.Now().Add(time.Hour))
	site, err := db.CreateSite(ctx, 0, srv.ID, "blog", []string{"blog.example.com"}, proxy.TLSAuto, "8.4")
	if err != nil {
		t.Fatal(err)
	}
	hc := func(name string) *HealthCheck {
		return &HealthCheck{Name: name, Kind: "https", Host: "192.0.2.1", Port: 443, Path: "/", Interval: 30, Timeout: 5}
	}
	c1, c0 := hc("replica"), hc("primary")
	if err := db.AddSiteReplica(ctx, site.ID, srv2.ID, c1, c0); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSiteReplica(ctx, site.ID, srv2.ID, hc("again"), nil); !errors.Is(err, ErrReplicaExists) {
		t.Fatalf("second replica on the same server: %v", err)
	}
	if err := db.AddSiteReplica(ctx, 999, srv2.ID, hc("x"), nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replica of a missing site: %v", err)
	}
	all, _ := db.Sites(ctx)
	if len(all) != 1 || all[0].ServerID != srv.ID || !slices.Equal(all[0].Replicas, []int64{srv2.ID}) || len(all[0].Domains) != 1 {
		t.Fatalf("sites: %+v", all[0])
	}
	got, _ := db.SiteByID(ctx, site.ID)
	if got.RouteID != site.RouteID {
		t.Fatalf("the site's route is not the primary's: %d", got.RouteID)
	}
	ps, err := db.SitePlacements(ctx, site.ID)
	if err != nil || len(ps) != 2 || !ps[0].Primary || ps[0].HealthCheck != c0.ID || ps[1].HealthCheck != c1.ID || ps[1].SyncedAt != nil {
		t.Fatalf("placements: %+v %v", ps, err)
	}
	for _, sid := range []int64{srv.ID, srv2.ID} {
		routes, _ := db.Routes(ctx, sid)
		if len(routes) != 1 || !routes[0].DNSChallenge || !routes[0].PHP || routes[0].SiteID != site.ID {
			t.Fatalf("routes on %d: %+v", sid, routes[0])
		}
	}
	if on, _ := db.SiteReplicasOn(ctx, srv2.ID); len(on) != 1 || on[0].ID != site.ID {
		t.Fatalf("replicas on s2: %v", on)
	}
	if on, _ := db.SiteReplicasOn(ctx, srv.ID); len(on) != 0 {
		t.Fatalf("the primary is no replica: %v", on)
	}
	if list, _ := db.SitesOnServer(ctx, srv2.ID); len(list) != 0 {
		t.Fatalf("a replica's server does not own the site: %v", list)
	}
	if rs, _ := db.ReplicatedSites(ctx); len(rs) != 1 {
		t.Fatalf("replicated sites: %v", rs)
	}

	// Sync state.
	now := time.Unix(1_800_000_000, 0)
	if err := db.SetSiteSync(ctx, site.ID, srv2.ID, now, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSiteSync(ctx, site.ID, srv2.ID, now.Add(time.Minute), "no space left"); err != nil {
		t.Fatal(err)
	}
	ps, _ = db.SitePlacements(ctx, site.ID)
	if ps[1].SyncedAt == nil || !ps[1].SyncedAt.Equal(now) || ps[1].SyncError != "no space left" {
		t.Fatalf("sync state: %+v", ps[1])
	}
	if err := db.SetSiteSync(ctx, site.ID, srv.ID+99, now, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sync of a server without the site: %v", err)
	}

	// Domains and PHP go to every route.
	if err := db.SetSiteDomains(ctx, site.ID, []string{"a.example.com", "b.example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []int64{srv.ID, srv2.ID} {
		routes, _ := db.Routes(ctx, sid)
		if !slices.Equal(routes[0].Domains, []string{"a.example.com", "b.example.com"}) {
			t.Fatalf("domains on %d: %v", sid, routes[0].Domains)
		}
	}
	before, _ := db.DesiredProxy(ctx, srv2.ID)
	if err := db.SetSitePHP(ctx, site.ID, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := db.DesiredProxy(ctx, srv2.ID)
	if after.Revision <= before.Revision {
		t.Fatal("switching PHP off left the replica's proxy alone")
	}

	// Checks move with the site; alone they do not.
	shop, _ := db.CreateProject(ctx, "Shop", "")
	if err := db.MoveToProject(ctx, ResourceHealthCheck, c1.ID, shop.ID); !errors.Is(err, ErrReplicaCheckMove) {
		t.Fatalf("move a placement's check alone: %v", err)
	}
	if err := db.MoveToProject(ctx, ResourceSite, site.ID, shop.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.HealthCheck(ctx, c1.ID); c.ProjectID != shop.ID {
		t.Fatalf("check stayed in project %d", c.ProjectID)
	}

	// Removing the last replica drops both checks and DNS-01.
	if err := db.RemoveSiteReplica(ctx, site.ID, srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove the primary as a replica: %v", err)
	}
	if err := db.RemoveSiteReplica(ctx, site.ID, srv2.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.HealthChecks(ctx); len(n) != 0 {
		t.Fatalf("checks left: %d", len(n))
	}
	if routes, _ := db.Routes(ctx, srv.ID); routes[0].DNSChallenge {
		t.Fatal("DNS-01 stayed on with one server")
	}

	// Deleting a replicated site returns its servers and drops its checks.
	if err := db.AddSiteReplica(ctx, site.ID, srv2.ID, hc("r2"), hc("p2")); err != nil {
		t.Fatal(err)
	}
	servers, err := db.DeleteSite(ctx, site.ID)
	if err != nil || !slices.Equal(servers, []int64{srv.ID, srv2.ID}) {
		t.Fatalf("delete: %v %v", servers, err)
	}
	if n, _ := db.HealthChecks(ctx); len(n) != 0 {
		t.Fatalf("checks left after delete: %d", len(n))
	}
	if _, err := db.DeleteSite(ctx, site.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete again: %v", err)
	}
}
