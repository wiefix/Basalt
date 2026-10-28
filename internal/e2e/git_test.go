package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/docker"
)

// gitServer is a repository served over HTTPS by git-http-backend, with a
// working copy to commit to.
type gitServer struct {
	t      *testing.T
	URL    string // https://127.0.0.1:port/app.git
	CAFile string
	work   string
	bare   string
}

func startGitServer(t *testing.T, files map[string]string) *gitServer {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	g := &gitServer{t: t, work: filepath.Join(root, "work"), bare: filepath.Join(root, "srv", "app.git")}
	g.git(root, "init", "-q", "-b", "main", g.work)
	g.commit(files, "first")
	g.git(root, "clone", "-q", "--bare", g.work, g.bare)
	g.git(g.work, "remote", "add", "origin", g.bare)

	srv := httptest.NewTLSServer(&cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Join(root, "srv"), "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(srv.Close)
	g.URL = srv.URL + "/app.git"
	g.CAFile = filepath.Join(root, "ca.pem")
	os.WriteFile(g.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644)
	return g
}

func (g *gitServer) git(dir string, args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files, commits them and returns the commit hash.
func (g *gitServer) commit(files map[string]string, msg string) string {
	for name, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(g.work, name)), 0o755)
		os.WriteFile(filepath.Join(g.work, name), []byte(body), 0o644)
	}
	g.git(g.work, "add", "-A")
	g.git(g.work, "commit", "-q", "-m", msg)
	return g.git(g.work, "rev-parse", "HEAD")
}

func (g *gitServer) push() { g.git(g.work, "push", "-q", "origin", "main") }

// addRepo serves another repository, name.git, with files; it returns its
// URL and head commit.
func (g *gitServer) addRepo(name string, files map[string]string) (string, string) {
	g.t.Helper()
	work := filepath.Join(filepath.Dir(g.work), name)
	g.git(filepath.Dir(work), "init", "-q", "-b", "main", work)
	for n, body := range files {
		os.WriteFile(filepath.Join(work, n), []byte(body), 0o644)
	}
	g.git(work, "add", "-A")
	g.git(work, "commit", "-q", "-m", "lib")
	g.git(filepath.Dir(work), "clone", "-q", "--bare", work, filepath.Join(filepath.Dir(g.bare), name+".git"))
	return strings.TrimSuffix(g.URL, "/app.git") + "/" + name + ".git", g.git(work, "rev-parse", "HEAD")
}

// deliver posts a push event for branch signed like GitHub does.
func (p *panel) deliver(appID, secret, branch, after string) (int, map[string]any) {
	p.t.Helper()
	body, _ := json.Marshal(map[string]any{"ref": "refs/heads/" + branch, "after": after})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, _ := http.NewRequest("POST", p.api.URL+"/api/v1/webhooks/apps/"+appID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req) // no session: webhooks are signed
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestGitAppAPIAndWebhooks(t *testing.T) {
	p := startPanel(t)
	h := startHost(t, p) // no Docker: deployments fail on the agent
	sid := mustAtoi(h.id)
	if _, err := p.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	create := func(want int, git map[string]string) map[string]any {
		return p.call(want, "POST", "/api/v1/apps", map[string]any{
			"server_id": sid, "name": "web", "source": "git", "git": git, "port": 8080,
		})
	}
	for _, bad := range []map[string]string{
		{"url": "file:///etc"}, {"url": "ext::sh -c id"}, {"url": "http://github.com/o/r"},
		{"url": "https://user:tok@github.com/o/r"}, {"url": "https://github.com/o/r", "branch": "--upload-pack=x"},
		{"url": "https://github.com/o/r", "dockerfile": "../../etc/passwd"}, {"url": "https://github.com/o/r", "context": "/"},
	} {
		create(http.StatusUnprocessableEntity, bad)
	}
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/apps", map[string]any{"server_id": sid, "name": "x", "source": "svn"})

	app := create(http.StatusCreated, map[string]string{"url": "git@github.com:org/web.git"})["app"].(map[string]any)
	aid := jsonID(app["id"])
	git := app["git"].(map[string]any)
	if app["source"] != "git" || git["branch"] != "main" || git["dockerfile"] != "Dockerfile" || git["context"] != "." || app["auto_deploy"] != true {
		t.Fatalf("app = %v", app)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(app["deploy_key"].(string)))
	if err != nil || key.Type() != "ssh-ed25519" {
		t.Fatalf("deploy key %v: %v", app["deploy_key"], err)
	}
	var sealed []byte
	p.db.R.QueryRow(`SELECT deploy_key FROM apps WHERE id = ?`, mustAtoi(aid)).Scan(&sealed)
	if bytes.Contains(sealed, []byte("PRIVATE KEY")) {
		t.Fatal("deploy key stored in clear")
	}
	base := "/api/v1/apps/" + aid
	newKey := p.call(200, "POST", base+"/deploy-key", nil)["app"].(map[string]any)["deploy_key"]
	if newKey == app["deploy_key"] {
		t.Fatal("deploy key not replaced")
	}

	hook := p.call(200, "GET", base+"/webhook", nil)["webhook"].(map[string]any)
	secret := hook["secret"].(string)
	if len(secret) != 64 || hook["path"] != "/api/v1/webhooks/apps/"+aid {
		t.Fatalf("webhook = %v", hook)
	}
	// Signatures are checked; unknown apps look the same as bad signatures.
	if code, _ := p.deliver(aid, "wrong secret", "main", strings.Repeat("a", 40)); code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", code)
	}
	if code, _ := p.deliver("999", secret, "main", strings.Repeat("a", 40)); code != http.StatusUnauthorized {
		t.Fatalf("unknown app: %d", code)
	}
	if code, r := p.deliver(aid, secret, "develop", strings.Repeat("a", 40)); code != http.StatusAccepted || !strings.Contains(fmt.Sprint(r["result"]), "another branch") {
		t.Fatalf("other branch: %d %v", code, r)
	}
	commit := strings.Repeat("ab", 20)
	if code, r := p.deliver(aid, secret, "main", commit); code != http.StatusAccepted || r["result"] != "deploying" {
		t.Fatalf("push: %d %v", code, r)
	}
	deps := p.call(200, "GET", base+"/deployments", nil)["deployments"].([]any)
	if len(deps) != 1 {
		t.Fatalf("deployments after a push: %v", deps)
	}
	var audit string
	p.db.R.QueryRow(`SELECT details FROM audit_log WHERE action = 'app.deploy_started'`).Scan(&audit)
	if !strings.Contains(audit, `"trigger":"webhook"`) || !strings.Contains(audit, commit) {
		t.Fatalf("audit = %s", audit)
	}

	// Rotating the secret invalidates the old one; switching auto-deploy off ignores pushes.
	hook = p.call(200, "PUT", base+"/webhook", map[string]any{"auto_deploy": false, "regenerate": true})["webhook"].(map[string]any)
	if code, _ := p.deliver(aid, secret, "main", commit); code != http.StatusUnauthorized {
		t.Fatalf("old secret: %d", code)
	}
	if code, r := p.deliver(aid, hook["secret"].(string), "main", commit); code != http.StatusAccepted || !strings.Contains(fmt.Sprint(r["result"]), "off") {
		t.Fatalf("auto-deploy off: %d %v", code, r)
	}

	// Only admins see the secret; image apps have no webhook.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "m@example.com", "name": "M", "password": "member password 1", "role": "member"})
	m := *p
	m.hc = &http.Client{Jar: newJar()}
	m.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "m@example.com", "password": "member password 1"})
	m.call(http.StatusForbidden, "GET", base+"/webhook", nil)
	img := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": sid, "name": "img", "image": "nginx:1.27", "port": 80})
	p.call(http.StatusNotFound, "GET", "/api/v1/apps/"+jsonID(img["app"].(map[string]any)["id"])+"/webhook", nil)
}

// TestGitAppWithDocker builds an app from a repository: a manual
// deployment, a push delivered by webhook, and a rollback reusing the
// earlier image. Opt-in like the other Docker tests.
func TestGitAppWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	dockerfile := func(name string) map[string]string {
		return map[string]string{"deploy/Dockerfile": "FROM traefik/whoami:v1.10\nENV WHOAMI_NAME=" + name + "\n"}
	}
	g := startGitServer(t, dockerfile("one"))
	p := startPanel(t)
	h := startHostWith(t, p, sock, func(c *agent.Config) { c.GitCAFile = g.CAFile })
	dc := docker.New(sock)

	r := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": mustAtoi(h.id), "name": "built", "source": "git", "port": 80,
		"git":     map[string]string{"url": g.URL, "dockerfile": "deploy/Dockerfile"},
		"domains": []string{"built.localhost"}, "tls": "internal",
	})
	aid := jsonID(r["app"].(map[string]any)["id"])
	base := "/api/v1/apps/" + aid
	t.Cleanup(func() {
		ctx := context.Background()
		list, _ := dc.ContainerList(ctx, appLabels(aid))
		for _, c := range list {
			dc.ContainerRemove(ctx, c.ID, true)
		}
		dc.NetworkRemove(ctx, appNet(aid))
		for _, d := range p.call(200, "GET", base+"/deployments", nil)["deployments"].([]any) {
			dc.ImageRemove(ctx, d.(map[string]any)["image"].(string))
		}
	})
	served := func(name string) func() bool {
		return func() bool { _, body := h.get("built.localhost", "/"); return strings.Contains(body, "Name: "+name) }
	}

	dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
	d1, log := followDeployment(t, p, base, jsonID(dep["id"]))
	if d1["status"] != "succeeded" || d1["commit_message"] != "first" || !strings.Contains(log, "Image built") {
		t.Fatalf("first deployment: %v\n%s", d1, log)
	}
	eventually(t, "built app served", served("one"))

	// A push delivered by webhook builds exactly the pushed commit.
	sha2 := g.commit(dockerfile("two"), "second")
	g.push()
	secret := p.call(200, "GET", base+"/webhook", nil)["webhook"].(map[string]any)["secret"].(string)
	if code, r := p.deliver(aid, secret, "main", sha2); code != http.StatusAccepted {
		t.Fatalf("webhook: %d %v", code, r)
	}
	latest := p.call(200, "GET", base+"/deployments", nil)["deployments"].([]any)[0].(map[string]any)
	d2, log := followDeployment(t, p, base, jsonID(latest["id"]))
	if d2["status"] != "succeeded" || d2["commit"] != sha2 {
		t.Fatalf("pushed deployment: %v\n%s", d2, log)
	}
	eventually(t, "pushed version served", served("two"))

	// Rolling back reuses the image built for the first commit.
	dep = p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{"rollback_to": mustAtoi(jsonID(d1["id"]))})["deployment"].(map[string]any)
	rb, log := followDeployment(t, p, base, jsonID(dep["id"]))
	if rb["status"] != "succeeded" || rb["commit"] != d1["commit"] || !strings.Contains(log, "Reusing the image") {
		t.Fatalf("rollback: %v\n%s", rb, log)
	}
	eventually(t, "rolled back", served("one"))
	_ = io.Discard
	_ = time.Second
}

// TestRailpackWithDocker: an app from a repository without a Dockerfile is
// built by Railpack (BUILDKIT_SYNTAX frontend, pinned) and answers on the
// port Basalt gives it as $PORT; a repository linking outside itself is
// refused before railpack reads it. Needs the railpack command (RAILPACK
// or PATH).
func TestRailpackWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	railpack := os.Getenv("RAILPACK")
	if railpack == "" {
		railpack = "railpack"
	}
	if _, err := exec.LookPath(railpack); err != nil {
		t.Skip("railpack is not installed")
	}
	// A first build downloads Railpack's images and the language runtime.
	deployWait = 15 * time.Minute
	t.Cleanup(func() { deployWait = 3 * time.Minute })
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	g := startGitServer(t, map[string]string{
		"package.json": `{"name":"hello","version":"1.0.0","scripts":{"build":"node -e \"require('fs').writeFileSync('built.txt', String(process.env.MESSAGE))\"","start":"node server.js"},"engines":{"node":"22"}}`,
		"server.js":    `require("http").createServer((q, s) => s.end("railpack built me, port " + process.env.PORT + ", " + require("fs").readFileSync("built.txt"))).listen(process.env.PORT);`,
	})
	p := startPanel(t)
	h := startHostWith(t, p, sock, func(c *agent.Config) { c.GitCAFile, c.Railpack = g.CAFile, railpack })
	eventually(t, "railpack version reported", func() bool {
		s := p.call(200, "GET", "/api/v1/servers/"+h.id, nil)["server"].(map[string]any)
		return s["railpack"] != nil && s["railpack"] != ""
	})
	dc := docker.New(sock)
	r := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": mustAtoi(h.id), "name": "auto", "source": "git", "port": 3000,
		"git":     map[string]any{"url": g.URL, "builder": "railpack", "build_env": []string{"MESSAGE"}},
		"domains": []string{"auto.localhost"}, "tls": "internal",
	})
	aid := jsonID(r["app"].(map[string]any)["id"])
	base := "/api/v1/apps/" + aid
	p.call(200, "PUT", base+"/env", map[string]any{"env": map[string]string{"MESSAGE": "hello at build time"}})
	t.Cleanup(func() {
		ctx := context.Background()
		list, _ := dc.ContainerList(ctx, appLabels(aid))
		for _, c := range list {
			dc.ContainerRemove(ctx, c.ID, true)
		}
		dc.NetworkRemove(ctx, appNet(aid))
		for _, d := range p.call(200, "GET", base+"/deployments", nil)["deployments"].([]any) {
			dc.ImageRemove(ctx, d.(map[string]any)["image"].(string))
		}
		// The agent's BuildKit builder, if Docker needed one.
		dc.ContainerRemove(ctx, "buildx_buildkit_basalt-"+e2eNamespace+"-railpack0", true)
		dc.VolumeRemove(ctx, "buildx_buildkit_basalt-"+e2eNamespace+"-railpack0_state")
	})
	dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
	d1, log := followDeployment(t, p, base, jsonID(dep["id"]))
	if d1["status"] != "succeeded" || !strings.Contains(log, "Detected: node") || !strings.Contains(log, "Image built") {
		t.Fatalf("deployment: %v\n%s", d1, log)
	}
	eventually(t, "the built app served", func() bool {
		_, body := h.get("auto.localhost", "/")
		return strings.Contains(body, "railpack built me, port 3000, hello at build time")
	})

	// A link to this host's files: refused.
	if err := os.Symlink("/etc/passwd", filepath.Join(g.work, "leak")); err != nil {
		t.Fatal(err)
	}
	g.commit(nil, "link out")
	g.push()
	dep = p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
	d2, log := followDeployment(t, p, base, jsonID(dep["id"]))
	if d2["status"] != "failed" || !strings.Contains(fmt.Sprint(d2["error"]), "outside the repository") {
		t.Fatalf("a link outside: %v\n%s", d2, log)
	}
	eventually(t, "the previous version still served", func() bool {
		_, body := h.get("auto.localhost", "/")
		return strings.Contains(body, "railpack built me")
	})
}

// TestBuildEnvWithDocker: a Dockerfile reads a build variable as a secret
// (RUN --mount=type=secret); the value reaches the build, not the image's
// history.
func TestBuildEnvWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	g := startGitServer(t, map[string]string{"Dockerfile": "FROM busybox:1.37\n" +
		"RUN --mount=type=secret,id=GREETING,env=GREETING mkdir /www && echo \"$GREETING\" > /www/index.html\n" +
		"CMD [\"httpd\", \"-f\", \"-p\", \"80\", \"-h\", \"/www\"]\n"})
	p := startPanel(t)
	h := startHostWith(t, p, sock, func(c *agent.Config) { c.GitCAFile = g.CAFile })
	dc := docker.New(sock)
	r := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": mustAtoi(h.id), "name": "greet", "source": "git", "port": 80,
		"git":     map[string]any{"url": g.URL, "build_env": []string{"GREETING"}},
		"domains": []string{"greet.localhost"}, "tls": "internal",
	})
	aid := jsonID(r["app"].(map[string]any)["id"])
	base := "/api/v1/apps/" + aid
	t.Cleanup(func() {
		ctx := context.Background()
		list, _ := dc.ContainerList(ctx, appLabels(aid))
		for _, c := range list {
			dc.ContainerRemove(ctx, c.ID, true)
		}
		dc.NetworkRemove(ctx, appNet(aid))
		for _, d := range p.call(200, "GET", base+"/deployments", nil)["deployments"].([]any) {
			dc.ImageRemove(ctx, d.(map[string]any)["image"].(string))
		}
	})
	p.call(200, "PUT", base+"/env", map[string]any{"env": map[string]string{"GREETING": "hi from a secret 7f3a"}})
	dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
	d, log := followDeployment(t, p, base, jsonID(dep["id"]))
	if d["status"] != "succeeded" || strings.Contains(log, "7f3a") {
		t.Fatalf("deployment: %v\n%s", d, log)
	}
	eventually(t, "the value built in", func() bool {
		_, body := h.get("greet.localhost", "/")
		return strings.Contains(body, "hi from a secret 7f3a")
	})
	out, err := exec.Command("docker", "--host", "unix://"+sock, "history", "--no-trunc", d["image"].(string)).CombinedOutput()
	if err != nil || strings.Contains(string(out), "7f3a") {
		t.Fatalf("history: %v %s", err, out)
	}
}

// TestSubmodulesWithDocker: a repository's submodules are fetched before the
// build.
func TestSubmodulesWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	g := startGitServer(t, map[string]string{"Dockerfile": "FROM busybox:1.37\nCOPY lib/hello.txt /www/index.html\nCMD [\"httpd\", \"-f\", \"-p\", \"80\", \"-h\", \"/www\"]\n"})
	libURL, libSHA := g.addRepo("lib", map[string]string{"hello.txt": "from the submodule 5c1e\n"})
	os.WriteFile(filepath.Join(g.work, ".gitmodules"), []byte("[submodule \"lib\"]\n\tpath = lib\n\turl = "+libURL+"\n"), 0o644)
	g.git(g.work, "update-index", "--add", "--cacheinfo", "160000,"+libSHA+",lib")
	g.git(g.work, "add", ".gitmodules")
	g.git(g.work, "commit", "-q", "-m", "add the submodule")
	g.push()
	p := startPanel(t)
	h := startHostWith(t, p, sock, func(c *agent.Config) { c.GitCAFile = g.CAFile })
	dc := docker.New(sock)
	r := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": mustAtoi(h.id), "name": "sub", "source": "git", "port": 80,
		"git": map[string]any{"url": g.URL}, "domains": []string{"sub.localhost"}, "tls": "internal",
	})
	aid := jsonID(r["app"].(map[string]any)["id"])
	base := "/api/v1/apps/" + aid
	t.Cleanup(func() {
		ctx := context.Background()
		list, _ := dc.ContainerList(ctx, appLabels(aid))
		for _, c := range list {
			dc.ContainerRemove(ctx, c.ID, true)
		}
		dc.NetworkRemove(ctx, appNet(aid))
		for _, d := range p.call(200, "GET", base+"/deployments", nil)["deployments"].([]any) {
			dc.ImageRemove(ctx, d.(map[string]any)["image"].(string))
		}
	})
	dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
	d, log := followDeployment(t, p, base, jsonID(dep["id"]))
	if d["status"] != "succeeded" || !strings.Contains(log, "Fetching submodules") {
		t.Fatalf("deployment: %v\n%s", d, log)
	}
	eventually(t, "the submodule's file served", func() bool {
		_, body := h.get("sub.localhost", "/")
		return strings.Contains(body, "from the submodule 5c1e")
	})
}
