package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runningVM(t *testing.T, h *Hypervisor, name string) VM {
	t.Helper()
	v := VM{Name: name, Index: 2, CPUs: 1, Mem: "1G"}
	if err := h.Create(v, h.BaseSnapshot, ""); err != nil {
		t.Fatal(err)
	}
	return v
}

// Sleeping saves the VM's memory to disk and ends its hypervisor.
func TestSleepSavesTheVM(t *testing.T) {
	h, f := newFakeHypervisor(t)
	v := runningVM(t, h, "test")
	if err := h.Sleep(v); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ch-remote --api-socket " + sock("test") + " pause",
		"ch-remote --api-socket " + sock("test") + " snapshot file://" + h.sleepDir("test"),
		"ch-remote --api-socket " + sock("test") + " shutdown-vmm"} {
		if !f.ran(want) {
			t.Errorf("did not run %q", want)
		}
	}
	if s := h.State("test"); s != "sleeping" {
		t.Errorf("state %q, want sleeping", s)
	}
	// After a host reboot it stays asleep, until someone needs it.
	f.cmds = nil
	if err := h.Start(v); err != nil || f.ran("systemd-run") {
		t.Errorf("Start booted a sleeping VM (%v)", err)
	}
}

// A snapshot that fails leaves the VM running as before.
func TestSleepFailureResumes(t *testing.T) {
	h, f := newFakeHypervisor(t)
	v := runningVM(t, h, "test")
	f.fail["ch-remote --api-socket "+sock("test")+" snapshot"] = true
	if err := h.Sleep(v); err == nil {
		t.Fatal("no error")
	}
	if !f.ran("ch-remote --api-socket " + sock("test") + " resume") {
		t.Error("the VM was left paused")
	}
	if s := h.State("test"); s != "running" {
		t.Errorf("state %q, want running", s)
	}
}

// Waking restores the VM where it was, with the disk's current device: a
// zvol may get another /dev/zdN after a host reboot.
func TestWakeRestores(t *testing.T) {
	h, f := newFakeHypervisor(t)
	v := runningVM(t, h, "test")
	if err := h.Sleep(v); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(h.sleepDir("test"), "config.json")
	f.cmds = nil
	if err := h.Wake(v); err != nil {
		t.Fatal(err)
	}
	var run string
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "systemd-run") {
			run = c
		}
	}
	for _, want := range []string{"--restore source_url=file://" + h.sleepDir("test") + ",memory_restore_mode=copyonwrite,resume=false",
		"-p User=", "-p NoNewPrivileges=yes"} {
		if !strings.Contains(run, want) && !(want == "-p User=" && h.VMUser == "") {
			t.Errorf("systemd-run lacks %q:\n%s", want, run)
		}
	}
	// Not restarted from the same memory after a crash: the disk has moved on.
	if strings.Contains(run, "Restart=on-failure") {
		t.Errorf("a restored VM restarts from its old memory:\n%s", run)
	}
	if exists(cfg) {
		t.Error("the snapshot is still there: it could be restored again over a newer disk")
	}
	if s := h.State("test"); s != "running" {
		t.Errorf("state %q", s)
	}
}

func TestWakeRewritesPaths(t *testing.T) {
	h, f := newFakeHypervisor(t)
	v := runningVM(t, h, "test")
	h.Sleep(v)
	f.fail["systemd-run"] = true // keep the snapshot to look at it
	if err := h.Wake(v); err == nil {
		t.Fatal("no error")
	}
	b, err := os.ReadFile(filepath.Join(h.sleepDir("test"), "config.json"))
	if err != nil {
		t.Fatal("a failed wake lost the snapshot:", err)
	}
	var c struct {
		Disks []struct{ Path string }
		Net   []struct{ Tap, MAC string }
		Vsock struct{ Socket string }
		CPUs  struct {
			Boot int `json:"boot_vcpus"`
		}
	}
	json.Unmarshal(b, &c)
	disk, _ := filepath.EvalSymlinks(h.dev("test"))
	if c.Disks[0].Path != disk || c.Disks[1].Path != h.config("test") || c.Net[0].Tap != "tap2" ||
		c.Net[0].MAC != v.MAC() || c.Vsock.Socket != vsockPath("test") {
		t.Errorf("paths not rewritten: %+v", c)
	}
	if c.CPUs.Boot != 2 {
		t.Error("other settings lost")
	}
	if s := h.State("test"); s != "sleeping" {
		t.Errorf("state %q after a failed wake, want sleeping", s)
	}
}

// Powering off a sleeping VM forgets its memory: restart boots it afresh.
func TestStopForgetsSleep(t *testing.T) {
	h, f := newFakeHypervisor(t)
	v := runningVM(t, h, "test")
	h.Sleep(v)
	if err := h.Stop(v); err != nil {
		t.Fatal(err)
	}
	f.cmds = nil
	h.Start(v)
	if !f.ran("systemd-run") || h.State("test") != "running" {
		t.Error("restart did not boot the VM")
	}
}

func TestSleepAndWakeCommands(t *testing.T) {
	c := newCLI(t, "alice", "bob")
	c.must("alice", "new test")
	if _, _, ok := c.run("bob", "sleep test"); ok {
		t.Error("bob put alice's VM to sleep")
	}
	c.must("alice", "sleep test")
	var list []vmJSON
	c.json("alice", "ls --json", &list)
	if list[0].State != "sleeping" {
		t.Errorf("state %q", list[0].State)
	}
	if out := c.must("alice", "wake test"); !strings.Contains(out, "test is awake") {
		t.Errorf("wake: %q", out)
	}
	c.json("alice", "ls --json", &list)
	if list[0].State != "running" {
		t.Errorf("state after wake %q", list[0].State)
	}
}

// ssh <vm>.boxe wakes the VM: users never need to know it slept.
func TestJumpWakesTheVM(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	c.must("alice", "sleep test")
	conn, err := c.conn["alice"].Dial("tcp", "test.boxe:22")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if s := c.box.hv.State("test"); s != "running" {
		t.Errorf("state %q after a jump", s)
	}
}

// A VM sleeps after a while without SSH or web connections and without
// using the CPU; unless its owner keeps it awake.
func TestIdleVMsSleep(t *testing.T) {
	c := newCLI(t, "alice")
	b, f := c.box, c.host
	b.sleepAfter = 30 * time.Minute
	c.must("alice", "new idle")
	c.must("alice", "new busy")
	c.must("alice", "new connected")
	c.must("alice", "new awake")
	c.must("alice", "keep-awake awake on")
	conn, err := c.conn["alice"].Dial("tcp", "connected.boxe:22")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	now := time.Now()
	for i := range 40 {
		at := now.Add(time.Duration(i) * time.Minute)
		f.mu.Lock()
		f.cpu[unit("busy")] += int64(30 * time.Second) // half a CPU
		f.cpu[unit("idle")] += int64(time.Second / 2)  // under 1%
		f.mu.Unlock()
		b.sleepIdle(at)
	}
	want := map[string]string{"idle": "sleeping", "busy": "running", "connected": "running", "awake": "running"}
	for name, state := range want {
		if s := b.hv.State(name); s != state {
			t.Errorf("%s: %s, want %s", name, s, state)
		}
	}
	var list []vmJSON
	c.json("alice", "ls --json", &list)
	for _, v := range list {
		if v.KeepAwake != (v.Name == "awake") {
			t.Errorf("%s: keep_awake %v", v.Name, v.KeepAwake)
		}
	}
}

// Without -sleep-after, VMs never sleep by themselves.
func TestNoSleepByDefault(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new idle")
	for i := range 120 {
		c.box.sleepIdle(time.Now().Add(time.Duration(i) * time.Minute))
	}
	if s := c.box.hv.State("idle"); s != "running" {
		t.Errorf("state %q", s)
	}
}

// The RAM quota counts the VMs that are awake: sleeping ones use none.
// Waking one up makes room by putting the member's other idle VMs to sleep,
// the longest unused first; VMs in use or kept awake are left alone.
func TestQuotaCountsAwakeVMs(t *testing.T) {
	c := newCLI(t, "alice", "bob")
	c.box.limits = Limits{UserMem: 4096}
	c.must("alice", "new a1 --mem 2G")
	c.must("alice", "new b1 --mem 2G")
	if _, _, ok := c.run("alice", "new c1 --mem 1G"); ok {
		t.Fatal("created a VM over the quota")
	}
	c.must("bob", "new d1 --mem 4G") // bob has his own quota
	c.must("alice", "sleep a1")
	c.must("alice", "new c1 --mem 1G")
	state := func(name string) string { return c.box.hv.State(name) }

	// a needs 2G: b is in use, so c goes to sleep.
	c.box.busy("b1", 1)
	c.must("alice", "wake a1")
	if state("a1") != "running" || state("b1") != "running" || state("c1") != "sleeping" {
		t.Fatalf("a1 %s, b1 %s, c1 %s", state("a1"), state("b1"), state("c1"))
	}
	// Nothing left to put to sleep: b is in use, a is kept awake.
	c.must("alice", "keep-awake a1 on")
	for _, cmd := range []string{"wake c1", "restart c1"} {
		out, errOut, ok := c.run("alice", cmd)
		if ok || !strings.Contains(out+errOut, "sleep") {
			t.Errorf("%s: %q %q, want a refusal that says to put a VM to sleep", cmd, out, errOut)
		}
	}
	if state("c1") != "sleeping" {
		t.Errorf("c1 %s", state("c1"))
	}
	// Once b is free, the oldest idle VM makes room.
	c.box.busy("b1", -1)
	c.must("alice", "wake c1")
	if state("b1") != "sleeping" || state("c1") != "running" || state("d1") != "running" {
		t.Errorf("b1 %s, c1 %s, d1 %s", state("b1"), state("c1"), state("d1"))
	}
}

// The idle loop picks a VM, then waits for the lock; meanwhile a wake can
// put that VM to sleep to make room, or someone can use it and leave.
func TestIdleSleepRechecksUnderTheLock(t *testing.T) {
	c := newCLI(t, "alice")
	b := c.box
	b.sleepAfter = 30 * time.Minute
	c.must("alice", "new gone")
	c.must("alice", "new used")
	vms, _ := b.store.List("alice")
	// gone: already asleep, its activity forgotten.
	c.must("alice", "sleep gone")
	b.actMu.Lock()
	delete(b.act, "gone")
	b.actMu.Unlock()
	// used: someone connected and left after the loop looked at it.
	b.busy("used", 1)
	b.busy("used", -1)

	for _, v := range vms {
		b.sleepIfIdle(v, time.Now().Add(time.Minute))
	}
	if s := b.hv.State("used"); s != "running" {
		t.Errorf("used: %s, want running", s)
	}
}

// The VM is restored paused and its snapshot deleted before it runs on: once
// it runs, its disk moves on from that memory, which must never come back.
// Even if boxed loses sight of it right after.
func TestWakeNeverRestoresTheSameMemoryTwice(t *testing.T) {
	h, f := newFakeHypervisor(t)
	v := runningVM(t, h, "test")
	if err := h.Sleep(v); err != nil {
		t.Fatal(err)
	}
	f.fail["ch-remote --api-socket "+sock("test")+" resume"] = true
	if err := h.Wake(v); err == nil {
		t.Fatal("no error")
	}
	if h.Sleeping("test") {
		t.Error("the snapshot outlived a restore that may have run")
	}
	if s := h.State("test"); s == "running" {
		t.Errorf("state %q: a VM restored but not resumed was left behind", s)
	}
}

// A boxed that stopped between restore and resume leaves the VM paused, its
// snapshot unused: the next wake starts over.
func TestWakeAfterAnInterruptedWake(t *testing.T) {
	h, f := newFakeHypervisor(t)
	v := runningVM(t, h, "test")
	if err := h.Sleep(v); err != nil {
		t.Fatal(err)
	}
	f.running[unit("test")], f.paused[unit("test")] = true, true
	if err := h.Wake(v); err != nil {
		t.Fatal(err)
	}
	if h.Sleeping("test") || h.State("test") != "running" || f.paused[unit("test")] {
		t.Errorf("state %q, sleeping %v, paused %v", h.State("test"), h.Sleeping("test"), f.paused[unit("test")])
	}
}

// cronAt is a schedule answer with a crontab line that runs at t (UTC).
func cronAt(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("localtime /usr/share/zoneinfo/Etc/UTC\n--- crontab exedev\n%d %d %d %d * /home/exedev/backup.sh\n",
		t.Minute(), t.Hour(), t.Day(), int(t.Month()))
}

// A VM goes to sleep with the time of the next job scheduled in it (cron,
// systemd timers), and ls says when it wakes up for it.
func TestSleepRecordsTheNextJob(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new test")
	job := time.Now().Add(2 * time.Hour).Truncate(time.Minute)
	c.host.schedule["test"] = cronAt(job)
	wakes := job.Add(-wakeLead)
	if out := c.must("alice", "sleep test"); !strings.Contains(out, wakes.Local().Format("15:04")) {
		t.Errorf("sleep does not say when it wakes up: %q", out)
	}
	var list []vmJSON
	c.json("alice", "ls --json", &list)
	if list[0].State != "sleeping" || list[0].WakesAt != wakes.UTC().Format(time.RFC3339) {
		t.Errorf("state %q, wakes_at %q, want %s", list[0].State, list[0].WakesAt, wakes.UTC().Format(time.RFC3339))
	}
	if out := c.must("alice", "ls"); !strings.Contains(out, "sleeping (wakes "+wakes.Local().Format("15:04")+")") {
		t.Errorf("ls:\n%s", out)
	}
	c.must("alice", "wake test")
	list = nil
	c.json("alice", "ls --json", &list)
	if list[0].WakesAt != "" {
		t.Errorf("awake, wakes_at %q", list[0].WakesAt)
	}
}

// VMs from an older image, whose boxe-agent does not know schedule (or
// without one), sleep as before: nothing wakes them up.
func TestOlderImagesJustSleep(t *testing.T) {
	c := newCLI(t, "alice")
	c.must("alice", "new old")
	c.must("alice", "new older")
	c.host.agentErr["schedule"] = "unknown command"
	c.must("alice", "sleep old")
	c.host.noAgent = true
	c.must("alice", "sleep older")
	var list []vmJSON
	c.json("alice", "ls --json", &list)
	for _, v := range list {
		if v.State != "sleeping" || v.WakesAt != "" {
			t.Errorf("%s: state %q, wakes_at %q", v.Name, v.State, v.WakesAt)
		}
	}
}

// A sleeping VM wakes up shortly before its job, so that the job runs on
// time inside it, and goes back to sleep soon after the job is done.
func TestVMWakesForItsJobs(t *testing.T) {
	c := newCLI(t, "alice")
	b, f := c.box, c.host
	b.sleepAfter = 30 * time.Minute
	c.must("alice", "new test")
	job := time.Now().Add(20 * time.Minute).Truncate(time.Minute)
	f.schedule["test"] = cronAt(job)
	c.must("alice", "sleep test")

	b.wakeJobs(job.Add(-2 * wakeLead))
	if s := b.hv.State("test"); s != "sleeping" {
		t.Fatalf("%s too early", s)
	}
	b.wakeJobs(job.Add(-wakeLead))
	if s := b.hv.State("test"); s != "running" {
		t.Fatalf("%s at the job's time", s)
	}
	// The job runs for a while, then the VM is idle again.
	b.sleepIdle(job.Add(-30 * time.Second))
	f.mu.Lock()
	f.cpu[unit("test")] += int64(20 * time.Second)
	f.mu.Unlock()
	b.sleepIdle(job.Add(30 * time.Second))
	b.sleepIdle(job.Add(90 * time.Second))
	if s := b.hv.State("test"); s != "running" {
		t.Fatalf("%s while its job may still run", s)
	}
	b.sleepIdle(job.Add(150 * time.Second))
	if s := b.hv.State("test"); s != "sleeping" {
		t.Fatalf("%s after its job, want sleeping well before -sleep-after", s)
	}
	// Until the same time next year.
	v, _ := b.store.Get("test")
	if want := job.UTC().AddDate(1, 0, 0); !v.NextJob.Equal(want) {
		t.Errorf("next job %s, want %s", v.NextJob, want)
	}
}

// Someone who connects to a VM woken up for a job makes it a VM in use, which
// sleeps after -sleep-after as usual.
func TestUseAfterAJobKeepsTheVMAwake(t *testing.T) {
	c := newCLI(t, "alice")
	b := c.box
	b.sleepAfter = 30 * time.Minute
	c.must("alice", "new test")
	job := time.Now().Add(20 * time.Minute).Truncate(time.Minute)
	c.host.schedule["test"] = cronAt(job)
	c.must("alice", "sleep test")
	b.wakeJobs(job.Add(-wakeLead))
	b.busy("test", 1)
	b.busy("test", -1)
	for i := range 10 {
		b.sleepIdle(job.Add(time.Duration(i) * time.Minute))
	}
	if s := b.hv.State("test"); s != "running" {
		t.Errorf("%s, want running", s)
	}
}

// An idle VM whose next job is due soon stays awake for it: going to sleep
// for a few minutes is not worth it.
func TestIdleVMStaysAwakeForAJobSoon(t *testing.T) {
	c := newCLI(t, "alice")
	b := c.box
	b.sleepAfter = 30 * time.Minute
	c.must("alice", "new test")
	now := time.Now()
	job := now.Add(38 * time.Minute).Truncate(time.Minute)
	c.host.schedule["test"] = cronAt(job)
	for i := range 45 {
		at := now.Add(time.Duration(i) * time.Minute)
		b.sleepIdle(at)
		if s := b.hv.State("test"); at.Before(job) && s != "running" {
			t.Fatalf("%s at %s, with a job at %s", s, at, job)
		}
	}
	if s := b.hv.State("test"); s != "sleeping" {
		t.Errorf("%s after the job", s)
	}
}

// Waking up for a job respects the owner's RAM quota, like any wake: if the
// VM does not fit, that run is skipped and it wakes up for the next one.
func TestJobWakeRespectsTheQuota(t *testing.T) {
	c := newCLI(t, "alice")
	b := c.box
	b.limits = Limits{UserMem: 4096}
	c.must("alice", "new a1 --mem 2G")
	c.must("alice", "new b1 --mem 2G")
	c.host.schedule["a1"] = "localtime /usr/share/zoneinfo/Etc/UTC\n--- crontab exedev\n*/10 * * * * x\n"
	c.must("alice", "sleep a1")
	c.must("alice", "new c1 --mem 2G")
	b.busy("b1", 1)
	b.busy("c1", 1)
	v, _ := b.store.Get("a1")
	due := v.NextJob
	if due.IsZero() {
		t.Fatal("no job")
	}
	b.wakeJobs(due.Add(-wakeLead))
	if s := b.hv.State("a1"); s != "sleeping" {
		t.Fatalf("a1 %s over the quota", s)
	}
	v, _ = b.store.Get("a1")
	if want := due.Add(10 * time.Minute); !v.NextJob.Equal(want) {
		t.Errorf("next job %s, want %s", v.NextJob, want)
	}
	b.busy("b1", -1)
	b.wakeJobs(v.NextJob.Add(-wakeLead))
	if a, b1 := b.hv.State("a1"), b.hv.State("b1"); a != "running" || b1 != "sleeping" {
		t.Errorf("a1 %s, b1 %s", a, b1)
	}
}

// A VM that could not wake up for a job tries again a little later, rather
// than waiting for a next run its schedule may not have (a systemd timer
// only says when it runs next).
func TestJobWakeRetries(t *testing.T) {
	c := newCLI(t, "alice")
	b := c.box
	b.limits = Limits{UserMem: 4096}
	c.must("alice", "new a1 --mem 2G")
	c.must("alice", "new b1 --mem 2G")
	c.host.schedule["a1"] = cronAt(time.Now().Add(time.Hour))
	c.must("alice", "sleep a1")
	c.must("alice", "new c1 --mem 2G")
	b.busy("b1", 1)
	b.busy("c1", 1)
	v, _ := b.store.Get("a1")
	now := v.NextJob.Add(-wakeLead)
	b.wakeJobs(now)
	v, _ = b.store.Get("a1")
	if want := now.Add(jobRetry); !v.NextJob.Equal(want) {
		t.Fatalf("next try %s, want %s", v.NextJob, want)
	}
	b.busy("b1", -1)
	b.wakeJobs(v.NextJob.Add(-wakeLead))
	if s := b.hv.State("a1"); s != "running" {
		t.Errorf("a1 %s on the second try", s)
	}
}
