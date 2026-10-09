// Package sysuser manages the system users and groups that isolate sites
// from each other on Linux. Commands are run with argument vectors (never a
// shell) and names are validated before use.
package sysuser

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
)

var nameRE = regexp.MustCompile(`^basalt-[a-z0-9-]{1,24}$`)

var ErrInvalidName = errors.New("sysuser: name must match basalt-[a-z0-9-]+")

// SiteUser is the system user (and its private group) of a site.
func SiteUser(siteID int64) string { return "basalt-s" + strconv.FormatInt(siteID, 10) }

func check(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LookupUser returns uid and primary gid.
func LookupUser(name string) (uid, gid int, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return -1, -1, err
	}
	uid, _ = strconv.Atoi(u.Uid)
	gid, _ = strconv.Atoi(u.Gid)
	return uid, gid, nil
}

func LookupGroup(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(g.Gid)
}

// EnsureGroup creates a system group if it does not exist.
func EnsureGroup(ctx context.Context, name string) (int, error) {
	if err := check(name); err != nil {
		return -1, err
	}
	if gid, err := LookupGroup(name); err == nil {
		return gid, nil
	}
	if err := run(ctx, "groupadd", "--system", name); err != nil {
		return -1, err
	}
	return LookupGroup(name)
}

// EnsureUser creates a locked system user with its own group, no login shell
// and the given home directory (not created).
func EnsureUser(ctx context.Context, name, home string) (uid, gid int, err error) {
	if err := check(name); err != nil {
		return -1, -1, err
	}
	if uid, gid, err := LookupUser(name); err == nil {
		return uid, gid, nil
	}
	if err := run(ctx, "useradd", "--system", "--user-group", "--no-create-home",
		"--home-dir", home, "--shell", "/usr/sbin/nologin", name); err != nil {
		return -1, -1, err
	}
	return LookupUser(name)
}

// AddToGroup adds an existing user (which may be outside the basalt-
// namespace, e.g. the proxy's user) to a basalt group.
func AddToGroup(ctx context.Context, userName, group string) error {
	if err := check(group); err != nil {
		return err
	}
	if _, err := user.Lookup(userName); err != nil {
		return err
	}
	return run(ctx, "usermod", "--append", "--groups", group, userName)
}

// DeleteUser removes a user and its private group; a missing user is not an error.
func DeleteUser(ctx context.Context, name string) error {
	if err := check(name); err != nil {
		return err
	}
	if _, err := user.Lookup(name); err != nil {
		return nil
	}
	return run(ctx, "userdel", name)
}
