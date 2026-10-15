package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUserManagement(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	now := time.Now()
	owner, _ := db.CreateFirstOwner(ctx, "o@example.com", "O", "x")
	m, _ := db.CreateUser(ctx, "m@example.com", "M", "x", RoleMember)
	for i, h := range [][]byte{[]byte("hash-m-1-xxxxxxx"), []byte("hash-m-2-xxxxxxx")} {
		db.CreateSession(ctx, Session{TokenHash: h, UserID: m.ID, CreatedAt: now, LastSeenAt: now.Add(time.Duration(i) * time.Minute), ExpiresAt: now.Add(time.Hour)})
	}
	db.CreateSession(ctx, Session{TokenHash: []byte("pending-xxxxxxxx"), UserID: m.ID, MFAPending: true, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)})
	list, _ := db.UserSessions(ctx, m.ID, now)
	if len(list) != 2 || string(list[0].TokenHash) != "hash-m-2-xxxxxxx" {
		t.Fatalf("sessions: %d", len(list))
	}
	if err := db.DeleteUserSessionByID(ctx, owner.ID, SessionID(list[0].TokenHash)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's session: %v", err)
	}
	if err := db.DeleteUserSessionByID(ctx, m.ID, SessionID(list[0].TokenHash)); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.UserSessions(ctx, m.ID, now); len(list) != 1 {
		t.Fatal("session not deleted")
	}

	// The last active owner can be neither demoted, disabled nor deleted.
	member, yes := RoleMember, true
	if _, err := db.UpdateUser(ctx, owner.ID, UserChange{Role: &member}); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demote: %v", err)
	}
	if _, err := db.UpdateUser(ctx, owner.ID, UserChange{Disabled: &yes}); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("disable: %v", err)
	}
	if err := db.DeleteUser(ctx, owner.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("delete: %v", err)
	}
	name := "Mia"
	u, err := db.UpdateUser(ctx, m.ID, UserChange{Name: &name, Disabled: &yes})
	if err != nil || u.Name != "Mia" || !u.Disabled || u.Role != RoleMember {
		t.Fatalf("update: %+v %v", u, err)
	}
	if list, _ := db.UserSessions(ctx, m.ID, now); len(list) != 0 {
		t.Fatal("disabling kept sessions")
	}
	ownerRole := RoleOwner
	db.UpdateUser(ctx, m.ID, UserChange{Role: &ownerRole})
	// A disabled owner does not count.
	if err := db.DeleteUser(ctx, owner.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("delete with only a disabled owner left: %v", err)
	}
	no := false
	db.UpdateUser(ctx, m.ID, UserChange{Disabled: &no})
	db.CreateAPIToken(ctx, &APIToken{UserID: owner.ID, Name: "t", Prefix: "p", Role: RoleViewer}, []byte("tok"))
	if err := db.DeleteUser(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.APITokenByHash(ctx, []byte("tok"), now); !errors.Is(err, ErrNotFound) {
		t.Fatal("token of a deleted user kept")
	}
	if _, err := db.UpdateUser(ctx, 999, UserChange{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
