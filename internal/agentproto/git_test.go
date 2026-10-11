package agentproto

import (
	"strings"
	"testing"
)

func TestValidateGitURL(t *testing.T) {
	for _, ok := range []string{
		"https://github.com/org/repo.git", "https://git.example.com:8443/team/app",
		"ssh://git@github.com/org/repo.git", "ssh://git@host:2222/org/repo.git",
		"git@github.com:org/repo.git", "deploy@git.example.com:team/app.git",
	} {
		if err := ValidateGitURL(ok); err != nil {
			t.Errorf("ValidateGitURL(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "file:///etc", "ext::sh -c id", "http://github.com/org/repo", "git://github.com/org/repo",
		"https://user:token@github.com/org/repo", "ssh://git:pw@host/org/repo", "https://github.com/",
		"https://github.com/org/repo?x=1", "https://github.com/org/repo#main", "-uhttps://x/y",
		"https://github.com/org/re po", "https://github.com/org/repo\n", "/local/path", "../repo",
		"git@host:org/repo;id", strings.Repeat("a", 1025),
	} {
		if err := ValidateGitURL(bad); err == nil {
			t.Errorf("ValidateGitURL(%q) accepted", bad)
		}
	}
	if !IsSSHGitURL("git@github.com:o/r.git") || IsSSHGitURL("https://github.com/o/r") {
		t.Error("IsSSHGitURL")
	}
}

func TestValidateBranchAndPaths(t *testing.T) {
	for _, ok := range []string{"main", "release/1.2", "feature_x-y.z"} {
		if err := ValidateBranch(ok); err != nil {
			t.Errorf("ValidateBranch(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "--upload-pack=x", "-x", "/main", "main/", "a..b", "a//b", "x.lock", "a b", "a~1", "a:b"} {
		if ValidateBranch(bad) == nil {
			t.Errorf("ValidateBranch(%q) accepted", bad)
		}
	}
	for _, ok := range []string{"Dockerfile", "docker/web/Dockerfile"} {
		if err := ValidateRepoPath(ok, false); err != nil {
			t.Errorf("ValidateRepoPath(%q) = %v", ok, err)
		}
	}
	if err := ValidateRepoPath(".", true); err != nil {
		t.Errorf("context . refused: %v", err)
	}
	for _, bad := range []string{"", ".", "/etc/passwd", "../x", "..", "a/../../x", "a/./b", "a//b", "x\x00"} {
		if ValidateRepoPath(bad, false) == nil {
			t.Errorf("ValidateRepoPath(%q) accepted", bad)
		}
	}
	g := GitSource{URL: "https://github.com/o/r", Branch: "main", Dockerfile: "Dockerfile", Context: ".",
		Commit: strings.Repeat("ab", 20)}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	g.Commit = "HEAD~1"
	if g.Validate() == nil {
		t.Error("symbolic commit accepted")
	}
}
