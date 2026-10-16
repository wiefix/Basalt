package server

import (
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/store"
)

func TestBackupSchedule(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	daily := &store.BackupPolicy{Schedule: store.ScheduleDaily, AtMinute: 3 * 60, UpdatedAt: at("2026-10-01T10:00:00Z")}
	// Set at 10:00: the 03:00 slot of that day had passed, so nothing is
	// due until the next day.
	if backupDue(daily, at("2026-10-01T23:00:00Z")) {
		t.Fatal("due before its first slot")
	}
	if got := nextBackup(daily, at("2026-10-01T23:00:00Z")); !got.Equal(at("2026-10-02T03:00:00Z")) {
		t.Fatalf("next = %v", got)
	}
	if !backupDue(daily, at("2026-10-02T03:00:30Z")) {
		t.Fatal("not due at its slot")
	}
	daily.LastScheduled = at("2026-10-02T03:00:31Z")
	if backupDue(daily, at("2026-10-02T20:00:00Z")) {
		t.Fatal("due twice a day")
	}
	// The server was offline for days: one backup when it is back.
	if !backupDue(daily, at("2026-10-05T12:00:00Z")) {
		t.Fatal("missed slot not caught up")
	}
	// Times are UTC whatever the panel's zone.
	ny, _ := time.LoadLocation("America/New_York")
	if !backupDue(daily, at("2026-10-03T03:05:00Z").In(ny)) {
		t.Fatal("slot not in UTC")
	}

	hourly := &store.BackupPolicy{Schedule: store.ScheduleHourly, AtMinute: 15, UpdatedAt: at("2026-10-01T10:20:00Z")}
	if backupDue(hourly, at("2026-10-01T11:10:00Z")) || !backupDue(hourly, at("2026-10-01T11:15:00Z")) {
		t.Fatal("hourly slot")
	}
	hourly.LastScheduled = at("2026-10-01T11:15:10Z")
	if backupDue(hourly, at("2026-10-01T12:14:59Z")) || !backupDue(hourly, at("2026-10-01T12:15:00Z")) {
		t.Fatal("hourly repeat")
	}
	if backupDue(&store.BackupPolicy{Schedule: store.ScheduleManual}, at("2026-10-01T12:00:00Z")) {
		t.Fatal("manual policy due")
	}
}

func TestCheckSchedule(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	// A different part each week, all of them within a year.
	seen := map[string]bool{}
	for w := range checkParts {
		seen[checkSubset(now.Add(time.Duration(w)*7*24*time.Hour))] = true
	}
	if len(seen) != checkParts || checkSubset(now) != checkSubset(now.Add(time.Hour)) {
		t.Fatalf("subsets: %v", seen)
	}
	for _, c := range []struct {
		last *store.BackupCheck
		due  bool
		next time.Time
	}{
		{nil, true, now},
		{&store.BackupCheck{Status: "running", CreatedAt: now.Add(-30 * 24 * time.Hour)}, false, time.Time{}},
		{&store.BackupCheck{Status: "succeeded", CreatedAt: now.Add(-6 * 24 * time.Hour)}, false, now.Add(24 * time.Hour)},
		{&store.BackupCheck{Status: "succeeded", CreatedAt: now.Add(-7 * 24 * time.Hour)}, true, now},
		// A failed check is tried again the next day.
		{&store.BackupCheck{Status: "failed", CreatedAt: now.Add(-23 * time.Hour)}, false, now.Add(time.Hour)},
		{&store.BackupCheck{Status: "failed", CreatedAt: now.Add(-25 * time.Hour)}, true, now.Add(-time.Hour)},
	} {
		tg := store.CheckTarget{Last: c.last}
		if checkDue(tg, now) != c.due || !nextCheck(tg, now).Equal(c.next) {
			t.Errorf("%+v: due %v next %v", c.last, checkDue(tg, now), nextCheck(tg, now))
		}
	}
}
