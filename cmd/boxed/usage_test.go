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
