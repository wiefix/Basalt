package siteworker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wiefix/basalt/internal/files"
)

func handle[T any](t *testing.T, h *Handler, msgType string, req any) (T, error) {
	t.Helper()
	var out T
	b, _ := json.Marshal(req)
	v, err := h.Handle(context.Background(), msgType, b)
	if err != nil {
		return out, err
	}
	b, _ = json.Marshal(v)
	return out, json.Unmarshal(b, &out)
}

// TestExtractCountSymlink: an archive sent in chunks lands in the site
// (only under its prefix), one escaping the site is refused, files are
// counted, links stay inside the site.
func TestExtractCountSymlink(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(func() (*files.Site, error) { return files.Open(dir) })
	defer h.Close()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range map[string]string{"app/a.txt": "a", "app/sub/b.txt": "bb", "other.txt": "x"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	os.Mkdir(filepath.Join(dir, "dest"), 0o755)

	begin, err := handle[ExtractResult](t, h, MsgExtract, Extract{Op: ExtractBegin, Kind: ExtractTar, Dest: "/dest", Prefix: "app/", Limits: files.DefaultLimits})
	if err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	for len(data) > 0 {
		n := min(len(data), 100)
		if _, err := handle[ExtractResult](t, h, MsgExtract, Extract{Op: ExtractChunk, ID: begin.ID, Data: data[:n]}); err != nil {
			t.Fatal(err)
		}
		data = data[n:]
	}
	end, err := handle[ExtractResult](t, h, MsgExtract, Extract{Op: ExtractEnd, ID: begin.ID})
	if err != nil || end.Result == nil || end.Result.Files != 2 {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "dest", "sub", "b.txt")); err != nil || string(b) != "bb" {
		t.Fatalf("b.txt = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dest", "other.txt")); err == nil {
		t.Error("a file outside the prefix was extracted")
	}
	if c, err := handle[CountResult](t, h, MsgCount, Count{Path: "/dest"}); err != nil || c.Files != 2 {
		t.Errorf("count = %+v, %v", c, err)
	}
	if _, err := handle[ExtractResult](t, h, MsgExtract, Extract{Op: ExtractBegin, Kind: ExtractTar, Dest: "/dest"}); err == nil {
		t.Error("an extraction without limits began")
	}
	if _, err := handle[ExtractResult](t, h, MsgExtract, Extract{Op: ExtractChunk, ID: "nope", Data: []byte("x")}); err == nil {
		t.Error("a chunk of an unknown extraction was taken")
	}
	// Aborted: the extraction is gone (what it began stays: callers remove
	// their folder on a failure).
	ab, _ := handle[ExtractResult](t, h, MsgExtract, Extract{Op: ExtractBegin, Kind: ExtractFile, Dest: "/dest", Name: "c.txt", Limits: files.DefaultLimits})
	if _, err := handle[ExtractResult](t, h, MsgExtract, Extract{Op: ExtractAbort, ID: ab.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := handle[ExtractResult](t, h, MsgExtract, Extract{Op: ExtractEnd, ID: ab.ID}); err == nil {
		t.Error("an aborted extraction ended")
	}

	if _, err := handle[struct{}](t, h, MsgSymlink, Symlink{Target: "sub", Path: "/dest/link"}); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "dest", "link")); err != nil || target != "sub" {
		t.Fatalf("link = %q, %v", target, err)
	}
	if _, err := handle[struct{}](t, h, MsgSymlink, Symlink{Target: "../../etc", Path: "/dest/out"}); err == nil {
		t.Error("a link out of the site was made")
	}
}
