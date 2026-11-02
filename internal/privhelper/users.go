package privhelper

import (
	"context"
	"fmt"
	"os/exec"
	"os/user"
	"strings"

	"github.com/wiefix/basalt/internal/sysuser"
)

// The programs below change the host's users; only this package runs
// them (internal/rules).

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureGroup creates a system group if it does not exist.
func ensureGroup(ctx context.Context, name string) error {
	if err := sysuser.Check(name); err != nil {
		return err
	}
	if _, err := sysuser.LookupGroup(name); err == nil {
		return nil
	}
	return run(ctx, "groupadd", "--system", name)
}

// ensureUser creates a locked system user with its own group, no login
// shell and the given home directory (not created).
func ensureUser(ctx context.Context, name, home string) (uid, gid int, err error) {
	if err := sysuser.Check(name); err != nil {
		return -1, -1, err
	}
	if uid, gid, err := sysuser.LookupUser(name); err == nil {
		return uid, gid, nil
	}
	if err := run(ctx, "useradd", "--system", "--user-group", "--no-create-home",
		"--home-dir", home, "--shell", "/usr/sbin/nologin", name); err != nil {
		return -1, -1, err
	}
	return sysuser.LookupUser(name)
}

// addToGroup adds an existing user (which may be outside the basalt-
// namespace, e.g. the proxy's user) to a basalt group.
func addToGroup(ctx context.Context, userName, group string) error {
	if err := sysuser.Check(group); err != nil {
		return err
	}
	if _, err := user.Lookup(userName); err != nil {
		return err
	}
	return run(ctx, "usermod", "--append", "--groups", group, userName)
}

// deleteUser removes a user and its private group; a missing user is not
// an error.
func deleteUser(ctx context.Context, name string) error {
	if err := sysuser.Check(name); err != nil {
		return err
	}
	if _, err := user.Lookup(name); err != nil {
		return nil
	}
	return run(ctx, "userdel", name)
}
