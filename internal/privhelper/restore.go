package privhelper

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"

	"github.com/wiefix/basalt/internal/restic"
	"github.com/wiefix/basalt/internal/sysuser"
)

// restoreDir, in the sites' folder, holds a site's restore while it is put
// in order: root's and 0700, so no site's user reaches into it.
const restoreDir = ".basalt-restore"

// restoreSite makes a site's folder hold exactly a snapshot of it: one of
// this site's files, taken of this folder. The repository is the agent's
// word, so the snapshot is restored aside first and made the site's
// there: every file its user's, no setuid or setgid file, no device, no
// extended attribute (file capabilities, ACLs). Only then does it take the
// folder's place.
func (h *Helper) restoreSite(ctx context.Context, r *restic.Runner, repo restic.Repo, req ResticRequest) error {
	dir := h.siteDir(req.SiteID)
	s, err := r.SnapshotByID(ctx, repo, req.Snapshot)
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("no such snapshot")
	}
	if !slices.Contains(s.Tags, "res=site-"+strconv.FormatInt(req.SiteID, 10)) || !slices.Contains(s.Tags, "part=files") ||
		len(s.Paths) != 1 || s.Paths[0] != dir {
		return errors.New("the snapshot is not of this site's files")
	}
	cur, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the site's folder: %w", err)
	}
	if !cur.IsDir() {
		return errors.New("the site's folder is not a directory")
	}
	own, err := h.restoreOwner(req.SiteID, cur)
	if err != nil {
		return err
	}

	aside := filepath.Join(h.cfg.SitesRoot, restoreDir)
	if err := os.Mkdir(aside, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create the restore folder: %w", err)
	}
	if fi, err := os.Lstat(aside); err != nil || !fi.IsDir() {
		return errors.New("the restore folder is not a directory")
	}
	if err := os.Chmod(aside, 0o700); err != nil {
		return err
	}
	// What an earlier restore of the site left (the helper stopped
	// midway) goes first.
	staged := filepath.Join(aside, strconv.FormatInt(req.SiteID, 10))
	spare := staged + ".old"
	for _, p := range []string{staged, spare} {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove a previous restore: %w", err)
		}
	}
	// On success staged holds the site's former files by then.
	defer os.RemoveAll(staged)
	if err := os.Mkdir(staged, 0o700); err != nil {
		return err
	}
	if err := r.RestoreTree(ctx, repo, s.ID, dir, staged); err != nil {
		return err
	}
	removed, err := own.apply(staged)
	if err != nil {
		return fmt.Errorf("give the restored files to the site: %w", err)
	}
	if removed > 0 {
		h.log.Warn("left out of a restore: devices, pipes or sockets", "site_id", req.SiteID, "files", removed)
	}
	if err := exchange(staged, dir, spare); err != nil {
		return fmt.Errorf("put the restored files in place: %w", err)
	}
	return nil
}

// restoreOwner is who restored files belong to.
type restoreOwner struct {
	uid, gid int   // gid: unless a file's group is one of keep
	keep     []int // the web group and the site's own
	// The folder itself keeps the owner and mode of the site's folder.
	topUID, topGID int
	topMode        fs.FileMode
}

// restoreOwner: with isolation the site's user owns the files; without, the
// owner of the site's folder.
func (h *Helper) restoreOwner(id int64, cur fs.FileInfo) (restoreOwner, error) {
	st, ok := cur.Sys().(*syscall.Stat_t)
	if !ok {
		return restoreOwner{}, errors.New("no owner of the site's folder")
	}
	o := restoreOwner{
		uid: int(st.Uid), gid: int(st.Gid), keep: []int{int(st.Gid)},
		topUID: int(st.Uid), topGID: int(st.Gid), topMode: cur.Mode() & (fs.ModePerm | fs.ModeSetgid | fs.ModeSticky),
	}
	if uid, gid, err := sysuser.LookupUser(sysuser.SiteUser(id)); err == nil {
		o.uid, o.gid = uid, gid
		o.keep = []int{gid}
		if www, err := sysuser.LookupGroup(h.cfg.WWWGroup); err == nil {
			o.keep = append(o.keep, www)
		}
	}
	return o, nil
}

// apply gives a restored tree to its owner and takes out what a site's
// files must not have; it reports how many entries it removed.
func (o restoreOwner) apply(dir string) (removed int, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		t := d.Type()
		if t&fs.ModeSymlink == 0 && !t.IsDir() && !t.IsRegular() {
			removed++
			return root.Remove(p)
		}
		fi, err := root.Lstat(p)
		if err != nil {
			return err
		}
		gid := o.gid
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && slices.Contains(o.keep, int(st.Gid)) {
			gid = int(st.Gid)
		}
		if err := root.Lchown(p, o.uid, gid); err != nil {
			return err
		}
		// The tree is root's alone: its paths stay what was walked.
		if err := dropXattrs(filepath.Join(dir, p)); err != nil {
			return err
		}
		if t&fs.ModeSymlink != 0 {
			return nil
		}
		mode := fi.Mode() & (fs.ModePerm | fs.ModeSetgid | fs.ModeSticky)
		if !t.IsDir() {
			mode &= fs.ModePerm
		}
		return root.Chmod(p, mode)
	})
	if err != nil {
		return removed, err
	}
	if err := os.Lchown(dir, o.topUID, o.topGID); err != nil {
		return removed, err
	}
	if err := dropXattrs(dir); err != nil {
		return removed, err
	}
	return removed, os.Chmod(dir, o.topMode)
}

// renameSwap swaps two folders with plain renames, through spare.
func renameSwap(a, b, spare string) error {
	if err := os.Rename(b, spare); err != nil {
		return err
	}
	if err := os.Rename(a, b); err != nil {
		if rerr := os.Rename(spare, b); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	return os.Rename(spare, a)
}
