package catalog

import (
	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/sshsig"
)

// Namespace is what catalog index signatures are made for
// (ssh-keygen -Y sign -n basalt-catalog), so no other signature by the
// same key passes for one.
const Namespace = "basalt-catalog"

// BasaltKey signs Basalt's catalog: the release signing key (the
// namespace keeps release and catalog signatures apart).
const BasaltKey = sshsig.ReleaseKey

// VerifySSHSig checks an armored SSH signature of msg by the trusted key
// (an authorized_keys line) for namespace.
func VerifySSHSig(msg, armored []byte, trusted, namespace string) error {
	return sshsig.Verify(msg, armored, trusted, namespace)
}

// SignSSHSig signs msg for namespace (tests and catalog tooling).
func SignSSHSig(msg []byte, signer ssh.Signer, namespace string) ([]byte, error) {
	return sshsig.Sign(msg, signer, namespace)
}
