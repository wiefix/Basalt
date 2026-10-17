package agent

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/restic"
)

// Single files and folders of a snapshot of a site's files: browsing,
// downloading (through a temporary file the panel reads in chunks, so a
// slow download never holds up the agent channel) and restoring a copy into
// a new folder of the site.

const (
	// exportTTL is how long an exported file waits to be read.
	exportTTL = 2 * time.Hour
	// maxExports bounds exported files waiting on the server.
	maxExports = 8
)

// restoreCopyLimits bound a restored copy (it goes through the archive
// extractor: confined paths, no symlinks, normalised modes).
var restoreCopyLimits = files.Limits{MaxTotalBytes: 100 << 30, MaxFiles: 1_000_000}

var errNotInBackup = errors.New("no such file or folder in this backup")

// snapshotNode finds what the request names: the snapshot (which must hold
// files of this very site), the absolute path in it, the path relative to
// the site's directory ("" for all of it) and the entry itself.
func (b *backupManager) snapshotNode(ctx context.Context, r *restic.Runner, repo restic.Repo, req agentproto.SnapshotFilesRequest) (snapID, abs, rel string, node *restic.Node, err error) {
	snap, err := r.SnapshotByID(ctx, repo, req.Snapshot)
	if err != nil {
		return "", "", "", nil, err
	}
	if snap == nil || snap.Host != backupHost || len(snap.Paths) != 1 ||
		!slices.Contains(snap.Tags, "res="+req.Resource) || !slices.Contains(snap.Tags, "part=files") {
		return "", "", "", nil, errors.New("no such backup of the site's files")
	}
	c, err := files.Clean(req.Path)
	if err != nil {
		return "", "", "", nil, err
	}
	abs = snap.Paths[0]
	if c != "." {
		abs, rel = path.Join(abs, c), c
	}
	node, _, err = r.List(ctx, repo, snap.ID, abs)
	if err != nil {
		return "", "", "", nil, err
	}
	if node == nil {
		return "", "", "", nil, errNotInBackup
	}
	return snap.ID, abs, rel, node, nil
}

func entryType(t string) string {
	switch t {
	case "file", "dir", "symlink":
		return t
	}
	return "other"
}

// Browse lists a folder of a snapshot of a site's files.
func (b *backupManager) Browse(ctx context.Context, req agentproto.SnapshotFilesRequest) (agentproto.BrowseResponse, error) {
	if err := req.Validate(); err != nil {
		return agentproto.BrowseResponse{}, err
	}
	repo, err := b.repo(req.Repo)
	if err != nil {
		return agentproto.BrowseResponse{}, err
	}
	r := b.runner(nil)
	id, abs, rel, node, err := b.snapshotNode(ctx, r, repo, req)
	if err != nil {
		return agentproto.BrowseResponse{}, err
	}
	if node.Type != "dir" {
		return agentproto.BrowseResponse{}, errors.New("not a folder")
	}
	_, children, err := r.List(ctx, repo, id, abs)
	if err != nil {
		return agentproto.BrowseResponse{}, err
	}
	out := agentproto.BrowseResponse{Path: rel, Entries: make([]agentproto.SnapshotEntry, 0, len(children))}
	for _, n := range children {
		out.Entries = append(out.Entries, agentproto.SnapshotEntry{
			Name: n.Name, Type: entryType(n.Type), Size: n.Size, ModTime: n.MTime,
			Mode: fmt.Sprintf("%04o", fs.FileMode(n.Mode).Perm()),
		})
	}
	slices.SortFunc(out.Entries, func(a, b agentproto.SnapshotEntry) int {
		if (a.Type == "dir") != (b.Type == "dir") {
			if a.Type == "dir" {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Name, b.Name)
	})
	if len(out.Entries) > files.MaxEntries {
		out.Entries = out.Entries[:files.MaxEntries]
	}
	return out, nil
}

func (b *backupManager) exportDir() string { return filepath.Join(b.dataDir, "exports") }

// cleanExports removes exported files nobody read for exportTTL and
// returns how many remain.
func (b *backupManager) cleanExports() int {
	entries, err := os.ReadDir(b.exportDir())
	if err != nil {
		return 0
	}
	left := 0
	for _, e := range entries {
		fi, err := e.Info()
		if err == nil && time.Since(fi.ModTime()) > exportTTL {
			os.Remove(filepath.Join(b.exportDir(), e.Name()))
			continue
		}
		left++
	}
	return left
}

// Export writes a file, or a folder as a zip archive, of a snapshot to a
// temporary file for the panel to read.
func (b *backupManager) Export(ctx context.Context, req agentproto.SnapshotFilesRequest) (agentproto.ExportResponse, error) {
	if err := req.Validate(); err != nil {
		return agentproto.ExportResponse{}, err
	}
	repo, err := b.repo(req.Repo)
	if err != nil {
		return agentproto.ExportResponse{}, err
	}
	r := b.runner(nil)
	id, abs, rel, node, err := b.snapshotNode(ctx, r, repo, req)
	if err != nil {
		return agentproto.ExportResponse{}, err
	}
	var name string
	var dump func(io.Writer) error
	switch node.Type {
	case "dir":
		name = path.Base(rel) + ".zip"
		if rel == "" {
			name = fmt.Sprintf("site-%d-%s.zip", req.SiteID, id[:8])
		}
		dump = func(w io.Writer) error { return r.DumpTree(ctx, repo, id, abs, "zip", w) }
	case "file":
		name = path.Base(rel)
		dump = func(w io.Writer) error { return r.Dump(ctx, repo, id, abs, w) }
	default:
		return agentproto.ExportResponse{}, errors.New("only files and folders can be downloaded")
	}

	if err := os.MkdirAll(b.exportDir(), 0o700); err != nil {
		return agentproto.ExportResponse{}, err
	}
	if b.cleanExports() >= maxExports {
		return agentproto.ExportResponse{}, errors.New("too many downloads are being prepared on this server; try again later")
	}
	b.slots <- struct{}{}
	defer func() { <-b.slots }()

	var raw [16]byte
	rand.Read(raw[:])
	eid := hex.EncodeToString(raw[:])
	final := filepath.Join(b.exportDir(), eid)
	f, err := os.OpenFile(final+".part", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return agentproto.ExportResponse{}, err
	}
	err = dump(f)
	size, _ := f.Seek(0, io.SeekCurrent)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(final+".part", final)
	}
	if err != nil {
		os.Remove(final + ".part")
		return agentproto.ExportResponse{}, err
	}
	return agentproto.ExportResponse{ID: eid, Name: name, Size: size}, nil
}

// ExportRead reads part of an exported file.
func (b *backupManager) ExportRead(req agentproto.ExportRead) (agentproto.ExportChunk, error) {
	if err := req.Validate(); err != nil {
		return agentproto.ExportChunk{}, err
	}
	p := filepath.Join(b.exportDir(), req.ID)
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return agentproto.ExportChunk{}, errors.New("the download expired; start it again")
	} else if err != nil {
		return agentproto.ExportChunk{}, err
	}
	defer f.Close()
	buf := make([]byte, req.Length)
	n, err := f.ReadAt(buf, req.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return agentproto.ExportChunk{}, err
	}
	now := time.Now()
	os.Chtimes(p, now, now) // still being read: not expired
	return agentproto.ExportChunk{Data: buf[:n]}, nil
}

// ExportClose drops an exported file.
func (b *backupManager) ExportClose(req agentproto.ExportClose) error {
	if err := req.Validate(); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(b.exportDir(), req.ID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// RestoreCopy restores a file or folder of a snapshot into a new folder
// "restored-<time>" at the top of the site, leaving everything else as it
// is. The copy is written through the site's confined file access, as the
// site's user; symbolic links and special files are left out.
func (b *backupManager) RestoreCopy(ctx context.Context, req agentproto.SnapshotFilesRequest) (agentproto.RestoreCopyResponse, error) {
	if err := req.Validate(); err != nil {
		return agentproto.RestoreCopyResponse{}, err
	}
	repo, err := b.repo(req.Repo)
	if err != nil {
		return agentproto.RestoreCopyResponse{}, err
	}
	r := b.runner(nil)
	id, abs, rel, node, err := b.snapshotNode(ctx, r, repo, req)
	if err != nil {
		return agentproto.RestoreCopyResponse{}, err
	}
	if node.Type != "dir" && node.Type != "file" {
		return agentproto.RestoreCopyResponse{}, errors.New("only files and folders can be restored")
	}
	site, err := b.sites.open(req.SiteID)
	if err != nil {
		return agentproto.RestoreCopyResponse{}, err
	}
	defer site.Close()
	b.slots <- struct{}{}
	defer func() { <-b.slots }()

	base := "restored-" + time.Now().UTC().Format("20060102-150405")
	folder := base
	for i := 2; ; i++ {
		err := site.Mkdir(folder)
		if err == nil {
			break
		}
		if !errors.Is(err, files.ErrExists) || i > 20 {
			return agentproto.RestoreCopyResponse{}, err
		}
		folder = fmt.Sprintf("%s-%d", base, i)
	}
	res, err := b.restoreInto(ctx, r, repo, site, id, abs, rel, node.Type, folder)
	if err != nil {
		site.Delete(folder)
		return agentproto.RestoreCopyResponse{}, err
	}
	return agentproto.RestoreCopyResponse{Folder: folder, Files: res.Files, Dirs: res.Dirs, Bytes: res.Bytes, Skipped: len(res.Skipped)}, nil
}

func (b *backupManager) restoreInto(ctx context.Context, r *restic.Runner, repo restic.Repo, site *files.Site, id, abs, rel, typ, folder string) (*files.ExtractResult, error) {
	dest, base := folder, path.Base(rel)
	if typ == "dir" && rel != "" {
		dest = folder + "/" + base
		if err := site.Mkdir(dest); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	dumped := make(chan error, 1)
	go func() {
		var err error
		if typ == "dir" {
			err = r.DumpTree(ctx, repo, id, abs, "tar", pw)
		} else {
			err = r.Dump(ctx, repo, id, abs, pw)
		}
		pw.CloseWithError(err)
		dumped <- err
	}()
	var res *files.ExtractResult
	var err error
	if typ == "dir" {
		res, err = site.ExtractTar(pr, dest, restoreCopyLimits)
	} else {
		res, err = site.ExtractFile(pr, dest, base, restoreCopyLimits)
	}
	if err != nil {
		cancel()
		pr.CloseWithError(err)
		<-dumped
		return nil, err
	}
	io.Copy(io.Discard, pr) // the end of the archive
	if err := <-dumped; err != nil {
		return nil, err
	}
	return res, nil
}
