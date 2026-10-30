package agentproto

import (
	"fmt"
	"regexp"
)

// DBUpgrade moves a PostgreSQL database to a newer major version during
// its deployment: with the database stopped, a container of the old
// version (From, without a network) dumps everything and one of the new
// version (the deployed service) loads it into the new version's
// directory of the volume, To/docker. The old data stays until the new
// version runs; then it is removed.
type DBUpgrade struct {
	Service string      `json:"service"`
	From    ServiceSpec `json:"from"`
	To      string      `json:"to"` // the new major version
}

var majorRE = regexp.MustCompile(`^[1-9][0-9]$`)

func (u *DBUpgrade) validate(s *AppSpec, vols map[string]bool) error {
	i := -1
	for j := range s.Services {
		if s.Services[j].Name == u.Service {
			i = j
		}
	}
	if i < 0 || len(s.Services) != 1 {
		return fmt.Errorf("upgrade of unknown service %q", u.Service)
	}
	if !majorRE.MatchString(u.To) {
		return fmt.Errorf("invalid version %q", u.To)
	}
	if u.From.Name != u.Service {
		return fmt.Errorf("upgrade of %q from service %q", u.Service, u.From.Name)
	}
	if err := u.From.Validate(vols); err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	// Both mount the same single volume.
	to := s.Services[i]
	if len(u.From.Mounts) != 1 || len(to.Mounts) != 1 || u.From.Mounts[0].Volume != to.Mounts[0].Volume || u.From.Mounts[0].ReadOnly {
		return fmt.Errorf("upgrade of %s: the old and new version must use the same volume", u.Service)
	}
	return nil
}
