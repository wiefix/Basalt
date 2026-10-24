package agent

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
)

const inotifyMask = syscall.IN_CREATE | syscall.IN_DELETE | syscall.IN_MODIFY | syscall.IN_ATTRIB |
	syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_FROM | syscall.IN_MOVED_TO | syscall.IN_DELETE_SELF |
	syscall.IN_ONLYDIR | syscall.IN_DONT_FOLLOW

// inotifyBackend watches every folder of the watched sites with one
// inotify instance (inotify does not watch a tree: each folder has its
// own watch, added as folders appear).
type inotifyBackend struct {
	w *siteWatcher
	f *os.File

	mu    sync.Mutex
	dirs  map[int32]watchedDir
	sites map[int64][]int32 // a site's watches
	// tooBig: sites with too many folders, left to the panel's round.
	tooBig map[int64]bool
}

type watchedDir struct {
	site int64
	path string
	top  bool // the site's own folder
}

func newWatchBackend(w *siteWatcher) watchBackend {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		w.log.Warn("cannot watch the sites' files; replicas are copied every minute", "err", err)
		return nil
	}
	b := &inotifyBackend{w: w, f: os.NewFile(uintptr(fd), "inotify"),
		dirs: map[int32]watchedDir{}, sites: map[int64][]int32{}, tooBig: map[int64]bool{}}
	go b.read()
	return b
}

func (b *inotifyBackend) fd() int { return int(b.f.Fd()) }

func (b *inotifyBackend) set(ids []int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.sites {
		if !slices.Contains(ids, id) {
			b.unwatchLocked(id)
		}
	}
	for id := range b.tooBig {
		if !slices.Contains(ids, id) {
			delete(b.tooBig, id)
		}
	}
	for _, id := range ids {
		if _, ok := b.sites[id]; !ok && !b.tooBig[id] {
			b.watchSiteLocked(id)
		}
	}
}

// watchSiteLocked watches every folder of a site.
func (b *inotifyBackend) watchSiteLocked(id int64) {
	dir := filepath.Join(b.w.root, strconv.FormatInt(id, 10))
	b.sites[id] = []int32{}
	if err := b.addTreeLocked(id, dir, true); err != nil {
		b.unwatchLocked(id)
		if errors.Is(err, errTooManyDirs) || errors.Is(err, syscall.ENOSPC) {
			b.tooBig[id] = true
			b.w.log.Warn("too many folders to watch in a site; its replicas are copied every minute", "site_id", id, "err", err)
		} else if !errors.Is(err, fs.ErrNotExist) {
			b.w.log.Warn("watch a site's files", "site_id", id, "err", err)
		}
	}
}

var errTooManyDirs = errors.New("too many folders")

// addTreeLocked watches dir and the folders below it (not through symlinks).
func (b *inotifyBackend) addTreeLocked(site int64, dir string, top bool) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != dir && errors.Is(err, fs.ErrNotExist) {
				return nil // gone meanwhile
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		isTop := top && p == dir
		if p != dir && watchIgnored(d.Name(), top && filepath.Dir(p) == dir) {
			return fs.SkipDir
		}
		if len(b.sites[site]) >= maxWatchDirs {
			return errTooManyDirs
		}
		wd, err := syscall.InotifyAddWatch(b.fd(), p, inotifyMask)
		if err != nil {
			if p != dir && (errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR)) {
				return fs.SkipDir
			}
			return err
		}
		w := int32(wd)
		if _, ok := b.dirs[w]; !ok {
			b.sites[site] = append(b.sites[site], w)
		}
		b.dirs[w] = watchedDir{site: site, path: p, top: isTop}
		return nil
	})
}

func (b *inotifyBackend) unwatchLocked(id int64) {
	for _, wd := range b.sites[id] {
		syscall.InotifyRmWatch(b.fd(), uint32(wd))
		delete(b.dirs, wd)
	}
	delete(b.sites, id)
}

// read takes the kernel's events until the backend is closed.
func (b *inotifyBackend) read() {
	buf := make([]byte, 64<<10)
	for {
		n, err := b.f.Read(buf)
		if err != nil {
			if !errors.Is(err, os.ErrClosed) {
				b.w.log.Warn("watching the sites' files stopped", "err", err)
			}
			return
		}
		b.events(buf[:n])
	}
}

// events handles a batch of inotify events.
func (b *inotifyBackend) events(buf []byte) {
	var changed, rescan []int64
	b.mu.Lock()
	for len(buf) >= syscall.SizeofInotifyEvent {
		wd := int32(binary.NativeEndian.Uint32(buf[0:]))
		mask := binary.NativeEndian.Uint32(buf[4:])
		size := int(binary.NativeEndian.Uint32(buf[12:]))
		if syscall.SizeofInotifyEvent+size > len(buf) {
			break
		}
		name := string(buf[syscall.SizeofInotifyEvent : syscall.SizeofInotifyEvent+size])
		if i := indexZero(name); i >= 0 {
			name = name[:i]
		}
		buf = buf[syscall.SizeofInotifyEvent+size:]
		if mask&syscall.IN_Q_OVERFLOW != 0 {
			// Events were lost: every site may have changed.
			for id := range b.sites {
				changed = append(changed, id)
			}
			continue
		}
		d, ok := b.dirs[wd]
		if !ok {
			continue
		}
		if mask&syscall.IN_IGNORED != 0 {
			delete(b.dirs, wd)
			b.sites[d.site] = slices.DeleteFunc(b.sites[d.site], func(x int32) bool { return x == wd })
			continue
		}
		if name != "" && watchIgnored(name, d.top) {
			continue
		}
		changed = append(changed, d.site)
		if mask&syscall.IN_ISDIR == 0 {
			continue
		}
		switch {
		case mask&(syscall.IN_MOVED_FROM|syscall.IN_MOVED_TO) != 0:
			// A folder moved: the paths of the watches below it are stale.
			rescan = append(rescan, d.site)
		case mask&syscall.IN_CREATE != 0:
			if err := b.addTreeLocked(d.site, filepath.Join(d.path, name), false); err != nil {
				rescan = append(rescan, d.site)
			}
		}
	}
	slices.Sort(rescan)
	for _, id := range slices.Compact(rescan) {
		if _, ok := b.sites[id]; ok {
			b.unwatchLocked(id)
			b.watchSiteLocked(id)
		}
	}
	b.mu.Unlock()
	for _, id := range changed {
		b.w.siteChanged(id)
	}
}

func indexZero(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return i
		}
	}
	return -1
}

func (b *inotifyBackend) close() { b.f.Close() }
