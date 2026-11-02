// Package sysuser names and looks up the system users and groups that
// isolate sites from each other on Linux. It only reads: creating and
// removing them is the privileged helper's (internal/privhelper).
package sysuser

import (
	"errors"
	"fmt"
	"os/user"
	"regexp"
	"strconv"
)

var nameRE = regexp.MustCompile(`^basalt-[a-z0-9-]{1,24}$`)

var ErrInvalidName = errors.New("sysuser: name must match basalt-[a-z0-9-]+")

// SiteUser is the system user (and its private group) of a site.
func SiteUser(siteID int64) string { return "basalt-s" + strconv.FormatInt(siteID, 10) }

// Check tells whether name is one Basalt may create or remove.
func Check(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
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
