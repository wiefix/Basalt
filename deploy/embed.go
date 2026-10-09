// Package deploy holds installation assets embedded in the binary.
package deploy

import _ "embed"

// InstallScript installs Basalt on a Debian/Ubuntu host (panel or agent).
//
//go:embed install.sh
var InstallScript []byte
