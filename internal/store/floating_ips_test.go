package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFloatingIPs(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()
	a, _ := db.CreateServer(ctx, "a", []byte("a"), time.Now().Add(time.Hour))
	b, _ := db.CreateServer(ctx, "b", []byte("b"), time.Now().Add(time.Hour))
	acc, err := db.CreateCloudAccount(ctx, "hetzner", "main", []byte("sealed"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCloudAccount(ctx, "hetzner", "main", []byte("x")); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := db.UpdateCloudAccount(ctx, acc.ID, "renamed", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.CloudAccount(ctx, acc.ID); got.Name != "renamed" || string(got.Secret) != "sealed" {
		t.Fatalf("rename kept the token: %+v", got)
	}

	f, err := db.CreateFloatingIP(ctx, FloatingIP{AccountID: acc.ID, ProviderID: "10", Address: "203.0.113.10",
		Network: "203.0.113.10", ServerID: a.ID,
		Servers: []FloatingIPServer{{ServerID: b.ID, ProviderServerID: "2"}, {ServerID: a.ID, ProviderServerID: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if f.AssignedAt == nil || f.ServerID != a.ID {
		t.Fatalf("created: %+v", f)
	}
	if _, err := db.CreateFloatingIP(ctx, FloatingIP{AccountID: acc.ID, ProviderID: "11", Address: "203.0.113.10", Network: "x"}); !errors.Is(err, ErrAddressTaken) {
		t.Fatalf("same address: %v", err)
	}
	got, _ := db.FloatingIP(ctx, f.ID)
	if len(got.Servers) != 2 || got.Servers[0].ServerID != b.ID || got.Servers[1].ProviderServerID != "1" {
		t.Fatalf("group order: %+v", got.Servers)
	}
	if list, _ := db.FloatingAddressesOn(ctx, b.ID); len(list) != 1 || list[0] != "203.0.113.10" {
		t.Fatalf("addresses on b: %v", list)
	}
	if err := db.DeleteCloudAccount(ctx, acc.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("account in use deleted: %v", err)
	}

	if err := db.SetFloatingIPServer(ctx, f.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.FloatingIP(ctx, f.ID); got.ServerID != b.ID || got.AssignedAt == nil {
		t.Fatalf("assigned: %+v", got)
	}
	if err := db.SetFloatingIPServers(ctx, f.ID, []FloatingIPServer{{ServerID: a.ID, ProviderServerID: "1"}}); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.FloatingAddressesOn(ctx, b.ID); len(list) != 0 {
		t.Fatalf("b left the group: %v", list)
	}
	// A deleted server leaves the group, and is no longer the assigned one.
	if err := db.DeleteServer(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.FloatingIP(ctx, f.ID); got.ServerID != 0 || len(got.Servers) != 1 {
		t.Fatalf("after the server was deleted: %+v", got)
	}

	if err := db.SetServerFloatingError(ctx, a.ID, "no ip"); err != nil {
		t.Fatal(err)
	}
	if errs, _ := db.ServerFloatingErrors(ctx); errs[a.ID] != "no ip" || len(errs) != 1 {
		t.Fatalf("errors: %v", errs)
	}
	if err := db.DeleteFloatingIP(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCloudAccount(ctx, acc.ID); err != nil {
		t.Fatal(err)
	}
}
