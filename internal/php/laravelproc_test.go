package php

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// laravelHost is a Host with systemd units in a temporary folder, its
// systemctl calls recorded and its systemctl show / journalctl output given
// by show and journal.
func laravelHost(t *testing.T) (h *Host, calls *[]string, show map[string]string, journal *string, outputs *[]string) {
	dir := t.TempDir()
	h = &Host{RunDir: filepath.Join(dir, "run"), UnitDir: filepath.Join(dir, "units"), ExtensionDir: filepath.Join(dir, "ext"), Units: true}
	os.MkdirAll(h.UnitDir, 0o755)
	os.MkdirAll(h.RunDir, 0o755)
	calls, outputs, journal = &[]string{}, &[]string{}, new(string)
	show = map[string]string{}
	h.Systemctl = func(args ...string) error { *calls = append(*calls, strings.Join(args, " ")); return nil }
	h.Output = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*outputs = append(*outputs, name+" "+strings.Join(args, " "))
		if name == "journalctl" {
			return []byte(*journal), nil
		}
		var b strings.Builder
		for _, a := range args {
			if s, ok := show[a]; ok {
				fmt.Fprintf(&b, "Id=%s\n%s\n\n", a, s)
			}
		}
		return []byte(b.String()), nil
	}
	return
}

func TestLaravelStatusAndRestart(t *testing.T) {
	h, calls, show, _, outputs := laravelHost(t)
	if got, err := h.LaravelStatus(context.Background(), 4); err != nil || len(got) != 0 {
		t.Fatalf("no processes: %v %v", got, err)
	}
	if err := h.LaravelRestart(4); err == nil {
		t.Fatal("restarted nothing")
	}
	for _, u := range []string{"basalt-php-site-4-queue-10.service", "basalt-php-site-4-queue-2.service", "basalt-php-site-4-schedule.service",
		"basalt-php-site-4.service", "basalt-php-site-40-schedule.service", "basalt-sites-laravel4.slice"} {
		os.WriteFile(filepath.Join(h.UnitDir, u), nil, 0o644)
	}
	show["basalt-php-site-4-schedule.service"] = "ActiveState=active\nSubState=running\nActiveEnterTimestamp=@1790000000\nNRestarts=0"
	show["basalt-php-site-4-queue-2.service"] = "ActiveState=activating\nSubState=auto-restart\nActiveEnterTimestamp=\nNRestarts=7"
	show["basalt-php-site-4-queue-10.service"] = "ActiveState=failed\nSubState=failed\nActiveEnterTimestamp=n/a\nNRestarts=x"
	got, err := h.LaravelStatus(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []LaravelProcess{
		{Name: "schedule", State: "active", SubState: "running", Since: time.Unix(1790000000, 0).UTC()},
		{Name: "queue-2", State: "activating", SubState: "auto-restart", Restarts: 7},
		{Name: "queue-10", State: "failed", SubState: "failed"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("status = %+v", got)
	}
	if (*outputs)[0] != "systemctl show --timestamp=unix --property=Id,ActiveState,SubState,ActiveEnterTimestamp,NRestarts "+
		"basalt-php-site-4-schedule.service basalt-php-site-4-queue-2.service basalt-php-site-4-queue-10.service" {
		t.Fatalf("show: %s", (*outputs)[0])
	}
	if err := h.LaravelRestart(4); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, ";"); got != "restart --no-block basalt-php-site-4-schedule.service basalt-php-site-4-queue-2.service basalt-php-site-4-queue-10.service" {
		t.Fatalf("restart: %s", got)
	}
	h.Units = false
	if got, err := h.LaravelStatus(context.Background(), 4); err != nil || got != nil {
		t.Fatalf("without systemd: %v %v", got, err)
	}
	if h.LaravelRestart(4) == nil {
		t.Fatal("restarted without systemd")
	}
}

func TestLaravelLog(t *testing.T) {
	h, _, _, journal, outputs := laravelHost(t)
	long := strings.Repeat("é", laravelLineMax)
	*journal = `{"__CURSOR":"c1","__REALTIME_TIMESTAMP":"1790000000123456","MESSAGE":"Running scheduled command","SYSLOG_IDENTIFIER":"basalt-site-4-schedule","PRIORITY":"6"}
{"__REALTIME_TIMESTAMP":"1790000001000000","MESSAGE":[104,105,255],"SYSLOG_IDENTIFIER":"basalt-site-4-queue-1","PRIORITY":"3"}
not json
{"__REALTIME_TIMESTAMP":"1790000002000000","MESSAGE":"` + long + `","SYSLOG_IDENTIFIER":"basalt-site-4-queue-2"}
{"__REALTIME_TIMESTAMP":"1790000003000000","MESSAGE":null,"SYSLOG_IDENTIFIER":"basalt-site-4-queue-2","PRIORITY":"6"}
`
	got, err := h.LaravelLog(context.Background(), 4, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("lines = %+v", got)
	}
	if got[0] != (LaravelLogLine{Time: time.UnixMicro(1790000000123456).UTC(), Process: "schedule", Message: "Running scheduled command"}) {
		t.Errorf("line 0 = %+v", got[0])
	}
	if got[1].Process != "queue-1" || got[1].Message != "hi�" || !got[1].Error {
		t.Errorf("line 1 = %+v", got[1])
	}
	if l := got[2].Message; len(l) > laravelLineMax+len("…") || !strings.HasSuffix(l, "é…") || got[2].Error {
		t.Errorf("long line: %d bytes", len(l))
	}
	if got[3].Message != "" {
		t.Errorf("null message: %+v", got[3])
	}
	cmd := (*outputs)[0]
	for _, want := range []string{"journalctl ", " --output=json ", " --lines=1000 ", " --identifier=basalt-site-4-schedule ",
		" --identifier=basalt-site-4-queue-1 ", " --identifier=basalt-site-4-queue-8"} {
		if !strings.Contains(cmd+" ", want) {
			t.Errorf("journalctl lacks %q: %s", want, cmd)
		}
	}
	*journal = ""
	if got, err := h.LaravelLog(context.Background(), 4, 0); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	if !strings.Contains((*outputs)[1], " --lines=1 ") {
		t.Errorf("lines bound: %s", (*outputs)[1])
	}
}

// TestRestartUpdatedLaravel: a Laravel process started before its PHP CLI
// was installed is restarted (the FPM masters are RestartUpdated's own).
func TestRestartUpdatedLaravel(t *testing.T) {
	h, calls, show, _, _ := laravelHost(t)
	cli := filepath.Join(t.TempDir(), "php8.4")
	os.WriteFile(cli, nil, 0o755) // installed now
	unit := func(name string) {
		os.WriteFile(filepath.Join(h.UnitDir, name), []byte("[Service]\nExecStart="+cli+" artisan queue:work --no-interaction\n"), 0o644)
	}
	unit("basalt-php-site-5-schedule.service")
	unit("basalt-php-site-5-queue-1.service")
	unit("basalt-php-site-6-queue-1.service")
	unit("basalt-php-site-7-queue-1.service")
	ts := func(d time.Duration) string { return fmt.Sprintf("@%d", time.Now().Add(d).Unix()) }
	show["basalt-php-site-5-schedule.service"] = "ActiveState=active\nExecMainStartTimestamp=" + ts(-time.Hour)
	show["basalt-php-site-5-queue-1.service"] = "ActiveState=active\nExecMainStartTimestamp=" + ts(-time.Hour)
	show["basalt-php-site-6-queue-1.service"] = "ActiveState=active\nExecMainStartTimestamp=" + ts(time.Hour)
	show["basalt-php-site-7-queue-1.service"] = "ActiveState=inactive\nExecMainStartTimestamp="
	got, err := h.RestartUpdated()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int64{5}) || strings.Join(*calls, ";") != "restart basalt-php-site-5-queue-1.service;restart basalt-php-site-5-schedule.service" {
		t.Fatalf("restarted %v, calls %v", got, *calls)
	}
}
