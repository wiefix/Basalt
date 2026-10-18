package agent

import (
	"archive/tar"
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/restic"
)

// Files of a snapshot of an app's volume. The snapshot holds one file, the
// volume as a tar stream (backup_apps.go); browsing reads the stream once
// and keeps its listing (snapshots never change), downloads and copies read
// it again and pick what they need.

const (
	// maxIndexEntries bounds the listing of a volume kept for browsing.
	maxIndexEntries = 300_000
	// indexesKept is how many volume listings are kept in memory.
	indexesKept = 4
)

// volumeIndex lists a volume snapshot by folder ("" is the top).
type volumeIndex struct {
	dirs  map[string][]agentproto.SnapshotEntry
	types map[string]string // path -> file, dir, symlink, other
}

type volumeIndexes struct {
	mu    sync.Mutex
	m     map[string]*volumeIndex
	order []string
}

func (c *volumeIndexes) get(key string) *volumeIndex {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[key]
}

func (c *volumeIndexes) put(key string, idx *volumeIndex) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*volumeIndex{}
	}
	if _, ok := c.m[key]; ok {
		return
	}
	c.m[key] = idx
	c.order = append(c.order, key)
	for len(c.order) > indexesKept {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
}

// tarName turns a member name of a volume's tar stream ("./dir/file") into
// a path relative to the volume's top ("" for the top itself); false for
// names that would leave it.
func tarName(name string) (string, bool) {
	for seg := range strings.SplitSeq(name, "/") {
		if seg == ".." {
			return "", false
		}
	}
	return strings.TrimPrefix(path.Clean("/"+name), "/"), true
}

func tarType(h *tar.Header) string {
	switch h.Typeflag {
	case tar.TypeReg:
		return "file"
	case tar.TypeDir:
		return "dir"
	case tar.TypeSymlink:
		return "symlink"
	}
	return "other"
}

// errStop ends reading a volume's stream early: what was wanted is done.
var errStop = errors.New("stop")

// readVolume feeds the tar stream of a volume snapshot to fn; fn returns
// errStop when it needs no more of it.
func readVolume(ctx context.Context, r *restic.Runner, repo restic.Repo, snapshot, file string, fn func(*tar.Reader) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	dumped := make(chan error, 1)
	go func() {
		err := r.Dump(ctx, repo, snapshot, "/"+file, pw)
		pw.CloseWithError(err)
		dumped <- err
	}()
	err := fn(tar.NewReader(pr))
	if err != nil {
		cancel()
		pr.CloseWithError(err)
		<-dumped
		if errors.Is(err, errStop) {
			return nil
		}
		return err
	}
	io.Copy(io.Discard, pr) // the end of the archive
	if err := <-dumped; err != nil {
		return fmt.Errorf("read the backup: %w", err)
	}
	return nil
}

// volumeSnapshot checks the request and returns the snapshot's ID, the
// volume's name and the path in it ("" for the top).
func (b *backupManager) volumeSnapshot(ctx context.Context, r *restic.Runner, repo restic.Repo, req agentproto.SnapshotFilesRequest) (id, volume, rel string, err error) {
	snap, err := b.checkSnapshot(ctx, r, repo, req)
	if err != nil {
		return "", "", "", err
	}
	c, err := files.Clean(req.Path)
	if err != nil {
		return "", "", "", err
	}
	if c == "." {
		c = ""
	}
	return snap.ID, strings.TrimPrefix(req.PartOf(), agentproto.PartVolume), c, nil
}

// volumeIndex lists a volume snapshot, reading it once.
func (b *backupManager) volumeIndex(ctx context.Context, r *restic.Runner, repo restic.Repo, snapshot, file string) (*volumeIndex, error) {
	key := snapshot + "/" + file
	if idx := b.indexes.get(key); idx != nil {
		return idx, nil
	}
	idx := &volumeIndex{dirs: map[string][]agentproto.SnapshotEntry{}, types: map[string]string{"": "dir"}}
	n := 0
	var add func(rel string, e agentproto.SnapshotEntry)
	add = func(rel string, e agentproto.SnapshotEntry) {
		if _, ok := idx.types[rel]; ok {
			return
		}
		parent := path.Dir(rel)
		if parent == "." {
			parent = ""
		}
		if _, ok := idx.types[parent]; !ok { // a folder the stream did not list itself
			add(parent, agentproto.SnapshotEntry{Name: path.Base(parent), Type: "dir", Mode: "0755"})
		}
		idx.types[rel] = e.Type
		idx.dirs[parent] = append(idx.dirs[parent], e)
		n++
	}
	err := readVolume(ctx, r, repo, snapshot, file, func(tr *tar.Reader) error {
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("read the volume's archive: %w", err)
			}
			rel, ok := tarName(h.Name)
			if !ok || rel == "" {
				continue
			}
			if n >= maxIndexEntries {
				return errors.New("the volume holds too many files to browse; download it whole instead")
			}
			e := agentproto.SnapshotEntry{Name: path.Base(rel), Type: tarType(h), ModTime: h.ModTime,
				Mode: fmt.Sprintf("%04o", h.FileInfo().Mode().Perm())}
			if e.Type == "file" {
				e.Size = uint64(max(h.Size, 0))
			}
			add(rel, e)
		}
	})
	if err != nil {
		return nil, err
	}
	b.indexes.put(key, idx)
	return idx, nil
}

func (b *backupManager) browseVolume(ctx context.Context, r *restic.Runner, repo restic.Repo, req agentproto.SnapshotFilesRequest) (agentproto.BrowseResponse, error) {
	id, vol, rel, err := b.volumeSnapshot(ctx, r, repo, req)
	if err != nil {
		return agentproto.BrowseResponse{}, err
	}
	idx, err := b.volumeIndex(ctx, r, repo, id, volumeFile(vol))
	if err != nil {
		return agentproto.BrowseResponse{}, err
	}
	switch idx.types[rel] {
	case "dir":
	case "":
		return agentproto.BrowseResponse{}, errNotInBackup
	default:
		return agentproto.BrowseResponse{}, errors.New("not a folder")
	}
	return agentproto.BrowseResponse{Path: rel, Entries: sortEntries(slices.Clone(idx.dirs[rel]))}, nil
}

// exportVolume writes a file of a volume snapshot, or a folder of it as a
// zip archive, for the panel to read.
func (b *backupManager) exportVolume(ctx context.Context, r *restic.Runner, repo restic.Repo, req agentproto.SnapshotFilesRequest) (agentproto.ExportResponse, error) {
	id, vol, rel, err := b.volumeSnapshot(ctx, r, repo, req)
	if err != nil {
		return agentproto.ExportResponse{}, err
	}
	return b.export(func(w io.Writer) (string, error) {
		kind := "" // file or dir, once the stream shows it
		if rel == "" {
			kind = "dir"
		}
		var zw *zip.Writer
		err := readVolume(ctx, r, repo, id, volumeFile(vol), func(tr *tar.Reader) error {
			for {
				h, err := tr.Next()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return fmt.Errorf("read the volume's archive: %w", err)
				}
				n, ok := tarName(h.Name)
				if !ok || n == "" {
					continue
				}
				sub := n
				switch {
				case rel == "":
				case n == rel:
					switch h.Typeflag {
					case tar.TypeReg:
						kind = "file"
						if _, err := io.Copy(w, tr); err != nil {
							return err
						}
						return errStop
					case tar.TypeDir:
						kind = "dir"
						continue
					}
					return errors.New("only files and folders can be downloaded")
				case strings.HasPrefix(n, rel+"/"):
					kind, sub = "dir", n[len(rel)+1:]
				default:
					continue
				}
				if zw == nil {
					zw = zip.NewWriter(w)
				}
				if err := zipEntry(zw, h, sub, tr); err != nil {
					return err
				}
			}
		})
		if err != nil {
			return "", err
		}
		switch kind {
		case "file":
			return path.Base(rel), nil
		case "dir":
			if zw == nil {
				zw = zip.NewWriter(w) // an empty folder
			}
			if err := zw.Close(); err != nil {
				return "", err
			}
			if rel == "" {
				return fmt.Sprintf("volume-%s-%s.zip", vol, id[:8]), nil
			}
			return path.Base(rel) + ".zip", nil
		}
		return "", errNotInBackup
	})
}

// zipEntry adds a folder or file of a volume's archive to a zip archive as
// name; links and special files are left out.
func zipEntry(zw *zip.Writer, h *tar.Header, name string, r io.Reader) error {
	fh := &zip.FileHeader{Name: name, Modified: h.ModTime}
	switch h.Typeflag {
	case tar.TypeDir:
		fh.Name += "/"
		fh.SetMode(fs.ModeDir | h.FileInfo().Mode().Perm())
		_, err := zw.CreateHeader(fh)
		return err
	case tar.TypeReg:
		fh.Method = zip.Deflate
		fh.SetMode(h.FileInfo().Mode().Perm())
		f, err := zw.CreateHeader(fh)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		return err
	}
	return nil
}

// restoreVolumeCopy restores a file or folder of a volume snapshot into a
// new folder "restored-<time>" at the top of the app's volume. Owners and
// permissions are kept (without setuid, setgid and sticky bits); symbolic
// links, hard links and special files are left out. It is written by the
// volume helper container, under the daemon's user namespaces like the app.
func (b *backupManager) restoreVolumeCopy(ctx context.Context, r *restic.Runner, repo restic.Repo, req agentproto.SnapshotFilesRequest) (agentproto.RestoreCopyResponse, error) {
	id, vol, rel, err := b.volumeSnapshot(ctx, r, repo, req)
	if err != nil {
		return agentproto.RestoreCopyResponse{}, err
	}
	bin, err := b.dockerCLI()
	if err != nil {
		return agentproto.RestoreCopyResponse{}, err
	}
	if err := b.helperImage(ctx, &deployLog{}); err != nil {
		return agentproto.RestoreCopyResponse{}, err
	}
	volName := appVolume(req.AppID, vol)
	if _, err := b.apps.dc.VolumeInspect(ctx, volName); errors.Is(err, docker.ErrNotFound) {
		return agentproto.RestoreCopyResponse{}, fmt.Errorf("the app has no volume %s now", vol)
	} else if err != nil {
		return agentproto.RestoreCopyResponse{}, err
	}
	b.slots <- struct{}{}
	defer func() { <-b.slots }()

	helper := func(stdin bool, args ...string) []string {
		run := []string{"run", "--rm", "--network", "none", "--log-driver", "none"}
		if stdin {
			run = append(run, "-i")
		}
		run = append(run, "--mount", "type=volume,source="+volName+",target=/data", volumeHelperImage)
		return b.dockerArgs(bin, append(run, args...)...)
	}
	base := restoredName()
	folder := base
	for i := 2; ; i++ {
		out, err := command(ctx, helper(false, "mkdir", "/data/"+folder)).CombinedOutput()
		if err == nil {
			break
		}
		if i > 20 || ctx.Err() != nil {
			return agentproto.RestoreCopyResponse{}, fmt.Errorf("create the folder in the volume: %s", strings.TrimSpace(string(out)))
		}
		folder = fmt.Sprintf("%s-%d", base, i)
	}
	res, err := b.copyIntoVolume(ctx, r, repo, id, vol, rel, helper(true, "tar", "-C", "/data/"+folder, "-xf", "-"))
	if err != nil {
		command(context.WithoutCancel(ctx), helper(false, "rm", "-rf", "/data/"+folder)).Run()
		return agentproto.RestoreCopyResponse{}, err
	}
	res.Folder = folder
	return res, nil
}

// copyIntoVolume streams what rel names in a volume snapshot, as a new tar
// archive with names under its base name, to the extracting command.
func (b *backupManager) copyIntoVolume(ctx context.Context, r *restic.Runner, repo restic.Repo, id, vol, rel string, extract []string) (agentproto.RestoreCopyResponse, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	cmd := command(ctx, extract)
	cmd.Stdin = pr
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return agentproto.RestoreCopyResponse{}, err
	}
	var res agentproto.RestoreCopyResponse
	found := rel == ""
	base := path.Base(rel)
	tw := tar.NewWriter(pw)
	err := readVolume(ctx, r, repo, id, volumeFile(vol), func(tr *tar.Reader) error {
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("read the volume's archive: %w", err)
			}
			n, ok := tarName(h.Name)
			if !ok || n == "" {
				continue
			}
			name, last := n, false
			switch {
			case rel == "":
			case n == rel:
				if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
					return errors.New("only files and folders can be restored")
				}
				name, last = base, h.Typeflag == tar.TypeReg
			case strings.HasPrefix(n, rel+"/"):
				name = base + "/" + n[len(rel)+1:]
			default:
				continue
			}
			found = true
			if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
				res.Skipped++
				continue
			}
			oh := &tar.Header{Typeflag: h.Typeflag, Name: name, Mode: h.Mode & 0o777, Uid: h.Uid, Gid: h.Gid,
				ModTime: h.ModTime.Truncate(time.Second), Format: tar.FormatGNU}
			if h.Typeflag == tar.TypeDir {
				oh.Name += "/"
				res.Dirs++
			} else {
				oh.Size = h.Size
				res.Files++
				res.Bytes += h.Size
			}
			if res.Bytes > restoreCopyLimits.MaxTotalBytes || res.Files+res.Dirs > restoreCopyLimits.MaxFiles {
				return files.ErrArchiveTooLarge
			}
			if err := tw.WriteHeader(oh); err != nil {
				return err
			}
			if oh.Size > 0 {
				if _, err := io.CopyN(tw, tr, oh.Size); err != nil {
					return err
				}
			}
			if last {
				return errStop
			}
		}
	})
	if err == nil && !found {
		err = errNotInBackup
	}
	if err == nil {
		err = tw.Close()
	}
	pw.CloseWithError(err)
	if err != nil {
		cancel()
		cmd.Wait()
		if errors.Is(err, io.ErrClosedPipe) && out.Len() > 0 {
			return res, fmt.Errorf("write into the volume: %s", strings.TrimSpace(out.String()))
		}
		return res, err
	}
	if werr := cmd.Wait(); werr != nil {
		return res, fmt.Errorf("write into the volume: %s", strings.TrimSpace(out.String()))
	}
	return res, nil
}
