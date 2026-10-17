// Package release reads Basalt releases: the signed checksum list
// (SHA256SUMS, signed with ssh-keygen -Y sign -n basalt-release) names
// the binaries, the version file and their SHA-256 hashes.
package release

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/wiefix/basalt/internal/sshsig"
)

// Files of a release.
const (
	SumsFile    = "SHA256SUMS"
	SigFile     = "SHA256SUMS.sig"
	VersionFile = "version.txt"
)

// BinaryName is the release file of the Linux binary for arch.
func BinaryName(arch string) string { return "basalt-linux-" + arch }

var (
	nameRE    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	versionRE = regexp.MustCompile(`^v(\d{1,4})\.(\d{1,4})\.(\d{1,4})(-[0-9A-Za-z.-]{1,32})?$`)
)

// Sums are the checked hashes of a release's files, by file name.
type Sums map[string]string

// VerifySums checks that sig is the release key's signature of sums and
// parses them ("<hex>  <name>" lines, as sha256sum writes).
func VerifySums(sums, sig []byte, key string) (Sums, error) {
	if err := sshsig.Verify(sums, sig, key, sshsig.ReleaseNamespace); err != nil {
		return nil, fmt.Errorf("the release is not signed by the Basalt release key: %w", err)
	}
	out := Sums{}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		hash, name, ok := strings.Cut(line, " ")
		name = strings.TrimPrefix(strings.TrimSpace(name), "*")
		if b, err := hex.DecodeString(hash); !ok || err != nil || len(b) != 32 || !nameRE.MatchString(name) {
			return nil, fmt.Errorf("invalid checksum line %q", line)
		}
		out[name] = strings.ToLower(hash)
	}
	if len(out) == 0 {
		return nil, errors.New("empty checksum list")
	}
	return out, nil
}

// ValidVersion reports whether v is a release version: vMAJOR.MINOR.PATCH
// with an optional pre-release suffix.
func ValidVersion(v string) bool { return versionRE.MatchString(v) }

// Newer reports whether release version a is newer than b. A build that is
// not a release ("dev") is older than any release.
func Newer(a, b string) bool {
	pa, pb := versionRE.FindStringSubmatch(a), versionRE.FindStringSubmatch(b)
	if pa == nil {
		return false
	}
	if pb == nil {
		return true
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	// 1.2.0 is newer than 1.2.0-rc.1; pre-releases compare as text.
	switch {
	case pa[4] == pb[4]:
		return false
	case pa[4] == "":
		return true
	case pb[4] == "":
		return false
	}
	return pa[4] > pb[4]
}
