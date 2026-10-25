package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestProjectResources: with memory and CPU limits, a project's apps must
// each have limits of their own (every compose service too), and their
// limits added up must fit; moving an app in counts the same.
func TestProjectResources(t *testing.T) {
	e := newProjectEnv(t)
	o := e.owner
	path := "/api/v1/projects/" + num(e.a)
	o.want(http.StatusUnprocessableEntity, "PUT", path+"/limits", map[string]any{"memory_mb": -1})
	o.want(http.StatusUnprocessableEntity, "PUT", path+"/limits", map[string]any{"cpus": 5000})
	o.want(http.StatusOK, "PUT", path+"/limits", map[string]any{"memory_mb": 1024, "cpus": 2})

	app := func(name string, mem int, cpus float64, project float64) string {
		r := o.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": e.serverID, "name": name,
			"image": "nginx:1.27", "port": 80, "memory_mb": mem, "cpus": cpus, "project_id": project}).body
		return num(r["app"].(map[string]any)["id"].(float64))
	}
	// A deployment that starts is taken as succeeded (the fake agent fails
	// them): the app then runs and counts.
	deploy := func(id string, code int, want string) {
		t.Helper()
		r := o.want(code, "POST", "/api/v1/apps/"+id+"/deploy", map[string]any{})
		if want != "" && !strings.Contains(r.body["error"].(string), want) {
			t.Fatalf("deploy %s: %v", id, r.body)
		}
		if code == http.StatusAccepted {
			for range 300 {
				var n int
				e.srv.db.R.QueryRow(`SELECT COUNT(*) FROM deployments WHERE status = 'running'`).Scan(&n)
				if n == 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			e.srv.db.W.Exec(`UPDATE deployments SET status = 'succeeded' WHERE app_id = ?`, id)
		}
	}
	// The app and database of the project's set-up have no limits.
	deploy(num(e.app), http.StatusUnprocessableEntity, "give each container a memory limit")
	small := app("small", 512, 1, e.a)
	deploy(small, http.StatusAccepted, "")
	big := app("big", 600, 0.5, e.a)
	// web and pg never ran: they count nothing. 512 + 600 > 1024.
	deploy(big, http.StatusUnprocessableEntity, "would use 1112 MB of memory, more than its 1024 MB")
	cpu := app("cpu", 100, 1.5, e.a)
	deploy(cpu, http.StatusUnprocessableEntity, "would use 2.5 CPUs, more than its 2")
	fits := app("fits", 512, 1, e.a)
	deploy(fits, http.StatusAccepted, "")
	if u := o.want(http.StatusOK, "GET", path, nil).body["usage"].(map[string]any); u["memory_mb"] != 1024.0 || u["cpus"] != 2.0 {
		t.Fatalf("usage: %v", u)
	}

	// A compose app: every service needs its limits.
	compose := func(name, file string) string {
		r := o.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": e.serverID, "name": name,
			"source": "compose", "compose": file, "project_id": e.a}).body
		return num(r["app"].(map[string]any)["id"].(float64))
	}
	o.want(http.StatusOK, "PUT", path+"/limits", map[string]any{"memory_mb": 4096, "cpus": 8})
	unlimited := compose("stack1", "services:\n  web:\n    image: nginx:1.27\n    mem_limit: 128m\n    cpus: 0.5\n  cache:\n    image: redis:7\n")
	deploy(unlimited, http.StatusUnprocessableEntity, "give each container a memory limit")
	limited := compose("stack2", "services:\n  web:\n    image: nginx:1.27\n    mem_limit: 128m\n    cpus: 0.5\n  cache:\n    image: redis:7\n    mem_limit: 64m\n    cpus: 0.25\n")
	deploy(limited, http.StatusAccepted, "")

	// Moving an app in counts its limits.
	other := o.want(http.StatusCreated, "POST", "/api/v1/projects", map[string]any{"name": "B"}).body["project"].(map[string]any)["id"].(float64)
	o.want(http.StatusOK, "PUT", "/api/v1/projects/"+num(other)+"/limits", map[string]any{"memory_mb": 100})
	r := o.want(http.StatusUnprocessableEntity, "PUT", "/api/v1/apps/"+small+"/project", map[string]any{"project_id": other})
	if !strings.Contains(r.body["error"].(string), "more than its 100 MB") {
		t.Fatalf("move: %v", r.body)
	}
	o.want(http.StatusOK, "PUT", "/api/v1/projects/"+num(other)+"/limits", map[string]any{"memory_mb": 0})
	o.want(http.StatusNoContent, "PUT", "/api/v1/apps/"+small+"/project", map[string]any{"project_id": other})
}
