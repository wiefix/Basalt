// Package restic runs the restic command line tool for backups. Commands
// are argument arrays, never shell lines; secrets reach restic through its
// environment only, never its arguments.
package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MinVersion is the oldest restic with everything used here
// (--stdin-from-command, restore of a subfolder with --delete).
const MinVersion = "0.17.0"

// Repo is a repository and how to open it.
type Repo struct {
	Location string            // e.g. "s3:https://host/bucket/path", "sftp:user@host:/path", "/var/backups/x"
	Password string            // repository password
	Env      map[string]string // backend credentials (AWS_ACCESS_KEY_ID, ...)
	Options  []string          // -o key=value
}

// Runner runs restic.
type Runner struct {
	Binary   string // path or name in PATH
	CacheDir string
	// Line, if set, receives restic's progress and error output, one line
	// at a time (no secrets: restic never prints the password).
	Line func(string)
}

// retryLock is how long a command waits for a locked repository.
const retryLock = "5m"

// ExitNoRepository is restic's exit code for a repository that does not exist.
const ExitNoRepository = 10

// Error is a failed restic command.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return e.Message }

// ErrNoRepository: the repository has not been created yet.
var ErrNoRepository = errors.New("repository does not exist")

func (e *Error) Is(target error) bool { return target == ErrNoRepository && e.Code == ExitNoRepository }

func (r *Runner) command(ctx context.Context, repo Repo, args ...string) *exec.Cmd {
	full := make([]string, 0, len(args)+2*len(repo.Options)+2)
	// Another restic using the repository (a check holds it exclusively)
	// or a lock left by a killed run: wait rather than fail at once.
	full = append(full, "--repo", repo.Location, "--retry-lock", retryLock)
	for _, o := range repo.Options {
		full = append(full, "-o", o)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, r.Binary, full...)
	// A minimal environment: nothing of the agent's own leaks in.
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.CacheDir,
		"RESTIC_CACHE_DIR=" + r.CacheDir,
		"RESTIC_PASSWORD=" + repo.Password,
		"RESTIC_PROGRESS_FPS=0.2",
		"TMPDIR=" + os.TempDir(),
	}
	for k, v := range repo.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.WaitDelay = 10 * time.Second
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) } // lets restic remove its lock
	return cmd
}

// run runs a command, returning its standard output; stderr goes to Line
// and into the error.
func (r *Runner) run(ctx context.Context, repo Repo, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := r.command(ctx, repo, args...)
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	tail := &tailWriter{line: r.Line}
	cmd.Stderr = tail
	err := cmd.Run()
	tail.flush()
	if err != nil {
		return out.Bytes(), r.wrap(err, tail.last(), out.Bytes())
	}
	return out.Bytes(), nil
}

// wrap turns a failed run into an *Error with restic's own message.
func (r *Runner) wrap(err error, stderr string, stdout []byte) error {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return fmt.Errorf("run restic: %w", err)
	}
	msg := strings.TrimSpace(stderr)
	// With --json the reason is an exit_error message on stdout.
	for line := range bytes.SplitSeq(stdout, []byte("\n")) {
		var m struct {
			Type    string `json:"message_type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &m) == nil {
			switch m.Type {
			case "exit_error":
				msg = m.Message
			case "error":
				if msg == "" {
					msg = m.Error.Message
				}
			}
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("restic exited with code %d", ee.ExitCode())
	}
	return &Error{Code: ee.ExitCode(), Message: strings.TrimPrefix(msg, "Fatal: ")}
}

var versionRE = regexp.MustCompile(`restic (\d+\.\d+\.\d+)`)

// Version returns restic's version, or an error if it is missing or older
// than MinVersion.
func (r *Runner) Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, r.Binary, "version").Output()
	if err != nil {
		return "", fmt.Errorf("restic is not installed: %w", err)
	}
	m := versionRE.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unexpected restic version output %q", out)
	}
	v := string(m[1])
	if !versionAtLeast(v, MinVersion) {
		return v, fmt.Errorf("restic %s is too old; %s or newer is required", v, MinVersion)
	}
	return v, nil
}

func versionAtLeast(have, want string) bool {
	h, w := strings.Split(have, "."), strings.Split(want, ".")
	for i := range 3 {
		a, _ := strconv.Atoi(h[i])
		b, _ := strconv.Atoi(w[i])
		if a != b {
			return a > b
		}
	}
	return true
}

// Init creates the repository unless it exists; it reports whether it did.
func (r *Runner) Init(ctx context.Context, repo Repo) (bool, error) {
	// A missing repository is expected here: keep restic's complaint out
	// of the log.
	quiet := *r
	quiet.Line = nil
	_, err := quiet.run(ctx, repo, nil, "cat", "config")
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, ErrNoRepository) {
		return false, err
	}
	if _, err := r.run(ctx, repo, nil, "init"); err != nil {
		return false, err
	}
	return true, nil
}

// BackupOptions select what one snapshot holds: Paths, or the standard
// output of StdinCommand stored as StdinFilename.
type BackupOptions struct {
	Host          string
	Tags          []string
	Paths         []string
	Excludes      []string
	StdinCommand  []string
	StdinFilename string
}

// Summary is restic's report on a backup.
type Summary struct {
	SnapshotID     string  `json:"snapshot_id"`
	FilesNew       int     `json:"files_new"`
	FilesChanged   int     `json:"files_changed"`
	DataAdded      uint64  `json:"data_added"`
	BytesProcessed uint64  `json:"total_bytes_processed"`
	FilesProcessed int     `json:"total_files_processed"`
	Duration       float64 `json:"total_duration"`
	// Incomplete: some files could not be read (restic exit code 3); the
	// snapshot holds the rest.
	Incomplete bool `json:"-"`
}

// Backup creates a snapshot.
func (r *Runner) Backup(ctx context.Context, repo Repo, o BackupOptions) (Summary, error) {
	args := []string{"backup", "--json", "--quiet", "--host", o.Host}
	for _, t := range o.Tags {
		args = append(args, "--tag", t)
	}
	for _, e := range o.Excludes {
		args = append(args, "--exclude", e)
	}
	if len(o.StdinCommand) > 0 {
		args = append(args, "--stdin-filename", o.StdinFilename, "--stdin-from-command", "--")
		args = append(args, o.StdinCommand...)
	} else {
		args = append(args, "--")
		args = append(args, o.Paths...)
	}
	out, err := r.run(ctx, repo, nil, args...)
	var s Summary
	for line := range bytes.SplitSeq(out, []byte("\n")) {
		var m struct {
			Type string `json:"message_type"`
		}
		if json.Unmarshal(line, &m) == nil && m.Type == "summary" {
			json.Unmarshal(line, &s)
		}
	}
	var re *Error
	if errors.As(err, &re) && re.Code == 3 && s.SnapshotID != "" {
		s.Incomplete = true
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if s.SnapshotID == "" {
		return s, errors.New("restic reported no snapshot")
	}
	return s, nil
}

// Snapshot is one snapshot in a repository.
type Snapshot struct {
	ID      string    `json:"id"`
	ShortID string    `json:"short_id"`
	Time    time.Time `json:"time"`
	Host    string    `json:"hostname"`
	Tags    []string  `json:"tags"`
	Paths   []string  `json:"paths"`
	Summary *struct {
		TotalBytes uint64 `json:"total_bytes_processed"`
		DataAdded  uint64 `json:"data_added"`
	} `json:"summary,omitempty"`
}

// Snapshots lists the snapshots of host that have all tags.
func (r *Runner) Snapshots(ctx context.Context, repo Repo, host string, tags []string) ([]Snapshot, error) {
	// Listing needs no lock: it works while a check holds the repository.
	args := []string{"snapshots", "--no-lock", "--json", "--host", host}
	if len(tags) > 0 {
		args = append(args, "--tag", strings.Join(tags, ","))
	}
	out, err := r.run(ctx, repo, nil, args...)
	if err != nil {
		return nil, err
	}
	var list []Snapshot
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse snapshots: %w", err)
	}
	return list, nil
}

// Keep is a retention policy: the newest Last snapshots, plus the newest
// of each of the last Daily days, Weekly weeks and Monthly months.
type Keep struct {
	Last    int `json:"last"`
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}

// Forget removes snapshots of host with all tags that the policy does not
// keep, separately for each set of tags. Data is freed by Prune.
func (r *Runner) Forget(ctx context.Context, repo Repo, host string, tags []string, k Keep) error {
	args := []string{"forget", "--quiet", "--host", host, "--group-by", "host,tags",
		"--keep-last", strconv.Itoa(max(k.Last, 1))}
	if len(tags) > 0 {
		args = append(args, "--tag", strings.Join(tags, ","))
	}
	for flag, n := range map[string]int{"--keep-daily": k.Daily, "--keep-weekly": k.Weekly, "--keep-monthly": k.Monthly} {
		if n > 0 {
			args = append(args, flag, strconv.Itoa(n))
		}
	}
	_, err := r.run(ctx, repo, nil, args...)
	return err
}

// Prune frees the data of forgotten snapshots.
func (r *Runner) Prune(ctx context.Context, repo Repo) error {
	_, err := r.run(ctx, repo, nil, "prune", "--quiet", "--max-unused", "10%")
	return err
}

// Check verifies the repository: its structure and, with readData (a
// restic --read-data-subset value such as "3/52"), that part of the data
// can be read and decrypted. Its findings go to Line.
func (r *Runner) Check(ctx context.Context, repo Repo, readData string) error {
	args := []string{"check"}
	if readData != "" {
		args = append(args, "--read-data-subset", readData)
	}
	cmd := r.command(ctx, repo, args...)
	// Standard output and error are copied concurrently: one Line at a time.
	line := r.Line
	if line != nil {
		var mu sync.Mutex
		line = func(s string) {
			mu.Lock()
			defer mu.Unlock()
			r.Line(s)
		}
	}
	out := &tailWriter{line: line, skip: checkNoise}
	tail := &tailWriter{line: line}
	cmd.Stdout, cmd.Stderr = out, tail
	err := cmd.Run()
	out.flush()
	tail.flush()
	if err != nil {
		return r.wrap(err, tail.last(), nil)
	}
	return nil
}

// checkNoise reports output of restic check nobody needs: progress bars
// ("[0:01] 50.00%  1 / 2 packs") and where its temporary cache is.
func checkNoise(s string) bool {
	return strings.HasPrefix(s, "[") || strings.HasPrefix(s, "using temporary cache")
}

// RestoreTree makes target hold exactly the folder src of a snapshot:
// files are restored and files not in the snapshot are deleted.
func (r *Runner) RestoreTree(ctx context.Context, repo Repo, snapshot, src, target string) error {
	_, err := r.run(ctx, repo, nil, "restore", "--quiet", snapshot+":"+src, "--target", target, "--delete")
	return err
}

// SnapshotByID returns one snapshot, nil if there is none with that id.
func (r *Runner) SnapshotByID(ctx context.Context, repo Repo, id string) (*Snapshot, error) {
	// An unknown id is no error for restic: it lists nothing.
	out, err := r.run(ctx, repo, nil, "snapshots", "--no-lock", "--json", id)
	if err != nil {
		return nil, err
	}
	var list []Snapshot
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parse snapshots: %w", err)
	}
	if len(list) != 1 {
		return nil, nil
	}
	return &list[0], nil
}

// Node is a file, directory or other entry in a snapshot.
type Node struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"` // file, dir, symlink, ...
	Path  string    `json:"path"`
	Size  uint64    `json:"size"`
	Mode  uint32    `json:"mode"` // Go fs.FileMode bits
	MTime time.Time `json:"mtime"`
}

// List returns the entry at an absolute path of a snapshot and, for a
// directory, what it holds directly. It reports a missing path as nil.
func (r *Runner) List(ctx context.Context, repo Repo, snapshot, p string) (*Node, []Node, error) {
	out, err := r.run(ctx, repo, nil, "ls", "--no-lock", "--json", snapshot, p)
	if err != nil {
		return nil, nil, err
	}
	var self *Node
	var children []Node
	for line := range bytes.SplitSeq(out, []byte("\n")) {
		var n struct {
			Node
			Type string `json:"struct_type"`
		}
		if len(line) == 0 || json.Unmarshal(line, &n) != nil || n.Type != "node" {
			continue
		}
		if n.Path == p {
			self = &n.Node
		} else {
			children = append(children, n.Node)
		}
	}
	return self, children, nil
}

// DumpTree writes the directory dir of a snapshot to w as an archive
// ("tar" or "zip") with paths relative to dir.
func (r *Runner) DumpTree(ctx context.Context, repo Repo, snapshot, dir, format string, w io.Writer) error {
	return r.dump(ctx, w, repo, "dump", "--quiet", "--archive", format, snapshot+":"+dir, "/")
}

// Dump writes one file of a snapshot to w.
func (r *Runner) Dump(ctx context.Context, repo Repo, snapshot, file string, w io.Writer) error {
	return r.dump(ctx, w, repo, "dump", "--quiet", snapshot, file)
}

func (r *Runner) dump(ctx context.Context, w io.Writer, repo Repo, args ...string) error {
	cmd := r.command(ctx, repo, args...)
	cmd.Stdout = w
	tail := &tailWriter{line: r.Line}
	cmd.Stderr = tail
	err := cmd.Run()
	tail.flush()
	if err != nil {
		return r.wrap(err, tail.last(), nil)
	}
	return nil
}

// tailWriter splits output into lines for Line and keeps the last few
// for error messages.
type tailWriter struct {
	line    func(string)
	skip    func(string) bool // lines left out
	partial []byte
	lines   []string
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.partial = append(t.partial, p...)
	for {
		i := bytes.IndexAny(t.partial, "\r\n")
		if i < 0 {
			break
		}
		t.emit(string(t.partial[:i]))
		t.partial = t.partial[i+1:]
	}
	if len(t.partial) > 64<<10 {
		t.emit(string(t.partial))
		t.partial = nil
	}
	return len(p), nil
}

func (t *tailWriter) emit(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	// With --json, restic writes its errors as JSON too.
	if strings.HasPrefix(s, "{") {
		var m struct {
			Type    string `json:"message_type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(s), &m) == nil {
			switch {
			case m.Message != "":
				s = m.Message
			case m.Error.Message != "":
				s = m.Error.Message
			}
		}
	}
	s = strings.TrimPrefix(s, "Fatal: ")
	if t.skip != nil && t.skip(s) {
		return
	}
	if t.line != nil {
		t.line(s)
	}
	t.lines = append(t.lines, s)
	if len(t.lines) > 5 {
		t.lines = t.lines[1:]
	}
}

func (t *tailWriter) flush() {
	if len(t.partial) > 0 {
		t.emit(string(t.partial))
		t.partial = nil
	}
}

func (t *tailWriter) last() string { return strings.Join(t.lines, "\n") }
