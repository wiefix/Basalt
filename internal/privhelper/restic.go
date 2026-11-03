package privhelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/restic"
)

// restic runs in the helper: it reads every site's files for a backup and
// writes them back, with their owners, on a restore. The agent names a
// repository (the panel's description of it), a site by its id and what
// to do; paths of the host come from the helper's configuration. Database
// dumps and Docker volumes the agent produces or consumes itself, through
// a pipe it hands over (see files.go).

// MsgRestic runs one restic command (ResticRequest → ResticResult; the
// lines restic prints arrive as stream frames, ResticLine).
const MsgRestic = "restic"

// restic commands of a ResticRequest.
const (
	ResticVersion     = "version"     // → Version
	ResticInit        = "init"        // create the repository unless it exists → Created
	ResticBackupSite  = "backup_site" // the files of SiteID → Summary
	ResticBackupStdin = "backup_stdin"
	ResticSnapshots   = "snapshots" // of Host with all Tags → Snapshots
	ResticSnapshot    = "snapshot"  // Snapshot by id → Snapshots (none or one)
	ResticForget      = "forget"    // Host, Tags, Keep
	ResticPrune       = "prune"
	ResticCheck       = "check"        // ReadData
	ResticRestoreSite = "restore_site" // Snapshot of SiteID's files, over them
	ResticList        = "ls"           // Path of Snapshot → Node, Children
	ResticDump        = "dump"         // file Path of Snapshot to the stdout file
	ResticDumpTree    = "dump_tree"    // folder Path of Snapshot as a Format archive
)

// ResticRequest is one restic command. A backup from standard input reads
// the request's first file (stored as StdinFilename); a dump writes to
// it.
type ResticRequest struct {
	Op            string                `json:"op"`
	Repo          agentproto.BackupRepo `json:"repo"`
	Host          string                `json:"host,omitempty"`
	Tags          []string              `json:"tags,omitempty"`
	SiteID        int64                 `json:"site_id,omitempty"`
	StdinFilename string                `json:"stdin_filename,omitempty"`
	Keep          restic.Keep           `json:"keep"`
	Snapshot      string                `json:"snapshot,omitempty"`
	Path          string                `json:"path,omitempty"`
	Format        string                `json:"format,omitempty"`
	ReadData      string                `json:"read_data,omitempty"`
}

// ResticResult is what a command found. A failure of restic itself comes
// as Code and Error (restic's exit code tells a missing repository).
type ResticResult struct {
	Version    string            `json:"version,omitempty"`
	Created    bool              `json:"created,omitempty"`
	Summary    *restic.Summary   `json:"summary,omitempty"`
	Incomplete bool              `json:"incomplete,omitempty"`
	Snapshots  []restic.Snapshot `json:"snapshots,omitempty"`
	Node       *restic.Node      `json:"node,omitempty"`
	Children   []restic.Node     `json:"children,omitempty"`
	Code       int               `json:"code,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// ResticLine is a line restic printed.
type ResticLine struct {
	Line string `json:"line"`
}

var (
	snapshotIDRE = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	tagRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9=_.-]{0,199}$`)
	fileNameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,199}$`)
	readDataRE   = regexp.MustCompile(`^[1-9][0-9]{0,3}/[1-9][0-9]{0,3}$`)
)

func (r *ResticRequest) validate() error {
	if r.Op != ResticVersion {
		if err := r.Repo.Validate(); err != nil {
			return err
		}
	}
	if r.Host != "" && !tagRE.MatchString(r.Host) {
		return errors.New("invalid snapshot host")
	}
	for _, t := range r.Tags {
		if !tagRE.MatchString(t) || strings.Contains(t, ",") {
			return fmt.Errorf("invalid tag %q", t)
		}
	}
	if r.Snapshot != "" && !snapshotIDRE.MatchString(r.Snapshot) {
		return errors.New("invalid snapshot id")
	}
	if r.Path != "" && (!strings.HasPrefix(r.Path, "/") || !agentproto.ValidSnapshotPath(r.Path)) {
		return errors.New("invalid path in a snapshot")
	}
	if r.ReadData != "" && !readDataRE.MatchString(r.ReadData) {
		return errors.New("invalid part of the data to read")
	}
	switch r.Op {
	case ResticBackupStdin:
		if !fileNameRE.MatchString(r.StdinFilename) {
			return errors.New("invalid file name of a backup")
		}
	case ResticBackupSite, ResticRestoreSite:
		if r.SiteID <= 0 {
			return errSiteID
		}
	case ResticDumpTree:
		if r.Format != "tar" && r.Format != "zip" {
			return errors.New("invalid archive format")
		}
	}
	switch r.Op {
	case ResticBackupSite, ResticBackupStdin, ResticSnapshots, ResticForget:
		if r.Host == "" {
			return errors.New("no snapshot host")
		}
	case ResticSnapshot, ResticRestoreSite, ResticList, ResticDump, ResticDumpTree:
		if r.Snapshot == "" {
			return errors.New("no snapshot")
		}
	}
	return nil
}

// Stdio is the files of a restic command: In for a backup from standard
// input, Out for a dump.
type Stdio struct {
	In, Out *os.File
}

func (s Stdio) files() []*os.File {
	var f []*os.File
	for _, x := range []*os.File{s.In, s.Out} {
		if x != nil {
			f = append(f, x)
		}
	}
	return f
}

// handleRestic serves MsgRestic: the files of the request's connection are
// its standard input or output.
func (h *Helper) handleRestic(ctx context.Context, payload json.RawMessage) (any, error) {
	req, err := decode[ResticRequest](payload)
	if err != nil {
		return nil, err
	}
	var stdio Stdio
	files := requestFiles(ctx)
	if len(files) > 1 {
		return nil, errors.New("too many files for restic")
	}
	if len(files) == 1 {
		if req.Op == ResticBackupStdin {
			stdio.In = files[0]
		} else {
			stdio.Out = files[0]
		}
	}
	var line func(string)
	if send := agentproto.StreamSender(ctx); send != nil {
		line = func(s string) { send(ResticLine{Line: s}) }
	}
	res, err := h.Restic(ctx, req, stdio, line)
	if re := (*restic.Error)(nil); errors.As(err, &re) {
		res.Code, res.Error = re.Code, re.Message
		return res, nil
	}
	return res, err
}

// Restic runs one restic command. line (may be nil) receives what restic
// prints.
func (h *Helper) Restic(ctx context.Context, req ResticRequest, stdio Stdio, line func(string)) (ResticResult, error) {
	var res ResticResult
	if err := req.validate(); err != nil {
		return res, err
	}
	if h.cfg.Restic == "" {
		return res, errors.New("backups are disabled on this server (no restic)")
	}
	r := &restic.Runner{Binary: h.cfg.Restic, CacheDir: filepath.Join(h.cfg.StateDir, "restic-cache"), Line: line}
	if req.Op == ResticVersion {
		v, err := r.Version(ctx)
		res.Version = v
		return res, err
	}
	repo, err := h.resticRepo(req.Repo)
	if err != nil {
		return res, err
	}
	if (stdio.In != nil) != (req.Op == ResticBackupStdin) {
		return res, errors.New("standard input is only for a backup from it")
	}
	if (stdio.Out != nil) != (req.Op == ResticDump || req.Op == ResticDumpTree) {
		return res, errors.New("standard output is only for a dump")
	}
	switch req.Op {
	case ResticInit:
		res.Created, err = r.Init(ctx, repo)
	case ResticBackupSite, ResticBackupStdin:
		return h.resticBackup(ctx, r, repo, req, stdio.In)
	case ResticSnapshots:
		res.Snapshots, err = r.Snapshots(ctx, repo, req.Host, req.Tags)
	case ResticSnapshot:
		var s *restic.Snapshot
		if s, err = r.SnapshotByID(ctx, repo, req.Snapshot); s != nil {
			res.Snapshots = []restic.Snapshot{*s}
		}
	case ResticForget:
		err = r.Forget(ctx, repo, req.Host, req.Tags, req.Keep)
	case ResticPrune:
		err = r.Prune(ctx, repo)
	case ResticCheck:
		err = r.Check(ctx, repo, req.ReadData)
	case ResticRestoreSite:
		err = h.restoreSite(ctx, r, repo, req)
	case ResticList:
		res.Node, res.Children, err = r.List(ctx, repo, req.Snapshot, req.Path)
	case ResticDump:
		err = r.Dump(ctx, repo, req.Snapshot, req.Path, stdio.Out)
	case ResticDumpTree:
		err = r.DumpTree(ctx, repo, req.Snapshot, req.Path, req.Format, stdio.Out)
	default:
		return res, fmt.Errorf("unknown restic command %q", req.Op)
	}
	return res, err
}

func (h *Helper) siteDir(id int64) string {
	return filepath.Join(h.cfg.SitesRoot, strconv.FormatInt(id, 10))
}

// resticBackup makes one snapshot: of a site's files, or of what the agent
// writes into the pipe. The agent keeps its end of the pipe open until it
// knows the dump succeeded; if it stops the request instead (or goes away),
// a snapshot restic may still have made of a cut-off dump is removed.
func (h *Helper) resticBackup(ctx context.Context, r *restic.Runner, repo restic.Repo, req ResticRequest, stdin io.Reader) (ResticResult, error) {
	o := restic.BackupOptions{Host: req.Host, Tags: req.Tags}
	if req.Op == ResticBackupSite {
		dir := h.siteDir(req.SiteID)
		if _, err := os.Lstat(dir); err != nil {
			return ResticResult{}, fmt.Errorf("the site's folder: %w", err)
		}
		o.Paths = []string{dir}
		// Temporary files and the file manager's unfinished uploads.
		// tmp/ itself stays, so a restore keeps the directory PHP uses.
		o.Excludes = []string{filepath.Join(dir, "tmp", "*"), filepath.Join(dir, "**", ".basalt-upload-*")}
	} else {
		o.Stdin, o.StdinFilename = stdin, req.StdinFilename
	}
	s, err := r.Backup(ctx, repo, o)
	if ctx.Err() != nil {
		if s.SnapshotID != "" {
			forget := *r
			forget.Line = nil
			if ferr := forget.ForgetIDs(context.WithoutCancel(ctx), repo, s.SnapshotID); ferr != nil {
				h.log.Warn("could not remove the snapshot of a stopped backup", "snapshot", s.SnapshotID, "err", ferr)
			}
		}
		return ResticResult{}, ctx.Err()
	}
	if err != nil {
		return ResticResult{}, err
	}
	return ResticResult{Summary: &s, Incomplete: s.Incomplete}, nil
}

// resticRepo turns the panel's description into what restic opens. SFTP
// keys and known hosts are the helper's; a local repository is created
// where Basalt and other users do not write.
func (h *Helper) resticRepo(spec agentproto.BackupRepo) (restic.Repo, error) {
	if err := os.MkdirAll(filepath.Join(h.cfg.StateDir, "restic-cache"), 0o700); err != nil {
		return restic.Repo{}, err
	}
	if spec.Kind == agentproto.StorageLocal {
		if err := h.localRepoAllowed(spec.Path); err != nil {
			return restic.Repo{}, err
		}
		if err := os.MkdirAll(spec.Path, 0o700); err != nil {
			return restic.Repo{}, fmt.Errorf("create the backup directory: %w", err)
		}
	}
	return restic.FromSpec(spec, filepath.Join(h.cfg.StateDir, "backup-ssh"))
}

// localRepoAllowed keeps local repositories out of places other users
// reach (site directories) or Basalt manages.
func (h *Helper) localRepoAllowed(p string) error {
	for _, forbidden := range []string{h.cfg.SitesRoot, h.cfg.AgentDataDir, h.cfg.StateDir} {
		if forbidden == "" {
			continue
		}
		abs, err := filepath.Abs(forbidden)
		if err != nil {
			continue
		}
		if p == abs || strings.HasPrefix(p+"/", abs+"/") || strings.HasPrefix(abs+"/", p+"/") {
			return errors.New("the backup directory must not contain or be inside the sites or the agent's data directory")
		}
	}
	for _, sys := range []string{"/proc", "/sys", "/dev", "/run", "/etc", "/bin", "/sbin", "/usr", "/lib", "/boot"} {
		if p == sys || strings.HasPrefix(p, sys+"/") {
			return fmt.Errorf("the backup directory cannot be under %s", sys)
		}
	}
	return nil
}
