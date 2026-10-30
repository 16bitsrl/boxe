package main

import (
	"strings"
	"testing"
	"time"
)

// `ssh boxe ls` shows what each VM uses: CPU over the last minute, the RAM
// it takes on the host and the space its disk takes, also when VMs never
// sleep.
func TestLsShowsUsage(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	c.must("alice", "new nap")
	c.must("alice", "sleep nap")
	c.host.cpu[unit("test")] = 1e9
	c.box.sleepIdle(time.Now().Add(-10 * time.Second))
	c.host.cpu[unit("test")] = 6e9 // 5 s of CPU in 10 s
	c.host.mem = map[string]int64{unit("test"): 700 << 20}
	c.host.disk = "boxe/vms/nap\t1073741824\nboxe/vms/test\t5476083302\n"

	out := c.must("alice", "ls")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "CPU%") || !strings.Contains(lines[0], "RAM USED") || !strings.Contains(lines[0], "DISK USED") {
		t.Fatalf("ls:\n%s", out)
	}
	test, nap := strings.Fields(lines[2]), strings.Fields(lines[1])
	if !has(test, "50%") || !has(test, "700M") || !has(test, "5.1G") {
		t.Errorf("test: %v", test)
	}
	if !has(nap, "1.0G") || has(nap, "0%") {
		t.Errorf("nap, asleep: %v", nap)
	}

	var list []vmJSON
	c.json("alice", "ls --json", &list)
	if list[1].CPUPercent < 49 || list[1].CPUPercent > 51 || list[1].MemUsed != 700<<20 || list[1].DiskUsed != 5476083302 {
		t.Errorf("ls --json: %+v", list[1])
	}
}

func has(fields []string, s string) bool {
	for _, f := range fields {
		if f == s {
			return true
		}
	}
	return false
}

// Once a minute boxed records what each VM uses, for the panel's charts; it
// keeps 30 days, and forgets a VM when it is deleted.
func TestUsageIsSampled(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	t0 := time.Now().Add(-time.Hour)
	c.host.cpu[unit("test")] = 1e9
	c.box.sleepIdle(t0)
	c.host.cpu[unit("test")] = 31e9 // 30 s of CPU in 60 s
	c.host.mem = map[string]int64{unit("test"): 700 << 20}
	c.host.disk = "boxe/vms/test\t5476083302\n"
	c.box.sample(t0.Add(time.Minute))

	got, err := c.box.store.Samples("test", t0)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	s := got[0]
	if s.CPU < 49 || s.CPU > 51 || s.Mem != 700<<20 || s.Disk != 5476083302 || !s.At.Equal(t0.Add(time.Minute).Truncate(time.Second)) {
		t.Errorf("%+v", s)
	}
	c.box.sample(t0.Add(31 * 24 * time.Hour))
	if got, _ := c.box.store.Samples("test", t0); len(got) != 1 || got[0].At.Before(t0.Add(30*24*time.Hour)) {
		t.Errorf("after 31 days: %+v", got)
	}
	c.must("alice", "rm test")
	if got, _ := c.box.store.Samples("test", t0); len(got) != 0 {
		t.Errorf("samples of a deleted VM: %+v", got)
	}
}
