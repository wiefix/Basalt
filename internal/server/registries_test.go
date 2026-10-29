package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/agentproto"
)

func TestNormalizeRegistryHost(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io": "ghcr.io", " GHCR.IO ": "ghcr.io", "https://ghcr.io/": "ghcr.io",
		"registry.example.com:5000": "registry.example.com:5000", "localhost:5000": "localhost:5000",
		"docker.io": "docker.io", "https://index.docker.io/v1/": "docker.io", "registry-1.docker.io": "docker.io",
		"127.0.0.1:5000": "127.0.0.1:5000",
	} {
		if got, err := normalizeRegistryHost(in); err != nil || got != want {
			t.Errorf("normalizeRegistryHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ghcr", "ghcr.io/org", "a b.com", "x.com:99999", "x.com:", "user@ghcr.io"} {
		if got, err := normalizeRegistryHost(bad); err == nil {
			t.Errorf("normalizeRegistryHost(%q) = %q; want an error", bad, got)
		}
	}
}

func TestImagesToPrune(t *testing.T) {
	ref := func(r, d string) agentproto.ImageRef { return agentproto.ImageRef{Ref: r, Digest: d} }
	a := ref("app:latest", "app@sha256:"+repeat("a"))
	b := ref("app:latest", "app@sha256:"+repeat("b"))
	c := ref("app:v3", "app@sha256:"+repeat("c"))
	rollbackToB := ref("app@sha256:"+repeat("b"), "app@sha256:"+repeat("b"))
	local := ref("local-app:dev", "sha256:"+repeat("e")) // no registry digest

	// Newest first. Keeping 3 (the new deployment and two previous ones).
	got := imagesToPrune([]agentproto.ImageRef{c, rollbackToB, b, a, local}, 3)
	// c and B are kept; b is the same image as the rollback; a goes despite
	// sharing the "latest" tag with a kept deployment; the local image is
	// removed by its reference.
	want := []agentproto.ImageRef{a, ref("local-app:dev", "")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prune = %+v\nwant  %+v", got, want)
	}
	if got := imagesToPrune([]agentproto.ImageRef{c, b}, 3); got != nil {
		t.Fatalf("pruned within the window: %+v", got)
	}
	// Keeping 1: everything previous goes, each image once.
	if got := imagesToPrune([]agentproto.ImageRef{c, b, rollbackToB, c}, 1); len(got) != 2 {
		t.Fatalf("keep 1: %+v", got)
	}
}

func repeat(s string) string {
	out := ""
	for range 64 {
		out += s
	}
	return out
}

func TestBuiltImagesToPrune(t *testing.T) {
	succeeded := []map[string]string{ // newest first
		{"web": "basalt/app-7:web-c", "db": "postgres@sha256:" + strings.Repeat("c", 64)},
		{"web": "basalt/app-7:web-b", "worker": "basalt/app-7:worker-b"},
		{"web": "basalt/app-7:web-a", "worker": "basalt/app-7:worker-a"},
		{"web": "basalt/app-7:web-0", "other": "basalt/app-8:x-0"},
	}
	// A rollback to the oldest deployment reuses its image.
	services := []agentproto.ServiceSpec{{Name: "web", Build: &agentproto.ServiceBuild{ReuseImage: "basalt/app-7:web-0"}}}
	got := builtImagesToPrune(7, succeeded, services, 3)
	var refs []string
	for _, r := range got {
		refs = append(refs, r.Ref)
	}
	if strings.Join(refs, ",") != "basalt/app-7:web-a,basalt/app-7:worker-a" {
		t.Fatalf("pruned %v", refs)
	}
	if got := builtImagesToPrune(7, succeeded[:1], nil, 3); len(got) != 0 {
		t.Fatalf("pruned %v", got)
	}
}
