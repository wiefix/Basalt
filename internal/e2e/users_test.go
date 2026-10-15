package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestUserManagement: admins change, disable, delete users and reset their
// passwords within their own rank; users see and end their sessions.
func TestUserManagement(t *testing.T) {
	p := startPanel(t)
	login := func(email, pw string) *panel {
		t.Helper()
		c := *p
		c.hc = &http.Client{Jar: newJar()}
		c.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": email, "password": pw})
		return &c
	}
	newUser := func(email, role string) float64 {
		t.Helper()
		u := p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
			"email": email, "name": "N", "password": "user password 12", "role": role,
		})["user"].(map[string]any)
		return u["id"].(float64)
	}
	ownerID := p.call(200, "GET", "/api/v1/auth/me", nil)["user"].(map[string]any)["id"].(float64)
	adminID := newUser("a@example.com", "admin")
	memberID := newUser("m@example.com", "member")
	admin := login("a@example.com", "user password 12")
	member := login("m@example.com", "user password 12")
	member2 := login("m@example.com", "user password 12")

	// Sessions: the member sees both, ends the other one.
	sess := member.call(200, "GET", "/api/v1/account/sessions", nil)["sessions"].([]any)
	if len(sess) != 2 {
		t.Fatalf("sessions: %v", sess)
	}
	var current, other string
	for _, x := range sess {
		m := x.(map[string]any)
		if m["current"] == true {
			current = m["id"].(string)
		} else {
			other = m["id"].(string)
		}
		if m["ip"] == "" || m["user_agent"] == "" {
			t.Fatalf("session view: %v", m)
		}
	}
	member.call(http.StatusUnprocessableEntity, "DELETE", "/api/v1/account/sessions/"+current, nil)
	p.call(http.StatusNotFound, "DELETE", "/api/v1/account/sessions/"+other, nil) // not the owner's
	member.call(http.StatusNoContent, "DELETE", "/api/v1/account/sessions/"+other, nil)
	member2.call(http.StatusUnauthorized, "GET", "/api/v1/auth/me", nil)
	member2 = login("m@example.com", "user password 12")
	member.call(http.StatusNoContent, "DELETE", "/api/v1/account/sessions", nil)
	member2.call(http.StatusUnauthorized, "GET", "/api/v1/auth/me", nil)
	member.call(200, "GET", "/api/v1/auth/me", nil)

	path := func(id float64) string { return fmt.Sprintf("/api/v1/users/%d", int64(id)) }
	// Rank: an admin cannot touch an owner nor grant owner; members manage nobody.
	admin.call(http.StatusForbidden, "PUT", path(ownerID), map[string]any{"name": "X"})
	admin.call(http.StatusForbidden, "DELETE", path(ownerID), nil)
	admin.call(http.StatusForbidden, "PUT", path(ownerID)+"/password", map[string]any{"password": "new password 123"})
	admin.call(http.StatusForbidden, "PUT", path(memberID), map[string]any{"role": "owner"})
	member.call(http.StatusForbidden, "PUT", path(memberID), map[string]any{"name": "Me"})
	// Nobody changes their own role, disables or deletes themselves.
	admin.call(http.StatusUnprocessableEntity, "PUT", path(adminID), map[string]any{"role": "viewer"})
	admin.call(http.StatusUnprocessableEntity, "DELETE", path(adminID), nil)
	p.call(http.StatusUnprocessableEntity, "PUT", path(ownerID), map[string]any{"disabled": true})
	p.call(http.StatusUnprocessableEntity, "PUT", path(memberID), map[string]any{"role": "god"})
	p.call(http.StatusUnprocessableEntity, "PUT", path(memberID), map[string]any{"name": ""})
	p.call(http.StatusNotFound, "PUT", "/api/v1/users/999", map[string]any{"name": "X"})
	// Renaming oneself is fine.
	if u := admin.call(200, "PUT", path(adminID), map[string]any{"name": "Ada"})["user"].(map[string]any); u["name"] != "Ada" {
		t.Fatalf("rename: %v", u)
	}

	// Role changes apply to the next request.
	admin.call(200, "PUT", path(memberID), map[string]any{"role": "viewer"})
	if me := member.call(200, "GET", "/api/v1/auth/me", nil)["user"].(map[string]any); me["role"] != "viewer" {
		t.Fatalf("role: %v", me)
	}
	// Disabling signs out (login and tokens of disabled users: auth and token tests).
	admin.call(200, "PUT", path(memberID), map[string]any{"disabled": true})
	member.call(http.StatusUnauthorized, "GET", "/api/v1/auth/me", nil)
	admin.call(200, "PUT", path(memberID), map[string]any{"disabled": false})
	member = login("m@example.com", "user password 12")

	// A password reset signs the user out; the new password works.
	admin.call(http.StatusUnprocessableEntity, "PUT", path(memberID)+"/password", map[string]any{"password": "short"})
	admin.call(http.StatusUnprocessableEntity, "PUT", path(adminID)+"/password", map[string]any{"password": "new password 123"})
	admin.call(http.StatusNoContent, "PUT", path(memberID)+"/password", map[string]any{"password": "new password 123", "reset_totp": true})
	member.call(http.StatusUnauthorized, "GET", "/api/v1/auth/me", nil)
	login("m@example.com", "new password 123")

	// The last owner stays: an owner cannot be demoted while alone.
	secondOwner := newUser("o2@example.com", "owner")
	o2 := login("o2@example.com", "user password 12")
	o2.call(200, "PUT", path(ownerID), map[string]any{"role": "admin"})
	p.call(http.StatusForbidden, "PUT", path(secondOwner), map[string]any{"role": "admin"}) // now an admin
	o2.call(http.StatusUnprocessableEntity, "PUT", path(secondOwner), map[string]any{"role": "admin"})
	o2.call(http.StatusNoContent, "DELETE", path(memberID), nil)
	o2.call(http.StatusNotFound, "DELETE", path(memberID), nil)
	o2.call(http.StatusNoContent, "DELETE", path(ownerID), nil)
	p.call(http.StatusUnauthorized, "GET", "/api/v1/auth/me", nil)

	var audit string
	p.db.R.QueryRow(`SELECT group_concat(action) FROM (SELECT action FROM audit_log WHERE action LIKE 'user.%' OR action LIKE 'session.%' ORDER BY id)`).Scan(&audit)
	for _, want := range []string{"session.ended", "session.ended_others", "user.updated", "user.password_reset", "user.deleted"} {
		if !strings.Contains(audit, want) {
			t.Fatalf("audit lacks %s: %s", want, audit)
		}
	}
}
