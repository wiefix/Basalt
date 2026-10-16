package agentproto

import "testing"

func TestVerifyRequest(t *testing.T) {
	repo := BackupRepo{Kind: StorageLocal, Path: "/var/backups/basalt/server-1", Password: "pw"}
	for _, ok := range []string{"", "1/52", "52/52", "7/100", "1/1"} {
		r := VerifyRequest{Repo: repo, ReadData: ok}
		if err := r.Validate(); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"0/52", "53/52", "100/52", "1/0", "1/", "/52", "5%", "1/52 ", "--read-data", "01/52", "1/99999"} {
		r := VerifyRequest{Repo: repo, ReadData: bad}
		if r.Validate() == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	r := VerifyRequest{Repo: BackupRepo{Kind: StorageLocal, Path: "relative", Password: "pw"}}
	if r.Validate() == nil {
		t.Error("invalid repository accepted")
	}
}
