package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestPasskeys(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	owner, _ := db.CreateFirstOwner(ctx, "o@example.com", "O", "x")
	m, _ := db.CreateUser(ctx, "m@example.com", "M", "x", RoleMember)
	h1, err := db.WebAuthnHandle(ctx, owner.ID)
	if err != nil || len(h1) != 32 {
		t.Fatalf("handle %x %v", h1, err)
	}
	if h2, _ := db.WebAuthnHandle(ctx, owner.ID); !bytes.Equal(h1, h2) {
		t.Fatal("handle changed")
	}
	if u, err := db.UserByWebAuthnHandle(ctx, h1); err != nil || u.ID != owner.ID {
		t.Fatalf("by handle: %v %v", u, err)
	}
	k := &Passkey{UserID: owner.ID, CredentialID: []byte("cred-1"), PublicKey: []byte("key"), Alg: -7, SignCount: 3, RPID: "panel.example.com", Name: "laptop"}
	if err := db.CreatePasskey(ctx, k); err != nil {
		t.Fatal(err)
	}
	if err := db.CreatePasskey(ctx, &Passkey{UserID: m.ID, CredentialID: []byte("cred-1"), PublicKey: []byte("k"), RPID: "x", Name: "a"}); !errors.Is(err, ErrPasskeyExists) {
		t.Fatalf("same credential: %v", err)
	}
	if err := db.CreatePasskey(ctx, &Passkey{UserID: owner.ID, CredentialID: []byte("cred-2"), PublicKey: []byte("k"), RPID: "x", Name: "laptop"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same name: %v", err)
	}
	// Names are per user.
	if err := db.CreatePasskey(ctx, &Passkey{UserID: m.ID, CredentialID: []byte("cred-3"), PublicKey: []byte("k"), RPID: "x", Name: "laptop"}); err != nil {
		t.Fatal(err)
	}
	// The counter never goes back.
	db.PasskeyUsed(ctx, k.ID, 10, true, time.Now())
	db.PasskeyUsed(ctx, k.ID, 4, true, time.Now())
	got, _ := db.PasskeyByCredentialID(ctx, []byte("cred-1"))
	if got.SignCount != 10 || !got.BackedUp || got.LastUsedAt == nil || got.Name != "laptop" {
		t.Fatalf("passkey %+v", got)
	}
	// Another user's passkeys cannot be renamed or removed.
	if err := db.RenamePasskey(ctx, m.ID, k.ID, "mine"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename another's: %v", err)
	}
	if err := db.DeletePasskey(ctx, m.ID, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete another's: %v", err)
	}
	if n, _ := db.CountPasskeys(ctx); n[owner.ID] != 1 || n[m.ID] != 1 {
		t.Fatalf("counts %v", n)
	}
	for i := range MaxPasskeys - 1 {
		if err := db.CreatePasskey(ctx, &Passkey{UserID: owner.ID, CredentialID: []byte{byte(i), 9}, PublicKey: []byte("k"), RPID: "x", Name: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreatePasskey(ctx, &Passkey{UserID: owner.ID, CredentialID: []byte("x"), PublicKey: []byte("k"), RPID: "x", Name: "one more"}); !errors.Is(err, ErrTooManyPasskeys) {
		t.Fatalf("limit: %v", err)
	}
	if err := db.DeleteUserPasskeys(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.Passkeys(ctx, owner.ID); len(list) != 0 {
		t.Fatal("passkeys kept")
	}
	// Deleting a user deletes their passkeys.
	db.DeleteUser(ctx, m.ID)
	if _, err := db.PasskeyByCredentialID(ctx, []byte("cred-3")); !errors.Is(err, ErrNotFound) {
		t.Fatal("passkey of a deleted user kept")
	}
}
