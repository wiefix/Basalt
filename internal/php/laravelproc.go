package php

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// LaravelProcess is the state of one of a site's Laravel services.
type LaravelProcess struct {
	Name     string    `json:"name"`      // schedule, queue-1, …
	State    string    `json:"state"`     // systemd's ActiveState: active, activating, failed, inactive, …
	SubState string    `json:"sub_state"` // running, auto-restart, dead, …
	Since    time.Time `json:"since,omitzero"`
	Restarts int       `json:"restarts"` // automatic restarts since the service was started
}

// LaravelLogLine is a line the site's Laravel processes wrote (their
// output goes to the journal).
type LaravelLogLine struct {
	Time    time.Time `json:"time"`
	Process string    `json:"process"`
	Message string    `json:"message"`
	// Error: logged with an error priority (a line starting with "<3>" or
	// lower, journald's prefix; stderr is logged like stdout).
	Error bool `json:"error,omitempty"`
}

// MaxLaravelLogLines bounds a LaravelLog request.
const MaxLaravelLogLines = 1000

// laravelLineMax bounds a line's length (a stack trace dumped on one line).
const laravelLineMax = 4 << 10

// output runs systemctl or journalctl and returns what it printed.
func (h *Host) output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if h.Output != nil {
		return h.Output(ctx, name, args...)
	}
	if name != "systemctl" && name != "journalctl" {
		return nil, fmt.Errorf("%s is not allowed", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %s", name, lastLine(stderr.String()))
	}
	return out, nil
}

// laravelServices lists the site's Laravel services, in order: the
// scheduler, then the workers by number.
func (h *Host) laravelServices(siteID int64) ([]string, error) {
	units, err := h.laravelUnits(siteID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, u := range units {
		if strings.HasSuffix(u, ".service") {
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return laravelOrder(siteID, a) - laravelOrder(siteID, b) })
	return out, nil
}

func laravelOrder(siteID int64, unit string) int {
	name := laravelName(siteID, unit)
	if n, ok := strings.CutPrefix(name, "queue-"); ok {
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

// laravelName is a service's short name: schedule, queue-1, ….
func laravelName(siteID int64, unit string) string {
	return strings.TrimSuffix(strings.TrimPrefix(unit, laravelPrefix(siteID)), ".service")
}

// unitProps are systemd properties of units, by unit name.
func (h *Host) unitProps(ctx context.Context, units []string, props ...string) (map[string]map[string]string, error) {
	args := append([]string{"show", "--timestamp=unix", "--property=Id," + strings.Join(props, ",")}, units...)
	out, err := h.output(ctx, "systemctl", args...)
	if err != nil {
		return nil, err
	}
	res := map[string]map[string]string{}
	cur := map[string]string{}
	flush := func() {
		if id := cur["Id"]; id != "" {
			res[id] = cur
		}
		cur = map[string]string{}
	}
	for line := range strings.Lines(string(out)) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			flush()
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			cur[k] = v
		}
	}
	flush()
	return res, nil
}

// unixTimestamp parses systemd's "@<seconds>" (zero for none).
func unixTimestamp(v string) time.Time {
	s, ok := strings.CutPrefix(v, "@")
	if !ok {
		return time.Time{}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// LaravelStatus reports the state of the site's Laravel services.
func (h *Host) LaravelStatus(ctx context.Context, siteID int64) ([]LaravelProcess, error) {
	if !h.Units {
		return nil, nil
	}
	units, err := h.laravelServices(siteID)
	if err != nil || len(units) == 0 {
		return nil, err
	}
	props, err := h.unitProps(ctx, units, "ActiveState", "SubState", "ActiveEnterTimestamp", "NRestarts")
	if err != nil {
		return nil, err
	}
	out := make([]LaravelProcess, 0, len(units))
	for _, u := range units {
		p := props[u]
		n, _ := strconv.Atoi(p["NRestarts"])
		out = append(out, LaravelProcess{Name: laravelName(siteID, u), State: p["ActiveState"], SubState: p["SubState"],
			Since: unixTimestamp(p["ActiveEnterTimestamp"]), Restarts: n})
	}
	return out, nil
}

// LaravelRestart restarts the site's Laravel services (a worker finishes
// its job first: queue:work stops at SIGTERM between jobs). It does not
// wait for them to be up again.
func (h *Host) LaravelRestart(siteID int64) error {
	if !h.Units {
		return errors.New("Laravel processes need systemd on the server")
	}
	units, err := h.laravelServices(siteID)
	if err != nil {
		return err
	}
	if len(units) == 0 {
		return errors.New("the site runs no Laravel processes")
	}
	return h.Systemctl(append([]string{"restart", "--no-block"}, units...)...)
}

// LaravelLog returns the last lines (at most n) the site's Laravel
// processes wrote, oldest first: those of the scheduler and of every
// worker there may have been.
func (h *Host) LaravelLog(ctx context.Context, siteID int64, n int) ([]LaravelLogLine, error) {
	if !h.Units {
		return nil, nil
	}
	n = min(max(n, 1), MaxLaravelLogLines)
	id := strconv.FormatInt(siteID, 10)
	args := []string{"--no-pager", "--quiet", "--output=json", "--lines=" + strconv.Itoa(n),
		"--output-fields=MESSAGE,SYSLOG_IDENTIFIER,PRIORITY"}
	args = append(args, "--identifier=basalt-site-"+id+"-schedule")
	for i := 1; i <= MaxLaravelWorkers; i++ {
		args = append(args, "--identifier=basalt-site-"+id+"-queue-"+strconv.Itoa(i))
	}
	out, err := h.output(ctx, "journalctl", args...)
	if err != nil {
		return nil, err
	}
	prefix := "basalt-site-" + id + "-"
	lines := []LaravelLogLine{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e struct {
			Message    json.RawMessage `json:"MESSAGE"`
			Identifier string          `json:"SYSLOG_IDENTIFIER"`
			Priority   string          `json:"PRIORITY"`
			Realtime   string          `json:"__REALTIME_TIMESTAMP"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		us, _ := strconv.ParseInt(e.Realtime, 10, 64)
		prio, err := strconv.Atoi(e.Priority)
		if err != nil {
			prio = 6
		}
		lines = append(lines, LaravelLogLine{
			Time: time.UnixMicro(us).UTC(), Process: strings.TrimPrefix(e.Identifier, prefix),
			Message: journalMessage(e.Message), Error: prio <= 3,
		})
	}
	return lines, sc.Err()
}

// journalMessage decodes a journal JSON field: a string, or an array of
// bytes when it is not valid UTF-8 (or null when too long).
func journalMessage(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var b []byte
		var arr []int
		if json.Unmarshal(raw, &arr) == nil {
			for _, c := range arr {
				b = append(b, byte(c))
			}
		}
		s = strings.ToValidUTF8(string(b), "�")
	}
	if len(s) > laravelLineMax {
		s = s[:laravelLineMax]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return s
}

// restartUpdatedLaravel restarts the Laravel services that started before
// their PHP CLI (or an extension) was last installed, like RestartUpdated
// does for the FPM masters; a worker would otherwise run the old code for
// up to an hour, the scheduler's loop until it is restarted.
func (h *Host) restartUpdatedLaravel(extensions []string) ([]int64, error) {
	paths, err := filepath.Glob(filepath.Join(h.UnitDir, "basalt-php-site-*-*.service"))
	if err != nil || len(paths) == 0 {
		return nil, err
	}
	var units []string
	for _, p := range paths {
		units = append(units, filepath.Base(p))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	props, err := h.unitProps(ctx, units, "ActiveState", "ExecMainStartTimestamp")
	if err != nil {
		return nil, err
	}
	var done []int64
	var errs []error
	for i, u := range units {
		p := props[u]
		started := unixTimestamp(p["ExecMainStartTimestamp"])
		if p["ActiveState"] != "active" || started.IsZero() {
			continue
		}
		id, err := strconv.ParseInt(strings.SplitN(strings.TrimPrefix(u, "basalt-php-site-"), "-", 2)[0], 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		bin := unitBinary(paths[i])
		if bin == "" {
			continue
		}
		stale := false
		for _, f := range append([]string{bin}, extensions...) {
			// Start times have whole seconds: a file put in place in the
			// same second counts as newer.
			if fi, err := os.Stat(f); err == nil && !changeTime(fi).Before(started) {
				stale = true
				break
			}
		}
		if !stale {
			continue
		}
		if err := h.Systemctl("restart", u); err != nil {
			errs = append(errs, err)
			continue
		}
		if !slices.Contains(done, id) {
			done = append(done, id)
		}
	}
	return done, errors.Join(errs...)
}
