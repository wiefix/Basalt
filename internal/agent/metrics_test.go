package agent

import (
	"math"
	"strings"
	"testing"
)

func TestParseCPUAndPercent(t *testing.T) {
	a, err := parseCPU(strings.NewReader(
		"cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 1 2 3 4 5 6 7 8 9 10\nintr 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	// guest (50) is already part of user and must not be added again.
	if a.total != 1000 || a.idle != 800 {
		t.Fatalf("times = %+v; want total 1000, idle 800", a)
	}
	b, _ := parseCPU(strings.NewReader("cpu  200 0 200 1300 100 0 0 0 0 0\n"))
	if got := cpuPercent(a, b); math.Abs(got-25) > 0.001 {
		t.Fatalf("cpu = %v; want 25", got)
	}
	if got := cpuPercent(b, a); got != 0 {
		t.Fatalf("counter reset should yield 0, got %v", got)
	}
	if _, err := parseCPU(strings.NewReader("intr 1\n")); err == nil {
		t.Fatal("missing cpu line accepted")
	}
}

func TestParseMeminfo(t *testing.T) {
	total, used, err := parseMeminfo(strings.NewReader(
		"MemTotal:        2000 kB\nMemFree:          100 kB\nMemAvailable:     500 kB\nBuffers: 1 kB\n"))
	if err != nil || total != 2000*1024 || used != 1500*1024 {
		t.Fatalf("total=%d used=%d err=%v", total, used, err)
	}
	if _, _, err := parseMeminfo(strings.NewReader("MemTotal: 1 kB\n")); err == nil {
		t.Fatal("missing MemAvailable accepted")
	}
}

func TestParseLoadavgUptime(t *testing.T) {
	l1, l5, l15, err := parseLoadavg("0.52 0.58 0.59 1/467 12345\n")
	if err != nil || l1 != 0.52 || l5 != 0.58 || l15 != 0.59 {
		t.Fatalf("%v %v %v %v", l1, l5, l15, err)
	}
	if up, err := parseUptime("12345.67 54321.00\n"); err != nil || up != 12345 {
		t.Fatalf("uptime %v %v", up, err)
	}
}

func TestContainerCPUUsage(t *testing.T) {
	// One full CPU of four busy: 100% of one CPU.
	if got := cpuUsage(cpuSample{0, 0}, cpuSample{1e9, 4e9}, 4); got != 100 {
		t.Fatalf("usage = %v", got)
	}
	// Counters reset (container restarted) or no time passed: no reading.
	if cpuUsage(cpuSample{5, 10}, cpuSample{1, 20}, 4) != 0 || cpuUsage(cpuSample{1, 10}, cpuSample{2, 10}, 4) != 0 {
		t.Fatal("bad samples gave a reading")
	}
}
