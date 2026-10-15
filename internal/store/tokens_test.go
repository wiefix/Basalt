package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAPITokens(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	now := time.Now()
	u, _ := db.CreateUser(ctx, "a@example.com", "A", "x", RoleAdmin)
	v, _ := db.CreateUser(ctx, "v@example.com", "V", "x", RoleViewer)
	soon := now.Add(time.Hour)
	ci := &APIToken{UserID: u.ID, Name: "ci", Prefix: "bst_abcd", Role: RoleMember, ExpiresAt: &soon}
	if err := db.CreateAPIToken(ctx, ci, []byte("hash-ci")); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAPIToken(ctx, &APIToken{UserID: u.ID, Name: "ci", Prefix: "p", Role: RoleViewer}, []byte("other")); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	// Names are per user.
	if err := db.CreateAPIToken(ctx, &APIToken{UserID: v.ID, Name: "ci", Prefix: "p", Role: RoleViewer}, []byte("hash-v")); err != nil {
		t.Fatal(err)
	}
	got, err := db.APITokenByHash(ctx, []byte("hash-ci"), now)
	if err != nil || got.ID != ci.ID || got.Role != RoleMember || got.UserEmail != "a@example.com" || got.ExpiresAt == nil {
		t.Fatalf("by hash: %+v %v", got, err)
	}
	if _, err := db.APITokenByHash(ctx, []byte("hash-ci"), soon); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired token found: %v", err)
	}
	if _, err := db.APITokenByHash(ctx, []byte("nope"), now); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	db.TouchAPIToken(ctx, ci.ID, "192.0.2.1", now)
	if got, _ := db.APIToken(ctx, ci.ID); got.LastUsedAt == nil || got.LastUsedIP != "192.0.2.1" {
		t.Fatalf("touch: %+v", got)
	}
	if list, _ := db.APITokens(ctx, u.ID); len(list) != 1 {
		t.Fatalf("own: %d", len(list))
	}
	if list, _ := db.APITokens(ctx, 0); len(list) != 2 || list[0].UserEmail != "v@example.com" {
		t.Fatalf("all: %+v", list)
	}
	if n, _ := db.DeleteExpiredAPITokens(ctx, soon); n != 1 {
		t.Fatalf("expired deleted: %d", n)
	}
	if err := db.DeleteAPIToken(ctx, ci.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	for i := range MaxAPITokens - 1 {
		if err := db.CreateAPIToken(ctx, &APIToken{UserID: v.ID, Name: string(rune('a'+i%26)) + string(rune('a'+i/26)), Prefix: "p", Role: RoleViewer},
			[]byte{byte(i), 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateAPIToken(ctx, &APIToken{UserID: v.ID, Name: "one more", Prefix: "p", Role: RoleViewer}, []byte("x")); !errors.Is(err, ErrTooManyTokens) {
		t.Fatalf("limit: %v", err)
	}
}
