package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A VM that nobody uses goes to sleep: Cloud Hypervisor saves its memory to
// SleepDir and exits, so it takes no RAM or CPU. It wakes up where it was
// when someone needs it: `ssh <vm>.boxe`, a web request, `ssh boxe wake`.
// Waking maps the saved memory copy-on-write, which takes a fraction of a
// second; pages are read from disk as the VM touches them.

func (h *Hypervisor) sleepDir(name string) string { return filepath.Join(h.SleepDir, name) }

// sleepDone marks a complete snapshot: a half-written one is never restored.
const sleepDone = "complete"

func (h *Hypervisor) Sleeping(name string) bool {
	return h.SleepDir != "" && exists(filepath.Join(h.sleepDir(name), sleepDone))
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func (h *Hypervisor) chRemote(name string, args ...string) ([]byte, error) {
	return h.exec(time.Minute, "ch-remote", append([]string{"--api-socket", sock(name)}, args...)...)
}

// Sleep saves v's memory and ends its hypervisor. On failure v keeps running.
func (h *Hypervisor) Sleep(v VM) error {
	if h.SleepDir == "" {
		return errors.New("sleeping is off on this host")
	}
	if !h.Running(v.Name) {
		return fmt.Errorf("%s is not running", v.Name)
	}
	// Not needed to wake up (memory and disk are saved together), but a copy
	// of a sleeping VM's disk then has everything written so far.
	h.flush(v)
	if err := h.forgetSleep(v.Name); err != nil {
		return err
	}
	if _, err := h.chRemote(v.Name, "pause"); err != nil {
		return err
	}
	if _, err := h.chRemote(v.Name, "snapshot", "file://"+h.sleepDir(v.Name)); err != nil {
		h.chRemote(v.Name, "resume")
		h.forgetSleep(v.Name)
		return err
	}
	if err := os.WriteFile(filepath.Join(h.sleepDir(v.Name), sleepDone), nil, 0o600); err != nil {
		h.chRemote(v.Name, "resume")
		h.forgetSleep(v.Name)
		return err
	}
	// The hypervisor exits by itself, which the unit takes as a clean end.
	if _, err := h.chRemote(v.Name, "shutdown-vmm"); err != nil {
		h.run("systemctl", "stop", unit(v.Name))
	}
	for i := 0; h.Running(v.Name); i++ {
		if i == 100 {
			h.run("systemctl", "stop", unit(v.Name))
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.run("systemctl", "reset-failed", unit(v.Name))
	return nil
}

// Wake restores a sleeping VM. It is restored paused, and its snapshot is
// deleted before it runs on: from then on the disk moves on from that
// memory, which must never come back. If the restore fails, the VM stays
// asleep; if resuming fails, it is off and boots afresh on restart.
func (h *Hypervisor) Wake(v VM) error {
	if !h.Sleeping(v.Name) {
		return nil
	}
	// Paused by a boxed that stopped halfway: it never ran, start over.
	if h.Running(v.Name) {
		h.run("systemctl", "stop", unit(v.Name))
	}
	if err := h.start(v, true); err != nil {
		h.run("systemctl", "stop", unit(v.Name))
		return err
	}
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if out, err := h.chRemote(v.Name, "info"); err == nil && strings.Contains(string(out), `"Paused"`) {
			err := h.forgetSleep(v.Name)
			if err == nil {
				_, err = h.chRemote(v.Name, "resume")
			}
			if err != nil {
				h.run("systemctl", "stop", unit(v.Name))
				h.run("systemctl", "reset-failed", unit(v.Name))
				return fmt.Errorf("%s did not resume after the restore: %v", v.Name, err)
			}
			return nil
		}
		if !h.Running(v.Name) {
			return fmt.Errorf("%s could not be restored (journalctl -u %s on the host)", v.Name, unit(v.Name))
		}
	}
	h.run("systemctl", "stop", unit(v.Name))
	return fmt.Errorf("%s: no answer after the restore", v.Name)
}

// forgetSleep deletes a VM's snapshot. Only what is in the directory: the
// directory itself is mounted into the VM's hypervisor.
func (h *Hypervisor) forgetSleep(name string) error {
	if h.SleepDir == "" {
		return nil
	}
	dir := h.sleepDir(name)
	// The marker first: what is left after a failure is not a snapshot.
	if err := os.Remove(filepath.Join(dir, sleepDone)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// retarget points the snapshot of v at its devices as they are now: the
// zvol may have another /dev/zdN after a host reboot.
func (h *Hypervisor) retarget(v VM, disk string) error {
	p := filepath.Join(h.sleepDir(v.Name), "config.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var c map[string]any
	if err := json.Unmarshal(b, &c); err != nil {
		return err
	}
	disks, _ := c["disks"].([]any)
	nets, _ := c["net"].([]any)
	if len(disks) != 2 || len(nets) != 1 {
		return fmt.Errorf("%s: unexpected devices in the snapshot", v.Name)
	}
	disks[0].(map[string]any)["path"] = disk
	disks[1].(map[string]any)["path"] = h.config(v.Name)
	nets[0].(map[string]any)["tap"] = tap(v)
	nets[0].(map[string]any)["mac"] = v.MAC()
	if vs, ok := c["vsock"].(map[string]any); ok {
		vs["socket"] = vsockPath(v.Name)
	}
	if b, err = json.Marshal(c); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// CPUUsage is the CPU time v's hypervisor has used, in ns; -1 if unknown.
func (h *Hypervisor) CPUUsage(name string) int64 {
	out, err := h.exec(5*time.Second, "systemctl", "show", "-p", "CPUUsageNSec", "--value", unit(name))
	n, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || perr != nil {
		return -1
	}
	return n
}

// MemUsage is the RAM v's hypervisor takes on the host, in bytes: the
// guest memory touched so far plus the hypervisor's own; 0 if unknown.
func (h *Hypervisor) MemUsage(name string) int64 {
	out, _ := h.exec(5*time.Second, "systemctl", "show", "-p", "MemoryCurrent", "--value", unit(name))
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n
}

// DiskUsage is the space each VM's disk takes in the pool, in bytes, by
// VM name: what the VM wrote, plus the image it started from.
func (h *Hypervisor) DiskUsage() map[string]int64 {
	use := map[string]int64{}
	out, err := h.exec(10*time.Second, "zfs", "list", "-Hp", "-o", "name,referenced", "-r", h.Dataset)
	if err != nil {
		return use
	}
	for _, line := range strings.Split(string(out), "\n") {
		ds, n, ok := strings.Cut(line, "\t")
		if name, isVM := strings.CutPrefix(ds, h.Dataset+"/"); ok && isVM {
			use[name], _ = strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		}
	}
	return use
}

// cpuPercent is the CPU v used since the last check of sleepIdle, up to a
// minute ago: 100 is one core. 0 if unknown.
func (b *Box) cpuPercent(name string, now time.Time) float64 {
	cpu := b.hv.CPUUsage(name)
	b.actMu.Lock()
	var last int64
	var at time.Time
	if a := b.act[name]; a != nil {
		last, at = a.cpu, a.at
	}
	b.actMu.Unlock()
	if cpu < last || at.IsZero() || now.Sub(at) < time.Second {
		return 0
	}
	return float64(cpu-last) / float64(now.Sub(at)) * 100
}

// Traffic is what v has sent and received, in bytes.
func Traffic(v VM) int64 {
	var total int64
	for _, f := range []string{"rx_bytes", "tx_bytes"} {
		b, _ := os.ReadFile(filepath.Join("/sys/class/net", tap(v), "statistics", f))
		n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		total += n
	}
	return total
}

// What counts as work: a VM doing less than this, with nobody connected,
// is idle. An idle VM uses about 2% of a CPU and almost no network.
const (
	busyCPU     = 0.05      // share of one CPU
	busyTraffic = 16 * 1024 // bytes per minute
)

// activity is what boxed knows of a running VM's use.
type activity struct {
	conns   int       // open SSH jumps and web requests
	last    time.Time // last time it was in use
	cpu     int64     // CPU time and traffic at the last check
	traffic int64
	at      time.Time
}

// busy counts a connection to the VM opening (+1) or closing (-1).
func (b *Box) busy(name string, delta int) {
	b.actMu.Lock()
	defer b.actMu.Unlock()
	a := b.activityOf(name, time.Now())
	a.conns += delta
	a.last = time.Now()
}

func (b *Box) activityOf(name string, now time.Time) *activity {
	if b.act == nil {
		b.act = map[string]*activity{}
	}
	a := b.act[name]
	if a == nil {
		a = &activity{last: now}
		b.act[name] = a
	}
	return a
}

// sleepIdle puts to sleep the VMs nobody has used for sleepAfter: no SSH or
// web connection, little CPU, little traffic. Runs every minute or so; its
// readings also give the CPU use in ls, so it runs even if VMs never sleep.
func (b *Box) sleepIdle(now time.Time) {
	sleeps := b.sleepAfter > 0 && b.hv.SleepDir != ""
	vms, err := b.store.List("")
	if err != nil {
		log.Printf("sleep: %v", err)
		return
	}
	for _, v := range vms {
		if b.hv.State(v.Name) != "running" {
			b.actMu.Lock()
			delete(b.act, v.Name)
			b.actMu.Unlock()
			continue
		}
		cpu, traffic := b.hv.CPUUsage(v.Name), Traffic(v)
		b.actMu.Lock()
		a := b.activityOf(v.Name, now)
		if dt := now.Sub(a.at); !a.at.IsZero() && dt > 0 {
			if float64(cpu-a.cpu) > busyCPU*float64(dt) || float64(traffic-a.traffic) > busyTraffic*dt.Minutes() {
				a.last = now
			}
		}
		if a.conns > 0 || v.KeepAwake {
			a.last = now // a VM kept awake is unused from when that ends
		}
		a.cpu, a.traffic, a.at = cpu, traffic, now
		idle := sleeps && now.Sub(a.last) >= b.sleepAfter
		b.actMu.Unlock()
		if idle {
			b.sleepIfIdle(v, now)
		}
	}
}

// sleepIfIdle puts v to sleep unless it was used meanwhile, or went to
// sleep already: a wake may have made room with it.
func (b *Box) sleepIfIdle(v VM, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.actMu.Lock()
	a := b.act[v.Name]
	idle := a != nil && a.conns == 0 && now.Sub(a.last) >= b.sleepAfter
	b.actMu.Unlock()
	cur, err := b.store.Get(v.Name)
	if !idle || err != nil || cur.Index != v.Index || cur.KeepAwake || b.hv.State(v.Name) != "running" {
		return
	}
	if err := b.hv.Sleep(cur); err != nil {
		log.Printf("%s: sleep %s: %v", cur.Owner, cur.Name, err)
		return
	}
	log.Printf("%s: %s went to sleep, unused for %s", cur.Owner, cur.Name, b.sleepAfter)
	b.actMu.Lock()
	delete(b.act, v.Name)
	b.actMu.Unlock()
}

// SleepLoop records what VMs use and puts idle ones to sleep, every so
// often (a minute: the CPU of a sample is over the time since the last).
func (b *Box) SleepLoop(every time.Duration) {
	for {
		time.Sleep(every)
		now := time.Now()
		b.sample(now)
		b.sleepIdle(now)
	}
}

// sampleMaxAge is how long the usage of VMs is kept, for the panel's charts.
const sampleMaxAge = 30 * 24 * time.Hour

// sample records what each VM uses now.
func (b *Box) sample(now time.Time) {
	vms, list, err := b.list("", now)
	if err != nil {
		log.Printf("sample: %v", err)
		return
	}
	for i, v := range vms {
		j := list[i]
		if err := b.store.AddSample(v.Name, Sample{At: now, CPU: j.CPUPercent, Mem: j.MemUsed, Disk: j.DiskUsed}); err != nil {
			log.Printf("sample: %v", err)
			return
		}
	}
	if err := b.store.PruneSamples(now.Add(-sampleMaxAge)); err != nil {
		log.Printf("sample: %v", err)
	}
}

// wake wakes the VM named name if it sleeps, and waits for its SSH server.
// user: only the owner may wake it; empty: any member (the web).
func (b *Box) wake(user, name string) error {
	b.mu.Lock()
	v, err := b.store.Get(name)
	if err == nil && user != "" && v.Owner != user {
		err = errNotFound
	}
	if err != nil || !b.hv.Sleeping(name) {
		b.mu.Unlock()
		return nil // nothing to wake: whoever called reports what is wrong
	}
	if err := b.makeRoom(v); err != nil {
		b.mu.Unlock()
		return err
	}
	start := time.Now()
	err = b.hv.Wake(v)
	b.mu.Unlock()
	if err != nil {
		log.Printf("%s: wake %s: %v", v.Owner, name, err)
		return fmt.Errorf("could not wake %s up", name)
	}
	b.busy(name, 0) // it counts as used from now
	if err := b.hv.WaitSSH(v, 10*time.Second); err != nil {
		return err
	}
	log.Printf("%s: %s woke up in %s", v.Owner, name, time.Since(start).Round(time.Millisecond))
	return nil
}

// makeRoom puts v's owner's idle VMs to sleep, the longest unused first,
// until v fits awake in their RAM quota. VMs in use or kept awake stay
// awake. Called with b.mu held.
func (b *Box) makeRoom(v VM) error {
	if b.limits.UserMem == 0 {
		return nil
	}
	vms, err := b.store.List(v.Owner)
	if err != nil {
		return err
	}
	awake, _ := memMiB(v.Mem)
	var idle []VM
	last := map[string]time.Time{}
	b.actMu.Lock()
	for _, o := range vms {
		if o.Name == v.Name || b.hv.Sleeping(o.Name) {
			continue
		}
		m, _ := memMiB(o.Mem)
		awake += m
		if a := b.act[o.Name]; a == nil || a.conns == 0 {
			if a != nil {
				last[o.Name] = a.last
			}
			if !o.KeepAwake && b.hv.SleepDir != "" {
				idle = append(idle, o)
			}
		}
	}
	b.actMu.Unlock()
	sort.SliceStable(idle, func(i, j int) bool { return last[idle[i].Name].Before(last[idle[j].Name]) })
	for _, o := range idle {
		if awake <= b.limits.UserMem {
			break
		}
		if b.hv.State(o.Name) != "running" {
			continue
		}
		if err := b.hv.Sleep(o); err != nil {
			log.Printf("%s: sleep %s: %v", o.Owner, o.Name, err)
			continue
		}
		log.Printf("%s: %s went to sleep to make room for %s", o.Owner, o.Name, v.Name)
		b.actMu.Lock()
		delete(b.act, o.Name)
		b.actMu.Unlock()
		m, _ := memMiB(o.Mem)
		awake -= m
	}
	if awake > b.limits.UserMem {
		return fmt.Errorf("%s does not fit in your %dG of RAM next to your other awake VMs, which are in use or kept awake: put one to sleep (ssh %s sleep <name>) or delete one",
			v.Name, b.limits.UserMem/1024, b.name)
	}
	return nil
}

func (b *Box) cmdSleep(o output, user string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ssh %s sleep <name>", b.name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.owned(user, args[0])
	if err != nil {
		return err
	}
	if err := b.hv.Sleep(v); err != nil {
		log.Printf("%s: sleep %s: %v", user, v.Name, err)
		return fmt.Errorf("could not put %s to sleep: %v", v.Name, err)
	}
	log.Printf("%s: put %s to sleep", user, v.Name)
	o.print(done{v.Name, "sleeping"}, v.Name+" is asleep: it wakes up when you connect to it\n")
	return nil
}

func (b *Box) cmdWake(o output, user string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ssh %s wake <name>", b.name)
	}
	if _, err := b.owned(user, args[0]); err != nil {
		return err
	}
	if err := b.wake(user, args[0]); err != nil {
		return err
	}
	o.print(done{args[0], "running"}, args[0]+" is awake\n")
	return nil
}

func (b *Box) cmdKeepAwake(o output, user string, args []string) error {
	if len(args) != 2 || (args[1] != "on" && args[1] != "off") {
		return fmt.Errorf("usage: ssh %s keep-awake <name> on|off", b.name)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.owned(user, args[0])
	if err != nil {
		return err
	}
	if err := b.store.SetKeepAwake(v.Name, args[1] == "on"); err != nil {
		return err
	}
	v.KeepAwake = args[1] == "on"
	text := v.Name + " goes to sleep when unused\n"
	if v.KeepAwake {
		text = v.Name + " stays awake\n"
	}
	o.print(b.vmJSON(v), text)
	return nil
}
