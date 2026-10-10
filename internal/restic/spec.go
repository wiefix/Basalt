package restic

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
)

// FromSpec turns the panel's description of a repository into what
// restic opens. SFTP keys and known hosts are kept in sshDir. A local
// directory is used as given: callers check where it may be.
func FromSpec(spec agentproto.BackupRepo, sshDir string) (Repo, error) {
	if err := spec.Validate(); err != nil {
		return Repo{}, err
	}
	r := Repo{Password: spec.Password, Env: map[string]string{}}
	switch spec.Kind {
	case agentproto.StorageS3:
		r.Location = "s3:" + strings.TrimSuffix(spec.Endpoint, "/") + "/" + spec.Bucket
		if p := strings.Trim(spec.Path, "/"); p != "" {
			r.Location += "/" + p
		}
		r.Env["AWS_ACCESS_KEY_ID"], r.Env["AWS_SECRET_ACCESS_KEY"] = spec.AccessKey, spec.SecretKey
		if spec.Region != "" {
			r.Env["AWS_DEFAULT_REGION"] = spec.Region
		}
	case agentproto.StorageSFTP:
		key, knownHosts, err := sshFiles(spec, sshDir)
		if err != nil {
			return Repo{}, err
		}
		r.Location = "sftp:" + spec.User + "@" + spec.Host + ":" + spec.Path
		// restic splits sftp.command on spaces; every part here is free of them.
		r.Options = []string{"sftp.command=ssh -i " + key + " -p " + strconv.Itoa(spec.Port) +
			" -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new" +
			" -o UserKnownHostsFile=" + knownHosts + " -o ConnectTimeout=20 -o ServerAliveInterval=30" +
			" " + spec.User + "@" + spec.Host + " -s sftp"}
	case agentproto.StorageLocal:
		r.Location = spec.Path
	}
	return r, nil
}

// sshFiles writes the storage's SSH key and returns it with the storage's
// known_hosts file (the host key is accepted on first use).
func sshFiles(spec agentproto.BackupRepo, dir string) (key, knownHosts string, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	if strings.ContainsAny(dir, " \t") {
		return "", "", errors.New("the data directory must not contain spaces for SFTP backups")
	}
	sum := sha256.Sum256([]byte(spec.SSHKey))
	key = filepath.Join(dir, hex.EncodeToString(sum[:8])+".key")
	if _, err := os.Stat(key); err != nil {
		data := spec.SSHKey
		if !strings.HasSuffix(data, "\n") {
			data += "\n"
		}
		tmp := key + ".tmp"
		if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
			return "", "", err
		}
		if err := os.Rename(tmp, key); err != nil {
			return "", "", err
		}
	}
	return key, filepath.Join(dir, "known_hosts_"+spec.Host+"_"+strconv.Itoa(spec.Port)), nil
}
