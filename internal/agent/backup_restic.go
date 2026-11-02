package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/privhelper"
	"github.com/wiefix/basalt/internal/restic"
)

// resticRun runs restic commands in the privileged helper, which reads and
// restores the sites' files. What restic reads from or writes to here (a
// database dump, a volume's tar, a download) goes through a pipe.
type resticRun struct {
	priv privileged
	// line, if set, receives what restic prints.
	line func(string)
}

func (r resticRun) run(ctx context.Context, req privhelper.ResticRequest, stdio privhelper.Stdio) (privhelper.ResticResult, error) {
	return r.priv.Restic(ctx, req, stdio, r.line)
}

func (r resticRun) Version(ctx context.Context) (string, error) {
	res, err := r.run(ctx, privhelper.ResticRequest{Op: privhelper.ResticVersion}, privhelper.Stdio{})
	return res.Version, err
}

// Init creates the repository unless it exists; it reports whether it did.
func (r resticRun) Init(ctx context.Context, repo agentproto.BackupRepo) (bool, error) {
	res, err := r.run(ctx, privhelper.ResticRequest{Op: privhelper.ResticInit, Repo: repo}, privhelper.Stdio{})
	return res.Created, err
}

func summary(res privhelper.ResticResult, err error) (restic.Summary, error) {
	if err != nil {
		return restic.Summary{}, err
	}
	if res.Summary == nil {
		return restic.Summary{}, errors.New("restic reported no snapshot")
	}
	s := *res.Summary
	s.Incomplete = res.Incomplete
	return s, nil
}

// BackupSite makes a snapshot of a site's files.
func (r resticRun) BackupSite(ctx context.Context, repo agentproto.BackupRepo, tags []string, siteID int64) (restic.Summary, error) {
	return summary(r.run(ctx, privhelper.ResticRequest{Op: privhelper.ResticBackupSite, Repo: repo,
		Host: backupHost, Tags: tags, SiteID: siteID}, privhelper.Stdio{}))
}

// BackupCommand makes a snapshot of what cmd writes to its standard
// output, stored as file. A command that fails leaves no snapshot: restic
// sees the end of its input only once cmd has succeeded, and is stopped
// otherwise.
func (r resticRun) BackupCommand(ctx context.Context, repo agentproto.BackupRepo, tags []string, file string, cmd *exec.Cmd) (restic.Summary, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return restic.Summary{}, err
	}
	defer pr.Close()
	defer pw.Close()
	var stderr tailBuffer
	cmd.Stdout, cmd.Stderr = pw, &stderr
	if err := cmd.Start(); err != nil {
		return restic.Summary{}, err
	}
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err != nil {
			stop()
		} else {
			pw.Close() // the end of the dump
		}
		waited <- err
	}()
	s, rerr := summary(r.run(rctx, privhelper.ResticRequest{Op: privhelper.ResticBackupStdin, Repo: repo,
		Host: backupHost, Tags: tags, StdinFilename: file}, privhelper.Stdio{In: pr}))
	// restic has stopped: a command still writing gets an error now.
	pr.Close()
	pw.Close()
	if cmdErr := <-waited; cmdErr != nil {
		return restic.Summary{}, fmt.Errorf("%v: %s", cmdErr, stderr.String())
	}
	return s, rerr
}

// Snapshots lists the snapshots of this agent with all tags.
func (r resticRun) Snapshots(ctx context.Context, repo agentproto.BackupRepo, tags []string) ([]restic.Snapshot, error) {
	res, err := r.run(ctx, privhelper.ResticRequest{Op: privhelper.ResticSnapshots, Repo: repo, Host: backupHost, Tags: tags}, privhelper.Stdio{})
	return res.Snapshots, err
}

// SnapshotByID returns one snapshot, nil if there is none with that id.
func (r resticRun) SnapshotByID(ctx context.Context, repo agentproto.BackupRepo, id string) (*restic.Snapshot, error) {
	res, err := r.run(ctx, privhelper.ResticRequest{Op: privhelper.ResticSnapshot, Repo: repo, Snapshot: id}, privhelper.Stdio{})
	if err != nil || len(res.Snapshots) == 0 {
		return nil, err
	}
	return &res.Snapshots[0], nil
}

func (r resticRun) Forget(ctx context.Context, repo agentproto.BackupRepo, tags []string, k restic.Keep) error {
	_, err := r.run(ctx, privhelper.ResticRequest{Op: privhelper.ResticForget, Repo: repo, Host: backupHost, Tags: tags, Keep: k}, privhelper.Stdio{})
	return err
}

func (r resticRun) Prune(ctx context.Context, repo agentproto.BackupRepo) error {
	_, err := r.run(ctx, privhelper.ResticRequest{Op: privhelper.ResticPrune, Repo: repo}, privhelper.Stdio{})
	return err
}

func (r resticRun) Check(ctx context.Context, repo agentproto.BackupRepo, readData string) error {
	_, err := r.run(ctx, privhelper.ResticRequest{Op: privhelper.ResticCheck, Repo: repo, ReadData: readData}, privhelper.Stdio{})
	return err
}

// RestoreSite makes a site's folder hold exactly a snapshot of its files.
func (r resticRun) RestoreSite(ctx context.Context, repo agentproto.BackupRepo, snapshot string, siteID int64) error {
	_, err := r.run(ctx, privhelper.ResticRequest{Op: privhelper.ResticRestoreSite, Repo: repo, Snapshot: snapshot, SiteID: siteID}, privhelper.Stdio{})
	return err
}

// List returns the entry at an absolute path of a snapshot and, for a
// directory, what it holds directly. It reports a missing path as nil.
func (r resticRun) List(ctx context.Context, repo agentproto.BackupRepo, snapshot, p string) (*restic.Node, []restic.Node, error) {
	res, err := r.run(ctx, privhelper.ResticRequest{Op: privhelper.ResticList, Repo: repo, Snapshot: snapshot, Path: p}, privhelper.Stdio{})
	return res.Node, res.Children, err
}

// DumpTree writes the directory dir of a snapshot to w as an archive
// ("tar" or "zip") with paths relative to dir.
func (r resticRun) DumpTree(ctx context.Context, repo agentproto.BackupRepo, snapshot, dir, format string, w io.Writer) error {
	return r.dump(ctx, privhelper.ResticRequest{Op: privhelper.ResticDumpTree, Repo: repo, Snapshot: snapshot, Path: dir, Format: format}, w)
}

// Dump writes one file of a snapshot to w.
func (r resticRun) Dump(ctx context.Context, repo agentproto.BackupRepo, snapshot, file string, w io.Writer) error {
	return r.dump(ctx, privhelper.ResticRequest{Op: privhelper.ResticDump, Repo: repo, Snapshot: snapshot, Path: file}, w)
}

// dump has restic write to w: directly if it is a file, else through a
// pipe.
func (r resticRun) dump(ctx context.Context, req privhelper.ResticRequest, w io.Writer) error {
	if f, ok := w.(*os.File); ok {
		_, err := r.run(ctx, req, privhelper.Stdio{Out: f})
		return err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(w, pr)
		// A reader that stopped early must not leave restic blocked.
		pr.Close()
		copied <- err
	}()
	_, err = r.run(ctx, req, privhelper.Stdio{Out: pw})
	pw.Close()
	if cerr := <-copied; err == nil && cerr != nil {
		err = cerr
	}
	return err
}
