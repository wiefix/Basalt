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

func TestSnapshotFilesRequest(t *testing.T) {
	repo := BackupRepo{Kind: StorageLocal, Path: "/var/backups/basalt", Password: "pw"}
	ok := func(r SnapshotFilesRequest) SnapshotFilesRequest {
		r.Repo, r.Snapshot = repo, "0123abcd"
		return r
	}
	for _, r := range []SnapshotFilesRequest{
		{Resource: "site-3", SiteID: 3},
		{Resource: "site-3", SiteID: 3, Part: "files", Path: "public/x"},
		{Resource: "site-3", SiteID: 3, Part: "mariadb-s3_wp"},
		{Resource: "app-4", AppID: 4, Part: "volume-data", Path: "a/b"},
		{Resource: "app-4", AppID: 4, Part: "dump-shop"},
	} {
		if r := ok(r); r.Validate() != nil {
			t.Errorf("%+v: %v", r, r.Validate())
		}
	}
	for name, r := range map[string]SnapshotFilesRequest{
		"site of another resource": {Resource: "site-4", SiteID: 3},
		"site and app":             {Resource: "app-4", AppID: 4, SiteID: 3, Part: "volume-data"},
		"site volume":              {Resource: "site-3", SiteID: 3, Part: "volume-data"},
		"app files":                {Resource: "app-4", AppID: 4},
		"app mariadb":              {Resource: "app-4", AppID: 4, Part: "mariadb-x"},
		"site app dump":            {Resource: "site-3", SiteID: 3, Part: "dump-x"},
		"bad volume":               {Resource: "app-4", AppID: 4, Part: "volume-../x"},
		"bad database":             {Resource: "app-4", AppID: 4, Part: "dump-a;b"},
		"path in a dump":           {Resource: "app-4", AppID: 4, Part: "dump-shop", Path: "x"},
		"control in path":          {Resource: "app-4", AppID: 4, Part: "volume-data", Path: "a\nb"},
	} {
		if r := ok(r); r.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
