package agentproto

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/wiefix/basalt/internal/docker"
)

// Limits on app settings. The panel checks them on input; the agent checks
// them again before touching Docker.
const (
	MaxEnvVars      = 200
	MaxEnvValueSize = 32 << 10
	MaxVolumes      = 20
)

var (
	envKeyRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	volumeNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// ValidateEnv checks environment variable names and values.
func ValidateEnv(env map[string]string) error {
	if len(env) > MaxEnvVars {
		return fmt.Errorf("at most %d environment variables", MaxEnvVars)
	}
	for k, v := range env {
		if !envKeyRE.MatchString(k) {
			return fmt.Errorf("invalid variable name %q (letters, digits and _, not starting with a digit)", k)
		}
		if len(v) > MaxEnvValueSize {
			return fmt.Errorf("value of %s is too long", k)
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("value of %s contains a NUL byte", k)
		}
	}
	return nil
}

// ValidateVolumes checks persistent volume names and mount paths.
func ValidateVolumes(vols []AppVolume) error {
	if len(vols) > MaxVolumes {
		return fmt.Errorf("at most %d volumes", MaxVolumes)
	}
	names, paths := map[string]bool{}, map[string]bool{}
	for _, v := range vols {
		if !volumeNameRE.MatchString(v.Name) {
			return fmt.Errorf("invalid volume name %q (a-z, 0-9, _ and -)", v.Name)
		}
		if !path.IsAbs(v.Path) || path.Clean(v.Path) != v.Path || v.Path == "/" || len(v.Path) > 512 ||
			strings.ContainsAny(v.Path, ",:\x00") {
			return fmt.Errorf("volume %s: mount path must be a clean absolute path other than /", v.Name)
		}
		if names[v.Name] || paths[v.Path] {
			return fmt.Errorf("volume %s or its path is used twice", v.Name)
		}
		names[v.Name], paths[v.Path] = true, true
	}
	return nil
}

// Validate checks a complete deployment spec.
func (s *AppSpec) Validate() error {
	if s.AppID <= 0 || s.DeploymentID <= 0 {
		return errors.New("invalid app or deployment id")
	}
	if !docker.ValidRef(s.Image) {
		return fmt.Errorf("invalid image reference %q", s.Image)
	}
	if s.Port < 0 || s.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if s.MemoryMB < 0 || s.MemoryMB > 1<<20 {
		return errors.New("invalid memory limit")
	}
	if s.CPUs < 0 || s.CPUs > 1024 {
		return errors.New("invalid CPU limit")
	}
	if err := ValidateEnv(s.Env); err != nil {
		return err
	}
	return ValidateVolumes(s.Volumes)
}
