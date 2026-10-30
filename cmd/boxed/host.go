package main

import (
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// hostInfo is what the admin's page of the panel shows about the host.
type hostInfo struct {
	Version  string
	Problems []string // as on /healthz
	CPUs     int
	Load     string // over 1, 5 and 15 minutes; "" if unknown
	MemTotal int64  // bytes; 0 if unknown
	MemFree  int64  // available to new VMs
	MemVMs   int64  // taken by the awake VMs, as they were sized
	DiskFree int64  // in the pool of the VM disks; -1 if unknown
	Members  []hostMember
	Backups  bool       // on
	Last     string     // when the last complete backup ended, RFC3339; "" never
	Run      *backupRun // the last run, if any
}

type hostMember struct {
	teamMember
	VMs      int
	MemAwake string // RAM of their awake VMs, and their limit
}

// backupRun is the last run of boxe-backup.sh: start and end in Unix
// seconds (no end while it runs, or if it died), the VMs it could not save.
type backupRun struct {
	Start  int64    `json:"start"`
	End    int64    `json:"end"`
	Failed []string `json:"failed"`
}

func (r backupRun) Started() string { return time.Unix(r.Start, 0).Format(time.RFC3339) }

// Took is how long the run took, e.g. 25m.
func (r backupRun) Took() string {
	return strings.TrimSuffix((time.Duration(r.End-r.Start) * time.Second).Round(time.Minute).String(), "0s")
}

func (b *Box) hostInfo() hostInfo {
	h := hostInfo{Version: version, Problems: b.problems(), CPUs: runtime.NumCPU(), DiskFree: -1}
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(raw)); len(f) >= 3 {
			h.Load = strings.Join(f[:3], " ")
		}
	}
	h.MemTotal, h.MemFree = meminfo("MemTotal"), meminfo("MemAvailable")
	if free, err := b.hv.FreeSpace(); err == nil {
		h.DiskFree = free
	}
	vms, _ := b.store.List("")
	count, awake := map[string]int{}, map[string]int64{}
	for _, v := range vms {
		count[v.Owner]++
		if b.hv.State(v.Name) == "running" {
			m, _ := memMiB(v.Mem)
			awake[v.Owner] += int64(m) << 20
			h.MemVMs += int64(m) << 20
		}
	}
	for _, m := range b.keys.Members() {
		mem := "0"
		if awake[m.User] > 0 {
			mem = size(awake[m.User])
		}
		if b.limits.UserMem > 0 {
			mem += " of " + size(int64(b.limits.UserMem)<<20)
		}
		h.Members = append(h.Members, hostMember{m, count[m.User], mem})
	}
	if _, err := os.Stat(b.backupConf); err == nil {
		h.Backups = true
		raw, _ := os.ReadFile(b.backupLast)
		if last, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil {
			h.Last = time.Unix(last, 0).Format(time.RFC3339)
		}
		var r backupRun
		if raw, err := os.ReadFile(b.backupStatus); err == nil && json.Unmarshal(raw, &r) == nil && r.Start > 0 {
			h.Run = &r
		}
	}
	return h
}

// meminfo is the field of /proc/meminfo, in bytes; 0 if unknown.
func meminfo(field string) int64 {
	raw, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == field+":" {
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}
