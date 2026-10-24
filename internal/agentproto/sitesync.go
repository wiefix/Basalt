package agentproto

import (
	"errors"
	"fmt"
)

// A site served by several servers gets its files copied one way, from the
// primary server to the others, by the panel: agents never reach each
// other. The panel compares the two manifests and moves what differs in
// chunks through itself.

// TypeSiteManifest lists a site's files and folders (not the site's logs/
// and tmp/, not symlinks). Streaming: the entries come in frames of
// ManifestBatch, the response only closes the list.
const TypeSiteManifest = "site_manifest"

// TypeSiteSyncRead reads parts of a site's files.
const TypeSiteSyncRead = "site_sync_read"

// TypeSiteSyncWrite changes a replica's copy of a site: deletions first,
// then folders, writes and permission changes, in that order.
const TypeSiteSyncWrite = "site_sync_write"

// TypeSiteWatch (panel -> agent) names the sites whose files the agent
// reports changes of: those it is the primary of, with replicas. The list
// replaces the previous one.
const TypeSiteWatch = "site_watch"

// TypeSiteChanged (agent -> panel, notification) names sites whose files
// changed, once they were quiet for a moment: the panel copies them to the
// replicas then, without waiting for its round every minute.
const TypeSiteChanged = "site_changed"

// MaxWatchedSites bounds a site_watch list.
const MaxWatchedSites = 10_000

type SiteWatch struct {
	Sites []int64 `json:"sites"`
}

func (w SiteWatch) Validate() error {
	if len(w.Sites) > MaxWatchedSites {
		return errors.New("too many sites to watch")
	}
	for _, id := range w.Sites {
		if id <= 0 {
			return fmt.Errorf("invalid site id %d", id)
		}
	}
	return nil
}

type SiteChanged struct {
	Sites []int64 `json:"sites"`
}

const (
	// ManifestBatch is the number of entries in one frame of a manifest.
	ManifestBatch = 4000
	// MaxManifest bounds the entries of a site that is copied.
	MaxManifest = 500_000
	// MaxSyncData bounds the file data of one read or write.
	MaxSyncData = 2 << 20
	// MaxSyncOps bounds the other operations of one write.
	MaxSyncOps = 5000
)

type SiteManifestRequest struct {
	SiteID int64 `json:"site_id"`
}

// ManifestEntry is a file or folder of a site, relative to its directory.
type ManifestEntry struct {
	Path  string `json:"p"`
	Dir   bool   `json:"d,omitempty"`
	Size  int64  `json:"s,omitempty"`
	MTime int64  `json:"m,omitempty"` // unix nanoseconds (files)
	Mode  uint32 `json:"o"`           // permission bits
}

// ManifestFrame is one frame of a manifest.
type ManifestFrame struct {
	Entries []ManifestEntry `json:"entries"`
}

type SiteManifestResponse struct {
	Count int `json:"count"`
}

type SyncRead struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}

type SiteSyncReadRequest struct {
	SiteID int64      `json:"site_id"`
	Reads  []SyncRead `json:"reads"`
}

// SyncChunk is a part of a file as read now: Missing, it is gone or no
// longer a regular file.
type SyncChunk struct {
	Path    string `json:"path"`
	Offset  int64  `json:"offset"`
	Data    []byte `json:"data,omitempty"`
	Size    int64  `json:"size"` // of the whole file
	MTime   int64  `json:"mtime"`
	Missing bool   `json:"missing,omitempty"`
}

type SiteSyncReadResponse struct {
	Chunks []SyncChunk `json:"chunks"`
}

// SyncWrite writes Data at Offset of a file (0 starts it over); Final puts
// the file in place with Mode and MTime.
type SyncWrite struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Data   []byte `json:"data,omitempty"`
	Final  bool   `json:"final,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	MTime  int64  `json:"mtime,omitempty"`
}

type SiteSyncWriteRequest struct {
	SiteID int64           `json:"site_id"`
	Delete []string        `json:"delete,omitempty"`
	Mkdir  []ManifestEntry `json:"mkdir,omitempty"`
	Write  []SyncWrite     `json:"write,omitempty"`
	Chmod  []ManifestEntry `json:"chmod,omitempty"`
}

// Validate bounds a read request.
func (r *SiteSyncReadRequest) Validate() error {
	if r.SiteID <= 0 {
		return errors.New("site_id required")
	}
	if len(r.Reads) > MaxSyncOps {
		return errors.New("too many reads")
	}
	total := 0
	for _, x := range r.Reads {
		if x.Offset < 0 || x.Length < 0 {
			return fmt.Errorf("%s: bad range", x.Path)
		}
		total += x.Length
	}
	if total > MaxSyncData {
		return errors.New("too much data in one read")
	}
	return nil
}

// Validate bounds a write request.
func (r *SiteSyncWriteRequest) Validate() error {
	if r.SiteID <= 0 {
		return errors.New("site_id required")
	}
	if len(r.Delete)+len(r.Mkdir)+len(r.Write)+len(r.Chmod) > MaxSyncOps {
		return errors.New("too many operations")
	}
	total := 0
	for _, w := range r.Write {
		if w.Offset < 0 {
			return fmt.Errorf("%s: bad offset", w.Path)
		}
		total += len(w.Data)
	}
	if total > MaxSyncData {
		return errors.New("too much data in one write")
	}
	for _, m := range append(append([]ManifestEntry{}, r.Mkdir...), r.Chmod...) {
		if m.Mode&^0o777 != 0 {
			return fmt.Errorf("%s: bad mode", m.Path)
		}
	}
	for _, w := range r.Write {
		if w.Mode&^0o777 != 0 {
			return fmt.Errorf("%s: bad mode", w.Path)
		}
	}
	return nil
}
